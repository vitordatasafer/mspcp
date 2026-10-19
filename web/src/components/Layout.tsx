import { useEffect, useRef, useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { logout } from "../auth";
import { api, initials, kindLabel, roleLabel, type TaskView } from "../api";
import { useSession, type Session } from "../session";
import { PowerProvider } from "./Power";
import {
  IconCluster, IconCollapse, IconDisk, IconExpand, IconHome, IconImage, IconLayers, IconLoader, IconLogout, IconSearch,
  IconServer, IconTasks, IconUser, IconUsers,
} from "./Icons";

// Módulos do trilho de ícones e os itens de cada um no menu lateral.
interface Item { to: string; label: string; end?: boolean }
interface Module { key: string; label: string; icon: React.ReactNode; to: string; match: string[]; groups: { title: string; items: Item[] }[] }

function modules(s: Session): Module[] {
  const accountTo = s.isStaff || !s.home ? "/clientes" : `/clientes/${s.home.id}`;
  const accountLabel = s.isStaff ? "MSPs e clientes" : s.home?.kind === "partner" ? "Cota e clientes" : "Conta e usuários";
  const list: Module[] = [
    { key: "home", label: "Visão geral", icon: <IconHome />, to: "/", match: ["/"], groups: [] },
    {
      key: "res", label: "Recursos", icon: <IconServer />, to: "/servidores", match: ["/servidores", "/vdcs"],
      groups: [
        { title: "Computação", items: [{ to: "/servidores", label: "Servidores" }] },
        { title: "Cota", items: [{ to: "/vdcs", label: "VDCs" }] },
      ],
    },
  ];
  if (s.canAdmin) {
    list.push({
      key: "acc", label: "Contas", icon: <IconUsers />, to: accountTo, match: ["/clientes"],
      groups: [{ title: "Contas", items: [{ to: accountTo, label: accountLabel, end: !s.isStaff }] }],
    });
  }
  if (s.canClusters) {
    list.push({
      key: "infra", label: "Infraestrutura", icon: <IconCluster />, to: "/clusters", match: ["/clusters", "/storage", "/imagens"],
      groups: [{ title: "Infraestrutura", items: [{ to: "/clusters", label: "Clusters" }, { to: "/storage", label: "Storage" }, { to: "/imagens", label: "Imagens" }] }],
    });
  }
  list.push({
    key: "ops", label: "Operação", icon: <IconTasks />, to: "/tarefas", match: ["/tarefas"],
    groups: [{ title: "Tarefas e eventos", items: [{ to: "/tarefas", label: "Tarefas" }] }],
  });
  list.push({ key: "me", label: "Minha conta", icon: <IconUser />, to: "/conta", match: ["/conta"], groups: [] });
  return list;
}

function current(mods: Module[], path: string) {
  return mods.find((m) => m.key !== "home" && m.match.some((p) => path === p || path.startsWith(p + "/"))) ?? mods[0];
}

const NAV_KEY = "mspcp.subnav.collapsed";

export function Layout() {
  const s = useSession();
  const loc = useLocation();
  const mods = modules(s);
  const mod = current(mods, loc.pathname);
  const [collapsed, setCollapsed] = useState(() => {
    try { return localStorage.getItem(NAV_KEY) === "1"; } catch { return false; }
  });
  const toggle = () => {
    setCollapsed((c) => {
      try { localStorage.setItem(NAV_KEY, c ? "0" : "1"); } catch { /* armazenamento indisponível */ }
      return !c;
    });
  };

  return (
    <PowerProvider>
      <div className="app">
        <TopBar />
        <nav className="rail" aria-label="Módulos">
          <button className="toggle" onClick={toggle} aria-label={collapsed ? "Mostrar menu" : "Recolher menu"} title={collapsed ? "Mostrar menu" : "Recolher menu"}>
            {collapsed ? <IconCollapse /> : <IconExpand />}
          </button>
          <span className="sep" />
          {mods.map((m) => (
            <NavLink key={m.key} to={m.to} end={m.key === "home"} className={() => (m.key === mod.key ? "active" : "")} title={m.label} aria-label={m.label}>
              {m.icon}
            </NavLink>
          ))}
        </nav>
        <nav className={`subnav${collapsed || mod.groups.length === 0 ? " hidden" : ""}`} aria-label={mod.label}>
          <div className="subnav-title">{mod.label}</div>
          {mod.groups.map((g) => (
            <div key={g.title}>
              <div className="subnav-group">{g.title}</div>
              {g.items.map((it) => (
                <NavLink key={it.to} to={it.to} end={it.end}>
                  {it.to === "/vdcs" ? <IconLayers /> : it.to === "/servidores" ? <IconServer /> : it.to === "/tarefas" ? <IconTasks /> : it.to.startsWith("/clientes") ? <IconUsers /> : it.to === "/storage" ? <IconDisk /> : it.to === "/imagens" ? <IconImage /> : <IconCluster />}
                  {it.label}
                </NavLink>
              ))}
            </div>
          ))}
        </nav>
        <main className="main">
          <div className="page"><Outlet /></div>
        </main>
        <FootBar />
      </div>
    </PowerProvider>
  );
}

function TopBar() {
  const s = useSession();
  const nav = useNavigate();
  const loc = useLocation();
  const [q, setQ] = useState("");
  const [open, setOpen] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        inputRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => !menuRef.current?.contains(e.target as Node) && setOpen(false);
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  useEffect(() => {
    if (!loc.pathname.startsWith("/servidores")) setQ("");
  }, [loc.pathname]);

  const topRole = s.me.memberships[0]?.role;
  return (
    <header className="topbar">
      <Link to="/" className="brand" aria-label="MSP Cloud, visão geral">
        <span className="brand-mark" aria-hidden="true"><span /><span /><span /></span>
        <span className="brand-text">MSP Cloud</span>
      </Link>
      <form
        className="search"
        role="search"
        onSubmit={(e) => { e.preventDefault(); nav(`/servidores${q.trim() ? `?q=${encodeURIComponent(q.trim())}` : ""}`); }}
      >
        <IconSearch />
        <label className="visually-hidden" htmlFor="busca-global">Buscar servidor</label>
        <input id="busca-global" ref={inputRef} value={q} onChange={(e) => setQ(e.target.value)} placeholder="Buscar servidor, cliente ou VDC" />
        <span className="kbd" aria-hidden="true">{isMac ? "⌘K" : "Ctrl K"}</span>
      </form>
      <div className="topbar-right">
        {s.home && <span className="account-name">{s.home.name}</span>}
        <div className="user-menu" ref={menuRef}>
          <button className="user-btn" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
            <span className="avatar" aria-hidden="true">{initials(s.name)}</span>
            <span className="who">{s.name}</span>
          </button>
          {open && (
            <div className="menu" role="menu">
              <div className="menu-head">
                <strong>{s.name}</strong>
                <span>{topRole ? roleLabel[topRole] : ""}{s.home ? `, ${kindLabel[s.home.kind]} ${s.home.name}` : ""}</span>
              </div>
              <Link role="menuitem" to="/conta" onClick={() => setOpen(false)}><IconUser />Minha conta</Link>
              <button role="menuitem" onClick={() => logout()}><IconLogout />Sair</button>
            </div>
          )}
        </div>
      </div>
    </header>
  );
}

// Rodapé com as tarefas: em andamento (azul) e falhas nas últimas 24 h (vermelho).
function FootBar() {
  const tasks = useQuery({
    queryKey: ["tasks", "recent"],
    queryFn: () => api<{ tasks: TaskView[] }>("/v1/tasks?limit=50").then((r) => r.tasks),
    refetchInterval: (q) => (q.state.data?.some((t) => t.status === "queued" || t.status === "running") ? 3000 : 15000),
  });
  const list = tasks.data ?? [];
  const running = list.filter((t) => t.status === "queued" || t.status === "running").length;
  const dayAgo = Date.now() - 86_400_000;
  const failed = list.filter((t) => t.status === "failed" && new Date(t.created_at).getTime() > dayAgo).length;
  const ok = list.filter((t) => t.status === "succeeded" && new Date(t.created_at).getTime() > dayAgo).length;
  return (
    <footer className="footbar">
      <Link to="/tarefas" aria-label={`Tarefas: ${running} em andamento, ${failed} com falha nas últimas 24 horas`}>
        Tarefas
        <span className="badge info">{running > 0 ? <IconLoader /> : null}{running}</span>
        {failed > 0 && <span className="badge err">{failed}</span>}
      </Link>
      <span className="small">{ok} {ok === 1 ? "concluída" : "concluídas"} nas últimas 24 h</span>
    </footer>
  );
}
