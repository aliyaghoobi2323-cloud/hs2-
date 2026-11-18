package engine

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func bindErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Source: &net.TCPAddr{IP: net.ParseIP("2001:db8:a::1")},
		Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRNOTAVAIL}}
}

func TestLocalAddrErr(t *testing.T) {
	if !localAddrErr(bindErr()) || !localAddrErr(fmt.Errorf("x: %w", bindErr())) {
		t.Fatal("bind EADDRNOTAVAIL not recognised")
	}
	if !localAddrErr(fmt.Errorf("dial tcp [2a01::1]:0->[2a01::2]:443: bind: cannot assign requested address")) {
		t.Fatal("text form not recognised")
	}
	if localAddrErr(nil) || localAddrErr(fmt.Errorf("connection refused")) {
		t.Fatal("false positive")
	}
}

// A path whose source address is not on the host is cooled at its first
// failure (even inside the start grace, even with nothing held), with a
// doubling cooldown, and logged at most once a minute; the dial loop stays
// on the other paths instead of hammering it.
func TestLBLocalAddrCooled(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	paths := []LBPath{
		{Local: "10.0.0.1", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 50, Primary: true},
		{Local: "2001:db8:a::1", Remote: "2001:db8::1", Addr: "[2001:db8::1]:443", Weight: 50},
	}
	lb, _ := NewLB(paths, logf)
	clock := time.Unix(1e9, 0)
	lb.now = func() time.Time { return clock }
	v6dials := 0
	dial := func(p LBPath) (any, error) {
		if !p.Primary {
			v6dials++
			return nil, bindErr()
		}
		return new(int), nil
	}
	// 2 minutes of slots redialing every 2 s (what the exit pool does)
	for s := 0; s < 60; s++ {
		clock = clock.Add(2 * time.Second)
		lb.dialVia(dial)
	}
	// cooldowns 15+30+60 s, then a probe: at most 4 dials in 120 s
	if v6dials > 4 || v6dials < 2 {
		t.Fatalf("%d dials on the dead-source path in 2 min (want 2..4)", v6dials)
	}
	st := lb.Stats()
	if st[1].State != "cooling" && st[1].State != "probing" {
		t.Fatalf("path state %q", st[1].State)
	}
	if st[0].Links < 50 {
		t.Fatalf("primary got only %d links", st[0].Links)
	}
	mu.Lock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "is not an address of this server") {
			n++
		}
	}
	mu.Unlock()
	if n < 1 || n > 3 {
		t.Fatalf("%d lines in 2 min (want 1..3, at most one a minute): %v", n, lines)
	}
}

// The primary is never cooled by a source-address failure (as before).
func TestLBLocalAddrPrimaryNotCooled(t *testing.T) {
	lb, _ := NewLB([]LBPath{{Local: "10.0.0.1", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 100, Primary: true}}, nil)
	lb.dialVia(func(LBPath) (any, error) { return nil, bindErr() })
	if st := lb.Stats(); st[0].State != "ok" {
		t.Fatalf("primary %q", st[0].State)
	}
}

type closeRec struct {
	mu     sync.Mutex
	closed bool
}

func (c *closeRec) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *closeRec) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// CloseLinksFrom closes exactly the links whose path dials from the address
// (after a SetPaths that dropped it, while they drain).
func TestLBCloseLinksFrom(t *testing.T) {
	paths := []LBPath{
		{Local: "10.0.0.1", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 50, Primary: true},
		{Local: "2a01::a", Remote: "2001:db8::1", Addr: "[2001:db8::1]:443", Weight: 50},
	}
	lb, _ := NewLB(paths, nil)
	var v4, v6 []*closeRec
	for i := 0; i < 6; i++ {
		lb.dialVia(func(p LBPath) (any, error) {
			c := &closeRec{}
			if p.Primary {
				v4 = append(v4, c)
			} else {
				v6 = append(v6, c)
			}
			return c, nil
		})
	}
	if len(v6) == 0 || len(v4) == 0 {
		t.Fatalf("placement %d/%d", len(v4), len(v6))
	}
	// the address changes: the new path set has the new source
	if err := lb.SetPaths([]LBPath{paths[0], {Local: "2a01::b", Remote: "2001:db8::1", Addr: "[2001:db8::1]:443", Weight: 50}}); err != nil {
		t.Fatal(err)
	}
	if n := lb.CloseLinksFrom("2a01::a"); n != len(v6) {
		t.Fatalf("closed %d, want %d", n, len(v6))
	}
	for _, c := range v6 {
		if !c.isClosed() {
			t.Fatal("a v6 link was not closed")
		}
		if w := lb.closeReason(c); !strings.Contains(w, "2a01::a left this server") {
			t.Fatalf("close reason %q", w)
		}
		lb.release(c) // what the pool does when the link ends
	}
	for _, c := range v4 {
		if c.isClosed() {
			t.Fatal("a v4 link was closed")
		}
	}
	for _, s := range lb.Stats() {
		if strings.HasPrefix(s.Path, "2a01::a>") {
			t.Fatalf("drained path still there: %+v", s)
		}
	}
}
