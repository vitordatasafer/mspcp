package api

// Login próprio: usuário e senha (argon2id), 2FA TOTP opcional e sessão em
// cookie HttpOnly. O token da sessão nunca é gravado: só o SHA-256 dele.
//
// CSRF: o cookie é SameSite=Strict e toda requisição que altera estado
// precisa do cabeçalho X-MSPVDC, que um site de terceiros não consegue
// mandar sem CORS (e a API não libera CORS).

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vitordatasafer/mspvdc/internal/localauth"
	"github.com/vitordatasafer/mspvdc/internal/rbac"
	"github.com/vitordatasafer/mspvdc/internal/store"
)

const sessionCookie = "mspvdc_session"

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// secureRequest diz se o navegador chegou por HTTPS (direto ou via proxy confiável).
func (s *Server) secureRequest(r *http.Request) bool {
	return r.TLS != nil || (s.TrustProxyHeaders && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", HttpOnly: true,
		Secure: s.secureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

func (s *Server) limiters() (*localauth.Limiter, *localauth.Limiter) {
	s.limOnce.Do(func() {
		s.userLim = localauth.NewLimiter(5, time.Minute)
		s.ipLim = localauth.NewLimiter(30, 10*time.Minute)
	})
	return s.userLim, s.ipLim
}

// ---------- login / logout ----------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-MSPVDC") == "" {
		writeErr(w, http.StatusForbidden, "cabeçalho X-MSPVDC ausente")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if in.Username == "" || in.Password == "" || len(in.Password) > 1024 {
		writeErr(w, http.StatusBadRequest, "informe usuário e senha")
		return
	}
	ip := remoteIP(r)
	userLim, ipLim := s.limiters()
	key := ip + "|" + in.Username
	now := time.Now()
	for _, l := range []struct {
		lim *localauth.Limiter
		key string
	}{{userLim, key}, {ipLim, ip}} {
		if blocked, left := l.lim.Blocked(l.key, now); blocked {
			w.Header().Set("Retry-After", itoa(int(left.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "muitas tentativas; aguarde um pouco e tente de novo")
			return
		}
	}
	fail := func(msg string) {
		userLim.Fail(key, now)
		ipLim.Fail(ip, now)
		s.Log.Warn("login recusado", "username", in.Username, "ip", ip, "motivo", msg)
		writeErr(w, http.StatusUnauthorized, msg)
	}

	st := s.store()
	u, err := st.GetUserByUsername(r.Context(), in.Username)
	if isNotFound(err) {
		localauth.DummyCheck(in.Password)
		fail("usuário ou senha inválidos")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	ok, err := localauth.CheckPassword(u.PasswordHash, in.Password)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok || u.Disabled {
		fail("usuário ou senha inválidos")
		return
	}
	if u.TOTPEnabled && u.TOTPSecret != nil {
		if in.TOTP == "" {
			// Senha certa: só agora revelamos que falta o segundo fator.
			writeJSON(w, http.StatusOK, map[string]any{"need_totp": true})
			return
		}
		if !localauth.CheckTOTP(*u.TOTPSecret, in.TOTP, now) {
			fail("código de verificação inválido")
			return
		}
	}
	userLim.Reset(key)

	tok := localauth.NewToken()
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.CreateSession(r.Context(), hashToken(tok), u.ID, ip, r.UserAgent()); err != nil {
			return err
		}
		if err := ts.MarkLogin(r.Context(), u.ID); err != nil {
			return err
		}
		return ts.Audit(r.Context(), store.AuditEntry{Subject: u.Username, Action: "auth.login", Target: u.Username,
			Details: map[string]any{"totp": u.TOTPEnabled}, RemoteIP: ip})
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.setSessionCookie(w, r, tok, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "must_change_password": u.MustChange})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-MSPVDC") == "" {
		writeErr(w, http.StatusForbidden, "cabeçalho X-MSPVDC ausente")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store().DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	s.setSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- minha conta ----------

func (s *Server) localUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	p := principal(r)
	if p.User == nil {
		writeErr(w, http.StatusBadRequest, "conta gerenciada pelo provedor de identidade")
		return nil, false
	}
	// Relê do banco: o principal pode ter sido montado antes de uma troca.
	u, err := s.store().GetUser(r.Context(), p.User.ID)
	if err != nil {
		s.internal(w, r, err)
		return nil, false
	}
	return &u, true
}

func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.localUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if ok, _ := localauth.CheckPassword(u.PasswordHash, in.Current); !ok {
		writeErr(w, http.StatusUnprocessableEntity, "senha atual incorreta")
		return
	}
	if err := localauth.ValidatePassword(in.New, u.Username); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if in.New == in.Current {
		writeErr(w, http.StatusUnprocessableEntity, "a nova senha precisa ser diferente da atual")
		return
	}
	hash, err := localauth.HashPassword(in.New)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.SetPassword(r.Context(), u.ID, hash, false); err != nil {
			return err
		}
		// Outras sessões abertas caem; a atual continua.
		if err := ts.DeleteUserSessions(r.Context(), u.ID, principal(r).SessionHash); err != nil {
			return err
		}
		return s.audit(r, ts, "user.password_change", u.Username, nil, nil)
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) totpSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := s.localUser(w, r)
	if !ok {
		return
	}
	if u.TOTPEnabled {
		writeErr(w, http.StatusConflict, "a verificação em duas etapas já está ativa")
		return
	}
	secret := localauth.NewTOTPSecret()
	if err := s.store().SetTOTPPending(r.Context(), u.ID, secret); err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret, "uri": localauth.TOTPURI(s.issuerName(), u.Username, secret),
	})
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) {
	u, ok := s.localUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	if u.TOTPPending == nil {
		writeErr(w, http.StatusBadRequest, "gere o código QR antes de confirmar")
		return
	}
	if !localauth.CheckTOTP(*u.TOTPPending, in.Code, time.Now()) {
		writeErr(w, http.StatusUnprocessableEntity, "código inválido; confira o relógio do celular e tente de novo")
		return
	}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.EnableTOTP(r.Context(), u.ID); err != nil {
			return err
		}
		return s.audit(r, ts, "user.totp_enable", u.Username, nil, nil)
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	u, ok := s.localUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if ok, _ := localauth.CheckPassword(u.PasswordHash, in.Password); !ok {
		writeErr(w, http.StatusUnprocessableEntity, "senha incorreta")
		return
	}
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.DisableTOTP(r.Context(), u.ID); err != nil {
			return err
		}
		return s.audit(r, ts, "user.totp_disable", u.Username, nil, nil)
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) issuerName() string {
	if s.ProductName != "" {
		return s.ProductName
	}
	return "mspvdc"
}

// ---------- gestão de usuários ----------

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}$`)

// createUser cria um usuário local já vinculado ao tenant, com senha
// temporária devolvida uma única vez.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	var in struct {
		Username  string     `json:"username"`
		Name      string     `json:"name"`
		Email     string     `json:"email"`
		Role      string     `json:"role"`
		ProjectID *uuid.UUID `json:"project_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	in.Name, in.Email = strings.TrimSpace(in.Name), strings.TrimSpace(in.Email)
	if !usernameRe.MatchString(in.Username) {
		writeErr(w, http.StatusBadRequest, "usuário: de 2 a 63 caracteres, letras minúsculas, números, ponto, hífen ou sublinhado")
		return
	}
	if in.Name == "" || len(in.Name) > 120 || len(in.Email) > 200 || (in.Email != "" && !strings.Contains(in.Email, "@")) {
		writeErr(w, http.StatusBadRequest, "informe o nome (e um e-mail válido, se houver)")
		return
	}
	if !rbac.RoleFitsTenant(in.Role, t.Kind) {
		writeErr(w, http.StatusBadRequest, "papel incompatível com o tipo da conta")
		return
	}
	if !principal(r).CanAssign(in.Role, t.Path, t.Kind) {
		writeErr(w, http.StatusForbidden, "sem permissão para dar esse papel")
		return
	}
	st := s.store()
	if in.ProjectID != nil {
		p, err := st.GetProject(r.Context(), *in.ProjectID)
		if err != nil || p.TenantID != t.ID {
			writeErr(w, http.StatusBadRequest, "o VDC escolhido não pertence a esta conta")
			return
		}
	}
	temp := localauth.TempPassword()
	hash, err := localauth.HashPassword(temp)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	var u store.User
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		var err error
		if u, err = ts.CreateUser(r.Context(), in.Username, in.Name, in.Email, hash, true); err != nil {
			return err
		}
		if err := ts.CreateMembership(r.Context(), u.Username, t.ID, in.ProjectID, in.Role); err != nil {
			return err
		}
		return s.audit(r, ts, "user.create", u.Username, &t.ID, map[string]any{"role": in.Role, "project_id": in.ProjectID})
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "já existe um usuário com esse nome")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "temporary_password": temp})
}

// canManageUser: quem pede precisa poder dar cada um dos papéis que o
// usuário tem. Assim um admin de cliente não mexe na senha de alguém da
// plataforma que por acaso também esteja vinculado ao cliente.
func (s *Server) canManageUser(r *http.Request, st *store.Store, username string) (bool, error) {
	ms, err := st.MembershipsOfSubject(r.Context(), username)
	if err != nil || len(ms) == 0 {
		return false, err
	}
	p := principal(r)
	for _, m := range ms {
		if !p.CanAssign(m.Role, m.TenantPath, m.TenantKind) {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(chi.URLParam(r, "username"))
	var in struct {
		ResetTOTP bool `json:"reset_totp"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	st := s.store()
	u, err := st.GetUserByUsername(r.Context(), username)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	ok, err := s.canManageUser(r, st, username)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	if p := principal(r); p.User != nil && p.User.ID == u.ID {
		writeErr(w, http.StatusBadRequest, "para a sua própria senha, use Minha conta")
		return
	}
	temp := localauth.TempPassword()
	hash, err := localauth.HashPassword(temp)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.SetPassword(r.Context(), u.ID, hash, true); err != nil {
			return err
		}
		if in.ResetTOTP {
			if err := ts.DisableTOTP(r.Context(), u.ID); err != nil {
				return err
			}
		}
		if err := ts.DeleteUserSessions(r.Context(), u.ID, nil); err != nil {
			return err
		}
		return s.audit(r, ts, "user.password_reset", u.Username, nil, map[string]any{"reset_totp": in.ResetTOTP})
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"temporary_password": temp})
}

func (s *Server) deleteMembership(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantInScope(w, r)
	if !ok {
		return
	}
	mid, err := uuid.Parse(chi.URLParam(r, "mid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	}
	st := s.store()
	m, tenantID, err := st.GetMembership(r.Context(), mid)
	if isNotFound(err) || (err == nil && tenantID != t.ID) {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	p := principal(r)
	if !p.CanAssign(m.Role, t.Path, t.Kind) {
		writeErr(w, http.StatusForbidden, "sem permissão para remover esse acesso")
		return
	}
	if m.Subject == p.Subject {
		writeErr(w, http.StatusBadRequest, "você não pode remover o próprio acesso")
		return
	}
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ts := store.New(tx)
		if err := ts.DeleteMembership(r.Context(), mid); err != nil {
			return err
		}
		// Sem nenhum vínculo restante, as sessões abertas do usuário caem.
		rest, err := ts.MembershipsOfSubject(r.Context(), m.Subject)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			if u, err := ts.GetUserByUsername(r.Context(), m.Subject); err == nil {
				if err := ts.DeleteUserSessions(r.Context(), u.ID, nil); err != nil {
					return err
				}
			}
		}
		return s.audit(r, ts, "membership.delete", m.Subject, &t.ID, map[string]any{"role": m.Role})
	})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
