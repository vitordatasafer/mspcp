// Package testutil traz dublês de teste: um PVE falso em TLS e um IdP OIDC.
package testutil

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// FakePVE simula o pedaço da API do Proxmox que o mspvdc usa: inventário,
// energia, pools, clone de template, configuração, disco, exclusão e
// console (vncproxy + vncwebsocket com um servidor VNC de mentira).
type FakePVE struct {
	Server    *httptest.Server
	AuthValue string // header Authorization esperado

	mu       sync.Mutex
	vms      []map[string]any
	configs  map[int]map[string]any
	pools    map[string]bool
	calls    []string // "POST node/kind/vmid/action", "CLONE 9000->105" ...
	polls    map[string]int
	pending  map[string]pendingAction // UPID -> ação em curso
	failNext string                   // exitstatus a devolver na próxima task
	tickets  map[string]int           // ticket VNC -> vmid
	seq      int
}

type pendingAction struct {
	vmid   int
	action string // start/stop/... | clone | delete | resize
	clone  map[string]any
}

// NewFakePVEServer sobe o PVE falso (o chamador fecha Server).
func NewFakePVEServer(tokenID, secret string) *FakePVE {
	f := &FakePVE{
		AuthValue: "PVEAPIToken=" + tokenID + "=" + secret, polls: map[string]int{}, pending: map[string]pendingAction{},
		configs: map[int]map[string]any{}, pools: map[string]bool{}, tickets: map[string]int{},
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	return f
}

func NewFakePVE(t *testing.T, tokenID, secret string) *FakePVE {
	f := NewFakePVEServer(tokenID, secret)
	t.Cleanup(f.Server.Close)
	return f
}

// CAPEM é o certificado do servidor de teste, para cadastrar no cluster.
func (f *FakePVE) CAPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Server.Certificate().Raw}))
}

// SetVMs troca o inventário (VMs e templates). A config de cada VM nasce
// de maxdisk/maxmem/maxcpu se ainda não existir.
func (f *FakePVE) SetVMs(vms ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms = vms
	for _, vm := range vms {
		id := toInt(vm["vmid"])
		if _, ok := f.configs[id]; !ok {
			f.configs[id] = defaultConfig(vm)
		}
	}
}

func defaultConfig(vm map[string]any) map[string]any {
	gb := toInt64(vm["maxdisk"]) >> 30
	if gb == 0 {
		gb = 10
	}
	cfg := map[string]any{
		"name": vm["name"], "cores": toInt(vm["maxcpu"]), "memory": fmt.Sprint(toInt64(vm["maxmem"]) >> 20),
		"scsi0": fmt.Sprintf("ceph-ssd:vm-%d-disk-0,size=%dG", toInt(vm["vmid"]), gb),
		"net0":  fmt.Sprintf("virtio=BC:24:11:00:%02X:%02X,bridge=vmbr0", toInt(vm["vmid"])/256%256, toInt(vm["vmid"])%256),
		"boot":  "order=scsi0",
	}
	if toInt(vm["template"]) == 1 {
		cfg["ide2"] = "ceph-ssd:vm-" + fmt.Sprint(vm["vmid"]) + "-cloudinit,media=cdrom"
	}
	return cfg
}

func (f *FakePVE) FailNextTask(exit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = exit
}

func (f *FakePVE) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Config devolve uma cópia da configuração atual de uma VM.
func (f *FakePVE) Config(vmid int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for k, v := range f.configs[vmid] {
		out[k] = v
	}
	return out
}

// VMByName acha uma VM pelo nome (nil se não existir).
func (f *FakePVE) VMByName(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, vm := range f.vms {
		if vm["name"] == name {
			cp := map[string]any{}
			for k, v := range vm {
				cp[k] = v
			}
			return cp
		}
	}
	return nil
}

// AddPool cria um pool (como se já existisse no cluster).
func (f *FakePVE) AddPool(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pools[p] = true
}

func (f *FakePVE) HasPool(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pools[p]
}

func VM(vmid int, node, name, pool, status string) map[string]any {
	return map[string]any{
		"vmid": vmid, "type": "qemu", "node": node, "name": name, "pool": pool,
		"status": status, "maxcpu": 2, "maxmem": 4 << 30, "maxdisk": 40 << 30, "template": 0,
		"id": fmt.Sprintf("qemu/%d", vmid),
	}
}

// Template cria um template qemu (disco de diskGB).
func Template(vmid int, node, name string, diskGB int) map[string]any {
	return map[string]any{
		"vmid": vmid, "type": "qemu", "node": node, "name": name, "pool": "", "status": "stopped",
		"maxcpu": 1, "maxmem": 1 << 30, "maxdisk": int64(diskGB) << 30, "template": 1, "id": fmt.Sprintf("qemu/%d", vmid),
	}
}

func toInt(v any) int { return int(toInt64(v)) }

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

var sizeRe = regexp.MustCompile(`size=(\d+)G`)

func (f *FakePVE) vm(vmid int) map[string]any {
	for _, vm := range f.vms {
		if toInt(vm["vmid"]) == vmid {
			return vm
		}
	}
	return nil
}

func (f *FakePVE) newUPID(node, typ string, vmid int) string {
	f.seq++
	return fmt.Sprintf("UPID:%s:0000ABCD:00001234:6700%04X:%s:%d:root@pam:", node, f.seq, typ, vmid)
}

func (f *FakePVE) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != f.AuthValue {
		http.Error(w, "authentication failure", http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// Console: conexão longa, fora do lock.
	if r.Method == http.MethodGet && len(parts) == 5 && parts[0] == "nodes" && parts[4] == "vncwebsocket" {
		f.vncWebsocket(w, r)
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
	}
	fail := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": msg})
	}
	vmid := 0
	if len(parts) >= 4 && parts[0] == "nodes" {
		vmid, _ = strconv.Atoi(parts[3])
	}
	switch {
	case r.Method == http.MethodGet && path == "/cluster/resources":
		switch r.URL.Query().Get("type") {
		case "vm":
			reply(f.vms)
		case "":
			reply(f.allResources())
		default:
			http.Error(w, "type não simulado", http.StatusBadRequest)
		}

	case r.Method == http.MethodGet && path == "/version":
		reply(map[string]any{"version": "8.3.2", "release": "8.3", "repoid": "fake"})

	case r.Method == http.MethodGet && path == "/access/permissions":
		privs := map[string]int{}
		for _, p := range strings.Fields("VM.Audit VM.PowerMgmt VM.Console VM.Clone VM.Allocate VM.Config.CPU VM.Config.Memory VM.Config.Disk VM.Config.Network VM.Config.Cloudinit VM.Config.Options Datastore.Audit Datastore.AllocateSpace Pool.Allocate Pool.Audit Sys.Audit") {
			privs[p] = 1
		}
		reply(map[string]any{"/": privs})

	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "nodes" && parts[2] == "network":
		reply([]map[string]any{{"iface": "vmbr0", "type": "bridge"}, {"iface": "vmbr1", "type": "bridge"}})

	case r.Method == http.MethodGet && path == "/cluster/sdn/vnets":
		reply([]map[string]any{{"vnet": "tenants", "zone": "evpn"}})

	case r.Method == http.MethodGet && path == "/cluster/nextid":
		n := 100
		for f.vm(n) != nil {
			n++
		}
		reply(strconv.Itoa(n))

	case r.Method == http.MethodPost && path == "/pools":
		id := r.PostForm.Get("poolid")
		if f.pools[id] {
			fail(http.StatusInternalServerError, "create pool failed: pool '"+id+"' already exists")
			return
		}
		f.pools[id] = true
		f.calls = append(f.calls, "POOL "+id)
		reply(nil)

	case r.Method == http.MethodPut && path == "/pools":
		id := r.PostForm.Get("poolid")
		if !f.pools[id] {
			fail(http.StatusInternalServerError, "pool '"+id+"' does not exist")
			return
		}
		for _, v := range strings.Split(r.PostForm.Get("vms"), ",") {
			vm := f.vm(toInt(v))
			if vm == nil {
				fail(http.StatusInternalServerError, "no such VMID '"+v+"'")
				return
			}
			if r.PostForm.Get("delete") == "1" {
				vm["pool"] = ""
				f.calls = append(f.calls, "UNPOOL "+v)
				continue
			}
			if p, _ := vm["pool"].(string); p != "" && p != id && r.PostForm.Get("allow-move") != "1" {
				fail(http.StatusInternalServerError, "VM "+v+" is already a pool member")
				return
			}
			vm["pool"] = id
			f.calls = append(f.calls, "MOVE "+v+" "+id)
		}
		reply(nil)

	// /nodes/{node}/{kind}/{vmid}/status/{action}
	case r.Method == http.MethodPost && len(parts) == 6 && parts[0] == "nodes" && parts[4] == "status":
		node, kind, action := parts[1], parts[2], parts[5]
		vm := f.vm(vmid)
		if vm == nil || vm["node"] != node {
			http.Error(w, "VM não existe neste node", http.StatusInternalServerError)
			return
		}
		f.calls = append(f.calls, fmt.Sprintf("POST %s/%s/%d/%s", node, kind, vmid, action))
		upid := f.newUPID(node, "qm"+action, vmid)
		f.pending[upid] = pendingAction{vmid: vmid, action: action}
		reply(upid)

	case r.Method == http.MethodPost && len(parts) == 5 && parts[0] == "nodes" && parts[4] == "clone":
		tpl := f.vm(vmid)
		newid := toInt(r.PostForm.Get("newid"))
		if tpl == nil || toInt(tpl["template"]) != 1 || tpl["node"] != parts[1] {
			fail(http.StatusInternalServerError, "template não existe neste node")
			return
		}
		if f.vm(newid) != nil {
			fail(http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d: config file already exists", newid))
			return
		}
		if r.PostForm.Get("full") != "1" || r.PostForm.Get("storage") == "" {
			fail(http.StatusBadRequest, "esperava full=1 e storage")
			return
		}
		if p := r.PostForm.Get("pool"); p != "" && !f.pools[p] {
			fail(http.StatusInternalServerError, "pool '"+p+"' does not exist")
			return
		}
		f.calls = append(f.calls, fmt.Sprintf("CLONE %d->%d %s", vmid, newid, r.PostForm.Get("storage")))
		upid := f.newUPID(parts[1], "qmclone", vmid)
		f.pending[upid] = pendingAction{vmid: newid, action: "clone", clone: map[string]any{
			"template": vmid, "name": r.PostForm.Get("name"), "pool": r.PostForm.Get("pool"), "storage": r.PostForm.Get("storage"),
		}}
		reply(upid)

	case len(parts) == 5 && parts[0] == "nodes" && parts[4] == "config":
		cfg, ok := f.configs[vmid]
		if !ok || f.vm(vmid) == nil {
			fail(http.StatusInternalServerError, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", parts[1], vmid))
			return
		}
		if r.Method == http.MethodGet {
			reply(cfg)
			return
		}
		if r.Method != http.MethodPut {
			fail(http.StatusNotImplemented, "use PUT")
			return
		}
		keys := make([]string, 0, len(r.PostForm))
		for k := range r.PostForm {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := r.PostForm.Get(k)
			if k == "sshkeys" {
				if dec, err := url.PathUnescape(v); err == nil {
					v = dec
				}
			}
			cfg[k] = v
		}
		vm := f.vm(vmid)
		if c := toInt(cfg["cores"]); c > 0 {
			vm["maxcpu"] = c
		}
		if m := toInt64(cfg["memory"]); m > 0 {
			vm["maxmem"] = m << 20
		}
		f.calls = append(f.calls, fmt.Sprintf("CONFIG %d %s", vmid, strings.Join(keys, ",")))
		reply(nil)

	case r.Method == http.MethodPut && len(parts) == 5 && parts[0] == "nodes" && parts[4] == "resize":
		cfg := f.configs[vmid]
		disk := r.PostForm.Get("disk")
		cur, _ := cfg[disk].(string)
		m := sizeRe.FindStringSubmatch(cur)
		want := toInt(strings.TrimSuffix(r.PostForm.Get("size"), "G"))
		if m == nil || want < toInt(m[1]) {
			fail(http.StatusInternalServerError, "shrinking disks is not supported")
			return
		}
		cfg[disk] = sizeRe.ReplaceAllString(cur, fmt.Sprintf("size=%dG", want))
		f.vm(vmid)["maxdisk"] = int64(want) << 30
		f.calls = append(f.calls, fmt.Sprintf("RESIZE %d %s %dG", vmid, disk, want))
		upid := f.newUPID(parts[1], "resize", vmid)
		f.pending[upid] = pendingAction{vmid: vmid, action: "resize"}
		reply(upid)

	case r.Method == http.MethodDelete && len(parts) == 4 && parts[0] == "nodes":
		vm := f.vm(vmid)
		if vm == nil {
			fail(http.StatusInternalServerError, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", parts[1], vmid))
			return
		}
		if vm["status"] == "running" {
			fail(http.StatusInternalServerError, fmt.Sprintf("VM %d is running - destroy failed", vmid))
			return
		}
		f.calls = append(f.calls, fmt.Sprintf("DELETE %d purge=%s", vmid, r.URL.Query().Get("purge")))
		upid := f.newUPID(parts[1], "qmdestroy", vmid)
		f.pending[upid] = pendingAction{vmid: vmid, action: "delete"}
		reply(upid)

	case r.Method == http.MethodPost && len(parts) == 5 && parts[0] == "nodes" && parts[4] == "vncproxy":
		vm := f.vm(vmid)
		if vm == nil || vm["status"] != "running" {
			fail(http.StatusInternalServerError, "VM não está ligada")
			return
		}
		f.seq++
		ticket := fmt.Sprintf("PVEVNC:%08X::fakeTicket%d", f.seq, f.seq)
		f.tickets[ticket] = vmid
		f.calls = append(f.calls, fmt.Sprintf("VNCPROXY %d", vmid))
		reply(map[string]any{"port": "5900", "ticket": ticket, "user": "portal@pve!mspvdc", "upid": f.newUPID(parts[1], "vncproxy", vmid)})

	// /nodes/{node}/tasks/{upid}/status
	case r.Method == http.MethodGet && len(parts) == 5 && parts[0] == "nodes" && parts[2] == "tasks" && parts[4] == "status":
		upid := parts[3]
		f.polls[upid]++
		if f.polls[upid] < 2 {
			reply(map[string]any{"status": "running"})
			return
		}
		exit := "OK"
		if f.failNext != "" {
			exit, f.failNext = f.failNext, ""
		}
		if p, ok := f.pending[upid]; ok {
			delete(f.pending, upid)
			if exit == "OK" {
				f.finish(p)
			}
		}
		reply(map[string]any{"status": "stopped", "exitstatus": exit})
	default:
		http.Error(w, "rota não simulada: "+r.Method+" "+path, http.StatusNotImplemented)
	}
}

// nodes devolve os nodes vistos nas VMs (ou um padrão).
func (f *FakePVE) nodes() []string {
	seen := map[string]bool{}
	var out []string
	for _, vm := range f.vms {
		if n, _ := vm["node"].(string); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		out = []string{"pve-1"}
	}
	sort.Strings(out)
	return out
}

// allResources imita /cluster/resources sem filtro: VMs, nodes e storages.
func (f *FakePVE) allResources() []map[string]any {
	out := append([]map[string]any{}, f.vms...)
	for _, n := range f.nodes() {
		out = append(out,
			map[string]any{"type": "node", "node": n, "status": "online", "maxcpu": 32, "maxmem": int64(128) << 30, "id": "node/" + n},
			map[string]any{"type": "storage", "node": n, "storage": "local-lvm", "status": "available", "shared": 0,
				"plugintype": "lvmthin", "content": "images,rootdir", "maxdisk": int64(500) << 30, "disk": int64(50) << 30},
			map[string]any{"type": "storage", "node": n, "storage": "ceph-ssd", "status": "available", "shared": 1,
				"plugintype": "rbd", "content": "images,rootdir", "maxdisk": int64(4096) << 30, "disk": int64(900) << 30},
			map[string]any{"type": "storage", "node": n, "storage": "ceph-hdd", "status": "available", "shared": 1,
				"plugintype": "rbd", "content": "images", "maxdisk": int64(16384) << 30, "disk": int64(2048) << 30},
		)
	}
	return out
}

// finish aplica o efeito de uma task bem-sucedida, como o PVE real.
func (f *FakePVE) finish(p pendingAction) {
	switch p.action {
	case "clone":
		tpl := f.vm(toInt(p.clone["template"]))
		vm := map[string]any{
			"vmid": p.vmid, "type": "qemu", "node": tpl["node"], "name": p.clone["name"], "pool": p.clone["pool"],
			"status": "stopped", "maxcpu": tpl["maxcpu"], "maxmem": tpl["maxmem"], "maxdisk": tpl["maxdisk"], "template": 0,
			"id": fmt.Sprintf("qemu/%d", p.vmid),
		}
		f.vms = append(f.vms, vm)
		cfg := map[string]any{}
		for k, v := range f.configs[toInt(tpl["vmid"])] {
			cfg[k] = v
		}
		st := p.clone["storage"].(string)
		cfg["name"] = p.clone["name"]
		cfg["scsi0"] = regexp.MustCompile(`^[^:]+:[^,]+`).ReplaceAllString(cfg["scsi0"].(string), fmt.Sprintf("%s:vm-%d-disk-0", st, p.vmid))
		cfg["net0"] = fmt.Sprintf("virtio=BC:24:11:AA:%02X:%02X,bridge=vmbr0", p.vmid/256%256, p.vmid%256)
		f.configs[p.vmid] = cfg
	case "delete":
		for i, vm := range f.vms {
			if toInt(vm["vmid"]) == p.vmid {
				f.vms = append(f.vms[:i], f.vms[i+1:]...)
				break
			}
		}
		delete(f.configs, p.vmid)
	case "resize":
	default:
		if vm := f.vm(p.vmid); vm != nil {
			vm["status"] = map[string]string{"start": "running", "reboot": "running", "shutdown": "stopped", "stop": "stopped"}[p.action]
		}
	}
}

func (f *FakePVE) vncWebsocket(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("vncticket")
	f.mu.Lock()
	vmid, ok := f.tickets[ticket]
	delete(f.tickets, ticket)
	name := ""
	if vm := f.vm(vmid); vm != nil {
		name, _ = vm["name"].(string)
	}
	f.mu.Unlock()
	if !ok || r.URL.Query().Get("port") != "5900" {
		http.Error(w, "ticket inválido", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	serveFakeVNC(context.WithoutCancel(ctx), conn, ticket, name)
}
