-- 0002_quotas: modelo de recursos inspirado no MultiPortal.
--
--   admin (provider) cadastra clusters, classes de storage e o mapeamento
--   classe -> storage do Proxmox em cada cluster;
--   MSP (partner) recebe uma cota global (vCPU, RAM, storage por classe) e a
--   lista de clusters liberados;
--   VDC (projects) é uma fatia da cota do MSP num único cluster, para o
--   próprio MSP ou para um cliente final. Sem overcommit: a soma das cotas
--   dos VDCs nunca passa a cota do MSP.

-- Hierarquia: provider > partner (MSP) > customer. Cliente final só existe
-- sob um MSP; clientes diretos ficam num MSP interno. Regra aplicada no
-- código (CreateTenant), pois depende do tipo do pai.

-- Rótulo de localização mostrado a MSPs e clientes no lugar do cluster.
ALTER TABLE clusters ADD COLUMN location text NOT NULL DEFAULT '';
UPDATE clusters SET location = dc WHERE location = '';

CREATE TABLE storage_classes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,          -- "SSD", "HDD", "NVMe"
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Em cada cluster, uma classe aponta para um storage do Proxmox.
CREATE TABLE storage_policies (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cluster_id  uuid NOT NULL REFERENCES clusters(id),
    class_id    uuid NOT NULL REFERENCES storage_classes(id),
    pve_storage text NOT NULL,                 -- id do storage no PVE (interno)
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cluster_id, class_id)
);

-- Cota global do MSP. NULL = ilimitado.
CREATE TABLE msp_limits (
    tenant_id     uuid PRIMARY KEY REFERENCES tenants(id),
    cpu_cores     integer CHECK (cpu_cores >= 0),
    memory_mb     bigint  CHECK (memory_mb >= 0),
    max_customers integer CHECK (max_customers >= 0),
    max_vdcs      integer CHECK (max_vdcs >= 0),
    max_vms       integer CHECK (max_vms >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Clusters liberados para o MSP.
CREATE TABLE msp_clusters (
    tenant_id  uuid NOT NULL REFERENCES tenants(id),
    cluster_id uuid NOT NULL REFERENCES clusters(id),
    PRIMARY KEY (tenant_id, cluster_id)
);

-- Cota de storage do MSP por classe. Sem linha = classe não liberada;
-- capacity_gb NULL = ilimitado.
CREATE TABLE msp_storage (
    tenant_id   uuid NOT NULL REFERENCES tenants(id),
    class_id    uuid NOT NULL REFERENCES storage_classes(id),
    capacity_gb bigint CHECK (capacity_gb >= 0),
    PRIMARY KEY (tenant_id, class_id)
);

-- Projeto = VDC: amarrado a um cluster, com cota fixa (modelo de alocação).
ALTER TABLE projects ADD COLUMN cluster_id uuid REFERENCES clusters(id);
ALTER TABLE projects ADD COLUMN cpu_cores  integer NOT NULL DEFAULT 0 CHECK (cpu_cores >= 0);
ALTER TABLE projects ADD COLUMN memory_mb  bigint  NOT NULL DEFAULT 0 CHECK (memory_mb >= 0);

CREATE TABLE vdc_storage (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    class_id    uuid NOT NULL REFERENCES storage_classes(id),
    capacity_gb bigint NOT NULL CHECK (capacity_gb >= 0),
    PRIMARY KEY (project_id, class_id)
);
