// Package store é a única camada que fala SQL. Toda leitura voltada a
// usuário recebe um Scope e filtra por ele dentro da própria query: não
// existe método "listar tudo" exposto para handlers de tenant.
package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("não encontrado")
	ErrConflict = errors.New("conflito")
)

type TenantKind string

const (
	KindProvider TenantKind = "provider"
	KindPartner  TenantKind = "partner"
	KindCustomer TenantKind = "customer"
)

type Tenant struct {
	ID        uuid.UUID  `json:"id"`
	ParentID  *uuid.UUID `json:"parent_id,omitempty"`
	Kind      TenantKind `json:"kind"`
	Name      string     `json:"name"`
	Path      string     `json:"-"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
}

// Project é o VDC: uma fatia de recursos num único cluster. Para MSP e
// cliente, o cluster aparece só como localização (rótulo + id opaco).
type Project struct {
	ID         uuid.UUID    `json:"id"`
	TenantID   uuid.UUID    `json:"tenant_id"`
	TenantName string       `json:"tenant_name,omitempty"`
	Name       string       `json:"name"`
	PoolName   string       `json:"-"`
	ClusterID  *uuid.UUID   `json:"location_id,omitempty"`
	Location   string       `json:"location"`
	CPUCores   int          `json:"cpu_cores"`
	MemoryMB   int64        `json:"memory_mb"`
	Storage    []VDCStorage `json:"storage"`
	Usage      *VDCUsage    `json:"usage,omitempty"`
	// Rede das VMs novas (vazia = padrão do cluster). Topologia: a API só
	// entrega para a equipe.
	Bridge    *string   `json:"-"`
	VLANTag   *int      `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

type VDCStorage struct {
	ClassID    uuid.UUID `json:"class_id"`
	ClassName  string    `json:"class_name"`
	CapacityGB int64     `json:"capacity_gb"`
	// UsedGB soma os discos registrados nessa classe (VMs criadas ou
	// atribuídas pelo portal).
	UsedGB int64 `json:"used_gb"`
}

// VDCUsage é o consumo atual, a partir do inventário sincronizado.
type VDCUsage struct {
	RunningCPU      int   `json:"running_cpu"`
	RunningMemoryMB int64 `json:"running_memory_mb"`
	VMs             int   `json:"vms"`
	DiskGB          int64 `json:"disk_gb"`
}

// MembershipView é um vínculo como aparece na tela de usuários do tenant.
type MembershipView struct {
	ID          uuid.UUID  `json:"id"`
	Subject     string     `json:"subject"`
	ProjectID   *uuid.UUID `json:"project_id,omitempty"`
	ProjectName *string    `json:"project_name,omitempty"`
	Role        string     `json:"role"`
	CreatedAt   time.Time  `json:"created_at"`
	// Dados do usuário local (vazios para usuários só do OIDC).
	Local       bool       `json:"local"`
	Name        string     `json:"name,omitempty"`
	Email       string     `json:"email,omitempty"`
	TOTPEnabled bool       `json:"totp_enabled"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

type Membership struct {
	ID         uuid.UUID
	Subject    string
	TenantID   uuid.UUID
	TenantPath string
	TenantKind TenantKind
	ProjectID  *uuid.UUID
	Role       string
}

// Cluster carrega dados internos; nunca é serializado para tenants.
type Cluster struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	DC             string     `json:"dc"`
	Location       string     `json:"location"`
	APIURL         string     `json:"api_url"`
	CACertPEM      string     `json:"-"`
	TokenID        string     `json:"token_id"`
	TokenSecretRef string     `json:"-"`
	TokenSecretEnc []byte     `json:"-"`
	DefaultBridge  string     `json:"default_bridge"`
	CPURatio       float64    `json:"cpu_ratio"`
	PVEVersion     string     `json:"pve_version"`
	NodesOnline    int        `json:"nodes_online"`
	NodesTotal     int        `json:"nodes_total"`
	CPUTotal       int        `json:"cpu_total"`
	MemoryTotalMB  int64      `json:"memory_total_mb"`
	Enabled        bool       `json:"enabled"`
	LastSyncAt     *time.Time `json:"last_sync_at,omitempty"`
	LastSyncError  *string    `json:"last_sync_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Resource é a visão pública de uma VM/CT. Não tem vmid, node nem cluster
// de propósito: o tipo em si impede vazar topologia na serialização.
type Resource struct {
	ID           uuid.UUID  `json:"id"`
	Alias        string     `json:"alias"`
	Kind         string     `json:"kind"`
	TenantID     *uuid.UUID `json:"tenant_id,omitempty"`
	TenantName   *string    `json:"tenant_name,omitempty"`
	ProjectID    *uuid.UUID `json:"project_id,omitempty"`
	ProjectName  *string    `json:"project_name,omitempty"`
	Status       string     `json:"status"`
	CPUs         int        `json:"cpus"`
	MaxMemBytes  int64      `json:"max_mem_bytes"`
	MaxDiskBytes int64      `json:"max_disk_bytes"`
	Deleting     bool       `json:"deleting,omitempty"`
	// Location é o rótulo de localização do cluster (nunca o nome dele).
	Location   string    `json:"location"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// ResourceLocation é o endereço interno de um recurso, só para workers.
type ResourceLocation struct {
	ResourceID uuid.UUID
	ClusterID  uuid.UUID
	Node       string
	VMID       int
	Kind       string
}

// Snapshot é uma VM/CT como o sincronizador a viu em /cluster/resources.
type Snapshot struct {
	VMID         int
	Kind         string
	Node         string
	Name         string
	Pool         string
	Status       string
	CPUs         int
	MaxMemBytes  int64
	MaxDiskBytes int64
}

type Task struct {
	ID          uuid.UUID  `json:"id"`
	ResourceID  *uuid.UUID `json:"resource_id,omitempty"`
	ProjectID   *uuid.UUID `json:"project_id,omitempty"`
	Label       string     `json:"label,omitempty"`
	Action      string     `json:"action"`
	Status      string     `json:"status"`
	RequestedBy string     `json:"requested_by"`
	Error       *string    `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type AuditEntry struct {
	Subject        string
	ActorTenantID  *uuid.UUID
	TargetTenantID *uuid.UUID
	Action         string
	Target         string
	Details        map[string]any
	RemoteIP       string
}

// Grant é uma permissão de visibilidade: a subárvore de tenants que começa em
// TenantPath, opcionalmente restrita a um projeto.
type Grant struct {
	TenantPath string
	ProjectID  *uuid.UUID
	// SeesUnassigned permite ver recursos ainda sem tenant (só provedor).
	SeesUnassigned bool
}

// Scope é o conjunto de grants de um principal para uma permissão.
// Scope vazio não enxerga nada.
type Scope []Grant
