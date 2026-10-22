package rbac_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vitordatasafer/mspvdc/internal/rbac"
	"github.com/vitordatasafer/mspvdc/internal/store"
)

func TestScopeHierarchy(t *testing.T) {
	prov, part, custA, custB := "/p/", "/p/x/", "/p/x/a/", "/p/b/"
	proj := uuid.New()

	partner := &rbac.Principal{Memberships: []store.Membership{{TenantPath: part, TenantKind: store.KindPartner, Role: rbac.RolePartnerAdmin}}}
	if !partner.Can(rbac.ResourcePower, custA, nil) {
		t.Error("parceiro deveria agir nos clientes dele")
	}
	if partner.Can(rbac.ResourceRead, custB, nil) || partner.Can(rbac.ResourceRead, prov, nil) {
		t.Error("parceiro não pode ver fora da própria subárvore")
	}
	if partner.Can(rbac.ResourceRead, "", nil) {
		t.Error("parceiro não pode ver recursos sem dono")
	}

	viewer := &rbac.Principal{Memberships: []store.Membership{{TenantPath: custA, TenantKind: store.KindCustomer, ProjectID: &proj, Role: rbac.RoleViewer}}}
	if !viewer.Can(rbac.ResourceRead, custA, &proj) {
		t.Error("viewer deveria ler o próprio projeto")
	}
	other := uuid.New()
	if viewer.Can(rbac.ResourceRead, custA, &other) || viewer.Can(rbac.ResourceRead, custA, nil) {
		t.Error("viewer restrito ao projeto não pode ver outros projetos")
	}
	if viewer.Can(rbac.ResourcePower, custA, &proj) {
		t.Error("viewer não pode ligar/desligar")
	}

	noc := &rbac.Principal{Memberships: []store.Membership{{TenantPath: prov, TenantKind: store.KindProvider, Role: rbac.RoleNOCOperator}}}
	if !noc.Can(rbac.ResourcePower, custB, nil) || !noc.Can(rbac.ResourceRead, "", nil) {
		t.Error("NOC deveria operar qualquer tenant e ver recursos sem dono")
	}
	if noc.Can(rbac.ClusterManage, prov, nil) || noc.Can(rbac.TenantCreate, prov, nil) {
		t.Error("NOC não administra clusters nem tenants")
	}

	custAdmin := &rbac.Principal{Memberships: []store.Membership{{TenantPath: custA, TenantKind: store.KindCustomer, Role: rbac.RoleCustomerAdmin}}}
	if custAdmin.CanAssign(rbac.RoleSuperAdmin, custA, store.KindCustomer) || custAdmin.CanAssign(rbac.RolePartnerAdmin, custA, store.KindCustomer) {
		t.Error("admin do cliente não pode atribuir papéis de provedor/parceiro")
	}
	if !custAdmin.CanAssign(rbac.RoleOperator, custA, store.KindCustomer) {
		t.Error("admin do cliente deveria poder criar operadores")
	}
}
