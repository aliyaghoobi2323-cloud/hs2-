package engine

import (
	"math/rand/v2"
	"net/netip"
	"testing"
)

// The user tag is keyed (UserTag; the R1 review, part A, finding A3): stable
// within a process, different under another key, and the exit cannot read
// the address back from it the way it could from the unkeyed FNV-1a.
func TestUserTagKeyed(t *testing.T) {
	if TuneTagKey.Get() == 0 {
		t.Skip("HS2_TUNE_TAG_KEY=0")
	}
	old := userTagSecret
	t.Cleanup(func() { setUserTagKey(old) })
	addrs := make([]netip.Addr, 200)
	for i := range addrs {
		addrs[i] = netip.AddrFrom4([4]byte{byte(rand.IntN(223) + 1), byte(rand.IntN(256)), byte(rand.IntN(256)), byte(rand.IntN(256))})
	}
	// stable within the process (and with every other tag of the same user)
	first := make([]uint32, len(addrs))
	for i, a := range addrs {
		first[i] = UserTag(a, classV6Prefix)
		if first[i] == 0 {
			t.Fatalf("%s: tag 0", a)
		}
		if again := UserTag(a, classV6Prefix); again != first[i] {
			t.Fatalf("%s: tag %08x then %08x", a, first[i], again)
		}
		if m := UserTag(netip.AddrFrom16(a.As16()), classV6Prefix); m != first[i] {
			t.Fatalf("%s v4-mapped: %08x, want %08x", a, m, first[i])
		}
	}
	v6a, v6b := netip.MustParseAddr("2001:db8:1:2::1"), netip.MustParseAddr("2001:db8:1:2:ffff::9")
	if UserTag(v6a, classV6Prefix) != UserTag(v6b, classV6Prefix) {
		t.Fatal("two addresses of one /64: different tags")
	}
	if UserTag(v6a, classV6Prefix) == UserTag(netip.MustParseAddr("2001:db8:1:3::1"), classV6Prefix) {
		t.Fatal("two /64s: the same tag")
	}
	// another key (another edge process): other tags
	setUserTagKey([]byte("another edge key"))
	same := 0
	for i, a := range addrs {
		if UserTag(a, classV6Prefix) == first[i] {
			same++
		}
	}
	if same != 0 {
		t.Fatalf("%d of %d tags the same under another key", same, len(addrs))
	}
	setUserTagKey(old)
	for i, a := range addrs {
		if UserTag(a, classV6Prefix) != first[i] {
			t.Fatalf("%s: the process's key back, but not its tag", a)
		}
	}
}

// The review's inversion (review_a_test.go, invertTag4: a meet in the middle
// over FNV-1a) reads an unkeyed tag back to its address in milliseconds; a
// keyed tag gives it nothing. The unkeyed case runs too, so this test fails
// if the inversion itself stopped working.
func TestUserTagKeyedNotInvertible(t *testing.T) {
	if TuneTagKey.Get() == 0 {
		t.Skip("HS2_TUNE_TAG_KEY=0")
	}
	v := TuneTagKey.Get()
	t.Cleanup(func() { TuneTagKey.Set(v) })
	for i := 0; i < 5; i++ {
		ip := netip.AddrFrom4([4]byte{byte(rand.IntN(223) + 1), byte(rand.IntN(256)), byte(rand.IntN(256)), byte(rand.IntN(256))})
		TuneTagKey.Set(1)
		for _, a := range invertTag4(UserTag(ip, classV6Prefix)) {
			if a == ip {
				t.Fatalf("the keyed tag of %s was read back to it", ip)
			}
		}
		// the same inversion against the unkeyed tag (it checks its
		// candidates with UserTag, so with the switch off)
		TuneTagKey.Set(0)
		found := false
		for _, a := range invertTag4(UserTag(ip, classV6Prefix)) {
			found = found || a == ip
		}
		if !found {
			t.Fatalf("the inversion no longer reads the unkeyed tag of %s: this test proves nothing", ip)
		}
	}
}

// HS2_TUNE_TAG_KEY=0: round 1's unkeyed FNV-1a, as before.
func TestUserTagKeyOff(t *testing.T) {
	v := TuneTagKey.Get()
	t.Cleanup(func() { TuneTagKey.Set(v) })
	TuneTagKey.Set(0)
	ip := netip.MustParseAddr("192.0.2.7")
	b := ip.As4()
	if got, want := UserTag(ip, classV6Prefix), fnvAdd(fnvOffset, b[:]); got != want {
		t.Fatalf("switch off: tag %08x, want FNV-1a %08x", got, want)
	}
}
