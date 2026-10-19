import { useMemo, useState } from "react";
import { Link, Navigate } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, kindLabel, type Tenant, type TenantKind } from "../api";
import { useSession } from "../session";
import { Modal } from "../components/Modal";

// Ordena a árvore em profundidade para exibir com recuo.
function flatten(tenants: Tenant[]): { t: Tenant; depth: number }[] {
  const byParent = new Map<string | undefined, Tenant[]>();
  const ids = new Set(tenants.map((t) => t.id));
  for (const t of tenants) {
    const key = t.parent_id && ids.has(t.parent_id) ? t.parent_id : undefined;
    byParent.set(key, [...(byParent.get(key) ?? []), t]);
  }
  const out: { t: Tenant; depth: number }[] = [];
  const walk = (parent: string | undefined, depth: number) => {
    const kids = (byParent.get(parent) ?? []).sort((a, b) =>
      a.kind === b.kind ? a.name.localeCompare(b.name, "pt-BR") : a.kind === "partner" ? -1 : 1,
    );
    for (const k of kids) {
      out.push({ t: k, depth });
      walk(k.id, depth + 1);
    }
  };
  walk(undefined, 0);
  return out;
}

export function TenantsPage() {
  const s = useSession();
  const rows = useMemo(() => flatten(s.tenants), [s.tenants]);
  const [creating, setCreating] = useState(false);

  // Cliente final e administrador de MSP vão direto para a própria conta.
  if (!s.isStaff && s.home) return <Navigate to={`/clientes/${s.home.id}`} replace />;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{s.isStaff ? "MSPs e clientes" : "Clientes"}</h1>
          <p>{s.isStaff ? "Cada MSP recebe uma cota e a distribui em VDCs para uso próprio ou dos clientes." : "Os clientes que a sua empresa atende."}</p>
        </div>
        {s.canCreateTenants && !creating && (
          <button className="btn btn-primary" onClick={() => setCreating(true)}>{s.isStaff ? "Nova conta" : "Novo cliente"}</button>
        )}
      </div>

      {creating && (
        <Modal title="Nova conta" onClose={() => setCreating(false)}>
          <NewTenant onDone={() => setCreating(false)} />
        </Modal>
      )}

      <div className="table-wrap">
        <table className="table">
          <thead>
            <tr><th>Nome</th><th>Tipo</th><th>Situação</th></tr>
          </thead>
          <tbody>
            {rows.map(({ t, depth }) => (
              <tr key={t.id}>
                <td style={{ paddingLeft: 10 + depth * 22 }}>
                  {depth > 0 && <span className="tree-indent muted" aria-hidden="true">└ </span>}
                  <Link to={`/clientes/${t.id}`}>{t.name}</Link>
                </td>
                <td className="kind">{kindLabel[t.kind]}</td>
                <td className="kind">{t.status === "active" ? "Ativo" : t.status === "suspended" ? "Suspenso" : "Encerrado"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function NewTenant({ onDone }: { onDone: () => void }) {
  const s = useSession();
  const qc = useQueryClient();
  const provider = s.tenants.find((t) => t.kind === "provider");
  const msps = s.tenants.filter((t) => t.kind === "partner");
  // Admin cria MSPs (sob o provedor) e clientes (sob um MSP); o MSP só cria clientes seus.
  const [kind, setKind] = useState<TenantKind>("customer");
  const [msp, setMsp] = useState(msps[0]?.id ?? "");
  const [name, setName] = useState("");
  const parentId = kind === "partner" ? provider?.id : msp;

  const create = useMutation({
    mutationFn: () => api<Tenant>("/v1/tenants", { method: "POST", json: { parent_id: parentId, kind, name } }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["tenants"] });
      onDone();
    },
  });

  return (
    <form onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <div className="form-grid">
        {provider && (
          <label className="field">
            Tipo
            <select className="select" value={kind} onChange={(e) => setKind(e.target.value as TenantKind)}>
              <option value="customer">Cliente final</option>
              <option value="partner">MSP</option>
            </select>
          </label>
        )}
        <label className="field">
          Nome
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
        </label>
        {kind === "customer" && msps.length > 1 && (
          <label className="field">
            Atendido pelo MSP
            <select className="select" value={msp} onChange={(e) => setMsp(e.target.value)}>
              {msps.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
            </select>
          </label>
        )}
      </div>
      {kind === "partner" && (
        <p className="muted small" style={{ marginTop: 10 }}>O MSP nasce sem recursos. Defina a cota dele em seguida.</p>
      )}
      {create.error && <div className="alert" style={{ marginTop: 12 }}>{create.error.message}</div>}
      <div className="form-actions">
        <button type="button" className="btn btn-ghost" onClick={onDone}>Cancelar</button>
        <button className="btn btn-primary" disabled={!name.trim() || !parentId || create.isPending}>
          {kind === "partner" ? "Criar MSP" : "Criar cliente"}
        </button>
      </div>
    </form>
  );
}
