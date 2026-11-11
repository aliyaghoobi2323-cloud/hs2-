package tlscarrier

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func rsaCert(t *testing.T, cn string, bits int) tls.Certificate {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: cn, Organization: []string{"Target Test Org"}},
		DNSNames:  []string{cn, "static.target.test", "img.target.test", "api.target.test", "cdn.target.test", "m.target.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// ticketTarget is a TLS 1.3 server that, like OpenSSL, sends records of the
// given lengths right after the client's Finished (on the wire a TLS 1.3
// NewSessionTicket is an application_data record like these).
func ticketTarget(t *testing.T, cert tls.Certificate, nst []int) string {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert},
		CurvePreferences: []tls.CurveID{tls.X25519}, SessionTicketsDisabled: true, MinVersion: tls.VersionTLS13})
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
				tc := c.(*tls.Conn)
				if tc.Handshake() != nil {
					return
				}
				for _, l := range nst {
					tc.Write(make([]byte, l-padOverhead))
				}
				tc.SetReadDeadline(time.Now().Add(2 * time.Second))
				io.Copy(io.Discard, tc)
			}()
		}
	}()
	return ln.Addr().String()
}

// tapProxy forwards to dst and records the server->client record lengths of
// each connection (application_data > 0, others < 0).
type tapProxy struct {
	addr string
	mu   sync.Mutex
	taps []*recordTap
}

func newTapProxy(t *testing.T, dst string) *tapProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &tapProxy{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", dst)
			if err != nil {
				c.Close()
				continue
			}
			tap := &recordTap{Conn: u}
			p.mu.Lock()
			p.taps = append(p.taps, tap)
			p.mu.Unlock()
			go func() { io.Copy(u, c); u.Close() }()
			go func() { io.Copy(c, tap); c.Close() }()
		}
	}()
	return p
}

func (p *tapProxy) last() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.taps[len(p.taps)-1]
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]int(nil), t.lens...)
}

// Items 4+5 (phase 4b): an RSA 2048 target leaf -> our throw-away cert is
// RSA 2048 (CertificateVerify record 281 B like OpenSSL's rsa_pss 2048); the
// target's post-Finished tickets (266/282) are mirrored as pad records right
// after our Finished — only to a dialer announcing realityCapPad; an old
// dialer (no bit) gets none and still connects.
func TestRealityRSAKeyAndTicketPads(t *testing.T) {
	cert := rsaCert(t, "www.target.test", 2048)
	l := newRealityLab(t, ticketTarget(t, cert, []int{266, 282}), nil)
	n, err := l.srv.targetChainSize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.srv.sampleNST(context.Background())
	m := l.srv.Mirror()
	if m.KeyKind != "rsa" || m.KeyBits != 2048 || fmt.Sprint(m.NST) != "[266 282]" {
		t.Fatalf("mirror %+v", m)
	}
	if k := l.srv.key.Load(); k.kind != "rsa" || k.bits != 2048 {
		t.Fatalf("cert key %s %d", k.kind, k.bits)
	}
	l.srv.CertPad.Store(n)
	px := newTapProxy(t, l.addr)
	dial := func() []int {
		car, err := DialPathFront(context.Background(), DialPath{Addr: px.addr, Key: l.key, Profile: l.dprof, Connect: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		echoOK(t, car)
		car.Close()
		return px.last()
	}
	// the target's own record sequence, through the same tap
	tp := newTapProxy(t, l.prof.Target)
	tc, err := tls.Dial("tcp", tp.addr, &tls.Config{InsecureSkipVerify: true, ServerName: "www.target.test"})
	if err != nil {
		t.Fatal(err)
	}
	tc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	tc.Read(make([]byte, 1))
	tc.Close()
	want := tp.last()
	got := dial()
	t.Logf("target records %v", want)
	t.Logf("ours (new dialer) %v", got)
	// ServerHello(-) CCS(-) EE Cert CV Fin 266 282 [proof, echo...]
	if len(got) < 8 || len(want) < 8 {
		t.Fatalf("records: ours %v target %v", got, want)
	}
	if fmt.Sprint(got[3:6]) != fmt.Sprint(want[3:6]) || got[6]+got[7] < 2*266 || got[6] > 282 || got[7] > 282 || got[6]%16 != 10 || got[7]%16 != 10 {
		t.Fatalf("Cert/CV/Fin/tickets: ours %v, target %v", got[3:8], want[3:8])
	}
	if m := l.srv.Mirror(); len(m.NSTSizes) != 2*nstProbes {
		t.Fatalf("ticket size samples %v", m.NSTSizes)
	}
	if got[4] != 281 {
		t.Fatalf("CertificateVerify record %d, want 281 (rsa_pss 2048)", got[4])
	}
	realityDialCaps = 0
	defer func() { realityDialCaps = realityCapPad }()
	old := dial()
	t.Logf("ours (old dialer) %v", old)
	if len(old) >= 8 && old[6] == 266 && old[7] == 282 {
		t.Fatalf("pads sent to an old dialer: %v", old)
	}
}

// Item 6: a target chain below our normal cert's size gets the minimal form,
// exactly sized when reachable (and the dialer still verifies the tag).
func TestRealityMinimalCert(t *testing.T) {
	rt := newRawTarget(t, "x", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	k := l.srv.key.Load()
	t.Logf("normal base %d B, minimal %d B", k.base, k.minLen)
	if k.minLen >= k.base-40 {
		t.Fatalf("minimal form %d B not much smaller than %d B", k.minLen, k.base)
	}
	for _, want := range []int64{int64(k.minLen) + 2, int64(k.base) - 5, 387} {
		l.srv.CertPad.Store(want)
		for i := 0; i < 10; i++ {
			c, err := l.srv.makeCert(make([]byte, 32), "www.target.test")
			if err != nil {
				t.Fatal(err)
			}
			got := int64(len(c.Certificate[0]))
			exp := want
			if want < int64(k.minLen) {
				exp = int64(k.minLen)
			}
			if got != exp {
				t.Fatalf("want %d: cert %d B, expected %d", want, got, exp)
			}
		}
	}
	l.srv.CertPad.Store(int64(k.minLen) + 2)
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatalf("dial with the minimal cert: %v", err)
	}
	car.Close()
}

// Item 1: on a "both" IP an unauthenticated request after a handshake with
// OUR certificate is answered by the target (HTTP layer), like plain HTTP
// and other SNIs on that IP.
func TestHybridOneOriginCover(t *testing.T) {
	cert := testCert(t, "www.target.test")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "target-origin")
		fmt.Fprintf(w, "target says hi")
	}), TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	go hs.ServeTLS(ln, "", "")
	t.Cleanup(func() { hs.Close() })
	l, _ := hybridLab(t, ln.Addr().String())
	get := func(addr, sni string, useTLS bool) string {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var rw net.Conn = c
		if useTLS {
			tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
			if err := tc.Handshake(); err != nil {
				t.Fatalf("%s: %v", sni, err)
			}
			rw = tc
		}
		rw.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(rw, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", sni)
		resp, err := http.ReadResponse(bufio.NewReader(rw), nil)
		if err != nil {
			t.Fatalf("%s tls=%v: %v", sni, useTLS, err)
		}
		b, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%d server=%q body=%q", resp.StatusCode, resp.Header.Get("Server"), strings.TrimSpace(string(b)))
	}
	direct := get(ln.Addr().String(), "www.target.test", true)
	other := get(l.addr, "other.example", true)
	ours := get(l.addr, "tun.ours.test", true)
	plainD := get(ln.Addr().String(), "x", false)
	plainB := get(l.addr, "x", false)
	t.Logf("direct %s | other SNI %s | our SNI %s | plain direct %s | plain both %s", direct, other, ours, plainD, plainB)
	if other != direct || ours != direct || plainB != plainD {
		t.Fatalf("origins differ: direct %s, other %s, ours %s; plain %s vs %s", direct, other, ours, plainD, plainB)
	}
	// and our tunnel still works through the same port
	tc, err := DialPathFront(context.Background(), DialPath{Addr: l.addr, SNI: "tun.ours.test", Key: l.key, Connect: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	echoOK(t, tc)
	tc.Close()
}

// Item 3: prober-visible time to the target's first byte on a "both" IP
// (lazy dial) vs a "reality" IP (dial at accept), with a 30 ms
// listener->target connect; the prober sends its hello at once or 100 ms
// after the connect. The pre-dial keeps the delayed case at parity.
func TestHybridCoverTimingParity(t *testing.T) {
	rt := newRawTarget(t, "TARGET-ANSWER", 0)
	slow := func(ctx context.Context, addr string) (net.Conn, error) {
		time.Sleep(30 * time.Millisecond)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	eager := newRealityLab(t, rt.ln.Addr().String(), nil)
	both, _ := hybridLab(t, rt.ln.Addr().String())
	for _, l := range []*realityLab{eager, both} {
		l.setup.Lock()
		l.srv.DialTarget = slow
		l.setup.Unlock()
	}
	hello := helloBytes(t, "other.example")
	probe := func(addr string, wait time.Duration) time.Duration {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		time.Sleep(wait)
		t0 := time.Now()
		c.Write(hello)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		return time.Since(t0)
	}
	med := func(addr string, wait time.Duration) time.Duration {
		var ds []time.Duration
		for i := 0; i < 7; i++ {
			ds = append(ds, probe(addr, wait))
		}
		for i := range ds {
			for j := i + 1; j < len(ds); j++ {
				if ds[j] < ds[i] {
					ds[i], ds[j] = ds[j], ds[i]
				}
			}
		}
		return ds[len(ds)/2]
	}
	res := map[string]time.Duration{}
	for _, w := range []time.Duration{0, 100 * time.Millisecond} {
		res[fmt.Sprint("eager/", w)] = med(eager.addr, w)
		res[fmt.Sprint("both/", w)] = med(both.addr, w)
		old := lazyPreDial
		both.setup.Lock()
		lazyPreDial = time.Hour // the phase-4 lazy dial without the pre-dial
		both.setup.Unlock()
		res[fmt.Sprint("lazy-only/", w)] = med(both.addr, w)
		both.setup.Lock()
		lazyPreDial = old
		both.setup.Unlock()
	}
	t.Logf("median time to first byte: %v", res)
	if d := res["both/100ms"] - res["eager/100ms"]; d > 10*time.Millisecond || d < -10*time.Millisecond {
		t.Fatalf("delayed hello: both %v vs eager %v", res["both/100ms"], res["eager/100ms"])
	}
	if d := res["both/0s"] - res["eager/0s"]; d > 10*time.Millisecond {
		t.Fatalf("immediate hello: both %v vs eager %v", res["both/0s"], res["eager/0s"])
	}
}

// Cost of the per-connection throw-away cert (the handshake's critical path).
func BenchmarkRealityMakeCert(b *testing.B) {
	t := &testing.T{}
	for _, c := range []struct {
		name string
		kind string
		bits int
		want int64
	}{{"ec-min-387", "ecdsa", 256, 387}, {"ec-2181", "ecdsa", 256, 2181}, {"rsa-2181", "rsa", 2048, 2181}} {
		b.Run(c.name, func(b *testing.B) {
			priv, _, _ := RealityKeygen()
			r, err := NewRealityServer(&FrontProfile{Kind: "reality", Target: "127.0.0.1:1", ServerNames: []string{"www.target.test"},
				PrivateKey: priv, ShortIDs: []string{"a1"}}, &Server{SharedKey: make([]byte, 32)})
			if err != nil {
				b.Fatal(err)
			}
			if err := r.useLeafKey(c.kind, c.bits); err != nil {
				b.Fatal(err)
			}
			r.CertPad.Store(c.want)
			ak := make([]byte, 32)
			b.ResetTimer()
			t0 := certTries.Load()
			for i := 0; i < b.N; i++ {
				if _, err := r.makeCert(ak, "www.target.test"); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(certTries.Load()-t0)/float64(b.N), "signs/op")
		})
	}
	_ = t
}
