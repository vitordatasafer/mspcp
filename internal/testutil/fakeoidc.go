package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// FakeOIDC é um IdP mínimo: discovery + JWKS com uma chave RSA.
type FakeOIDC struct {
	Server   *httptest.Server
	Issuer   string
	Audience string
	Key      *rsa.PrivateKey
	KID      string
}

func NewFakeOIDC(t *testing.T, audience string) *FakeOIDC {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeOIDC{Audience: audience, Key: key, KID: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.Issuer, "jwks_uri": f.Issuer + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.KID, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	f.Server = httptest.NewServer(mux)
	f.Issuer = f.Server.URL
	t.Cleanup(f.Server.Close)
	return f
}

// Token emite um access token válido por ttl.
func (f *FakeOIDC) Token(t *testing.T, sub string, ttl time.Duration) string {
	return f.TokenWith(t, jwt.MapClaims{
		"iss": f.Issuer, "aud": f.Audience, "sub": sub,
		"iat": time.Now().Unix(), "exp": time.Now().Add(ttl).Unix(),
	})
}

func (f *FakeOIDC) TokenWith(t *testing.T, claims jwt.MapClaims) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.KID
	s, err := tok.SignedString(f.Key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
