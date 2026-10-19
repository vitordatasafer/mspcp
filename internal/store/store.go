package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX é satisfeito por *pgxpool.Pool, *pgx.Conn e pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

type Store struct{ db DBTX }

func New(db DBTX) *Store { return &Store{db: db} }

// WithTx devolve um Store que executa dentro da transação.
func (s *Store) WithTx(tx pgx.Tx) *Store { return &Store{db: tx} }

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------- tenants ----------

// allowedChild define a hierarquia de revenda.
func allowedChild(parent, child TenantKind) bool {
	switch parent {
	case KindProvider:
		return child == KindPartner // clientes diretos ficam num MSP interno
	case KindPartner:
		return child == KindCustomer
	}
	return false
}

const tenantCols = `id, parent_id, kind, name, path, status, created_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.ParentID, &t.Kind, &t.Name, &t.Path, &t.Status, &t.CreatedAt)
	return t, err
}

// CreateTenant cria um tenant. Sem parentID, cria a raiz (provedor).
func (s *Store) CreateTenant(ctx context.Context, parentID *uuid.UUID, kind TenantKind, name string) (Tenant, error) {
	id := uuid.New()
	path := "/" + id.String() + "/"
	if parentID == nil {
		if kind != KindProvider {
			return Tenant{}, fmt.Errorf("tenant sem pai precisa ser provider")
		}
	} else {
		parent, err := s.GetTenant(ctx, *parentID)
		if err != nil {
			return Tenant{}, err
		}
		if !allowedChild(parent.Kind, kind) {
			return Tenant{}, fmt.Errorf("um %s não pode ter filho %s", parent.Kind, kind)
		}
		path = parent.Path + id.String() + "/"
	}
	t, err := scanTenant(s.db.QueryRow(ctx, `
		INSERT INTO tenants (id, parent_id, kind, name, path) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+tenantCols, id, parentID, kind, name, path))
	if err != nil {
		return t, err
	}
	// MSP nasce sem recursos: o admin define a cota depois. Contagens ficam
	// sem limite até serem definidas.
	if kind == KindPartner {
		_, err = s.db.Exec(ctx, `INSERT INTO msp_limits (tenant_id) VALUES ($1)`, id)
	}
	return t, err
}

func (s *Store) GetTenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	t, err := scanTenant(s.db.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = $1`, id))
	return t, notFound(err)
}

// ListTenants lista os tenants dentro do escopo (o próprio e descendentes).
func (s *Store) ListTenants(ctx context.Context, scope Scope) ([]Tenant, error) {
	where, args := scope.sqlFilter("t.path", "t.id", "NULL::uuid", 1)
	rows, err := s.db.Query(ctx, `SELECT `+tenantColsT+` FROM tenants t WHERE `+where+` ORDER BY t.path`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) { return scanTenant(r) })
}

const tenantColsT = `t.id, t.parent_id, t.kind, t.name, t.path, t.status, t.created_at`

// ---------- projects (VDCs) ----------

const projectCols = `p.id, p.tenant_id, p.name, p.pool_name, p.created_at, p.cluster_id, COALESCE(c.location, ''), p.cpu_cores, p.memory_mb, p.bridge, p.vlan_tag`

// projectFrom junta o cluster (só para o rótulo de localização) e o uso atual
// calculado a partir dos recursos sincronizados.
const projectFrom = ` FROM projects p
	JOIN tenants t ON t.id = p.tenant_id
	LEFT JOIN clusters c ON c.id = p.cluster_id
	LEFT JOIN LATERAL (
		SELECT COALESCE(sum(r.cpus) FILTER (WHERE r.status = 'running'), 0) AS run_cpu,
		       COALESCE(sum(r.maxmem_bytes) FILTER (WHERE r.status = 'running'), 0) AS run_mem,
		       count(*) AS vms,
		       COALESCE(sum(r.maxdisk_bytes), 0) AS disk
		FROM resources r WHERE r.project_id = p.id AND NOT r.orphaned
	) u ON true`

const projectUsageCols = `, u.run_cpu::bigint, (u.run_mem / 1048576)::bigint, u.vms, (u.disk / 1073741824)::bigint`

func scanProject(row pgx.Row) (Project, error) {
	var p Project
	var u VDCUsage
	err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.PoolName, &p.CreatedAt, &p.ClusterID, &p.Location,
		&p.CPUCores, &p.MemoryMB, &p.Bridge, &p.VLANTag, &u.RunningCPU, &u.RunningMemoryMB, &u.VMs, &u.DiskGB)
	p.Usage = &u
	return p, err
}

// VDCSpec descreve um VDC a criar ou a nova cota de um existente.
type VDCSpec struct {
	TenantID  uuid.UUID
	Name      string
	ClusterID uuid.UUID
	CPUCores  int
	MemoryMB  int64
	Storage   map[uuid.UUID]int64 // classe -> GB
}

// CreateProject grava o VDC. As regras de cota ficam no pacote quota, que
// chama isto dentro da transação que trava o MSP.
func (s *Store) CreateProject(ctx context.Context, spec VDCSpec) (Project, error) {
	id := uuid.New()
	// Pool do Proxmox: só [A-Za-z0-9_-]; prefixos curtos mantêm o nome legível.
	pool := fmt.Sprintf("t-%s-%s", spec.TenantID.String()[:8], id.String()[:8])
	_, err := s.db.Exec(ctx, `
		INSERT INTO projects (id, tenant_id, name, pool_name, cluster_id, cpu_cores, memory_mb)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, spec.TenantID, spec.Name, pool, spec.ClusterID, spec.CPUCores, spec.MemoryMB)
	if isUniqueViolation(err) {
		return Project{}, ErrConflict
	}
	if err != nil {
		return Project{}, err
	}
	if err := s.setVDCStorage(ctx, id, spec.Storage); err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, id)
}

// UpdateProjectQuota troca a cota de um VDC (nome e cluster não mudam).
func (s *Store) UpdateProjectQuota(ctx context.Context, id uuid.UUID, spec VDCSpec) (Project, error) {
	if _, err := s.db.Exec(ctx, `UPDATE projects SET cpu_cores = $2, memory_mb = $3 WHERE id = $1`,
		id, spec.CPUCores, spec.MemoryMB); err != nil {
		return Project{}, err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM vdc_storage WHERE project_id = $1`, id); err != nil {
		return Project{}, err
	}
	if err := s.setVDCStorage(ctx, id, spec.Storage); err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, id)
}

func (s *Store) setVDCStorage(ctx context.Context, id uuid.UUID, st map[uuid.UUID]int64) error {
	for class, gb := range st {
		if _, err := s.db.Exec(ctx, `INSERT INTO vdc_storage (project_id, class_id, capacity_gb) VALUES ($1, $2, $3)`,
			id, class, gb); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetProject(ctx context.Context, id uuid.UUID) (Project, error) {
	p, err := scanProject(s.db.QueryRow(ctx, `SELECT `+projectCols+projectUsageCols+projectFrom+` WHERE p.id = $1`, id))
	if err != nil {
		return p, notFound(err)
	}
	ps := []Project{p}
	if err := s.loadVDCStorage(ctx, ps); err != nil {
		return p, err
	}
	return ps[0], nil
}

// ListProjects lista os VDCs de um tenant.
func (s *Store) ListProjects(ctx context.Context, tenantID uuid.UUID) ([]Project, error) {
	return s.listProjects(ctx, `p.tenant_id = $1`, tenantID)
}

// ListProjectsUnder lista os VDCs de toda a subárvore (MSP e seus clientes).
func (s *Store) ListProjectsUnder(ctx context.Context, tenantPath string) ([]Project, error) {
	return s.listProjects(ctx, `starts_with(t.path, $1)`, tenantPath)
}

// ListProjectsScope lista os VDCs que o escopo enxerga (vínculo num VDC
// só enxerga aquele VDC).
func (s *Store) ListProjectsScope(ctx context.Context, scope Scope) ([]Project, error) {
	where, args := scope.sqlFilter("t.path", "t.id", "p.id", 1)
	return s.listProjects(ctx, where, args...)
}

func (s *Store) listProjects(ctx context.Context, where string, args ...any) ([]Project, error) {
	rows, err := s.db.Query(ctx, `SELECT `+projectCols+projectUsageCols+`, t.name`+projectFrom+` WHERE `+where+` ORDER BY t.name, p.name`, args...)
	if err != nil {
		return nil, err
	}
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) {
		var p Project
		var u VDCUsage
		err := r.Scan(&p.ID, &p.TenantID, &p.Name, &p.PoolName, &p.CreatedAt, &p.ClusterID, &p.Location,
			&p.CPUCores, &p.MemoryMB, &p.Bridge, &p.VLANTag, &u.RunningCPU, &u.RunningMemoryMB, &u.VMs, &u.DiskGB, &p.TenantName)
		p.Usage = &u
		return p, err
	})
	if err != nil {
		return nil, err
	}
	return ps, s.loadVDCStorage(ctx, ps)
}

func (s *Store) loadVDCStorage(ctx context.Context, ps []Project) error {
	if len(ps) == 0 {
		return nil
	}
	idx := map[uuid.UUID]int{}
	ids := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		idx[p.ID] = i
		ids[i] = p.ID
		ps[i].Storage = []VDCStorage{}
	}
	rows, err := s.db.Query(ctx, `
		SELECT vs.project_id, vs.class_id, sc.name, vs.capacity_gb,
		       COALESCE((SELECT sum(d.size_gb) FROM resource_disks d JOIN resources r ON r.id = d.resource_id
		                 WHERE r.project_id = vs.project_id AND d.class_id = vs.class_id AND NOT r.orphaned), 0)::bigint
		FROM vdc_storage vs JOIN storage_classes sc ON sc.id = vs.class_id
		WHERE vs.project_id = ANY($1) ORDER BY sc.name`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid uuid.UUID
		var st VDCStorage
		if err := rows.Scan(&pid, &st.ClassID, &st.ClassName, &st.CapacityGB, &st.UsedGB); err != nil {
			return err
		}
		i := idx[pid]
		ps[i].Storage = append(ps[i].Storage, st)
	}
	return rows.Err()
}

// ---------- memberships ----------

// ListMemberships lista os vínculos diretos de um tenant (sem descendentes).
func (s *Store) ListMemberships(ctx context.Context, tenantID uuid.UUID) ([]MembershipView, error) {
	rows, err := s.db.Query(ctx, `
		SELECT m.id, m.subject, m.project_id, p.name, m.role, m.created_at,
		       u.id IS NOT NULL, COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(u.totp_enabled, false), u.last_login_at
		FROM memberships m
		LEFT JOIN projects p ON p.id = m.project_id
		LEFT JOIN users u ON u.username = m.subject
		WHERE m.tenant_id = $1 ORDER BY m.subject, m.role`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MembershipView, error) {
		var m MembershipView
		err := r.Scan(&m.ID, &m.Subject, &m.ProjectID, &m.ProjectName, &m.Role, &m.CreatedAt,
			&m.Local, &m.Name, &m.Email, &m.TOTPEnabled, &m.LastLoginAt)
		return m, err
	})
}

func (s *Store) CreateMembership(ctx context.Context, subject string, tenantID uuid.UUID, projectID *uuid.UUID, role string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO memberships (subject, tenant_id, project_id, role) VALUES ($1, $2, $3, $4)`,
		subject, tenantID, projectID, role)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// MembershipsBySubject devolve os vínculos ativos do usuário (tenants
// suspensos ou encerrados não concedem nada).
func (s *Store) MembershipsBySubject(ctx context.Context, subject string) ([]Membership, error) {
	rows, err := s.db.Query(ctx, `
		SELECT m.id, m.subject, m.tenant_id, t.path, t.kind, m.project_id, m.role
		FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.subject = $1 AND t.status = 'active'`, subject)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Membership, error) {
		var m Membership
		err := r.Scan(&m.ID, &m.Subject, &m.TenantID, &m.TenantPath, &m.TenantKind, &m.ProjectID, &m.Role)
		return m, err
	})
}

// ---------- clusters ----------

const clusterCols = `id, name, dc, location, api_url, ca_cert_pem, token_id, token_secret_ref, token_secret_enc, default_bridge, cpu_ratio::float8, pve_version, nodes_online, nodes_total, cpu_total, memory_total_mb, enabled, last_sync_at, last_sync_error, created_at`

func scanCluster(row pgx.Row) (Cluster, error) {
	var c Cluster
	err := row.Scan(&c.ID, &c.Name, &c.DC, &c.Location, &c.APIURL, &c.CACertPEM, &c.TokenID, &c.TokenSecretRef, &c.TokenSecretEnc, &c.DefaultBridge, &c.CPURatio, &c.PVEVersion, &c.NodesOnline, &c.NodesTotal, &c.CPUTotal, &c.MemoryTotalMB, &c.Enabled, &c.LastSyncAt, &c.LastSyncError, &c.CreatedAt)
	return c, err
}

func (s *Store) CreateCluster(ctx context.Context, c Cluster) (Cluster, error) {
	if c.DefaultBridge == "" {
		c.DefaultBridge = "vmbr0"
	}
	if c.CPURatio < 1 {
		c.CPURatio = 1
	}
	out, err := scanCluster(s.db.QueryRow(ctx, `
		INSERT INTO clusters (name, dc, location, api_url, ca_cert_pem, token_id, token_secret_ref, token_secret_enc, default_bridge, cpu_ratio, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+clusterCols,
		c.Name, c.DC, c.Location, c.APIURL, c.CACertPEM, c.TokenID, c.TokenSecretRef, c.TokenSecretEnc, c.DefaultBridge, c.CPURatio, ""))
	if isUniqueViolation(err) {
		return Cluster{}, ErrConflict
	}
	return out, err
}

func (s *Store) GetCluster(ctx context.Context, id uuid.UUID) (Cluster, error) {
	c, err := scanCluster(s.db.QueryRow(ctx, `SELECT `+clusterCols+` FROM clusters WHERE id = $1`, id))
	return c, notFound(err)
}

func (s *Store) ListClusters(ctx context.Context, onlyEnabled bool) ([]Cluster, error) {
	rows, err := s.db.Query(ctx, `SELECT `+clusterCols+` FROM clusters WHERE (NOT $1 OR enabled) ORDER BY location, name`, onlyEnabled)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Cluster, error) { return scanCluster(r) })
}

func (s *Store) MarkClusterSynced(ctx context.Context, id uuid.UUID, syncErr error) error {
	var msg *string
	if syncErr != nil {
		m := syncErr.Error()
		msg = &m
	}
	_, err := s.db.Exec(ctx, `UPDATE clusters SET last_sync_at = now(), last_sync_error = $2 WHERE id = $1`, id, msg)
	return err
}

// ---------- resources ----------

// SyncResult resume uma rodada de sincronização de um cluster.
type SyncResult struct {
	Seen, Unassigned, Orphaned int
}

// ApplySnapshots grava o que o sincronizador viu num cluster. Deve rodar em
// transação (WithTx): marca como órfão o que não apareceu nesta rodada.
// O pool do Proxmox decide tenant e projeto; pool desconhecido = sem dono.
func (s *Store) ApplySnapshots(ctx context.Context, clusterID uuid.UUID, snaps []Snapshot) (SyncResult, error) {
	var res SyncResult
	pools := map[string]Project{}
	// Só valem os VDCs deste cluster: uma VM num pool de VDC de outro cluster
	// fica sem dono, em vez de consumir a cota errada.
	rows, err := s.db.Query(ctx, `SELECT id, tenant_id, name, pool_name, created_at FROM projects WHERE cluster_id = $1`, clusterID)
	if err != nil {
		return res, err
	}
	projects, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) {
		var p Project
		err := r.Scan(&p.ID, &p.TenantID, &p.Name, &p.PoolName, &p.CreatedAt)
		return p, err
	})
	if err != nil {
		return res, err
	}
	for _, p := range projects {
		pools[p.PoolName] = p
	}

	batch := &pgx.Batch{}
	for _, sn := range snaps {
		var tenantID, projectID *uuid.UUID
		if p, ok := pools[sn.Pool]; ok {
			tenantID, projectID = &p.TenantID, &p.ID
		} else {
			res.Unassigned++
		}
		alias := sn.Name
		if alias == "" {
			alias = fmt.Sprintf("%s-%d", sn.Kind, sn.VMID)
		}
		batch.Queue(`
			INSERT INTO resources (alias, kind, tenant_id, project_id, cluster_id, node, vmid,
			                       status, cpus, maxmem_bytes, maxdisk_bytes, orphaned, last_seen_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, false, now())
			ON CONFLICT (cluster_id, vmid) DO UPDATE SET
				kind = EXCLUDED.kind, tenant_id = EXCLUDED.tenant_id, project_id = EXCLUDED.project_id,
				node = EXCLUDED.node, status = EXCLUDED.status, cpus = EXCLUDED.cpus,
				maxmem_bytes = EXCLUDED.maxmem_bytes, maxdisk_bytes = EXCLUDED.maxdisk_bytes,
				orphaned = false, last_seen_at = now()`,
			alias, sn.Kind, tenantID, projectID, clusterID, sn.Node, sn.VMID,
			sn.Status, sn.CPUs, sn.MaxMemBytes, sn.MaxDiskBytes)
		res.Seen++
	}
	if err := s.db.SendBatch(ctx, batch).Close(); err != nil {
		return res, err
	}

	// now() é o instante de início da transação: tudo que foi tocado acima tem
	// last_seen_at = now(); o resto sumiu do cluster.
	tag, err := s.db.Exec(ctx, `
		UPDATE resources SET orphaned = true
		WHERE cluster_id = $1 AND last_seen_at < now() AND NOT orphaned`, clusterID)
	if err != nil {
		return res, err
	}
	res.Orphaned = int(tag.RowsAffected())
	return res, nil
}

const resourceColsR = `r.id, r.alias, r.kind, r.tenant_id, t.name, r.project_id, p.name, r.status, r.cpus, r.maxmem_bytes, r.maxdisk_bytes, r.deleting, c.location, r.last_seen_at`

// resourceFrom junta tenant (para o filtro de escopo), projeto (para o nome)
// e cluster (só para o rótulo de localização).
const resourceFrom = ` FROM resources r LEFT JOIN tenants t ON t.id = r.tenant_id LEFT JOIN projects p ON p.id = r.project_id JOIN clusters c ON c.id = r.cluster_id`

func scanResource(row pgx.Row) (Resource, error) {
	var r Resource
	err := row.Scan(&r.ID, &r.Alias, &r.Kind, &r.TenantID, &r.TenantName, &r.ProjectID, &r.ProjectName, &r.Status, &r.CPUs, &r.MaxMemBytes, &r.MaxDiskBytes, &r.Deleting, &r.Location, &r.LastSeenAt)
	return r, err
}

func (s *Store) ListResources(ctx context.Context, scope Scope) ([]Resource, error) {
	where, args := scope.sqlFilter("t.path", "r.tenant_id", "r.project_id", 1)
	rows, err := s.db.Query(ctx, `
		SELECT `+resourceColsR+resourceFrom+`
		WHERE NOT r.orphaned AND `+where+` ORDER BY r.alias, r.id`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Resource, error) { return scanResource(r) })
}

// GetResource devolve o recurso só se ele estiver no escopo; fora dele é
// ErrNotFound (não revelamos que o ID existe).
func (s *Store) GetResource(ctx context.Context, scope Scope, id uuid.UUID) (Resource, error) {
	where, args := scope.sqlFilter("t.path", "r.tenant_id", "r.project_id", 2)
	r, err := scanResource(s.db.QueryRow(ctx, `
		SELECT `+resourceColsR+resourceFrom+`
		WHERE r.id = $1 AND NOT r.orphaned AND `+where, append([]any{id}, args...)...))
	return r, notFound(err)
}

// Location é para workers: devolve o endereço interno sem checar escopo.
func (s *Store) Location(ctx context.Context, resourceID uuid.UUID) (ResourceLocation, error) {
	var l ResourceLocation
	err := s.db.QueryRow(ctx, `SELECT id, cluster_id, node, vmid, kind FROM resources WHERE id = $1`, resourceID).
		Scan(&l.ResourceID, &l.ClusterID, &l.Node, &l.VMID, &l.Kind)
	return l, notFound(err)
}

func (s *Store) SetResourceStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := s.db.Exec(ctx, `UPDATE resources SET status = $2 WHERE id = $1`, id, status)
	return err
}

// ---------- tasks ----------

const taskColsK = `k.id, k.resource_id, k.project_id, k.label, k.action, k.status, k.requested_by, k.error, k.created_at, k.started_at, k.finished_at`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.ResourceID, &t.ProjectID, &t.Label, &t.Action, &t.Status, &t.RequestedBy, &t.Error, &t.CreatedAt, &t.StartedAt, &t.FinishedAt)
	return t, err
}

// NewTask descreve uma Task a criar. ResourceID é nil na criação de VM.
type NewTask struct {
	ResourceID  *uuid.UUID
	TenantID    *uuid.UUID
	ProjectID   *uuid.UUID
	Action      string
	RequestedBy string
	Label       string
	Params      map[string]any
}

// CreateTask cria a Task na fila. Já existindo uma ativa para o recurso,
// devolve ErrConflict (índice parcial único).
func (s *Store) CreateTask(ctx context.Context, resourceID uuid.UUID, tenantID *uuid.UUID, action, requestedBy string) (Task, error) {
	var projectID *uuid.UUID
	if err := s.db.QueryRow(ctx, `SELECT project_id FROM resources WHERE id = $1`, resourceID).Scan(&projectID); err != nil {
		return Task{}, notFound(err)
	}
	return s.InsertTask(ctx, NewTask{ResourceID: &resourceID, TenantID: tenantID, ProjectID: projectID, Action: action, RequestedBy: requestedBy})
}

func (s *Store) InsertTask(ctx context.Context, n NewTask) (Task, error) {
	params := n.Params
	if params == nil {
		params = map[string]any{}
	}
	b, err := json.Marshal(params)
	if err != nil {
		return Task{}, err
	}
	t, err := scanTask(s.db.QueryRow(ctx, `
		INSERT INTO tasks AS k (resource_id, tenant_id, project_id, action, requested_by, label, params)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+taskColsK, n.ResourceID, n.TenantID, n.ProjectID, n.Action, n.RequestedBy, n.Label, b))
	if isUniqueViolation(err) {
		return Task{}, ErrConflict
	}
	return t, err
}

// taskFrom: a Task pode não ter recurso (criação); o escopo usa o tenant e
// o VDC gravados na própria Task.
const taskFrom = ` FROM tasks k LEFT JOIN resources r ON r.id = k.resource_id LEFT JOIN tenants t ON t.id = k.tenant_id`

func (s *Store) GetTask(ctx context.Context, scope Scope, id uuid.UUID) (Task, error) {
	where, args := scope.sqlFilter("t.path", "k.tenant_id", "COALESCE(k.project_id, r.project_id)", 2)
	t, err := scanTask(s.db.QueryRow(ctx, `SELECT `+taskColsK+taskFrom+` WHERE k.id = $1 AND `+where, append([]any{id}, args...)...))
	return t, notFound(err)
}

// ListTasks devolve as Tasks mais recentes de um recurso. Quem chama já
// confirmou que o recurso está no escopo do usuário.
func (s *Store) ListTasks(ctx context.Context, resourceID uuid.UUID, limit int) ([]Task, error) {
	rows, err := s.db.Query(ctx, `SELECT `+taskColsK+` FROM tasks k WHERE k.resource_id = $1
		ORDER BY k.created_at DESC LIMIT $2`, resourceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Task, error) { return scanTask(r) })
}

// TaskView é uma Task com o nome do servidor, para listas.
type TaskView struct {
	Task
	ResourceAlias string  `json:"resource_alias"`
	TenantName    *string `json:"tenant_name,omitempty"`
}

// ListTasksScope devolve as Tasks mais recentes no escopo.
func (s *Store) ListTasksScope(ctx context.Context, scope Scope, limit int) ([]TaskView, error) {
	where, args := scope.sqlFilter("t.path", "k.tenant_id", "COALESCE(k.project_id, r.project_id)", 2)
	rows, err := s.db.Query(ctx, `
		SELECT `+taskColsK+`, COALESCE(r.alias, NULLIF(k.label, ''), '—'), t.name`+taskFrom+`
		WHERE `+where+` ORDER BY k.created_at DESC LIMIT $1`, append([]any{limit}, args...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TaskView, error) {
		var v TaskView
		err := r.Scan(&v.ID, &v.ResourceID, &v.ProjectID, &v.Label, &v.Action, &v.Status, &v.RequestedBy, &v.Error, &v.CreatedAt,
			&v.StartedAt, &v.FinishedAt, &v.ResourceAlias, &v.TenantName)
		return v, err
	})
}

// WorkerTask é a Task como o worker a lê (com parâmetros e UPID).
type WorkerTask struct {
	Task
	TenantID *uuid.UUID
	UPID     *string
	Params   map[string]any
}

// TaskForWorker lê a Task sem escopo (uso exclusivo do worker).
func (s *Store) TaskForWorker(ctx context.Context, id uuid.UUID) (Task, *string, error) {
	wt, err := s.WorkerTask(ctx, id)
	return wt.Task, wt.UPID, err
}

func (s *Store) WorkerTask(ctx context.Context, id uuid.UUID) (WorkerTask, error) {
	var wt WorkerTask
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT `+taskColsK+`, k.tenant_id, k.upid, k.params FROM tasks k WHERE k.id = $1`, id).
		Scan(&wt.ID, &wt.ResourceID, &wt.ProjectID, &wt.Label, &wt.Action, &wt.Status, &wt.RequestedBy, &wt.Error,
			&wt.CreatedAt, &wt.StartedAt, &wt.FinishedAt, &wt.TenantID, &wt.UPID, &raw)
	if err != nil {
		return wt, notFound(err)
	}
	wt.Params = map[string]any{}
	err = json.Unmarshal(raw, &wt.Params)
	return wt, err
}

// MergeTaskParams grava passos já feitos (vmid escolhido, UPIDs), para que
// um retry continue de onde parou em vez de repetir.
func (s *Store) MergeTaskParams(ctx context.Context, id uuid.UUID, kv map[string]any) error {
	b, err := json.Marshal(kv)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE tasks SET params = params || $2::jsonb WHERE id = $1`, id, b)
	return err
}

func (s *Store) SetTaskResource(ctx context.Context, id, resourceID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE tasks SET resource_id = $2 WHERE id = $1`, id, resourceID)
	return err
}

func (s *Store) MarkTaskRunning(ctx context.Context, id uuid.UUID, upid string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE tasks SET status = 'running', upid = COALESCE(NULLIF($2, ''), upid), started_at = COALESCE(started_at, now())
		WHERE id = $1`, id, upid)
	return err
}

func (s *Store) FinishTask(ctx context.Context, id uuid.UUID, ok bool, errText string) error {
	status := "succeeded"
	var e *string
	if !ok {
		status = "failed"
		e = &errText
	}
	_, err := s.db.Exec(ctx, `
		UPDATE tasks SET status = $2, error = $3, finished_at = now() WHERE id = $1`, id, status, e)
	return err
}

// ---------- audit ----------

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO audit_log (subject, actor_tenant_id, target_tenant_id, action, target, details, remote_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.Subject, e.ActorTenantID, e.TargetTenantID, e.Action, e.Target, b, e.RemoteIP)
	return err
}
