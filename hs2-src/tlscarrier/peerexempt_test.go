package tlscarrier

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// dialFrom: a TCP connection from a given loopback source (127.0.0.x share
// the /24 that preMaxNet counts).
func dialFrom(t *testing.T, src, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 2 * time.Second}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Skipf("dial from %s: %v (needs Linux loopback /8)", src, err)
	}
	return c
}

// gotFIN: the server closed the connection without a byte (capClose).
func gotFIN(c net.Conn, d time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	return err == io.EOF
}

func closeAll(cs []net.Conn) {
	for _, c := range cs {
		c.Close()
	}
}

func setPeerExempt(t *testing.T, on bool) {
	old := PeerExemptOn.Load()
	PeerExemptOn.Store(on)
	t.Cleanup(func() { PeerExemptOn.Store(old) })
}

// An exempt IP is not counted by the per-/24 cap: 64+20 idle pre-hello conns
// from it, and the next still reaches the target.
func TestPeerExemptNotCapped(t *testing.T) {
	setPeerExempt(t, true)
	rt := newRawTarget(t, "T", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	l.srv.Peers.SetStatic([]string{"127.0.0.1"})
	p0 := rejected("prehello_cap")
	var cs []net.Conn
	defer func() { closeAll(cs) }()
	for i := 0; i < preMaxNet+20; i++ {
		cs = append(cs, dialFrom(t, "127.0.0.1", l.addr))
	}
	time.Sleep(200 * time.Millisecond)
	c := dialFrom(t, "127.0.0.1", l.addr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, _ := io.ReadAll(c); string(b) != "T" {
		t.Fatalf("exempt conn over 64: got %q", b)
	}
	if d := rejected("prehello_cap"); d != p0 {
		t.Fatalf("prehello_cap +%d for an exempt IP", d-p0)
	}
	if s := l.srv.Peers.Snap(); s.Exempted < int64(preMaxNet+21) {
		t.Fatalf("snap %+v", s)
	}
}

// The exempt IP's own bound holds (a spoofed/stolen peer IP is not
// unlimited); over it: the same FIN as the /24 cap.
func TestPeerExemptBound(t *testing.T) {
	setPeerExempt(t, true)
	rt := newRawTarget(t, "T", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	l.srv.Peers.SetStatic([]string{"127.0.0.1"})
	l.srv.Peers.SetBound(1) // 2*1+8 = 10 -> floor 72
	if s := l.srv.Peers.Snap(); s.Bound != peerBoundFloor {
		t.Fatalf("bound %d, want floor %d", s.Bound, peerBoundFloor)
	}
	l.srv.Peers.SetBound(100)
	if s := l.srv.Peers.Snap(); s.Bound != 208 {
		t.Fatalf("bound %d, want 208", s.Bound)
	}
	l.srv.Peers.mu.Lock()
	l.srv.Peers.bound = 5
	l.srv.Peers.mu.Unlock()
	p0 := rejected("prehello_cap_peer")
	var cs []net.Conn
	defer func() { closeAll(cs) }()
	for i := 0; i < 5; i++ {
		cs = append(cs, dialFrom(t, "127.0.0.1", l.addr))
	}
	time.Sleep(200 * time.Millisecond)
	c := dialFrom(t, "127.0.0.1", l.addr)
	defer c.Close()
	if !gotFIN(c, 3*time.Second) {
		t.Fatal("6th exempt pre-hello conn with bound 5: not closed")
	}
	if rejected("prehello_cap_peer") != p0+1 {
		t.Fatalf("prehello_cap_peer %d -> %d", p0, rejected("prehello_cap_peer"))
	}
	// the bound frees as hellos arrive
	cs[0].Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	time.Sleep(300 * time.Millisecond)
	c2 := dialFrom(t, "127.0.0.1", l.addr)
	defer c2.Close()
	c2.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, _ := io.ReadAll(c2); string(b) != "T" {
		t.Fatalf("after a slot freed: %q", b)
	}
}

// rq387's case: a same-/24 neighbour (127.0.0.2) holds 64 idle conns; it is
// still capped at 64, while our peer (127.0.0.1) authenticates links.
func TestPeerExemptNeighbourFloodStillCapped(t *testing.T) {
	setPeerExempt(t, true)
	rt := newRawTarget(t, "T", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	l.srv.Peers.SetStatic([]string{"127.0.0.1"})
	var cs []net.Conn
	defer func() { closeAll(cs) }()
	for i := 0; i < preMaxNet; i++ {
		cs = append(cs, dialFrom(t, "127.0.0.2", l.addr))
	}
	time.Sleep(200 * time.Millisecond)
	p0 := rejected("prehello_cap")
	n := dialFrom(t, "127.0.0.2", l.addr)
	defer n.Close()
	if !gotFIN(n, 3*time.Second) || rejected("prehello_cap") != p0+1 {
		t.Fatal("neighbour over 64: not capped")
	}
	// key off: today's behaviour, the peer is capped like the neighbour
	PeerExemptOn.Store(false)
	c := dialFrom(t, "127.0.0.1", l.addr)
	if !gotFIN(c, 3*time.Second) {
		t.Fatal("key off: peer not capped")
	}
	c.Close()
	PeerExemptOn.Store(true)
	p0 = rejected("prehello_cap")
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		car, err := l.dial(l.dprof)
		if err != nil {
			t.Fatalf("peer link %d with the neighbour at the cap: %v", i, err)
		}
		car.Close()
	}
	if rejected("prehello_cap") != p0 {
		t.Fatal("peer links were capped")
	}
	t.Logf("5 peer links in %v with the neighbour at the cap", time.Since(t0))
}

// A non-exempt flood is capped exactly as before (the exempt list holds
// another IP).
func TestPeerExemptOthersCapped(t *testing.T) {
	setPeerExempt(t, true)
	rt := newRawTarget(t, "T", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	l.srv.Peers.SetStatic([]string{"10.9.9.9"})
	var cs []net.Conn
	defer func() { closeAll(cs) }()
	for i := 0; i < preMaxNet; i++ {
		c, err := net.Dial("tcp", l.addr)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	time.Sleep(200 * time.Millisecond)
	c, _ := net.Dial("tcp", l.addr)
	defer c.Close()
	if !gotFIN(c, 3*time.Second) {
		t.Fatal("non-exempt conn over the cap not closed")
	}
}

// An authenticated link teaches its IP (24 h, LRU); a failed auth does not.
func TestPeerExemptLearnAndExpiry(t *testing.T) {
	setPeerExempt(t, true)
	rt := newRawTarget(t, "T", 0)
	l := newRealityLab(t, rt.ln.Addr().String(), nil)
	lo := &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}
	c, _ := net.Dial("tcp", l.addr) // a probe: plain hello, no auth
	c.Write(plainHello(t, "www.target.test"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.ReadAll(c)
	c.Close()
	if l.srv.Peers.Exempt(lo) {
		t.Fatal("an unauthenticated hello was learned")
	}
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatal(err)
	}
	car.Close()
	if !l.srv.Peers.Exempt(lo) {
		t.Fatal("authenticated link IP not learned")
	}

	p := NewPeerAllow()
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }
	a := &net.TCPAddr{IP: net.ParseIP("192.0.2.7")}
	p.Learn(a)
	now = now.Add(peerLearnTTL - time.Minute)
	if !p.Exempt(a) {
		t.Fatal("expired too early")
	}
	p.Learn(a) // refreshed by a new auth
	now = now.Add(peerLearnTTL - time.Minute)
	if !p.Exempt(a) {
		t.Fatal("refresh lost")
	}
	now = now.Add(2 * time.Minute)
	if p.Exempt(a) || p.Snap().Learned != 0 {
		t.Fatalf("not expired after 24 h: %+v", p.Snap())
	}
	// LRU bound: the oldest is evicted
	for i := 0; i <= peerLearnMax; i++ {
		p.Learn(&net.TCPAddr{IP: net.IPv4(10, 1, byte(i>>8), byte(i))})
	}
	if s := p.Snap(); s.Learned != peerLearnMax {
		t.Fatalf("learned %d, want %d", s.Learned, peerLearnMax)
	}
	if p.Exempt(&net.TCPAddr{IP: net.IPv4(10, 1, 0, 0)}) || !p.Exempt(&net.TCPAddr{IP: net.IPv4(10, 1, 4, 0)}) {
		t.Fatal("LRU eviction order")
	}
	// static: replaced as a whole (reload), v4-mapped v6 normalised
	p.SetStatic([]string{"198.51.100.1", "bad", "2001:db8::1"})
	if !p.Exempt(&net.TCPAddr{IP: net.ParseIP("::ffff:198.51.100.1")}) || !p.Exempt(&net.TCPAddr{IP: net.ParseIP("2001:db8::1")}) {
		t.Fatal("static")
	}
	p.SetStatic([]string{"198.51.100.2"})
	if p.Exempt(&net.TCPAddr{IP: net.ParseIP("198.51.100.1")}) || !p.Exempt(&net.TCPAddr{IP: net.ParseIP("198.51.100.2")}) {
		t.Fatal("static not replaced")
	}
	var nilP *PeerAllow
	nilP.Learn(a)
	if nilP.Exempt(a) {
		t.Fatal("nil allowlist")
	}
}

// Probe responses are byte-identical with the key on (prober's IP exempt,
// the worst case: a stolen peer IP) and off.
func TestPeerExemptProbeParity(t *testing.T) {
	run := func(on bool) []string {
		setPeerExempt(t, on)
		rt := newRawTarget(t, "HTTP/1.1 200 OK\r\nServer: nginx\r\n\r\nhello", 0)
		l := newRealityLab(t, rt.ln.Addr().String(), nil)
		if on {
			l.srv.Peers.SetStatic([]string{"127.0.0.1"})
		}
		var out []string
		for _, probe := range [][]byte{
			[]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
			plainHello(t, "www.target.test"),
			plainHello(t, "random.example"),
			bytes.Repeat([]byte{0x5a}, 1500),
			nil, // silent
		} {
			c, err := net.Dial("tcp", l.addr)
			if err != nil {
				t.Fatal(err)
			}
			if probe != nil {
				c.Write(probe)
			}
			c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
			b, err := io.ReadAll(c)
			kind := "eof"
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				kind = "open"
			} else if err != nil {
				kind = "err"
			}
			c.Close()
			out = append(out, fmt.Sprintf("%s %q", kind, b))
		}
		return out
	}
	off := run(false)
	on := run(true)
	for i := range off {
		if off[i] != on[i] {
			t.Errorf("probe %d differs: off %s / on %s", i, off[i], on[i])
		}
	}
}
