package engine

import (
	"testing"
	"time"
)

func mb232(x float64) float64 { return x * 1e6 / 8 }

// hl232 (lab hl231 eS3): u1b's growth steps with no queue under an
// unconfirmed guess (431.8) left the cap kept at 491.4 (path ~475); the u8
// onset window did not open and its first, partial tick (49 ms at 434.8)
// followed down + hold. Now the kept cap is at most qNearPath x what the
// links delivered with no queue, and u8's first tick takes the onset rule.
func TestQueueCtlHl232KeepAfterGrowthAndOnset(t *testing.T) {
	defer func(k bool) { queueCtlKeep = k }(queueCtlKeep)
	queueCtlKeep = true
	if !qSeenOn() {
		t.Skip("HS2_QUEUE_CTL_SEEN=0")
	}
	now := time.Unix(1e9, 0)
	q := &qCtl{rate: mb232(438), seen: mb232(431.8), seenAt: now, seenGuess: true, kept: true,
		fitRate: mb232(438), fitAt: now, up: true}
	tick := func(g, d float64) {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: time.Duration(d * float64(time.Millisecond)), g: mb232(g)})
	}
	for i := 0; i < 5; i++ {
		tick(1.4, 0)
	}
	for _, s := range [][2]float64{{182.6, 1}, {436.8, 3}, {445.6, 3}, {437.3, 0}, {463.8, 10}, {319.9, 0}, {436.0, 11}, {474.9, 4}} {
		tick(s[0], s[1])
	}
	for i := 0; i < qIdleTicks+2; i++ {
		tick(9.8, 0)
	}
	if !q.kept {
		t.Fatalf("not kept: cap %.1f", q.rate*8/1e6)
	}
	if lim := qNearPath * mb232(474.9); q.rate > lim+1 {
		t.Fatalf("kept %.1f over qNearPath x 474.9 delivered with no queue (%.1f)", q.rate*8/1e6, lim*8/1e6)
	}
	if !q.seenClear || q.seen != mb232(474.9) {
		t.Fatalf("seen %.1f clear=%v, want 474.9 from the clear ticks", q.seen*8/1e6, q.seenClear)
	}
	kept := q.rate
	// u8: the burst's first tick, 49 ms at 434.8
	tick(434.8, 49)
	if q.hold != 0 {
		t.Fatalf("onset tick held (hold %d), cap %.1f (kept %.1f)", q.hold, q.rate*8/1e6, kept*8/1e6)
	}
	if want := qSeenFrac * mb232(474.9); q.rate < want-1 {
		t.Fatalf("onset cap %.1f, want >= %.1f", q.rate*8/1e6, want*8/1e6)
	}
}

// hl232: the burst's first tick with a big queue that does not fill a cap
// kept over seen (but would bind the onset's cap) is the onset: qSeenFrac x
// seen, no hold. One under that (a path that shrank) follows down + hold.
func TestQueueCtlHl232OnsetPartialTick(t *testing.T) {
	defer func(k bool) { queueCtlKeep = k }(queueCtlKeep)
	queueCtlKeep = true
	if !qSeenOn() {
		t.Skip("HS2_QUEUE_CTL_SEEN=0")
	}
	for _, c := range []struct {
		rate, g float64
		onset   bool
		want    float64
	}{
		{491.4, 434.8, true, qSeenFrac * 475}, // kept 3 % over seen: to 0.975 x seen

		{491.4, 300.0, false, 0}, // a path that shrank: follow-down + hold
	} {
		now := time.Unix(1e9, 0)
		q := &qCtl{rate: mb232(c.rate), seen: mb232(475), seenAt: now, kept: true, fitRate: mb232(c.rate), fitAt: now, up: true}
		for i := 0; i < 3; i++ {
			now = now.Add(healthTick)
			q.step(qIn{now: now, delay: 0, g: mb232(1.4)})
		}
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: 49 * time.Millisecond, g: mb232(c.g)})
		if !c.onset {
			if q.hold == 0 && q.rate == mb232(c.rate) {
				// a queue under qFastDelay waits a tick (lostQ); 49 ms follows at once
				t.Fatalf("rate %.1f g %.1f: no follow-down", c.rate, c.g)
			}
			continue
		}
		if q.hold != 0 || q.onset != 0 {
			t.Fatalf("rate %.1f g %.1f: hold %d onset %d, want the onset rule", c.rate, c.g, q.hold, q.onset)
		}
		if got := q.rate * 8 / 1e6; got < c.want-0.01 || got > c.want+0.01 {
			t.Fatalf("rate %.1f g %.1f: cap %.2f, want %.2f", c.rate, c.g, got, c.want)
		}
		if q.onsetQ != 49*time.Millisecond {
			t.Fatalf("onsetQ %v, want 49ms for the stripe gate", q.onsetQ)
		}
	}
}

// hl232: a cap kept with no seen (measured or guessed) and no growth past one
// keeps the rules before hl232 (TestQueueCtlKeepFitAfterPath); a seen a
// queue measured is not replaced by clear ticks within qSeenGrow of it.
func TestQueueCtlHl232KeepMeasuredSeenUnchanged(t *testing.T) {
	defer func(k bool) { queueCtlKeep = k }(queueCtlKeep)
	queueCtlKeep = true
	if !qSeenOn() {
		t.Skip("HS2_QUEUE_CTL_SEEN=0")
	}
	now := time.Unix(1e9, 0)
	q := &qCtl{rate: mb232(470), seen: mb232(475), seenAt: now, clearMax: mb232(480), clearAt: now}
	q.keepClearSeen(now)
	if q.seenClear || q.seen != mb232(475) {
		t.Fatalf("seen %.1f clear %v: a measured seen within qSeenGrow must stay", q.seen*8/1e6, q.seenClear)
	}
	q = &qCtl{rate: mb232(470), clearMax: mb232(480), clearAt: now}
	q.keepClearSeen(now)
	if q.seenClear || q.seen != 0 {
		t.Fatalf("no seen and no dropped guess: clearMax must not become seen")
	}
}
