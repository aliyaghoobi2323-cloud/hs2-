package tlscarrier

import (
	"net"
	"testing"
)

// b411: an IPv6 peer is exempt by its /64 (its auto6 address may move
// inside it), and the exempt bound is shared by the whole /64, so other
// addresses of that /64 cannot multiply the bound.
func TestB411V6PrefixBoundShared(t *testing.T) {
	p := NewPeerAllow()
	p.SetStatic([]string{"2001:db8:5:6::2", "auto6", "0.0.0.0", "::"})
	if s := p.Snap(); s.Static != 1 {
		t.Fatalf("static %d, want 1 (auto6 and unspecified skipped)", s.Static)
	}
	p.mu.Lock()
	p.bound = 3
	p.mu.Unlock()
	a := func(s string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(s), Port: 1} }
	var rel []func()
	for _, ip := range []string{"2001:db8:5:6::2", "2001:db8:5:6::9", "2001:db8:5:6::a"} {
		r, ex, ok := p.acquire(a(ip))
		if !ex || !ok {
			t.Fatalf("%s: exempt %v ok %v", ip, ex, ok)
		}
		rel = append(rel, r)
	}
	if _, ex, ok := p.acquire(a("2001:db8:5:6::b")); !ex || ok {
		t.Fatalf("4th conn of the /64 with bound 3: exempt %v ok %v, want exempt over its bound", ex, ok)
	}
	if _, ex, _ := p.acquire(a("2001:db8:5:7::2")); ex {
		t.Fatal("another /64 must not be exempt")
	}
	rel[0]()
	if r, _, ok := p.acquire(a("2001:db8:5:6::c")); !ok {
		t.Fatal("a freed slot must be reusable")
	} else {
		r()
	}
	// learned IPv6 also covers its /64, and leaves it when it expires
	q := NewPeerAllow()
	q.Learn(a("2001:db8:9:9::1"))
	if !q.Exempt(a("2001:db8:9:9::77")) {
		t.Fatal("learned IPv6: its /64 must be exempt")
	}
	q.mu.Lock()
	q.dropLearnedLocked(q.lru.Back())
	n := len(q.learn64)
	q.mu.Unlock()
	if n != 0 || q.Exempt(a("2001:db8:9:9::77")) {
		t.Fatal("dropped learned IPv6: its /64 must go")
	}
}
