package localauth

import (
	"crypto/rand"
	"crypto/sha512"
	"strings"
)

// SHA512Crypt gera um hash "$6$salt$..." (crypt(3) SHA-512, 5000 rounds).
// É o formato que o cloud-init e o /etc/shadow entendem; o Proxmox grava o
// valor como veio quando ele já está nesse formato, então a senha da VM não
// precisa trafegar nem ficar guardada em texto puro.
func SHA512Crypt(pw string) string {
	const saltChars = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = saltChars[int(b[i])%len(saltChars)]
	}
	return sha512CryptSalt([]byte(pw), b)
}

func sha512CryptSalt(pw, salt []byte) string {
	const rounds = 5000
	if len(salt) > 16 {
		salt = salt[:16]
	}
	h := sha512.New()
	h.Write(pw)
	h.Write(salt)
	h.Write(pw)
	B := h.Sum(nil)

	h.Reset()
	h.Write(pw)
	h.Write(salt)
	for n := len(pw); n > 0; n -= 64 {
		if n > 64 {
			h.Write(B)
		} else {
			h.Write(B[:n])
		}
	}
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 != 0 {
			h.Write(B)
		} else {
			h.Write(pw)
		}
	}
	A := h.Sum(nil)

	h.Reset()
	for range len(pw) {
		h.Write(pw)
	}
	DP := h.Sum(nil)
	P := repeatTo(DP, len(pw))

	h.Reset()
	for range 16 + int(A[0]) {
		h.Write(salt)
	}
	DS := h.Sum(nil)
	S := repeatTo(DS, len(salt))

	C := A
	for i := range rounds {
		h.Reset()
		if i&1 != 0 {
			h.Write(P)
		} else {
			h.Write(C)
		}
		if i%3 != 0 {
			h.Write(S)
		}
		if i%7 != 0 {
			h.Write(P)
		}
		if i&1 != 0 {
			h.Write(C)
		} else {
			h.Write(P)
		}
		C = h.Sum(nil)
	}

	const alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var out strings.Builder
	enc := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			out.WriteByte(alphabet[w&0x3f])
			w >>= 6
		}
	}
	order := [][3]int{{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
		{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41}}
	for _, o := range order {
		enc(C[o[0]], C[o[1]], C[o[2]], 4)
	}
	enc(0, 0, C[63], 2)
	return "$6$" + string(salt) + "$" + out.String()
}

func repeatTo(src []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		k := n - len(out)
		if k > len(src) {
			k = len(src)
		}
		out = append(out, src[:k]...)
	}
	return out
}
