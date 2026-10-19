import { UserManager, WebStorageStateStore, type User } from "oidc-client-ts";

// O portal lê /config.json para saber como entrar: login próprio (usuário,
// senha e 2FA, com sessão em cookie) ou OIDC (Keycloak). O mesmo build serve
// os dois modos.
export interface PortalConfig {
  auth: "local" | "oidc";
  product_name: string;
  demo_hint?: string;
  oidc_issuer?: string;
  oidc_client_id?: string;
}

let config: Promise<PortalConfig> | null = null;

export function getConfig(): Promise<PortalConfig> {
  if (!config) {
    config = fetch("/config.json").then((r) => {
      if (!r.ok) throw new Error("Não foi possível carregar a configuração do portal.");
      return r.json() as Promise<PortalConfig>;
    });
  }
  return config;
}

// ---------- OIDC ----------

let manager: Promise<UserManager> | null = null;

export function getManager(): Promise<UserManager> {
  if (!manager) {
    manager = getConfig().then(
      (cfg) =>
        new UserManager({
          authority: cfg.oidc_issuer!,
          client_id: cfg.oidc_client_id!,
          redirect_uri: `${location.origin}/callback`,
          post_logout_redirect_uri: `${location.origin}/`,
          response_type: "code", // authorization code + PKCE
          scope: "openid profile email",
          userStore: new WebStorageStateStore({ store: window.sessionStorage }),
          automaticSilentRenew: false,
          loadUserInfo: false,
        }),
    );
  }
  return manager;
}

export async function oidcUser(): Promise<User | null> {
  const u = await (await getManager()).getUser();
  return u && !u.expired ? u : null;
}

// ---------- entrar e sair ----------

function here() {
  return location.pathname + location.search;
}

// login leva para a tela de entrada, guardando para onde voltar.
export async function login(returnTo = here()) {
  const cfg = await getConfig();
  if (cfg.auth === "oidc") {
    await (await getManager()).signinRedirect({ state: { returnTo } });
    return;
  }
  if (location.pathname === "/login") return;
  const next = returnTo && returnTo !== "/" && !returnTo.startsWith("/login") ? `?next=${encodeURIComponent(returnTo)}` : "";
  location.assign(`/login${next}`);
}

export async function logout() {
  const cfg = await getConfig();
  if (cfg.auth === "oidc") {
    const m = await getManager();
    const u = await m.getUser();
    await m.removeUser();
    await m.signoutRedirect({ id_token_hint: u?.id_token });
    return;
  }
  await fetch("/auth/logout", { method: "POST", headers: { "X-MSPCP": "1" }, credentials: "same-origin" });
  location.assign("/login");
}

export interface LoginResult {
  ok?: boolean;
  need_totp?: boolean;
  must_change_password?: boolean;
}

export async function localLogin(username: string, password: string, totp?: string): Promise<LoginResult> {
  const res = await fetch("/auth/login", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-MSPCP": "1" },
    body: JSON.stringify({ username, password, totp: totp || "" }),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Erro ${res.status}`);
  return data as LoginResult;
}
