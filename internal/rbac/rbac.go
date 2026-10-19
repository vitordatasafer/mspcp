// Package rbac traduz os vínculos (memberships) de um usuário em escopos de
// visibilidade por permissão. A regra é simples: um papel num tenant vale
// para esse tenant e toda a subárvore abaixo dele; um papel num projeto vale
// só para aquele projeto.
package rbac

import (
	"github.com/google/uuid"

	"github.com/mspclouds/mspcp/internal/store"
)

type Permission string

const (
	ResourceRead     Permission = "resource.read"
	ResourcePower    Permission = "resource.power"
	ResourceCreate   Permission = "resource.create"  // criar VM a partir de imagem
	ResourceDelete   Permission = "resource.delete"  // excluir VM
	ResourceConsole  Permission = "resource.console" // console no navegador
	ResourceAssign   Permission = "resource.assign"  // mover VM existente para um VDC
	TenantRead       Permission = "tenant.read"
	TenantCreate     Permission = "tenant.create"
	VDCManage        Permission = "vdc.manage"    // criar VDCs e ajustar cotas
	LimitsManage     Permission = "limits.manage" // definir a cota dos MSPs
	MembershipManage Permission = "membership.manage"
	ClusterManage    Permission = "cluster.manage"
	ClusterRead      Permission = "cluster.read"
)

const (
	RoleSuperAdmin    = "super_admin"
	RoleNOCOperator   = "noc_operator"
	RolePartnerAdmin  = "partner_admin"
	RoleCustomerAdmin = "customer_admin"
	RoleOperator      = "operator"
	RoleViewer        = "viewer"
)

var rolePerms = map[string][]Permission{
	RoleSuperAdmin: {ResourceRead, ResourcePower, ResourceCreate, ResourceDelete, ResourceConsole, ResourceAssign,
		TenantRead, TenantCreate, VDCManage, MembershipManage, ClusterManage, ClusterRead, LimitsManage},
	RoleNOCOperator: {ResourceRead, ResourcePower, ResourceConsole, TenantRead, ClusterRead},
	// O MSP distribui a própria cota em VDCs; o cliente final usa a dele.
	RolePartnerAdmin: {ResourceRead, ResourcePower, ResourceCreate, ResourceDelete, ResourceConsole,
		TenantRead, TenantCreate, VDCManage, MembershipManage},
	RoleCustomerAdmin: {ResourceRead, ResourcePower, ResourceCreate, ResourceDelete, ResourceConsole,
		TenantRead, MembershipManage},
	RoleOperator: {ResourceRead, ResourcePower, ResourceConsole},
	RoleViewer:   {ResourceRead},
}

// RoleFitsTenant diz se um papel pode ser atribuído num tenant daquele tipo.
func RoleFitsTenant(role string, kind store.TenantKind) bool {
	switch role {
	case RoleSuperAdmin, RoleNOCOperator:
		return kind == store.KindProvider
	case RolePartnerAdmin:
		return kind == store.KindPartner
	case RoleCustomerAdmin:
		return kind == store.KindCustomer
	case RoleOperator, RoleViewer:
		return true
	}
	return false
}

func roleHas(role string, p Permission) bool {
	for _, x := range rolePerms[role] {
		if x == p {
			return true
		}
	}
	return false
}

// Principal é o usuário autenticado e seus vínculos ativos.
type Principal struct {
	Subject     string
	Email       string
	Name        string
	Memberships []store.Membership
	// Login local (nil quando veio de um token OIDC).
	User *store.User
	// SessionHash identifica a sessão atual (para não derrubá-la ao trocar a senha).
	SessionHash []byte
}

// Scope devolve os grants que concedem a permissão.
func (p *Principal) Scope(perm Permission) store.Scope {
	var s store.Scope
	for _, m := range p.Memberships {
		if !roleHas(m.Role, perm) {
			continue
		}
		s = append(s, store.Grant{
			TenantPath:     m.TenantPath,
			ProjectID:      m.ProjectID,
			SeesUnassigned: m.TenantKind == store.KindProvider && m.ProjectID == nil,
		})
	}
	return s
}

// Can checa a permissão sobre um tenant (path) e projeto específicos.
func (p *Principal) Can(perm Permission, tenantPath string, projectID *uuid.UUID) bool {
	return p.Scope(perm).Contains(tenantPath, projectID)
}

// IsProviderStaff indica vínculo no tenant raiz (equipe interna).
func (p *Principal) IsProviderStaff() bool {
	for _, m := range p.Memberships {
		if m.TenantKind == store.KindProvider {
			return true
		}
	}
	return false
}

// HomeTenant é o tenant "de origem" do usuário, usado na auditoria.
// Prefere o vínculo mais alto na árvore.
func (p *Principal) HomeTenant() *uuid.UUID {
	var best *store.Membership
	for i := range p.Memberships {
		m := &p.Memberships[i]
		if best == nil || len(m.TenantPath) < len(best.TenantPath) {
			best = m
		}
	}
	if best == nil {
		return nil
	}
	id := best.TenantID
	return &id
}

// CanAssign diz se o principal pode dar `role` num tenant (path, kind).
// Papéis de provedor só são atribuídos por quem administra clusters.
func (p *Principal) CanAssign(role string, tenantPath string, kind store.TenantKind) bool {
	if !RoleFitsTenant(role, kind) {
		return false
	}
	if role == RoleSuperAdmin || role == RoleNOCOperator {
		return p.Can(ClusterManage, tenantPath, nil)
	}
	return p.Can(MembershipManage, tenantPath, nil)
}
