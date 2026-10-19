-- 0004_clusters: clusters cadastrados pelo portal e recursos reservados por
-- cluster para cada MSP.
--
--   * o segredo do token pode ficar no próprio banco, cifrado (AES-GCM com a
--     chave mestra do servidor); token_secret_ref continua valendo para
--     "file:" e "env:";
--   * o sincronizador guarda a capacidade física (nodes), os storages e as
--     bridges de cada cluster, para a tela do cluster e as reservas;
--   * a cota de vCPU, memória e storage do MSP passa a ser por cluster
--     (reserva). Os limites de quantidade (clientes, VDCs, servidores)
--     continuam globais em msp_limits.

ALTER TABLE clusters ADD COLUMN token_secret_enc bytea;
ALTER TABLE clusters ALTER COLUMN token_secret_ref SET DEFAULT '';
ALTER TABLE clusters ADD COLUMN cpu_ratio numeric(4,2) NOT NULL DEFAULT 1 CHECK (cpu_ratio >= 1 AND cpu_ratio <= 20);
ALTER TABLE clusters ADD COLUMN pve_version text NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN nodes_online integer NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN nodes_total integer NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN cpu_total integer NOT NULL DEFAULT 0;      -- threads somadas dos nodes
ALTER TABLE clusters ADD COLUMN memory_total_mb bigint NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN created_by text NOT NULL DEFAULT '';

-- Storages vistos no cluster (um por nome; compartilhado conta uma vez).
CREATE TABLE cluster_storages (
    cluster_id uuid NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    storage    text NOT NULL,
    type       text NOT NULL DEFAULT '',
    shared     boolean NOT NULL DEFAULT false,
    content    text NOT NULL DEFAULT '',
    nodes      integer NOT NULL DEFAULT 0,
    total_gb   bigint NOT NULL DEFAULT 0,
    used_gb    bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (cluster_id, storage)
);

-- Bridges (e VNets do SDN) presentes nos nodes.
CREATE TABLE cluster_bridges (
    cluster_id uuid NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    name       text NOT NULL,
    kind       text NOT NULL DEFAULT 'bridge' CHECK (kind IN ('bridge', 'vnet')),
    nodes      integer NOT NULL DEFAULT 0,
    PRIMARY KEY (cluster_id, name)
);

-- Reserva de um MSP num cluster.
CREATE TABLE msp_reservations (
    tenant_id  uuid NOT NULL REFERENCES tenants(id),
    cluster_id uuid NOT NULL REFERENCES clusters(id),
    cpu_cores  integer NOT NULL CHECK (cpu_cores >= 0),
    memory_mb  bigint  NOT NULL CHECK (memory_mb >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, cluster_id)
);

CREATE TABLE msp_reservation_storage (
    tenant_id   uuid NOT NULL,
    cluster_id  uuid NOT NULL,
    class_id    uuid NOT NULL REFERENCES storage_classes(id),
    capacity_gb bigint NOT NULL CHECK (capacity_gb >= 0),
    PRIMARY KEY (tenant_id, cluster_id, class_id),
    FOREIGN KEY (tenant_id, cluster_id) REFERENCES msp_reservations(tenant_id, cluster_id) ON DELETE CASCADE
);

-- Migra o modelo anterior: cada MSP recebe, em cada cluster liberado, a
-- reserva exata do que já distribuiu em VDCs lá (nada fica acima da cota).
INSERT INTO msp_reservations (tenant_id, cluster_id, cpu_cores, memory_mb)
SELECT mc.tenant_id, mc.cluster_id,
       COALESCE((SELECT sum(p.cpu_cores) FROM projects p JOIN tenants t ON t.id = p.tenant_id
                 WHERE starts_with(t.path, m.path) AND p.cluster_id = mc.cluster_id), 0),
       COALESCE((SELECT sum(p.memory_mb) FROM projects p JOIN tenants t ON t.id = p.tenant_id
                 WHERE starts_with(t.path, m.path) AND p.cluster_id = mc.cluster_id), 0)
FROM msp_clusters mc JOIN tenants m ON m.id = mc.tenant_id;

INSERT INTO msp_reservation_storage (tenant_id, cluster_id, class_id, capacity_gb)
SELECT r.tenant_id, r.cluster_id, ms.class_id,
       COALESCE((SELECT sum(vs.capacity_gb) FROM vdc_storage vs JOIN projects p ON p.id = vs.project_id
                 JOIN tenants t ON t.id = p.tenant_id
                 WHERE starts_with(t.path, m.path) AND p.cluster_id = r.cluster_id AND vs.class_id = ms.class_id), 0)
FROM msp_reservations r JOIN tenants m ON m.id = r.tenant_id JOIN msp_storage ms ON ms.tenant_id = r.tenant_id;

DROP TABLE msp_storage;
DROP TABLE msp_clusters;
ALTER TABLE msp_limits DROP COLUMN cpu_cores;
ALTER TABLE msp_limits DROP COLUMN memory_mb;
