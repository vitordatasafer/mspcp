import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import QRCode from "qrcode";
import { api, roleLabel, type Me } from "../api";
import { logout } from "../auth";
import { useSession } from "../session";

const MIN = 10;

function PasswordForm({ onDone, submitLabel = "Trocar senha" }: { onDone: () => void; submitLabel?: string }) {
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [again, setAgain] = useState("");
  const save = useMutation({
    mutationFn: () => api("/v1/me/password", { method: "PUT", json: { current_password: cur, new_password: next } }),
    onSuccess: () => {
      setCur(""); setNext(""); setAgain("");
      onDone();
    },
  });
  const mismatch = again !== "" && again !== next;
  return (
    <form onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
      <div className="stack" style={{ gap: 12 }}>
        <label className="field">
          Senha atual
          <input className="input" type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} />
        </label>
        <label className="field">
          Nova senha
          <input className="input" type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
          <span className="hint">Pelo menos {MIN} caracteres. Uma frase curta é mais fácil de lembrar.</span>
        </label>
        <label className="field">
          Repita a nova senha
          <input className="input" type="password" autoComplete="new-password" value={again} onChange={(e) => setAgain(e.target.value)} aria-invalid={mismatch} />
          {mismatch && <span className="hint err">As senhas não conferem.</span>}
        </label>
      </div>
      {save.error && <div className="alert" style={{ marginTop: 12 }}>{save.error.message}</div>}
      {save.isSuccess && <div className="note-ok" role="status" style={{ marginTop: 12 }}>Senha trocada. As outras sessões abertas foram encerradas.</div>}
      <div className="form-actions">
        <button className="btn btn-primary" disabled={!cur || next.length < MIN || next !== again || save.isPending}>{submitLabel}</button>
      </div>
    </form>
  );
}

// Primeiro acesso (ou senha resetada): só dá para trocar a senha.
export function ForcedPasswordChange({ me }: { me: Me }) {
  const qc = useQueryClient();
  return (
    <div className="center-screen">
      <div className="panel" style={{ textAlign: "left", width: "min(440px, 100%)", maxWidth: 440 }}>
        <h2>Defina a sua senha</h2>
        <p className="muted small" style={{ margin: "6px 0 16px" }}>
          Olá, {me.name || me.subject}. Você entrou com uma senha temporária; escolha uma senha nova para continuar.
        </p>
        <PasswordForm submitLabel="Salvar e continuar" onDone={() => qc.invalidateQueries({ queryKey: ["me"] })} />
        <button className="btn btn-ghost btn-sm" style={{ marginTop: 4 }} onClick={() => logout()}>Sair</button>
      </div>
    </div>
  );
}

export function AccountPage() {
  const s = useSession();
  const qc = useQueryClient();
  const me = s.me;
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Minha conta</h1>
          <p>{s.name}{me.email ? ` · ${me.email}` : ""}</p>
        </div>
      </div>
      <div className="two-col">
        <section className="panel">
          <h2 style={{ marginBottom: 12 }}>Acessos</h2>
          <dl className="kv">
            <dt>Usuário</dt><dd>{me.subject}</dd>
            {me.memberships.map((m, i) => {
              const t = s.tenants.find((x) => x.id === m.tenant_id);
              return (
                <div key={i} style={{ display: "contents" }}>
                  <dt>{t?.name ?? "Conta"}</dt>
                  <dd>{roleLabel[m.role] ?? m.role}{m.project_id ? ", em um VDC" : ""}</dd>
                </div>
              );
            })}
          </dl>
        </section>
        {me.local ? (
          <div className="stack">
            <section className="panel">
              <h2 style={{ marginBottom: 12 }}>Senha</h2>
              <PasswordForm onDone={() => undefined} />
            </section>
            <TwoFactor enabled={me.totp_enabled} onChange={() => qc.invalidateQueries({ queryKey: ["me"] })} />
          </div>
        ) : (
          <section className="panel">
            <h2>Senha e verificação</h2>
            <p className="muted small" style={{ marginTop: 8 }}>Sua conta é gerenciada pelo login da empresa (SSO).</p>
          </section>
        )}
      </div>
    </>
  );
}

function TwoFactor({ enabled, onChange }: { enabled: boolean; onChange: () => void }) {
  const [setup, setSetup] = useState<{ secret: string; uri: string } | null>(null);
  const [qr, setQr] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");

  useEffect(() => {
    if (!setup) return;
    QRCode.toDataURL(setup.uri, { margin: 1, width: 196, errorCorrectionLevel: "M" }).then(setQr).catch(() => setQr(null));
  }, [setup]);

  const start = useMutation({
    mutationFn: () => api<{ secret: string; uri: string }>("/v1/me/totp/setup", { method: "POST" }),
    onSuccess: (d) => { setSetup(d); setCode(""); },
  });
  const enable = useMutation({
    mutationFn: () => api("/v1/me/totp/enable", { method: "POST", json: { code } }),
    onSuccess: () => { setSetup(null); onChange(); },
  });
  const disable = useMutation({
    mutationFn: () => api("/v1/me/totp/disable", { method: "POST", json: { password } }),
    onSuccess: () => { setPassword(""); onChange(); },
  });

  return (
    <section className="panel">
      <div className="panel-head">
        <h2>Verificação em duas etapas</h2>
        <span className={`pill ${enabled ? "pill-ok" : ""}`}>{enabled ? "Ativa" : "Desativada"}</span>
      </div>
      {enabled ? (
        <form onSubmit={(e) => { e.preventDefault(); disable.mutate(); }}>
          <p className="muted small" style={{ marginBottom: 12 }}>
            Ao entrar, o portal pede o código do app autenticador além da senha.
          </p>
          <label className="field">
            Confirme a senha para desativar
            <input className="input" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </label>
          {disable.error && <div className="alert" style={{ marginTop: 12 }}>{disable.error.message}</div>}
          <div className="form-actions">
            <button className="btn btn-danger" disabled={!password || disable.isPending}>Desativar</button>
          </div>
        </form>
      ) : !setup ? (
        <>
          <p className="muted small">
            Proteja a conta com um código do celular (Google Authenticator, Microsoft Authenticator, 1Password, Authy…).
          </p>
          {start.error && <div className="alert" style={{ marginTop: 12 }}>{start.error.message}</div>}
          <div className="form-actions">
            <button className="btn btn-primary" onClick={() => start.mutate()} disabled={start.isPending}>Ativar</button>
          </div>
        </>
      ) : (
        <form onSubmit={(e) => { e.preventDefault(); enable.mutate(); }}>
          <ol className="steps">
            <li>No app autenticador, leia o código QR.</li>
            <li>Digite o código de 6 dígitos que aparecer.</li>
          </ol>
          <div className="totp-setup">
            {qr ? <img src={qr} width={196} height={196} alt="Código QR para o app autenticador" /> : <div className="qr-placeholder" />}
            <div>
              <p className="small muted">Sem câmera? Digite esta chave no app:</p>
              <code className="secret">{setup.secret.replace(/(.{4})/g, "$1 ").trim()}</code>
              <label className="field" style={{ marginTop: 14 }}>
                Código
                <input className="input code" inputMode="numeric" autoComplete="one-time-code" maxLength={6}
                  value={code} onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))} />
              </label>
            </div>
          </div>
          {enable.error && <div className="alert" style={{ marginTop: 12 }}>{enable.error.message}</div>}
          <div className="form-actions">
            <button type="button" className="btn btn-ghost" onClick={() => setSetup(null)}>Cancelar</button>
            <button className="btn btn-primary" disabled={code.length !== 6 || enable.isPending}>Confirmar e ativar</button>
          </div>
        </form>
      )}
    </section>
  );
}
