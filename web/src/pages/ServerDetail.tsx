import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { actionLabel, api, ApiError, bytes, dateTime, duration, gbText, when, type Project, type Resource, type Task } from "../api";
import { useSession } from "../session";
import { DetailActions, StatusText, usePower } from "../components/Power";
import { IconChevronRight, IconMonitor } from "../components/Icons";
import { Meter } from "../components/Meter";

type Tab = "info" | "history";

export function ServerDetailPage() {
  const { id } = useParams();
  const s = useSession();
  const nav = useNavigate();
  const [tab, setTab] = useState<Tab>("info");
  const { pending } = usePower();

  const res = useQuery({
    queryKey: ["resources", id],
    queryFn: () => api<Resource>(`/v1/resources/${id}`),
    refetchInterval: (q) => (q.state.data && (pending[q.state.data.id] || q.state.data.deleting) ? 2000 : 10_000),
    retry: false,
  });
  const projects = useQuery({ queryKey: ["projects", "all"], queryFn: () => api<{ projects: Project[] }>("/v1/projects").then((r) => r.projects) });
  const tasks = useQuery({
    queryKey: ["tasks", id],
    queryFn: () => api<{ tasks: Task[] }>(`/v1/resources/${id}/tasks`).then((x) => x.tasks),
    enabled: !!res.data,
  });

  if (res.error) {
    const missing = res.error instanceof ApiError && res.error.status === 404;
    return (
      <div className="panel empty">
        <strong>{missing ? "Servidor não encontrado" : "Não foi possível abrir o servidor"}</strong>
        {missing ? "Ele pode ter sido removido ou estar fora do seu acesso." : res.error.message}{" "}
        <Link to="/servidores">Voltar para servidores</Link>
      </div>
    );
  }
  if (!res.data) return <p className="muted">Carregando…</p>;
  const r = res.data;
  const vdc = projects.data?.find((p) => p.id === r.project_id);
  const tenant = s.tenants.find((t) => t.id === r.tenant_id);

  return (
    <>
      <nav className="crumbs" aria-label="Você está em">
        <Link to="/servidores">Servidores</Link><IconChevronRight /><span>{r.alias}</span>
      </nav>
      <div className="detail-head">
        <div className="detail-icon" aria-hidden="true"><IconMonitor /></div>
        <div className="detail-title">
          <h1>{r.alias}</h1>
          <div style={{ marginTop: 4 }}><StatusText r={r} /></div>
          <DetailActions r={r} onDeleted={() => nav("/servidores")} />
        </div>
      </div>

      <div className="tabs" role="tablist">
        <button role="tab" aria-selected={tab === "info"} onClick={() => setTab("info")}>Visão geral</button>
        <button role="tab" aria-selected={tab === "history"} onClick={() => setTab("history")}>Histórico de ações</button>
      </div>

      {tab === "info" ? (
        <div className="grid-2">
          <section className="card">
            <div className="card-head">Informações básicas</div>
            <div className="card-body">
              <dl className="kv">
                <dt>Estado</dt><dd><StatusText r={r} /></dd>
                <dt>Tipo</dt><dd>{r.kind === "lxc" ? "Contêiner (LXC)" : "Máquina virtual (KVM)"}</dd>
                <dt>vCPU</dt><dd>{r.cpus}</dd>
                <dt>Memória</dt><dd>{bytes(r.max_mem_bytes)}</dd>
                <dt>Disco</dt><dd>{bytes(r.max_disk_bytes)}</dd>
                <dt>Atualizado</dt><dd>{when(r.last_seen_at)}</dd>
                <dt>ID</dt><dd className="muted small">{r.id}</dd>
              </dl>
            </div>
          </section>
          <div className="stack">
            {vdc && (
              <section className="card">
                <div className="card-head">Cota do VDC {vdc.name}</div>
                <div className="card-body" style={{ paddingTop: 12 }}>
                  <div className="meters">
                    <Meter label="vCPU ligadas" limit={vdc.cpu_cores} used={vdc.usage?.running_cpu ?? 0} />
                    <Meter label="Memória ligada" limit={Math.round(vdc.memory_mb / 1024)} used={Math.round((vdc.usage?.running_memory_mb ?? 0) / 1024)} fmt={gbText} />
                  </div>
                  <p className="meter-note" style={{ marginTop: 10 }}>
                    Disco: {vdc.storage.length ? vdc.storage.map((x) => `${x.class_name} ${gbText(x.used_gb)} de ${gbText(x.capacity_gb)}`).join(", ") : "sem cota"}
                  </p>
                </div>
              </section>
            )}
            <section className="card">
              <div className="card-head">Objetos relacionados</div>
              <div className="card-body">
                <dl className="kv">
                  {s.multiTenant && (
                    <>
                      <dt>Cliente</dt>
                      <dd>{tenant && s.canAdmin ? <Link to={`/clientes/${tenant.id}`}>{tenant.name}</Link> : r.tenant_name ?? "Sem cliente"}</dd>
                    </>
                  )}
                  <dt>VDC</dt><dd>{vdc ? <Link to="/vdcs">{vdc.name}</Link> : r.project_name ?? "Sem VDC"}</dd>
                  <dt>Localização</dt><dd>{r.location || vdc?.location || "—"}</dd>
                </dl>
              </div>
            </section>
          </div>
        </div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead><tr><th>Ação</th><th>Estado</th><th>Solicitado por</th><th>Início</th><th>Duração</th></tr></thead>
            <tbody>
              {tasks.data?.map((t) => (
                <tr key={t.id}>
                  <td>{actionLabel[t.action].verb}</td>
                  <td><TaskStatus t={t} /></td>
                  <td>{t.requested_by}</td>
                  <td className="num">{dateTime(t.created_at)}</td>
                  <td className="num">{duration(t.started_at ?? t.created_at, t.finished_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {tasks.data && tasks.data.length === 0 && <div className="empty"><strong>Nenhuma ação ainda</strong>As ações feitas pelo portal neste servidor aparecem aqui.</div>}
        </div>
      )}
    </>
  );
}

export function TaskStatus({ t }: { t: Task }) {
  const map = { queued: ["busy", "Na fila"], running: ["busy", "Em andamento"], succeeded: ["ok", "Concluída"], failed: ["err", "Falhou"] } as const;
  const [dot, text] = map[t.status];
  return (
    <span>
      <span className="status"><span className={`dot ${dot}`} />{text}</span>
      {t.status === "failed" && t.error && <span className="state-fail">{t.error}</span>}
    </span>
  );
}
