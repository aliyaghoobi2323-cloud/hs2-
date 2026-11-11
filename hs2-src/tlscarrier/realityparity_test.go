package tlscarrier

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// plainHello is a Chrome ClientHello for sni without any reality signal.
func plainHello(t *testing.T, sni string) []byte {
	t.Helper()
	c1, s1 := net.Pipe()
	ch := make(chan []byte, 1)
	go func() {
		b := make([]byte, 16<<10)
		n, _ := s1.Read(b)
		ch <- append([]byte(nil), b[:n]...)
		s1.Close()
	}()
	u := utls.UClient(c1, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, clientHello)
	go u.Handshake()
	h := <-ch
	c1.Close()
	return h
}

// sec41: a stranger's splice (plain bytes, a foreign SNI, no SNI) is no relay:
// 32 KB free then 8 KB/s even when the profile allows more.
func TestRealityForeignSpliceLimits(t *testing.T) {
	rt := newRawTarget(t, "T", 400<<10)
	l := newRealityLab(t, rt.ln.Addr().String(), func(p *FrontProfile) {
		p.FallbackMaxBytes, p.FallbackRateBps = 1<<20, 1<<20
	})
	for _, first := range [][]byte{[]byte("GET / HTTP/1.1\r\n\r\n"), plainHello(t, "www.cloudflare.com")} {
		c, err := net.Dial("tcp", l.addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write(first)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := io.Copy(io.Discard, c)
		c.Close()
		// 32 KB + 2 s at 8 KB/s (+ one 16 KB chunk of slack)
		if n < 32<<10 || n > 32<<10+16<<10+16<<10 {
			t.Fatalf("stranger got %d B in 2 s", n)
		}
	}
}

// sec41: reconnecting does not reset the free allowance of a client net.
func TestRealityNetBudget(t *testing.T) {
	var b netBudget
	got := int64(0)
	for i := 0; i < 100; i++ {
		got += b.take("1.2.3.0", 256<<10)
	}
	if got != netFreeBudget {
		t.Fatalf("free bytes over 100 connections %d, want the budget %d", got, netFreeBudget)
	}
	if b.take("1.2.4.0", 100) != 100 {
		t.Fatal("another net is not limited")
	}
}

// sec41: the authenticated handshake uses the target's suite and group.
func TestRealityTargetSuiteParity(t *testing.T) {
	cert := testCert(t, "www.target.test")
	for _, cs := range []uint16{tls.TLS_AES_256_GCM_SHA384, tls.TLS_AES_128_GCM_SHA256, tls.TLS_CHACHA20_POLY1305_SHA256} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		// a "target" that answers with suite cs (we write its ServerHello by hand
		// through a uTLS server with that preference)
		go func(cs uint16) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					rel := lockSuite(cs)
					s := utls.Server(&firstWriteConn{Conn: c, f: rel}, &utls.Config{Certificates: []utls.Certificate{{Certificate: cert.Certificate, PrivateKey: cert.PrivateKey}},
						CurvePreferences: []utls.CurveID{utls.X25519}})
					s.Handshake()
					rel()
					c.Close()
				}()
			}
		}(cs)
		l := newRealityLab(t, ln.Addr().String(), nil)
		car, err := l.dial(l.dprof)
		if err != nil {
			t.Fatal(err)
		}
		got := car.conn.(*utls.UConn).ConnectionState()
		if got.CipherSuite != cs {
			t.Fatalf("suite %#x, want the target's %#x", got.CipherSuite, cs)
		}
		car.Close()
		ln.Close()
	}
	if len(utlsSuites13) != 3 || utlsSuites13[0] != tls.TLS_AES_128_GCM_SHA256 {
		t.Fatalf("defaults not restored: %#x", utlsSuites13)
	}
}

func TestParseServerHelloHRR(t *testing.T) {
	m := make([]byte, 4+2+32+1)
	m[0] = 2
	copy(m[6:], hrrRandom)
	if _, ok := parseServerHello(m); ok {
		t.Fatal("HRR taken for a ServerHello")
	}
}

// sec41: the net budget is charged with bytes actually spliced: many small
// visits from one /24 stay full speed, a bulk reader is capped once the
// budget is used up even across reconnects.
func TestRealitySpliceNetBudgetCharged(t *testing.T) {
	rt := newRawTarget(t, "T", 300<<10)
	l := newRealityLab(t, rt.ln.Addr().String(), func(p *FrontProfile) {
		p.FallbackMaxBytes, p.FallbackRateBps = 1<<20, 64<<10
	})
	l.srv.budget.take("127.0.0.0", netFreeBudget-100<<10) // the net has used most of its minute
	c, _ := net.Dial("tcp", l.addr)
	c.Write(plainHello(t, "www.target.test"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := io.Copy(io.Discard, c)
	c.Close()
	// 100 KB free, then 2 s at 64 KB/s (+ a chunk of slack)
	if n < 100<<10 || n > 100<<10+128<<10+32<<10 {
		t.Fatalf("got %d B in 2 s", n)
	}
}
