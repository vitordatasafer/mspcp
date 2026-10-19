package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------- classes e políticas de storage ----------

type StorageClass struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

// StoragePolicy mapeia uma classe para um storage do Proxmox num cluster.
// Só a equipe da plataforma vê esse mapeamento.
type StoragePolicy struct {
	ID         uuid.UUID `json:"id"`
	ClusterID  uuid.UUID `json:"cluster_id"`
	ClassID    uuid.UUID `json:"class_id"`
	ClassName  string    `json:"class_name"`
	PVEStorage string    `json:"pve_storage"`
}

func (s *Store) CreateStorageClass(ctx context.Context, name, desc string) (StorageClass, error) {
	var c StorageClass
	err := s.db.QueryRow(ctx, `INSERT INTO storage_classes (name, description) VALUES ($1, $2) RETURNING id, name, description`, name, desc).
		Scan(&c.ID, &c.Name, &c.Description)
	if isUniqueViolation(err) {
		return c, ErrConflict
	}
	return c, err
}

func (s *Store) ListStorageClasses(ctx context.Context) ([]StorageClass, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, description FROM storage_classes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StorageClass, error) {
		var c StorageClass
		err := r.Scan(&c.ID, &c.Name, &c.Description)
		return c, err
	})
}

// SetStoragePolicy cria ou troca o storage do PVE de uma classe num cluster.
func (s *Store) SetStoragePolicy(ctx context.Context, clusterID, classID uuid.UUID, pveStorage string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO storage_policies (cluster_id, class_id, pve_storage) VALUES ($1, $2, $3)
		ON CONFLICT (cluster_id, class_id) DO UPDATE SET pve_storage = EXCLUDED.pve_storage`,
		clusterID, classID, pveStorage)
	return err
}

func (s *Store) ListStoragePolicies(ctx context.Context) ([]StoragePolicy, error) {
	rows, err := s.db.Query(ctx, `
		SELECT sp.id, sp.cluster_id, sp.class_id, sc.name, sp.pve_storage
		FROM storage_policies sp JOIN storage_classes sc ON sc.id = sp.class_id
		ORDER BY sp.cluster_id, sc.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StoragePolicy, error) {
		var p StoragePolicy
		err := r.Scan(&p.ID, &p.ClusterID, &p.ClassID, &p.ClassName, &p.PVEStorage)
		return p, err
	})
}

// ClassesInCluster devolve as classes que têm storage mapeado no cluster.
func (s *Store) ClassesInCluster(ctx context.Context, clusterID uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := s.db.Query(ctx, `SELECT class_id FROM storage_policies WHERE cluster_id = $1`, clusterID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// ---------- limites do MSP ----------

// MSPLimits é a cota do MSP. vCPU, memória e storage vêm das reservas por
// cluster (somadas aqui só para exibição); os limites de quantidade são
// globais. Ponteiro nil = ilimitado.
type MSPLimits struct {
	TenantID     uuid.UUID            `json:"tenant_id"`
	CPUCores     *int                 `json:"cpu_cores"`
	MemoryMB     *int64               `json:"memory_mb"`
	MaxCustomers *int                 `json:"max_customers"`
	MaxVDCs      *int                 `json:"max_vdcs"`
	MaxVMs       *int                 `json:"max_vms"`
	Clusters     []uuid.UUID          `json:"locations"`
	Storage      map[uuid.UUID]*int64 `json:"storage"` // classe -> GB somados das reservas
	Reservations []Reservation        `json:"reservations"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

// Reserva é o que um MSP contratou num cluster: vCPU, memória e storage
// por classe. Os VDCs do MSP naquele cluster saem daqui.
type Reservation struct {
	TenantID   uuid.UUID           `json:"tenant_id"`
	TenantName string              `json:"tenant_name,omitempty"`
	ClusterID  uuid.UUID           `json:"location_id"`
	Location   string              `json:"location"`
	CPUCores   int                 `json:"cpu_cores"`
	MemoryMB   int64               `json:"memory_mb"`
	Storage    map[uuid.UUID]int64 `json:"storage"`
	// Já distribuído em VDCs do MSP neste cluster.
	AllocatedCPU      int                 `json:"allocated_cpu"`
	AllocatedMemoryMB int64               `json:"allocated_memory_mb"`
	AllocatedStorage  map[uuid.UUID]int64 `json:"allocated_storage"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

// GetMSPLimits lê a cota. Com forUpdate, trava a linha do MSP até o fim da
// transação: é assim que dois VDCs simultâneos não estouram a cota juntos.
func (s *Store) GetMSPLimits(ctx context.Context, tenantID uuid.UUID, forUpdate bool) (MSPLimits, error) {
	l := MSPLimits{TenantID: tenantID, Storage: map[uuid.UUID]*int64{}, Clusters: []uuid.UUID{}}
	q := `SELECT max_customers, max_vdcs, max_vms, updated_at FROM msp_limits WHERE tenant_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := s.db.QueryRow(ctx, q, tenantID).Scan(&l.MaxCustomers, &l.MaxVDCs, &l.MaxVMs, &l.UpdatedAt); err != nil {
		return l, notFound(err)
	}
	rs, err := s.ListReservations(ctx, &tenantID, nil)
	if err != nil {
		return l, err
	}
	l.Reservations = rs
	cpu, mem := 0, int64(0)
	for _, r := range rs {
		cpu += r.CPUCores
		mem += r.MemoryMB
		l.Clusters = append(l.Clusters, r.ClusterID)
		for c, gb := range r.Storage {
			v := gb
			if cur := l.Storage[c]; cur != nil {
				v += *cur
			}
			l.Storage[c] = &v
		}
	}
	l.CPUCores, l.MemoryMB = &cpu, &mem
	return l, nil
}

// SaveMSPCounts grava os limites de quantidade (validação no pacote quota).
func (s *Store) SaveMSPCounts(ctx context.Context, l MSPLimits) error {
	_, err := s.db.Exec(ctx, `
		UPDATE msp_limits SET max_customers = $2, max_vdcs = $3, max_vms = $4, updated_at = now()
		WHERE tenant_id = $1`, l.TenantID, l.MaxCustomers, l.MaxVDCs, l.MaxVMs)
	return err
}

// ListReservations lista reservas por MSP e/ou cluster, com o que já foi
// distribuído em VDCs.
func (s *Store) ListReservations(ctx context.Context, tenantID, clusterID *uuid.UUID) ([]Reservation, error) {
	rows, err := s.db.Query(ctx, `
		SELECT r.tenant_id, t.name, r.cluster_id, c.location, r.cpu_cores, r.memory_mb, r.updated_at,
		       COALESCE(a.cpu, 0)::int, COALESCE(a.mem, 0)::bigint
		FROM msp_reservations r
		JOIN tenants t ON t.id = r.tenant_id
		JOIN clusters c ON c.id = r.cluster_id
		LEFT JOIN LATERAL (
			SELECT sum(p.cpu_cores) AS cpu, sum(p.memory_mb) AS mem
			FROM projects p JOIN tenants pt ON pt.id = p.tenant_id
			WHERE starts_with(pt.path, t.path) AND p.cluster_id = r.cluster_id
		) a ON true
		WHERE ($1::uuid IS NULL OR r.tenant_id = $1) AND ($2::uuid IS NULL OR r.cluster_id = $2)
		ORDER BY t.name, c.location`, tenantID, clusterID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Reservation, error) {
		r := Reservation{Storage: map[uuid.UUID]int64{}, AllocatedStorage: map[uuid.UUID]int64{}}
		err := row.Scan(&r.TenantID, &r.TenantName, &r.ClusterID, &r.Location, &r.CPUCores, &r.MemoryMB, &r.UpdatedAt,
			&r.AllocatedCPU, &r.AllocatedMemoryMB)
		return r, err
	})
	if err != nil || len(out) == 0 {
		return []Reservation{}, err
	}
	idx := map[[2]uuid.UUID]int{}
	for i, r := range out {
		idx[[2]uuid.UUID{r.TenantID, r.ClusterID}] = i
	}
	srows, err := s.db.Query(ctx, `
		SELECT rs.tenant_id, rs.cluster_id, rs.class_id, rs.capacity_gb,
		       COALESCE((SELECT sum(vs.capacity_gb) FROM vdc_storage vs JOIN projects p ON p.id = vs.project_id
		                 JOIN tenants pt ON pt.id = p.tenant_id
		                 WHERE starts_with(pt.path, t.path) AND p.cluster_id = rs.cluster_id AND vs.class_id = rs.class_id), 0)::bigint
		FROM msp_reservation_storage rs JOIN tenants t ON t.id = rs.tenant_id
		WHERE ($1::uuid IS NULL OR rs.tenant_id = $1) AND ($2::uuid IS NULL OR rs.cluster_id = $2)`, tenantID, clusterID)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var t, c, class uuid.UUID
		var gb, alloc int64
		if err := srows.Scan(&t, &c, &class, &gb, &alloc); err != nil {
			return nil, err
		}
		if i, ok := idx[[2]uuid.UUID{t, c}]; ok {
			out[i].Storage[class] = gb
			out[i].AllocatedStorage[class] = alloc
		}
	}
	return out, srows.Err()
}

// GetReservation devolve a reserva do MSP no cluster (ok=false se não há).
func (s *Store) GetReservation(ctx context.Context, tenantID, clusterID uuid.UUID) (Reservation, bool, error) {
	rs, err := s.ListReservations(ctx, &tenantID, &clusterID)
	if err != nil || len(rs) == 0 {
		return Reservation{}, false, err
	}
	return rs[0], true, nil
}

// SaveReservation cria ou troca a reserva (validação no pacote quota).
func (s *Store) SaveReservation(ctx context.Context, r Reservation) error {
	if _, err := s.db.Exec(ctx, `
		INSERT INTO msp_reservations (tenant_id, cluster_id, cpu_cores, memory_mb) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, cluster_id) DO UPDATE SET cpu_cores = EXCLUDED.cpu_cores,
			memory_mb = EXCLUDED.memory_mb, updated_at = now()`,
		r.TenantID, r.ClusterID, r.CPUCores, r.MemoryMB); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM msp_reservation_storage WHERE tenant_id = $1 AND cluster_id = $2`, r.TenantID, r.ClusterID); err != nil {
		return err
	}
	for class, gb := range r.Storage {
		if _, err := s.db.Exec(ctx, `INSERT INTO msp_reservation_storage (tenant_id, cluster_id, class_id, capacity_gb) VALUES ($1, $2, $3, $4)`,
			r.TenantID, r.ClusterID, class, gb); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteReservation(ctx context.Context, tenantID, clusterID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM msp_reservations WHERE tenant_id = $1 AND cluster_id = $2`, tenantID, clusterID)
	return err
}

// LockCluster trava o cluster (reservas simultâneas não passam da capacidade).
func (s *Store) LockCluster(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `SELECT 1 FROM clusters WHERE id = $1 FOR UPDATE`, id)
	return err
}

// ---------- consumo do MSP ----------

// MSPUsage soma o que o MSP já distribuiu (cotas de VDC) e o que está em uso.
type MSPUsage struct {
	AllocatedCPU      int                 `json:"allocated_cpu"`
	AllocatedMemoryMB int64               `json:"allocated_memory_mb"`
	AllocatedStorage  map[uuid.UUID]int64 `json:"allocated_storage"`
	Customers         int                 `json:"customers"`
	VDCs              int                 `json:"vdcs"`
	VMs               int                 `json:"vms"`
	RunningCPU        int                 `json:"running_cpu"`
	RunningMemoryMB   int64               `json:"running_memory_mb"`
	// Clusters com VDC: não podem sair da lista de liberados.
	ClustersInUse []uuid.UUID `json:"locations_in_use"`
}

// MSPUsage calcula o consumo da subárvore do MSP. excludeVDC deixa um VDC
// de fora da soma (usado ao validar a nova cota desse mesmo VDC).
func (s *Store) MSPUsage(ctx context.Context, mspPath string, excludeVDC *uuid.UUID) (MSPUsage, error) {
	u := MSPUsage{AllocatedStorage: map[uuid.UUID]int64{}, ClustersInUse: []uuid.UUID{}}
	if err := s.db.QueryRow(ctx, `
		SELECT COALESCE(sum(p.cpu_cores), 0)::int, COALESCE(sum(p.memory_mb), 0)::bigint, count(*)::int
		FROM projects p JOIN tenants t ON t.id = p.tenant_id
		WHERE starts_with(t.path, $1) AND ($2::uuid IS NULL OR p.id <> $2)`, mspPath, excludeVDC).
		Scan(&u.AllocatedCPU, &u.AllocatedMemoryMB, &u.VDCs); err != nil {
		return u, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT vs.class_id, sum(vs.capacity_gb)::bigint
		FROM vdc_storage vs JOIN projects p ON p.id = vs.project_id JOIN tenants t ON t.id = p.tenant_id
		WHERE starts_with(t.path, $1) AND ($2::uuid IS NULL OR p.id <> $2)
		GROUP BY vs.class_id`, mspPath, excludeVDC)
	if err != nil {
		return u, err
	}
	for rows.Next() {
		var id uuid.UUID
		var gb int64
		if err := rows.Scan(&id, &gb); err != nil {
			rows.Close()
			return u, err
		}
		u.AllocatedStorage[id] = gb
	}
	rows.Close()
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE kind = 'customer')::int FROM tenants WHERE starts_with(path, $1)`, mspPath).
		Scan(&u.Customers); err != nil {
		return u, err
	}
	if err := s.db.QueryRow(ctx, `
		SELECT count(*)::int,
		       COALESCE(sum(r.cpus) FILTER (WHERE r.status = 'running'), 0)::int,
		       (COALESCE(sum(r.maxmem_bytes) FILTER (WHERE r.status = 'running'), 0) / 1048576)::bigint
		FROM resources r JOIN tenants t ON t.id = r.tenant_id
		WHERE starts_with(t.path, $1) AND NOT r.orphaned`, mspPath).
		Scan(&u.VMs, &u.RunningCPU, &u.RunningMemoryMB); err != nil {
		return u, err
	}
	rows, err = s.db.Query(ctx, `
		SELECT DISTINCT p.cluster_id FROM projects p JOIN tenants t ON t.id = p.tenant_id
		WHERE starts_with(t.path, $1) AND p.cluster_id IS NOT NULL`, mspPath)
	if err != nil {
		return u, err
	}
	u.ClustersInUse, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return u, err
}

// LockProject trava o VDC (para checar a cota ao ligar uma VM).
func (s *Store) LockProject(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `SELECT 1 FROM projects WHERE id = $1 FOR UPDATE`, id)
	return err
}

// ProjectCommitted soma CPU e RAM das VMs ligadas do VDC mais as que já
// têm um "start" na fila, sem contar a VM que está pedindo para ligar.
func (s *Store) ProjectCommitted(ctx context.Context, projectID, exceptResource uuid.UUID) (cpu int, memMB int64, err error) {
	err = s.db.QueryRow(ctx, `
		SELECT COALESCE(sum(r.cpus), 0)::int, (COALESCE(sum(r.maxmem_bytes), 0) / 1048576)::bigint
		FROM resources r
		WHERE r.project_id = $1 AND NOT r.orphaned AND r.id <> $2
		  AND (r.status = 'running' OR EXISTS (
		        SELECT 1 FROM tasks k WHERE k.resource_id = r.id AND k.action = 'start'
		          AND k.status IN ('queued', 'running')))`, projectID, exceptResource).Scan(&cpu, &memMB)
	return
}

// ResourceQuotaInfo devolve o VDC e o tamanho de uma VM (para a checagem
// de cota ao ligar).
func (s *Store) ResourceQuotaInfo(ctx context.Context, resourceID uuid.UUID) (projectID *uuid.UUID, cpus int, memMB int64, err error) {
	err = s.db.QueryRow(ctx, `SELECT project_id, cpus, maxmem_bytes / 1048576 FROM resources WHERE id = $1`, resourceID).
		Scan(&projectID, &cpus, &memMB)
	return
}
