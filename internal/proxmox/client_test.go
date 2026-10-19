package proxmox_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mspclouds/mspcp/internal/proxmox"
	"github.com/mspclouds/mspcp/internal/testutil"
)

func TestAllowlistCheck(t *testing.T) {
	allow := proxmox.ParseAllowlist("pve-a.mgmt.local, 10.0.0.5:8006")
	ok := []string{"https://pve-a.mgmt.local", "https://PVE-A.mgmt.local:8443", "https://10.0.0.5:8006", "https://10.0.0.5"}
	for _, u := range ok {
		if _, err := allow.Check(u); err != nil {
			t.Errorf("%s deveria passar: %v", u, err)
		}
	}
	bad := []string{
		"http://pve-a.mgmt.local",            // sem TLS
		"https://evil.example.com",           // fora da lista
		"https://10.0.0.5:9000",              // porta não liberada
		"https://user:pw@pve-a.mgmt.local",   // userinfo
		"https://pve-a.mgmt.local/api2/json", // caminho
		"https://pve-a.mgmt.local?x=1",       // query
		"https://pve-a.mgmt.local.evil.com",  // sufixo
		"https://169.254.169.254",            // metadata
	}
	for _, u := range bad {
		if _, err := allow.Check(u); err == nil {
			t.Errorf("%s deveria ser recusada", u)
		}
	}
	if _, err := allow.Check("https://evil.example.com"); !errors.Is(err, proxmox.ErrNotAllowed) {
		t.Errorf("esperava ErrNotAllowed, veio %v", err)
	}
}

func selfSignedPEM(t *testing.T) string {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "outra-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestClientTLSAndAuth(t *testing.T) {
	pve := testutil.NewFakePVE(t, "portal@pve!sync", "s3cr3t")
	pve.SetVMs(testutil.VM(101, "pve1", "web", "t-a", "running"),
		map[string]any{"vmid": 9000, "type": "qemu", "node": "pve1", "template": 1})
	allow := proxmox.ParseAllowlist("127.0.0.1")

	c, err := proxmox.NewClient(pve.Server.URL, pve.CAPEM(), "portal@pve!sync", "s3cr3t", allow)
	if err != nil {
		t.Fatal(err)
	}
	vms, err := c.ClusterVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].VMID != 101 {
		t.Fatalf("esperava só a VM 101 (template filtrado), veio %+v", vms)
	}

	// CA errada: handshake tem que falhar.
	wrong, err := proxmox.NewClient(pve.Server.URL, selfSignedPEM(t), "portal@pve!sync", "s3cr3t", allow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.ClusterVMs(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("esperava erro de certificado, veio %v", err)
	}

	// Token errado: 401 vira APIError.
	bad, _ := proxmox.NewClient(pve.Server.URL, pve.CAPEM(), "portal@pve!sync", "errado", allow)
	_, err = bad.ClusterVMs(context.Background())
	var apiErr *proxmox.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("esperava APIError 401, veio %v", err)
	}

	// Ação + acompanhamento do UPID.
	upid, err := c.Power(context.Background(), "pve1", "qemu", 101, "start")
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := c.TaskStatus(context.Background(), "pve1", upid)
	if ts.Done() {
		t.Fatal("primeira consulta deveria estar running")
	}
	ts, _ = c.TaskStatus(context.Background(), "pve1", upid)
	if !ts.OK() {
		t.Fatalf("esperava stopped/OK, veio %+v", ts)
	}

	if _, err := c.Power(context.Background(), "../etc", "qemu", 101, "start"); err == nil {
		t.Fatal("node inválido deveria ser recusado")
	}
	if _, err := c.Power(context.Background(), "pve1", "qemu", 101, "destroy"); err == nil {
		t.Fatal("ação fora da lista deveria ser recusada")
	}
}

func TestClientRefusesRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	c, err := proxmox.NewClient(srv.URL, ca, "a@pve!b", "x", proxmox.ParseAllowlist("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClusterVMs(context.Background()); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("esperava redirect recusado, veio %v", err)
	}
}

func TestEmptyAllowlistBlocksLocal(t *testing.T) {
	var a proxmox.Allowlist
	for _, bad := range []string{"https://127.0.0.1:8006", "https://169.254.169.254", "https://[::1]:8006", "https://0.0.0.0"} {
		if _, err := a.Check(bad); err == nil {
			t.Errorf("%s deveria ser recusado sem allowlist", bad)
		}
	}
	if _, err := a.Check("https://10.0.0.11:8006"); err != nil {
		t.Errorf("IP de rede privada deveria passar: %v", err)
	}
	// Nome que resolve para loopback: barrado na conexão.
	c, err := proxmox.NewClient("https://localhost:1", "", "a@pve!b", "x", a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(context.Background(), false); err == nil || !strings.Contains(err.Error(), "fora") {
		t.Errorf("conexão a loopback deveria ser barrada: %v", err)
	}
}
