// devstack sobe o mspcp inteiro para desenvolvimento e demonstração, sem
// Proxmox real: dois clusters PVE simulados (Cirion e Ascenty, com templates
// e console), usuários locais prontos, API, worker e o portal.
//
//	DATABASE_URL=postgres://... go run ./cmd/devstack -reset
//	DATABASE_URL=postgres://... go run ./cmd/devstack -reset -public 192.168.0.50
//
// Precisa só de um PostgreSQL vazio (o -reset apaga o schema public).
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mspclouds/mspcp/internal/api"
	"github.com/mspclouds/mspcp/internal/db"
	"github.com/mspclouds/mspcp/internal/inventory"
	"github.com/mspclouds/mspcp/internal/jobs"
	"github.com/mspclouds/mspcp/internal/localauth"
	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/quota"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/secrets"
	"github.com/mspclouds/mspcp/internal/store"
	"github.com/mspclouds/mspcp/internal/testutil"
)

const (
	tokenID      = "portal@pve!mspcp"
	secret       = "segredo-de-demonstracao"
	demoPassword = "demo12345"
)

type demoUser struct{ username, name, email string }

var users = []demoUser{
	{"vitor", "Vitor", "vitor@mspclouds.com"},
	{"noc", "Plantão NOC", "noc@mspclouds.com"},
	{"carla", "Carla Mendes", "carla@alphati.com.br"},
	{"marcos", "Marcos Souza", "marcos@paoquente.com.br"},
	{"julia", "Júlia Ramos", "julia@clinicasorriso.com.br"},
	{"leo", "Léo Martins", "leo@paoquente.com.br"},
}

func main() {
	dsn := flag.String("db", os.Getenv("DATABASE_URL"), "PostgreSQL")
	web := flag.String("web", "web/dist", "build do portal (vazio = não servir)")
	addr := flag.String("addr", "localhost:8080", "onde escutar sem -public")
	public := flag.String("public", "", "IP ou nome pelo qual o navegador acessa a demo (ex.: 192.168.0.50): "+
		"escuta em todas as interfaces, na porta de -addr, com HTTPS e certificado autoassinado")
	reset := flag.Bool("reset", false, "apaga o schema public e recria os dados de exemplo")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "defina DATABASE_URL ou -db")
		os.Exit(2)
	}
	_, port, err := net.SplitHostPort(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "-addr precisa estar no formato host:porta")
		os.Exit(2)
	}

	// Sem -public: http://localhost. Com -public: HTTPS com certificado
	// autoassinado (cookie Secure e console funcionam fora de localhost).
	cfg := listenCfg{listen: *addr, origin: "http://" + *addr}
	if *public != "" {
		cert, pool, err := selfSigned(*public)
		if err != nil {
			fmt.Fprintln(os.Stderr, "certificado:", err)
			os.Exit(1)
		}
		cfg = listenCfg{listen: ":" + port, origin: "https://" + net.JoinHostPort(*public, port), cert: &cert, roots: pool}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(ctx, log, *dsn, *web, cfg, *reset); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("devstack", "err", err)
		os.Exit(1)
	}
}

// listenCfg: onde escutar e o endereço que o navegador usa (origin).
type listenCfg struct {
	listen string
	origin string
	cert   *tls.Certificate // nil = HTTP
	roots  *x509.CertPool   // para a própria API confiar no certificado
}

func run(ctx context.Context, log *slog.Logger, dsn, web string, cfg listenCfg, reset bool) error {
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if reset {
		if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
			return err
		}
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	// Dois "datacenters" simulados.
	cirion := testutil.NewFakePVEServer(tokenID, secret)
	defer cirion.Server.Close()
	ascenty := testutil.NewFakePVEServer(tokenID, secret)
	defer ascenty.Server.Close()

	if err := seed(ctx, pool, cirion, ascenty); err != nil {
		return fmt.Errorf("dados de exemplo: %w", err)
	}

	// Worker + sincronizador.
	os.Setenv("MSPCP_DEMO_PVE_SECRET", secret)
	// PVEs simulados em 127.0.0.1; PROXMOX_ALLOWED_HOSTS libera um Proxmox real
	// para testar o cadastro pela tela.
	allow := proxmox.ParseAllowlist("127.0.0.1," + os.Getenv("PROXMOX_ALLOWED_HOSTS"))
	box, err := demoBox()
	if err != nil {
		return err
	}
	clients := proxmox.NewClients(allow, secrets.Multi{"env": secrets.Env{}}, box)
	worker, err := jobs.NewWorkerClient(pool,
		&jobs.PowerWorker{Pool: pool, Clients: clients, PollInterval: time.Second, Log: log},
		&jobs.VMWorker{Pool: pool, Clients: clients, PollInterval: time.Second, Log: log}, 10)
	if err != nil {
		return err
	}
	if err := worker.Start(ctx); err != nil {
		return err
	}
	defer worker.Stop(context.Background()) //nolint:errcheck
	syncer := &inventory.Syncer{Pool: pool, Clients: clients, Interval: 3 * time.Second, Log: log}
	go syncer.Run(ctx)

	inserter, err := jobs.NewInsertClient(pool)
	if err != nil {
		return err
	}
	srv := &api.Server{
		Pool: pool, River: inserter, Allow: allow, Clients: clients, Log: log, WebDir: web,
		AuthMode: "local", ProductName: "mspcp demo", DemoHint: "Demonstração: usuários vitor, noc, carla, marcos, julia e leo; senha " + demoPassword,
	}
	go serve(ctx, log, cfg, srv.Handler())

	fmt.Printf("\n  Portal:  %s\n", cfg.origin)
	if cfg.cert != nil {
		fmt.Printf("  Certificado autoassinado: aceite o aviso do navegador uma vez.\n")
	}
	fmt.Printf("  Usuários: vitor (admin), noc, carla (MSP Alpha TI), marcos e julia (clientes), leo (leitura)\n")
	fmt.Printf("  Senha de todos: %s\n\n", demoPassword)
	<-ctx.Done()
	return ctx.Err()
}

// demoBox guarda a chave mestra da demo num arquivo temporário, para um
// cluster cadastrado pela tela continuar funcionando depois de reiniciar.
func demoBox() (*secrets.Box, error) {
	path := filepath.Join(os.TempDir(), "mspcp-demo-master.key")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
			return nil, err
		}
	}
	return secrets.LoadBox(path)
}

func serve(ctx context.Context, log *slog.Logger, cfg listenCfg, h http.Handler) {
	hs := &http.Server{Addr: cfg.listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	var err error
	if cfg.cert != nil {
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cfg.cert}, MinVersion: tls.VersionTLS12}
		err = hs.ListenAndServeTLS("", "")
	} else {
		err = hs.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("servidor", "addr", cfg.listen, "err", err)
	}
}

// selfSigned gera, em memória, um certificado autoassinado para o host
// (IP ou nome) e para localhost. Vale só enquanto o processo roda.
func selfSigned(host string) (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "mspcp demo " + host, Organization: []string{"mspcp devstack"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	if ip := net.ParseIP(host); ip != nil {
		tpl.IPAddresses = append(tpl.IPAddresses, ip)
	} else {
		tpl.DNSNames = append(tpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}

func vm(vmid int, kind, node, name, pool, status string, cpu, memGB, diskGB int) map[string]any {
	return map[string]any{"vmid": vmid, "type": kind, "node": node, "name": name, "pool": pool, "status": status,
		"maxcpu": cpu, "maxmem": int64(memGB) << 30, "maxdisk": int64(diskGB) << 30, "template": 0}
}

// seed cria a árvore de exemplo uma vez (passando pelas mesmas regras de
// cota da API) e sempre reconfigura os PVEs simulados, que vivem em memória.
func seed(ctx context.Context, pool *pgxpool.Pool, cirion, ascenty *testutil.FakePVE) error {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return seedTree(ctx, store.New(tx), cirion, ascenty) }); err != nil {
			return err
		}
	} else {
		// Reinício sem -reset: os PVEs simulados mudaram de porta e CA.
		for i, pve := range []*testutil.FakePVE{cirion, ascenty} {
			name := []string{"cirion-01", "ascenty-01"}[i]
			if _, err := pool.Exec(ctx, `UPDATE clusters SET api_url = $2, ca_cert_pem = $3 WHERE name = $1`,
				name, pve.Server.URL, pve.CAPEM()); err != nil {
				return err
			}
		}
	}

	pools := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT t.name || '/' || p.name, p.pool_name FROM projects p JOIN tenants t ON t.id = p.tenant_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		pools[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}

	cirion.SetVMs(
		vm(101, "qemu", "pve-cir-n1", "erp-padaria", pools["Padaria Pão Quente/Produção"], "running", 4, 8, 120),
		vm(102, "qemu", "pve-cir-n2", "pdv-loja-centro", pools["Padaria Pão Quente/Produção"], "running", 2, 4, 60),
		vm(103, "qemu", "pve-cir-n1", "pdv-loja-bairro", pools["Padaria Pão Quente/Produção"], "stopped", 2, 4, 60),
		vm(110, "qemu", "pve-cir-n3", "erp-homolog", pools["Padaria Pão Quente/Homologação"], "stopped", 2, 4, 80),
		vm(201, "qemu", "pve-cir-n2", "fileserver-lima", pools["Lima Advocacia/Principal"], "running", 4, 16, 2048),
		vm(202, "lxc", "pve-cir-n3", "vpn-lima", pools["Lima Advocacia/Principal"], "running", 1, 1, 8),
		vm(150, "qemu", "pve-cir-n1", "monitoramento-alpha", pools["Alpha TI Soluções/Uso interno"], "running", 2, 4, 50),
		vm(9001, "qemu", "pve-cir-n3", "teste-noc", "", "stopped", 1, 2, 20),
		vm(9002, "qemu", "pve-cir-n2", "migrar-cliente-x", "", "running", 2, 4, 40),
		testutil.Template(9100, "pve-cir-n1", "tpl-debian-12", 3),
		testutil.Template(9101, "pve-cir-n1", "tpl-ubuntu-2404", 4),
	)
	ascenty.SetVMs(
		vm(301, "qemu", "pve-asc-n1", "sistema-clinica", pools["Clínica Sorriso/Produção"], "running", 8, 32, 500),
		vm(302, "qemu", "pve-asc-n2", "banco-imagens", pools["Clínica Sorriso/Produção"], "running", 4, 16, 4096),
		vm(303, "lxc", "pve-asc-n2", "dns-clinica", pools["Clínica Sorriso/Produção"], "stopped", 1, 1, 8),
		testutil.Template(9100, "pve-asc-n1", "tpl-debian-12", 3),
	)
	// Os pools dos VDCs já "existem" nos PVEs simulados.
	for _, pve := range []*testutil.FakePVE{cirion, ascenty} {
		for _, p := range pools {
			pve.AddPool(p)
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func seedTree(ctx context.Context, st *store.Store, cirion, ascenty *testutil.FakePVE) error {
	prov, err := st.CreateTenant(ctx, nil, store.KindProvider, "mspclouds")
	if err != nil {
		return err
	}
	ssd, err := st.CreateStorageClass(ctx, "SSD", "Ceph em SSD, uso geral")
	if err != nil {
		return err
	}
	hdd, err := st.CreateStorageClass(ctx, "HDD", "Ceph em HDD, arquivos e backup")
	if err != nil {
		return err
	}
	var clusters []store.Cluster
	for _, c := range []struct {
		name, dc, loc string
		pve           *testutil.FakePVE
	}{{"cirion-01", "Cirion", "São Paulo 1", cirion}, {"ascenty-01", "Ascenty", "São Paulo 2", ascenty}} {
		cl, err := st.CreateCluster(ctx, store.Cluster{Name: c.name, DC: c.dc, Location: c.loc, APIURL: c.pve.Server.URL,
			CACertPEM: c.pve.CAPEM(), TokenID: tokenID, TokenSecretRef: "env:MSPCP_DEMO_PVE_SECRET", CPURatio: 4})
		if err != nil {
			return err
		}
		if err := st.SetStoragePolicy(ctx, cl.ID, ssd.ID, "ceph-ssd"); err != nil {
			return err
		}
		if err := st.SetStoragePolicy(ctx, cl.ID, hdd.ID, "ceph-hdd"); err != nil {
			return err
		}
		clusters = append(clusters, cl)
	}
	cir, asc := clusters[0].ID, clusters[1].ID

	// MSPs: um parceiro externo e o MSP interno que atende os clientes diretos.
	alpha, err := st.CreateTenant(ctx, &prov.ID, store.KindPartner, "Alpha TI Soluções")
	if err != nil {
		return err
	}
	direto, err := st.CreateTenant(ctx, &prov.ID, store.KindPartner, "mspclouds Direto")
	if err != nil {
		return err
	}
	if err := quota.SetLimits(ctx, st, alpha, store.MSPLimits{MaxCustomers: ptr(10), MaxVDCs: ptr(20), MaxVMs: ptr(100)}); err != nil {
		return err
	}
	// Recursos reservados para cada MSP em cada cluster.
	for _, rv := range []struct {
		msp      store.Tenant
		cluster  store.Cluster
		cpu      int
		memGB    int64
		ssd, hdd int64
	}{
		{alpha, clusters[0], 40, 112, 1500, 6000},
		{alpha, clusters[1], 8, 16, 500, 0},
		{direto, clusters[0], 16, 32, 500, 0},
		{direto, clusters[1], 48, 192, 2000, 8000},
	} {
		res := store.Reservation{CPUCores: rv.cpu, MemoryMB: rv.memGB * 1024, Storage: map[uuid.UUID]int64{ssd.ID: rv.ssd}}
		if rv.hdd > 0 {
			res.Storage[hdd.ID] = rv.hdd
		}
		if err := quota.SetReservation(ctx, st, rv.msp, rv.cluster, res); err != nil {
			return fmt.Errorf("reserva %s/%s: %w", rv.msp.Name, rv.cluster.Name, err)
		}
	}
	padaria, err := st.CreateTenant(ctx, &alpha.ID, store.KindCustomer, "Padaria Pão Quente")
	if err != nil {
		return err
	}
	lima, err := st.CreateTenant(ctx, &alpha.ID, store.KindCustomer, "Lima Advocacia")
	if err != nil {
		return err
	}
	clinica, err := st.CreateTenant(ctx, &direto.ID, store.KindCustomer, "Clínica Sorriso")
	if err != nil {
		return err
	}

	vdc := func(t store.Tenant, name string, cluster uuid.UUID, cpu int, memGB int64, ssdGB, hddGB int64) (store.Project, error) {
		spec := store.VDCSpec{TenantID: t.ID, Name: name, ClusterID: cluster, CPUCores: cpu, MemoryMB: memGB * 1024,
			Storage: map[uuid.UUID]int64{ssd.ID: ssdGB}}
		if hddGB > 0 {
			spec.Storage[hdd.ID] = hddGB
		}
		if err := quota.CheckVDC(ctx, st, t, spec, nil); err != nil {
			return store.Project{}, fmt.Errorf("VDC %s/%s: %w", t.Name, name, err)
		}
		return st.CreateProject(ctx, spec)
	}
	padProd, err := vdc(padaria, "Produção", cir, 6, 16, 400, 0)
	if err != nil {
		return err
	}
	for _, v := range []struct {
		t       store.Tenant
		name    string
		cluster uuid.UUID
		cpu     int
		mem     int64
		ssd     int64
		hdd     int64
	}{
		{padaria, "Homologação", cir, 4, 8, 200, 0},
		{lima, "Principal", cir, 8, 24, 200, 3072},
		{alpha, "Uso interno", cir, 4, 8, 100, 0},
		{clinica, "Produção", asc, 16, 64, 1024, 6144},
	} {
		if _, err := vdc(v.t, v.name, v.cluster, v.cpu, v.mem, v.ssd, v.hdd); err != nil {
			return err
		}
	}

	hash, err := localauth.HashPassword(demoPassword)
	if err != nil {
		return err
	}
	for _, u := range users {
		if _, err := st.CreateUser(ctx, u.username, u.name, u.email, hash, false); err != nil {
			return err
		}
	}
	// Catálogo: os templates simulados publicados como imagens.
	for _, img := range []struct {
		cluster uuid.UUID
		vmid    int
		name    string
		disk    int
	}{{cir, 9100, "Debian 12", 10}, {cir, 9101, "Ubuntu 24.04 LTS", 12}, {asc, 9100, "Debian 12", 10}} {
		if _, err := st.CreateImage(ctx, img.cluster, img.vmid, img.name, "linux", img.disk); err != nil {
			return err
		}
	}

	for _, m := range []struct {
		sub    string
		tenant store.Tenant
		proj   *uuid.UUID
		role   string
	}{
		{"vitor", prov, nil, rbac.RoleSuperAdmin},
		{"noc", prov, nil, rbac.RoleNOCOperator},
		{"carla", alpha, nil, rbac.RolePartnerAdmin},
		{"marcos", padaria, nil, rbac.RoleCustomerAdmin},
		{"julia", clinica, nil, rbac.RoleCustomerAdmin},
		{"leo", padaria, &padProd.ID, rbac.RoleViewer},
	} {
		if err := st.CreateMembership(ctx, m.sub, m.tenant.ID, m.proj, m.role); err != nil {
			return err
		}
	}
	return nil
}
