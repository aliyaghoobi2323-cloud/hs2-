package main

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// cutRelay is a TCP relay in front of the server whose kill resets every
// connection it carries then (a link's death under them); later ones pass.
type cutRelay struct {
	ln    net.Listener
	to    string
	mu    sync.Mutex
	conns []net.Conn
}

func newCutRelay(t *testing.T, to string) *cutRelay {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &cutRelay{ln: ln, to: to}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			a, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", to)
			if err != nil {
				a.Close()
				continue
			}
			r.mu.Lock()
			r.conns = append(r.conns, a, b)
			r.mu.Unlock()
			go func() { io.Copy(b, a); b.(*net.TCPConn).CloseWrite() }()
			go func() { io.Copy(a, b); a.(*net.TCPConn).CloseWrite() }()
		}
	}()
	return r
}

// kill resets every connection the relay carries now.
func (r *cutRelay) kill() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
	}
	n := len(r.conns) / 2
	r.conns = nil
	return n
}

func loopServer(t *testing.T) (addr, dir string) {
	dir = t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go acceptLoop(ln, dir)
	return ln.Addr().String(), dir
}

// TestCutsLoopback: a kill cell on loopback (cuts.go): at KillS every
// connection is reset; each flow logs its cut, opens again and recovers,
// and the games and calls show the gap.
func TestCutsLoopback(t *testing.T) {
	srv, dir := loopServer(t)
	relay := newCutRelay(t, srv)
	addr := relay.ln.Addr().String()
	s := &Spec{TotalS: 5, Bulk: addr, Echo: addr, KillS: 2,
		Users: []User{
			{ID: "g", Flows: []Flow{{Kind: "game", N: 1}}},
			{ID: "v", Flows: []Flow{{Kind: "call", N: 1, Rate: 1.5}}},
			{ID: "s", Flows: []Flow{{Kind: "stream", N: 1, Rate: 2}}},
			{ID: "w", Flows: []Flow{{Kind: "web", N: 1, Every: 0.5}}},
			{ID: "p", Flows: []Flow{{Kind: "probe", N: 1}}},
			{ID: "d", Flows: []Flow{{Kind: "dn", N: 1}}},
		},
		Windows: []Window{{"main", 0.5, 4.5}, {"all", 0.5, 5}}}
	if err := s.check(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(2 * time.Second)
		n := relay.kill()
		t.Logf("killed %d connections", n)
	}()
	out := runClient(s, dir)
	if out.KillS != 2 || len(out.Conns) == 0 {
		t.Fatalf("no connection log: kill %v, %d records", out.KillS, len(out.Conns))
	}
	cut := map[string]int{}
	for _, c := range out.Conns {
		if c.X >= 0 {
			cut[c.K]++
			if c.X < 1.9 || c.X > 3 {
				t.Errorf("%s cut at %v, the kill at 2", c.K, c.X)
			}
		}
	}
	back := map[string]float64{}
	for _, r := range out.Recov {
		back[r.K] = r.Back
	}
	t.Logf("cut %v, back %v", cut, back)
	for _, k := range []string{"game", "call", "stream", "web", "probe", "dn"} {
		if cut[k] == 0 {
			// on loopback a viewer's buffer is full at once (its connection
			// idle at the kill: an idle connection's death is noticed at
			// its next use) and a page is done in milliseconds
			if k != "stream" && k != "web" {
				t.Errorf("%s: no cut connection logged", k)
			}
			continue
		}
		b, ok := back[k]
		if !ok || b < 0 || b > 2 {
			t.Errorf("%s: recovery %v (logged %v)", k, b, ok)
		}
	}
	gaps := map[string]float64{}
	for _, o := range out.Outage {
		if o.K == "game" || o.K == "call" || o.K == "probe" {
			gaps[o.K+"/"+o.Dir] = o.GapMs
		}
	}
	t.Logf("outage gaps ms %v", gaps)
	for _, k := range []string{"game/dn", "game/up", "call/dn", "call/up", "probe/rtt"} {
		if g := gaps[k]; g < 150 || g > 2000 {
			t.Errorf("%s gap %v ms, want the redial's (>= 150) and < 2 s", k, g)
		}
	}
	// the game's messages after the cut arrived: its later connection's
	// record was merged (main window: sent about 4 s x 64, lost only the
	// cut's own)
	rc := out.Classes["game"].(*RTClass)
	if o := rc.Up["main"]; o == nil || o.N < 150 || rc.Errs != 0 {
		t.Fatalf("game up after the cut: %+v, %d flows without a record", o, rc.Errs)
	}
}

// TestFrozenLoopback: frozen downloads (frozen.go) open in waves, read, stop
// reading and stay; the light user's probe is timed.
func TestFrozenLoopback(t *testing.T) {
	addr, dir := loopServer(t)
	s := &Spec{TotalS: 3, Bulk: addr, Echo: addr,
		Users: []User{
			{ID: "f", Flows: []Flow{{Kind: "frozen", N: 8, StartS: 0.5, Wave: 4, ReadS: 0.3, Rcvbuf: 16384}}},
			{ID: "p", Flows: []Flow{{Kind: "probe", N: 1}}},
		},
		Windows: []Window{{"main", 0.5, 2.5}, {"all", 0.5, 3}}}
	if err := s.check(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	out := runClient(s, dir)
	f := out.Frozen
	if f == nil {
		t.Fatal("no frozen output")
	}
	t.Logf("%+v in %v", *f, time.Since(t0).Round(time.Millisecond))
	if f.Conns != 8 || f.Frozen != 8 || f.Ended != 0 || f.MBytes <= 0 {
		t.Fatalf("frozen %+v", *f)
	}
	if g := f.ProbeGapMs["max"]; g <= 0 || g > 600 {
		t.Fatalf("probe gap %v ms on loopback", g)
	}
	if out.Conns != nil || out.Recov != nil || out.Outage != nil {
		t.Fatal("a connection log without a kill")
	}
}

// longestGap: the longest interval between times in a window, and a late
// first one
func TestLongestGap(t *testing.T) {
	ts := []float64{0.5, 1.0, 1.2, 2.9, 3.0, 9.0}
	if g, at := longestGap(ts, 1.0, 5.0); g < 1.69 || g > 1.71 || at != 1.2 {
		t.Fatalf("gap %v at %v, want 1.7 at 1.2", g, at)
	}
	if g, at := longestGap([]float64{3.5}, 1.0, 5.0); g != 2.5 || at != 1.0 {
		t.Fatalf("late first: gap %v at %v", g, at)
	}
}
