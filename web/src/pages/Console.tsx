import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import * as rfbModule from "@novnc/novnc/lib/rfb";
import type RFBType from "@novnc/novnc/lib/rfb";

// O noVNC 1.4 é CommonJS; dependendo do empacotador, a classe vem em
// .default ou em .default.default.
type RFBCtor = typeof RFBType;
const mod = rfbModule as unknown as { default?: RFBCtor & { default?: RFBCtor } };
const RFB: RFBCtor = (mod.default?.default ?? mod.default ?? (rfbModule as unknown as RFBCtor));
type RFB = RFBType;
import { api, type Resource } from "../api";

type State = "connecting" | "connected" | "closed" | "error";

// Console da VM no navegador (noVNC). O portal pede um ticket de uso único
// e conecta no websocket da própria API, que repassa para o Proxmox.
export function ConsolePage() {
  const { id } = useParams();
  const screen = useRef<HTMLDivElement>(null);
  const rfb = useRef<RFB | null>(null);
  const [state, setState] = useState<State>("connecting");
  const [message, setMessage] = useState("");
  const [attempt, setAttempt] = useState(0);
  const res = useQuery({ queryKey: ["resources", id], queryFn: () => api<Resource>(`/v1/resources/${id}`), retry: false });

  useEffect(() => {
    if (res.data) document.title = `Console de ${res.data.alias}`;
    return () => { document.title = "MSP Cloud"; };
  }, [res.data]);

  useEffect(() => {
    let cancelled = false;
    setState("connecting");
    setMessage("");
    (async () => {
      try {
        const t = await api<{ path: string; password: string }>(`/v1/resources/${id}/console`, { method: "POST" });
        if (cancelled || !screen.current) return;
        const url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}${t.path}`;
        const r = new RFB(screen.current, url, { credentials: { password: t.password }, wsProtocols: ["binary"] });
        r.scaleViewport = true;
        r.resizeSession = false;
        r.background = "#0b0f15";
        r.addEventListener("connect", () => { setState("connected"); r.focus(); });
        r.addEventListener("disconnect", (e) => {
          const clean = (e as CustomEvent<{ clean: boolean }>).detail?.clean;
          setState(clean ? "closed" : "error");
          setMessage(clean ? "A sessão do console foi encerrada." : "A conexão com o console caiu.");
        });
        r.addEventListener("credentialsrequired", () => r.sendCredentials({ password: t.password }));
        r.addEventListener("securityfailure", (e) => {
          setState("error");
          setMessage((e as CustomEvent<{ reason?: string }>).detail?.reason || "O console recusou a autenticação.");
        });
        rfb.current = r;
      } catch (err) {
        if (!cancelled) {
          setState("error");
          setMessage((err as Error).message);
        }
      }
    })();
    return () => {
      cancelled = true;
      rfb.current?.disconnect();
      rfb.current = null;
    };
  }, [id, attempt]);

  const fullscreen = useCallback(() => {
    const el = screen.current?.parentElement;
    if (!document.fullscreenElement) el?.requestFullscreen?.();
    else document.exitFullscreen?.();
  }, []);

  const label = { connecting: "Conectando…", connected: "Conectado", closed: "Desconectado", error: "Sem conexão" }[state];
  return (
    <div className="console">
      <header className="console-bar">
        <Link to={`/servidores/${id}`} className="console-back">‹ {res.data?.alias ?? "Servidor"}</Link>
        <span className="status">
          <span className={`dot ${state === "connected" ? "ok" : state === "connecting" ? "busy" : "err"}`} />{label}
        </span>
        <span className="spacer" />
        <button className="btn btn-sm" disabled={state !== "connected"} onClick={() => rfb.current?.sendCtrlAltDel()}>Ctrl+Alt+Del</button>
        <button className="btn btn-sm" onClick={fullscreen}>Tela cheia</button>
        {state !== "connected" && state !== "connecting" && (
          <button className="btn btn-sm btn-primary" onClick={() => setAttempt((n) => n + 1)}>Reconectar</button>
        )}
      </header>
      <div className="console-stage">
        <div ref={screen} className="console-screen" aria-label="Tela do servidor" />
        {state !== "connected" && (
          <div className="console-overlay" role="status">
            {state === "connecting" ? "Abrindo o console…" : message}
          </div>
        )}
      </div>
    </div>
  );
}
