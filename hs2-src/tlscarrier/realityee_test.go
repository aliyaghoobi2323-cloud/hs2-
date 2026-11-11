package tlscarrier

// §11.8.3: the authenticated handshake's EncryptedExtensions equals the
// target's, byte for byte (so the same length and extension list), for a
// target that acks the SNI (OpenSSL) and one that does not (nginx, Go).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// lockedBuf is a key log (written by handshake goroutines).
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// serverHSSecret is the last SERVER_HANDSHAKE_TRAFFIC_SECRET in the log.
func (l *lockedBuf) serverHSSecret() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sec []byte
	sc := bufio.NewScanner(bytes.NewReader(l.b.Bytes()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 && f[0] == "SERVER_HANDSHAKE_TRAFFIC_SECRET" {
			sec, _ = hex.DecodeString(f[2])
		}
	}
	return sec
}

func hkdfExpandLabel(h func() hash.Hash, secret []byte, label string, n int) []byte {
	var info []byte
	info = binary.BigEndian.AppendUint16(info, uint16(n))
	info = append(info, byte(len("tls13 "+label)))
	info = append(info, "tls13 "+label...)
	info = append(info, 0)
	out := make([]byte, n)
	io.ReadFull(hkdf.Expand(h, secret, info), out)
	return out
}

// decryptEE finds the ServerHello and the first encrypted record in the
// server's bytes and returns the EncryptedExtensions message and that
// record's length.
func decryptEE(t *testing.T, in, secret []byte) (ee []byte, recLen int) {
	t.Helper()
	var suite uint16
	for off := 0; off+5 <= len(in); {
		typ, l := in[off], int(binary.BigEndian.Uint16(in[off+3:]))
		if off+5+l > len(in) {
			break
		}
		body := in[off+5 : off+5+l]
		if typ == 22 && body[0] == 2 {
			th, ok := parseServerHello(body)
			if !ok {
				t.Fatal("no TLS 1.3 ServerHello")
			}
			suite = th.suite
		}
		if typ == 23 {
			h, kl := sha256.New, 16
			switch suite {
			case tls.TLS_AES_256_GCM_SHA384:
				h, kl = sha512.New384, 32
			case tls.TLS_CHACHA20_POLY1305_SHA256:
				kl = 32
			}
			key, iv := hkdfExpandLabel(h, secret, "key", kl), hkdfExpandLabel(h, secret, "iv", 12)
			var a cipher.AEAD
			if suite == tls.TLS_CHACHA20_POLY1305_SHA256 {
				a, _ = chacha20poly1305.New(key)
			} else {
				b, _ := aes.NewCipher(key)
				a, _ = cipher.NewGCM(b)
			}
			pt, err := a.Open(nil, iv, body, in[off:off+5]) // sequence 0: nonce = iv
			if err != nil {
				t.Fatalf("decrypt the first encrypted record (suite %#x): %v", suite, err)
			}
			pt = bytes.TrimRight(pt, "\x00")
			if len(pt) < 5 || pt[len(pt)-1] != 22 || pt[0] != 8 {
				t.Fatalf("first encrypted record is not EncryptedExtensions: % x", pt)
			}
			pt = pt[:len(pt)-1]
			ml := int(pt[1])<<16 | int(pt[2])<<8 | int(pt[3])
			return pt[:4+ml], l
		}
		off += 5 + l
	}
	t.Fatal("no encrypted record")
	return nil, 0
}

// eeExtensions lists the extension ids of an EncryptedExtensions message.
func eeExtensions(ee []byte) []uint16 {
	var ids []uint16
	e := ee[6:]
	for len(e) >= 4 {
		ids = append(ids, binary.BigEndian.Uint16(e))
		e = e[4+int(binary.BigEndian.Uint16(e[2:])):]
	}
	return ids
}

func hasExt(ee []byte, id uint16) bool { return slices.Contains(eeExtensions(ee), id) }

// eeTapProxy forwards to dst and records what dst sends back.
type eeTapProxy struct {
	ln  net.Listener
	mu  sync.Mutex
	out []byte
}

func newEETapProxy(t *testing.T, dst string) *eeTapProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &eeTapProxy{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s, err := net.Dial("tcp", dst)
				if err != nil {
					return
				}
				defer s.Close()
				go func() { io.Copy(s, c); s.Close() }() // s_server serves one connection at a time
				b := make([]byte, 16<<10)
				for {
					n, err := s.Read(b)
					p.mu.Lock()
					p.out = append(p.out, b[:n]...)
					p.mu.Unlock()
					if _, werr := c.Write(b[:n]); err != nil || werr != nil {
						return
					}
				}
			}()
		}
	}()
	return p
}

func (p *eeTapProxy) bytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.out...)
}

// directEE: a Chrome hello straight to the target, its EE and record length.
func directEE(t *testing.T, target, sni string) ([]byte, int) {
	px := newEETapProxy(t, target)
	kl := &lockedBuf{}
	c, err := net.Dial("tcp", px.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u := utls.UClient(c, &utls.Config{ServerName: sni, InsecureSkipVerify: true, KeyLogWriter: kl}, clientHello)
	u.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		t.Fatalf("direct handshake: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return decryptEE(t, px.bytes(), kl.serverHSSecret())
}

// opensslTarget runs `openssl s_server` (TLS 1.3, ALPN h2) with cert; "" when
// openssl is not installed.
//
// servername: also give it -servername (s_server then installs an SNI
// callback; whether OpenSSL acks depends on that).
func opensslTarget(t *testing.T, cert tls.Certificate, servername bool) string {
	bin, err := exec.LookPath("openssl")
	if err != nil {
		return ""
	}
	dir := t.TempDir()
	kd, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	os.WriteFile(filepath.Join(dir, "c.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600)
	os.WriteFile(filepath.Join(dir, "k.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	args := []string{"s_server", "-accept", addr, "-cert", filepath.Join(dir, "c.pem"), "-key", filepath.Join(dir, "k.pem"),
		"-tls1_3", "-alpn", "h2,http/1.1", "-quiet"}
	if servername {
		args = append(args, "-servername", "www.target.test", "-cert2", filepath.Join(dir, "c.pem"), "-key2", filepath.Join(dir, "k.pem"))
	}
	cmd := exec.Command(bin, args...)
	stdin, _ := cmd.StdinPipe() // kept open: s_server stops at stdin EOF
	if err := cmd.Start(); err != nil {
		return ""
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close() // s_server serves one connection at a time: this one ends at once
			return addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("openssl s_server did not start")
	return ""
}

func TestRealityEEParity(t *testing.T) {
	cert := testCert(t, "www.target.test")
	// kind: "utls-ack" (the fork with ServerNameAck: OpenSSL-like),
	// "utls-noack" (upstream uTLS, nginx-like), "go" (crypto/tls: Go 1.27
	// acks, after ALPN)
	serve := func(kind string) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					var s interface{ Handshake() error }
					if kind == "go" {
						s = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}})
					} else {
						s = utls.Server(c, &utls.Config{Certificates: []utls.Certificate{{Certificate: cert.Certificate, PrivateKey: cert.PrivateKey}},
							NextProtos: []string{"h2", "http/1.1"}, ServerNameAck: kind == "utls-ack"})
					}
					c.SetDeadline(time.Now().Add(5 * time.Second))
					s.Handshake()
					io.Copy(io.Discard, c)
				}()
			}
		}()
		return ln.Addr().String()
	}
	type target struct {
		name, addr string
		ack        int // 1 / 0: must (not) ack; -1: whatever the target does
	}
	targets := []target{{"utls-noack", serve("utls-noack"), 0}, {"utls-ack", serve("utls-ack"), 1}, {"go", serve("go"), -1}}
	for _, sn := range []bool{false, true} {
		if a := opensslTarget(t, cert, sn); a != "" {
			targets = append(targets, target{fmt.Sprintf("openssl-servername=%v", sn), a, -1})
		} else {
			t.Log("openssl not installed: the real OpenSSL targets are skipped")
		}
	}
	kl := &lockedBuf{}
	w := io.Writer(kl)
	realityKeyLog.Store(&w)
	defer realityKeyLog.Store(nil)
	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			want, wantRec := directEE(t, tg.addr, "www.target.test")
			t.Logf("%s: target EE % x", tg.name, want)
			ack := hasExt(want, 0)
			if tg.ack >= 0 && ack != (tg.ack == 1) {
				t.Fatalf("target EE extensions %v: the ack is not as the target is set up", eeExtensions(want))
			}
			l := newRealityLab(t, tg.addr, nil)
			if _, err := l.srv.targetChainSize(context.Background()); err != nil {
				t.Fatal(err)
			}
			m := l.srv.Mirror()
			if m.SNIAck != ack || m.EELen != wantRec {
				t.Fatalf("mirror SNIAck %v EELen %d, want %v %d", m.SNIAck, m.EELen, ack, wantRec)
			}
			px := newEETapProxy(t, l.addr)
			car, err := DialPathFront(context.Background(), DialPath{Addr: px.ln.Addr().String(), Key: l.key, Profile: l.dprof, Connect: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer car.Close()
			got, gotRec := decryptEE(t, px.bytes(), kl.serverHSSecret())
			t.Logf("%s: target EE record %d B ext %v; ours %d B ext %v", tg.name, wantRec, eeExtensions(want), gotRec, eeExtensions(got))
			// The same length and the same extensions. The order inside the
			// encrypted EE is invisible on the wire (OpenSSL writes server_name
			// first, as we do; Go 1.27 after ALPN), so it is not compared.
			gi, wi := eeExtensions(got), eeExtensions(want)
			slices.Sort(gi)
			slices.Sort(wi)
			if gotRec != wantRec || len(got) != len(want) || !slices.Equal(gi, wi) {
				t.Fatalf("EE differs: ours %d B % x, target %d B % x", gotRec, got, wantRec, want)
			}
		})
	}
	// the record-length rule itself
	for _, c := range []struct {
		rec  int
		alpn string
		want bool
	}{{32, "h2", false}, {36, "h2", true}, {23, "", false}, {27, "", true}, {42, "http/1.1", true}, {38, "http/1.1", false}, {40, "h2", false}} {
		if eeSNIAck(c.rec, c.alpn) != c.want {
			t.Fatalf("eeSNIAck(%d, %q) != %v", c.rec, c.alpn, c.want)
		}
	}
}

// ee51: the post-Finished records are replayed as one observed sequence
// (count and order), not as random picks: www.digikala.com sends two tickets
// then a 57 B h2 SETTINGS record; random picks gave [306 306 306] / [57 306
// 306] on the lab.
func TestRealityPostFinishedSequence(t *testing.T) {
	cert := testCert(t, "www.target.test")
	l := newRealityLab(t, ticketTarget(t, cert, []int{306, 306, 57}), nil)
	if _, err := l.srv.targetChainSize(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.srv.sampleNST(context.Background())
	if m := l.srv.Mirror(); len(m.NSTSeqs) != nstProbes {
		t.Fatalf("sequences %v", m.NSTSeqs)
	}
	px := newTapProxy(t, l.addr)
	for i := 0; i < 12; i++ {
		car, err := DialPathFront(context.Background(), DialPath{Addr: px.addr, Key: l.key, Profile: l.dprof, Connect: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		echoOK(t, car)
		car.Close()
		// ServerHello(-) CCS(-) EE Cert CV Fin, then the target's sequence
		if got := px.last(); len(got) < 9 || fmt.Sprint(got[6:9]) != "[306 306 57]" {
			t.Fatalf("dial %d: records %v, want [306 306 57] after Finished", i, got)
		}
	}
}
