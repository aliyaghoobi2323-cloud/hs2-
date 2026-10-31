package engine

import (
	"net/netip"
	"testing"
)

// UserTag runs once per TUN packet (l3_link.go), so it must not allocate.
func TestUserTagNoAlloc(t *testing.T) {
	v4, v6 := netip.MustParseAddr("192.0.2.7"), netip.MustParseAddr("2001:db8:1:2::1")
	UserTag(v4, classV6Prefix) // the pool's first state
	if n := testing.AllocsPerRun(1000, func() {
		UserTag(v4, classV6Prefix)
		UserTag(v6, classV6Prefix)
	}); n > 0 {
		t.Fatalf("%.1f allocations a run, want 0", n)
	}
}

func BenchmarkUserTag(b *testing.B) {
	ip := netip.MustParseAddr("192.0.2.7")
	for _, sw := range []struct {
		name string
		v    int
	}{{"keyed", 1}, {"unkeyed", 0}} {
		b.Run(sw.name, func(b *testing.B) {
			defer func(v int) { TuneTagKey.Set(v) }(TuneTagKey.Get())
			TuneTagKey.Set(sw.v)
			b.ReportAllocs()
			for b.Loop() {
				UserTag(ip, classV6Prefix)
			}
		})
	}
}
