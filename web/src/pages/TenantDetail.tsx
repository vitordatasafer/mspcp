import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  api, gbText, kindLabel, roleLabel, when,
  type Capacity, type MembershipView, type Project, type Tenant, type TenantKind,
} from "../api";
import { useSession } from "../session";
import { Mini } from "./Clusters";
import { VDCRow } from "../components/VDC";
import { Modal } from "../components/Modal";
import { VDCModal } from "./VDCs";

const rolesFor: Record<TenantKind, string[]> = {
  provider: ["super_admin", "noc_operator", "viewer"],
  partner: ["partner_admin", "operator", "viewer"],
  customer: ["customer_admin", "operator", "viewer"],
};

const capacityQuery = (mspId: string) => ({
  queryKey: ["capacity", mspId],
  queryFn: () => api<Capacity>(`/v1/tenants/${mspId}/capacity`),
});

export function TenantDetailPage() {
  const { id } = useParams();
  const s = useSession();
  const t = s.tenants.find((x) => x.id === id);
  if (!t) {
    return (
      <div className="panel empty">
        <strong>Conta não encontrada</strong>
        Ela pode ter sido removida ou estar fora do seu acesso. <Link to="/clientes">Voltar</Link>
      </div>
    );
  }
  const parent = s.tenants.find((x) => x.id === t.parent_id);
  return (
    <>
      {s.isStaff && <Link className="back" to="/clientes">{s.isStaff ? "MSPs e clientes" : "Clientes"}</Link>}
      <div className="page-head">
        <div>
          <h1>{t.name}</h1>
          <p>
            {kindLabel[t.kind]}
            {parent && parent.kind === "partner" ? `, atendido pelo MSP ${parent.name}` : ""}
          </p>
        </div>
      </div>

      {t.kind === "partner" && <MSPView msp={t} />}
      {t.kind === "customer" && <CustomerVDCs t={t} msp={parent} />}

      <div className="two-col" style={{ marginTop: 16 }}>
        {t.kind === "partner" ? <MSPCustomers msp={t} /> : t.kind === "provider" ? <ProviderNote /> : <div />}
        <Users t={t} />
      </div>
    </>
  );
}

function ProviderNote() {
  return (
    <div className="panel">
      <h2>Equipe da plataforma</h2>
      <p className="muted small" style={{ marginTop: 8 }}>
        Quem tem papel aqui administra clusters, MSPs e a cota de cada MSP.
      </p>
    </div>
  );
}

// ---------- MSP: pool de recursos, limites e VDCs ----------

function MSPView({ msp }: { msp: Tenant }) {
  const s = useSession();
  const cap = useQuery(capacityQuery(msp.id));
  const [editLimits, setEditLimits] = useState(false);
  const [newVDC, setNewVDC] = useState(false);
  const [editing, setEditing] = useState<Project | null>(null);

  if (cap.isPending) return <p className="muted">Carregando cota…</p>;
  if (cap.error) return <div className="alert">{cap.error.message}</div>;
  const { limits, usage, vdcs } = cap.data;
  const mb = (n: number) => Math.round(n / 1024);
  const className = (id: string) =>
    vdcs.flatMap((v) => v.storage).find((x) => x.class_id === id)?.class_name
    ?? cap.data.locations.flatMap((l) => l.storage_classes).find((c) => c.id === id)?.name
    ?? "Storage";
  const classes = [...new Set(limits.reservations.flatMap((r) => Object.keys(r.storage)))];

  return (
    <>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Recursos reservados</h2>
            <p className="muted small">Reservados pela plataforma em cada localização. Os VDCs saem daqui.</p>
          </div>
          {s.canSetLimits && <button className="btn btn-sm" onClick={() => setEditLimits(true)}>Alterar limites</button>}
        </div>
        {editLimits && (
          <Modal title={`Limites do MSP ${msp.name}`} onClose={() => setEditLimits(false)}>
            <LimitsForm msp={msp} capacity={cap.data} onDone={() => setEditLimits(false)} />
          </Modal>
        )}
        {limits.reservations.length === 0 ? (
          <p className="muted small">
            Nenhum recurso reservado ainda.{s.canManageClusters ? " Reserve em Infraestrutura › Clusters › (cluster) › Reservas de MSP." : " Fale com o seu provedor."}
          </p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Localização</th><th>vCPU</th><th>Memória</th>
                {classes.map((c) => <th key={c}>{className(c)}</th>)}
                {s.canManageClusters && <th className="actions" />}
              </tr>
            </thead>
            <tbody>
              {limits.reservations.map((r) => (
                <tr key={r.location_id}>
                  <td><b>{r.location}</b></td>
                  <td><Mini used={r.allocated_cpu} total={r.cpu_cores} fmt={String} /></td>
                  <td><Mini used={mb(r.allocated_memory_mb)} total={mb(r.memory_mb)} fmt={gbText} /></td>
                  {classes.map((c) => (
                    <td key={c}>{r.storage[c] !== undefined ? <Mini used={r.allocated_storage[c] ?? 0} total={r.storage[c]} fmt={gbText} /> : <span className="muted">—</span>}</td>
                  ))}
                  {s.canManageClusters && <td className="actions"><Link className="btn btn-sm" to={`/clusters/${r.location_id}`}>Gerenciar</Link></td>}
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <div className="counts">
          <span>Distribuído em VDCs: <b>{usage.allocated_cpu}</b> de {limits.cpu_cores ?? 0} vCPU, <b>{gbText(mb(usage.allocated_memory_mb))}</b> de {gbText(mb(limits.memory_mb ?? 0))}</span>
          <span>Ligado: <b>{usage.running_cpu}</b> vCPU, <b>{gbText(mb(usage.running_memory_mb))}</b></span>
        </div>
        <div className="counts">
          <span><b>{usage.customers}</b> de {limits.max_customers ?? "∞"} clientes</span>
          <span><b>{usage.vdcs}</b> de {limits.max_vdcs ?? "∞"} VDCs</span>
          <span><b>{usage.vms}</b> de {limits.max_vms ?? "∞"} servidores</span>
        </div>
      </section>

      <section className="panel">
        <div className="panel-head">
          <h2>VDCs</h2>
          {s.canManageVDCs && (
            <button className="btn btn-primary btn-sm" onClick={() => setNewVDC(true)} disabled={cap.data.locations.length === 0}>
              Novo VDC
            </button>
          )}
        </div>
        {newVDC && <VDCModal mspId={msp.id} onClose={() => setNewVDC(false)} />}
        {editing && <VDCModal mspId={msp.id} vdc={editing} onClose={() => setEditing(null)} />}
        {vdcs.length > 0 && (
          <p className="muted small">As barras mostram o que está ligado em relação à cota de cada VDC.</p>
        )}
        {vdcs.length === 0 ? (
          <p className="muted small">
            Nenhum VDC ainda. Um VDC reserva parte desta cota numa localização, para uso do MSP ou de um cliente.
          </p>
        ) : (
          vdcs.map((v) => (
            <VDCRow key={v.id} v={v} showTenant onEdit={s.canManageVDCs ? () => { setNewVDC(false); setEditing(v); } : undefined} />
          ))
        )}
      </section>
    </>
  );
}

function LimitsForm({ msp, capacity, onDone }: { msp: Tenant; capacity: Capacity; onDone: () => void }) {
  const qc = useQueryClient();
  const l = capacity.limits;
  const str = (n: number | null) => (n === null ? "" : String(n));
  const [f, setF] = useState({ customers: str(l.max_customers), vdcs: str(l.max_vdcs), vms: str(l.max_vms) });
  const num = (v: string) => (v.trim() === "" ? null : Number(v));
  const save = useMutation({
    mutationFn: () =>
      api(`/v1/tenants/${msp.id}/limits`, {
        method: "PUT",
        json: { max_customers: num(f.customers), max_vdcs: num(f.vdcs), max_vms: num(f.vms) },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["capacity", msp.id] });
      onDone();
    },
  });
  const field = (k: keyof typeof f, label: string) => (
    <label className="field">
      {label}
      <input className="input" type="number" min={0} value={f[k]} placeholder="sem limite" onChange={(e) => setF({ ...f, [k]: e.target.value })} />
    </label>
  );
  return (
    <form onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
      <p className="muted small" style={{ marginBottom: 14 }}>
        Quantidades máximas do MSP (vazio = sem limite). vCPU, memória e storage são reservados por cluster, em Infraestrutura › Clusters.
      </p>
      <div className="form-grid">
        {field("customers", "Clientes")}
        {field("vdcs", "VDCs")}
        {field("vms", "Servidores")}
      </div>
      {save.error && <div className="alert" style={{ marginTop: 12 }}>{save.error.message}</div>}
      <div className="form-actions">
        <button type="button" className="btn btn-ghost" onClick={onDone}>Cancelar</button>
        <button className="btn btn-primary" disabled={save.isPending}>Salvar limites</button>
      </div>
    </form>
  );
}

function MSPCustomers({ msp }: { msp: Tenant }) {
  const s = useSession();
  const qc = useQueryClient();
  const customers = s.tenants.filter((x) => x.parent_id === msp.id);
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => api<Tenant>("/v1/tenants", { method: "POST", json: { parent_id: msp.id, kind: "customer", name } }),
    onSuccess: () => {
      setName("");
      qc.invalidateQueries({ queryKey: ["tenants"] });
      qc.invalidateQueries({ queryKey: ["capacity", msp.id] });
    },
  });
  return (
    <section className="panel">
      <div className="panel-head"><h2>Clientes</h2></div>
      {s.canCreateTenants && (
        <form style={{ display: "flex", gap: 8, marginBottom: 12 }} onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
          <label className="visually-hidden" htmlFor="novo-cliente">Nome do novo cliente</label>
          <input id="novo-cliente" className="input" placeholder="Nome do novo cliente" value={name} onChange={(e) => setName(e.target.value)} />
          <button className="btn" disabled={!name.trim() || create.isPending}>Criar cliente</button>
        </form>
      )}
      {create.error && <div className="alert" style={{ marginBottom: 12 }}>{create.error.message}</div>}
      {customers.length === 0 ? (
        <p className="muted small">Este MSP ainda não tem clientes.</p>
      ) : (
        <table className="table">
          <tbody>
            {customers.map((c) => (
              <tr key={c.id}><td><Link to={`/clientes/${c.id}`}>{c.name}</Link></td></tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

// ---------- Cliente: VDCs recebidos ----------

function CustomerVDCs({ t, msp }: { t: Tenant; msp?: Tenant }) {
  const s = useSession();
  const [newVDC, setNewVDC] = useState(false);
  const [editing, setEditing] = useState<Project | null>(null);
  const vdcs = useQuery({
    queryKey: ["projects", t.id],
    queryFn: () => api<{ projects: Project[] }>(`/v1/tenants/${t.id}/projects`).then((r) => r.projects),
  });
  // Só MSP e admin criam VDCs; precisam da cota do MSP para isso.
  const canManage = s.canManageVDCs && !!msp;

  return (
    <section className="panel">
      <div className="panel-head">
        <h2>VDCs</h2>
        {canManage && (
          <button className="btn btn-primary btn-sm" onClick={() => setNewVDC(true)}>Novo VDC</button>
        )}
      </div>
      {newVDC && <VDCModal mspId={msp!.id} onlyOwner={t.id} onClose={() => setNewVDC(false)} />}
      {editing && <VDCModal mspId={msp!.id} vdc={editing} onClose={() => setEditing(null)} />}
      {vdcs.data && vdcs.data.length === 0 && (
        <p className="muted small">
          {canManage ? "Crie um VDC para reservar recursos a este cliente." : "Nenhum VDC ainda. Fale com o seu provedor para receber recursos."}
        </p>
      )}
      {vdcs.data && vdcs.data.length > 0 && (
        <p className="muted small">As barras mostram o que está ligado em relação à cota de cada VDC.</p>
      )}
      {vdcs.data?.map((v) => (
        <VDCRow key={v.id} v={v} showTenant={false} onEdit={canManage ? () => { setNewVDC(false); setEditing(v); } : undefined} />
      ))}
    </section>
  );
}

// ---------- Usuários ----------

function Users({ t }: { t: Tenant }) {
  const s = useSession();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [secret, setSecret] = useState<{ who: string; password: string; created: boolean } | null>(null);
  const [removing, setRemoving] = useState<MembershipView | null>(null);
  const [resetting, setResetting] = useState<MembershipView | null>(null);

  const members = useQuery({
    queryKey: ["memberships", t.id],
    queryFn: () => api<{ memberships: MembershipView[] }>(`/v1/tenants/${t.id}/memberships`).then((r) => r.memberships),
    retry: false,
  });
  const refresh = () => qc.invalidateQueries({ queryKey: ["memberships", t.id] });

  if (members.error) {
    return (
      <section className="panel">
        <h2>Usuários</h2>
        <p className="muted small" style={{ marginTop: 8 }}>{members.error.message}</p>
      </section>
    );
  }
  const local = s.me.local;
  return (
    <section className="panel">
      <div className="panel-head">
        <h2>Usuários</h2>
        {local && <button className="btn btn-primary btn-sm" onClick={() => setAdding(true)}>Novo usuário</button>}
      </div>
      {members.data && members.data.length === 0 && <p className="muted small">Ninguém tem acesso direto a esta conta.</p>}
      {members.data && members.data.length > 0 && (
        <table className="table">
          <tbody>
            {members.data.map((m) => (
              <tr key={m.id}>
                <td>
                  <b>{m.name || m.subject}</b>
                  <div className="kind">
                    {m.subject}{m.email ? ` · ${m.email}` : ""}
                    {m.local && (m.totp_enabled ? <span className="pill pill-ok" style={{ marginLeft: 6 }}>2FA</span> : null)}
                  </div>
                </td>
                <td className="kind">
                  {roleLabel[m.role] ?? m.role}
                  {m.project_id ? `, só no VDC ${m.project_name ?? ""}` : ""}
                  <div>{m.local ? (m.last_login_at ? `último acesso ${when(m.last_login_at)}` : "nunca entrou") : "login da empresa"}</div>
                </td>
                <td className="actions">
                  {m.subject !== s.me.subject && (
                    <span style={{ display: "inline-flex", gap: 6 }}>
                      {m.local && <button className="btn btn-sm" onClick={() => setResetting(m)}>Resetar senha</button>}
                      <button className="btn btn-sm btn-danger" onClick={() => setRemoving(m)}>Remover</button>
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {!local && <LinkSubject t={t} onDone={refresh} />}

      {adding && (
        <NewUserModal t={t} onClose={() => setAdding(false)}
          onCreated={(who, password) => { setAdding(false); setSecret({ who, password, created: true }); refresh(); }} />
      )}
      {resetting && (
        <ResetModal m={resetting} onClose={() => setResetting(null)}
          onDone={(password) => { setSecret({ who: resetting.subject, password, created: false }); setResetting(null); refresh(); }} />
      )}
      {removing && <RemoveModal t={t} m={removing} onClose={() => setRemoving(null)} onDone={() => { setRemoving(null); refresh(); }} />}
      {secret && (
        <Modal title={secret.created ? "Usuário criado" : "Senha resetada"} onClose={() => setSecret(null)}
          footer={<button className="btn btn-primary" onClick={() => setSecret(null)}>Pronto, anotei</button>}>
          <p>Passe para <b>{secret.who}</b> a senha temporária abaixo. Ela aparece só agora; no primeiro acesso o portal pede uma senha nova.</p>
          <div className="temp-pass">
            <code>{secret.password}</code>
            <button className="btn btn-sm" onClick={() => navigator.clipboard?.writeText(secret.password)}>Copiar</button>
          </div>
        </Modal>
      )}
    </section>
  );
}

function NewUserModal({ t, onClose, onCreated }: { t: Tenant; onClose: () => void; onCreated: (who: string, password: string) => void }) {
  const options = rolesFor[t.kind];
  const [f, setF] = useState({ username: "", name: "", email: "", role: options[0], project: "" });
  const projects = useQuery({
    queryKey: ["projects", t.id],
    queryFn: () => api<{ projects: Project[] }>(`/v1/tenants/${t.id}/projects`).then((r) => r.projects),
    enabled: t.kind !== "provider",
  });
  const create = useMutation({
    mutationFn: () =>
      api<{ temporary_password: string }>(`/v1/tenants/${t.id}/users`, {
        method: "POST",
        json: { username: f.username.trim(), name: f.name.trim(), email: f.email.trim(), role: f.role, project_id: f.project || null },
      }),
    onSuccess: (d) => onCreated(f.username.trim(), d.temporary_password),
  });
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.value });
  const okUser = /^[a-z0-9][a-z0-9._-]{1,62}$/.test(f.username.trim());
  return (
    <Modal title={`Novo usuário em ${t.name}`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-primary" disabled={!okUser || !f.name.trim() || create.isPending} onClick={() => create.mutate()}>Criar usuário</button>
      </>}>
      <div className="stack" style={{ gap: 12 }}>
        <label className="field">
          Usuário
          <input className="input" value={f.username} onChange={(e) => setF({ ...f, username: e.target.value.toLowerCase() })} autoComplete="off" placeholder="maria.silva" />
          <span className="hint">Usado para entrar. Letras minúsculas, números, ponto, hífen.</span>
        </label>
        <label className="field">Nome<input className="input" value={f.name} onChange={set("name")} placeholder="Maria Silva" /></label>
        <label className="field">E-mail (opcional)<input className="input" type="email" value={f.email} onChange={set("email")} /></label>
        <label className="field">
          Papel
          <select className="select" value={f.role} onChange={set("role")}>
            {options.map((r) => <option key={r} value={r}>{roleLabel[r]}</option>)}
          </select>
        </label>
        {t.kind !== "provider" && (f.role === "operator" || f.role === "viewer") && (projects.data?.length ?? 0) > 0 && (
          <label className="field">
            Alcance
            <select className="select" value={f.project} onChange={set("project")}>
              <option value="">Todos os VDCs</option>
              {projects.data?.map((p) => <option key={p.id} value={p.id}>Só {p.name}</option>)}
            </select>
          </label>
        )}
        {create.error && <div className="alert">{create.error.message}</div>}
      </div>
    </Modal>
  );
}

function ResetModal({ m, onClose, onDone }: { m: MembershipView; onClose: () => void; onDone: (password: string) => void }) {
  const [totp, setTotp] = useState(false);
  const reset = useMutation({
    mutationFn: () => api<{ temporary_password: string }>(`/v1/users/${encodeURIComponent(m.subject)}/reset-password`, { method: "POST", json: { reset_totp: totp } }),
    onSuccess: (d) => onDone(d.temporary_password),
  });
  return (
    <Modal title={`Resetar a senha de ${m.name || m.subject}?`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-primary" disabled={reset.isPending} onClick={() => reset.mutate()}>Gerar senha temporária</button>
      </>}>
      <p>A senha atual deixa de valer e as sessões abertas são encerradas.</p>
      {m.totp_enabled && (
        <label className="checks" style={{ marginTop: 12 }}>
          <span><input type="checkbox" checked={totp} onChange={(e) => setTotp(e.target.checked)} /> Desativar também a verificação em duas etapas (celular perdido)</span>
        </label>
      )}
      {reset.error && <div className="alert" style={{ marginTop: 12 }}>{reset.error.message}</div>}
    </Modal>
  );
}

function RemoveModal({ t, m, onClose, onDone }: { t: Tenant; m: MembershipView; onClose: () => void; onDone: () => void }) {
  const del = useMutation({
    mutationFn: () => api(`/v1/tenants/${t.id}/memberships/${m.id}`, { method: "DELETE" }),
    onSuccess: onDone,
  });
  return (
    <Modal title={`Remover o acesso de ${m.name || m.subject}?`} onClose={onClose}
      footer={<>
        <button className="btn" onClick={onClose}>Cancelar</button>
        <button className="btn btn-danger-solid" disabled={del.isPending} onClick={() => del.mutate()}>Remover acesso</button>
      </>}>
      <p>A pessoa deixa de ver {t.name}{m.project_id ? ` (VDC ${m.project_name ?? ""})` : ""}. Se não tiver outro acesso, não consegue mais entrar.</p>
      {del.error && <div className="alert" style={{ marginTop: 12 }}>{del.error.message}</div>}
    </Modal>
  );
}

// No modo OIDC, o usuário já existe no Keycloak: só se vincula o "sub".
function LinkSubject({ t, onDone }: { t: Tenant; onDone: () => void }) {
  const options = rolesFor[t.kind];
  const [subject, setSubject] = useState("");
  const [role, setRole] = useState(options[options.length - 1]);
  const add = useMutation({
    mutationFn: () => api(`/v1/tenants/${t.id}/memberships`, { method: "POST", json: { subject: subject.trim(), role } }),
    onSuccess: () => { setSubject(""); onDone(); },
  });
  return (
    <form style={{ marginTop: 16 }} onSubmit={(e) => { e.preventDefault(); add.mutate(); }}>
      <h3 style={{ marginBottom: 10 }}>Dar acesso</h3>
      <div className="form-grid">
        <label className="field">Usuário no SSO<input className="input" value={subject} onChange={(e) => setSubject(e.target.value)} /></label>
        <label className="field">
          Papel
          <select className="select" value={role} onChange={(e) => setRole(e.target.value)}>
            {options.map((r) => <option key={r} value={r}>{roleLabel[r]}</option>)}
          </select>
        </label>
      </div>
      {add.error && <div className="alert" style={{ marginTop: 12 }}>{add.error.message}</div>}
      <div className="form-actions"><button className="btn btn-primary" disabled={!subject.trim() || add.isPending}>Dar acesso</button></div>
    </form>
  );
}
