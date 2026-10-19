// Package api expõe a API HTTP v1, consumida tanto pelo console interno
// quanto pelo portal do tenant. A autorização acontece aqui (RBAC) e no
// store (todo SELECT voltado a usuário é filtrado pelo escopo).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/mspclouds/mspcp/internal/auth"
	"github.com/mspclouds/mspcp/internal/localauth"
	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/store"
)

type Server struct {
	Pool *pgxpool.Pool
	// Verifier valida tokens OIDC (Bearer). nil = só login local.
	Verifier auth.Verifier
	// AuthMode diz ao portal como entrar: "local" (padrão) ou "oidc".
	AuthMode string
	// ProductName aparece no app autenticador (2FA).
	ProductName string
	// DemoHint aparece na tela de login (só na demonstração).
	DemoHint string
	// Clients fala com os clusters (console no navegador).
	Clients *proxmox.Clients
	River   *river.Client[pgx.Tx]
	Allow   proxmox.Allowlist
	Log     *slog.Logger
	// TrustProxyHeaders liga X-Forwarded-For/X-Real-IP (só atrás do Nginx).
	TrustProxyHeaders bool
	// WebDir, se definido, serve o portal (build do Vite) na raiz.
	WebDir string
	// Config pública entregue ao portal em /config.json.
	OIDCIssuer   string
	OIDCClientID string

	limOnce  sync.Once
	userLim  *localauth.Limiter
	ipLim    *localauth.Limiter
	consoles consoleTickets
}

func (s *Server) store() *store.Store { return store.New(s.Pool) }

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if s.TrustProxyHeaders {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "banco indisponível")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.With(middleware.Timeout(30*time.Second)).Post("/auth/login", s.login)
	r.With(middleware.Timeout(30*time.Second)).Post("/auth/logout", s.logout)

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.authenticate)
		// Console: conexão longa, fora do timeout das demais rotas.
		r.Get("/console/{ticket}", s.consoleSocket)

		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(30 * time.Second))
			r.Get("/me", s.getMe)
			r.Put("/me/password", s.changeMyPassword)
			r.Post("/me/totp/setup", s.totpSetup)
			r.Post("/me/totp/enable", s.totpEnable)
			r.Post("/me/totp/disable", s.totpDisable)

			r.Get("/tenants", s.listTenants)
			r.Post("/tenants", s.createTenant)
			r.Get("/tenants/{id}/projects", s.listProjects)
			r.Post("/tenants/{id}/projects", s.createProject)
			r.Get("/tenants/{id}/memberships", s.listMemberships)
			r.Post("/tenants/{id}/memberships", s.createMembership)
			r.Delete("/tenants/{id}/memberships/{mid}", s.deleteMembership)
			r.Post("/tenants/{id}/users", s.createUser)
			r.Post("/users/{username}/reset-password", s.resetPassword)
			r.Get("/tenants/{id}/capacity", s.getCapacity)
			r.Put("/tenants/{id}/limits", s.putLimits)
			r.Get("/projects", s.listAllProjects)
			r.Put("/projects/{id}/quota", s.updateProjectQuota)
			r.Put("/projects/{id}/network", s.setProjectNetwork)
			r.Post("/projects/{id}/vms", s.createVM)

			r.Get("/storage-classes", s.listStorageClasses)
			r.Post("/storage-classes", s.createStorageClass)
			r.Get("/storage-policies", s.listStoragePolicies)
			r.Put("/clusters/{id}/storage-policies", s.setStoragePolicy)

			r.Get("/clusters", s.listClusters)
			r.Post("/clusters", s.createCluster)
			r.Get("/clusters/{id}/templates", s.listTemplates)
			r.Post("/clusters/probe", s.probeCluster)
			r.Get("/clusters/{id}", s.getCluster)
			r.Put("/clusters/{id}", s.updateCluster)
			r.Post("/clusters/{id}/sync", s.syncCluster)
			r.Put("/clusters/{id}/reservations/{tenant}", s.putReservation)
			r.Delete("/clusters/{id}/reservations/{tenant}", s.deleteReservation)

			r.Get("/images", s.listImages)
			r.Post("/images", s.createImage)
			r.Delete("/images/{id}", s.deleteImage)

			r.Get("/resources", s.listResources)
			r.Get("/resources/{id}", s.getResource)
			r.Delete("/resources/{id}", s.deleteVM)
			r.Post("/resources/{id}/actions", s.createAction)
			r.Post("/resources/{id}/assign", s.assignVM)
			r.Post("/resources/{id}/console", s.openConsole)
			r.Get("/resources/{id}/tasks", s.listResourceTasks)

			r.Get("/tasks", s.listTasks)
			r.Get("/tasks/{id}", s.getTask)
		})
	})

	// Configuração pública do portal: nada sensível, só como entrar.
	r.Get("/config.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"auth": s.authMode(), "product_name": s.issuerName(), "demo_hint": s.DemoHint,
			"oidc_issuer": s.OIDCIssuer, "oidc_client_id": s.OIDCClientID,
		})
	})
	if s.WebDir != "" {
		r.Handle("/*", s.spa())
	}
	return r
}

func (s *Server) authMode() string {
	if s.AuthMode == "" {
		return "local"
	}
	return s.AuthMode
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- autenticação ----------

type ctxKey struct{}

func principal(r *http.Request) *rbac.Principal {
	return r.Context().Value(ctxKey{}).(*rbac.Principal)
}

// authenticate aceita a sessão do login local (cookie) ou, se houver
// Verifier, um token OIDC (Bearer).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.store()
		var p *rbac.Principal
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if isUnsafe(r.Method) && r.Header.Get("X-MSPCP") == "" {
				writeErr(w, http.StatusForbidden, "cabeçalho X-MSPCP ausente")
				return
			}
			h := hashToken(c.Value)
			u, err := st.SessionUser(r.Context(), h)
			if isNotFound(err) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sessão expirada; entre de novo", "code": "session_expired"})
				return
			} else if err != nil {
				s.internal(w, r, err)
				return
			}
			p = &rbac.Principal{Subject: u.Username, Email: u.Email, Name: u.Name, User: &u, SessionHash: h}
		} else if raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && raw != "" && s.Verifier != nil {
			claims, err := s.Verifier.Verify(r.Context(), raw)
			if err != nil {
				writeErr(w, http.StatusUnauthorized, "token inválido")
				return
			}
			p = &rbac.Principal{Subject: claims.Subject, Email: claims.Email}
		} else {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "não autenticado", "code": "login_required"})
			return
		}
		ms, err := st.MembershipsBySubject(r.Context(), p.Subject)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		p.Memberships = ms
		me := r.URL.Path == "/v1/me" || strings.HasPrefix(r.URL.Path, "/v1/me/")
		// Senha temporária: só dá para trocar a senha (e ver /me).
		if p.User != nil && p.User.MustChange && !me {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "troque a senha temporária antes de continuar", "code": "must_change_password"})
			return
		}
		if len(ms) == 0 && !me {
			writeErr(w, http.StatusForbidden, "usuário sem acesso a nenhum tenant")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("erro interno", "err", err, "path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()))
	writeErr(w, http.StatusInternalServerError, "erro interno")
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "não encontrado")
		return uuid.Nil, false
	}
	return id, true
}

func remoteIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (s *Server) audit(r *http.Request, st *store.Store, action, target string, targetTenant *uuid.UUID, details map[string]any) error {
	p := principal(r)
	if details == nil {
		details = map[string]any{}
	}
	details["request_id"] = middleware.GetReqID(r.Context())
	return st.Audit(r.Context(), store.AuditEntry{
		Subject: p.Subject, ActorTenantID: p.HomeTenant(), TargetTenantID: targetTenant,
		Action: action, Target: target, Details: details, RemoteIP: remoteIP(r),
	})
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
