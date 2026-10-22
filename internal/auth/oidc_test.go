package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/vitordatasafer/mspvdc/internal/auth"
	"github.com/vitordatasafer/mspvdc/internal/testutil"
)

func TestOIDCVerifier(t *testing.T) {
	idp := testutil.NewFakeOIDC(t, "mspvdc")
	v := auth.NewOIDCVerifier(idp.Issuer, "mspvdc", nil)
	ctx := context.Background()

	c, err := v.Verify(ctx, idp.Token(t, "alice", time.Minute))
	if err != nil || c.Subject != "alice" {
		t.Fatalf("token válido recusado: %v", err)
	}

	cases := map[string]string{
		"expirado":       idp.Token(t, "alice", -time.Hour),
		"audience outra": idp.TokenWith(t, jwt.MapClaims{"iss": idp.Issuer, "aud": "outro", "sub": "a", "exp": time.Now().Add(time.Minute).Unix()}),
		"issuer outro":   idp.TokenWith(t, jwt.MapClaims{"iss": "https://evil", "aud": "mspvdc", "sub": "a", "exp": time.Now().Add(time.Minute).Unix()}),
		"sem exp":        idp.TokenWith(t, jwt.MapClaims{"iss": idp.Issuer, "aud": "mspvdc", "sub": "a"}),
		"sem sub":        idp.TokenWith(t, jwt.MapClaims{"iss": idp.Issuer, "aud": "mspvdc", "exp": time.Now().Add(time.Minute).Unix()}),
		"lixo":           "abc.def.ghi",
	}
	// HS256 assinado com a "chave pública" (ataque clássico de confusão de algoritmo).
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": idp.Issuer, "aud": "mspvdc", "sub": "a", "exp": time.Now().Add(time.Minute).Unix()})
	hs.Header["kid"] = idp.KID
	cases["hs256"], _ = hs.SignedString([]byte("qualquer"))
	// alg none
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": idp.Issuer, "aud": "mspvdc", "sub": "a", "exp": time.Now().Add(time.Minute).Unix()})
	cases["none"], _ = none.SignedString(jwt.UnsafeAllowNoneSignatureType)

	for name, tok := range cases {
		if _, err := v.Verify(ctx, tok); err == nil {
			t.Errorf("%s: deveria ser recusado", name)
		}
	}
}
