import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, gbText, when, type ClusterDetail, type ProbeResult, type Reservation } from "../api";
import { useSession } from "../session";
import { Meter } from "../components/Meter";
import { Modal } from "../components/Modal";
import { IconChevronRight, IconCluster, IconPlus, IconRefresh } from "../components/Icons";
import { SyncStatus } from "./Clusters";

type Tab = "overview" | "reservations" | "storage" | "network" | "connection";

const gbOfMb = (mb: number) => Math.round(mb / 1024);

export function ClusterDetailPage() {
  const { id } = useParams();
  const s = useSession();
  const qc = useQueryClient();
  const [tab, setTab] = useState<Tab>("overview");
  const q = useQuery({
    queryKey: ["cluster", id],
    queryFn: () => api<ClusterDetail>(`/v1/clusters/${id}`),
    refetchInterval: 20_000,
    retry: false,
  });
  const sync = useMutation({
    mutationFn: () => api(`/v1/clusters/${id}/sync`, { method: "POST" }),
    onSettled: () => { qc.invalidateQueries({ queryKey: ["cluster", id] }); qc.invalidateQueries({ queryKey: ["clusters"] }); },
  });

  if (q.error) {
    const missing = q.error instanceof ApiError && q.error.status === 404;
    return (
      <div className="panel empty">
        <strong>{missing ? "Cluster não encontrado" : "Não foi possível abrir o cluster"}</strong>
        {missing ? "" : q.error.message} <Link to="/clusters">Voltar para clusters</Link>
      </div>
    );
  }
  if (!q.data) return <p className="muted">Carregando…</p>;
  const d = q.data;
  const c = d.cluster;
  const manage = s.canManageClusters;

  return (
    <>
      <nav className="crumbs" aria-label="Você está em">
        <Link to="/clusters">Clusters</Link><IconChevronRight /><span>{c.name}</span>
      </nav>
      <div className="detail-head">
        <div className="detail-icon" aria-hidden="true"><IconCluster /></div>
        <div className="detail-title">
          <h1>{c.name}</h1>
          <p className="muted small" style={{ marginTop: 2 }}>
            {c.location} · {c.dc} · {c.pve_version ? `PVE ${c.pve_version} · ` : ""}{c.nodes_online} de {c.nodes_total} nodes online
          </p>
          <div style={{ marginTop: 6 }}><SyncStatus c={c} /></div>
          <div className="detail-actions">
            <button className="btn" disabled={sync.isPending} onClick={() => sync.mutate()}><IconRefresh />{sync.isPending ? "Lendo o cluster…" : "Sincronizar agora"}</button>
          </div>
          {sync.error && <div className="alert" style={{ marginTop: 8 }}>{sync.error.message}</div>}
        </div>
      </div>

      <div className="tabs" role="tablist">
        {([["overview", "Visão geral"], ["reservations", `Reservas de MSP (${d.reservations.length})`], ["storage", "Storage"], ["network", "Rede"], ["connection", "Conexão"]] as const).map(([k, label]) => (
          <button key={k} role="tab" aria-selected={tab === k} onClick={() => setTab(k)}>{label}</button>
        ))}
      </div>

      {tab === "overview" && <Overview d={d} onTab={setTab} />}
      {tab === "reservations" && <Reservations d={d} manage={manage} />}
      {tab === "storage" && <StorageTab d={d} manage={manage} />}
      {tab === "network" && <NetworkTab d={d} manage={manage} />}
      {tab === "connection" && <ConnectionTab d={d} manage={manage} />}
    </>
  );
}

// ---------- Visão geral ----------

function Overview({ d, onTab }: { d: ClusterDetail; onTab: (t: Tab) => void }) {
  const c = d.cluster;
  return (
    <div className="grid-2">
      <section className="panel">
        <div className="panel-head">
          <h2>Capacidade reservada para MSPs</h2>
          <button className="btn btn-sm" onClick={() => onTab("reservations")}>Ver reservas</button>
        </div>
        {c.cpu_total === 0 ? (
          <p className="muted small">O cluster ainda não foi lido. Clique em Sincronizar agora.</p>
        ) : (
          <div className="meters">
            {d.capacity.map((l) => {
              const mem = l.name === "Memória";
              return (
                <Meter
                  key={l.class_id ?? l.name}
                  label={l.class_id ? `${l.name} (${l.pve_storage})` : l.name}
                  limit={(mem ? gbOfMb(l.total) : l.total) || null}
                  allocated={mem ? gbOfMb(l.reserved) : l.reserved}
                  allocatedWord="reservados"
                  fmt={l.name === "vCPU" ? (n) => n.toLocaleString("pt-BR") : gbText}
                />
              );
            })}
          </div>
        )}
        {c.cpu_ratio > 1 && <p className="meter-note" style={{ marginTop: 10 }}>vCPU: {c.cpu_total} threads × {c.cpu_ratio} = {Math.floor(c.cpu_total * c.cpu_ratio)} reserváveis.</p>}
        {d.capacity.length === 2 && <p className="meter-note" style={{ marginTop: 10 }}>Nenhuma classe de storage mapeada neste cluster ainda (aba Storage).</p>}
      </section>
      <section className="panel">
        <h2 style={{ marginBottom: 10 }}>Resumo</h2>
        <dl className="kv">
          <dt>Endereço</dt><dd>{c.api_url}</dd>
          <dt>Token</dt><dd>{c.token_id}</dd>
          <dt>Threads / memória</dt><dd>{c.cpu_total} / {gbText(gbOfMb(c.memory_total_mb))}</dd>
          <dt>Bridge padrão</dt><dd>{c.default_bridge}</dd>
          <dt>Templates</dt><dd>{d.templates} <Link to="/imagens">publicar imagens</Link></dd>
          <dt>Última leitura</dt><dd>{when(c.last_sync_at)}</dd>
          <dt>Cadastrado</dt><dd>{when(c.created_at)}</dd>
        </dl>
      </section>
    </div>
  );
}

// ---------- Reservas ----------

function Reservations({ d, manage }: { d: ClusterDetail; manage: boolean }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState<Reservation | "new" | null>(null);
  const [removing, setRemoving] = useState<Reservation | null>(null);
  const mapped = d.capacity.filter((l) => l.class_id);
  const del = useMutation({
    mutationFn: (r: Reservation) => api(`/v1/clusters/${d.cluster.id}/reservations/${r.tenant_id}`, { method: "DELETE" }),
    onSuccess: () => { setRemoving(null); qc.invalidateQueries({ queryKey: ["cluster", d.cluster.id] }); qc.invalidateQueries({ queryKey: ["capacity"] }); },
  });
  const free = d.msps.filter((m) => !d.reservations.some((r) => r.tenant_id === m.id));
  return (
    <section className="panel">
      <div className="panel-head">
        <div>
          <h2>Recursos reservados para MSPs</h2>
          <p className="muted small">Cada MSP divide a reserva dele em VDCs para clientes ou uso próprio. A soma das reservas não passa a capacidade do cluster.</p>
        </div>
        {manage && <button className="btn btn-primary btn-sm" disabled={free.length === 0} onClick={() => setEditing("new")}><IconPlus />Reservar para MSP</button>}
      </div>
      {d.reservations.length === 0 ? (
        <p className="muted small">{d.msps.length === 0 ? "Nenhum MSP cadastrado ainda. Crie em Contas." : "Nenhum MSP tem recursos neste cluster ainda."}</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>MSP</th><th>vCPU</th><th>Memória</th>
              {mapped.map((l) => <th key={l.class_id}>{l.name}</th>)}
              {manage && <th className="actions" />}
            </tr>
          </thead>
          <tbody>
            {d.reservations.map((r) => (
              <tr key={r.tenant_id}>
                <td><Link to={`/clientes/${r.tenant_id}`}>{r.tenant_name}</Link></td>
                <td><Used used={r.allocated_cpu} total={r.cpu_cores} fmt={String} /></td>
                <td><Used used={gbOfMb(r.allocated_memory_mb)} total={gbOfMb(r.memory_mb)} fmt={gbText} /></td>
                {mapped.map((l) => (
                  <td key={l.class_id}>
                    {r.storage[l.class_id!] !== undefined ? <Used used={r.allocated_storage[l.class_id!] ?? 0} total={r.storage[l.class_id!]} fmt={gbText} /> : <span className="muted">—</span>}
                  </td>
                ))}
                {manage && (
                  <td className="actions">
                    <span style={{ display: "inline-flex", gap: 6 }}>
                      <button className="btn btn-sm" onClick={() => setEditing(r)}>Alterar</button>
                      <button className="btn btn-sm btn-danger" onClick={() => setRemoving(r)}>Remover</button>
                    </span>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {d.reservations.length > 0 && <p className="meter-note" style={{ marginTop: 8 }}>Barras: quanto da reserva o MSP já distribuiu em VDCs.</p>}
      {editing && <ReservationModal d={d} r={editing === "new" ? undefined : editing} candidates={free} onClose={() => setEditing(null)} />}
      {removing && (
        <Modal title={`Remover a reserva de ${removing.tenant_name}?`} onClose={() => setRemoving(null)}
          footer={<>
            <button className="btn" onClick={() => setRemoving(null)}>Cancelar</button>
            <button className="btn btn-danger-solid" disabled={del.isPending} onClick={() => del.mutate(removing)}>Remover reserva</button>
          </>}>
          <p>O MSP deixa de ter esta localização. Só é possível se ele não tiver VDCs aqui.</p>
          {del.error && <div className="alert" style={{ marginTop: 12 }}>{del.error.message}</div>}
        </Modal>
      )}
    </section>
  );
}

function Used({ used, total, fmt }: { used: number; total: number; fmt: (n: number) => string }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return (
    <div className="mini">
      <div className={`meter-bar${total > 0 && used >= total ? " full" : ""}`}><span className="used" style={{ width: `${pct}%` }} /></div>
      <span>{fmt(used)} de {fmt(total)}</span>
    </div>
  );
}

function ReservationModal({ d, r, candidates, onClose }: { d: ClusterDetail; r?: Reservation; candidates: { id: string; name: string }[]; onClose: () => void }) {
  const qc = useQueryClient();
  const c = d.cluster;
  const [msp, setMsp] = useState(r?.tenant_id ?? candidates[0]?.id ?? "");
  const [cpu, setCpu] = useState(String(r?.cpu_cores ?? 8));
  const [mem, setMem] = useState(String(r ? gbOfMb(r.memory_mb) : 16));
  const mapped = d.capacity.filter((l) => l.class_id);
  const [disk, setDisk] = useState<Record<string, string>>(
    Object.fromEntries(mapped.map((l) => [l.class_id!, r?.storage[l.class_id!] !== undefined ? String(r.storage[l.class_id!]) : ""])),
  );
  // O que sobra no cluster sem contar a reserva que está sendo editada.
  const line = (name: string) => d.capacity.find((l) => l.name === name)!;
  const freeCPU = line("vCPU").total - line("vCPU").reserved + (r?.cpu_cores ?? 0);
  const freeMem = gbOfMb(line("Memória").total - line("Memória").reserved + (r?.memory_mb ?? 0));
  const save = useMutation({
    mutationFn: () => api(`/v1/clusters/${c.id}/reservations/${msp}`, {
      method: "PUT",
      json: {
        cpu_cores: Number(cpu), memory_mb: Math.round(Number(mem) * 1024),
        storage: Object.entries(disk).filter(([, v]) => v.trim() !== "").map(([class_id, v]) => ({ class_id, capacity_gb: Number(v) })),
      },
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["cluster", c.id] });
      qc.invalidateQueries({ queryKey: ["clusters"] });
      qc.invalidateQueries({ queryKey: ["capacity"] });
      onClose();
    },
  });
  return (
    <Modal title={r ? `Reserva de ${r.tenant_name} em ${c.name}` : `Reservar recursos em ${c.name}`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-primary" disabled={!msp || save.isPending} onClick={() => save.mutate()}>Salvar reserva</button>
      </>}>
      <div className="stack" style={{ gap: 12 }}>
        {!r && (
          <label className="field">
            MSP
            <select className="select" value={msp} onChange={(e) => setMsp(e.target.value)}>
              {candidates.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
            </select>
          </label>
        )}
        <div className="form-grid">
          <label className="field">
            vCPU
            <input className="input" type="number" min={0} value={cpu} onChange={(e) => setCpu(e.target.value)} />
            <span className="hint">{c.cpu_total ? `${Math.max(0, freeCPU)} livres no cluster` : "capacidade ainda não lida"}{r ? `; ${r.allocated_cpu} já em VDCs` : ""}</span>
          </label>
          <label className="field">
            Memória
            <span className="input-suffix"><input className="input" type="number" min={0} value={mem} onChange={(e) => setMem(e.target.value)} /><span>GB</span></span>
            <span className="hint">{c.memory_total_mb ? `${gbText(Math.max(0, freeMem))} livres` : "capacidade ainda não lida"}{r ? `; ${gbText(gbOfMb(r.allocated_memory_mb))} em VDCs` : ""}</span>
          </label>
          {mapped.map((l) => {
            const id = l.class_id!;
            const freeGB = l.total - l.reserved + (r?.storage[id] ?? 0);
            return (
              <label className="field" key={id}>
                {l.name}
                <span className="input-suffix">
                  <input className="input" type="number" min={0} value={disk[id] ?? ""} placeholder="não liberado" onChange={(e) => setDisk({ ...disk, [id]: e.target.value })} />
                  <span>GB</span>
                </span>
                <span className="hint">{l.total ? `${gbText(Math.max(0, freeGB))} livres em ${l.pve_storage}` : `${l.pve_storage}, tamanho ainda não lido`}</span>
              </label>
            );
          })}
        </div>
        {mapped.length === 0 && <p className="hint">Mapeie uma classe de storage (aba Storage) para reservar disco.</p>}
        {save.error && <div className="alert">{save.error.message}</div>}
      </div>
    </Modal>
  );
}

// ---------- Storage ----------

function StorageTab({ d, manage }: { d: ClusterDetail; manage: boolean }) {
  const qc = useQueryClient();
  const [className, setClassName] = useState("");
  const current = (classId: string) => d.capacity.find((l) => l.class_id === classId)?.pve_storage ?? "";
  const usable = d.storages.filter((st) => st.content.includes("images") || st.content === "");
  const setPolicy = useMutation({
    mutationFn: (v: { cls: string; pve: string }) => api(`/v1/clusters/${d.cluster.id}/storage-policies`, { method: "PUT", json: { class_id: v.cls, pve_storage: v.pve } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["cluster", d.cluster.id] }),
  });
  const addClass = useMutation({
    mutationFn: () => api("/v1/storage-classes", { method: "POST", json: { name: className.trim() } }),
    onSuccess: () => { setClassName(""); qc.invalidateQueries({ queryKey: ["cluster", d.cluster.id] }); qc.invalidateQueries({ queryKey: ["storage-classes"] }); },
  });
  return (
    <div className="two-col">
      <section className="panel">
        <h2 style={{ marginBottom: 6 }}>Classes neste cluster</h2>
        <p className="muted small" style={{ marginBottom: 12 }}>A classe (SSD, HDD…) é o que se reserva e vende; aqui ela aponta para um storage do Proxmox.</p>
        <table className="table">
          <thead><tr><th>Classe</th><th>Storage do Proxmox</th></tr></thead>
          <tbody>
            {d.storage_classes.map((cls) => (
              <tr key={cls.id}>
                <td><b>{cls.name}</b></td>
                <td>
                  {manage ? (
                    <select className="select" aria-label={`Storage do Proxmox para ${cls.name}`} value={current(cls.id)}
                      onChange={(e) => e.target.value && setPolicy.mutate({ cls: cls.id, pve: e.target.value })}>
                      <option value="">não usar aqui</option>
                      {usable.map((st) => <option key={st.storage} value={st.storage}>{st.storage} ({st.type}, {gbText(st.total_gb)})</option>)}
                      {current(cls.id) && !usable.some((st) => st.storage === current(cls.id)) && <option value={current(cls.id)}>{current(cls.id)} (não visto)</option>}
                    </select>
                  ) : (
                    <span className="kind">{current(cls.id) || "não usada"}</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {d.storage_classes.length === 0 && <p className="muted small">Nenhuma classe criada ainda.</p>}
        {setPolicy.error && <div className="alert" style={{ marginTop: 10 }}>{setPolicy.error.message}</div>}
        {manage && (
          <form style={{ display: "flex", gap: 8, marginTop: 14 }} onSubmit={(e) => { e.preventDefault(); addClass.mutate(); }}>
            <label className="visually-hidden" htmlFor="nova-classe-c">Nova classe de storage</label>
            <input id="nova-classe-c" className="input" placeholder="Nova classe, ex.: SSD" value={className} onChange={(e) => setClassName(e.target.value)} />
            <button className="btn" disabled={!className.trim() || addClass.isPending}>Criar classe</button>
          </form>
        )}
        {addClass.error && <div className="alert" style={{ marginTop: 10 }}>{addClass.error.message}</div>}
      </section>
      <section className="panel">
        <h2 style={{ marginBottom: 12 }}>Storages vistos no cluster</h2>
        {d.storages.length === 0 ? <p className="muted small">Nenhum ainda; sincronize o cluster.</p> : (
          <table className="table">
            <thead><tr><th>Storage</th><th>Tipo</th><th className="num">Usado</th><th className="num">Total</th></tr></thead>
            <tbody>
              {d.storages.map((st) => (
                <tr key={st.storage}>
                  <td><b>{st.storage}</b><div className="kind">{st.shared ? "compartilhado" : `local em ${st.nodes} node${st.nodes > 1 ? "s" : ""}`} · {st.content || "—"}</div></td>
                  <td className="kind">{st.type}</td>
                  <td className="num">{gbText(st.used_gb)}</td>
                  <td className="num">{gbText(st.total_gb)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}

// ---------- Rede ----------

function NetworkTab({ d, manage }: { d: ClusterDetail; manage: boolean }) {
  const qc = useQueryClient();
  const [bridge, setBridge] = useState(d.cluster.default_bridge);
  const save = useMutation({
    mutationFn: () => api(`/v1/clusters/${d.cluster.id}`, { method: "PUT", json: { default_bridge: bridge } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["cluster", d.cluster.id] }),
  });
  return (
    <div className="two-col">
      <section className="panel">
        <h2 style={{ marginBottom: 6 }}>Rede padrão das VMs</h2>
        <p className="muted small" style={{ marginBottom: 12 }}>VMs criadas pelo portal entram nesta bridge, a menos que o VDC tenha rede própria (bridge ou VLAN definidas na tela de VDCs).</p>
        <label className="field">
          Bridge padrão
          <select className="select" value={bridge} disabled={!manage} onChange={(e) => setBridge(e.target.value)}>
            {!d.bridges.some((b) => b.name === bridge) && <option value={bridge}>{bridge}</option>}
            {d.bridges.map((b) => <option key={b.name} value={b.name}>{b.name}{b.kind === "vnet" ? " (VNet SDN)" : ""}</option>)}
          </select>
        </label>
        {save.error && <div className="alert" style={{ marginTop: 10 }}>{save.error.message}</div>}
        {save.isSuccess && <div className="note-ok" style={{ marginTop: 10 }}>Salvo.</div>}
        {manage && <div className="form-actions"><button className="btn btn-primary" disabled={bridge === d.cluster.default_bridge || save.isPending} onClick={() => save.mutate()}>Salvar</button></div>}
      </section>
      <section className="panel">
        <h2 style={{ marginBottom: 12 }}>Bridges e VNets</h2>
        {d.bridges.length === 0 ? <p className="muted small">Nenhuma lida ainda (o token precisa de Sys.Audit nos nodes).</p> : (
          <table className="table">
            <tbody>
              {d.bridges.map((b) => (
                <tr key={b.name}>
                  <td><b>{b.name}</b></td>
                  <td className="kind">{b.kind === "vnet" ? "VNet do SDN" : `bridge em ${b.nodes} node${b.nodes > 1 ? "s" : ""}`}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}

// ---------- Conexão ----------

function ConnectionTab({ d, manage }: { d: ClusterDetail; manage: boolean }) {
  const qc = useQueryClient();
  const c = d.cluster;
  const [f, setF] = useState({ name: c.name, dc: c.dc, location: c.location, cpu_ratio: String(c.cpu_ratio) });
  const [conn, setConn] = useState({ api_url: c.api_url, token_id: c.token_id, token_secret: "", ca_cert_pem: "" });
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const refresh = () => { qc.invalidateQueries({ queryKey: ["cluster", c.id] }); qc.invalidateQueries({ queryKey: ["clusters"] }); };
  const saveInfo = useMutation({
    mutationFn: () => api(`/v1/clusters/${c.id}`, { method: "PUT", json: { name: f.name, dc: f.dc, location: f.location, cpu_ratio: Number(f.cpu_ratio) } }),
    onSuccess: refresh,
  });
  const toggle = useMutation({
    mutationFn: () => api(`/v1/clusters/${c.id}`, { method: "PUT", json: { enabled: !c.enabled } }),
    onSuccess: refresh,
  });
  const connBody = () => {
    const b: Record<string, string> = {};
    if (conn.api_url.trim() !== c.api_url) b.api_url = conn.api_url.trim();
    if (conn.token_id.trim() !== c.token_id) b.token_id = conn.token_id.trim();
    if (conn.token_secret.trim()) b.token_secret = conn.token_secret.trim();
    if (conn.ca_cert_pem.trim()) b.ca_cert_pem = conn.ca_cert_pem.trim();
    return b;
  };
  const test = useMutation({
    mutationFn: () => api<ProbeResult>("/v1/clusters/probe", { method: "POST", json: { cluster_id: c.id, api_url: conn.api_url.trim(), token_id: conn.token_id.trim(), token_secret: conn.token_secret.trim(), ca_cert_pem: conn.ca_cert_pem.trim() } }),
    onSuccess: setProbe,
  });
  const saveConn = useMutation({
    mutationFn: () => api(`/v1/clusters/${c.id}`, { method: "PUT", json: connBody() }),
    onSuccess: () => { setConn({ ...conn, token_secret: "", ca_cert_pem: "" }); setProbe(null); refresh(); },
  });
  const changed = Object.keys(connBody()).length > 0;
  return (
    <div className="two-col">
      <section className="panel">
        <h2 style={{ marginBottom: 12 }}>Identificação e capacidade</h2>
        <div className="form-grid">
          <label className="field">Nome<input className="input" disabled={!manage} value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} /></label>
          <label className="field">Datacenter<input className="input" disabled={!manage} value={f.dc} onChange={(e) => setF({ ...f, dc: e.target.value })} /></label>
          <label className="field">Localização<input className="input" disabled={!manage} value={f.location} onChange={(e) => setF({ ...f, location: e.target.value })} /><span className="hint">O nome que MSPs e clientes veem.</span></label>
          <label className="field">
            vCPU por thread
            <input className="input" type="number" min={1} max={20} step={0.5} disabled={!manage} value={f.cpu_ratio} onChange={(e) => setF({ ...f, cpu_ratio: e.target.value })} />
            <span className="hint">{c.cpu_total} threads → {Math.floor(c.cpu_total * Number(f.cpu_ratio || 1))} vCPU reserváveis.</span>
          </label>
        </div>
        {saveInfo.error && <div className="alert" style={{ marginTop: 10 }}>{saveInfo.error.message}</div>}
        {saveInfo.isSuccess && <div className="note-ok" style={{ marginTop: 10 }}>Salvo.</div>}
        {manage && (
          <div className="form-actions">
            <button className={`btn ${c.enabled ? "btn-danger" : ""}`} disabled={toggle.isPending} onClick={() => toggle.mutate()}>
              {c.enabled ? "Desativar sincronização" : "Reativar"}
            </button>
            <button className="btn btn-primary" disabled={saveInfo.isPending} onClick={() => saveInfo.mutate()}>Salvar</button>
          </div>
        )}
      </section>
      <section className="panel">
        <h2 style={{ marginBottom: 6 }}>Conexão com o Proxmox</h2>
        <p className="muted small" style={{ marginBottom: 12 }}>
          {d.stored_secret ? "Segredo do token guardado cifrado no portal." : "Segredo do token fora do banco (arquivo)."}{" "}
          {d.pinned_ca ? "Certificado do cluster fixado no cadastro." : "Certificado validado pelas autoridades do sistema."}
        </p>
        <div className="stack" style={{ gap: 12 }}>
          <label className="field">Endereço<input className="input" disabled={!manage} value={conn.api_url} onChange={(e) => { setConn({ ...conn, api_url: e.target.value }); setProbe(null); }} /></label>
          <label className="field">Token<input className="input" disabled={!manage} value={conn.token_id} onChange={(e) => { setConn({ ...conn, token_id: e.target.value }); setProbe(null); }} /></label>
          <label className="field">
            Novo segredo
            <input className="input" type="password" disabled={!manage} autoComplete="off" placeholder="deixe vazio para manter" value={conn.token_secret} onChange={(e) => { setConn({ ...conn, token_secret: e.target.value }); setProbe(null); }} />
          </label>
          <label className="field">
            Nova CA (opcional)
            <textarea className="textarea" disabled={!manage} rows={3} placeholder="-----BEGIN CERTIFICATE----- (vazio = manter; endereço novo = fixa o que for apresentado)" value={conn.ca_cert_pem} onChange={(e) => { setConn({ ...conn, ca_cert_pem: e.target.value }); setProbe(null); }} />
          </label>
        </div>
        {probe && (probe.ok
          ? <div className="note-ok" style={{ marginTop: 10 }}>Conexão ok: PVE {probe.version}, {probe.nodes_online} nodes{probe.missing_privileges.length ? `; faltam ${probe.missing_privileges.join(", ")}` : ""}.</div>
          : <div className="alert" style={{ marginTop: 10 }}>{probe.error}</div>)}
        {(test.error || saveConn.error) && <div className="alert" style={{ marginTop: 10 }}>{(test.error ?? saveConn.error)!.message}</div>}
        {manage && (
          <div className="form-actions">
            <button className="btn" disabled={test.isPending} onClick={() => test.mutate()}>{test.isPending ? "Testando…" : "Testar"}</button>
            <button className="btn btn-primary" disabled={!changed || saveConn.isPending} onClick={() => saveConn.mutate()}>Salvar conexão</button>
          </div>
        )}
      </section>
    </div>
  );
}
