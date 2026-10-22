package proxmox

// Leitura da infraestrutura do cluster (nodes, storages, bridges) e a sonda
// usada no cadastro: certificado apresentado, versão e o que o token enxerga.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vitordatasafer/mspvdc/internal/store"
)

// Res é uma linha de /cluster/resources (qualquer tipo).
type Res struct {
	Type       string  `json:"type"` // node | qemu | lxc | storage | sdn | pool
	Node       string  `json:"node"`
	Status     string  `json:"status"`
	VMID       int     `json:"vmid"`
	Name       string  `json:"name"`
	Pool       string  `json:"pool"`
	Template   int     `json:"template"`
	MaxCPU     float64 `json:"maxcpu"`
	MaxMem     int64   `json:"maxmem"`
	MaxDisk    int64   `json:"maxdisk"`
	Disk       int64   `json:"disk"`
	Storage    string  `json:"storage"`
	Shared     int     `json:"shared"`
	Content    string  `json:"content"`
	PluginType string  `json:"plugintype"`
}

// Snapshot é tudo que o sincronizador lê de um cluster numa rodada.
type Snapshot struct {
	Inventory store.ClusterInventory
	VMs       []VM
	Templates []VM
}

// Read lê o cluster: /cluster/resources (uma chamada), a versão e as
// bridges de cada node online.
func (c *Client) Read(ctx context.Context, withBridges bool) (Snapshot, error) {
	var snap Snapshot
	var all []Res
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", nil, &all); err != nil {
		return snap, err
	}
	var ver struct {
		Version string `json:"version"`
		Release string `json:"release"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, &ver); err == nil {
		snap.Inventory.Version = ver.Version
	}
	storages := map[string]*store.ClusterStorage{}
	var online []string
	for _, r := range all {
		switch r.Type {
		case "node":
			snap.Inventory.NodesTotal++
			if r.Status == "online" {
				snap.Inventory.NodesOnline++
				snap.Inventory.CPUTotal += int(r.MaxCPU)
				snap.Inventory.MemoryTotalMB += r.MaxMem >> 20
				online = append(online, r.Node)
			}
		case "qemu", "lxc":
			v := VM{VMID: r.VMID, Type: r.Type, Node: r.Node, Name: r.Name, Pool: r.Pool, Status: r.Status,
				MaxCPU: r.MaxCPU, MaxMem: r.MaxMem, MaxDisk: r.MaxDisk, Template: r.Template}
			if r.Template == 1 {
				if r.Type == "qemu" {
					snap.Templates = append(snap.Templates, v)
				}
			} else {
				snap.VMs = append(snap.VMs, v)
			}
		case "storage":
			if r.Status != "" && r.Status != "available" {
				continue
			}
			st := storages[r.Storage]
			if st == nil {
				st = &store.ClusterStorage{Storage: r.Storage, Type: r.PluginType, Shared: r.Shared == 1, Content: r.Content}
				storages[r.Storage] = st
			}
			st.Nodes++
			if !st.Shared || st.Nodes == 1 {
				st.TotalGB += r.MaxDisk >> 30
				st.UsedGB += r.Disk >> 30
			}
		}
	}
	for _, st := range storages {
		snap.Inventory.Storages = append(snap.Inventory.Storages, *st)
	}
	sort.Slice(snap.Inventory.Storages, func(i, j int) bool { return snap.Inventory.Storages[i].Storage < snap.Inventory.Storages[j].Storage })

	if withBridges {
		seen := map[string]*store.ClusterBridge{}
		for _, n := range online {
			var nets []struct {
				Iface string `json:"iface"`
				Type  string `json:"type"`
			}
			if !nodeRe.MatchString(n) {
				continue
			}
			if err := c.do(ctx, http.MethodGet, "/nodes/"+n+"/network?type=any_bridge", nil, &nets); err != nil {
				continue // sem Sys.Audit no node: segue sem bridges
			}
			for _, x := range nets {
				b := seen[x.Iface]
				if b == nil {
					b = &store.ClusterBridge{Name: x.Iface, Kind: "bridge"}
					seen[x.Iface] = b
				}
				b.Nodes++
			}
		}
		var vnets []struct {
			VNet string `json:"vnet"`
		}
		if err := c.do(ctx, http.MethodGet, "/cluster/sdn/vnets", nil, &vnets); err == nil {
			for _, v := range vnets {
				seen[v.VNet] = &store.ClusterBridge{Name: v.VNet, Kind: "vnet", Nodes: len(online)}
			}
		}
		snap.Inventory.Bridges = []store.ClusterBridge{}
		for _, b := range seen {
			snap.Inventory.Bridges = append(snap.Inventory.Bridges, *b)
		}
		sort.Slice(snap.Inventory.Bridges, func(i, j int) bool { return snap.Inventory.Bridges[i].Name < snap.Inventory.Bridges[j].Name })
	}
	return snap, nil
}

// ---------- sonda do certificado ----------

// CertInfo descreve o certificado que o servidor apresentou.
type CertInfo struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotAfter    time.Time `json:"not_after"`
	Fingerprint string    `json:"fingerprint_sha256"`
	IsCA        bool      `json:"is_ca"`
}

// TLSProbe é o resultado de conectar sem confiar em nada ainda.
type TLSProbe struct {
	Chain []CertInfo `json:"chain"`
	// TrustedBySystem: a cadeia vale pelas CAs do sistema e o nome confere
	// (ex.: Let's Encrypt); nesse caso não é preciso fixar nada.
	TrustedBySystem bool `json:"trusted_by_system"`
	// SuggestedPEM é o que fixar: a CA do cluster se ela veio na cadeia
	// (pve-root-ca), senão o próprio certificado do servidor.
	SuggestedPEM string `json:"suggested_pem"`
	Pinned       string `json:"pinned_subject"`
}

func ProbeTLS(ctx context.Context, apiURL string, allow Allowlist) (TLSProbe, error) {
	var p TLSProbe
	u, err := allow.Check(apiURL)
	if err != nil {
		return p, err
	}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := allow.dialer()(dctx, "tcp", u.Host)
	if err != nil {
		return p, err
	}
	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}) //nolint:gosec // só lê o certificado
	defer conn.Close()
	if err := conn.HandshakeContext(dctx); err != nil {
		return p, err
	}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return p, errors.New("o servidor não apresentou certificado")
	}
	for _, c := range certs {
		sum := sha256.Sum256(c.Raw)
		h := strings.ToUpper(hex.EncodeToString(sum[:]))
		var fp []string
		for i := 0; i < len(h); i += 2 {
			fp = append(fp, h[i:i+2])
		}
		p.Chain = append(p.Chain, CertInfo{Subject: c.Subject.String(), Issuer: c.Issuer.String(), NotAfter: c.NotAfter,
			Fingerprint: strings.Join(fp, ":"), IsCA: c.IsCA})
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{DNSName: u.Hostname(), Intermediates: inter}); err == nil {
		p.TrustedBySystem = true
	}
	pin := certs[0]
	for _, c := range certs[1:] {
		if c.IsCA {
			pin = c // a mais alta da cadeia
		}
	}
	p.Pinned = pin.Subject.String()
	p.SuggestedPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pin.Raw}))
	return p, nil
}

// Whoami confere o token: devolve o que ele enxerga do cluster.
func (c *Client) Permissions(ctx context.Context) (map[string]map[string]int, error) {
	var out map[string]map[string]int
	err := c.do(ctx, http.MethodGet, "/access/permissions?path="+url.QueryEscape("/"), nil, &out)
	return out, err
}
