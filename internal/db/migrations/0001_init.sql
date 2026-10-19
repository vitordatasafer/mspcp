-- 0001_init: núcleo multi-tenant da Fase 0.
-- Regra: vmid, node e cluster_id ficam só em colunas internas de `resources`;
-- a API pública nunca as serializa.

CREATE TABLE tenants (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id   uuid REFERENCES tenants(id),
    kind        text NOT NULL CHECK (kind IN ('provider', 'partner', 'customer')),
    name        text NOT NULL,
    -- Caminho materializado "/<raiz>/<parceiro>/<cliente>/": permite checar
    -- "é descendente de" com starts_with() sem CTE recursiva por request.
    path        text NOT NULL UNIQUE,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'provider') = (parent_id IS NULL))
);
CREATE INDEX tenants_path_idx ON tenants (path text_pattern_ops);

CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants(id),
    name        text NOT NULL,
    -- Nome do resource pool no Proxmox (o mesmo em todos os clusters).
    pool_name   text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE memberships (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject     text NOT NULL,              -- claim "sub" do OIDC
    tenant_id   uuid NOT NULL REFERENCES tenants(id),
    project_id  uuid REFERENCES projects(id),
    role        text NOT NULL CHECK (role IN (
                    'super_admin', 'noc_operator',
                    'partner_admin', 'customer_admin',
                    'operator', 'viewer')),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX memberships_uniq
    ON memberships (subject, tenant_id, COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE INDEX memberships_subject_idx ON memberships (subject);

CREATE TABLE clusters (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text NOT NULL UNIQUE,
    dc               text NOT NULL,
    api_url          text NOT NULL,          -- validado contra PROXMOX_ALLOWED_HOSTS
    ca_cert_pem      text NOT NULL,          -- CA do cluster; verificação TLS obrigatória
    token_id         text NOT NULL,          -- user@realm!tokenname
    token_secret_ref text NOT NULL,          -- referência no cofre, nunca o segredo
    enabled          boolean NOT NULL DEFAULT true,
    last_sync_at     timestamptz,
    last_sync_error  text,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE resources (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),  -- ID público
    alias         text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('qemu', 'lxc')),
    tenant_id     uuid REFERENCES tenants(id),
    project_id    uuid REFERENCES projects(id),
    -- Internos: nunca sair do backend.
    cluster_id    uuid NOT NULL REFERENCES clusters(id),
    node          text NOT NULL,
    vmid          integer NOT NULL,
    -- Estado sincronizado.
    status        text NOT NULL,
    cpus          integer NOT NULL DEFAULT 0,
    maxmem_bytes  bigint NOT NULL DEFAULT 0,
    maxdisk_bytes bigint NOT NULL DEFAULT 0,
    orphaned      boolean NOT NULL DEFAULT false,
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cluster_id, vmid)
);
CREATE INDEX resources_tenant_idx ON resources (tenant_id);
CREATE INDEX resources_project_idx ON resources (project_id);

CREATE TABLE tasks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id   uuid NOT NULL REFERENCES resources(id),
    tenant_id     uuid REFERENCES tenants(id),
    action        text NOT NULL CHECK (action IN ('start', 'shutdown', 'stop', 'reboot')),
    status        text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    upid          text,                       -- interno
    requested_by  text NOT NULL,
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz
);
-- Trava por recurso: no máximo uma ação em andamento por VM.
CREATE UNIQUE INDEX tasks_one_active_per_resource
    ON tasks (resource_id) WHERE status IN ('queued', 'running');
CREATE INDEX tasks_resource_idx ON tasks (resource_id, created_at DESC);

CREATE TABLE audit_log (
    id               bigserial PRIMARY KEY,
    at               timestamptz NOT NULL DEFAULT now(),
    subject          text NOT NULL,
    actor_tenant_id  uuid,
    target_tenant_id uuid,
    action           text NOT NULL,
    target           text,
    details          jsonb NOT NULL DEFAULT '{}'::jsonb,
    remote_ip        text
);
CREATE INDEX audit_target_tenant_idx ON audit_log (target_tenant_id, at DESC);

CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log é append-only';
END $$;
CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
