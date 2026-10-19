import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { actionLabel, api, ApiError, type Action, type Resource, type Task } from "../api";
import { useSession } from "../session";
import { Modal } from "./Modal";
import { AssignVMModal, DeleteVMModal } from "./VMForms";
import { IconChevron, IconMonitor, IconMove, IconPower, IconRestart, IconStop, IconTrash } from "./Icons";

// Acompanha as ações disparadas nesta aba (servidor -> task em curso) e o
// último resultado. Fica no Layout, então lista, detalhe e rodapé enxergam
// o mesmo estado.
interface PowerState {
  pending: Record<string, { taskId: string; action: Action }>;
  results: Record<string, { ok: boolean; text: string }>;
  run: (r: Resource, action: Action) => Promise<void>;
}

const PowerContext = createContext<PowerState | null>(null);

export function usePower() {
  const p = useContext(PowerContext);
  if (!p) throw new Error("usePower fora do PowerProvider");
  return p;
}

export function PowerProvider({ children }: { children: React.ReactNode }) {
  const qc = useQueryClient();
  const [pending, setPending] = useState<PowerState["pending"]>({});
  const [results, setResults] = useState<PowerState["results"]>({});

  const run = useCallback(async (r: Resource, action: Action) => {
    setResults((x) => {
      const { [r.id]: _, ...rest } = x;
      return rest;
    });
    try {
      const task = await api<Task>(`/v1/resources/${r.id}/actions`, { method: "POST", json: { action } });
      setPending((x) => ({ ...x, [r.id]: { taskId: task.id, action } }));
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (e) {
      const text = e instanceof ApiError && e.status === 409
        ? "Já existe uma ação em andamento neste servidor."
        : (e as Error).message;
      setResults((x) => ({ ...x, [r.id]: { ok: false, text } }));
    }
  }, [qc]);

  const finish = useCallback((resourceId: string, t: Task) => {
    setPending((x) => {
      const { [resourceId]: _, ...rest } = x;
      return rest;
    });
    setResults((x) => ({
      ...x,
      [resourceId]: t.status === "succeeded"
        ? { ok: true, text: actionLabel[t.action].done }
        : { ok: false, text: t.error ? `Falhou: ${t.error}` : "A ação falhou." },
    }));
    qc.invalidateQueries({ queryKey: ["resources"] });
    qc.invalidateQueries({ queryKey: ["projects"] });
    qc.invalidateQueries({ queryKey: ["tasks"] });
  }, [qc]);

  return (
    <PowerContext.Provider value={{ pending, results, run }}>
      {Object.entries(pending).map(([rid, p]) => (
        <TaskWatcher key={p.taskId} resourceId={rid} taskId={p.taskId} onDone={finish} />
      ))}
      {children}
    </PowerContext.Provider>
  );
}

function TaskWatcher({ resourceId, taskId, onDone }: { resourceId: string; taskId: string; onDone: (rid: string, t: Task) => void }) {
  const q = useQuery({
    queryKey: ["task", taskId],
    queryFn: () => api<Task>(`/v1/tasks/${taskId}`),
    refetchInterval: (query) => {
      const s = query.state.data?.status;
      return s === "succeeded" || s === "failed" ? false : 1000;
    },
  });
  useEffect(() => {
    if (q.data && (q.data.status === "succeeded" || q.data.status === "failed")) onDone(resourceId, q.data);
  }, [q.data, resourceId, onDone]);
  return null;
}

// ---------- Estado ----------

export function statusOf(r: Resource) {
  if (r.status === "running") return { dot: "ok", text: "Ligado" };
  if (r.status === "stopped") return { dot: "err", text: "Desligado" };
  return { dot: "", text: r.status };
}

export function StatusText({ r }: { r: Resource }) {
  const { pending, results } = usePower();
  const p = pending[r.id];
  const res = results[r.id];
  if (r.deleting) {
    return <span className="status" role="status"><span className="dot busy" />Excluindo…</span>;
  }
  if (p) {
    return <span className="status" role="status"><span className="dot busy" />{actionLabel[p.action].doing}…</span>;
  }
  const st = statusOf(r);
  return (
    <span>
      <span className="status"><span className={`dot ${st.dot}`} />{st.text}</span>
      {res && !res.ok && <span className="state-fail" role="status">{res.text}</span>}
    </span>
  );
}

// ---------- Ações ----------

const confirmText: Partial<Record<Action, string>> = {
  shutdown: "O sistema operacional recebe o pedido de desligamento e encerra os serviços.",
  reboot: "O servidor reinicia e fica indisponível por alguns instantes.",
  stop: "Desliga na hora, como tirar da tomada. Dados não salvos podem se perder.",
};

function availableActions(r: Resource): Action[] {
  return r.status === "running" ? ["shutdown", "reboot", "stop"] : ["start"];
}

function openConsole(r: Resource) {
  window.open(`/servidores/${r.id}/console`, `console-${r.id}`, "noopener");
}

// Excluir e atribuir: diálogos compartilhados por lista e detalhe.
function useLifecycle(r: Resource, onDeleted?: () => void) {
  const [dialog, setDialog] = useState<"delete" | "assign" | null>(null);
  const node =
    dialog === "delete" ? <DeleteVMModal r={r} onClose={() => setDialog(null)} onDone={onDeleted} />
    : dialog === "assign" ? <AssignVMModal r={r} onClose={() => setDialog(null)} />
    : null;
  return { open: setDialog, node };
}

function useConfirm(r: Resource) {
  const { run } = usePower();
  const [confirming, setConfirming] = useState<Action | null>(null);
  const ask = (a: Action) => (confirmText[a] ? setConfirming(a) : run(r, a));
  const dialog = confirming && (
    <Modal
      title={`${actionLabel[confirming].verb} ${r.alias}?`}
      onClose={() => setConfirming(null)}
      footer={
        <>
          <button className="btn" onClick={() => setConfirming(null)}>Cancelar</button>
          <button
            className={`btn ${confirming === "stop" ? "btn-danger-solid" : "btn-primary"}`}
            onClick={() => { const a = confirming; setConfirming(null); run(r, a); }}
          >
            {actionLabel[confirming].verb}
          </button>
        </>
      }
    >
      <p>{confirmText[confirming]}</p>
    </Modal>
  );
  return { ask, dialog };
}

// Menu "..." da linha da tabela.
export function RowActions({ r }: { r: Resource }) {
  const s = useSession();
  const { pending } = usePower();
  const { ask, dialog } = useConfirm(r);
  const life = useLifecycle(r);
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);
  if (!s.canPower && !s.canCreateVM && !s.canAssign) return null;
  const busy = !!pending[r.id] || !!r.deleting;
  const on = r.status === "running";
  const pick = (f: () => void) => () => { setOpen(false); f(); };
  return (
    <div className="dropdown" ref={ref}>
      <button
        className="btn btn-more"
        aria-label={`Ações de ${r.alias}`}
        aria-haspopup="menu"
        aria-expanded={open}
        disabled={busy}
        onClick={() => setOpen(!open)}
      >
        •••
      </button>
      {open && (
        <div className="menu" role="menu">
          {s.canPower && availableActions(r).map((a) => (
            <button key={a} role="menuitem" className={a === "stop" ? "danger" : ""} onClick={pick(() => ask(a))}>
              {a === "start" ? <IconPower /> : a === "reboot" ? <IconRestart /> : <IconStop />}
              {actionLabel[a].verb}
            </button>
          ))}
          {s.canConsole && (
            <button role="menuitem" disabled={!on} onClick={pick(() => openConsole(r))}><IconMonitor />Abrir console</button>
          )}
          {(s.canAssign || s.canCreateVM) && <div className="menu-sep" role="separator" />}
          {s.canAssign && <button role="menuitem" onClick={pick(() => life.open("assign"))}><IconMove />Atribuir a VDC</button>}
          {s.canCreateVM && <button role="menuitem" className="danger" onClick={pick(() => life.open("delete"))}><IconTrash />Excluir</button>}
        </div>
      )}
      {dialog}
      {life.node}
    </div>
  );
}

// Botões do topo da página de detalhe.
export function DetailActions({ r, onDeleted }: { r: Resource; onDeleted?: () => void }) {
  const s = useSession();
  const { pending } = usePower();
  const { ask, dialog } = useConfirm(r);
  const life = useLifecycle(r, onDeleted);
  const [more, setMore] = useState(false);
  if (!s.canPower && !s.canCreateVM && !s.canAssign) return null;
  const busy = !!pending[r.id] || !!r.deleting;
  const on = r.status === "running";
  return (
    <div className="detail-actions">
      {s.canPower && (
        <>
          <button className="btn" disabled={busy || on} onClick={() => ask("start")}><IconPower />Ligar</button>
          <button className="btn" disabled={busy || !on} onClick={() => ask("shutdown")}><IconStop />Desligar</button>
          <button className="btn" disabled={busy || !on} onClick={() => ask("reboot")}><IconRestart />Reiniciar</button>
        </>
      )}
      {s.canConsole && (
        <button className="btn" disabled={busy || !on} onClick={() => openConsole(r)} title={on ? undefined : "Ligue o servidor para abrir o console"}>
          <IconMonitor />Console
        </button>
      )}
      <div className="dropdown left">
        <button className="btn" disabled={busy} aria-haspopup="menu" aria-expanded={more} onClick={() => setMore(!more)}>
          Mais ações <IconChevron />
        </button>
        {more && (
          <div className="menu" role="menu" onMouseLeave={() => setMore(false)}>
            {s.canPower && (
              <button role="menuitem" className="danger" disabled={!on} onClick={() => { setMore(false); ask("stop"); }}>
                <IconStop />Forçar desligamento
              </button>
            )}
            {s.canAssign && <button role="menuitem" onClick={() => { setMore(false); life.open("assign"); }}><IconMove />Atribuir a VDC</button>}
            {s.canCreateVM && <button role="menuitem" className="danger" onClick={() => { setMore(false); life.open("delete"); }}><IconTrash />Excluir servidor</button>}
          </div>
        )}
      </div>
      {dialog}
      {life.node}
    </div>
  );
}
