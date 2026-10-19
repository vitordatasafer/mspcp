import { Fragment, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  actionLabel, api, gbText, initials, roleLabel, when,
  type Capacity, type Cluster, type Project, type Resource, type TaskView,
} from "../api";
import { TaskTarget } from "../components/TaskTarget";
import { useSession } from "../session";
import { IconChevronRight, IconClock } from "../components/Icons";

const gb = (bytes: number) => Math.round(bytes / 1024 ** 3);
const mbToGb = (mb: number) => Math.round(mb / 1024);

export function DashboardPage() {
  const s = useSession();
  const resources = useQuery({ queryKey: ["resources"], queryFn: () => api<{ resources: Resource[] }>("/v1/resources").then((r) => r.resources), refetchInterval: 15_000 });
  const projects = useQuery({ queryKey: ["projects", "all"], queryFn: () => api<{ projects: Project[] }>("/v1/projects").then((r) => r.projects), refetchInterval: 15_000 });
  const isMSP = !s.isStaff && s.home?.kind === "partner";
  const capacity = useQuery({ queryKey: ["capacity", s.home?.id], queryFn: () => api<Capacity>(`/v1/tenants/${s.home!.id}/capacity`), enabled: isMSP });
  const clusters = useQuery({ queryKey: ["clusters"], queryFn: () => api<{ clusters: Cluster[] }>("/v1/clusters").then((r) => r.clusters), enabled: s.canClusters });
  const tasks = useQuery({ queryKey: ["tasks", "recent"], queryFn: () => api<{ tasks: TaskView[] }>("/v1/tasks?limit=50").then((r) => r.tasks) });

  const rs = resources.data ?? [];
  const ps = projects.data ?? [];
  const running = rs.filter((r) => r.status === "running");
  const stopped = rs.filter((r) => r.status === "stopped").length;

  // vCPU e memória: o que cada perfil precisa acompanhar.
  let cpu: DonutData, mem: DonutData;
  if (isMSP && capacity.data) {
    const { limits, usage } = capacity.data;
    cpu = { title: "vCPU da cota do MSP", used: usage.allocated_cpu, total: limits.cpu_cores, rows: [["Distribuído", `${usage.allocated_cpu}`], ["Ligado", `${usage.running_cpu}`], ["Cota", limits.cpu_cores === null ? "sem limite" : `${limits.cpu_cores}`]] };
    mem = { title: "Memória da cota do MSP", used: mbToGb(usage.allocated_memory_mb), total: limits.memory_mb === null ? null : mbToGb(limits.memory_mb), rows: [["Distribuído", gbText(mbToGb(usage.allocated_memory_mb))], ["Ligado", gbText(mbToGb(usage.running_memory_mb))], ["Cota", limits.memory_mb === null ? "sem limite" : gbText(mbToGb(limits.memory_mb))]] };
  } else if (!s.isStaff) {
    const quotaCpu = ps.reduce((n, p) => n + p.cpu_cores, 0);
    const quotaMem = ps.reduce((n, p) => n + p.memory_mb, 0);
    const runCpu = ps.reduce((n, p) => n + (p.usage?.running_cpu ?? 0), 0);
    const runMem = ps.reduce((n, p) => n + (p.usage?.running_memory_mb ?? 0), 0);
    cpu = { title: "vCPU ligadas nos VDCs", used: runCpu, total: quotaCpu, rows: [["Em uso", `${runCpu}`], ["Cota", `${quotaCpu}`]] };
    mem = { title: "Memória ligada nos VDCs", used: mbToGb(runMem), total: mbToGb(quotaMem), rows: [["Em uso", gbText(mbToGb(runMem))], ["Cota", gbText(mbToGb(quotaMem))]] };
  } else {
    const totCpu = rs.reduce((n, r) => n + r.cpus, 0);
    const runCpu = running.reduce((n, r) => n + r.cpus, 0);
    const totMem = rs.reduce((n, r) => n + r.max_mem_bytes, 0);
    const runMem = running.reduce((n, r) => n + r.max_mem_bytes, 0);
    cpu = { title: "vCPU em servidores ligados", used: runCpu, total: totCpu, rows: [["Ligadas", `${runCpu}`], ["Configuradas", `${totCpu}`]] };
    mem = { title: "Memória em servidores ligados", used: gb(runMem), total: gb(totMem), rows: [["Ligada", gbText(gb(runMem))], ["Configurada", gbText(gb(totMem))]] };
  }

  const busiest = [...ps]
    .filter((p) => p.cpu_cores > 0)
    .sort((a, b) => (b.usage?.running_cpu ?? 0) / b.cpu_cores - (a.usage?.running_cpu ?? 0) / a.cpu_cores)
    .slice(0, 5);
  const topRole = s.me.memberships[0]?.role;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Visão geral</h1>
          {s.home && <p>{s.home.name}</p>}
        </div>
      </div>
      <div className="dash">
        <section className="card" aria-labelledby="c-srv">
          <h2 id="c-srv">Servidores <Link to="/servidores" aria-label="Ver servidores"><IconChevronRight /></Link></h2>
          <Stat total={rs.length} label="Total" rows={[["ok", "Ligados", running.length], ["err", "Desligados", stopped], ["", "Outros", rs.length - running.length - stopped]]} />
        </section>

        {s.canClusters ? (
          <section className="card" aria-labelledby="c-cl">
            <h2 id="c-cl">Clusters <Link to="/clusters" aria-label="Ver clusters"><IconChevronRight /></Link></h2>
            <Stat
              total={clusters.data?.length ?? 0}
              label="Total"
              rows={[
                ["ok", "Sincronizando", clusters.data?.filter((c) => c.enabled && c.last_sync_at && !c.last_sync_error).length ?? 0],
                ["err", "Falhando", clusters.data?.filter((c) => c.last_sync_error).length ?? 0],
                ["", "Aguardando", clusters.data?.filter((c) => c.enabled && !c.last_sync_at && !c.last_sync_error).length ?? 0],
              ]}
            />
          </section>
        ) : (
          <section className="card" aria-labelledby="c-vdc">
            <h2 id="c-vdc">VDCs <Link to="/vdcs" aria-label="Ver VDCs"><IconChevronRight /></Link></h2>
            <Stat
              total={ps.length}
              label="Total"
              rows={[
                ["info", "Localizações", new Set(ps.map((p) => p.location)).size],
                ["ok", "Com servidor ligado", ps.filter((p) => (p.usage?.running_cpu ?? 0) > 0).length],
                ["", "Servidores", ps.reduce((n, p) => n + (p.usage?.vms ?? 0), 0)],
              ]}
            />
          </section>
        )}

        <section className="card welcome" aria-label="Boas-vindas">
          <div className="welcome-band" />
          <div className="welcome-body">
            <div className="welcome-avatar"><span className="avatar">{initials(s.name)}</span></div>
            <h2>Bem-vindo, {s.name.split(" ")[0]}</h2>
            <p className="muted small">{topRole ? roleLabel[topRole] : ""}</p>
            <Clock />
          </div>
        </section>

        <section className="card" aria-label={cpu.title}><h2>{cpu.title}</h2><Donut d={cpu} tone="primary" /></section>
        <section className="card" aria-label={mem.title}><h2>{mem.title}</h2><Donut d={mem} tone="warn" fmt={gbText} /></section>

        <section className="card span-rows" aria-labelledby="c-tk">
          <h2 id="c-tk">Tarefas recentes <Link to="/tarefas" aria-label="Ver tarefas"><IconChevronRight /></Link></h2>
          {(tasks.data ?? []).length === 0 ? (
            <p className="muted small">Nenhuma ação feita pelo portal ainda.</p>
          ) : (
            <ul className="history">
              {(tasks.data ?? []).slice(0, 8).map((t) => (
                <li key={t.id}>
                  <span className={`dot ${t.status === "succeeded" ? "ok" : t.status === "failed" ? "err" : "busy"}`} aria-hidden="true" />
                  <span>
                    {actionLabel[t.action].verb} <TaskTarget t={t} />
                    <span className="muted small"> por {t.requested_by}</span>
                  </span>
                  <span className="muted small">{when(t.created_at)}</span>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="card span-2" aria-labelledby="c-top">
          <h2 id="c-top">VDCs mais ocupados <Link to="/vdcs" aria-label="Ver VDCs"><IconChevronRight /></Link></h2>
          {busiest.length === 0 ? (
            <p className="muted small">Nenhum VDC ainda.</p>
          ) : (
            <table className="table">
              <thead><tr><th>VDC</th>{s.multiTenant && <th>Cliente</th>}<th>Localização</th><th>vCPU ligadas</th><th>Memória ligada</th></tr></thead>
              <tbody>
                {busiest.map((p) => (
                  <tr key={p.id}>
                    <td>{p.name}</td>
                    {s.multiTenant && <td>{p.tenant_name}</td>}
                    <td>{p.location}</td>
                    <td><Mini used={p.usage?.running_cpu ?? 0} total={p.cpu_cores} /></td>
                    <td><Mini used={mbToGb(p.usage?.running_memory_mb ?? 0)} total={mbToGb(p.memory_mb)} fmt={gbText} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      </div>
    </>
  );
}

function Stat({ total, label, rows }: { total: number; label: string; rows: [string, string, number][] }) {
  return (
    <div className="stat">
      <div className="stat-total"><b>{total}</b><span>{label}</span></div>
      <ul>
        {rows.map(([dot, name, n]) => (
          <li key={name}><span><span className={`dot ${dot}`} />{name}</span><b>{n}</b></li>
        ))}
      </ul>
    </div>
  );
}

interface DonutData { title: string; used: number; total: number | null; rows: [string, string][] }

function Donut({ d, tone, fmt = String }: { d: DonutData; tone: "primary" | "warn"; fmt?: (n: number) => string }) {
  const pct = d.total && d.total > 0 ? Math.min(100, (d.used / d.total) * 100) : 0;
  const color = pct >= 90 ? "var(--err)" : tone === "warn" ? "var(--warn)" : "var(--primary)";
  const r = 44, c = 2 * Math.PI * r;
  return (
    <div className="donut-row">
      <svg className="donut" viewBox="0 0 112 112" role="img" aria-label={`${pct.toFixed(0)}% de ${d.title}`}>
        <circle className="donut-track" cx="56" cy="56" r={r} fill="none" strokeWidth="10" />
        <circle cx="56" cy="56" r={r} fill="none" stroke={color} strokeWidth="10" strokeLinecap="round"
          strokeDasharray={`${(pct / 100) * c} ${c}`} transform="rotate(-90 56 56)" />
      </svg>
      <div className="donut-val">
        <b style={{ color }}>{d.total === null ? fmt(d.used) : `${pct.toLocaleString("pt-BR", { maximumFractionDigits: 1 })}%`}</b>
        <dl>{d.rows.map(([k, v]) => <Fragment key={k}><dt>{k}</dt><dd>{v}</dd></Fragment>)}</dl>
      </div>
    </div>
  );
}

export function Mini({ used, total, fmt = String }: { used: number; total: number; fmt?: (n: number) => string }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return (
    <div className="mini">
      <span>{fmt(used)} de {fmt(total)}</span>
      <div className={`meter-bar${pct >= 100 ? " full" : ""}`}><span className="used" style={{ width: `${pct}%` }} /></div>
    </div>
  );
}

function Clock() {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(t);
  }, []);
  return (
    <div className="clock" aria-label="Horário">
      <IconClock />
      {now.toLocaleString("pt-BR", { dateStyle: "short", timeStyle: "medium" })}
    </div>
  );
}
