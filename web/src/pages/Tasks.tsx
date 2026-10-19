import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { actionLabel, api, dateTime, duration, type TaskView } from "../api";
import { TaskTarget } from "../components/TaskTarget";
import { useSession } from "../session";
import { TaskStatus } from "./ServerDetail";
import { IconRefresh } from "../components/Icons";

type Filter = "all" | "active" | "failed";

export function TasksPage() {
  const s = useSession();
  const qc = useQueryClient();
  const [filter, setFilter] = useState<Filter>("all");
  const tasks = useQuery({
    queryKey: ["tasks", "page"],
    queryFn: () => api<{ tasks: TaskView[] }>("/v1/tasks?limit=200").then((r) => r.tasks),
    refetchInterval: (q) => (q.state.data?.some((t) => t.status === "queued" || t.status === "running") ? 2000 : 10_000),
  });
  const list = (tasks.data ?? []).filter((t) =>
    filter === "all" ? true : filter === "failed" ? t.status === "failed" : t.status === "queued" || t.status === "running",
  );

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Tarefas</h1>
          <p>Ações feitas pelo portal nos servidores que você enxerga, das mais recentes para as mais antigas.</p>
        </div>
      </div>
      <div className="toolbar">
        <button className="btn btn-icon" aria-label="Atualizar" title="Atualizar" onClick={() => qc.invalidateQueries({ queryKey: ["tasks"] })}><IconRefresh /></button>
        <div className="seg" role="group" aria-label="Filtrar tarefas">
          {([["all", "Todas"], ["active", "Em andamento"], ["failed", "Com falha"]] as const).map(([v, label]) => (
            <button key={v} aria-pressed={filter === v} onClick={() => setFilter(v)}>{label}</button>
          ))}
        </div>
      </div>
      <div className="table-wrap">
        <table className="table">
          <thead>
            <tr>
              <th>Ação</th>
              <th>Servidor</th>
              {s.multiTenant && <th>Cliente</th>}
              <th>Estado</th>
              <th>Solicitado por</th>
              <th>Início</th>
              <th>Duração</th>
            </tr>
          </thead>
          <tbody>
            {tasks.isPending && <tr><td colSpan={7} className="muted">Carregando…</td></tr>}
            {list.map((t) => (
              <tr key={t.id}>
                <td>{actionLabel[t.action].verb}</td>
                <td><TaskTarget t={t} /></td>
                {s.multiTenant && <td>{t.tenant_name ?? <span className="muted">Sem cliente</span>}</td>}
                <td><TaskStatus t={t} /></td>
                <td>{t.requested_by}</td>
                <td className="num">{dateTime(t.created_at)}</td>
                <td className="num">{duration(t.started_at ?? t.created_at, t.finished_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {tasks.data && list.length === 0 && (
          <div className="empty"><strong>Nenhuma tarefa {filter === "all" ? "ainda" : "com esse filtro"}</strong>Criar, ligar, desligar, reiniciar ou excluir um servidor cria uma tarefa aqui.</div>
        )}
        {list.length > 0 && <div className="table-foot"><span>Total: {list.length}</span></div>}
      </div>
    </>
  );
}
