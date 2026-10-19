package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------- templates e imagens ----------

// PVETemplate é um template visto no cluster (só para a equipe).
type PVETemplate struct {
	ClusterID    uuid.UUID  `json:"cluster_id"`
	VMID         int        `json:"vmid"`
	Node         string     `json:"node"`
	Name         string     `json:"name"`
	MaxDiskBytes int64      `json:"max_disk_bytes"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	ImageID      *uuid.UUID `json:"image_id,omitempty"`
	ImageName    *string    `json:"image_name,omitempty"`
}

// SaveTemplates troca a lista de templates do cluster pela vista agora.
func (s *Store) SaveTemplates(ctx context.Context, clusterID uuid.UUID, ts []PVETemplate) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM pve_templates WHERE cluster_id = $1`, clusterID); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := s.db.Exec(ctx, `
			INSERT INTO pve_templates (cluster_id, vmid, node, name, kind, maxdisk_bytes) VALUES ($1, $2, $3, $4, 'qemu', $5)`,
			clusterID, t.VMID, t.Node, t.Name, t.MaxDiskBytes); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListTemplates(ctx context.Context, clusterID uuid.UUID) ([]PVETemplate, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.cluster_id, t.vmid, t.node, t.name, t.maxdisk_bytes, t.last_seen_at, i.id, i.name
		FROM pve_templates t LEFT JOIN images i ON i.cluster_id = t.cluster_id AND i.vmid = t.vmid
		WHERE t.cluster_id = $1 ORDER BY t.name, t.vmid`, clusterID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PVETemplate, error) {
		var t PVETemplate
		err := r.Scan(&t.ClusterID, &t.VMID, &t.Node, &t.Name, &t.MaxDiskBytes, &t.LastSeenAt, &t.ImageID, &t.ImageName)
		return t, err
	})
}

// Template devolve um template do cluster (node onde ele está).
func (s *Store) Template(ctx context.Context, clusterID uuid.UUID, vmid int) (PVETemplate, error) {
	var t PVETemplate
	err := s.db.QueryRow(ctx, `SELECT cluster_id, vmid, node, name, maxdisk_bytes, last_seen_at FROM pve_templates
		WHERE cluster_id = $1 AND vmid = $2`, clusterID, vmid).
		Scan(&t.ClusterID, &t.VMID, &t.Node, &t.Name, &t.MaxDiskBytes, &t.LastSeenAt)
	return t, notFound(err)
}

// Image é um template publicado no catálogo. VMID só sai para a equipe.
type Image struct {
	ID         uuid.UUID `json:"id"`
	LocationID uuid.UUID `json:"location_id"`
	Location   string    `json:"location"`
	VMID       int       `json:"-"`
	Name       string    `json:"name"`
	OSFamily   string    `json:"os_family"`
	MinDiskGB  int       `json:"min_disk_gb"`
	Enabled    bool      `json:"enabled"`
	// Available = o template ainda existe no cluster.
	Available bool      `json:"available"`
	CreatedAt time.Time `json:"created_at"`
}

const imageCols = `i.id, i.cluster_id, c.location, i.vmid, i.name, i.os_family, i.min_disk_gb, i.enabled,
	EXISTS (SELECT 1 FROM pve_templates t WHERE t.cluster_id = i.cluster_id AND t.vmid = i.vmid), i.created_at`

func scanImage(row pgx.Row) (Image, error) {
	var i Image
	err := row.Scan(&i.ID, &i.LocationID, &i.Location, &i.VMID, &i.Name, &i.OSFamily, &i.MinDiskGB, &i.Enabled, &i.Available, &i.CreatedAt)
	return i, err
}

func (s *Store) CreateImage(ctx context.Context, clusterID uuid.UUID, vmid int, name, osFamily string, minDisk int) (Image, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `INSERT INTO images (cluster_id, vmid, name, os_family, min_disk_gb) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		clusterID, vmid, name, osFamily, minDisk).Scan(&id)
	if isUniqueViolation(err) {
		return Image{}, ErrConflict
	}
	if err != nil {
		return Image{}, err
	}
	return s.GetImage(ctx, id)
}

func (s *Store) GetImage(ctx context.Context, id uuid.UUID) (Image, error) {
	i, err := scanImage(s.db.QueryRow(ctx, `SELECT `+imageCols+` FROM images i JOIN clusters c ON c.id = i.cluster_id WHERE i.id = $1`, id))
	return i, notFound(err)
}

// ListImages lista o catálogo; clusterID nil = todas as localizações.
func (s *Store) ListImages(ctx context.Context, clusterID *uuid.UUID, onlyEnabled bool) ([]Image, error) {
	rows, err := s.db.Query(ctx, `SELECT `+imageCols+` FROM images i JOIN clusters c ON c.id = i.cluster_id
		WHERE ($1::uuid IS NULL OR i.cluster_id = $1) AND (NOT $2 OR i.enabled) ORDER BY c.location, i.name`, clusterID, onlyEnabled)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Image, error) { return scanImage(r) })
}

func (s *Store) DeleteImage(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM images WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---------- recursos criados pelo portal ----------

// UpsertResource grava a VM que o worker acabou de criar, sem esperar o
// próximo ciclo do sincronizador.
func (s *Store) UpsertResource(ctx context.Context, clusterID uuid.UUID, sn Snapshot, tenantID, projectID *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO resources (alias, kind, tenant_id, project_id, cluster_id, node, vmid, status, cpus, maxmem_bytes, maxdisk_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (cluster_id, vmid) DO UPDATE SET
			alias = EXCLUDED.alias, kind = EXCLUDED.kind, tenant_id = EXCLUDED.tenant_id, project_id = EXCLUDED.project_id,
			node = EXCLUDED.node, status = EXCLUDED.status, cpus = EXCLUDED.cpus, maxmem_bytes = EXCLUDED.maxmem_bytes,
			maxdisk_bytes = EXCLUDED.maxdisk_bytes, orphaned = false, deleting = false, last_seen_at = now()
		RETURNING id`,
		sn.Name, sn.Kind, tenantID, projectID, clusterID, sn.Node, sn.VMID, sn.Status, sn.CPUs, sn.MaxMemBytes, sn.MaxDiskBytes).Scan(&id)
	return id, err
}

// SetResourceDisks troca os discos registrados de um recurso (GB por classe).
func (s *Store) SetResourceDisks(ctx context.Context, resourceID uuid.UUID, byClass map[uuid.UUID]int64) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM resource_disks WHERE resource_id = $1`, resourceID); err != nil {
		return err
	}
	for class, gb := range byClass {
		if gb <= 0 {
			continue
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO resource_disks (resource_id, class_id, size_gb) VALUES ($1, $2, $3)`,
			resourceID, class, gb); err != nil {
			return err
		}
	}
	return nil
}

// SetResourceOwner troca VDC e tenant de um recurso (atribuição).
func (s *Store) SetResourceOwner(ctx context.Context, resourceID uuid.UUID, tenantID, projectID *uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE resources SET tenant_id = $2, project_id = $3 WHERE id = $1`, resourceID, tenantID, projectID)
	return err
}

func (s *Store) SetResourceDeleting(ctx context.Context, id uuid.UUID, deleting bool) error {
	_, err := s.db.Exec(ctx, `UPDATE resources SET deleting = $2 WHERE id = $1`, id, deleting)
	return err
}

// MarkResourceGone tira o recurso do portal depois de excluído no cluster.
func (s *Store) MarkResourceGone(ctx context.Context, id uuid.UUID) error {
	if _, err := s.db.Exec(ctx, `UPDATE resources SET orphaned = true, deleting = false, tenant_id = NULL, project_id = NULL WHERE id = $1`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM resource_disks WHERE resource_id = $1`, id)
	return err
}

// ResourceInternal é a visão completa de um recurso, só para backend.
type ResourceInternal struct {
	ResourceLocation
	Alias       string
	Status      string
	TenantID    *uuid.UUID
	ProjectID   *uuid.UUID
	CPUs        int
	MaxMemBytes int64
}

func (s *Store) ResourceInternal(ctx context.Context, id uuid.UUID) (ResourceInternal, error) {
	var r ResourceInternal
	err := s.db.QueryRow(ctx, `SELECT id, cluster_id, node, vmid, kind, alias, status, tenant_id, project_id, cpus, maxmem_bytes
		FROM resources WHERE id = $1`, id).
		Scan(&r.ResourceID, &r.ClusterID, &r.Node, &r.VMID, &r.Kind, &r.Alias, &r.Status, &r.TenantID, &r.ProjectID, &r.CPUs, &r.MaxMemBytes)
	return r, notFound(err)
}

// ---------- rede do VDC ----------

func (s *Store) SetProjectNetwork(ctx context.Context, id uuid.UUID, bridge *string, vlan *int) error {
	_, err := s.db.Exec(ctx, `UPDATE projects SET bridge = $2, vlan_tag = $3 WHERE id = $1`, id, bridge, vlan)
	return err
}

func (s *Store) SetPoolState(ctx context.Context, id uuid.UUID, ready bool, errText *string) error {
	_, err := s.db.Exec(ctx, `UPDATE projects SET pool_ready = $2, pool_error = $3 WHERE id = $1`, id, ready, errText)
	return err
}

func (s *Store) ClusterDefaultBridge(ctx context.Context, clusterID uuid.UUID) (string, error) {
	var b string
	err := s.db.QueryRow(ctx, `SELECT default_bridge FROM clusters WHERE id = $1`, clusterID).Scan(&b)
	return b, notFound(err)
}

// StoragePolicyFor devolve o storage do PVE de uma classe num cluster.
func (s *Store) StoragePolicyFor(ctx context.Context, clusterID, classID uuid.UUID) (string, error) {
	var st string
	err := s.db.QueryRow(ctx, `SELECT pve_storage FROM storage_policies WHERE cluster_id = $1 AND class_id = $2`, clusterID, classID).Scan(&st)
	return st, notFound(err)
}

// ---------- contas para a cota de criação ----------

// ProjectPendingCreates soma o que as criações ainda na fila vão ocupar no
// VDC: disco por classe e, para as que ligam no fim, CPU e memória.
func (s *Store) ProjectPendingCreates(ctx context.Context, projectID uuid.UUID) (diskByClass map[uuid.UUID]int64, cpu int, memMB int64, err error) {
	diskByClass = map[uuid.UUID]int64{}
	rows, err := s.db.Query(ctx, `
		SELECT (params->>'storage_class_id')::uuid, COALESCE((params->>'disk_gb')::bigint, 0),
		       COALESCE((params->>'start')::boolean, false), COALESCE((params->>'cpu_cores')::int, 0), COALESCE((params->>'memory_mb')::bigint, 0)
		FROM tasks WHERE project_id = $1 AND action = 'create' AND status IN ('queued', 'running') AND resource_id IS NULL`, projectID)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var class uuid.UUID
		var gb, m int64
		var start bool
		var c int
		if err := rows.Scan(&class, &gb, &start, &c, &m); err != nil {
			return nil, 0, 0, err
		}
		diskByClass[class] += gb
		if start {
			cpu += c
			memMB += m
		}
	}
	return diskByClass, cpu, memMB, rows.Err()
}

// MSPVMCount conta VMs da subárvore do MSP, incluindo criações na fila.
func (s *Store) MSPVMCount(ctx context.Context, mspPath string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM resources r JOIN tenants t ON t.id = r.tenant_id WHERE starts_with(t.path, $1) AND NOT r.orphaned)
		     + (SELECT count(*) FROM tasks k JOIN tenants t ON t.id = k.tenant_id
		        WHERE starts_with(t.path, $1) AND k.action = 'create' AND k.status IN ('queued', 'running') AND k.resource_id IS NULL)`,
		mspPath).Scan(&n)
	return n, err
}

// UsedDiskByClass soma os discos registrados das VMs do VDC.
func (s *Store) UsedDiskByClass(ctx context.Context, projectID uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.class_id, sum(d.size_gb)::bigint FROM resource_disks d JOIN resources r ON r.id = d.resource_id
		WHERE r.project_id = $1 AND NOT r.orphaned GROUP BY d.class_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var c uuid.UUID
		var gb int64
		if err := rows.Scan(&c, &gb); err != nil {
			return nil, err
		}
		out[c] = gb
	}
	return out, rows.Err()
}

// ClassForStorage resolve o storage do PVE de volta para a classe.
func (s *Store) ClassesByStorage(ctx context.Context, clusterID uuid.UUID) (map[string]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `SELECT pve_storage, class_id FROM storage_policies WHERE cluster_id = $1`, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uuid.UUID{}
	for rows.Next() {
		var st string
		var c uuid.UUID
		if err := rows.Scan(&st, &c); err != nil {
			return nil, err
		}
		out[st] = c
	}
	return out, rows.Err()
}
