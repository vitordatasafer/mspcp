// Package proxmox é o cliente da API do PVE. Defesas de fábrica:
//
//   - SSRF: com PROXMOX_ALLOWED_HOSTS definido, só fala com esses hosts;
//     sem ele, aceita qualquer host, menos loopback, link-local (metadados
//     de nuvem) e multicast, conferidos no momento da conexão. Não segue
//     redirects e ignora proxies de ambiente.
//   - TLS: ou confia no certificado/CA fixado no cadastro do cluster (sem
//     exigir que o nome bata, já que a CA é só daquele cluster), ou nas CAs
//     do sistema com verificação de nome. Não existe opção de desligar.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Allowlist de hosts ("host" ou "host:porta").
type Allowlist []string

func ParseAllowlist(s string) Allowlist {
	var a Allowlist
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
			a = append(a, h)
		}
	}
	return a
}

var ErrNotAllowed = errors.New("host fora de PROXMOX_ALLOWED_HOSTS")

// Check valida a URL base da API de um cluster.
func (a Allowlist) Check(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.User != nil || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("endereço deve ser https://host[:porta] sem caminho: %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "8006"
	}
	if len(a) == 0 {
		if ip := net.ParseIP(host); ip != nil && blockedIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrNotAllowed, host)
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
		u.Path = ""
		return u, nil
	}
	for _, entry := range a {
		eh, ep, err := net.SplitHostPort(entry)
		if err != nil {
			eh, ep = entry, ""
		}
		if eh == host && (ep == "" || ep == port) {
			u.Host = net.JoinHostPort(u.Hostname(), port)
			u.Path = ""
			return u, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotAllowed, host)
}

// blockedIP: destinos que nunca são um Proxmox legítimo quando não há
// allowlist (evita usar o portal para alcançar serviços locais).
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
}

// dialer devolve o DialContext que confere o IP real quando não há allowlist.
func (a Allowlist) dialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if len(a) == 0 {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return fmt.Errorf("%w: %s", ErrNotAllowed, host)
			}
			return nil
		}
	}
	return d.DialContext
}

// tlsConfig: CA/certificado fixado (sem checar nome) ou CAs do sistema.
func tlsConfig(caPEM string) (*tls.Config, error) {
	if strings.TrimSpace(caPEM) == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("certificado da CA sem PEM válido")
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // a verificação é feita abaixo, contra o certificado fixado
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("o servidor não apresentou certificado")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			})
			if err != nil {
				return fmt.Errorf("certificado do cluster não confere com o cadastrado: %w", err)
			}
			return nil
		},
	}, nil
}

type Client struct {
	base  *url.URL
	auth  string
	httpc *http.Client
}

// NewClient cria o cliente de um cluster. caPEM vazio = CAs do sistema.
func NewClient(apiURL, caPEM, tokenID, tokenSecret string, allow Allowlist) (*Client, error) {
	base, err := allow.Check(apiURL)
	if err != nil {
		return nil, err
	}
	tc, err := tlsConfig(caPEM)
	if err != nil {
		return nil, err
	}
	if !tokenIDRe.MatchString(tokenID) || tokenSecret == "" {
		return nil, errors.New("token deve ser usuario@realm!nome e o segredo não pode ser vazio")
	}
	tr := &http.Transport{
		Proxy:                 nil, // rede de gestão: nunca via proxy do ambiente
		TLSClientConfig:       tc,
		DialContext:           allow.dialer(),
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		base: base,
		auth: "PVEAPIToken=" + tokenID + "=" + tokenSecret,
		httpc: &http.Client{
			Transport: tr,
			Timeout:   60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirect recusado")
			},
		},
	}, nil
}

var tokenIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+![A-Za-z][A-Za-z0-9._-]*$`)

// APIError é uma resposta não-2xx do PVE.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("proxmox: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	// path já vem com segmentos escapados (e query, se houver).
	target := c.base.String() + "/api2/json" + path
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 32<<20)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(limited, 512))
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(limited).Decode(&env); err != nil {
		return fmt.Errorf("proxmox: resposta inválida: %w", err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// VM é uma entrada de /cluster/resources?type=vm.
type VM struct {
	VMID     int     `json:"vmid"`
	Type     string  `json:"type"` // qemu | lxc
	Node     string  `json:"node"`
	Name     string  `json:"name"`
	Pool     string  `json:"pool"`
	Status   string  `json:"status"`
	MaxCPU   float64 `json:"maxcpu"`
	MaxMem   int64   `json:"maxmem"`
	MaxDisk  int64   `json:"maxdisk"`
	Template int     `json:"template"`
}

// ClusterVMs lista VMs e CTs do cluster numa única chamada (sem templates).
func (c *Client) ClusterVMs(ctx context.Context) ([]VM, error) {
	var all []VM
	if err := c.do(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, v := range all {
		if v.Template == 0 && (v.Type == "qemu" || v.Type == "lxc") {
			out = append(out, v)
		}
	}
	return out, nil
}

var nodeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)

var powerActions = map[string]bool{"start": true, "shutdown": true, "stop": true, "reboot": true}

// Power dispara uma ação de energia e devolve o UPID da task no PVE.
func (c *Client) Power(ctx context.Context, node, kind string, vmid int, action string) (string, error) {
	if !nodeRe.MatchString(node) || (kind != "qemu" && kind != "lxc") || vmid <= 0 || !powerActions[action] {
		return "", fmt.Errorf("proxmox: parâmetros inválidos")
	}
	var upid string
	path := fmt.Sprintf("/nodes/%s/%s/%s/status/%s", node, kind, strconv.Itoa(vmid), action)
	if err := c.do(ctx, http.MethodPost, path, url.Values{}, &upid); err != nil {
		return "", err
	}
	if !strings.HasPrefix(upid, "UPID:") {
		return "", fmt.Errorf("proxmox: UPID inesperado %q", upid)
	}
	return upid, nil
}

// TaskStatus é o estado de uma task do PVE.
type TaskStatus struct {
	Status     string `json:"status"`     // running | stopped
	ExitStatus string `json:"exitstatus"` // "OK" em sucesso
}

func (t TaskStatus) Done() bool { return t.Status == "stopped" }
func (t TaskStatus) OK() bool   { return t.Done() && t.ExitStatus == "OK" }

func (c *Client) TaskStatus(ctx context.Context, node, upid string) (TaskStatus, error) {
	var ts TaskStatus
	if !nodeRe.MatchString(node) || !strings.HasPrefix(upid, "UPID:") {
		return ts, fmt.Errorf("proxmox: parâmetros inválidos")
	}
	err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/tasks/"+url.PathEscape(upid)+"/status", nil, &ts)
	return ts, err
}
