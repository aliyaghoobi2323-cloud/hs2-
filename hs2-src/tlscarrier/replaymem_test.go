package tlscarrier

import (
	"encoding/binary"
	"testing"
	"time"
)

func nonceOf(i uint64) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b, i)
	return b
}

// A reused nonce is refused; the map never grows past the cap, and a flood of
// fresh auths stays O(1) each (no full scans).
func TestReplayMemBoundedAndRejectsReuse(t *testing.T) {
	r := newReplayMem()
	if !r.add(nonceOf(1)) || r.add(nonceOf(1)) {
		t.Fatal("reuse not refused")
	}
	start := time.Now()
	for i := uint64(2); i < 3*replayCap; i++ {
		r.add(nonceOf(i))
		if len(r.m) > replayCap {
			t.Fatalf("map grew to %d past the cap %d", len(r.m), replayCap)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("%d adds took %s: not O(1)", 3*replayCap, d)
	}
	if len(r.fifo)-r.head != len(r.m) {
		t.Fatalf("queue %d live entries, map %d", len(r.fifo)-r.head, len(r.m))
	}
	// The newest entries are still remembered.
	if r.add(nonceOf(3*replayCap - 1)) {
		t.Fatal("a recent nonce was forgotten")
	}
}
