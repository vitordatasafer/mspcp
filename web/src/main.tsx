import { lazy, StrictMode, Suspense, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes, useNavigate } from "react-router-dom";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import "./styles.css";

import { api, ApiError, type Me, type Tenant } from "./api";
import { getConfig, getManager, login, logout, oidcUser } from "./auth";
import { buildSession, SessionContext } from "./session";
import { Layout } from "./components/Layout";
import { ServersPage } from "./pages/Servers";
import { TenantsPage } from "./pages/Tenants";
import { TenantDetailPage } from "./pages/TenantDetail";
import { ClustersPage, StoragePage } from "./pages/Clusters";
import { ClusterDetailPage } from "./pages/ClusterDetail";
import { DashboardPage } from "./pages/Dashboard";
import { ServerDetailPage } from "./pages/ServerDetail";
import { VDCsPage } from "./pages/VDCs";
import { TasksPage } from "./pages/Tasks";
import { LoginPage } from "./pages/Login";
import { AccountPage, ForcedPasswordChange } from "./pages/Account";
import { ImagesPage } from "./pages/Images";
// O noVNC só carrega quando alguém abre um console.
const ConsolePage = lazy(() => import("./pages/Console").then((m) => ({ default: m.ConsolePage })));

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (n, err) => !(err instanceof ApiError && err.status < 500) && n < 2,
      refetchOnWindowFocus: true,
    },
  },
});

function Callback() {
  const nav = useNavigate();
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    getManager()
      .then((m) => m.signinRedirectCallback())
      .then((u) => {
        const to = (u.state as { returnTo?: string } | undefined)?.returnTo;
        nav(to && to.startsWith("/") && !to.startsWith("/callback") ? to : "/", { replace: true });
      })
      .catch((e: Error) => setError(e.message));
  }, [nav]);
  return (
    <div className="center-screen">
      {error ? (
        <div className="panel">
          <h2>Não foi possível concluir o login</h2>
          <p className="muted" style={{ margin: "8px 0 16px" }}>{error}</p>
          <button className="btn btn-primary" onClick={() => login("/")}>Entrar de novo</button>
        </div>
      ) : (
        <p className="muted">Concluindo login…</p>
      )}
    </div>
  );
}

function Authenticated({ children }: { children: React.ReactNode }) {
  const cfg = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  // OIDC: sem token, vai para o Keycloak. Login local: a própria API diz (401).
  const [oidcName, setOidcName] = useState<string | null>(null);
  const oidc = cfg.data?.auth === "oidc";
  useEffect(() => {
    if (!oidc) return;
    oidcUser().then((u) => {
      if (!u) login();
      else setOidcName((u.profile.name as string) || u.profile.preferred_username || u.profile.sub);
    });
  }, [oidc]);
  const ready = !!cfg.data && (!oidc || !!oidcName);

  const me = useQuery({ queryKey: ["me"], queryFn: () => api<Me>("/v1/me"), enabled: ready, retry: false });
  const mustChange = !!me.data?.must_change_password;
  const hasAccess = (me.data?.memberships.length ?? 0) > 0;
  const tenants = useQuery({
    queryKey: ["tenants"],
    queryFn: () => api<{ tenants: Tenant[] }>("/v1/tenants").then((r) => r.tenants),
    enabled: !!me.data && !mustChange && hasAccess,
  });

  if (cfg.error || me.error) {
    const err = (cfg.error ?? me.error)!;
    if (err instanceof ApiError && err.status === 401) {
      return <div className="center-screen"><p className="muted">Abrindo a tela de entrada…</p></div>;
    }
    return (
      <div className="center-screen">
        <div className="panel">
          <h2>Não foi possível abrir o portal</h2>
          <p className="muted" style={{ margin: "8px 0 16px" }}>{err.message}</p>
          <button className="btn" onClick={() => location.reload()}>Tentar de novo</button>
        </div>
      </div>
    );
  }
  if (!ready || !me.data) {
    return <div className="center-screen"><p className="muted">Carregando…</p></div>;
  }
  if (mustChange) return <ForcedPasswordChange me={me.data} />;
  if (!hasAccess) {
    return (
      <div className="center-screen">
        <div className="panel">
          <h2>Sua conta ainda não tem acesso</h2>
          <p className="muted" style={{ margin: "8px 0 16px" }}>
            Peça ao administrador da sua empresa para liberar seu usuário em um cliente ou VDC.
          </p>
          <button className="btn" onClick={() => logout()}>Sair</button>
        </div>
      </div>
    );
  }
  if (tenants.isPending) return <div className="center-screen"><p className="muted">Carregando…</p></div>;
  const session = buildSession(me.data, me.data.name || oidcName || me.data.subject, tenants.data ?? []);
  return <SessionContext.Provider value={session}>{children}</SessionContext.Provider>;
}

function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/callback" element={<Callback />} />
      <Route
        path="/servidores/:id/console"
        element={<Authenticated><Suspense fallback={<div className="center-screen"><p className="muted">Abrindo o console…</p></div>}><ConsolePage /></Suspense></Authenticated>}
      />
      <Route
        element={
          <Authenticated>
            <Layout />
          </Authenticated>
        }
      >
        <Route index element={<DashboardPage />} />
        <Route path="servidores" element={<ServersPage />} />
        <Route path="servidores/:id" element={<ServerDetailPage />} />
        <Route path="vdcs" element={<VDCsPage />} />
        <Route path="clientes" element={<TenantsPage />} />
        <Route path="clientes/:id" element={<TenantDetailPage />} />
        <Route path="clusters" element={<ClustersPage />} />
        <Route path="clusters/:id" element={<ClusterDetailPage />} />
        <Route path="storage" element={<StoragePage />} />
        <Route path="imagens" element={<ImagesPage />} />
        <Route path="tarefas" element={<TasksPage />} />
        <Route path="conta" element={<AccountPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
