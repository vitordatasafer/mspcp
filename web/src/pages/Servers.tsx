import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { api, bytes, fold, type Resource, type TaskView } from "../api";
import { useSession } from "../session";
import { RowActions, StatusText } from "../components/Power";
import { NewVMModal } from "../components/VMForms";
import { IconPlus, IconRefresh, IconSearch } from "../components/Icons";

type Filter = "all" | "running" | "stopped";
const PAGE = 20;

export function ServersPage() {
  const s = useSession();
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const [filter, setFilter] = useState<Filter>("all");
  const [page, setPage] = useState(0);
  const [creating, setCreating] = useState(false);
  const showActions = s.canPower || s.canCreateVM || s.canAssign;
  // Criações em andamento aparecem na lista antes de a VM existir.
  const tasks = useQuery({
    queryKey: ["tasks", "recent"],
    queryFn: () => api<{ tasks: TaskView[] }>("/v1/tasks?limit=50").then((r) => r.tasks),
    refetchInterval: (q) => (q.state.data?.some((t) => t.status === "queued" || t.status === "running") ? 3000 : 15000),
  });
  const building = (tasks.data ?? []).filter((t) => t.action === "create" && (t.status === "queued" || t.status === "running") && !t.resource_id);

  const resources = useQuery({
    queryKey: ["resources"],
    queryFn: () => api<{ resources: Resource[] }>("/v1/resources").then((r) => r.resources),
    refetchInterval: building.length > 0 ? 3000 : 10_000,
  });

  const list = resources.data ?? [];
  const visible = useMemo(() => {
    const term = fold(q.trim());
    return list.filter((r) => {
      if (filter === "running" && r.status !== "running") return false;
      if (filter === "stopped" && r.status === "running") return false;
      if (!term) return true;
      return [r.alias, r.tenant_name, r.project_name].some((x) => fold(x).includes(term));
    });
  }, [list, q, filter]);
  const pages = Math.max(1, Math.ceil(visible.length / PAGE));
  const cur = Math.min(page, pages - 1);
  const rows = visible.slice(cur * PAGE, cur * PAGE + PAGE);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Servidores</h1>
          <p>{s.multiTenant ? "Máquinas virtuais e contêineres de todos os clientes que você atende." : "Suas máquinas virtuais e contêineres."}</p>
        </div>
      </div>

      <div className="toolbar">
        {s.canCreateVM && <button className="btn btn-primary" onClick={() => setCreating(true)}><IconPlus />Nova VM</button>}
        <button className="btn btn-icon" aria-label="Atualizar" title="Atualizar" onClick={() => qc.invalidateQueries({ queryKey: ["resources"] })}>
          <IconRefresh />
        </button>
        <div className="seg" role="group" aria-label="Filtrar por estado">
          {([["all", "Todos"], ["running", "Ligados"], ["stopped", "Desligados"]] as const).map(([v, label]) => (
            <button key={v} aria-pressed={filter === v} onClick={() => { setFilter(v); setPage(0); }}>{label}</button>
          ))}
        </div>
        <div className="input-icon">
          <IconSearch />
          <label className="visually-hidden" htmlFor="busca">Filtrar servidores</label>
          <input
            id="busca"
            className="input"
            type="search"
            placeholder={s.multiTenant ? "Nome, cliente ou VDC" : "Nome ou VDC"}
            value={q}
            onChange={(e) => { setParams(e.target.value ? { q: e.target.value } : {}, { replace: true }); setPage(0); }}
          />
        </div>
      </div>

      {resources.error && <div className="alert" style={{ marginBottom: 12 }}>Não foi possível carregar os servidores: {resources.error.message}</div>}

      <div className="table-wrap">
        <table className="table">
          <thead>
            <tr>
              <th>Nome</th>
              <th>Estado</th>
              {s.multiTenant && <th>Cliente</th>}
              <th>VDC</th>
              <th>Tipo</th>
              <th className="num">vCPU</th>
              <th className="num">Memória</th>
              <th className="num">Disco</th>
              {showActions && <th className="actions">Ações</th>}
            </tr>
          </thead>
          <tbody>
            {resources.isPending && <tr><td colSpan={9} className="muted">Carregando servidores…</td></tr>}
            {cur === 0 && building.map((t) => (
              <tr key={t.id} className="ghost">
                <td>{t.resource_alias}</td>
                <td><span className="status" role="status"><span className="dot busy" />{t.status === "queued" ? "Na fila para criar" : "Criando…"}</span></td>
                {s.multiTenant && <td>{t.tenant_name ?? ""}</td>}
                <td colSpan={showActions ? 6 : 5} className="muted small">Clonando a imagem e aplicando a configuração</td>
              </tr>
            ))}
            {rows.map((r) => (
              <tr key={r.id}>
                <td><Link to={`/servidores/${r.id}`}>{r.alias}</Link></td>
                <td><StatusText r={r} /></td>
                {s.multiTenant && <td>{r.tenant_name ?? <span className="muted">Sem cliente</span>}</td>}
                <td>{r.project_name ?? <span className="muted">Sem VDC</span>}</td>
                <td className="kind">{r.kind === "lxc" ? "Contêiner" : "VM"}</td>
                <td className="num">{r.cpus}</td>
                <td className="num">{bytes(r.max_mem_bytes)}</td>
                <td className="num">{bytes(r.max_disk_bytes)}</td>
                {showActions && <td className="actions"><RowActions r={r} /></td>}
              </tr>
            ))}
          </tbody>
        </table>
        {resources.data && list.length === 0 && building.length === 0 && (
          <div className="empty">
            <strong>Nenhum servidor por aqui ainda</strong>
            {s.isStaff
              ? "Cadastre um cluster e crie VDCs; depois crie VMs pelas imagens ou atribua as que já existem."
              : s.canCreateVM ? "Crie a primeira VM com o botão Nova VM." : "Quando um servidor for criado para a sua conta, ele aparece nesta lista."}
          </div>
        )}
        {resources.data && list.length > 0 && visible.length === 0 && (
          <div className="empty"><strong>Nenhum servidor com esse filtro</strong>Tente outro termo ou mostre todos.</div>
        )}
        {visible.length > 0 && (
          <div className="table-foot">
            <span>Itens {cur * PAGE + 1} a {cur * PAGE + rows.length}. Total: {visible.length}</span>
            {pages > 1 && (
              <span style={{ display: "flex", gap: 6, alignItems: "center" }}>
                <button className="btn btn-sm" disabled={cur === 0} onClick={() => setPage(cur - 1)} aria-label="Página anterior">‹</button>
                <span className="num">{cur + 1} de {pages}</span>
                <button className="btn btn-sm" disabled={cur >= pages - 1} onClick={() => setPage(cur + 1)} aria-label="Próxima página">›</button>
              </span>
            )}
          </div>
        )}
      </div>
      {creating && <NewVMModal onClose={() => setCreating(false)} />}
    </>
  );
}
