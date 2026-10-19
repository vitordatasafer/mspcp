import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, fold, gbText, type Capacity, type Project, type Tenant } from "../api";
import { useSession } from "../session";
import { Modal } from "../components/Modal";
import { VDCForm } from "../components/VDC";
import { IconPlus, IconRefresh, IconSearch } from "../components/Icons";
import { Mini } from "./Dashboard";

const gb = (mb: number) => Math.round(mb / 1024);

export function VDCsPage() {
  const s = useSession();
  const qc = useQueryClient();
  const [q, setQ] = useState("");
  const [editing, setEditing] = useState<Project | null>(null);
  const [creating, setCreating] = useState(false);
  const vdcs = useQuery({
    queryKey: ["projects", "all"],
    queryFn: () => api<{ projects: Project[] }>("/v1/projects").then((r) => r.projects),
    refetchInterval: 15_000,
  });
  const term = fold(q.trim());
  const list = (vdcs.data ?? []).filter((v) => !term || [v.name, v.tenant_name, v.location].some((x) => fold(x).includes(term)));

  return (
    <>
      <div className="page-head">
        <div>
          <h1>VDCs</h1>
          <p>Cada VDC reserva vCPU, memória e disco numa localização. Ligar um servidor respeita a cota do VDC.</p>
        </div>
      </div>
      <div className="toolbar">
        <button className="btn btn-icon" aria-label="Atualizar" title="Atualizar" onClick={() => qc.invalidateQueries({ queryKey: ["projects"] })}><IconRefresh /></button>
        {s.canManageVDCs && (
          <button className="btn btn-primary" onClick={() => setCreating(true)}><IconPlus />Novo VDC</button>
        )}
        <div className="input-icon">
          <IconSearch />
          <label className="visually-hidden" htmlFor="busca-vdc">Filtrar VDCs</label>
          <input id="busca-vdc" className="input" type="search" placeholder="Nome, cliente ou localização" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
      </div>

      <div className="table-wrap">
        <table className="table">
          <thead>
            <tr>
              <th>Nome</th>
              {s.multiTenant && <th>Cliente</th>}
              <th>Localização</th>
              <th>vCPU ligadas</th>
              <th>Memória ligada</th>
              <th>Disco (usado de cota)</th>
              <th className="num">Servidores</th>
              {s.canManageVDCs && <th className="actions">Ações</th>}
            </tr>
          </thead>
          <tbody>
            {vdcs.isPending && <tr><td colSpan={8} className="muted">Carregando…</td></tr>}
            {list.map((v) => (
              <tr key={v.id}>
                <td>{v.name}</td>
                {s.multiTenant && <td>{s.canAdmin ? <Link to={`/clientes/${v.tenant_id}`}>{v.tenant_name}</Link> : v.tenant_name}</td>}
                <td>{v.location || "—"}</td>
                <td><Mini used={v.usage?.running_cpu ?? 0} total={v.cpu_cores} /></td>
                <td><Mini used={gb(v.usage?.running_memory_mb ?? 0)} total={gb(v.memory_mb)} fmt={gbText} /></td>
                <td className="kind">{v.storage.length ? v.storage.map((x) => `${x.class_name} ${gbText(x.used_gb)} de ${gbText(x.capacity_gb)}`).join(", ") : "sem cota"}</td>
                <td className="num">{v.usage?.vms ?? 0}</td>
                {s.canManageVDCs && <td className="actions"><button className="btn btn-sm" onClick={() => setEditing(v)}>Ajustar cota</button></td>}
              </tr>
            ))}
          </tbody>
        </table>
        {vdcs.data && list.length === 0 && (
          <div className="empty">
            <strong>{term ? "Nenhum VDC com esse filtro" : "Nenhum VDC ainda"}</strong>
            {term ? "Tente outro termo." : s.canManageVDCs ? "Crie um VDC para reservar recursos a um cliente ou ao próprio MSP." : "Fale com o seu provedor para receber recursos."}
          </div>
        )}
      </div>

      {editing && <VDCModal vdc={editing} onClose={() => setEditing(null)} />}
      {creating && <VDCModal onClose={() => setCreating(false)} />}
    </>
  );
}

// O MSP responsável por uma conta: ela mesma, se for MSP; senão o pai.
function mspOf(tenants: Tenant[], tenantId: string): Tenant | undefined {
  const t = tenants.find((x) => x.id === tenantId);
  if (!t) return undefined;
  return t.kind === "partner" ? t : tenants.find((x) => x.id === t.parent_id);
}

export function VDCModal({ vdc, onClose, mspId, onlyOwner }: { vdc?: Project; onClose: () => void; mspId?: string; onlyOwner?: string }) {
  const s = useSession();
  const msps = s.tenants.filter((t) => t.kind === "partner");
  const initial = mspId ?? (vdc ? mspOf(s.tenants, vdc.tenant_id)?.id : s.home?.kind === "partner" ? s.home.id : msps[0]?.id);
  const [msp, setMsp] = useState(initial ?? "");
  const cap = useQuery({ queryKey: ["capacity", msp], queryFn: () => api<Capacity>(`/v1/tenants/${msp}/capacity`), enabled: !!msp });
  const owners = onlyOwner
    ? s.tenants.filter((t) => t.id === onlyOwner)
    : [...s.tenants.filter((t) => t.id === msp), ...s.tenants.filter((t) => t.parent_id === msp)];

  return (
    <Modal title={vdc ? `Ajustar cota de ${vdc.name}` : "Novo VDC"} onClose={onClose} wide>
      {!vdc && !mspId && s.isStaff && msps.length > 1 && (
        <label className="field" style={{ marginBottom: 14, maxWidth: 320 }}>
          MSP
          <select className="select" value={msp} onChange={(e) => setMsp(e.target.value)}>
            {msps.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
          </select>
          <span className="hint">Os recursos saem da cota deste MSP.</span>
        </label>
      )}
      {cap.isPending && <p className="muted">Carregando cota do MSP…</p>}
      {cap.error && <div className="alert">{cap.error.message}</div>}
      {cap.data && (
        cap.data.locations.length === 0 && !vdc
          ? <div className="alert">Este MSP ainda não tem localização liberada. Defina a cota dele primeiro.</div>
          : <VDCForm key={msp} capacity={cap.data} owners={owners} vdc={vdc} defaultOwner={onlyOwner} onDone={onClose} />
      )}
    </Modal>
  );
}
