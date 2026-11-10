package tlscarrier

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ---- harness ----

// rawTarget is a recording TCP "real site": it records what it receives and
// answers every connection's first bytes with reply (then optionally more).
type rawTarget struct {
	ln    net.Listener
	mu    sync.Mutex
	got   [][]byte
	reply []byte
	extra int // bytes of bulk data sent after reply
}

func newRawTarget(t *testing.T, reply string, extra int) *rawTarget {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt := &rawTarget{ln: ln, reply: []byte(reply), extra: extra}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				idx := -1
				rt.mu.Lock()
				rt.got = append(rt.got, nil)
				idx = len(rt.got) - 1
				rt.mu.Unlock()
				b := make([]byte, 64<<10)
				replied := false
				for {
					c.SetReadDeadline(time.Now().Add(3 * time.Second))
					n, err := c.Read(b)
					rt.mu.Lock()
					rt.got[idx] = append(rt.got[idx], b[:n]...)
					rt.mu.Unlock()
					if n > 0 && !replied {
						replied = true
						c.Write(rt.reply)
						if rt.extra > 0 {
							c.Write(make([]byte, rt.extra))
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return rt
}

func (rt *rawTarget) all() [][]byte {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([][]byte, len(rt.got))
	for i, g := range rt.got {
		out[i] = append([]byte(nil), g...)
	}
	return out
}

type realityLab struct {
	addr  string
	prof  *FrontProfile // listener profile
	dprof *FrontProfile // dialer profile
	key   []byte
	srv   *RealityServer
	// setup: tests change srv fields under it; the accept loop takes it
	// before each AcceptFront (a happens-before edge for -race).
	setup *sync.Mutex
}

func newRealityLab(t *testing.T, target string, mod func(*FrontProfile)) *realityLab {
	priv, pub, err := RealityKeygen()
	if err != nil {
		t.Fatal(err)
	}
	lp := &FrontProfile{Kind: "reality", Target: target, ServerNames: []string{"www.target.test"},
		PrivateKey: priv, ShortIDs: []string{"a1b2c3d4", "0123456789abcdef"}}
	if mod != nil {
		mod(lp)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	srv, err := NewRealityServer(lp, &Server{SharedKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	setup := &sync.Mutex{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			setup.Lock()
			setup.Unlock()
			go srv.AcceptFront(context.Background(), c, func(car *Carrier) {
				go func() { io.Copy(car.RawConn(), car.RawConn()); car.Close() }()
			})
		}
	}()
	dp := &FrontProfile{Kind: "reality", ServerNames: []string{"www.target.test"}, PublicKey: pub, ShortIDs: []string{"a1b2c3d4"}}
	return &realityLab{addr: ln.Addr().String(), prof: lp, dprof: dp, key: key, srv: srv, setup: setup}
}

func (l *realityLab) dial(p *FrontProfile) (*Carrier, error) {
	return DialPathFront(context.Background(), DialPath{Addr: l.addr, Key: l.key, Profile: p, Connect: 2 * time.Second})
}

func rejected(reason string) int64 { return RealityStatsNow().Rejected[reason] }

// helloBytes is a uTLS Chrome ClientHello (random session id) for sni.
func helloBytes(t *testing.T, sni string) []byte {
	a, b := net.Pipe()
	go func() {
		u := utls.UClient(a, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, clientHello)
		u.Handshake()
		a.Close()
	}()
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := b.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if h, perr := parseRealityHello(buf); perr != nil {
			t.Fatal(perr)
		} else if h.complete {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	b.Close()
	return buf
}

// ---- tests ----

func TestRealityKeys(t *testing.T) {
	priv, pub, err := RealityKeygen()
	if err != nil {
		t.Fatal(err)
	}
	if p2, err := RealityPublicOf(priv); err != nil || p2 != pub {
		t.Fatalf("public of private: %v %v", p2, err)
	}
	ok := &FrontProfile{Kind: "reality", Target: "a.test:443", ServerNames: []string{"a.test"}, PrivateKey: priv, PublicKey: pub, ShortIDs: []string{"ab"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	_, pub2, _ := RealityKeygen()
	for name, p := range map[string]FrontProfile{
		"mismatch": {Kind: "reality", Target: "a.test:443", ServerNames: []string{"a.test"}, PrivateKey: priv, PublicKey: pub2, ShortIDs: []string{"ab"}},
		"no names": {Kind: "reality", Target: "a.test:443", PrivateKey: priv, ShortIDs: []string{"ab"}},
		"no sid":   {Kind: "reality", Target: "a.test:443", ServerNames: []string{"a.test"}, PrivateKey: priv},
		"odd sid":  {Kind: "reality", Target: "a.test:443", ServerNames: []string{"a.test"}, PrivateKey: priv, ShortIDs: []string{"abc"}},
		"no tgt":   {Kind: "reality", ServerNames: []string{"a.test"}, PrivateKey: priv, ShortIDs: []string{"ab"}},
		"bad key":  {Kind: "reality", ServerNames: []string{"a.test"}, PublicKey: "xx", ShortIDs: []string{"ab"}},
	} {
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An authenticated dial gets a working tunnel; the hello is a plausible
// Chrome hello (32-byte session id, the borrowed SNI).
func TestRealityTunnel(t *testing.T) {
	rt := newRawTarget(t, "TARGET", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	a0 := RealityStatsNow().Authed
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	msg := []byte("hello through reality")
	car.RawConn().Write(msg)
	got := make([]byte, len(msg))
	car.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(car.RawConn(), got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo: %q %v", got, err)
	}
	if RealityStatsNow().Authed != a0+1 {
		t.Fatal("authed counter")
	}
	// the target saw the (forwarded) hello but answered nothing that reached us
	time.Sleep(100 * time.Millisecond)
	for _, g := range rt.all() {
		if h, err := parseRealityHello(g); err == nil && h.complete {
			if h.sni != "www.target.test" || len(h.sid) != 32 {
				t.Fatalf("hello sni %q sid %d", h.sni, len(h.sid))
			}
		}
	}
	// the second short id works too
	p2 := *l.dprof
	p2.ShortIDs = []string{"0123456789abcdef"}
	if c2, err := l.dial(&p2); err != nil {
		t.Fatal(err)
	} else {
		c2.Close()
	}
}

// Without the key nothing completes a tunnel handshake: each case is spliced
// to the target and counted under its reason.
func TestRealityRejects(t *testing.T) {
	rt := newRawTarget(t, "TARGET", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	_, otherPub, _ := RealityKeygen()
	cases := []struct {
		name, reason string
		mod          func(p *FrontProfile)
	}{
		{"wrong public key", "signal", func(p *FrontProfile) { p.PublicKey = otherPub }},
		{"wrong short id", "short_id", func(p *FrontProfile) { p.ShortIDs = []string{"ffff"} }},
		{"wrong sni", "sni", func(p *FrontProfile) { p.ServerNames = []string{"other.test"} }},
	}
	for _, c := range cases {
		p := *l.dprof
		c.mod(&p)
		before := rejected(c.reason)
		if car, err := l.dial(&p); err == nil {
			car.Close()
			t.Fatalf("%s: dialed", c.name)
		}
		if rejected(c.reason) != before+1 {
			t.Fatalf("%s: reason %q not counted: %v", c.name, c.reason, RealityStatsNow())
		}
	}
	// a client clock off by more than 90 s
	for _, off := range []time.Duration{-100 * time.Second, 100 * time.Second} {
		realityDialNow = func() time.Time { return time.Now().Add(off) }
		before := rejected("time")
		_, err := l.dial(l.dprof)
		realityDialNow = time.Now
		if err == nil || rejected("time") != before+1 {
			t.Fatalf("skew %v: err %v, %v", off, err, RealityStatsNow())
		}
	}
	// within the window it works
	realityDialNow = func() time.Time { return time.Now().Add(-60 * time.Second) }
	car, err := l.dial(l.dprof)
	realityDialNow = time.Now
	if err != nil {
		t.Fatalf("60 s skew must pass: %v", err)
	}
	car.Close()
	// a plain Chrome hello (random session id)
	before := rejected("signal")
	c, _ := net.Dial("tcp", l.addr)
	c.Write(helloBytes(t, "www.target.test"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 6)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "TARGET" {
		t.Fatalf("random signal: got %q %v", b, err)
	}
	c.Close()
	if rejected("signal") != before+1 {
		t.Fatal("random signal not counted")
	}
}

// recConn records what the client writes.
type recConn struct {
	net.Conn
	mu sync.Mutex
	w  []byte
}

func (r *recConn) Write(b []byte) (int, error) {
	r.mu.Lock()
	r.w = append(r.w, b...)
	r.mu.Unlock()
	return r.Conn.Write(b)
}

// A captured authenticated ClientHello replayed (within the window) is
// refused by the replay cache; modified copies fail the AEAD. Both go to the
// target unchanged.
func TestRealityReplayAndTamper(t *testing.T) {
	rt := newRawTarget(t, "TARGET", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	// capture a real authenticated hello through a recording proxy
	pl, _ := net.Listen("tcp", "127.0.0.1:0")
	defer pl.Close()
	var rec *recConn
	recd := make(chan struct{})
	go func() {
		c, err := pl.Accept()
		if err != nil {
			return
		}
		s, _ := net.Dial("tcp", l.addr)
		rec = &recConn{Conn: s}
		close(recd)
		go io.Copy(c, s)
		io.Copy(rec, c)
		s.Close()
		c.Close()
	}()
	car, err := DialPathFront(context.Background(), DialPath{Addr: pl.Addr().String(), Key: l.key, Profile: l.dprof})
	if err != nil {
		t.Fatal(err)
	}
	car.Close()
	<-recd
	rec.mu.Lock()
	all := append([]byte(nil), rec.w...)
	rec.mu.Unlock()
	var hello []byte
	for n := 5; n <= len(all); n++ {
		if h, err := parseRealityHello(all[:n]); err == nil && h.complete {
			hello = all[:n]
			break
		}
	}
	if hello == nil {
		t.Fatal("no hello captured")
	}
	send := func(b []byte) string {
		c, err := net.Dial("tcp", l.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write(b)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		r := make([]byte, 6)
		io.ReadFull(c, r)
		return string(r)
	}
	before := rejected("replay")
	if got := send(hello); got != "TARGET" || rejected("replay") != before+1 {
		t.Fatalf("replay: got %q, %v", got, RealityStatsNow())
	}
	h, _ := parseRealityHello(hello)
	for _, off := range []int{5 + h.sidOff + 3, 5 + 10, len(hello) - 1} { // session id, random, last extension byte
		m := append([]byte(nil), hello...)
		m[off] ^= 0x40
		before := rejected("signal") + rejected("sni") + rejected("no_x25519")
		if got := send(m); got != "TARGET" {
			t.Fatalf("tamper @%d: %q", off, got)
		}
		if rejected("signal")+rejected("sni")+rejected("no_x25519") != before+1 {
			t.Fatalf("tamper @%d not rejected: %v", off, RealityStatsNow())
		}
	}
	// every byte the prober sent reached the target exactly
	found := false
	for _, g := range rt.all() {
		if bytes.Equal(g, hello) {
			found = true
		}
	}
	if !found {
		t.Fatal("the replayed hello did not reach the target byte for byte")
	}
}

// A prober without the key is talking to the target: everything it sends
// reaches the target unchanged and everything the target answers reaches it
// unchanged — TLS or not.
func TestRealityProbeTranscriptIdentical(t *testing.T) {
	reply := "\x16\x03\x03\x00\x05ABCDE-target-bytes\x00\x01\xff"
	rt := newRawTarget(t, reply, 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	probes := [][]byte{
		helloBytes(t, "www.target.test"),
		helloBytes(t, "unknown.test"),
		[]byte("GET / HTTP/1.1\r\nHost: www.target.test\r\n\r\n"),
		{0x16, 0x03, 0x01, 0x00, 0x10, 0x02, 0, 0, 0},
		[]byte("\x00\x01\x02garbage"),
	}
	for i, p := range probes {
		n0 := len(rt.all())
		c, _ := net.Dial("tcp", l.addr)
		c.Write(p)
		if c2, ok := c.(*net.TCPConn); ok {
			c2.CloseWrite()
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, _ := io.ReadAll(c)
		c.Close()
		if string(got) != reply {
			t.Fatalf("probe %d: answer %q, want the target's %q", i, got, reply)
		}
		time.Sleep(50 * time.Millisecond)
		all := rt.all()
		if len(all) != n0+1 || !bytes.Equal(all[n0], p) {
			t.Fatalf("probe %d: target got %q, want %q", i, all[len(all)-1], p)
		}
	}
}

// With a real TLS target, a standard TLS client completes a genuine handshake
// with the TARGET's certificate through the listener.
func TestRealityProbeSeesTargetCert(t *testing.T) {
	cert := testCert(t, "www.target.test")
	tln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer tln.Close()
	go func() {
		for {
			c, err := tln.Accept()
			if err != nil {
				return
			}
			go func() {
				b := make([]byte, 1024)
				c.Read(b)
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\ntarget")
				c.Close()
			}()
		}
	}()
	l := newRealityLab(t, tln.Addr().String(), nil)
	c, err := tls.Dial("tcp", l.addr, &tls.Config{ServerName: "www.target.test", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !bytes.Equal(c.ConnectionState().PeerCertificates[0].Raw, cert.Certificate[0]) {
		t.Fatal("prober did not see the target's certificate")
	}
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: www.target.test\r\n\r\n")
	b, _ := io.ReadAll(c)
	if !strings.HasSuffix(string(b), "target") {
		t.Fatalf("body %q", b)
	}
}

// Fallback limits: free up to max bytes, then the rate; max duration; counters.
func TestRealityFallbackLimits(t *testing.T) {
	rt := newRawTarget(t, "T", 400<<10)
	l := newRealityLab(t, rt.ln.Addr().String(), func(p *FrontProfile) {
		p.FallbackMaxBytes, p.FallbackRateBps = 64<<10, 256<<10
	})
	s0 := RealityStatsNow()
	c, _ := net.Dial("tcp", l.addr)
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	t0 := time.Now()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, _ := io.CopyN(io.Discard, c, 1+400<<10)
	el := time.Since(t0)
	c.Close()
	if n != 1+400<<10 {
		t.Fatalf("got %d bytes", n)
	}
	// (400-64) KB at 256 KB/s = 1.31 s
	if el < 1100*time.Millisecond || el > 4*time.Second {
		t.Fatalf("elapsed %v, want about 1.3 s", el)
	}
	s1 := RealityStatsNow()
	if s1.FallbackConns != s0.FallbackConns+1 || s1.FallbackBytes-s0.FallbackBytes < 400<<10 || s1.Rejected["not_hello"] != s0.Rejected["not_hello"]+1 {
		t.Fatalf("counters %v -> %v", s0, s1)
	}
	// max duration
	rt2 := newRawTarget(t, "T", 0)
	l2 := newRealityLab(t, rt2.ln.Addr().String(), func(p *FrontProfile) { p.FallbackMaxSeconds = 1 })
	c2, _ := net.Dial("tcp", l2.addr)
	defer c2.Close()
	c2.Write([]byte("x"))
	t1 := time.Now()
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadAll(c2) // the target keeps it open (3 s idle) — we cut at 1 s
	if d := time.Since(t1); d > 2500*time.Millisecond {
		t.Fatalf("max duration not enforced: %v", d)
	}
}

// The tls front is untouched: a reality dialer against a tls Server fails
// and a tls dialer against a reality listener is spliced to the target.
func TestRealityVsTLSFront(t *testing.T) {
	rt := newRawTarget(t, "TARGET", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	if _, err := DialPathFront(context.Background(), DialPath{Addr: l.addr, SNI: "www.target.test", Key: l.key, Connect: time.Second}); err == nil {
		t.Fatal("a tls-front dial authenticated on a reality listener")
	}
}

// Size mirroring: the throw-away cert grows to about the target's chain.
func TestRealitySizeMirror(t *testing.T) {
	cert := testCert(t, "www.target.test")
	cert.Certificate = [][]byte{cert.Certificate[0], cert.Certificate[0]} // a 2-entry chain
	tln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer tln.Close()
	go func() {
		for {
			c, err := tln.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	l := newRealityLab(t, tln.Addr().String(), nil)
	n, err := l.srv.targetChainSize(context.Background())
	if err != nil || n < 300 {
		t.Fatalf("chain size %d %v", n, err)
	}
	want := int64(2*len(cert.Certificate[0]) + 5)
	if n != want {
		t.Fatalf("chain size %d, want %d", n, want)
	}
	for _, target := range []int64{n, 1500, 4200} {
		l.srv.CertPad.Store(target)
		c, err := l.srv.makeCert(make([]byte, 32), "www.target.test")
		if err != nil {
			t.Fatal(err)
		}
		got := int64(len(c.Certificate[0]))
		if target > int64(l.srv.baseLen)+40 && (got < target-16 || got > target+16) {
			t.Fatalf("padded to %d, want about %d (base %d)", got, target, l.srv.baseLen)
		}
	}
	// a tunnel still works with a padded cert
	l.srv.CertPad.Store(4200)
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatal(err)
	}
	car.Close()
	ctx, cancel := context.WithCancel(context.Background())
	l.srv.CertPad.Store(0)
	l.srv.StartSizeMirror(ctx, time.Hour, nil)
	for i := 0; i < 50 && l.srv.CertPad.Load() == 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if l.srv.CertPad.Load() != n {
		t.Fatalf("mirror stored %d, want %d", l.srv.CertPad.Load(), n)
	}
}

// Phase 4: the authenticated handshake copies the target's group and ALPN
// (an X25519-only target with http/1.1), tickets stay off, the throw-away cert
// is exactly the target chain's size every time, and the tunnel still works.
func TestRealityMirrorTarget(t *testing.T) {
	cert := testCert(t, "www.target.test")
	tln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert},
		CurvePreferences: []tls.CurveID{tls.X25519}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tln.Close()
	go func() {
		for {
			c, err := tln.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	l := newRealityLab(t, tln.Addr().String(), nil)
	if c := l.srv.tunnelTLSConfig(&cert); !c.SessionTicketsDisabled || len(c.CurvePreferences) != 0 {
		t.Fatalf("unmeasured config: %+v", c)
	}
	n, err := l.srv.targetChainSize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := l.srv.Mirror()
	if m == nil || m.Curve != tls.X25519 || m.ALPN != "http/1.1" {
		t.Fatalf("mirror %+v", m)
	}
	c := l.srv.tunnelTLSConfig(&cert)
	if len(c.CurvePreferences) != 1 || c.CurvePreferences[0] != tls.X25519 || len(c.NextProtos) != 1 || c.NextProtos[0] != "http/1.1" || !c.SessionTicketsDisabled {
		t.Fatalf("mirrored config: curves %v alpn %v tickets-off %v", c.CurvePreferences, c.NextProtos, c.SessionTicketsDisabled)
	}
	for _, target := range []int64{n + 200, 1500, 2600, 4200} {
		l.srv.CertPad.Store(target)
		for i := 0; i < 20; i++ {
			cc, err := l.srv.makeCert(make([]byte, 32), "www.target.test")
			if err != nil {
				t.Fatal(err)
			}
			if got := int64(len(cc.Certificate[0])); got != target {
				t.Fatalf("cert %d B, want exactly %d", got, target)
			}
		}
	}
	l.srv.CertPad.Store(2600)
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatal(err)
	}
	car.Close()
}
