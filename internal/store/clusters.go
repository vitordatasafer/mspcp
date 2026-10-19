package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ClusterStorage é um storage do Proxmox como o sincronizador o viu.
type ClusterStorage struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Shared  bool   `json:"shared"`
	Content string `json:"content"`
	Nodes   int    `json:"nodes"`
	TotalGB int64  `json:"total_gb"`
	UsedGB  int64  `json:"used_gb"`
}

// ClusterBridge é uma bridge Linux/OVS ou VNet do SDN.
type ClusterBridge struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Nodes int    `json:"nodes"`
}

// ClusterInventory é o que o sincronizador sabe da infraestrutura.
type ClusterInventory struct {
	Version       string
	NodesOnline   int
	NodesTotal    int
	CPUTotal      int
	MemoryTotalMB int64
	Storages      []ClusterStorage
	Bridges       []ClusterBridge // nil = não atualizar
}

func (s *Store) SaveClusterInventory(ctx context.Context, id uuid.UUID, inv ClusterInventory) error {
	if _, err := s.db.Exec(ctx, `
		UPDATE clusters SET pve_version = $2, nodes_online = $3, nodes_total = $4, cpu_total = $5, memory_total_mb = $6
		WHERE id = $1`, id, inv.Version, inv.NodesOnline, inv.NodesTotal, inv.CPUTotal, inv.MemoryTotalMB); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM cluster_storages WHERE cluster_id = $1`, id); err != nil {
		return err
	}
	for _, st := range inv.Storages {
		if _, err := s.db.Exec(ctx, `
			INSERT INTO cluster_storages (cluster_id, storage, type, shared, content, nodes, total_gb, used_gb)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, st.Storage, st.Type, st.Shared, st.Content, st.Nodes, st.TotalGB, st.UsedGB); err != nil {
			return err
		}
	}
	if inv.Bridges == nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM cluster_bridges WHERE cluster_id = $1`, id); err != nil {
		return err
	}
	for _, b := range inv.Bridges {
		if _, err := s.db.Exec(ctx, `INSERT INTO cluster_bridges (cluster_id, name, kind, nodes) VALUES ($1, $2, $3, $4)`,
			id, b.Name, b.Kind, b.Nodes); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListClusterStorages(ctx context.Context, id uuid.UUID) ([]ClusterStorage, error) {
	rows, err := s.db.Query(ctx, `SELECT storage, type, shared, content, nodes, total_gb, used_gb FROM cluster_storages
		WHERE cluster_id = $1 ORDER BY storage`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClusterStorage, error) {
		var c ClusterStorage
		err := r.Scan(&c.Storage, &c.Type, &c.Shared, &c.Content, &c.Nodes, &c.TotalGB, &c.UsedGB)
		return c, err
	})
}

func (s *Store) ListClusterBridges(ctx context.Context, id uuid.UUID) ([]ClusterBridge, error) {
	rows, err := s.db.Query(ctx, `SELECT name, kind, nodes FROM cluster_bridges WHERE cluster_id = $1 ORDER BY kind, name`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClusterBridge, error) {
		var b ClusterBridge
		err := r.Scan(&b.Name, &b.Kind, &b.Nodes)
		return b, err
	})
}

// ClusterUpdate são os campos editáveis de um cluster. nil = não muda.
type ClusterUpdate struct {
	Name, DC, Location, DefaultBridge *string
	CPURatio                          *float64
	Enabled                           *bool
	APIURL, CACertPEM, TokenID        *string
	TokenSecretEnc                    []byte
}

func (s *Store) UpdateCluster(ctx context.Context, id uuid.UUID, u ClusterUpdate) (Cluster, error) {
	c, err := scanCluster(s.db.QueryRow(ctx, `
		UPDATE clusters SET
			name = COALESCE($2, name), dc = COALESCE($3, dc), location = COALESCE($4, location),
			default_bridge = COALESCE($5, default_bridge), cpu_ratio = COALESCE($6, cpu_ratio),
			enabled = COALESCE($7, enabled), api_url = COALESCE($8, api_url), ca_cert_pem = COALESCE($9, ca_cert_pem),
			token_id = COALESCE($10, token_id),
			token_secret_enc = COALESCE($11, token_secret_enc),
			token_secret_ref = CASE WHEN $11::bytea IS NULL THEN token_secret_ref ELSE '' END
		WHERE id = $1 RETURNING `+clusterCols,
		id, u.Name, u.DC, u.Location, u.DefaultBridge, u.CPURatio, u.Enabled, u.APIURL, u.CACertPEM, u.TokenID, u.TokenSecretEnc))
	if isUniqueViolation(err) {
		return c, ErrConflict
	}
	return c, notFound(err)
}

// ClassCapacityGB devolve, por classe, o tamanho do storage do Proxmox
// mapeado no cluster (0 se o storage ainda não foi visto).
func (s *Store) ClassCapacityGB(ctx context.Context, clusterID uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := s.db.Query(ctx, `
		SELECT sp.class_id, COALESCE(cs.total_gb, 0) FROM storage_policies sp
		LEFT JOIN cluster_storages cs ON cs.cluster_id = sp.cluster_id AND cs.storage = sp.pve_storage
		WHERE sp.cluster_id = $1`, clusterID)
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

// ClusterReserved soma as reservas de todos os MSPs no cluster, menos a de
// excludeTenant (que está sendo alterada).
func (s *Store) ClusterReserved(ctx context.Context, clusterID uuid.UUID, excludeTenant *uuid.UUID) (cpu int, memMB int64, storage map[uuid.UUID]int64, err error) {
	storage = map[uuid.UUID]int64{}
	if err = s.db.QueryRow(ctx, `
		SELECT COALESCE(sum(cpu_cores), 0)::int, COALESCE(sum(memory_mb), 0)::bigint FROM msp_reservations
		WHERE cluster_id = $1 AND ($2::uuid IS NULL OR tenant_id <> $2)`, clusterID, excludeTenant).Scan(&cpu, &memMB); err != nil {
		return
	}
	rows, err := s.db.Query(ctx, `
		SELECT class_id, sum(capacity_gb)::bigint FROM msp_reservation_storage
		WHERE cluster_id = $1 AND ($2::uuid IS NULL OR tenant_id <> $2) GROUP BY class_id`, clusterID, excludeTenant)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c uuid.UUID
		var gb int64
		if err = rows.Scan(&c, &gb); err != nil {
			return
		}
		storage[c] = gb
	}
	err = rows.Err()
	return
}
