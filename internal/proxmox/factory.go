package proxmox

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/mspclouds/mspcp/internal/secrets"
	"github.com/mspclouds/mspcp/internal/store"
)

// Clients monta e reaproveita um Client por cluster. O segredo do token é
// resolvido no cofre só na criação e fica apenas em memória.
type Clients struct {
	allow   Allowlist
	secrets secrets.Resolver
	box     *secrets.Box

	mu    sync.Mutex
	cache map[uuid.UUID]cached
}

type cached struct {
	fingerprint string
	client      *Client
}

func NewClients(allow Allowlist, s secrets.Resolver, box *secrets.Box) *Clients {
	return &Clients{allow: allow, secrets: s, box: box, cache: map[uuid.UUID]cached{}}
}

// Box cifra os segredos que o portal guarda no banco.
func (f *Clients) Box() *secrets.Box { return f.box }

func (f *Clients) For(ctx context.Context, c store.Cluster) (*Client, error) {
	fp := c.APIURL + "\x00" + c.CACertPEM + "\x00" + c.TokenID + "\x00" + c.TokenSecretRef + "\x00" + string(c.TokenSecretEnc)
	f.mu.Lock()
	if e, ok := f.cache[c.ID]; ok && e.fingerprint == fp {
		f.mu.Unlock()
		return e.client, nil
	}
	f.mu.Unlock()

	var secret string
	var err error
	if len(c.TokenSecretEnc) > 0 {
		secret, err = f.box.Open(c.TokenSecretEnc)
	} else {
		secret, err = f.secrets.Resolve(ctx, c.TokenSecretRef)
	}
	if err != nil {
		return nil, err
	}
	cl, err := NewClient(c.APIURL, c.CACertPEM, c.TokenID, secret, f.allow)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.cache[c.ID] = cached{fingerprint: fp, client: cl}
	f.mu.Unlock()
	return cl, nil
}

// Allowlist expõe a allowlist para validar cadastros de cluster.
func (f *Clients) Allowlist() Allowlist { return f.allow }
