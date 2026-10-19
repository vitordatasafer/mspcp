package localauth

import (
	"strings"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("senha-forte-123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash inesperado: %s", h)
	}
	if ok, _ := CheckPassword(h, "senha-forte-123"); !ok {
		t.Fatal("senha certa recusada")
	}
	if ok, _ := CheckPassword(h, "senha-forte-124"); ok {
		t.Fatal("senha errada aceita")
	}
	if _, err := CheckPassword("lixo", "x"); err == nil {
		t.Fatal("hash inválido deveria dar erro")
	}
	if ValidatePassword("curta", "x") == nil || ValidatePassword("Fulano1234", "fulano1234") == nil {
		t.Fatal("regras de senha não aplicadas")
	}
	if p := TempPassword(); len(p) != 19 || ValidatePassword(p, "x") != nil {
		t.Fatalf("senha temporária inválida: %q", p)
	}
}

// Vetor do RFC 6238 (SHA1, segredo "12345678901234567890").
func TestTOTPRFC(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	cases := map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range cases {
		if got := TOTPCode(secret, time.Unix(ts, 0)); got != want {
			t.Errorf("t=%d: %s, esperava %s", ts, got, want)
		}
	}
	now := time.Unix(1111111109, 0)
	if !CheckTOTP(secret, "081804", now) || !CheckTOTP(secret, TOTPCode(secret, now.Add(30*time.Second)), now) {
		t.Fatal("código válido recusado")
	}
	if CheckTOTP(secret, TOTPCode(secret, now.Add(5*time.Minute)), now) || CheckTOTP(secret, "12345", now) {
		t.Fatal("código fora da janela aceito")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if b, _ := l.Blocked("a", now); b {
			t.Fatalf("bloqueou cedo demais (%d)", i)
		}
		l.Fail("a", now)
	}
	if b, _ := l.Blocked("a", now); !b {
		t.Fatal("deveria bloquear após 3 falhas")
	}
	if b, _ := l.Blocked("a", now.Add(2*time.Minute)); b {
		t.Fatal("bloqueio deveria expirar")
	}
	l.Reset("a")
	if b, _ := l.Blocked("a", now); b {
		t.Fatal("reset não liberou")
	}
}

func TestSHA512Crypt(t *testing.T) {
	// Vetores conferidos com `openssl passwd -6 -salt <salt> <senha>`.
	for _, c := range []struct{ pw, salt, want string }{
		{"Hello world!", "saltstring", "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"senha com espaço e ç", "ab./XY12", "$6$ab./XY12$jyesJH2UvG3dYx.hISULBWX8KI7siCwrt.7hRh/uCl/srbAHhLi8Lo/SmeL5lT5fcV8efewHD79Ln1UvhSztf."},
	} {
		if got := sha512CryptSalt([]byte(c.pw), []byte(c.salt)); got != c.want {
			t.Errorf("sha512crypt(%q,%q) = %s", c.pw, c.salt, got)
		}
	}
	if h := SHA512Crypt("x"); !strings.HasPrefix(h, "$6$") || len(h) != 3+16+1+86 {
		t.Fatalf("formato inesperado: %s", h)
	}
}
