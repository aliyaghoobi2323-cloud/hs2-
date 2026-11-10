package tlscarrier

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// hybridLab: a "both" listener (reality + our tls name "tun.ours.test").
func hybridLab(t *testing.T, target string) (*realityLab, string) {
	l := newRealityLab(t, target, func(p *FrontProfile) { p.Kind = FrontBoth })
	be, stop := backend(t)
	t.Cleanup(stop)
	l.srv.TLS = &Server{SharedKey: l.key, Cert: testCert(t, "tun.ours.test"), BackendAddr: be}
	l.srv.TLSName = func(_ net.Addr, sni string) bool { return sni == "tun.ours.test" }
	return l, be
}

func echoOK(t *testing.T, car *Carrier) {
	t.Helper()
	msg := []byte("ping-through")
	car.RawConn().Write(msg)
	got := make([]byte, len(msg))
	car.SetReadDeadline(time.Now().Add(3 * time.Second))
	n := 0
	for n < len(got) {
		k, err := car.RawConn().Read(got[n:])
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
		n += k
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo %q", got)
	}
}

// One port, three classes: a valid reality signal -> reality; our tls name ->
// tls; anything else (a browser/prober hello for any other name, or for the
// reality name without the signal) -> cover = the target, byte for byte.
func TestHybridClassify(t *testing.T) {
	rt := newRawTarget(t, "TARGET-ANSWER", 0)
	l, _ := hybridLab(t, rt.ln.Addr().String())

	dp := *l.dprof
	dp.Kind = FrontBoth
	car, err := l.dial(&dp)
	if err != nil {
		t.Fatalf("reality via both: %v", err)
	}
	echoOK(t, car)
	car.Close()
	if s := l.srv.Counters.Snap(); s.Reality != 1 || s.TLS != 0 {
		t.Fatalf("after reality: %+v", s)
	}

	tc, err := DialPathFront(context.Background(), DialPath{Addr: l.addr, SNI: "tun.ours.test", Key: l.key, Connect: 2 * time.Second})
	if err != nil {
		t.Fatalf("tls via both: %v", err)
	}
	echoOK(t, tc)
	tc.Close()
	if s := l.srv.Counters.Snap(); s.TLS != 1 {
		t.Fatalf("after tls: %+v", s)
	}

	for i, sni := range []string{"other.example", "www.target.test"} {
		hello := helloBytes(t, sni)
		c, err := net.Dial("tcp", l.addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write(hello)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 64)
		n, _ := c.Read(b)
		c.Close()
		if string(b[:n]) != "TARGET-ANSWER" {
			t.Fatalf("cover %s: got %q", sni, b[:n])
		}
		if s := l.srv.Counters.Snap(); s.Cover != int64(i+1) {
			t.Fatalf("cover count %+v", s)
		}
	}
	// the target got the probers' hellos exactly
	time.Sleep(100 * time.Millisecond)
	found := 0
	for _, g := range rt.all() {
		if h, err := parseRealityHello(g); err == nil && h.complete && (h.sni == "other.example") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("target saw %d other.example hellos", found)
	}
	// a tls client of a DIFFERENT tunnel name is not ours: cover
	if _, err := DialPathFront(context.Background(), DialPath{Addr: l.addr, SNI: "nope.test", Key: l.key, Connect: 2 * time.Second}); err == nil {
		t.Fatal("tls with a foreign sni must not reach the tunnel")
	}
}

// Target unreachable: the listener refuses reality (target_down) and a
// "both" dialer falls back to tls on the spot; after N failures in a row
// the path cools on tls; reality returns after the cool-down.
func TestBothFallbackTargetDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	l, _ := hybridLab(t, dead)
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	pol := &BothPolicy{N: 3, Cool: 10 * time.Minute, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	dp := *l.dprof
	dp.Kind = FrontBoth
	p := DialPath{Addr: l.addr, SNI: "tun.ours.test", Key: l.key, Connect: 2 * time.Second, Profile: &dp}
	td0 := rejected("target_down")
	for i := 0; i < 5; i++ {
		c, err := BothFront{Policy: pol}.DialFront(context.Background(), p)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		echoOK(t, c)
		c.Close()
	}
	st := pol.Stats()
	if len(st) != 1 || st[0].RealityFail != 3 || st[0].TLSOK != 5 || st[0].Mode != FrontTLS || st[0].Fallbacks != 1 {
		t.Fatalf("stats %+v", st)
	}
	if rejected("target_down") != td0+3 {
		t.Fatalf("target_down %d", rejected("target_down")-td0)
	}
	if s := l.srv.Counters.Snap(); s.Reality != 0 || s.TLS != 5 {
		t.Fatalf("listener %+v", s)
	}
	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	if pol.UseTLS(BothPathKey(p)) {
		t.Fatal("cool-down did not expire")
	}
}

type fakeDialer struct {
	err   error
	calls int
}

func (f *fakeDialer) DialFront(context.Context, DialPath) (*Carrier, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &Carrier{}, nil
}

// The policy state machine with fake fronts.
func TestBothPolicyStateMachine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pol := &BothPolicy{N: 2, Cool: time.Minute, Now: func() time.Time { return now }}
	r, s := &fakeDialer{err: errors.New("handshake")}, &fakeDialer{}
	b := BothFront{Policy: pol, reality: r, tls: s}
	p := DialPath{Addr: "10.0.0.1:443", BindIP: "10.0.0.2", Profile: &FrontProfile{Kind: "both", PublicKey: "x", ServerNames: []string{"a"}, ShortIDs: []string{"a1"}}}
	b.DialFront(context.Background(), p) // reality fail 1 -> tls
	b.DialFront(context.Background(), p) // reality fail 2 -> cool-down -> tls
	b.DialFront(context.Background(), p) // tls only
	if r.calls != 2 || s.calls != 3 {
		t.Fatalf("reality %d tls %d", r.calls, s.calls)
	}
	now = now.Add(2 * time.Minute)
	r.err = nil
	b.DialFront(context.Background(), p) // reality again, ok
	if r.calls != 3 || s.calls != 3 {
		t.Fatalf("after cool: reality %d tls %d", r.calls, s.calls)
	}
	st := pol.Stats()[0]
	if st.Mode != FrontReality || st.ConsecFails != 0 || st.RealityOK != 1 || st.Fallbacks != 1 {
		t.Fatalf("%+v", st)
	}
	// a connect failure is the path's, not the front's: no tls retry, no count
	r.err = &ConnectError{errors.New("refused")}
	if _, err := b.DialFront(context.Background(), p); err == nil {
		t.Fatal("connect error must surface")
	}
	if s.calls != 3 || pol.Stats()[0].ConsecFails != 0 || pol.Stats()[0].ConnectFail != 1 {
		t.Fatalf("connect error handling: tls %d %+v", s.calls, pol.Stats()[0])
	}
	// both fronts fail -> the dial fails (the LB cuts the path as usual)
	r.err, s.err = errors.New("hs"), errors.New("hs")
	if _, err := b.DialFront(context.Background(), p); err == nil {
		t.Fatal("both failing must fail")
	}
}

// "both" profiles validate like reality and get the BothFront dialer.
func TestBothProfile(t *testing.T) {
	_, pub, _ := RealityKeygen()
	p := &FrontProfile{Kind: "both", ServerNames: []string{"a.test"}, ShortIDs: []string{"ab"}, PublicKey: pub}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := DialerFor(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(BothFront); !ok {
		t.Fatalf("%T", d)
	}
	p.ShortIDs = nil
	if p.Validate() == nil {
		t.Fatal("both without short_ids accepted")
	}
}
