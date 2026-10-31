//go:build !race

package engine

import (
	"net/netip"
	"testing"
)

// UserTag runs once per TUN packet (l3_link.go), so it must not allocate.
//
// Not under -race: there sync.Pool drops items on purpose (one Put in four)
// to shake out races, so the keyed path's pooled hash state is rebuilt now
// and then and this count says nothing about the normal build. The race
// detector found no data race in UserTag itself: TestUserTagKeyed and the
// rest of usertag_key_test.go still run under -race.
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
