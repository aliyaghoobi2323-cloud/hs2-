package engine

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"testing"
	"time"
)

// The R1 independent review, part A (wire protocol and security):
// docs/architecture/wip/r1/review/A-wire-security.fa.md. Tests named
// TestReviewA*; the ones that prove a finding left open fail by design and
// run only with HS2_REVIEW_RED=1 (reviewRed).

// reviewRed skips a proof of an open finding unless HS2_REVIEW_RED=1.
func reviewRed(t *testing.T) {
	t.Helper()
	if os.Getenv("HS2_REVIEW_RED") != "1" {
		t.Skip("proof of an open review finding (HS2_REVIEW_RED=1 runs it)")
	}
}

// tunPushCost is the mean time (ns) of n pushes to sq once it holds idle
// flows: each push a packet of a flow never seen, of user tag(i).
func tunPushCost(sq *tunSched, from, n int, tag func(i int) uint32) float64 {
	t0 := time.Now()
	for i := from; i < from+n; i++ {
		b := make([]byte, 64)
		sq.push(&b, tag(i), uint32(i)+1, false)
	}
	return float64(time.Since(t0).Nanoseconds()) / float64(n)
}

// A TUN side channel's per-flow queues (l3sched.go) cost each packet a pass
// over every flow its user had in the last tunFlowIdle (2 s) once the user is
// at tunUserPkts (longest), and over every user of the link once the link is
// at tunLinkPkts (fattest): flows with nothing queued stay in those maps
// until the tick forgets them, so a burst of new flows — a scan, a DHT
// client, packets with changing ports into a routed TUN — made each packet
// cost O(flows) under the scheduler's lock, on pumpTun, the TUN's one
// reader for every link (review A, finding A2). The pass must cover only
// what has packets queued (bounded by tunUserPkts and tunLinkPkts).
func TestReviewATunFloodCost(t *testing.T) {
	for _, c := range []struct {
		name string
		tag  func(i int) uint32
	}{
		{"one user", func(int) uint32 { return 7 }},
		// a user per flow: the link's bound (fattest) is the one hit
		{"a user a flow", func(i int) uint32 { return uint32(i)%1000003 + 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sq := newTunSched(nil)
			defer sq.close() // (its bytes leave the users' accounts, userMems)
			// fill to the bounds, then time pushes with few idle flows and
			// with many (as a burst of 64k new flows within 2 s leaves)
			tunPushCost(sq, 0, 4096, c.tag)
			few := tunPushCost(sq, 4096, 2048, c.tag)
			tunPushCost(sq, 8192, 60000, c.tag)
			many := tunPushCost(sq, 70000, 2048, c.tag)
			t.Logf("%s: %.0f ns a push at ~6k flows, %.0f ns at ~70k (%.1fx); flows %d, users %d",
				c.name, few, many, many/few, len(sq.flows), len(sq.users))
			if many > 8*few && many > 20000 {
				t.Fatalf("a push costs %.0f ns with 70k idle flows against %.0f ns with 6k: O(flows) per packet", many, few)
			}
		})
	}
}

// The overflow choice is unchanged: a user past tunUserPkts loses the head
// of its longest flow, and the link past tunLinkPkts the head of the longest
// flow of its fattest user (the fix only skips what has nothing queued).
func TestReviewATunDropChoice(t *testing.T) {
	sq := newTunSched(nil)
	push := func(tag, key uint32, n int) {
		for i := 0; i < n; i++ {
			b := make([]byte, 10)
			sq.push(&b, tag, key, false)
		}
	}
	// user 1: a long flow (60) and many short ones up to its bound
	push(1, 100, 60)
	for k := uint32(0); k < 964; k++ {
		push(1, 1000+k, 1)
	}
	if u := sq.users[1]; u.pkts != tunUserPkts {
		t.Fatalf("user 1 holds %d, want %d", u.pkts, tunUserPkts)
	}
	push(1, 5000, 1) // past the user's bound: the long flow loses its head
	if f := sq.flows[100]; f.n() != 59 {
		t.Fatalf("the longest flow holds %d, want 59 (it pays for the user's overflow)", f.n())
	}
	// fill the link from other users, then a new user: the fattest (1)
	// pays, from its longest flow
	for tag := uint32(2); sq.pkts < tunLinkPkts; tag++ {
		push(tag, 10000+tag, 1)
	}
	before := sq.flows[100].n()
	push(99999, 99999, 1)
	if got := sq.flows[100].n(); got != before-1 {
		t.Fatalf("the fattest user's longest flow holds %d, want %d", got, before-1)
	}
	tunQueuedSetsAgree(t, sq)
	// drained, a flow leaves the sets; the tick forgets the idle ones
	for sq.flows[100].n() > 0 {
		sq.dropHead(sq.flows[100])
	}
	tunQueuedSetsAgree(t, sq)
	sq.close()
	tunQueuedSetsAgree(t, sq)
}

// tunQueuedSetsAgree checks the sets of what has packets queued (the fix,
// review_a_fix_test.go) against the queues themselves; none before it.
var tunQueuedSetsAgree = func(t *testing.T, sq *tunSched) {}

// fnvInv32 is the inverse of fnvPrime mod 2^32.
func fnvInv32() uint32 {
	x := uint32(fnvPrime) // Newton: x = x(2 - p x), doubling the good bits
	for i := 0; i < 5; i++ {
		x *= 2 - fnvPrime*x
	}
	return x
}

// invertTag4 recovers the IPv4 addresses whose user tag is tag
// (UserTag: FNV-1a of the 4 bytes) by meeting in the middle: 2 x 65536
// steps, not 2^32.
func invertTag4(tag uint32) []netip.Addr {
	inv := fnvInv32()
	mid := make(map[uint32][]uint16, 1<<16)
	for b := 0; b < 1<<16; b++ {
		h := fnvAdd(fnvOffset, []byte{byte(b >> 8), byte(b)})
		mid[h] = append(mid[h], uint16(b))
	}
	var out []netip.Addr
	x3 := tag * inv // h3 ^ b3
	for b3 := 0; b3 < 256; b3++ {
		x2 := (x3 ^ uint32(b3)) * inv // h2 ^ b2
		for b2 := 0; b2 < 256; b2++ {
			for _, lo := range mid[x2^uint32(b2)] {
				a := netip.AddrFrom4([4]byte{byte(lo >> 8), byte(lo), byte(b2), byte(b3)})
				if UserTag(a, classV6Prefix) == tag {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

// The user tag every user stream's SYN carries to the exit (§4.2, C1) is
// an unkeyed FNV-1a of the user's IPv4 address (or IPv6 /64): the exit, or
// whoever reads its memory, gets each connection's user address back from
// it in milliseconds. Before R1 the exit never learned which address in Iran
// a connection came from. The tag must not reveal the address (a key only
// the edge holds, e.g. a random salt per edge process: the exit only needs
// the tags to tell users apart) (review A, finding A3).
func TestReviewAUserTagRevealsAddress(t *testing.T) {
	reviewRed(t)
	ip := netip.AddrFrom4([4]byte{byte(rand.IntN(223) + 1), byte(rand.IntN(256)), byte(rand.IntN(256)), byte(rand.IntN(256))})
	tag := UserTag(ip, classV6Prefix)
	t0 := time.Now()
	got := invertTag4(tag)
	el := time.Since(t0)
	for _, a := range got {
		if a == ip {
			t.Fatalf("the exit reads the user's address %s back from its tag %08x in %v (%d candidates: %v)", ip, tag, el, len(got), fmt.Sprint(got))
		}
	}
	t.Logf("tag %08x of %s not inverted (%d candidates)", tag, ip, len(got))
}
