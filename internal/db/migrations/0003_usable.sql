-- 0003_usable: o que falta para uso real.
--   * login local (usuários, sessões, 2FA TOTP);
--   * catálogo de imagens (templates do Proxmox publicados pelo admin);
--   * rede padrão por cluster e por VDC;
--   * discos criados pelo portal, por classe de storage (cota de disco);
--   * estado do pool no Proxmox para cada VDC.

CREATE TABLE users (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username             text NOT NULL UNIQUE CHECK (username = lower(username) AND username ~ '^[a-z0-9][a-z0-9._-]{1,62}$'),
    name                 text NOT NULL DEFAULT '',
    email                text NOT NULL DEFAULT '',
    password_hash        text NOT NULL,
    must_change_password boolean NOT NULL DEFAULT true,
    totp_secret          text,                 -- base32; só vale com totp_enabled
    totp_pending         text,                 -- segredo em cadastro, ainda não confirmado
    totp_enabled         boolean NOT NULL DEFAULT false,
    disabled             boolean NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    last_login_at        timestamptz
);

-- O id da sessão nunca é gravado: só o SHA-256 dele.
CREATE TABLE sessions (
    token_hash  bytea PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    remote_ip   text NOT NULL DEFAULT '',
    user_agent  text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- Rede: bridge (ou VNet do SDN) e VLAN opcional. O VDC herda do cluster.
ALTER TABLE clusters ADD COLUMN default_bridge text NOT NULL DEFAULT 'vmbr0';
ALTER TABLE projects ADD COLUMN bridge text;            -- NULL = padrão do cluster
ALTER TABLE projects ADD COLUMN vlan_tag integer CHECK (vlan_tag BETWEEN 1 AND 4094);
ALTER TABLE projects ADD COLUMN pool_ready boolean NOT NULL DEFAULT false;
ALTER TABLE projects ADD COLUMN pool_error text;

-- Templates vistos no cluster pelo sincronizador.
CREATE TABLE pve_templates (
    cluster_id   uuid NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    vmid         integer NOT NULL,
    node         text NOT NULL,
    name         text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('qemu', 'lxc')),
    maxdisk_bytes bigint NOT NULL DEFAULT 0,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, vmid)
);

-- Imagem = template publicado com um nome amigável.
CREATE TABLE images (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cluster_id  uuid NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    vmid        integer NOT NULL,
    name        text NOT NULL,                 -- "Debian 12", "Ubuntu 24.04"
    os_family   text NOT NULL DEFAULT 'linux' CHECK (os_family IN ('linux', 'windows')),
    min_disk_gb integer NOT NULL DEFAULT 10 CHECK (min_disk_gb > 0),
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cluster_id, vmid),
    UNIQUE (cluster_id, name)
);

-- Discos criados pelo portal: base da cota de disco por classe.
CREATE TABLE resource_disks (
    resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    class_id    uuid NOT NULL REFERENCES storage_classes(id),
    size_gb     bigint NOT NULL CHECK (size_gb > 0),
    PRIMARY KEY (resource_id, class_id)
);

-- Ações novas nas tasks e alvo opcional (criação ainda sem recurso).
ALTER TABLE tasks DROP CONSTRAINT tasks_action_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_action_check
    CHECK (action IN ('start', 'shutdown', 'stop', 'reboot', 'create', 'delete', 'assign'));
ALTER TABLE tasks ALTER COLUMN resource_id DROP NOT NULL;
ALTER TABLE tasks ADD COLUMN project_id uuid REFERENCES projects(id);
ALTER TABLE tasks ADD COLUMN params jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE tasks ADD COLUMN label text NOT NULL DEFAULT '';   -- nome mostrado (VM a criar)

-- Recurso em exclusão some das listas assim que o pedido é aceito.
ALTER TABLE resources ADD COLUMN deleting boolean NOT NULL DEFAULT false;
