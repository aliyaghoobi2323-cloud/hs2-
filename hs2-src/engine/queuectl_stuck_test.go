package engine

import (
	"testing"
	"time"
)

// qctl61, the stuck cap (lat59, d8): the edge's light upload, ~7 Mbit/s,
// sees the download's queue (24-130 ms) while its links receive the 8
// downloads. No cap may start there, by its rate alone or by the reverse
// traffic alone.
func TestQueueCtlLightUploadUnderDownloadsNeverCaps(t *testing.T) {
	for _, c := range []struct {
		name  string
		g, rx float64
	}{
		{"light, downloads", 7e6 / 8, 1600e6 / 8},
		{"light, no reverse", 7e6 / 8, 0},
		{"30 Mbit/s, downloads", 30e6 / 8, 1600e6 / 8},
	} {
		for _, fast := range []bool{false, true} {
			func() {
				defer func(v bool) { queueCtlFast = v }(queueCtlFast)
				queueCtlFast = fast
				var q qCtl
				now := time.Unix(1e9, 0)
				for i := 0; i < 60; i++ {
					now = now.Add(healthTick)
					d := time.Duration(24+i%5*25) * time.Millisecond
					if r, note := q.step(qIn{now: now, delay: d, g: c.g, rx: c.rx}); r > 0 {
						t.Fatalf("%s fast=%v: capped at %.1f Mbit/s on tick %d (%s)", c.name, fast, mbitps(r), i, note)
					}
				}
			}()
		}
	}
	// An upload of its own rate, the downloads light: still capped as before.
	var q qCtl
	now := time.Unix(1e9, 0)
	capped := false
	for i := 0; i < qStartTicks; i++ {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: 60 * time.Millisecond, g: qC, rx: qC / 10})
		capped = r > 0
	}
	if !capped {
		t.Fatalf("a real upload queue was not capped after %d ticks", qStartTicks)
	}
}

// qStuck replays lat59's stuck cap: a cap set at rate, then the links
// deliver frac of it (the shaped links' payload; the cap binds on what the
// controller does not count) at delay, with no queue it would see; it
// returns the tick the cap came off (0: never in n ticks).
func qStuck(rate, frac float64, delay time.Duration, rx float64, n int) int {
	q := qCtl{rate: rate}
	now := time.Unix(1e9, 0)
	for i := 1; i <= n; i++ {
		now = now.Add(healthTick)
		if r, _ := q.step(qIn{now: now, delay: delay, g: frac * rate, rx: rx}); r == 0 {
			return i
		}
	}
	return 0
}

// A cap that neither binds nor idles comes off after qLimboTicks (before
// qctl61 it stayed for the whole 40 s run and beyond), at any delay under
// qHigh; one that binds, or idles, keeps its own rules.
func TestQueueCtlLimboCapComesOff(t *testing.T) {
	for _, d := range []time.Duration{2 * time.Millisecond, 10 * time.Millisecond} {
		for _, frac := range []float64{0.5, 0.67, 0.85} {
			if got := qStuck(qC, frac, d, 0, 100); got != qLimboTicks {
				t.Errorf("delay %v, %.0f%% of the cap: off at tick %d, want %d", d, 100*frac, got, qLimboTicks)
			}
		}
	}
	if got := qStuck(qC, 0.95, 10*time.Millisecond, 0, 100); got != 0 {
		t.Errorf("a binding cap at 10 ms came off at tick %d", got)
	}
	if got := qStuck(qC, 0.2, 2*time.Millisecond, 0, 100); got != qIdleTicks {
		t.Errorf("an idle cap came off at tick %d, want %d (idle)", got, qIdleTicks)
	}
	// A limbo release is an idle one: a queue that comes back is capped
	// again after qStartTicks, not held off for a minute.
	q := qCtl{rate: qC}
	now := time.Unix(1e9, 0)
	for i := 0; i < qLimboTicks; i++ {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: 10 * time.Millisecond, g: 0.7 * qC})
	}
	if q.rate != 0 || now.Before(q.release) {
		t.Fatalf("after limbo: rate %.0f, release in %v", q.rate, q.release.Sub(now))
	}
	for i := 1; i <= qStartTicks; i++ {
		now = now.Add(healthTick)
		if r, _ := q.step(qIn{now: now, delay: 60 * time.Millisecond, g: qC}); (r > 0) != (i == qStartTicks) {
			t.Fatalf("queue back after limbo: tick %d cap %.1f", i, mbitps(r))
		}
	}
}

// A cap in place when the other direction takes over (downloads start, the
// upload falls) comes off after qRevTicks.
func TestQueueCtlReverseTakesCapOff(t *testing.T) {
	if got := qStuck(qC, 0.3, 60*time.Millisecond, 20*qC, 100); got != qRevTicks {
		t.Fatalf("reverse traffic: cap off at tick %d, want %d", got, qRevTicks)
	}
	// Both directions heavy (rx under qRevFrac of what it sends): the cap holds.
	if got := qStuck(qC, 0.95, 10*time.Millisecond, 2*qC, 100); got != 0 {
		t.Fatalf("both directions heavy: cap off at tick %d", got)
	}
}
