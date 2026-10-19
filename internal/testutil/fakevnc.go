package testutil

// Servidor VNC (RFB 3.8) mínimo para o PVE falso: autenticação VNC com o
// ticket (como o Proxmox faz), uma tela de texto e eco do que se digita.
// Serve para a demo e para os testes do console de ponta a ponta.

import (
	"bufio"
	"context"
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
)

const vncW, vncH = 720, 400

type pixFmt struct {
	bigEndian        bool
	rs, gs, bs       uint8
	rmax, gmax, bmax uint16
}

func serveFakeVNC(ctx context.Context, conn net.Conn, password, vmName string) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "RFB 003.008\n"); err != nil {
		return
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(br, ver); err != nil {
		return
	}
	// Segurança: só VNC auth (tipo 2).
	if _, err := conn.Write([]byte{1, 2}); err != nil {
		return
	}
	choice, err := br.ReadByte()
	if err != nil || choice != 2 {
		return
	}
	challenge := make([]byte, 16)
	_, _ = rand.Read(challenge)
	if _, err := conn.Write(challenge); err != nil {
		return
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(br, resp); err != nil {
		return
	}
	if subtle.ConstantTimeCompare(resp, VNCResponse(password, challenge)) != 1 {
		msg := "senha VNC incorreta"
		out := binary.BigEndian.AppendUint32(nil, 1)
		out = binary.BigEndian.AppendUint32(out, uint32(len(msg)))
		_, _ = conn.Write(append(out, msg...))
		return
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return
	}
	if _, err := br.ReadByte(); err != nil { // ClientInit (shared)
		return
	}
	name := "mspcp " + vmName
	init := binary.BigEndian.AppendUint16(nil, vncW)
	init = binary.BigEndian.AppendUint16(init, vncH)
	init = append(init, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0)
	init = binary.BigEndian.AppendUint32(init, uint32(len(name)))
	init = append(init, name...)
	if _, err := conn.Write(init); err != nil {
		return
	}

	scr := &vncScreen{pf: pixFmt{rs: 16, gs: 8, bs: 0, rmax: 255, gmax: 255, bmax: 255}, vm: vmName}
	scr.lines = []string{
		"mspcp - console de demonstracao (PVE simulado)",
		"",
		"Debian GNU/Linux 12 " + vmName + " tty1",
		"",
	}
	scr.prompt()
	var mu sync.Mutex
	wantUpdate := false
	dirty := true
	send := func() error {
		mu.Lock()
		defer mu.Unlock()
		if !wantUpdate || !dirty {
			return nil
		}
		wantUpdate, dirty = false, false
		_, err := conn.Write(scr.frame())
		return err
	}
	for ctx.Err() == nil {
		typ, err := br.ReadByte()
		if err != nil {
			return
		}
		switch typ {
		case 0: // SetPixelFormat
			b := make([]byte, 19)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			p := b[3:]
			mu.Lock()
			scr.pf = pixFmt{bigEndian: p[2] != 0, rmax: binary.BigEndian.Uint16(p[4:]), gmax: binary.BigEndian.Uint16(p[6:]),
				bmax: binary.BigEndian.Uint16(p[8:]), rs: p[10], gs: p[11], bs: p[12]}
			dirty = true
			mu.Unlock()
		case 2: // SetEncodings
			b := make([]byte, 3)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(b[1:]))
			if _, err := io.ReadFull(br, make([]byte, 4*n)); err != nil {
				return
			}
		case 3: // FramebufferUpdateRequest
			b := make([]byte, 9)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			mu.Lock()
			wantUpdate = true
			if b[0] == 0 {
				dirty = true // pedido não incremental: manda tudo
			}
			mu.Unlock()
		case 4: // KeyEvent
			b := make([]byte, 7)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			if b[0] == 1 {
				mu.Lock()
				scr.key(binary.BigEndian.Uint32(b[3:]))
				dirty = true
				mu.Unlock()
			}
		case 5: // PointerEvent
			if _, err := io.ReadFull(br, make([]byte, 5)); err != nil {
				return
			}
		case 6: // ClientCutText
			b := make([]byte, 7)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			if _, err := io.ReadFull(br, make([]byte, binary.BigEndian.Uint32(b[3:]))); err != nil {
				return
			}
		default:
			return // mensagem que não simulamos
		}
		if err := send(); err != nil {
			return
		}
	}
}

// vncResponse cifra o desafio com a senha, como no VNC (DES com os bits de
// cada byte da chave invertidos).
func VNCResponse(password string, challenge []byte) []byte {
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			if b&(1<<j) != 0 {
				r |= 1 << (7 - j)
			}
		}
		key[i] = r
	}
	c, _ := des.NewCipher(key)
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out
}

type vncScreen struct {
	pf    pixFmt
	vm    string
	lines []string
	cur   string
	pass  bool
}

func (s *vncScreen) prompt() {
	if s.pass {
		s.cur = "Password: "
	} else {
		s.cur = s.vm + " login: "
	}
}

func (s *vncScreen) key(sym uint32) {
	switch {
	case sym == 0xff0d: // Enter
		s.lines = append(s.lines, s.cur)
		if s.pass {
			s.lines = append(s.lines, "", "Login incorrect (VM simulada)", "")
		}
		s.pass = !s.pass
		s.prompt()
	case sym == 0xff08: // Backspace
		min := len(s.vm + " login: ")
		if s.pass {
			min = len("Password: ")
		}
		if len(s.cur) > min {
			s.cur = s.cur[:len(s.cur)-1]
		}
	case sym >= 0x20 && sym <= 0x7e:
		if len(s.cur) < 56 {
			if s.pass {
				s.cur += "*"
			} else {
				s.cur += string(rune(sym))
			}
		}
	}
	if len(s.lines) > 18 {
		s.lines = s.lines[len(s.lines)-18:]
	}
}

// frame monta um FramebufferUpdate com um retângulo Raw da tela inteira.
func (s *vncScreen) frame() []byte {
	const scale, cw, ch = 2, 12, 18
	fg := [3]uint8{0xd8, 0xde, 0xe9}
	bg := [3]uint8{0x10, 0x14, 0x1c}
	accent := [3]uint8{0x5b, 0x9b, 0xff}
	pix := make([][3]uint8, vncW*vncH)
	for i := range pix {
		pix[i] = bg
	}
	draw := func(row int, text string, color [3]uint8) {
		y0 := 16 + row*ch
		for i, r := range text {
			g, ok := font5x7[r]
			if !ok {
				g = font5x7['?']
			}
			x0 := 16 + i*cw
			for col := 0; col < 5; col++ {
				bits := g[col]
				for ry := 0; ry < 7; ry++ {
					if bits&(1<<ry) == 0 {
						continue
					}
					for dy := 0; dy < scale; dy++ {
						for dx := 0; dx < scale; dx++ {
							x, y := x0+col*scale+dx, y0+ry*scale+dy
							if x < vncW && y < vncH {
								pix[y*vncW+x] = color
							}
						}
					}
				}
			}
		}
	}
	for i, l := range s.lines {
		c := fg
		if i == 0 {
			c = accent
		}
		draw(i, l, c)
	}
	draw(len(s.lines), s.cur+"_", fg)

	out := []byte{0, 0, 0, 1}
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint16(out, 0)
	out = binary.BigEndian.AppendUint16(out, vncW)
	out = binary.BigEndian.AppendUint16(out, vncH)
	out = binary.BigEndian.AppendUint32(out, 0) // Raw
	scaleTo := func(v uint8, max uint16) uint32 { return uint32(v) * uint32(max) / 255 }
	buf := make([]byte, 4)
	for _, p := range pix {
		v := scaleTo(p[0], s.pf.rmax)<<s.pf.rs | scaleTo(p[1], s.pf.gmax)<<s.pf.gs | scaleTo(p[2], s.pf.bmax)<<s.pf.bs
		if s.pf.bigEndian {
			binary.BigEndian.PutUint32(buf, v)
		} else {
			binary.LittleEndian.PutUint32(buf, v)
		}
		out = append(out, buf...)
	}
	return out
}

// Fonte 5x7 clássica (colunas, bit 0 = linha de cima).
var font5x7 = func() map[rune][5]byte {
	const chars = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"
	data := strings.Fields(`
00,00,00,00,00 00,00,5F,00,00 00,07,00,07,00 14,7F,14,7F,14 24,2A,7F,2A,12 23,13,08,64,62 36,49,55,22,50
00,05,03,00,00 00,1C,22,41,00 00,41,22,1C,00 08,2A,1C,2A,08 08,08,3E,08,08 00,50,30,00,00 08,08,08,08,08
00,60,60,00,00 20,10,08,04,02 3E,51,49,45,3E 00,42,7F,40,00 42,61,51,49,46 21,41,45,4B,31 18,14,12,7F,10
27,45,45,45,39 3C,4A,49,49,30 01,71,09,05,03 36,49,49,49,36 06,49,49,29,1E 00,36,36,00,00 00,56,36,00,00
00,08,14,22,41 14,14,14,14,14 41,22,14,08,00 02,01,51,09,06 32,49,79,41,3E 7E,11,11,11,7E 7F,49,49,49,36
3E,41,41,41,22 7F,41,41,22,1C 7F,49,49,49,41 7F,09,09,01,01 3E,41,41,51,32 7F,08,08,08,7F 00,41,7F,41,00
20,40,41,3F,01 7F,08,14,22,41 7F,40,40,40,40 7F,02,04,02,7F 7F,04,08,10,7F 3E,41,41,41,3E 7F,09,09,09,06
3E,41,51,21,5E 7F,09,19,29,46 46,49,49,49,31 01,01,7F,01,01 3F,40,40,40,3F 1F,20,40,20,1F 7F,20,18,20,7F
63,14,08,14,63 03,04,78,04,03 61,51,49,45,43 00,00,7F,41,41 02,04,08,10,20 41,41,7F,00,00 04,02,01,02,04
40,40,40,40,40 00,01,02,04,00 20,54,54,54,78 7F,48,44,44,38 38,44,44,44,20 38,44,44,48,7F 38,54,54,54,18
08,7E,09,01,02 08,14,54,54,3C 7F,08,04,04,78 00,44,7D,40,00 20,40,44,3D,00 00,7F,10,28,44 00,41,7F,40,00
7C,04,18,04,78 7C,08,04,04,78 38,44,44,44,38 7C,14,14,14,08 08,14,14,18,7C 7C,08,04,04,08 48,54,54,54,20
04,3F,44,40,20 3C,40,40,20,7C 1C,20,40,20,1C 3C,40,30,40,3C 44,28,10,28,44 0C,50,50,50,3C 44,64,54,4C,44
00,08,36,41,00 00,00,7F,00,00 00,41,36,08,00 08,04,08,10,08`)
	m := map[rune][5]byte{}
	for i, r := range chars {
		var g [5]byte
		for j, h := range strings.Split(data[i], ",") {
			var v byte
			for _, c := range h {
				v <<= 4
				switch {
				case c >= '0' && c <= '9':
					v |= byte(c - '0')
				default:
					v |= byte(c-'A') + 10
				}
			}
			g[j] = v
		}
		m[r] = g
	}
	return m
}()
