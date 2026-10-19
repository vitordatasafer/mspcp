// Package localauth tem as peças do login próprio: hash de senha (argon2id),
// geração de senhas temporárias, TOTP (RFC 6238) e um limitador de
// tentativas em memória.
package localauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// ---------- senha ----------

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
)

// MinPasswordLen é o tamanho mínimo aceito para senhas escolhidas pelo usuário.
const MinPasswordLen = 10

func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		b.EncodeToString(salt), b.EncodeToString(key)), nil
}

var ErrBadHash = errors.New("hash de senha inválido")

func CheckPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrBadHash
	}
	b := base64.RawStdEncoding
	salt, err := b.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := b.DecodeString(parts[5])
	if err != nil {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyCheck gasta o mesmo tempo de uma verificação real; usado quando o
// usuário não existe, para não revelar isso pelo tempo de resposta.
func DummyCheck(pw string) {
	salt := make([]byte, 16)
	argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// ValidatePassword aplica a regra mínima para senhas definidas pelo usuário.
func ValidatePassword(pw, username string) error {
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("a senha precisa ter pelo menos %d caracteres", MinPasswordLen)
	}
	if strings.EqualFold(pw, username) {
		return errors.New("a senha não pode ser igual ao usuário")
	}
	return nil
}

// TempPassword gera uma senha temporária legível (sem caracteres ambíguos).
func TempPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var sb strings.Builder
	for i := 0; i < 16; i++ {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		sb.WriteByte(alphabet[n.Int64()])
	}
	return sb.String()
}

// NewToken gera um token aleatório de 32 bytes em base64url.
func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- TOTP ----------

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTPURI monta o otpauth:// para o app autenticador.
func TOTPURI(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

func totpAt(secret string, counter uint64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

// TOTPCode devolve o código do momento (usado nos testes).
func TOTPCode(secret string, t time.Time) string {
	c, _ := totpAt(secret, uint64(t.Unix()/30))
	return c
}

// CheckTOTP aceita o código do período atual e de um período antes ou
// depois (relógios levemente fora de sincronia).
func CheckTOTP(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(code) != 6 {
		return false
	}
	step := now.Unix() / 30
	for d := int64(-1); d <= 1; d++ {
		want, err := totpAt(secret, uint64(step+d))
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// ---------- limitador de tentativas ----------

// Limiter bloqueia uma chave (usuário ou IP) por um tempo depois de várias
// falhas seguidas. Em memória: reiniciar o serviço zera os contadores.
type Limiter struct {
	Max   int
	Block time.Duration

	mu    sync.Mutex
	fails map[string]*entry
}

type entry struct {
	n     int
	until time.Time
}

func NewLimiter(max int, block time.Duration) *Limiter {
	return &Limiter{Max: max, Block: block, fails: map[string]*entry{}}
}

// Blocked diz se a chave está bloqueada e por quanto tempo ainda.
func (l *Limiter) Blocked(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.fails[key]
	if e == nil || now.After(e.until) {
		return false, 0
	}
	return e.n >= l.Max, e.until.Sub(now)
}

func (l *Limiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.fails[key]
	if e == nil || now.After(e.until) {
		e = &entry{}
		l.fails[key] = e
	}
	e.n++
	e.until = now.Add(l.Block)
	if len(l.fails) > 100_000 { // limpeza simples para não crescer sem fim
		for k, v := range l.fails {
			if now.After(v.until) {
				delete(l.fails, k)
			}
		}
	}
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
