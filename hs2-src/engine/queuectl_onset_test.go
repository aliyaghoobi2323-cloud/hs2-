package engine

import (
	"math"
	"testing"
	"time"
)

// hl229: a new burst's single onset queue tick (< qFastDelay) under a cap
// within qOnsetNear of seen goes to qSeenFrac x seen with no hold, once per
// burst; a second queue tick, or a big one, takes the usual hold.
func TestQueueCtlOnsetQueueNoHold(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	var q *qCtl
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	set := func() {
		q = &qCtl{rate: mb(464.5), seen: mb(461.4), seenAt: now, path: mb(461.4), pathAt: now, kept: true, fitAt: now, fitRate: mb(464.5), up: true}
		tick(6*time.Millisecond, 13)
		tick(6*time.Millisecond, 13)
	}
	// lab hl228 aS3 u1b: the burst's first full tick, then a 23 ms queue
	set()
	tick(3*time.Millisecond, 0.96*464.5) // the burst probe (stops at 1.02 x seen)
	if q.onset == 0 || !q.onsetUntil.After(now) {
		t.Fatalf("no onset window after a burst under a cap at seen")
	}
	r := tick(23*time.Millisecond, 460)
	if want := qSeenFrac * 461.4; math.Abs(r-want) > 0.2 || q.hold != 0 {
		t.Fatalf("onset queue: cap %.1f hold %d, want %.1f and no hold", r, q.hold, want)
	}
	if q.onset != 0 || !q.onsetAt.Equal(now) || !q.onsetUntil.Equal(now.Add(qProbeQuiet)) {
		t.Fatalf("onset not consumed / drain window not set")
	}
	// the probe goes on from the next ticks (no 10 s hold)
	r0 := r
	for i := 0; i < qProbeTicks+2; i++ {
		r = tick(3*time.Millisecond, 0.97*q.rate*8/1e6)
	}
	if r <= r0 {
		t.Fatalf("no rise in %d clear ticks after the onset drop: %.1f", qProbeTicks+2, r)
	}
	// once per burst: a second queue tick in the window takes the hold
	set()
	tick(3*time.Millisecond, 0.96*464.5)
	tick(23*time.Millisecond, 460)
	tick(20*time.Millisecond, 0.99*q.rate*8/1e6)
	if q.hold == 0 {
		t.Fatalf("second onset queue tick: no hold")
	}
	// hl231: a big onset queue (>= qFastDelay) is the onset too: no hold;
	// a second big one in the window takes hl216's drain with its hold
	set()
	tick(3*time.Millisecond, 0.96*464.5)
	r = tick(45*time.Millisecond, 460)
	if want := qSeenFrac * 461.4; math.Abs(r-want) > 0.2 || q.hold != 0 {
		t.Fatalf("45 ms onset queue: cap %.1f hold %d, want %.1f and no hold", r, q.hold, want)
	}
	tick(45*time.Millisecond, 0.99*q.rate*8/1e6)
	if q.hold == 0 {
		t.Fatalf("second 45 ms onset queue tick: no hold")
	}
	// a cap well under seen (not within qOnsetNear): no onset window
	q = &qCtl{rate: mb(430), seen: mb(461.4), seenAt: now, path: mb(461.4), pathAt: now, kept: true, fitAt: now, fitRate: mb(430)}
	tick(6*time.Millisecond, 13)
	tick(6*time.Millisecond, 13)
	tick(3*time.Millisecond, 0.96*430)
	if q.onset != 0 {
		t.Fatalf("onset window under a cap 7 %% under seen")
	}
}

// hl229: a run that ended with >= qKeepSureTicks binding clear ticks at
// qSeenFrac x seen is kept at qKeepSure x seen; one that queued near its end
// at qSeenFrac x seen.
func TestQueueCtlKeepSure(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	for _, c := range []struct {
		name  string
		clear int
		want  float64
	}{
		{"sure", qKeepSureTicks, qKeepSure * 470},
		{"short", qKeepSureTicks - 1, qSeenFrac * 470},
	} {
		now := time.Unix(1e9, 0)
		q := &qCtl{rate: mb(qSeenFrac * 470), seen: mb(470), seenAt: now, path: mb(470), pathAt: now, fitAt: now, fitRate: mb(qSeenFrac * 470)}
		tick := func(d time.Duration, g float64) {
			now = now.Add(healthTick)
			q.step(qIn{now: now, delay: d, g: mb(g)})
		}
		q.clearRun = 0
		for i := 0; i < c.clear; i++ {
			q.probe = -100 // keep the probe from stepping
			tick(4*time.Millisecond, 0.97*q.rate*8/1e6)
			q.rate = mb(qSeenFrac * 470)
		}
		for i := 0; i < qIdleTicks+1; i++ {
			tick(2*time.Millisecond, 10)
		}
		if r := q.rate * 8 / 1e6; !q.kept || math.Abs(r-c.want) > 0.2 {
			t.Fatalf("%s: kept %v at %.1f, want %.1f", c.name, q.kept, r, c.want)
		}
	}
}

// hl229 (lab bS1): an onset guess under the path that a probe ran clear past
// is dropped (no ceiling), and an unconfirmed guess does not clamp the kept
// cap under the cap that fit.
func TestQueueCtlOnsetGuessDropped(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	tick := func(q *qCtl, d time.Duration, g float64) {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: d, g: mb(g)})
	}
	// kept: a guess of 431.8, the cap fit at 433.8 with no queue
	q := &qCtl{rate: mb(433.8), seen: mb(431.8), seenAt: now, seenGuess: true, fitAt: now, fitRate: mb(433.8)}
	for i := 0; i < qIdleTicks+1; i++ {
		tick(q, 0, 1.4)
	}
	if r := q.rate * 8 / 1e6; !q.kept || math.Abs(r-433.8) > 0.2 {
		t.Fatalf("kept %v at %.1f under an unconfirmed guess, want 433.8 (the cap that fit)", q.kept, r)
	}
	// growth past the guess drops it
	q = &qCtl{rate: mb(449.3), seen: mb(431.8), seenAt: now, seenGuess: true}
	tick(q, 3*time.Millisecond, 447.7)
	if q.seen != 0 || q.seenGuess {
		t.Fatalf("growth past a guess: seen %.1f guess %v, want dropped", q.seen*8/1e6, q.seenGuess)
	}
	// a measured seen is not dropped by the same tick
	q = &qCtl{rate: mb(449.3), seen: mb(431.8), seenAt: now}
	tick(q, 3*time.Millisecond, 447.7)
	if q.seen == 0 {
		t.Fatalf("a measured seen dropped")
	}
}

// hl230 (lab hl229 cS1): under an unconfirmed onset guess every clear binding
// tick raises the cap qUpFast — no creep, no probe wait — until a binding queue
// tick sets a real seen; an unconfirmed guess is never the kept cap.
func TestQueueCtlGuessStepFast(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	tick := func(q *qCtl, d time.Duration, g float64) {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: d, g: mb(g)})
	}
	// a ramp guess 412.4, the path 476, the cap at 0.975 x guess
	const path = 476.0
	var q *qCtl
	for _, guess := range []float64{412.4, 470} {
		q = &qCtl{rate: mb(0.975 * guess), seen: mb(guess), seenAt: now, seenGuess: true}
		n := 0
		for ; n < 12 && q.seenGuess && q.seen > 0; n++ {
			r := q.rate * 8 / 1e6
			if r > 1.005*path {
				break
			}
			prev := q.rate
			tick(q, 2*time.Millisecond, math.Min(0.99*r, path))
			if q.rate < prev*1.019 && q.seenGuess && q.seen > 0 {
				t.Fatalf("tick %d: clear binding tick under a guess rose %.1f -> %.1f, want +2 %%", n, prev*8/1e6, q.rate*8/1e6)
			}
		}
		if r := q.rate * 8 / 1e6; r < 0.98*path {
			t.Fatalf("after %d ticks the cap is %.1f, want >= %.1f", n, r, 0.98*path)
		}
		if n > 9 {
			t.Fatalf("%d ticks from 0.975 x %.1f to the path %.1f, want <= 9", n, guess, path)
		}
		// the first binding queue tick at the path confirms a real seen
		if q.seenGuess && q.seen > 0 {
			tick(q, 20*time.Millisecond, path)
			if q.seenGuess || math.Abs(q.seen*8/1e6-path) > 0.5 {
				t.Fatalf("queue tick: seen %.1f guess %v, want %.1f measured", q.seen*8/1e6, q.seenGuess, path)
			}
		}
		t.Logf("guess %.1f -> path %.1f in %d ticks, cap %.1f", guess, path, n, q.rate*8/1e6)
	}
	const guess = 412.4

	// a single upload filling 0.95 of its cap: not "clear binding" for this
	// rule (qNearFill), the usual creep/probe applies
	q = &qCtl{rate: mb(0.975 * guess), seen: mb(guess), seenAt: now, seenGuess: true}
	tick(q, 2*time.Millisecond, 0.95*0.975*guess)
	if r := q.rate * 8 / 1e6; r > 1.006*0.975*guess {
		t.Fatalf("0.95 fill rose to %.1f, want creep (<= %.1f)", r, 1.006*0.975*guess)
	}

	// kept: an unconfirmed guess is neither ceiling nor floor — the cap that fit
	q = &qCtl{rate: mb(405), seen: mb(guess), seenAt: now, seenGuess: true, fitAt: now, fitRate: mb(395)}
	for i := 0; i < qIdleTicks+1; i++ {
		tick(q, 0, 1.4)
	}
	if r := q.rate * 8 / 1e6; !q.kept || math.Abs(r-395) > 0.2 {
		t.Fatalf("kept %v at %.1f under a guess %.1f, want 395 (the cap that fit, not 0.975 x guess)", q.kept, r, guess)
	}
}

// hl231: lab hl230 traces (edge, Iran): a new burst's first binding queue
// tick of any size under a kept cap at the path goes to qSeenFrac x seen
// with no hold; a second queue tick in the window takes the drain and hold.
func TestQueueCtlOnsetBigNoHoldTraces(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	ms := time.Millisecond
	now := time.Unix(1e9, 0)
	var q *qCtl
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	set := func(cap, seen float64, guess bool) {
		q = &qCtl{rate: mb(cap), seen: mb(seen), seenAt: now, seenGuess: guess, kept: true, fitAt: now, fitRate: mb(cap), up: true}
		if !guess {
			q.path, q.pathAt = mb(seen), now
		}
		tick(0, 1.4)
		tick(0, 18.3)
	}
	type tk struct {
		d time.Duration
		g float64
	}
	cases := []struct {
		name       string
		cap, seen  float64
		pre        []tk // the burst's first ticks (clear)
		onset      tk
		next       tk   // the tick after the drop
		secondHold bool // next is a second queue tick: the drain and hold
	}{
		// dS1 u8 04:25:28: kept 468.2; 308.1 at 3 ms, then 77 ms at 468.0, then 2 ms at 483.6
		{"dS1 u8", 468.2, 475.3, []tk{{3 * ms, 308.1}}, tk{77 * ms, 468.0}, tk{2 * ms, 483.6}, false},
		// dS2 u8 04:34:43: kept 469.5; 326.8 at 3 ms, 91 ms at 473.6, 3 ms at 481.4
		{"dS2 u8", 469.5, 476.6, []tk{{3 * ms, 326.8}}, tk{91 * ms, 473.6}, tk{3 * ms, 481.4}, false},
		// dS3 u8 04:43:58: kept 470 -> burst probe; 78 ms at 471.3, then 70 ms at 472.6
		{"dS3 u8", 470.0, 477.2, []tk{{3 * ms, 447.5}}, tk{78 * ms, 471.3}, tk{70 * ms, 472.6}, true},
		// dRS2 u1hi 04:53:59: kept 232.4; 167.3, 241.3 (burst probe 239.4), 56 ms at 234.7, 3 ms at 220.5
		{"dRS2 u1hi", 232.4, 235.9, []tk{{3 * ms, 167.3}, {3 * ms, 241.3}}, tk{56 * ms, 234.7}, tk{3 * ms, 225}, false},
	}
	for _, c := range cases {
		set(c.cap, c.seen, false)
		for _, p := range c.pre {
			tick(p.d, p.g)
		}
		if q.onset == 0 {
			t.Fatalf("%s: no onset window", c.name)
		}
		seen := q.seen * 8 / 1e6
		r := tick(c.onset.d, c.onset.g)
		if want := qSeenFrac * seen; math.Abs(r-want) > 0.3 || q.hold != 0 {
			t.Fatalf("%s: onset %v: cap %.1f hold %d, want %.1f and no hold", c.name, c.onset.d, r, q.hold, want)
		}
		if q.onsetQ != c.onset.d || !q.onsetAt.Equal(now) || !q.onsetUntil.Equal(now.Add(qProbeQuiet)) {
			t.Fatalf("%s: stripe window not set for the onset queue (%v)", c.name, q.onsetQ)
		}
		r0 := r
		r = tick(c.next.d, c.next.g)
		if c.secondHold {
			if q.hold == 0 || r >= r0 {
				t.Fatalf("%s: second queue tick %v: cap %.1f hold %d, want the drain and hold", c.name, c.next.d, r, q.hold)
			}
			continue
		}
		if r < r0 || q.hold != 0 {
			t.Fatalf("%s: clear tick after the onset: cap %.1f hold %d", c.name, r, q.hold)
		}
		for i := 0; i < qProbeTicks+2; i++ {
			r = tick(3*ms, 0.97*q.rate*8/1e6)
		}
		if r <= r0 || q.hold != 0 {
			t.Fatalf("%s: no rise in %d clear ticks after the onset drop: %.1f (from %.1f)", c.name, qProbeTicks+2, r, r0)
		}
	}
	// an unconfirmed guess (412.4, a download's ramp): the onset queue tick's
	// delivery is seen; the cap goes to qSeenFrac of it with no hold
	set(405, 412.4, true)
	tick(3*ms, 0.6*405)
	if q.onset == 0 || !q.onsetGuess {
		t.Fatalf("guess: no onset window under a cap near the guess")
	}
	r := tick(60*ms, 401)
	if want := qSeenFrac * 401; math.Abs(r-want) > 0.3 || q.hold != 0 || q.seenGuess || math.Abs(q.seen*8/1e6-401) > 0.1 {
		t.Fatalf("guess onset: cap %.1f hold %d seen %.1f guess %v, want %.1f, seen 401, no hold", r, q.hold, q.seen*8/1e6, q.seenGuess, want)
	}
}
