package proxmox

// Operações além de energia: pools, clone por template, configuração,
// disco, exclusão e console (vncproxy).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var (
	poolRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	storageRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)
	vmNameRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidVMName segue a regra do PVE (nome DNS) em minúsculas.
func ValidVMName(n string) bool { return vmNameRe.MatchString(n) }

func checkVM(node, kind string, vmid int) error {
	if !nodeRe.MatchString(node) || (kind != "qemu" && kind != "lxc") || vmid <= 0 {
		return fmt.Errorf("proxmox: parâmetros inválidos")
	}
	return nil
}

func vmPath(node, kind string, vmid int) string {
	return fmt.Sprintf("/nodes/%s/%s/%d", node, kind, vmid)
}

// IsExists reconhece o erro "já existe" do PVE (criar pool repetido etc.).
func IsExists(err error) bool {
	var e *APIError
	return errors.As(err, &e) && strings.Contains(strings.ToLower(e.Body), "already exist")
}

// ---------- templates ----------

// ClusterTemplates lista os templates de VM (qemu) do cluster.
func (c *Client) ClusterTemplates(ctx context.Context) ([]VM, error) {
	var all []VM
	if err := c.do(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &all); err != nil {
		return nil, err
	}
	var out []VM
	for _, v := range all {
		if v.Template == 1 && v.Type == "qemu" {
			out = append(out, v)
		}
	}
	return out, nil
}

// ---------- pools ----------

func (c *Client) CreatePool(ctx context.Context, pool, comment string) error {
	if !poolRe.MatchString(pool) {
		return fmt.Errorf("proxmox: nome de pool inválido")
	}
	err := c.do(ctx, http.MethodPost, "/pools", url.Values{"poolid": {pool}, "comment": {comment}}, nil)
	if IsExists(err) {
		return nil
	}
	return err
}

// MoveToPool põe a VM no pool (tirando de outro, se estiver). Usa a API do
// PVE 8.1+ (PUT /pools com allow-move) e cai para a antiga se precisar.
func (c *Client) MoveToPool(ctx context.Context, pool string, vmid int) error {
	if !poolRe.MatchString(pool) || vmid <= 0 {
		return fmt.Errorf("proxmox: parâmetros inválidos")
	}
	err := c.do(ctx, http.MethodPut, "/pools", url.Values{"poolid": {pool}, "vms": {strconv.Itoa(vmid)}, "allow-move": {"1"}}, nil)
	var e *APIError
	if errors.As(err, &e) && (e.Status == http.StatusNotImplemented || e.Status == http.StatusBadRequest && strings.Contains(e.Body, "allow-move")) {
		return c.do(ctx, http.MethodPut, "/pools/"+pool, url.Values{"vms": {strconv.Itoa(vmid)}}, nil)
	}
	return err
}

// RemoveFromPool tira a VM do pool.
func (c *Client) RemoveFromPool(ctx context.Context, pool string, vmid int) error {
	if !poolRe.MatchString(pool) || vmid <= 0 {
		return fmt.Errorf("proxmox: parâmetros inválidos")
	}
	err := c.do(ctx, http.MethodPut, "/pools", url.Values{"poolid": {pool}, "vms": {strconv.Itoa(vmid)}, "delete": {"1"}}, nil)
	var e *APIError
	if errors.As(err, &e) && e.Status == http.StatusNotImplemented {
		return c.do(ctx, http.MethodPut, "/pools/"+pool, url.Values{"vms": {strconv.Itoa(vmid)}, "delete": {"1"}}, nil)
	}
	return err
}

// ---------- criação ----------

func (c *Client) NextID(ctx context.Context) (int, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	n, err := strconv.Atoi(strings.Trim(s, `"`))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("proxmox: nextid inesperado %q", raw)
	}
	return n, nil
}

// CloneSpec descreve o clone completo de um template.
type CloneSpec struct {
	Node     string // node onde o template está
	Template int
	NewID    int
	Name     string
	Pool     string
	Storage  string // storage de destino dos discos
}

func (c *Client) Clone(ctx context.Context, s CloneSpec) (string, error) {
	if err := checkVM(s.Node, "qemu", s.Template); err != nil || s.NewID <= 0 || !ValidVMName(s.Name) ||
		!poolRe.MatchString(s.Pool) || !storageRe.MatchString(s.Storage) {
		return "", fmt.Errorf("proxmox: parâmetros de clone inválidos")
	}
	form := url.Values{
		"newid": {strconv.Itoa(s.NewID)}, "name": {s.Name}, "full": {"1"},
		"pool": {s.Pool}, "storage": {s.Storage},
	}
	return c.upid(ctx, http.MethodPost, vmPath(s.Node, "qemu", s.Template)+"/clone", form)
}

func (c *Client) upid(ctx context.Context, method, path string, form url.Values) (string, error) {
	var upid string
	if err := c.do(ctx, method, path, form, &upid); err != nil {
		return "", err
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return "", fmt.Errorf("proxmox: UPID inesperado %q", upid)
	}
	return upid, nil
}

// VMConfig é a configuração atual (valores como o PVE devolve).
type VMConfig map[string]any

func (c *Client) Config(ctx context.Context, node string, vmid int) (VMConfig, error) {
	if err := checkVM(node, "qemu", vmid); err != nil {
		return nil, err
	}
	var cfg VMConfig
	err := c.do(ctx, http.MethodGet, vmPath(node, "qemu", vmid)+"/config", nil, &cfg)
	return cfg, err
}

// SetConfig aplica a configuração de forma síncrona (PUT).
func (c *Client) SetConfig(ctx context.Context, node string, vmid int, form url.Values) error {
	if err := checkVM(node, "qemu", vmid); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, vmPath(node, "qemu", vmid)+"/config", form, nil)
}

// Resize aumenta um disco para o tamanho absoluto (GB). Em versões novas o
// PVE devolve uma task; nas antigas, nada.
func (c *Client) Resize(ctx context.Context, node string, vmid int, disk string, sizeGB int) (string, error) {
	if err := checkVM(node, "qemu", vmid); err != nil || !diskKeyRe.MatchString(disk) || sizeGB <= 0 {
		return "", fmt.Errorf("proxmox: parâmetros inválidos")
	}
	var out json.RawMessage
	err := c.do(ctx, http.MethodPut, vmPath(node, "qemu", vmid)+"/resize",
		url.Values{"disk": {disk}, "size": {strconv.Itoa(sizeGB) + "G"}}, &out)
	if err != nil {
		return "", err
	}
	var upid string
	_ = json.Unmarshal(out, &upid)
	return upid, nil
}

// Delete exclui a VM/CT e os discos (purge tira de backups e HA).
func (c *Client) Delete(ctx context.Context, node, kind string, vmid int) (string, error) {
	if err := checkVM(node, kind, vmid); err != nil {
		return "", err
	}
	q := "?purge=1&destroy-unreferenced-disks=1"
	return c.upid(ctx, http.MethodDelete, vmPath(node, kind, vmid)+q, nil)
}

// WaitTask acompanha uma task do PVE até terminar.
func (c *Client) WaitTask(ctx context.Context, node, upid string, poll time.Duration) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		ts, err := c.TaskStatus(ctx, node, upid)
		if err != nil {
			return err
		}
		if ts.Done() {
			if !ts.OK() {
				return &TaskFailed{Exit: ts.ExitStatus}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// TaskFailed é uma task do PVE que terminou com erro.
type TaskFailed struct{ Exit string }

func (e *TaskFailed) Error() string { return "proxmox: " + e.Exit }

// ---------- discos ----------

var diskKeyRe = regexp.MustCompile(`^(scsi|virtio|sata|ide)\d{1,2}$`)

// Disk é um disco de VM lido da configuração.
type Disk struct {
	Key     string
	Storage string
	SizeGB  int
}

// Disks devolve os discos de dados (sem CD-ROM nem cloud-init), em ordem.
func (cfg VMConfig) Disks() []Disk {
	var out []Disk
	for k, v := range cfg {
		s, ok := v.(string)
		if !ok || !diskKeyRe.MatchString(k) {
			continue
		}
		if strings.Contains(s, "media=cdrom") || strings.Contains(s, "cloudinit") {
			continue
		}
		vol := strings.SplitN(s, ",", 2)[0]
		storage, _, ok := strings.Cut(vol, ":")
		if !ok {
			continue
		}
		out = append(out, Disk{Key: k, Storage: storage, SizeGB: parseSizeGB(s)})
	}
	sort.Slice(out, func(i, j int) bool { return diskOrder(out[i].Key) < diskOrder(out[j].Key) })
	return out
}

// BootDisk escolhe o disco de boot: o primeiro de "boot: order=" que for
// disco; senão o primeiro disco.
func (cfg VMConfig) BootDisk() (Disk, bool) {
	disks := cfg.Disks()
	if len(disks) == 0 {
		return Disk{}, false
	}
	if boot, ok := cfg["boot"].(string); ok {
		if order, ok := strings.CutPrefix(boot, "order="); ok {
			for _, k := range strings.Split(order, ";") {
				for _, d := range disks {
					if d.Key == k {
						return d, true
					}
				}
			}
		}
	}
	if bd, ok := cfg["bootdisk"].(string); ok {
		for _, d := range disks {
			if d.Key == bd {
				return d, true
			}
		}
	}
	return disks[0], true
}

func diskOrder(k string) int {
	base := map[string]int{"scsi": 0, "virtio": 100, "sata": 200, "ide": 300}
	for p, b := range base {
		if n, ok := strings.CutPrefix(k, p); ok {
			i, _ := strconv.Atoi(n)
			return b + i
		}
	}
	return 999
}

var sizeRe = regexp.MustCompile(`(?:^|,)size=(\d+(?:\.\d+)?)([KMGT]?)`)

func parseSizeGB(s string) int {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "K":
		f /= 1024 * 1024
	case "M":
		f /= 1024
	case "T":
		f *= 1024
	case "":
		f /= 1024 * 1024 * 1024
	}
	return int(f + 0.999)
}

// ---------- console ----------

// VNCTicket é o que o vncproxy devolve.
type VNCTicket struct {
	Port   int    `json:"-"`
	Ticket string `json:"ticket"`
	User   string `json:"user"`
}

func (c *Client) VNCProxy(ctx context.Context, node, kind string, vmid int) (VNCTicket, error) {
	var t VNCTicket
	if err := checkVM(node, kind, vmid); err != nil {
		return t, err
	}
	var raw struct {
		Port   json.RawMessage `json:"port"`
		Ticket string          `json:"ticket"`
		User   string          `json:"user"`
	}
	if err := c.do(ctx, http.MethodPost, vmPath(node, kind, vmid)+"/vncproxy", url.Values{"websocket": {"1"}}, &raw); err != nil {
		return t, err
	}
	p := strings.Trim(string(raw.Port), `"`)
	port, err := strconv.Atoi(p)
	if err != nil || port <= 0 || raw.Ticket == "" {
		return t, fmt.Errorf("proxmox: vncproxy inesperado")
	}
	return VNCTicket{Port: port, Ticket: raw.Ticket, User: raw.User}, nil
}

// DialVNC abre o websocket do console no PVE, autenticado com o token.
func (c *Client) DialVNC(ctx context.Context, node, kind string, vmid int, t VNCTicket) (*websocket.Conn, error) {
	if err := checkVM(node, kind, vmid); err != nil {
		return nil, err
	}
	u := *c.base
	u.Scheme = "wss"
	u.Path = "/api2/json" + vmPath(node, kind, vmid) + "/vncwebsocket"
	u.RawQuery = url.Values{"port": {strconv.Itoa(t.Port)}, "vncticket": {t.Ticket}}.Encode()
	hc := *c.httpc
	hc.Timeout = 0 // conexão longa
	if tr, ok := c.httpc.Transport.(*http.Transport); ok {
		t2 := tr.Clone() // websocket exige HTTP/1.1
		t2.ForceAttemptHTTP2 = false
		t2.TLSClientConfig.NextProtos = []string{"http/1.1"}
		hc.Transport = t2
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient:   &hc,
		HTTPHeader:   http.Header{"Authorization": {c.auth}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("proxmox: console HTTP %d: %w", resp.StatusCode, err)
		}
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	return conn, nil
}
