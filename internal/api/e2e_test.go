package api_test

// Teste ponta a ponta da Fase 0 contra PostgreSQL real, PVE falso em TLS e
// IdP falso. Roda só com TEST_DATABASE_URL definido (o banco é recriado).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vitordatasafer/mspvdc/internal/api"
	"github.com/vitordatasafer/mspvdc/internal/auth"
	"github.com/vitordatasafer/mspvdc/internal/db"
	"github.com/vitordatasafer/mspvdc/internal/inventory"
	"github.com/vitordatasafer/mspvdc/internal/jobs"
	"github.com/vitordatasafer/mspvdc/internal/proxmox"
	"github.com/vitordatasafer/mspvdc/internal/rbac"
	"github.com/vitordatasafer/mspvdc/internal/secrets"
	"github.com/vitordatasafer/mspvdc/internal/store"
	"github.com/vitordatasafer/mspvdc/internal/testutil"
)

type env struct {
	t    *testing.T
	srv  *httptest.Server
	idp  *testutil.FakeOIDC
	pool *pgxpool.Pool
}

func (e *env) call(sub, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if sub != "" {
		req.Header.Set("Authorization", "Bearer "+e.idp.Token(e.t, sub, time.Minute))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) must(want int, sub, method, path string, body any, out any) []byte {
	e.t.Helper()
	code, b := e.call(sub, method, path, body)
	if code != want {
		e.t.Fatalf("%s %s como %q: esperava %d, veio %d: %s", method, path, sub, want, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("json: %v: %s", err, b)
		}
	}
	return b
}

type resList struct {
	Resources []store.Resource `json:"resources"`
}

func aliases(rs []store.Resource) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Alias)
	}
	return strings.Join(s, ",")
}

func TestPhase0EndToEnd(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL não definido")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil { // idempotente
		t.Fatalf("segunda migração: %v", err)
	}

	// Bootstrap: provedor + super_admin.
	st := store.New(pool)
	prov, err := st.CreateTenant(ctx, nil, store.KindProvider, "mspclouds")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMembership(ctx, "admin", prov.ID, nil, rbac.RoleSuperAdmin); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMembership(ctx, "noc", prov.ID, nil, rbac.RoleNOCOperator); err != nil {
		t.Fatal(err)
	}

	// Dublês e serviço.
	t.Setenv("TEST_PVE_SECRET", "s3cr3t")
	pve := testutil.NewFakePVE(t, "portal@pve!mspvdc", "s3cr3t")
	idp := testutil.NewFakeOIDC(t, "mspvdc")
	allow := proxmox.ParseAllowlist("127.0.0.1")
	clients := proxmox.NewClients(allow, secrets.Multi{"env": secrets.Env{}}, secrets.NewRandomBox())
	rc, err := jobs.NewInsertClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{Pool: pool, Verifier: auth.NewOIDCVerifier(idp.Issuer, "mspvdc", nil), River: rc, Allow: allow, Log: log}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e := &env{t: t, srv: srv, idp: idp, pool: pool}

	// ---- autenticação ----
	if code, _ := e.call("", "GET", "/v1/me", nil); code != 401 {
		t.Fatalf("sem token: esperava 401, veio %d", code)
	}
	if code, _ := e.call("desconhecido", "GET", "/v1/tenants", nil); code != 403 {
		t.Fatalf("usuário sem vínculo: esperava 403, veio %d", code)
	}

	// ---- clusters, localizações e storage (só a equipe) ----
	pve2 := testutil.NewFakePVE(t, "portal@pve!mspvdc", "s3cr3t")
	clusterIn := map[string]any{"name": "cirion-01", "dc": "Cirion", "location": "São Paulo 1", "api_url": pve.Server.URL,
		"ca_cert_pem": pve.CAPEM(), "token_id": "portal@pve!mspvdc", "token_secret_ref": "env:TEST_PVE_SECRET"}
	e.must(404, "noc", "POST", "/v1/clusters", clusterIn, nil)
	bad := map[string]any{}
	for k, v := range clusterIn {
		bad[k] = v
	}
	bad["api_url"] = "https://169.254.169.254"
	e.must(400, "admin", "POST", "/v1/clusters", bad, nil)
	var c1, c2 store.Cluster
	e.must(201, "admin", "POST", "/v1/clusters", clusterIn, &c1)
	clusterIn2 := map[string]any{"name": "ascenty-01", "dc": "Ascenty", "location": "São Paulo 2", "api_url": pve2.Server.URL,
		"ca_cert_pem": pve2.CAPEM(), "token_id": "portal@pve!mspvdc", "token_secret_ref": "env:TEST_PVE_SECRET"}
	e.must(201, "admin", "POST", "/v1/clusters", clusterIn2, &c2)
	e.must(200, "noc", "GET", "/v1/clusters", nil, nil)
	var ssd store.StorageClass
	e.must(201, "admin", "POST", "/v1/storage-classes", map[string]any{"name": "SSD"}, &ssd)
	e.must(404, "noc", "POST", "/v1/storage-classes", map[string]any{"name": "NVMe"}, nil)
	for _, c := range []store.Cluster{c1, c2} {
		e.must(204, "admin", "PUT", "/v1/clusters/"+c.ID.String()+"/storage-policies", map[string]any{"class_id": ssd.ID, "pve_storage": "ceph-ssd"}, nil)
	}

	// ---- árvore: provedor > MSP P > cliente A; provedor > MSP interno Q > cliente B ----
	var P, Q, A, B store.Tenant
	e.must(201, "admin", "POST", "/v1/tenants", map[string]any{"parent_id": prov.ID, "kind": "partner", "name": "MSP P"}, &P)
	e.must(201, "admin", "POST", "/v1/tenants", map[string]any{"parent_id": prov.ID, "kind": "partner", "name": "MSP interno Q"}, &Q)
	e.must(201, "admin", "POST", "/v1/tenants", map[string]any{"parent_id": P.ID, "kind": "customer", "name": "Cliente A"}, &A)
	e.must(201, "admin", "POST", "/v1/tenants", map[string]any{"parent_id": Q.ID, "kind": "customer", "name": "Cliente B"}, &B)
	e.must(400, "admin", "POST", "/v1/tenants", map[string]any{"parent_id": prov.ID, "kind": "customer", "name": "direto"}, nil)
	e.must(400, "admin", "POST", "/v1/tenants", map[string]any{"parent_id": A.ID, "kind": "customer", "name": "neto"}, nil)

	// ---- cota dos MSPs: global, com localizações liberadas ----
	// Reserva do MSP num cluster (vCPU, memória e storage) e limite global
	// de clientes.
	reservation := func(cpu int) map[string]any {
		return map[string]any{"cpu_cores": cpu, "memory_mb": 16384,
			"storage": []map[string]any{{"class_id": ssd.ID, "capacity_gb": 100}}}
	}
	reserve := func(code int, sub string, msp store.Tenant, cluster store.Cluster, cpu int) {
		e.t.Helper()
		e.must(code, sub, "PUT", "/v1/clusters/"+cluster.ID.String()+"/reservations/"+msp.ID.String(), reservation(cpu), nil)
	}
	vdc := func(name string, loc uuid.UUID, cpu int, memMB int64, ssdGB int64) map[string]any {
		return map[string]any{"name": name, "location_id": loc, "cpu_cores": cpu, "memory_mb": memMB,
			"storage": []map[string]any{{"class_id": ssd.ID, "capacity_gb": ssdGB}}}
	}
	// MSP sem cota não distribui nada.
	e.must(422, "admin", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("producao", c1.ID, 4, 4096, 50), nil)
	for _, m := range []store.Tenant{P, Q} {
		e.must(204, "admin", "PUT", "/v1/tenants/"+m.ID.String()+"/limits", map[string]any{"max_customers": 2}, nil)
		reserve(204, "admin", m, c1, 10)
	}

	var projA, projB struct {
		Project store.Project `json:"project"`
		Pool    string        `json:"proxmox_pool"`
	}
	e.must(201, "admin", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("producao", c1.ID, 4, 4096, 50), &projA)
	e.must(201, "admin", "POST", "/v1/tenants/"+B.ID.String()+"/projects", vdc("producao", c1.ID, 4, 4096, 50), &projB)
	if projA.Pool == "" || projB.Pool == "" {
		t.Fatal("super_admin deveria receber o nome do pool")
	}
	if projA.Project.Location != "São Paulo 1" {
		t.Fatalf("VDC deveria mostrar a localização, veio %q", projA.Project.Location)
	}
	// Localização não liberada, CPU acima do pool e storage acima do pool.
	e.must(422, "admin", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("dr", c2.ID, 1, 1024, 10), nil)
	e.must(422, "admin", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("grande", c1.ID, 7, 1024, 10), nil)
	e.must(422, "admin", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("disco", c1.ID, 1, 1024, 60), nil)

	for _, m := range []struct {
		tenant, sub, role string
		project           any
	}{
		{P.ID.String(), "pat", rbac.RolePartnerAdmin, nil},
		{A.ID.String(), "alice", rbac.RoleCustomerAdmin, nil},
		{B.ID.String(), "bob", rbac.RoleCustomerAdmin, nil},
		{A.ID.String(), "vic", rbac.RoleViewer, projA.Project.ID},
	} {
		e.must(201, "admin", "POST", "/v1/tenants/"+m.tenant+"/memberships", map[string]any{"subject": m.sub, "role": m.role, "project_id": m.project}, nil)
	}
	// Escalada de privilégio recusada.
	e.must(400, "alice", "POST", "/v1/tenants/"+A.ID.String()+"/memberships", map[string]any{"subject": "x", "role": "super_admin"}, nil)
	e.must(404, "alice", "POST", "/v1/tenants/"+B.ID.String()+"/memberships", map[string]any{"subject": "x", "role": "operator"}, nil)
	e.must(201, "alice", "POST", "/v1/tenants/"+A.ID.String()+"/memberships", map[string]any{"subject": "oscar", "role": "operator"}, nil)

	// MSP: cria clientes sob si (até o limite), nunca sob o provedor.
	e.must(201, "pat", "POST", "/v1/tenants", map[string]any{"parent_id": P.ID, "kind": "customer", "name": "Cliente C"}, nil)
	e.must(422, "pat", "POST", "/v1/tenants", map[string]any{"parent_id": P.ID, "kind": "customer", "name": "Cliente D"}, nil)
	e.must(404, "pat", "POST", "/v1/tenants", map[string]any{"parent_id": prov.ID, "kind": "customer", "name": "X"}, nil)

	// Cliente não cria VDC; o MSP cria para o cliente e para uso próprio.
	e.must(403, "alice", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("homolog", c1.ID, 1, 1024, 10), nil)
	var projA2 map[string]any
	e.must(201, "pat", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc("homolog", c1.ID, 2, 2048, 10), &projA2)
	if _, ok := projA2["proxmox_pool"]; ok {
		t.Fatal("MSP não deveria ver o nome do pool do Proxmox")
	}
	e.must(201, "pat", "POST", "/v1/tenants/"+P.ID.String()+"/projects", vdc("interno", c1.ID, 2, 2048, 10), nil)

	// Limites: só o admin define; nunca abaixo do distribuído.
	e.must(403, "pat", "PUT", "/v1/tenants/"+P.ID.String()+"/limits", map[string]any{"max_customers": 50}, nil)
	reserve(404, "pat", P, c1, 50)
	reserve(422, "admin", P, c1, 6)                                                                         // 8 distribuídos
	e.must(422, "admin", "DELETE", "/v1/clusters/"+c1.ID.String()+"/reservations/"+P.ID.String(), nil, nil) // há VDCs
	e.must(422, "admin", "PUT", "/v1/tenants/"+P.ID.String()+"/limits", map[string]any{"max_customers": 1}, nil)
	reserve(204, "admin", P, c1, 12)
	reserve(204, "admin", P, c2, 12)
	e.must(404, "admin", "PUT", "/v1/clusters/"+c1.ID.String()+"/reservations/"+A.ID.String(), reservation(1), nil) // cliente não recebe reserva

	// Capacidade: o MSP vê seu pool e as localizações, nunca o cluster.
	var capP struct {
		Usage     store.MSPUsage   `json:"usage"`
		Locations []map[string]any `json:"locations"`
	}
	capBody := e.must(200, "pat", "GET", "/v1/tenants/"+P.ID.String()+"/capacity", nil, &capP)
	if capP.Usage.AllocatedCPU != 8 || capP.Usage.Customers != 2 || capP.Usage.VDCs != 3 || len(capP.Locations) != 2 {
		t.Fatalf("capacidade do MSP inesperada: %+v", capP)
	}
	for _, leak := range []string{"cirion-01", "ascenty-01", "127.0.0.1", "ceph-ssd", "pool"} {
		if strings.Contains(string(capBody), leak) {
			t.Errorf("capacidade vaza %q para o MSP: %s", leak, capBody)
		}
	}
	e.must(404, "alice", "GET", "/v1/tenants/"+P.ID.String()+"/capacity", nil, nil)
	e.must(404, "pat", "GET", "/v1/clusters", nil, nil)
	e.must(404, "pat", "GET", "/v1/storage-policies", nil, nil)

	// ---- sincronização ----
	pve.SetVMs(
		testutil.VM(101, "pve-cirion-n1", "web-a", projA.Pool, "stopped"),
		testutil.VM(104, "pve-cirion-n1", "web-a2", projA.Pool, "stopped"),
		testutil.VM(102, "pve-cirion-n2", "db-b", projB.Pool, "running"),
		testutil.VM(103, "pve-cirion-n2", "sem-dono", "", "running"),
	)
	syncer := &inventory.Syncer{Pool: pool, Clients: clients, Interval: time.Hour, Log: log}
	syncer.SyncAll(ctx)

	var la, lb, lp, lv, lad resList
	bodyA := e.must(200, "alice", "GET", "/v1/resources", nil, &la)
	e.must(200, "bob", "GET", "/v1/resources", nil, &lb)
	e.must(200, "pat", "GET", "/v1/resources", nil, &lp)
	e.must(200, "vic", "GET", "/v1/resources", nil, &lv)
	e.must(200, "admin", "GET", "/v1/resources", nil, &lad)
	for name, got := range map[string]string{
		"alice": aliases(la.Resources), "bob": aliases(lb.Resources), "pat": aliases(lp.Resources),
		"vic": aliases(lv.Resources), "admin": aliases(lad.Resources),
	} {
		want := map[string]string{"alice": "web-a,web-a2", "bob": "db-b", "pat": "web-a,web-a2", "vic": "web-a,web-a2", "admin": "db-b,sem-dono,web-a,web-a2"}[name]
		if got != want {
			t.Errorf("%s vê %q, esperava %q", name, got, want)
		}
	}
	// Nada de topologia na resposta.
	for _, leak := range []string{"vmid", "node", "pve-cirion", "cluster", "\"pool\""} {
		if strings.Contains(string(bodyA), leak) {
			t.Errorf("resposta do tenant vaza %q: %s", leak, bodyA)
		}
	}

	resA := la.Resources[0].ID.String()
	resB := lb.Resources[0].ID.String()

	// ---- isolamento: B não enxerga nem age em A ----
	e.must(404, "bob", "GET", "/v1/resources/"+resA, nil, nil)
	e.must(404, "bob", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "stop"}, nil)
	e.must(404, "pat", "GET", "/v1/resources/"+resB, nil, nil)
	e.must(403, "vic", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "start"}, nil)
	e.must(400, "alice", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "destroy"}, nil)
	e.must(400, "alice", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "start", "node": "x"}, nil)

	// ---- start ponta a ponta ----
	var task store.Task
	e.must(202, "alice", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "start"}, &task)
	e.must(409, "alice", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "stop"}, nil)
	// Cota do VDC: 4 GB. web-a (4 GB) já tem start na fila; web-a2 não cabe.
	resA2 := la.Resources[1].ID.String()
	quotaBody := e.must(422, "alice", "POST", "/v1/resources/"+resA2+"/actions", map[string]any{"action": "start"}, nil)
	if !strings.Contains(string(quotaBody), "cota de memória") {
		t.Fatalf("esperava recusa por cota de memória: %s", quotaBody)
	}
	e.must(404, "bob", "GET", "/v1/tasks/"+task.ID.String(), nil, nil)

	worker, err := jobs.NewWorkerClient(pool, &jobs.PowerWorker{Pool: pool, Clients: clients, PollInterval: 20 * time.Millisecond, Log: log}, &jobs.VMWorker{Pool: pool, Clients: clients, PollInterval: 20 * time.Millisecond, Log: log}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop(context.Background()) //nolint:errcheck

	waitTask := func(id string) store.Task {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var tk store.Task
			e.must(200, "alice", "GET", "/v1/tasks/"+id, nil, &tk)
			if tk.Status == "succeeded" || tk.Status == "failed" {
				return tk
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("task %s não terminou", id)
		return store.Task{}
	}
	if got := waitTask(task.ID.String()); got.Status != "succeeded" {
		t.Fatalf("task falhou: %+v", got)
	}
	if calls := pve.Calls(); len(calls) != 1 || calls[0] != "POST pve-cirion-n1/qemu/101/start" {
		t.Fatalf("chamadas ao PVE: %v", calls)
	}
	var r store.Resource
	e.must(200, "alice", "GET", "/v1/resources/"+resA, nil, &r)
	if r.Status != "running" {
		t.Fatalf("status deveria ser running, veio %s", r.Status)
	}
	// Reduzir a cota abaixo do que está ligado é recusado.
	e.must(422, "pat", "PUT", "/v1/projects/"+projA.Project.ID.String()+"/quota", vdc("", c1.ID, 1, 4096, 50), nil)
	e.must(403, "alice", "PUT", "/v1/projects/"+projA.Project.ID.String()+"/quota", vdc("", c1.ID, 4, 8192, 50), nil)
	var resized store.Project
	e.must(200, "pat", "PUT", "/v1/projects/"+projA.Project.ID.String()+"/quota", vdc("", c1.ID, 4, 8192, 50), &resized)
	if resized.MemoryMB != 8192 || resized.Usage == nil || resized.Usage.RunningCPU != 2 {
		t.Fatalf("VDC ajustado inesperado: %+v", resized)
	}

	// ---- sem overcommit mesmo com pedidos simultâneos ----
	// P: 12 vCPU, 8 distribuídos. Cinco pedidos de 2 vCPU ao mesmo tempo:
	// exatamente dois cabem.
	codes := make(chan int, 5)
	for i := range 5 {
		go func() {
			code, _ := e.call("pat", "POST", "/v1/tenants/"+A.ID.String()+"/projects", vdc(fmt.Sprintf("paralelo-%d", i), c1.ID, 2, 1024, 5))
			codes <- code
		}()
	}
	created := 0
	for range 5 {
		switch c := <-codes; c {
		case 201:
			created++
		case 422:
		default:
			t.Fatalf("criação simultânea: status inesperado %d", c)
		}
	}
	if created != 2 {
		t.Fatalf("esperava 2 VDCs criados em paralelo, vieram %d", created)
	}

	// ---- listas de VDCs e tarefas respeitam o escopo ----
	type projList struct {
		Projects []store.Project `json:"projects"`
	}
	var vb, vv, vp projList
	e.must(200, "bob", "GET", "/v1/projects", nil, &vb)
	e.must(200, "vic", "GET", "/v1/projects", nil, &vv)
	e.must(200, "pat", "GET", "/v1/projects", nil, &vp)
	if len(vb.Projects) != 1 || vb.Projects[0].TenantID != B.ID {
		t.Fatalf("bob deveria ver só o VDC do cliente B: %+v", vb.Projects)
	}
	if len(vv.Projects) != 1 || vv.Projects[0].ID != projA.Project.ID {
		t.Fatalf("vic deveria ver só o VDC dele: %+v", vv.Projects)
	}
	for _, p := range vp.Projects {
		if p.TenantID == B.ID {
			t.Fatal("MSP P não pode ver VDC do cliente do MSP Q")
		}
	}
	type taskList struct {
		Tasks []store.TaskView `json:"tasks"`
	}
	var ta, tb taskList
	e.must(200, "alice", "GET", "/v1/tasks", nil, &ta)
	e.must(200, "bob", "GET", "/v1/tasks?limit=5", nil, &tb)
	if len(ta.Tasks) == 0 || ta.Tasks[0].ResourceAlias == "" {
		t.Fatalf("alice deveria ver as tarefas dela: %+v", ta.Tasks)
	}
	if len(tb.Tasks) != 0 {
		t.Fatalf("bob não pode ver tarefas do cliente A: %+v", tb.Tasks)
	}

	// ---- falha reportada pelo PVE ----
	pve.FailNextTask("VM is locked (backup)")
	e.must(202, "alice", "POST", "/v1/resources/"+resA+"/actions", map[string]any{"action": "shutdown"}, &task)
	if got := waitTask(task.ID.String()); got.Status != "failed" || got.Error == nil || !strings.Contains(*got.Error, "locked") {
		t.Fatalf("esperava falha com motivo, veio %+v", got)
	}

	// ---- NOC age em tenant de terceiro e fica auditado ----
	e.must(202, "noc", "POST", "/v1/resources/"+resB+"/actions", map[string]any{"action": "reboot"}, nil)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE subject = 'noc' AND action = 'resource.reboot' AND target_tenant_id = $1`, B.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ação do NOC deveria estar auditada (n=%d, err=%v)", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET subject = 'x'`); err == nil {
		t.Fatal("audit_log deveria ser append-only")
	}

	// ---- VM removida do cluster vira órfã e some do portal ----
	pve.SetVMs(testutil.VM(101, "pve-cirion-n1", "web-a", projA.Pool, "running"))
	syncer.SyncAll(ctx)
	e.must(200, "bob", "GET", "/v1/resources", nil, &lb)
	if len(lb.Resources) != 0 {
		t.Fatalf("bob deveria ficar sem recursos, vê %s", aliases(lb.Resources))
	}
	e.must(404, "bob", "GET", "/v1/resources/"+resB, nil, nil)
}
