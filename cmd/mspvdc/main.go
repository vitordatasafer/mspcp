// Binário único com subcomandos:
//
//	mspvdc migrate                         aplica migrations (schema + River)
//	mspvdc bootstrap -name N -admin USER   cria o provedor e o 1º super_admin (senha temporária)
//	mspvdc reset-password -user USER       nova senha temporária (e desliga o 2FA)
//	mspvdc cluster-add ...                 cadastra um cluster Proxmox e guarda o segredo do token
//	mspvdc api                             sobe a API HTTP(S) e o portal
//	mspvdc worker                          sobe sincronizador + executor de Tasks
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vitordatasafer/mspvdc/internal/api"
	"github.com/vitordatasafer/mspvdc/internal/auth"
	"github.com/vitordatasafer/mspvdc/internal/db"
	"github.com/vitordatasafer/mspvdc/internal/inventory"
	"github.com/vitordatasafer/mspvdc/internal/jobs"
	"github.com/vitordatasafer/mspvdc/internal/localauth"
	"github.com/vitordatasafer/mspvdc/internal/proxmox"
	"github.com/vitordatasafer/mspvdc/internal/rbac"
	"github.com/vitordatasafer/mspvdc/internal/secrets"
	"github.com/vitordatasafer/mspvdc/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "uso: mspvdc <migrate|bootstrap|reset-password|cluster-add|api|worker>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := newLogger()

	var err error
	switch os.Args[1] {
	case "migrate":
		err = runMigrate(ctx)
	case "bootstrap":
		err = runBootstrap(ctx, os.Args[2:])
	case "reset-password":
		err = runResetPassword(ctx, os.Args[2:])
	case "cluster-add":
		err = runClusterAdd(ctx, os.Args[2:])
	case "api":
		err = runAPI(ctx, log)
	case "worker":
		err = runWorker(ctx, log)
	default:
		err = fmt.Errorf("subcomando desconhecido %q", os.Args[1])
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("encerrando com erro", "err", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Fprintf(os.Stderr, "variável obrigatória ausente: %s\n", k)
		os.Exit(2)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func runMigrate(ctx context.Context) error {
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	return db.Migrate(ctx, pool)
}

func runBootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	name := fs.String("name", "mspclouds", "nome do tenant provedor")
	admin := fs.String("admin", "admin", "usuário do primeiro super_admin")
	oidc := fs.Bool("oidc", false, "o admin vem do OIDC (-admin é o sub); não cria usuário local")
	_ = fs.Parse(args)
	*admin = strings.ToLower(strings.TrimSpace(*admin))
	if *admin == "" {
		return errors.New("-admin é obrigatório")
	}
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	temp := localauth.TempPassword()
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		st := store.New(tx)
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE kind = 'provider')`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("já existe um tenant provedor; bootstrap só roda uma vez (para recuperar o acesso use reset-password)")
		}
		t, err := st.CreateTenant(ctx, nil, store.KindProvider, *name)
		if err != nil {
			return err
		}
		if !*oidc {
			hash, err := localauth.HashPassword(temp)
			if err != nil {
				return err
			}
			if _, err := st.CreateUser(ctx, *admin, "Administrador", "", hash, true); err != nil {
				return err
			}
		}
		if err := st.CreateMembership(ctx, *admin, t.ID, nil, rbac.RoleSuperAdmin); err != nil {
			return err
		}
		return st.Audit(ctx, store.AuditEntry{Subject: "bootstrap", TargetTenantID: &t.ID, Action: "bootstrap", Target: *admin})
	})
	if err != nil {
		return err
	}
	fmt.Printf("Provedor %q criado. Administrador: %s\n", *name, *admin)
	if !*oidc {
		fmt.Printf("Senha temporária: %s\n(será pedida a troca no primeiro acesso)\n", temp)
	}
	return nil
}

func runResetPassword(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	user := fs.String("user", "", "usuário")
	keepTOTP := fs.Bool("keep-totp", false, "mantém a verificação em duas etapas")
	_ = fs.Parse(args)
	if *user == "" {
		return errors.New("-user é obrigatório")
	}
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	temp := localauth.TempPassword()
	hash, err := localauth.HashPassword(temp)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		st := store.New(tx)
		u, err := st.GetUserByUsername(ctx, strings.ToLower(*user))
		if err != nil {
			return fmt.Errorf("usuário %q: %w", *user, err)
		}
		if err := st.SetPassword(ctx, u.ID, hash, true); err != nil {
			return err
		}
		if !*keepTOTP {
			if err := st.DisableTOTP(ctx, u.ID); err != nil {
				return err
			}
		}
		if err := st.DeleteUserSessions(ctx, u.ID, nil); err != nil {
			return err
		}
		return st.Audit(ctx, store.AuditEntry{Subject: "cli", Action: "user.password_reset", Target: u.Username,
			Details: map[string]any{"reset_totp": !*keepTOTP}})
	})
	if err != nil {
		return err
	}
	fmt.Printf("Senha temporária de %s: %s\n", *user, temp)
	return nil
}

// runClusterAdd cadastra um cluster: grava o segredo do token em
// SECRETS_DIR (0600) e a referência no banco. Valida a conexão antes.
func runClusterAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cluster-add", flag.ExitOnError)
	name := fs.String("name", "", "nome interno (ex.: lab-01)")
	dc := fs.String("dc", "", "datacenter (ex.: Cirion)")
	location := fs.String("location", "", "rótulo mostrado a MSPs e clientes (ex.: São Paulo 1)")
	apiURL := fs.String("url", "", "https://host:8006")
	caFile := fs.String("ca-file", "", "CA do cluster (/etc/pve/pve-root-ca.pem); vazio = fixa o certificado apresentado")
	tokenID := fs.String("token-id", "", "user@realm!nome")
	secretFile := fs.String("secret-file", "", "arquivo com o segredo do token (- = ler da entrada padrão)")
	bridge := fs.String("bridge", "vmbr0", "bridge padrão das VMs novas")
	_ = fs.Parse(args)
	if *name == "" || *dc == "" || *apiURL == "" || *tokenID == "" || *secretFile == "" {
		fs.Usage()
		return errors.New("faltam parâmetros")
	}
	var ca []byte
	var err error
	if *caFile != "" {
		if ca, err = os.ReadFile(*caFile); err != nil {
			return err
		}
	}
	var raw []byte
	if *secretFile == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 4096))
	} else {
		raw, err = os.ReadFile(*secretFile)
	}
	if err != nil {
		return err
	}
	secret := strings.TrimSpace(string(raw))
	allow := proxmox.ParseAllowlist(os.Getenv("PROXMOX_ALLOWED_HOSTS"))
	if len(ca) == 0 {
		p, err := proxmox.ProbeTLS(ctx, *apiURL, allow)
		if err != nil {
			return err
		}
		if !p.TrustedBySystem {
			ca = []byte(p.SuggestedPEM)
			fmt.Printf("Certificado fixado: %s (SHA-256 %s)\n", p.Pinned, p.Chain[len(p.Chain)-1].Fingerprint)
		}
	}
	cl, err := proxmox.NewClient(*apiURL, string(ca), *tokenID, secret, allow)
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snap, err := cl.Read(tctx, false)
	if err != nil {
		return fmt.Errorf("teste de conexão falhou: %w", err)
	}
	fmt.Printf("Conexão ok: PVE %s, %d nodes, %d VMs/CTs e %d templates visíveis para o token.\n",
		snap.Inventory.Version, snap.Inventory.NodesOnline, len(snap.VMs), len(snap.Templates))

	box, err := masterBox(nil)
	if err != nil {
		return err
	}
	if box == nil {
		return errors.New("defina MASTER_KEY_FILE (a chave que cifra o segredo no banco)")
	}
	enc, err := box.Seal(secret)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if *location == "" {
		*location = *dc
	}
	c, err := store.New(pool).CreateCluster(ctx, store.Cluster{Name: *name, DC: *dc, Location: *location, APIURL: *apiURL,
		CACertPEM: string(ca), TokenID: *tokenID, TokenSecretEnc: enc, DefaultBridge: *bridge})
	if err != nil {
		return err
	}
	fmt.Printf("Cluster %s cadastrado (%s); o segredo ficou cifrado no banco.\n", c.Name, c.ID)
	return nil
}

// allowlist lê PROXMOX_ALLOWED_HOSTS. Vazia, o serviço sobe, mas nenhum
// cluster pode ser cadastrado nem acessado.
func allowlist(log *slog.Logger) proxmox.Allowlist {
	a := proxmox.ParseAllowlist(os.Getenv("PROXMOX_ALLOWED_HOSTS"))
	if len(a) == 0 {
		log.Warn("PROXMOX_ALLOWED_HOSTS vazio: nenhum cluster Proxmox será acessado")
	}
	return a
}

// masterBox carrega a chave que cifra os tokens guardados no banco.
func masterBox(log *slog.Logger) (*secrets.Box, error) {
	path := os.Getenv("MASTER_KEY_FILE")
	if path == "" {
		if log != nil {
			log.Warn("MASTER_KEY_FILE não definido: não será possível cadastrar clusters pelo portal")
		}
		return nil, nil
	}
	return secrets.LoadBox(path)
}

func secretsResolver() secrets.Resolver {
	return secrets.Multi{"file": secrets.Dir(envOr("SECRETS_DIR", "/etc/mspvdc/secrets")), "env": secrets.Env{}}
}

func runAPI(ctx context.Context, log *slog.Logger) error {
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	rc, err := jobs.NewInsertClient(pool)
	if err != nil {
		return err
	}
	allow := allowlist(log)
	box, err := masterBox(log)
	if err != nil {
		return err
	}
	srv := &api.Server{
		Pool:              pool,
		AuthMode:          envOr("AUTH_MODE", "local"),
		ProductName:       envOr("PRODUCT_NAME", "mspvdc"),
		River:             rc,
		Allow:             allow,
		Clients:           proxmox.NewClients(allow, secretsResolver(), box),
		Log:               log,
		TrustProxyHeaders: os.Getenv("TRUST_PROXY_HEADERS") == "true",
		WebDir:            os.Getenv("WEB_DIR"),
	}
	switch srv.AuthMode {
	case "local":
	case "oidc":
		httpc, err := oidcHTTPClient(os.Getenv("OIDC_CA_FILE"))
		if err != nil {
			return err
		}
		srv.Verifier = auth.NewOIDCVerifier(mustEnv("OIDC_ISSUER"), mustEnv("OIDC_AUDIENCE"), httpc)
		srv.OIDCIssuer = os.Getenv("OIDC_ISSUER")
		srv.OIDCClientID = envOr("OIDC_CLIENT_ID", os.Getenv("OIDC_AUDIENCE"))
	default:
		return fmt.Errorf("AUTH_MODE deve ser local ou oidc")
	}

	// Limpeza periódica das sessões vencidas.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := store.New(pool).PurgeSessions(ctx); err != nil && ctx.Err() == nil {
				log.Warn("limpeza de sessões", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	certFile, keyFile := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	hs := &http.Server{
		Addr:              envOr("LISTEN_ADDR", ":8443"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if certFile != "" {
		hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	errc := make(chan error, 2)
	go func() {
		log.Info("api ouvindo", "addr", hs.Addr, "tls", certFile != "", "auth", srv.AuthMode)
		if certFile != "" {
			errc <- hs.ListenAndServeTLS(certFile, keyFile)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	// Opcional: porta 80 só redireciona para HTTPS.
	var redirect *http.Server
	if addr := os.Getenv("HTTP_REDIRECT_ADDR"); addr != "" && certFile != "" {
		redirect = &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if _, port, err := net.SplitHostPort(hs.Addr); err == nil && port != "443" {
				host = net.JoinHostPort(host, port)
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
		})}
		go func() {
			if err := redirect.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if redirect != nil {
		_ = redirect.Shutdown(shutCtx)
	}
	return hs.Shutdown(shutCtx)
}

func runWorker(ctx context.Context, log *slog.Logger) error {
	pool, err := db.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	box, err := masterBox(log)
	if err != nil {
		return err
	}
	clients := proxmox.NewClients(allowlist(log), secretsResolver(), box)

	interval, err := time.ParseDuration(envOr("SYNC_INTERVAL", "20s"))
	if err != nil {
		return fmt.Errorf("SYNC_INTERVAL: %w", err)
	}
	concurrency, err := strconv.Atoi(envOr("WORKER_CONCURRENCY", "50"))
	if err != nil {
		return fmt.Errorf("WORKER_CONCURRENCY: %w", err)
	}

	rc, err := jobs.NewWorkerClient(pool,
		&jobs.PowerWorker{Pool: pool, Clients: clients, Log: log},
		&jobs.VMWorker{Pool: pool, Clients: clients, Log: log}, concurrency)
	if err != nil {
		return err
	}
	if err := rc.Start(ctx); err != nil {
		return err
	}
	syncer := &inventory.Syncer{Pool: pool, Clients: clients, Interval: interval, Log: log}
	go syncer.Run(ctx)
	log.Info("worker rodando", "sync_interval", interval, "concurrency", concurrency)

	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rc.Stop(stopCtx)
}

// oidcHTTPClient confia na CA do sistema, mais OIDC_CA_FILE se informado
// (Keycloak interno com CA própria).
func oidcHTTPClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("OIDC_CA_FILE sem certificado válido")
		}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, Proxy: http.ProxyFromEnvironment},
	}, nil
}
