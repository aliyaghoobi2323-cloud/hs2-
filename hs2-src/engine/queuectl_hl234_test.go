package engine

import (
	"fmt"
	"testing"
	"time"
)

// TestQueueCtlHl234OwnShaperQueue (lab hl233 gS1/gS3 u8, -3.3 %): 8 uploads'
// onset under a kept cap, then a 102-142 ms tick while the links delivered
// >= the cap. With that delay in our own shaper's backlog: the cap stays, no
// hold. With no backlog (the path's queue), or a path queue past qOwnPath
// left after the backlog: hl216's qDown + hold as before.
func TestQueueCtlHl234OwnShaperQueue(t *testing.T) {
	defer func(k bool) { queueCtlKeep = k }(queueCtlKeep)
	queueCtlKeep = true
	if !qSeenOn() {
		t.Skip("HS2_QUEUE_CTL_SEEN=0")
	}
	for _, c := range []struct {
		own float64 // ms of the 120 ms tick in our shaper
		cut bool
	}{
		{110, false},
		{0, true},
		{80, true}, // 40 ms left on the path
		{-3, true}, // a 16 ms tick with 3 ms own: the path's (own not most of it)
	} {
		now := time.Unix(1e9, 0)
		q := &qCtl{rate: mb232(470.2), seen: mb232(477.4), seenAt: now, kept: true, fitRate: mb232(470.2), fitAt: now,
			path: mb232(477.4), pathAt: now, clearRun: qKeepSureTicks}
		var tr string
		tick := func(g, d, own float64) {
			now = now.Add(healthTick)
			q.step(qIn{now: now, delay: time.Duration(d * float64(time.Millisecond)), own: time.Duration(own * float64(time.Millisecond)), g: mb232(g)})
			tr += fmt.Sprintf(" %.0f/%.0f/%.0f->%.1f h%d", g, d, own, q.rate*8/1e6, q.hold)
		}
		for i := 0; i < 5; i++ {
			tick(14, 5, 0)
		}
		tick(465, 112, 0) // the onset tick (no backlog known: the onset rule)
		before := q.rate
		if c.own < 0 {
			tick(476, 16, -c.own)
		} else {
			tick(476, 120, c.own)
		}
		if c.cut {
			if q.rate >= before || q.hold == 0 {
				t.Fatalf("own %v: no cut: %s", c.own, tr)
			}
			continue
		}
		if q.rate != before || q.hold != 0 {
			t.Fatalf("own %v: own-shaper queue cut/held: %s", c.own, tr)
		}
		// the backlog drains, the path stays clear: the cap is still there to rise
		tick(474, 4, 0)
		if q.rate < before {
			t.Fatalf("own %v: cap fell after drain: %s", c.own, tr)
		}
	}
}

func TestQBacklogOf(t *testing.T) {
	for in, want := range map[string]uint64{
		" Sent 1 bytes 1 pkt (dropped 3, overlimits 0 requeues 0)\n backlog 15140b 10p requeues 0\n": 15140,
		" backlog 2878Kb 1923p requeues 0":   2878 * 1024,
		" backlog 3Mb 2000p requeues 0":      3 << 20,
		" backlog 0b 0p requeues 0":          0,
		"qdisc cake 8001: parent 1fe1:2 ...": 0,
	} {
		if got := qBacklogOf(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
	if d := qOwnDelay(uint64(0.1*12.5e6*qWire), 12.5e6); d < 99*time.Millisecond || d > 101*time.Millisecond {
		t.Errorf("qOwnDelay %v, want 100ms", d)
	}
	if qOwnDelay(1000, 0) != 0 {
		t.Error("qOwnDelay with no cap")
	}
}
