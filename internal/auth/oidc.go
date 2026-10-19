// Package auth valida access tokens OIDC (JWT) contra o JWKS do provedor de
// identidade (Keycloak). Não guarda sessão: cada request traz o Bearer.
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims são os campos do token que o portal usa.
type Claims struct {
	Subject string
	Email   string
}

type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Claims, error)
}

var ErrInvalidToken = errors.New("token inválido")

// OIDCVerifier busca o jwks_uri pela discovery e mantém as chaves em cache.
// Um kid desconhecido dispara um refresh, limitado a um por minuto para que
// tokens forjados não virem um ataque de amplificação contra o IdP.
type OIDCVerifier struct {
	issuer     string
	audience   string
	httpClient *http.Client

	mu          sync.RWMutex
	jwksURI     string
	keys        map[string]any
	lastRefresh time.Time
	minRefresh  time.Duration
}

func NewOIDCVerifier(issuer, audience string, httpClient *http.Client) *OIDCVerifier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &OIDCVerifier{
		issuer:     strings.TrimSuffix(issuer, "/"),
		audience:   audience,
		httpClient: httpClient,
		keys:       map[string]any{},
		minRefresh: time.Minute,
	}
}

func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (Claims, error) {
	var claims struct {
		jwt.RegisteredClaims
		Email string `json:"email"`
	}
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Subject == "" {
		return Claims{}, fmt.Errorf("%w: sem sub", ErrInvalidToken)
	}
	return Claims{Subject: claims.Subject, Email: claims.Email}, nil
}

func (v *OIDCVerifier) key(ctx context.Context, kid string) (any, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	v.mu.RUnlock()
	if ok {
		return k, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q desconhecido", kid)
}

func (v *OIDCVerifier) refresh(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.lastRefresh.IsZero() && time.Since(v.lastRefresh) < v.minRefresh {
		return nil
	}
	v.lastRefresh = time.Now()

	if v.jwksURI == "" {
		var disc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, v.issuer+"/.well-known/openid-configuration", &disc); err != nil {
			return fmt.Errorf("discovery OIDC: %w", err)
		}
		if strings.TrimSuffix(disc.Issuer, "/") != v.issuer || disc.JWKSURI == "" {
			return fmt.Errorf("discovery OIDC: issuer não confere")
		}
		v.jwksURI = disc.JWKSURI
	}

	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, v.jwksURI, &set); err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]any{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil {
			continue // chave de tipo não suportado: ignora
		}
		keys[k.Kid] = pub
	}
	v.keys = keys
	return nil
}

func (v *OIDCVerifier) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

func (k jwk) publicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64int(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64int(k.E)
		if err != nil {
			return nil, err
		}
		if n.BitLen() < 2048 {
			return nil, errors.New("RSA abaixo de 2048 bits")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, errors.New("curva não suportada")
		}
		x, err := b64int(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64int(k.Y)
		if err != nil {
			return nil, err
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		if !pub.Curve.IsOnCurve(x, y) {
			return nil, errors.New("ponto fora da curva")
		}
		return pub, nil
	}
	return nil, errors.New("kty não suportado")
}
