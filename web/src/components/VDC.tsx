import { useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, gbText, type Capacity, type Project, type Tenant } from "../api";
import { useSession } from "../session";
import { Meter } from "./Meter";

const gb = (mb: number) => Math.round(mb / 1024);

// Uma linha de VDC: identificação e consumo contra a cota.
export function VDCRow({ v, showTenant, onEdit }: { v: Project; showTenant: boolean; onEdit?: () => void }) {
  const u = v.usage;
  return (
    <div className="vdc">
      <div className="vdc-name">
        <strong>{v.name}</strong>
        <span>
          {showTenant && v.tenant_name ? `${v.tenant_name}, ` : ""}
          {v.location || "Sem localização"}
          {u ? `, ${u.vms} ${u.vms === 1 ? "servidor" : "servidores"}` : ""}
        </span>
      </div>
      <div>
        <div className="meters">
          <Meter label="vCPU" limit={v.cpu_cores} used={u?.running_cpu ?? 0} />
          <Meter label="Memória" limit={gb(v.memory_mb)} used={gb(u?.running_memory_mb ?? 0)} fmt={(n) => gbText(n)} />
        </div>
        <p className="meter-note" style={{ marginTop: 8 }}>
          Disco: {v.storage.length ? v.storage.map((s) => `${s.class_name} ${gbText(s.used_gb)} de ${gbText(s.capacity_gb)}`).join(", ") : "sem cota"}
          {u && u.disk_gb > v.storage.reduce((n, s) => n + s.used_gb, 0) ? `; ${gbText(u.disk_gb)} no total das VMs` : ""}
        </p>
      </div>
      <div>{onEdit && <button className="btn btn-sm" onClick={onEdit}>Ajustar cota</button>}</div>
    </div>
  );
}

// Formulário de VDC: criação (sem `vdc`) ou ajuste de cota. Mostra quanto
// ainda há no pool do MSP para cada recurso.
export function VDCForm({
  capacity,
  owners,
  vdc,
  defaultOwner,
  onDone,
}: {
  capacity: Capacity;
  owners: Tenant[]; // quem pode receber o VDC (o MSP e/ou seus clientes)
  vdc?: Project;
  defaultOwner?: string;
  onDone: () => void;
}) {
  const s = useSession();
  const qc = useQueryClient();
  const editing = !!vdc;
  const [owner, setOwner] = useState(vdc?.tenant_id ?? defaultOwner ?? owners[0]?.id ?? "");
  const [name, setName] = useState(vdc?.name ?? "");
  const [locationId, setLocationId] = useState(vdc?.location_id ?? capacity.locations[0]?.id ?? "");
  const [cpu, setCpu] = useState(String(vdc?.cpu_cores ?? 2));
  const [mem, setMem] = useState(String(vdc ? gb(vdc.memory_mb) : 4));
  const [disk, setDisk] = useState<Record<string, string>>(
    Object.fromEntries((vdc?.storage ?? []).map((x) => [x.class_id, String(x.capacity_gb)])),
  );

  const location = capacity.locations.find((l) => l.id === locationId);
  // Reserva do MSP na localização escolhida; ao ajustar, a cota atual deste
  // VDC volta para a reserva.
  const res = capacity.limits.reservations.find((r) => r.location_id === (vdc?.location_id ?? locationId));
  const free = useMemo(() => {
    const back = (n: number) => (editing ? n : 0);
    return {
      cpu: res ? res.cpu_cores - res.allocated_cpu + back(vdc?.cpu_cores ?? 0) : 0,
      mem: res ? gb(res.memory_mb - res.allocated_memory_mb + back(vdc?.memory_mb ?? 0)) : 0,
      disk: (classId: string) => {
        if (!res) return 0;
        const mine = vdc?.storage.find((x) => x.class_id === classId)?.capacity_gb ?? 0;
        return (res.storage[classId] ?? 0) - (res.allocated_storage[classId] ?? 0) + back(mine);
      },
    };
  }, [res, vdc, editing]);

  const save = useMutation({
    mutationFn: () => {
      const body = {
        name,
        location_id: locationId,
        cpu_cores: Number(cpu),
        memory_mb: Number(mem) * 1024,
        storage: Object.entries(disk)
          .filter(([, v]) => v !== "" && Number(v) > 0)
          .map(([class_id, v]) => ({ class_id, capacity_gb: Number(v) })),
      };
      return editing
        ? api(`/v1/projects/${vdc!.id}/quota`, { method: "PUT", json: body })
        : api(`/v1/tenants/${owner}/projects`, { method: "POST", json: body });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["capacity"] });
      qc.invalidateQueries({ queryKey: ["projects"] });
      qc.invalidateQueries({ queryKey: ["resources"] });
      onDone();
    },
  });

  const avail = (n: number, unit: (n: number) => string) =>
    `${unit(Math.max(0, n))} livres na reserva${res ? ` em ${res.location}` : ""}`;

  return (
    <form onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
      <div className="form-grid">
        {!editing && (
          <>
            <label className="field">
              Nome
              <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="Produção" required autoFocus />
            </label>
            {owners.length > 1 && (
              <label className="field">
                Para
                <select className="select" value={owner} onChange={(e) => setOwner(e.target.value)}>
                  {owners.map((t) => (
                    <option key={t.id} value={t.id}>{t.kind === "partner" ? `${t.name} (uso próprio)` : t.name}</option>
                  ))}
                </select>
              </label>
            )}
            <label className="field">
              Localização
              <select className="select" value={locationId} onChange={(e) => { setLocationId(e.target.value); setDisk({}); }}>
                {capacity.locations.map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}
              </select>
              {capacity.locations.length === 0 && <span className="hint">O MSP ainda não tem recursos reservados em nenhum cluster.</span>}
            </label>
          </>
        )}
        <label className="field">
          vCPU
          <input className="input" type="number" min={1} value={cpu} onChange={(e) => setCpu(e.target.value)} required />
          <span className="hint">{avail(free.cpu, String)}</span>
        </label>
        <label className="field">
          Memória
          <span className="input-suffix">
            <input className="input" type="number" min={1} value={mem} onChange={(e) => setMem(e.target.value)} required />
            <span>GB</span>
          </span>
          <span className="hint">{avail(free.mem, gbText)}</span>
        </label>
        {(location?.storage_classes ?? []).map((c) => (
          <label className="field" key={c.id}>
            Disco {c.name}
            <span className="input-suffix">
              <input
                className="input"
                type="number"
                min={0}
                value={disk[c.id] ?? ""}
                placeholder="0"
                onChange={(e) => setDisk({ ...disk, [c.id]: e.target.value })}
              />
              <span>GB</span>
            </span>
            <span className="hint">{avail(free.disk(c.id), gbText)}</span>
          </label>
        ))}
      </div>
      {save.error && <div className="alert" style={{ marginTop: 12 }}>{save.error.message}</div>}
      <div className="form-actions">
        <button type="button" className="btn btn-ghost" onClick={onDone}>Cancelar</button>
        <button className="btn btn-primary" disabled={save.isPending || (!editing && (!name.trim() || !locationId))}>
          {editing ? "Salvar cota" : "Criar VDC"}
        </button>
      </div>
      {!s.isStaff && !editing && (
        <p className="muted small" style={{ marginTop: 8 }}>
          Os recursos saem da reserva do seu MSP nessa localização. O cliente não pode passar dessa cota ao ligar servidores.
        </p>
      )}
    </form>
  );
}
