import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, gbText, when, type Cluster, type ProbeResult, type StorageClass, type StoragePolicy } from "../api";
import { useSession } from "../session";
import { Modal } from "../components/Modal";
import { IconPlus, IconRefresh } from "../components/Icons";

export function ClustersPage() {
  const s = useSession();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const clusters = useQuery({
    queryKey: ["clusters"],
    queryFn: () => api<{ clusters: Cluster[] }>("/v1/clusters").then((r) => r.clusters),
    refetchInterval: 15_000,
  });

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Clusters</h1>
          <p>Os clusters Proxmox da plataforma. Em cada um você reserva recursos para os MSPs; MSPs e clientes veem só a localização.</p>
        </div>
      </div>
      <div className="toolbar">
        {s.canManageClusters && (
          <button className="btn btn-primary" onClick={() => setAdding(true)}><IconPlus />Adicionar cluster</button>
        )}
        <button className="btn btn-icon" aria-label="Atualizar" title="Atualizar" onClick={() => qc.invalidateQueries({ queryKey: ["clusters"] })}><IconRefresh /></button>
      </div>

      {adding && <AddClusterModal onClose={() => setAdding(false)} />}

      {clusters.error && <div className="alert">{clusters.error.message}</div>}
      {clusters.data && clusters.data.length === 0 && (
        <div className="panel empty">
          <strong>Nenhum cluster cadastrado</strong>
          {s.canManageClusters ? "Adicione o primeiro: informe o endereço do Proxmox e um token de API." : "A administração ainda não cadastrou clusters."}
        </div>
      )}
      {clusters.data && clusters.data.length > 0 && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr><th>Cluster</th><th>Localização</th><th>Nodes</th><th>vCPU reservadas</th><th>Memória reservada</th><th className="num">MSPs</th><th>Situação</th></tr>
            </thead>
            <tbody>
              {clusters.data.map((c) => {
                const cpuCap = Math.floor(c.cpu_total * c.cpu_ratio);
                return (
                  <tr key={c.id}>
                    <td><Link to={`/clusters/${c.id}`}>{c.name}</Link><div className="kind">{c.api_url.replace(/^https:\/\//, "")}{c.pve_version ? ` · PVE ${c.pve_version}` : ""}</div></td>
                    <td>{c.location}<div className="kind">{c.dc}</div></td>
                    <td className="num">{c.nodes_total ? `${c.nodes_online} de ${c.nodes_total}` : "—"}</td>
                    <td><Mini used={c.reserved_cpu ?? 0} total={cpuCap} fmt={String} /></td>
                    <td><Mini used={Math.round((c.reserved_memory_mb ?? 0) / 1024)} total={Math.round(c.memory_total_mb / 1024)} fmt={gbText} /></td>
                    <td className="num">{c.msps ?? 0}</td>
                    <td><SyncStatus c={c} /></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

export function Mini({ used, total, fmt }: { used: number; total: number; fmt: (n: number) => string }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return (
    <div className="mini">
      <div className={`meter-bar${total > 0 && used >= total ? " full" : ""}`}><span className="used" style={{ width: `${pct}%` }} /></div>
      <span>{fmt(used)} de {total > 0 ? fmt(total) : "?"}</span>
    </div>
  );
}

export function SyncStatus({ c }: { c: Cluster }) {
  if (!c.enabled) return <span className="status"><span className="dot" />Desativado</span>;
  if (c.last_sync_error) {
    return <><span className="status"><span className="dot err" />Falhando</span><span className="state-fail">{c.last_sync_error}</span></>;
  }
  if (c.last_sync_at) return <span className="status" title={`Lido ${when(c.last_sync_at)}`}><span className="dot ok" />Sincronizado</span>;
  return <span className="status"><span className="dot busy" />Aguardando a primeira leitura</span>;
}

// Cadastro em dois passos: dados de acesso → testar (certificado, versão,
// capacidade, permissões) → confirmar.
function AddClusterModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [f, setF] = useState({ name: "", dc: "", location: "", api_url: "https://", token_id: "mspcp@pve!portal", token_secret: "", ca_cert_pem: "" });
  const [advanced, setAdvanced] = useState(false);
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const [bridge, setBridge] = useState("");
  const [ratio, setRatio] = useState("1");
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setF({ ...f, [k]: e.target.value });
    if (["api_url", "token_id", "token_secret", "ca_cert_pem"].includes(k)) setProbe(null);
  };
  const url = f.api_url.trim().replace(/\/+$/, "");
  const test = useMutation({
    mutationFn: () => api<ProbeResult>("/v1/clusters/probe", { method: "POST", json: { api_url: url, token_id: f.token_id.trim(), token_secret: f.token_secret.trim(), ca_cert_pem: f.ca_cert_pem.trim() } }),
    onSuccess: (p) => {
      setProbe(p);
      const vm = p.bridges.find((b) => b.name === "vmbr0") ?? p.bridges[0];
      setBridge(vm?.name ?? "vmbr0");
    },
  });
  const create = useMutation({
    mutationFn: () => api<Cluster>("/v1/clusters", {
      method: "POST",
      json: { name: f.name.trim(), dc: f.dc.trim(), location: f.location.trim(), api_url: url, token_id: f.token_id.trim(),
        token_secret: f.token_secret.trim(), ca_cert_pem: probe?.ca_cert_pem ?? "", default_bridge: bridge, cpu_ratio: Number(ratio) },
    }),
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ["clusters"] });
      onClose();
      nav(`/clusters/${c.id}`);
    },
  });
  const canTest = /^https:\/\/[^/\s]+$/.test(url) && f.token_id.includes("!") && f.token_secret.trim() !== "";
  const ok = probe?.ok;
  const leaf = probe?.tls?.chain[0];
  const pinned = probe?.tls?.chain.find((c) => c.subject === probe?.tls?.pinned_subject) ?? leaf;

  return (
    <Modal title="Adicionar cluster Proxmox" onClose={onClose} wide
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        {!ok ? (
          <button className="btn btn-primary" disabled={!canTest || test.isPending} onClick={() => test.mutate()}>
            {test.isPending ? "Testando…" : "Testar conexão"}
          </button>
        ) : (
          <button className="btn btn-primary" disabled={!f.name.trim() || !f.dc.trim() || create.isPending} onClick={() => create.mutate()}>
            {create.isPending ? "Cadastrando…" : "Cadastrar cluster"}
          </button>
        )}
      </>}>
      <div className="stack" style={{ gap: 16 }}>
        <fieldset className="fs">
          <legend>Identificação</legend>
          <div className="form-grid">
            <label className="field">Nome<input className="input" value={f.name} onChange={set("name")} placeholder="cirion-01" /></label>
            <label className="field">Datacenter<input className="input" value={f.dc} onChange={set("dc")} placeholder="Cirion" /></label>
            <label className="field">
              Localização
              <input className="input" value={f.location} onChange={set("location")} placeholder="São Paulo 1" />
              <span className="hint">O nome que MSPs e clientes veem. Vazio = o datacenter.</span>
            </label>
          </div>
        </fieldset>
        <fieldset className="fs">
          <legend>Acesso à API</legend>
          <div className="form-grid">
            <label className="field">
              Endereço
              <input className="input" value={f.api_url} onChange={set("api_url")} placeholder="https://10.0.0.11:8006" />
              <span className="hint">Qualquer node do cluster, porta 8006.</span>
            </label>
            <label className="field">
              Token
              <input className="input" value={f.token_id} onChange={set("token_id")} placeholder="mspcp@pve!portal" />
            </label>
            <label className="field">
              Segredo do token
              <input className="input" type="password" value={f.token_secret} onChange={set("token_secret")} autoComplete="off" placeholder="xxxxxxxx-xxxx-…" />
              <span className="hint">Fica cifrado no banco.</span>
            </label>
          </div>
          <p className="hint" style={{ marginTop: 10 }}>
            Ainda não tem token? No node, rode <code>/etc/scripts/mspcp-pve-setup.sh</code> ou crie em Datacenter › Permissões › API Tokens
            (usuário com o papel MSPCP, separação de privilégios desligada).
          </p>
          <button type="button" className="btn btn-ghost btn-sm" style={{ marginTop: 6 }} onClick={() => setAdvanced(!advanced)}>
            {advanced ? "Esconder" : "Informar"} a CA do cluster (opcional)
          </button>
          {advanced && (
            <label className="field" style={{ marginTop: 8 }}>
              CA do cluster (conteúdo de /etc/pve/pve-root-ca.pem)
              <textarea className="textarea" value={f.ca_cert_pem} onChange={set("ca_cert_pem")} placeholder="-----BEGIN CERTIFICATE-----" />
              <span className="hint">Sem ela, o portal fixa o certificado que o servidor apresentar no teste (confira a impressão digital).</span>
            </label>
          )}
        </fieldset>

        {test.error && <div className="alert">{test.error.message}</div>}
        {probe && !probe.ok && <div className="alert" role="alert">{probe.error}</div>}
        {probe && probe.ok && (
          <fieldset className="fs probe-ok">
            <legend>Conectado</legend>
            <div className="probe-grid">
              <div><span className="kind">Proxmox VE</span><b>{probe.version}</b></div>
              <div><span className="kind">Nodes online</span><b>{probe.nodes_online} de {probe.nodes_total}</b></div>
              <div><span className="kind">CPU (threads)</span><b>{probe.cpu_total}</b></div>
              <div><span className="kind">Memória</span><b>{gbText(Math.round(probe.memory_total_mb / 1024))}</b></div>
              <div><span className="kind">VMs / templates</span><b>{probe.vms} / {probe.templates}</b></div>
              <div><span className="kind">Storages</span><b>{probe.storages.length}</b></div>
            </div>
            <div className="cert-box">
              {probe.tls?.trusted_by_system ? (
                <p className="small">Certificado válido por uma autoridade pública ({leaf?.issuer}); nada a fixar.</p>
              ) : (
                <>
                  <p className="small"><b>Certificado a fixar:</b> {pinned?.subject}{f.ca_cert_pem.trim() ? " (CA informada)" : ""}</p>
                  {!f.ca_cert_pem.trim() && (
                    <p className="small">Impressão digital SHA-256: <code className="fp">{pinned?.fingerprint_sha256}</code></p>
                  )}
                  <p className="hint">Confira no node com <code>openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pve-ssl.pem</code> (ou pve-root-ca.pem, se for a CA).</p>
                </>
              )}
            </div>
            {probe.missing_privileges.length > 0 && (
              <div className="alert warn" style={{ marginTop: 10 }}>
                O token não tem: {probe.missing_privileges.join(", ")}. Algumas funções vão falhar até ajustar o papel no Proxmox.
              </div>
            )}
            <div className="form-grid" style={{ marginTop: 12 }}>
              <label className="field">
                Bridge padrão das VMs
                <select className="select" value={bridge} onChange={(e) => setBridge(e.target.value)}>
                  {probe.bridges.length === 0 && <option value="vmbr0">vmbr0</option>}
                  {probe.bridges.map((b) => <option key={b.name} value={b.name}>{b.name}{b.kind === "vnet" ? " (VNet SDN)" : ""}</option>)}
                </select>
              </label>
              <label className="field">
                vCPU por thread
                <input className="input" type="number" min={1} max={20} step={0.5} value={ratio} onChange={(e) => setRatio(e.target.value)} />
                <span className="hint">1 = sem overcommit de CPU. Reservável: {Math.floor(probe.cpu_total * Number(ratio || 1))} vCPU.</span>
              </label>
            </div>
          </fieldset>
        )}
        {create.error && <div className="alert">{create.error.message}</div>}
      </div>
    </Modal>
  );
}

export function StoragePage() {
  const clusters = useQuery({
    queryKey: ["clusters"],
    queryFn: () => api<{ clusters: Cluster[] }>("/v1/clusters").then((r) => r.clusters),
  });
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Storage por localização</h1>
          <p>As classes de storage são o que MSPs e clientes contratam; em cada cluster a classe aponta para um storage do Proxmox.</p>
        </div>
      </div>
      {clusters.error && <div className="alert">{clusters.error.message}</div>}
      {clusters.data && clusters.data.length === 0 && (
        <div className="panel empty"><strong>Nenhum cluster cadastrado</strong>Cadastre um cluster antes de mapear o storage.</div>
      )}
      {clusters.data && clusters.data.length > 0 && <Storage clusters={clusters.data} />}
    </>
  );
}

// Classes de storage são globais (o que se vende: SSD, HDD); em cada cluster
// a classe aponta para um storage do Proxmox.
function Storage({ clusters }: { clusters: Cluster[] }) {
  const s = useSession();
  const qc = useQueryClient();
  const classes = useQuery({
    queryKey: ["storage-classes"],
    queryFn: () => api<{ storage_classes: StorageClass[] }>("/v1/storage-classes").then((r) => r.storage_classes),
  });
  const policies = useQuery({
    queryKey: ["storage-policies"],
    queryFn: () => api<{ storage_policies: StoragePolicy[] }>("/v1/storage-policies").then((r) => r.storage_policies),
  });
  const [className, setClassName] = useState("");
  const [draft, setDraft] = useState<Record<string, string>>({});
  const key = (cluster: string, cls: string) => `${cluster}/${cls}`;

  const addClass = useMutation({
    mutationFn: () => api("/v1/storage-classes", { method: "POST", json: { name: className } }),
    onSuccess: () => {
      setClassName("");
      qc.invalidateQueries({ queryKey: ["storage-classes"] });
    },
  });
  const setPolicy = useMutation({
    mutationFn: (v: { cluster: string; cls: string; pve: string }) =>
      api(`/v1/clusters/${v.cluster}/storage-policies`, { method: "PUT", json: { class_id: v.cls, pve_storage: v.pve } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["storage-policies"] }),
  });

  const current = (cluster: string, cls: string) =>
    policies.data?.find((p) => p.cluster_id === cluster && p.class_id === cls)?.pve_storage ?? "";

  return (
    <section className="panel">
      <p className="muted small" style={{ marginBottom: 12 }}>
        Informe, em cada cluster, o storage do Proxmox que atende cada classe. Classe sem storage não pode ser usada naquela localização.
      </p>
      <div style={{ overflowX: "auto" }}>
        <table className="table">
          <thead>
            <tr>
              <th>Classe</th>
              {clusters.map((c) => <th key={c.id}>{c.location}</th>)}
            </tr>
          </thead>
          <tbody>
            {classes.data?.map((cls) => (
              <tr key={cls.id}>
                <td><b>{cls.name}</b></td>
                {clusters.map((c) => {
                  const k = key(c.id, cls.id);
                  const value = draft[k] ?? current(c.id, cls.id);
                  const changed = draft[k] !== undefined && draft[k] !== current(c.id, cls.id);
                  return (
                    <td key={c.id}>
                      {s.canManageClusters ? (
                        <form
                          style={{ display: "flex", gap: 6 }}
                          onSubmit={(e) => {
                            e.preventDefault();
                            setPolicy.mutate({ cluster: c.id, cls: cls.id, pve: value });
                            setDraft(({ [k]: _, ...rest }) => rest);
                          }}
                        >
                          <input
                            className="input"
                            aria-label={`Storage do Proxmox para ${cls.name} em ${c.location}`}
                            placeholder="sem storage"
                            value={value}
                            onChange={(e) => setDraft({ ...draft, [k]: e.target.value })}
                          />
                          {changed && <button className="btn btn-sm" disabled={!value.trim()}>Salvar</button>}
                        </form>
                      ) : (
                        <span className="kind">{value || "sem storage"}</span>
                      )}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {setPolicy.error && <div className="alert" style={{ marginTop: 10 }}>{setPolicy.error.message}</div>}
      {s.canManageClusters && (
        <form style={{ display: "flex", gap: 8, marginTop: 14, maxWidth: 420 }} onSubmit={(e) => { e.preventDefault(); addClass.mutate(); }}>
          <label className="visually-hidden" htmlFor="nova-classe">Nova classe de storage</label>
          <input id="nova-classe" className="input" placeholder="Nova classe, por exemplo NVMe" value={className} onChange={(e) => setClassName(e.target.value)} />
          <button className="btn" disabled={!className.trim() || addClass.isPending}>Criar classe</button>
        </form>
      )}
      {addClass.error && <div className="alert" style={{ marginTop: 10 }}>{addClass.error.message}</div>}
    </section>
  );
}

