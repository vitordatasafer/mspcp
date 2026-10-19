package api

import (
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// spa serve o build estático do portal com fallback para index.html (rotas
// do React Router) e cabeçalhos de segurança. O navegador só fala com esta
// origem e com o Keycloak (login e troca de código).
func (s *Server) spa() http.Handler {
	files := http.FileServer(http.Dir(s.WebDir))
	connect := "'self'"
	if u, err := url.Parse(s.OIDCIssuer); err == nil && u.Host != "" {
		connect += " " + u.Scheme + "://" + u.Host
	}
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"font-src 'self'; connect-src " + connect + "; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")

		clean := path.Clean("/" + r.URL.Path)
		full := filepath.Join(s.WebDir, filepath.FromSlash(clean))
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				h.Set("Cache-Control", "public, max-age=31536000, immutable") // nomes com hash
			}
			files.ServeHTTP(w, r)
			return
		}
		// Rota do SPA: entrega o index.html sem cache.
		h.Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(s.WebDir, "index.html"))
	})
}
