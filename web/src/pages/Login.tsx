import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getConfig, localLogin, login } from "../auth";

// Tela de entrada do login próprio: usuário e senha e, se a conta tiver
// verificação em duas etapas, o código do app autenticador.
export function LoginPage() {
  const cfg = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [step, setStep] = useState<"password" | "totp">("password");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const codeRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (cfg.data?.auth === "oidc") login("/");
  }, [cfg.data]);
  useEffect(() => {
    if (step === "totp") codeRef.current?.focus();
  }, [step]);

  const next = () => {
    const n = new URLSearchParams(location.search).get("next");
    return n && n.startsWith("/") && !n.startsWith("//") && !n.startsWith("/login") ? n : "/";
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const r = await localLogin(username, password, step === "totp" ? code : undefined);
      if (r.need_totp) {
        setStep("totp");
        setBusy(false);
        return;
      }
      location.assign(next());
    } catch (err) {
      setError((err as Error).message);
      if (step === "totp") setCode("");
      setBusy(false);
    }
  };

  const product = cfg.data?.product_name || "MSP Cloud";
  return (
    <div className="login">
      <div className="login-side" aria-hidden="true">
        <span className="brand-mark big"><span /><span /><span /></span>
        <strong>MSP Cloud</strong>
        <p>Servidores, VDCs e cotas de todos os seus clientes num só lugar.</p>
      </div>
      <main className="login-main">
        <form className="login-card" onSubmit={submit} noValidate>
          <h1>Entrar</h1>
          <p className="muted small" style={{ marginBottom: 18 }}>{product}</p>
          {step === "password" ? (
            <>
              <label className="field">
                Usuário
                <input className="input" autoComplete="username" autoCapitalize="none" spellCheck={false} autoFocus
                  value={username} onChange={(e) => setUsername(e.target.value)} />
              </label>
              <label className="field" style={{ marginTop: 12 }}>
                Senha
                <input className="input" type="password" autoComplete="current-password"
                  value={password} onChange={(e) => setPassword(e.target.value)} />
              </label>
            </>
          ) : (
            <>
              <p className="small" style={{ marginBottom: 10 }}>
                Abra o app autenticador e digite o código de 6 dígitos de <b>{username}</b>.
              </p>
              <label className="field">
                Código de verificação
                <input ref={codeRef} className="input code" inputMode="numeric" autoComplete="one-time-code" maxLength={6}
                  value={code} onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))} />
              </label>
            </>
          )}
          {error && <div className="alert" role="alert" style={{ marginTop: 14 }}>{error}</div>}
          <button className="btn btn-primary block" style={{ marginTop: 18 }}
            disabled={busy || !username.trim() || !password || (step === "totp" && code.length !== 6)}>
            {busy ? "Entrando…" : step === "totp" ? "Confirmar" : "Entrar"}
          </button>
          {step === "totp" && (
            <button type="button" className="btn btn-ghost block" style={{ marginTop: 8 }}
              onClick={() => { setStep("password"); setCode(""); setError(null); }}>
              Voltar
            </button>
          )}
          {cfg.data?.demo_hint && <p className="demo-hint">{cfg.data.demo_hint}</p>}
        </form>
      </main>
    </div>
  );
}
