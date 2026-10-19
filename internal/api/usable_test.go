package api_test

// Ponta a ponta da versão utilizável: login local com 2FA, gestão de
// usuários, catálogo de imagens, criar/excluir/atribuir VM e console,
// contra PostgreSQL real e o PVE falso.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/mspclouds/mspcp/internal/api"
	"github.com/mspclouds/mspcp/internal/db"
	"github.com/mspclouds/mspcp/internal/inventory"
	"github.com/mspclouds/mspcp/internal/jobs"
	"github.com/mspclouds/mspcp/internal/localauth"
	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/rbac"
	"github.com/mspclouds/mspcp/internal/secrets"
	"github.com/mspclouds/mspcp/internal/store"
	"github.com/mspclouds/mspcp/internal/testutil"
)

type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar}}
}

func (b *browser) do(method, path string, body any, csrf bool) (int, []byte) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-MSPCP", "1")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (b *browser) must(want int, method, path string, body any, out any) []byte {
	b.t.Helper()
	code, raw := b.do(method, path, body, true)
	if code != want {
		b.t.Fatalf("%s %s: esperava %d, veio %d: %s", method, path, want, code, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			b.t.Fatalf("json: %v: %s", err, raw)
		}
	}
	return raw
}

func (b *browser) login(user, pass string) {
	b.t.Helper()
	b.must(200, "POST", "/auth/login", map[string]any{"username": user, "password": pass}, nil)
}

func TestUsableEndToEnd(t *testing.T) {
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

	// Bootstrap como o CLI faz: provedor + admin local com senha temporária.
	st := store.New(pool)
	prov, err := st.CreateTenant(ctx, nil, store.KindProvider, "mspclouds")
	if err != nil {
		t.Fatal(err)
	}
	temp := localauth.TempPassword()
	hash, _ := localauth.HashPassword(temp)
	if _, err := st.CreateUser(ctx, "admin", "Administrador", "", hash, true); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMembership(ctx, "admin", prov.ID, nil, rbac.RoleSuperAdmin); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_PVE_SECRET", "s3cr3t")
	pve := testutil.NewFakePVE(t, "portal@pve!mspcp", "s3cr3t")
	allow := proxmox.ParseAllowlist("127.0.0.1")
	clients := proxmox.NewClients(allow, secrets.Multi{"env": secrets.Env{}}, secrets.NewRandomBox())
	rc, err := jobs.NewInsertClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{Pool: pool, River: rc, Allow: allow, Clients: clients, Log: log, AuthMode: "local"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	worker, err := jobs.NewWorkerClient(pool,
		&jobs.PowerWorker{Pool: pool, Clients: clients, PollInterval: 10 * time.Millisecond, Log: log},
		&jobs.VMWorker{Pool: pool, Clients: clients, PollInterval: 10 * time.Millisecond, Log: log}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop(context.Background()) //nolint:errcheck
	syncer := &inventory.Syncer{Pool: pool, Clients: clients, Interval: time.Hour, Log: log}

	// ---- login, CSRF, senha temporária ----
	adm := newBrowser(t, srv.URL)
	var cfg map[string]string
	adm.must(200, "GET", "/config.json", nil, &cfg)
	if cfg["auth"] != "local" {
		t.Fatalf("config.json deveria anunciar login local: %v", cfg)
	}
	if code, _ := adm.do("POST", "/auth/login", map[string]any{"username": "admin", "password": temp}, false); code != 403 {
		t.Fatalf("login sem X-MSPCP deveria ser recusado, veio %d", code)
	}
	adm.must(401, "POST", "/auth/login", map[string]any{"username": "admin", "password": "errada-errada"}, nil)
	adm.must(401, "POST", "/auth/login", map[string]any{"username": "ninguem", "password": "errada-errada"}, nil)
	var lr map[string]any
	adm.must(200, "POST", "/auth/login", map[string]any{"username": "ADMIN", "password": temp}, &lr)
	if lr["must_change_password"] != true {
		t.Fatalf("senha temporária deveria exigir troca: %v", lr)
	}
	adm.must(403, "GET", "/v1/tenants", nil, nil) // bloqueado até trocar
	var me map[string]any
	adm.must(200, "GET", "/v1/me", nil, &me)
	if me["must_change_password"] != true || me["local"] != true {
		t.Fatalf("/me inesperado: %v", me)
	}
	adm.must(422, "PUT", "/v1/me/password", map[string]any{"current_password": temp, "new_password": "curta"}, nil)
	adm.must(422, "PUT", "/v1/me/password", map[string]any{"current_password": "x", "new_password": "admin-senha-nova-1"}, nil)
	adm.must(204, "PUT", "/v1/me/password", map[string]any{"current_password": temp, "new_password": "admin-senha-nova-1"}, nil)
	adm.must(200, "GET", "/v1/tenants", nil, nil)
	if code, _ := adm.do("PUT", "/v1/me/password", map[string]any{"current_password": "a", "new_password": "b"}, false); code != 403 {
		t.Fatalf("mudança sem X-MSPCP deveria ser recusada, veio %d", code)
	}

	// ---- 2FA ----
	var setup map[string]string
	adm.must(200, "POST", "/v1/me/totp/setup", nil, &setup)
	if !strings.HasPrefix(setup["uri"], "otpauth://totp/") {
		t.Fatalf("URI TOTP inesperada: %v", setup)
	}
	adm.must(422, "POST", "/v1/me/totp/enable", map[string]any{"code": "000000"}, nil)
	adm.must(204, "POST", "/v1/me/totp/enable", map[string]any{"code": localauth.TOTPCode(setup["secret"], time.Now())}, nil)
	adm.must(204, "POST", "/auth/logout", nil, nil)
	adm.must(401, "GET", "/v1/me", nil, nil)
	adm.must(200, "POST", "/auth/login", map[string]any{"username": "admin", "password": "admin-senha-nova-1"}, &lr)
	if lr["need_totp"] != true {
		t.Fatalf("deveria pedir o código: %v", lr)
	}
	adm.must(401, "POST", "/auth/login", map[string]any{"username": "admin", "password": "admin-senha-nova-1", "totp": "123456"}, nil)
	adm.must(200, "POST", "/auth/login", map[string]any{"username": "admin", "password": "admin-senha-nova-1",
		"totp": localauth.TOTPCode(setup["secret"], time.Now())}, nil)
	adm.must(200, "GET", "/v1/tenants", nil, nil)

	// Limitador: 5 falhas seguidas bloqueiam o par IP+usuário.
	att := newBrowser(t, srv.URL)
	for i := 0; i < 5; i++ {
		att.must(401, "POST", "/auth/login", map[string]any{"username": "admin", "password": "chute-errado"}, nil)
	}
	att.must(429, "POST", "/auth/login", map[string]any{"username": "admin", "password": "admin-senha-nova-1"}, nil)

	// ---- área de clusters: testar, cadastrar (segredo cifrado), reservar ----
	clusterForm := map[string]any{"name": "lab-01", "dc": "Lab", "location": "Laboratório",
		"api_url": pve.Server.URL, "token_id": "portal@pve!mspcp", "token_secret": "s3cr3t", "default_bridge": "vmbr1"}
	var probe struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		CA       string `json:"ca_cert_pem"`
		Version  string `json:"version"`
		CPUTotal int    `json:"cpu_total"`
		Storages []store.ClusterStorage
		Bridges  []store.ClusterBridge
		TLS      struct {
			TrustedBySystem bool `json:"trusted_by_system"`
		}
		Missing []string `json:"missing_privileges"`
	}
	bad := map[string]any{"api_url": pve.Server.URL, "token_id": "portal@pve!mspcp", "token_secret": "errado"}
	adm.must(200, "POST", "/v1/clusters/probe", bad, &probe)
	if probe.OK || !strings.Contains(probe.Error, "token") {
		t.Fatalf("token errado deveria falhar no teste: %+v", probe)
	}
	adm.must(200, "POST", "/v1/clusters/probe", clusterForm, &probe)
	if !probe.OK || probe.TLS.TrustedBySystem || !strings.Contains(probe.CA, "BEGIN CERTIFICATE") || probe.Version == "" ||
		probe.CPUTotal == 0 || len(probe.Storages) != 3 || len(probe.Bridges) != 3 || len(probe.Missing) != 0 {
		t.Fatalf("teste do cluster inesperado: %+v", probe)
	}
	badForm := map[string]any{}
	for k, v := range clusterForm {
		badForm[k] = v
	}
	badForm["token_secret"] = "errado"
	adm.must(422, "POST", "/v1/clusters", badForm, nil) // não grava cluster que não conecta
	var c1 store.Cluster
	raw0 := adm.must(201, "POST", "/v1/clusters", clusterForm, &c1)
	if strings.Contains(string(raw0), "s3cr3t") || c1.CPUTotal == 0 || c1.PVEVersion == "" {
		t.Fatalf("cadastro inesperado: %s", raw0)
	}
	var enc []byte
	if err := pool.QueryRow(ctx, `SELECT token_secret_enc FROM clusters WHERE id = $1`, c1.ID).Scan(&enc); err != nil ||
		len(enc) == 0 || strings.Contains(string(enc), "s3cr3t") {
		t.Fatalf("segredo deveria estar cifrado no banco (err=%v)", err)
	}
	var ssd store.StorageClass
	adm.must(201, "POST", "/v1/storage-classes", map[string]any{"name": "SSD"}, &ssd)
	adm.must(204, "PUT", "/v1/clusters/"+c1.ID.String()+"/storage-policies", map[string]any{"class_id": ssd.ID, "pve_storage": "ceph-ssd"}, nil)
	var msp, cli store.Tenant
	adm.must(201, "POST", "/v1/tenants", map[string]any{"parent_id": prov.ID, "kind": "partner", "name": "MSP Lab"}, &msp)
	adm.must(201, "POST", "/v1/tenants", map[string]any{"parent_id": msp.ID, "kind": "customer", "name": "Cliente Lab"}, &cli)
	adm.must(204, "PUT", "/v1/tenants/"+msp.ID.String()+"/limits", map[string]any{"max_vms": 4}, nil)
	resPath := "/v1/clusters/" + c1.ID.String() + "/reservations/" + msp.ID.String()
	// Acima da capacidade física do cluster (1 node: 32 threads, 128 GB; SSD 4 TB).
	adm.must(422, "PUT", resPath, map[string]any{"cpu_cores": 40, "memory_mb": 8192}, nil)
	adm.must(422, "PUT", resPath, map[string]any{"cpu_cores": 8, "memory_mb": 200 * 1024}, nil)
	adm.must(422, "PUT", resPath, map[string]any{"cpu_cores": 8, "memory_mb": 8192,
		"storage": []map[string]any{{"class_id": ssd.ID, "capacity_gb": 5000}}}, nil)
	// Com razão 2 de vCPU, 40 cabe.
	adm.must(200, "PUT", "/v1/clusters/"+c1.ID.String(), map[string]any{"cpu_ratio": 2}, nil)
	adm.must(204, "PUT", resPath, map[string]any{"cpu_cores": 40, "memory_mb": 32768,
		"storage": []map[string]any{{"class_id": ssd.ID, "capacity_gb": 500}}}, nil)
	adm.must(422, "PUT", "/v1/clusters/"+c1.ID.String(), map[string]any{"cpu_ratio": 1}, nil) // 40 reservados > 32
	adm.must(204, "PUT", resPath, map[string]any{"cpu_cores": 16, "memory_mb": 32768,
		"storage": []map[string]any{{"class_id": ssd.ID, "capacity_gb": 500}}}, nil)
	var detail struct {
		Capacity []struct {
			Name     string `json:"name"`
			Total    int64  `json:"total"`
			Reserved int64  `json:"reserved"`
		} `json:"capacity"`
		Reservations []store.Reservation `json:"reservations"`
		StoredSecret bool                `json:"stored_secret"`
	}
	adm.must(200, "GET", "/v1/clusters/"+c1.ID.String(), nil, &detail)
	if !detail.StoredSecret || len(detail.Reservations) != 1 || detail.Capacity[0].Total != 64 || detail.Capacity[0].Reserved != 16 ||
		len(detail.Capacity) != 3 || detail.Capacity[2].Total != 4096 || detail.Capacity[2].Reserved != 500 {
		t.Fatalf("detalhe do cluster inesperado: %+v", detail)
	}
	var vdc struct {
		Project store.Project `json:"project"`
		Pool    string        `json:"proxmox_pool"`
	}
	adm.must(201, "POST", "/v1/tenants/"+cli.ID.String()+"/projects", map[string]any{"name": "Produção", "location_id": c1.ID,
		"cpu_cores": 4, "memory_mb": 4096, "storage": []map[string]any{{"class_id": ssd.ID, "capacity_gb": 60}}}, &vdc)
	adm.must(204, "PUT", "/v1/projects/"+vdc.Project.ID.String()+"/network", map[string]any{"bridge": "vmbr2", "vlan_tag": 120}, nil)

	pve.SetVMs(
		testutil.Template(9000, "pve-lab-n1", "tpl-debian-12", 3),
		testutil.VM(150, "pve-lab-n1", "legado", "", "stopped"),
	)
	syncer.SyncAll(ctx)
	var tpls struct {
		Templates []store.PVETemplate `json:"templates"`
	}
	adm.must(200, "GET", "/v1/clusters/"+c1.ID.String()+"/templates", nil, &tpls)
	if len(tpls.Templates) != 1 || tpls.Templates[0].VMID != 9000 {
		t.Fatalf("templates inesperados: %+v", tpls)
	}
	var img store.Image
	adm.must(201, "POST", "/v1/images", map[string]any{"location_id": c1.ID, "template_vmid": 9000, "name": "Debian 12", "min_disk_gb": 8}, &img)
	adm.must(409, "POST", "/v1/images", map[string]any{"location_id": c1.ID, "template_vmid": 9000, "name": "Outro"}, nil)

	// ---- usuários ----
	var created struct {
		User store.User `json:"user"`
		Temp string     `json:"temporary_password"`
	}
	adm.must(201, "POST", "/v1/tenants/"+msp.ID.String()+"/users", map[string]any{"username": "pat", "name": "Pat MSP", "role": "partner_admin"}, &created)
	adm.must(409, "POST", "/v1/tenants/"+msp.ID.String()+"/users", map[string]any{"username": "pat", "name": "Outro", "role": "viewer"}, nil)
	pat := newBrowser(t, srv.URL)
	pat.login("pat", created.Temp)
	pat.must(204, "PUT", "/v1/me/password", map[string]any{"current_password": created.Temp, "new_password": "pat-senha-boa-1"}, nil)
	// O MSP cria o admin do cliente; não consegue dar papel de provedor.
	pat.must(404, "POST", "/v1/tenants/"+prov.ID.String()+"/users", map[string]any{"username": "x1", "name": "X", "role": "super_admin"}, nil)
	pat.must(201, "POST", "/v1/tenants/"+cli.ID.String()+"/users", map[string]any{"username": "alice", "name": "Alice", "role": "customer_admin"}, &created)
	alice := newBrowser(t, srv.URL)
	alice.login("alice", created.Temp)
	alice.must(204, "PUT", "/v1/me/password", map[string]any{"current_password": created.Temp, "new_password": "alice-senha-boa"}, nil)
	// Cliente não reseta a senha do MSP nem do admin.
	alice.must(404, "POST", "/v1/users/pat/reset-password", nil, nil)
	alice.must(404, "POST", "/v1/users/admin/reset-password", nil, nil)
	var reset map[string]string
	pat.must(200, "POST", "/v1/users/alice/reset-password", map[string]any{"reset_totp": true}, &reset)
	alice.must(401, "GET", "/v1/me", nil, nil) // sessão derrubada pelo reset
	alice.login("alice", reset["temporary_password"])
	alice.must(204, "PUT", "/v1/me/password", map[string]any{"current_password": reset["temporary_password"], "new_password": "alice-senha-nova"}, nil)
	var ms struct {
		Memberships []store.MembershipView `json:"memberships"`
	}
	pat.must(200, "GET", "/v1/tenants/"+cli.ID.String()+"/memberships", nil, &ms)
	if len(ms.Memberships) != 1 || !ms.Memberships[0].Local || ms.Memberships[0].Name != "Alice" {
		t.Fatalf("lista de usuários inesperada: %+v", ms.Memberships)
	}

	// ---- imagens vistas pelo cliente: sem vmid ----
	raw := alice.must(200, "GET", "/v1/images?location_id="+c1.ID.String(), nil, nil)
	if strings.Contains(string(raw), "9000") || !strings.Contains(string(raw), "Debian 12") {
		t.Fatalf("catálogo do cliente inesperado: %s", raw)
	}

	// ---- criar VM ----
	vmReq := func(name string, disk int, start bool) map[string]any {
		return map[string]any{"name": name, "image_id": img.ID, "cpu_cores": 2, "memory_mb": 2048, "disk_gb": disk,
			"storage_class_id": ssd.ID, "username": "debian", "password": "senha-da-vm-1",
			"ssh_keys": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExemploChaveDeTeste+/= vitor@lab",
			"ip_mode":  "static", "ip_address": "10.20.0.10/24", "gateway": "10.20.0.1", "start": start}
	}
	vp := "/v1/projects/" + vdc.Project.ID.String() + "/vms"
	alice.must(400, "POST", vp, map[string]any{"name": "Nome Inválido", "image_id": img.ID, "cpu_cores": 1, "memory_mb": 512, "disk_gb": 10, "storage_class_id": ssd.ID, "username": "u", "password": "12345678"}, nil)
	alice.must(422, "POST", vp, vmReq("pequena", 5, false), nil) // abaixo do mínimo da imagem
	alice.must(422, "POST", vp, vmReq("grande", 70, false), nil) // acima do disco do VDC
	var task store.Task
	alice.must(202, "POST", vp, vmReq("web-01", 20, true), &task)
	// A cota de disco conta a criação ainda na fila: 20 + 45 > 60.
	alice.must(422, "POST", vp, vmReq("web-02", 45, false), nil)

	waitTask := func(b *browser, id uuid.UUID) store.Task {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			var tk store.Task
			b.must(200, "GET", "/v1/tasks/"+id.String(), nil, &tk)
			if tk.Status == "succeeded" || tk.Status == "failed" {
				return tk
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("task %s não terminou", id)
		return store.Task{}
	}
	created1 := waitTask(alice, task.ID)
	if created1.Status != "succeeded" || created1.ResourceID == nil {
		t.Fatalf("criação falhou: %+v", created1)
	}
	vm := pve.VMByName("web-01")
	if vm == nil || vm["pool"] != vdc.Pool || vm["status"] != "running" {
		t.Fatalf("VM no PVE inesperada: %+v", vm)
	}
	cfgVM := pve.Config(int(vm["vmid"].(int)))
	for k, want := range map[string]string{"cores": "2", "memory": "2048", "ciuser": "debian",
		"ipconfig0": "ip=10.20.0.10/24,gw=10.20.0.1", "scsi0": "ceph-ssd:vm-100-disk-0,size=20G"} {
		if got := fmtAny(cfgVM[k]); got != want {
			t.Errorf("config %s = %q, esperava %q", k, got, want)
		}
	}
	if net := fmtAny(cfgVM["net0"]); !strings.Contains(net, "bridge=vmbr2,tag=120") || !strings.HasPrefix(net, "virtio=BC:24:11:") {
		t.Errorf("net0 inesperada: %s", net)
	}
	if pw := fmtAny(cfgVM["cipassword"]); !strings.HasPrefix(pw, "$6$") {
		t.Errorf("cipassword deveria ir como hash: %q", pw)
	}
	if k := fmtAny(cfgVM["sshkeys"]); !strings.HasPrefix(k, "ssh-ed25519 AAAA") || !strings.Contains(k, "+/=") {
		t.Errorf("sshkeys não decodificou certo: %q", k)
	}
	var cap struct {
		Projects []store.Project `json:"projects"`
	}
	alice.must(200, "GET", "/v1/projects", nil, &cap)
	if len(cap.Projects) != 1 || cap.Projects[0].Storage[0].UsedGB != 20 || cap.Projects[0].Usage.RunningCPU != 2 {
		t.Fatalf("uso do VDC inesperado: %+v", cap.Projects)
	}
	if raw := alice.must(200, "GET", "/v1/projects", nil, nil); strings.Contains(string(raw), "vmbr2") {
		t.Log(string(raw))
		t.Fatal("rede do VDC não deveria aparecer para o cliente")
	}

	// ---- console ----
	resID := *created1.ResourceID
	var con struct {
		Path     string `json:"path"`
		Password string `json:"password"`
	}
	alice.must(200, "POST", "/v1/resources/"+resID.String()+"/console", nil, &con)
	pat.must(200, "POST", "/v1/resources/"+resID.String()+"/console", nil, nil)
	// Outro usuário não usa o ticket de Alice.
	if code := wsDial(t, pat, con.Path); code != 404 {
		t.Fatalf("ticket de outro usuário deveria dar 404, veio %d", code)
	}
	alice.must(200, "POST", "/v1/resources/"+resID.String()+"/console", nil, &con)
	name := vncHandshake(t, alice, con.Path, con.Password)
	if !strings.Contains(name, "web-01") {
		t.Fatalf("nome do desktop VNC inesperado: %q", name)
	}
	if code := wsDial(t, alice, con.Path); code != 404 {
		t.Fatalf("ticket usado deveria valer uma vez só, veio %d", code)
	}

	// ---- atribuir VM existente ----
	var lst struct {
		Resources []store.Resource `json:"resources"`
	}
	adm.must(200, "GET", "/v1/resources", nil, &lst)
	var legado uuid.UUID
	for _, r := range lst.Resources {
		if r.Alias == "legado" {
			legado = r.ID
		}
	}
	pat.must(404, "POST", "/v1/resources/"+legado.String()+"/assign", map[string]any{"project_id": vdc.Project.ID}, nil)
	adm.must(202, "POST", "/v1/resources/"+legado.String()+"/assign", map[string]any{"project_id": vdc.Project.ID}, &task)
	if got := waitTask(adm, task.ID); got.Status != "succeeded" {
		t.Fatalf("atribuição falhou: %+v", got)
	}
	if v := pve.VMByName("legado"); v["pool"] != vdc.Pool {
		t.Fatalf("VM deveria estar no pool do VDC: %+v", v)
	}
	alice.must(200, "GET", "/v1/resources", nil, &lst)
	if aliases(lst.Resources) != "legado,web-01" {
		t.Fatalf("cliente deveria ver as duas VMs: %s", aliases(lst.Resources))
	}

	// ---- excluir ----
	alice.must(400, "DELETE", "/v1/resources/"+resID.String(), map[string]any{"confirm_name": "web"}, nil)
	alice.must(202, "DELETE", "/v1/resources/"+resID.String(), map[string]any{"confirm_name": "web-01"}, &task)
	alice.must(409, "DELETE", "/v1/resources/"+resID.String(), map[string]any{"confirm_name": "web-01"}, nil)
	if got := waitTask(alice, task.ID); got.Status != "succeeded" {
		t.Fatalf("exclusão falhou: %+v", got)
	}
	if pve.VMByName("web-01") != nil {
		t.Fatal("VM deveria ter sumido do PVE")
	}
	calls := strings.Join(pve.Calls(), "\n")
	for _, want := range []string{"/stop", "DELETE 100 purge=1"} {
		if !strings.Contains(calls, want) {
			t.Errorf("faltou %q nas chamadas ao PVE:\n%s", want, calls)
		}
	}
	alice.must(404, "GET", "/v1/resources/"+resID.String(), nil, nil)

	// ---- remover acesso ----
	pat.must(200, "GET", "/v1/tenants/"+cli.ID.String()+"/memberships", nil, &ms)
	pat.must(204, "DELETE", "/v1/tenants/"+cli.ID.String()+"/memberships/"+ms.Memberships[0].ID.String(), nil, nil)
	alice.must(401, "GET", "/v1/resources", nil, nil) // sem vínculo, a sessão cai
}

func fmtAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func wsURL(base, path string) string { return "ws" + strings.TrimPrefix(base, "http") + path }

func wsDial(t *testing.T, b *browser, path string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(b.base, path), &websocket.DialOptions{HTTPClient: b.c, Subprotocols: []string{"binary"}})
	if err == nil {
		c.CloseNow()
		return 101
	}
	if resp == nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// vncHandshake abre o console pelo portal e faz o aperto de mão RFB com a
// senha devolvida pela API; devolve o nome do desktop.
func vncHandshake(t *testing.T, b *browser, path, password string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(b.base, path), &websocket.DialOptions{
		HTTPClient: b.c, Subprotocols: []string{"binary"}, HTTPHeader: http.Header{"Origin": {b.base}},
	})
	if err != nil {
		t.Fatalf("websocket do console: %v", err)
	}
	defer c.CloseNow()
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	read := func(n int) []byte {
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("RFB: %v", err)
		}
		return buf
	}
	if v := string(read(12)); v != "RFB 003.008\n" {
		t.Fatalf("versão RFB: %q", v)
	}
	_, _ = conn.Write([]byte("RFB 003.008\n"))
	if sec := read(2); sec[0] != 1 || sec[1] != 2 {
		t.Fatalf("tipos de segurança: %v", sec)
	}
	_, _ = conn.Write([]byte{2})
	ch := read(16)
	_, _ = conn.Write(testutil.VNCResponse(password, ch))
	if res := read(4); binary.BigEndian.Uint32(res) != 0 {
		t.Fatal("senha VNC recusada")
	}
	_, _ = conn.Write([]byte{1})
	init := read(24)
	return string(read(int(binary.BigEndian.Uint32(init[20:]))))
}
