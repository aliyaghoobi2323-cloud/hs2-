package engine

import (
	"math"
	"testing"
	"time"
)

// HS2_QUEUE_CTL_KEEP (hl204): a cap that fit the path is kept through idle
// while the path's rate is fresh (and through the other direction's queue),
// and removed once it is older than qPathMemory; key off: removed after
// qIdleTicks as before.
func TestQueueCtlKeepIdleCap(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	const g = 60e6 // bytes/s, ~480 Mbit/s
	run := func(keep bool) (q *qCtl, now time.Time) {
		queueCtlKeep = keep
		q = &qCtl{}
		now = time.Unix(1e9, 0)
		tick := func(in qIn) float64 {
			now = now.Add(healthTick)
			in.now = now
			r, _ := q.step(in)
			return r
		}
		for i := 0; i < qStartTicks+qGraceTicks; i++ { // a heavy burst's queue: capped
			tick(qIn{delay: 80 * time.Millisecond, g: g})
		}
		if q.rate == 0 {
			t.Fatalf("keep=%v: no cap after a queue", keep)
		}
		for i := 0; i < 3; i++ { // the cap binds, no queue: it fits the path
			tick(qIn{delay: 3 * time.Millisecond, g: 0.95 * q.rate})
		}
		for i := 0; i < 3*qIdleTicks; i++ { // the burst ended; the download direction's queue shows
			tick(qIn{delay: 60 * time.Millisecond, g: 0.2e6, rx: 50e6})
		}
		return q, now
	}
	if q, _ := run(false); q.rate != 0 {
		t.Errorf("key off: cap %.0f after idle, want removed", q.rate)
	}
	q, now := run(true)
	if q.rate == 0 {
		t.Fatalf("key on: cap removed in idle, want kept")
	}
	if !q.release.IsZero() {
		t.Errorf("key on: a back-off was set (%v) while the kept cap idled", q.release)
	}
	kept := q.rate
	// Next burst: capped from its first tick (no onset), rate unchanged.
	now = now.Add(healthTick)
	if r, _ := q.step(qIn{now: now, delay: 3 * time.Millisecond, g: 0.95 * kept}); r < kept {
		t.Errorf("key on: next burst cap %.0f, want at least %.0f", r, kept)
	}
	// Idle past qPathMemory: removed.
	for i := 0; i < int(qPathMemory/healthTick)+2*qIdleTicks; i++ {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: 2 * time.Millisecond, g: 0.1e6})
	}
	if q.rate != 0 {
		t.Errorf("key on: cap %.0f after %v idle, want removed", q.rate, qPathMemory)
	}
}

// hl205: a back-off the reverse rule set does not keep this direction
// uncapped when its own big queue comes (u8 right after a download); key off,
// and a back-off of any other kind, it still does.
func TestQueueCtlKeepRevBackoffOwnQueue(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	const g = 60e6
	run := func(keep, revOff bool) float64 {
		queueCtlKeep = keep
		now := time.Unix(1e9, 0)
		q := &qCtl{revOff: revOff, release: now.Add(5 * time.Minute), releaseFor: 5 * time.Minute}
		var r float64
		for i := 0; i < qStartTicks+1; i++ {
			now = now.Add(healthTick)
			r, _ = q.step(qIn{now: now, delay: 250 * time.Millisecond, g: g, rx: 2e6})
		}
		if r > 0 && !q.release.IsZero() {
			t.Errorf("keep=%v revOff=%v: capped but back-off still set", keep, revOff)
		}
		return r
	}
	if r := run(true, true); r == 0 {
		t.Errorf("key on, reverse back-off: own queue left uncapped")
	}
	if r := run(false, true); r != 0 {
		t.Errorf("key off: capped %.0f inside the back-off", r)
	}
	if r := run(true, false); r != 0 {
		t.Errorf("key on, non-reverse back-off: capped %.0f inside it", r)
	}
	// The other direction's queue (reverse) still blocks inside the back-off.
	queueCtlKeep = true
	now := time.Unix(1e9, 0)
	q := &qCtl{revOff: true, release: now.Add(5 * time.Minute)}
	for i := 0; i < qStartTicks+1; i++ {
		now = now.Add(healthTick)
		if r, _ := q.step(qIn{now: now, delay: 80 * time.Millisecond, g: 4e6, rx: 50e6}); r != 0 {
			t.Fatalf("reverse queue capped inside the back-off")
		}
	}
	// The reverse rule sets revOff on its drop.
	q = &qCtl{rate: 50e6, hold: 10}
	for i := 0; i < qRevTicks; i++ {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: 80 * time.Millisecond, g: 30e6, rx: 200e6})
	}
	if q.rate != 0 || !q.revOff {
		t.Errorf("reverse drop: rate %.0f revOff %v, want 0 true", q.rate, q.revOff)
	}
}

// hl205: a kept cap learned on a slow path regrows fast once the path is
// back to full rate (the cap binds with no queue well past the old rate).
func TestQueueCtlKeepRegrowsAfterPathRateUp(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	const real = 60e6 // the path now (~480 Mbit/s); it was ~232
	ticks := func(keep bool) int {
		queueCtlKeep = keep
		now := time.Unix(1e9, 0)
		q := &qCtl{rate: 28e6, path: 29e6, pathAt: now, good: 28e6}
		for i := 1; i <= 400; i++ {
			now = now.Add(healthTick)
			in := qIn{now: now, delay: 3 * time.Millisecond, g: 0.95 * q.rate}
			if q.rate > real {
				in.delay, in.g = 80*time.Millisecond, real
			}
			q.step(in)
			if q.rate >= 0.9*real && q.rate <= real {
				return i
			}
		}
		return 400
	}
	on, off := ticks(true), ticks(false)
	if on > 30 {
		t.Errorf("key on: %d ticks to regrow to the path, want <= 30", on)
	}
	if off < 100 {
		t.Errorf("key off: %d ticks (want the old slow climb, >= 100)", off)
	}
	t.Logf("ticks to 90%% of the path: key on %d, off %d", on, off)
}

// hl208: a cap that crept up with no queue after the path's rate was learned
// is kept at that fit cap, not at 0.97 of the older overshoot; one learned
// after the last fit is still clamped.
func TestQueueCtlKeepFitAfterPath(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	queueCtlKeep = true
	now := time.Unix(1e9, 0)
	idle := func(q *qCtl) {
		for i := 0; i < qIdleTicks+1; i++ {
			now = now.Add(healthTick)
			q.step(qIn{now: now, delay: 5 * time.Millisecond, g: 1.7e6})
		}
	}
	// fit at 61.4 after the path (60.5) was learned: kept, but not above the path (hl216)
	q := &qCtl{rate: 61.4e6, path: 60.5e6, pathAt: now}
	now = now.Add(healthTick)
	q.step(qIn{now: now, delay: 3 * time.Millisecond, g: 0.93 * q.rate})
	fit := q.fitRate
	idle(q)
	if want := math.Min(fit, 60.5e6); q.rate != want || !q.kept {
		t.Fatalf("kept %.2f, want %.2f (the fit cap %.2f, not above the path)", q.rate/1e6, want/1e6, fit/1e6)
	}
	// path learned after the last fit: clamped to 0.97 of it
	q = &qCtl{rate: 61.4e6, path: 60.5e6, fitRate: 61.4e6, fitAt: now.Add(-time.Minute), pathAt: now}
	idle(q)
	if want := qBackFrac * 60.5e6; q.rate != want {
		t.Fatalf("kept %.2f, want %.2f (0.97 of the path)", q.rate/1e6, want/1e6)
	}
}

// hl208: past the path's last rate with no queue the cap rises 2 %/tick (not
// 0.5 %), so a fixed path is overshot by at most ~2 %; under it, as before.
func TestQueueCtlKeepAbovePathStep(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	for _, keep := range []bool{true, false} {
		queueCtlKeep = keep
		now := time.Unix(1e9, 0).Add(healthTick)
		q := &qCtl{rate: 60.5e6, path: 60e6, pathAt: now}
		q.step(qIn{now: now, delay: 3 * time.Millisecond, g: 0.995 * q.rate})
		want := 60.5e6 * qUpSlow
		if keep {
			want = 60.5e6 * qUpFast
		}
		if math.Abs(q.rate-want) > 1 {
			t.Fatalf("keep=%v: %.3f, want %.3f", keep, q.rate/1e6, want/1e6)
		}
		q = &qCtl{rate: 59.5e6, path: 60e6, pathAt: now}
		q.step(qIn{now: now.Add(healthTick), delay: 3 * time.Millisecond, g: 0.95 * q.rate})
		if want := 59.5e6 * qUpSlow; math.Abs(q.rate-want) > 1 {
			t.Fatalf("keep=%v under the path: %.3f, want %.3f", keep, q.rate/1e6, want/1e6)
		}
	}
}

// hl208: a cap past the path that the links do not fill (8 downloads: ~0.92 of
// it) neither regrows nor rises 2 %/tick.
func TestQueueCtlKeepNoFalseRegrow(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	queueCtlKeep = true
	now := time.Unix(1e9, 0).Add(healthTick)
	q := &qCtl{rate: 62.25e6, path: 58.1e6, pathAt: now} // 498 vs a 465 path
	q.step(qIn{now: now, delay: 3 * time.Millisecond, g: 56.75e6})
	if q.path == 0 || math.Abs(q.rate-62.25e6*qUpSlow) > 1 {
		t.Fatalf("path %.2f rate %.3f: want no regrow, 0.5 %% step", q.path/1e6, q.rate/1e6)
	}
}

// hl214: a single upload under a kept cap at the path's last rate delivers
// ~0.95 of it (never more than the path): 2 %/tick up to qRegrowFrac, then
// the regrow once it delivers more than the path.
func TestQueueCtlKeepUploadRegrow(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	queueCtlKeep = true
	now := time.Unix(1e9, 0).Add(healthTick)
	path := 29e6 // 232 Mbit/s
	q := &qCtl{rate: path, path: path, pathAt: now}
	n := 0
	for ; q.path != 0 && n < 40; n++ {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: 3 * time.Millisecond, g: math.Min(0.95*q.rate, 62.5e6)})
	}
	if q.path != 0 || n > 6 {
		t.Fatalf("regrow after %d ticks (path %.2f, rate %.2f): want <= 6", n, q.path/1e6, q.rate/1e6)
	}
}

// hl216: just under the path's last rate, links that fill the cap with no
// queue rise 2 %/tick (a path that grew back regrows in ~4 ticks, not ~10);
// links that do not fill it rise 0.5 % as before.
func TestQueueCtlKeepNearPathFill(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	queueCtlKeep = true
	now := time.Unix(1e9, 0).Add(healthTick)
	path := 30e6 // 240 Mbit/s, the path grew to 500
	q := &qCtl{rate: 0.99 * path, path: path, pathAt: now}
	n := 0
	for ; q.path != 0 && n < 40; n++ {
		now = now.Add(healthTick)
		q.step(qIn{now: now, delay: 3 * time.Millisecond, g: math.Min(0.98*q.rate, 62.5e6)})
	}
	if q.path != 0 || n > 5 {
		t.Fatalf("regrow after %d ticks (rate %.2f): want <= 5", n, q.rate/1e6)
	}
	q = &qCtl{rate: 0.99 * path, path: path, pathAt: now}
	q.step(qIn{now: now.Add(healthTick), delay: 3 * time.Millisecond, g: 0.95 * q.rate})
	if want := 0.99 * path * qUpSlow; math.Abs(q.rate-want) > 1 {
		t.Fatalf("not filled: %.3f, want %.3f", q.rate/1e6, want/1e6)
	}
}

// hl216: a binding cap with a queue of qFastDelay or more steps to qDown once
// (then 0.97 steps while the hold runs); a small queue steps 0.97.
func TestQueueCtlKeepBigQueueDrain(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	for _, keep := range []bool{true, false} {
		queueCtlKeep = keep
		now := time.Unix(1e9, 0).Add(healthTick)
		q := &qCtl{rate: 60e6, kept: true}
		q.step(qIn{now: now, delay: 80 * time.Millisecond, g: 60e6})
		want := 60e6 * qBackFrac
		if keep {
			want = 60e6 * qDown
		}
		if math.Abs(q.rate-want) > 1 || math.Abs(q.path-60e6) > 1 {
			t.Fatalf("keep=%v: rate %.3f path %.3f, want %.3f / 60", keep, q.rate/1e6, q.path/1e6, want/1e6)
		}
		r := q.rate
		q.step(qIn{now: now.Add(healthTick), delay: 80 * time.Millisecond, g: r})
		if math.Abs(q.rate-r*qBackFrac) > 1 {
			t.Fatalf("keep=%v second tick: %.3f, want %.3f", keep, q.rate/1e6, r*qBackFrac/1e6)
		}
		q = &qCtl{rate: 60e6}
		q.step(qIn{now: now, delay: 20 * time.Millisecond, g: 60e6})
		want = 60e6 * qBackFrac
		if keep {
			want = 60e6 * qSeenFrac // hl223: under what the path delivered with a queue
		}
		if math.Abs(q.rate-want) > 1 {
			t.Fatalf("keep=%v small queue: %.3f", keep, q.rate/1e6)
		}
	}
}

// hl216: a kept cap at the path does not rise on the first tick it binds (a
// burst starting); it does on the next.
func TestQueueCtlKeepNoRiseOnOnset(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	queueCtlKeep = true
	now := time.Unix(1e9, 0).Add(healthTick)
	q := &qCtl{rate: 58.5e6, path: 58.5e6, pathAt: now, kept: true}
	q.step(qIn{now: now, delay: 2 * time.Millisecond, g: 0.965 * q.rate})
	if q.rate != 58.5e6 || q.kept {
		t.Fatalf("onset: rate %.3f kept %v, want 58.5 unchanged, not kept", q.rate/1e6, q.kept)
	}
	q.step(qIn{now: now.Add(healthTick), delay: 2 * time.Millisecond, g: 0.98 * q.rate})
	if want := 58.5e6 * qUpFast; math.Abs(q.rate-want) > 1 {
		t.Fatalf("second tick: %.3f, want %.3f", q.rate/1e6, want/1e6)
	}
}

// hl219: under KEEP a cap that does not bind follows the path down only on
// the second small queue tick in a row (lab d8: one 16 ms tick cut 487 -> 406
// for the rest of the burst), or at once on a queue of qFastDelay or more.
func TestQueueCtlKeepFollowDownTwoTicks(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	now := time.Unix(1e9, 0).Add(healthTick)
	tick := func(q *qCtl, i int, d time.Duration, g float64) {
		q.step(qIn{now: now.Add(time.Duration(i) * healthTick), delay: d, g: g})
	}
	queueCtlKeep = false
	q := &qCtl{rate: 487e6 / 8}
	tick(q, 0, 16*time.Millisecond, 419e6/8)
	if want := q.floorOf(math.Min(487e6/8*qDown, qBackFrac*419e6/8)); math.Abs(q.rate-want) > 1 {
		t.Fatalf("keep off: %.1f, want %.1f (at once)", q.rate*8/1e6, want*8/1e6)
	}
	queueCtlKeep = true
	q = &qCtl{rate: 487e6 / 8}
	tick(q, 0, 16*time.Millisecond, 419e6/8)
	if q.rate != 487e6/8 || q.path != 0 {
		t.Fatalf("one tick: rate %.1f path %.1f, want 487 / 0", q.rate*8/1e6, q.path*8/1e6)
	}
	tick(q, 1, 10*time.Millisecond, 440e6/8) // no queue: resets the count
	tick(q, 2, 16*time.Millisecond, 419e6/8)
	if q.rate != 487e6/8 {
		t.Fatalf("tick after a clear one: %.1f, want 487", q.rate*8/1e6)
	}
	tick(q, 3, 18*time.Millisecond, 400e6/8)
	if want := q.floorOf(qBackFrac * 400e6 / 8); math.Abs(q.rate-want) > 1 || q.path != q.rate {
		t.Fatalf("second tick: rate %.1f path %.1f, want %.1f", q.rate*8/1e6, q.path*8/1e6, want*8/1e6)
	}
	q = &qCtl{rate: 487e6 / 8}
	tick(q, 0, 45*time.Millisecond, 419e6/8)
	if want := q.floorOf(qBackFrac * 419e6 / 8); math.Abs(q.rate-want) > 1 {
		t.Fatalf("big queue: %.1f, want %.1f at once", q.rate*8/1e6, want*8/1e6)
	}
}

// hl219: a queue under a cap that rose past the path (qUpFirst) after a clear
// tick is that cap's overshoot: cap and path go to what the clear tick
// delivered, not to what the queue tick did; a throttle is followed next tick.
func TestQueueCtlKeepOvershootNotThrottle(t *testing.T) {
	defer func(v bool) { queueCtlKeep = v }(queueCtlKeep)
	queueCtlKeep = true
	now := time.Unix(1e9, 0).Add(healthTick)
	q := &qCtl{rate: 548.4e6 / 8}
	q.step(qIn{now: now, delay: time.Millisecond, g: 477.3e6 / 8})
	q.step(qIn{now: now.Add(healthTick), delay: 44 * time.Millisecond, g: 408.2e6 / 8})
	if want := qBackFrac * 477.3e6 / 8; math.Abs(q.rate-want) > 1 || q.path != q.rate || q.hold != 0 {
		t.Fatalf("overshoot: rate %.1f path %.1f hold %d, want %.1f", q.rate*8/1e6, q.path*8/1e6, q.hold, want*8/1e6)
	}
	r := q.rate
	q.step(qIn{now: now.Add(2 * healthTick), delay: 90 * time.Millisecond, g: 236e6 / 8})
	if want := q.floorOf(math.Min(r*qDown, qBackFrac*236e6/8)); math.Abs(q.rate-want) > 1 {
		t.Fatalf("throttle next tick: %.1f, want %.1f", q.rate*8/1e6, want*8/1e6)
	}
}

// hl223: lab hl222 rS1 u1b replay. The first upload: onset at 476.8 (59 ms),
// then the cap no longer jumps to 498 (qUpFirst) but stops at qNearPath of
// what the path delivered; kept at that through idle; the next upload
// rises only qUpSlow near it; an overshoot goes back to qNearPath of the
// queued delivery, not 0.97 of the cap.
func TestQueueCtlKeepSeenPath(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	q := &qCtl{}
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	tick(66*time.Millisecond, 415)
	if r := tick(59*time.Millisecond, 476.8); math.Abs(r-0.95*476.8) > 0.1 {
		t.Fatalf("onset cap %.1f, want %.1f", r, 0.95*476.8)
	}
	if q.seen != 0 {
		t.Fatalf("hl224: seen %.1f learned from the onset tick", q.seen*8/1e6)
	}
	// hl226: the onset's rate is the guess once the grace ends: qUpFast toward qSeenFrac of it
	if r := tick(2*time.Millisecond, 434.2); math.Abs(r-0.95*476.8*qUpFast) > 0.1 || !q.seenGuess {
		t.Fatalf("first rise %.1f (guess %v), want %.1f (qUpFast to the onset guess)", r, q.seenGuess, 0.95*476.8*qUpFast)
	}
	// a probe past it: a queue at 477.7 delivered: back to qSeenFrac of it
	q.rate = mb(490)
	if r := tick(18*time.Millisecond, 477.7); math.Abs(r-qSeenFrac*477.7) > 0.1 {
		t.Fatalf("overshoot: %.1f, want %.1f", r, qSeenFrac*477.7)
	}
	// idle: kept at qNearPath of 477.7
	var r float64
	for i := 0; i < qIdleTicks+1; i++ {
		r = tick(0, 1.4)
	}
	if math.Abs(r-qSeenFrac*477.7) > 0.1 || !q.kept {
		t.Fatalf("kept %.1f (kept=%v), want %.1f", r, q.kept, qSeenFrac*477.7)
	}
	// a little more than seen is not growth (tS1: 477.6 over 476.1 went +10 %)
	q.hold = 0
	// hl226: the kept cap's first binding tick after idle is a burst: it probes at once
	r0 := q.rate * 8 / 1e6
	if r := tick(3*time.Millisecond, 470); math.Abs(r-r0*qBurstUp) > 0.1 {
		t.Fatalf("burst's first full tick: %.1f, want %.1f (qBurstUp)", r, r0*qBurstUp)
	}
	r0 = q.rate * 8 / 1e6
	if r := tick(3*time.Millisecond, 479); math.Abs(r-r0*qUpFast) > 0.1 {
		t.Fatalf("479 over seen 477.7 filling the cap: %.1f, want %.1f (qUpFast probe)", r, r0*qUpFast)
	}
	// path grew: the links deliver qSeenGrow over seen -> the old rises (>= 2 %)
	r0 = q.rate * 8 / 1e6
	if r := tick(3*time.Millisecond, 495); r < r0*qUpFast-0.1 {
		t.Fatalf("path grew: %.1f, want >= %.1f", r, r0*qUpFast)
	}
	// HS2_QUEUE_CTL_SEEN=0: the first rise is qUpFirst again
	qSeenOff = true
	defer func() { qSeenOff = false }()
	q, now = &qCtl{}, now.Add(time.Hour)
	tick(66*time.Millisecond, 415)
	tick(59*time.Millisecond, 476.8)
	if r := tick(2*time.Millisecond, 434.2); math.Abs(r-0.95*476.8*qUpFirst) > 0.1 {
		t.Fatalf("SEEN=0: first rise %.1f, want %.1f", r, 0.95*476.8*qUpFirst)
	}
}

// hl224 item 3: the queue ticks the first cap drains (grace) are BBR's ramp,
// not the path: no seen from them (hl223 d1a: a 215 ms onset at 460 kept the
// cap at 448 for 30 s).
func TestQueueCtlSeenNotFromOnset(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	q := &qCtl{}
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	tick(215*time.Millisecond, 420)
	tick(215*time.Millisecond, 460) // the cap: 0.95 x 460
	tick(120*time.Millisecond, 440) // draining (grace)
	if q.seen != 0 {
		t.Fatalf("seen %.1f from the onset/grace ticks", q.seen*8/1e6)
	}
	// later, a binding overshoot learns it
	tick(3*time.Millisecond, 437)
	r0 := q.rate * 8 / 1e6
	tick(20*time.Millisecond, 0.95*r0)
	if math.Abs(q.seen*8/1e6-0.95*r0) > 0.1 {
		t.Fatalf("seen %.1f after a binding queue tick, want %.1f", q.seen*8/1e6, 0.95*r0)
	}
}

// hl224 item 2: an idle kept cap is exactly min(cap, qSeenFrac x seen): no
// creep above the path kept (hl223 uS1: 481 kept, the next u8 onset 76 ms).
// hl224 item 1: the growth probe — after qProbeTicks clear binding ticks at
// qSeenFrac x seen the cap rises qUpFast until a queue; seen = the most it
// delivered. And a path that doubles (rate19 250 -> 500) regrows.
func TestQueueCtlSeenKeptAndProbe(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	q := &qCtl{}
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	set := func(capMb, seenMb float64) {
		q = &qCtl{rate: mb(capMb), seen: mb(seenMb), seenAt: now, path: seenMb, pathAt: now}
	}
	// kept: a cap that crept to 481 over seen 477.7
	set(481, 477.7)
	var r float64
	for i := 0; i < qIdleTicks+1; i++ {
		r = tick(0, 1.4)
	}
	if want := qSeenFrac * 477.7; math.Abs(r-want) > 0.1 || !q.kept {
		t.Fatalf("kept %.1f (kept=%v), want exactly %.1f", r, q.kept, want)
	}
	// ... and one under it stays where it is (min)
	set(450, 477.7)
	for i := 0; i < qIdleTicks+1; i++ {
		r = tick(0, 1.4)
	}
	if math.Abs(r-450) > 0.1 {
		t.Fatalf("kept %.1f, want 450 (min of cap and 0.975 x seen)", r)
	}
	// probe on a fixed path: 3 slow ticks, then qUpFast until a queue
	set(qSeenFrac*477.7, 477.7)
	r0 := q.rate * 8 / 1e6
	for i := 0; i < qProbeTicks; i++ {
		r0 *= qUpSlow
		if r = tick(3*time.Millisecond, 0.99*q.rate*8/1e6); math.Abs(r-r0) > 0.1 {
			t.Fatalf("probe wait %d: %.1f, want %.1f (qUpSlow)", i, r, r0)
		}
	}
	r0 *= qUpFast
	if r = tick(3*time.Millisecond, 0.99*q.rate*8/1e6); math.Abs(r-r0) > 0.1 {
		t.Fatalf("probe: %.1f, want %.1f (qUpFast)", r, r0)
	}
	pm := 481.0 // the probe delivers over seen, under qProbeGrow x it
	r0 *= qUpFast
	if r = tick(3*time.Millisecond, pm); math.Abs(r-r0) > 0.1 {
		t.Fatalf("probe 2: %.1f, want %.1f", r, r0)
	}
	if r = tick(18*time.Millisecond, 480); math.Abs(r-qSeenFrac*pm) > 0.1 || math.Abs(q.seen*8/1e6-pm) > 0.1 {
		t.Fatalf("probe hit a queue: cap %.1f seen %.1f, want %.1f / %.1f", r, q.seen*8/1e6, qSeenFrac*pm, pm)
	}
	if q.probe != 0 {
		t.Fatalf("probe count %d after a queue", q.probe)
	}
	// rate19: path 250 -> 500, the cap at 0.975 x 243 (single upload ~0.96 of it)
	set(qSeenFrac*243, 243)
	n := 0
	for ; n < 40 && q.rate*8/1e6 < 450; n++ {
		path := 500.0
		tick(3*time.Millisecond, math.Min(0.96*q.rate*8/1e6, path))
	}
	if n > 20 {
		t.Fatalf("250 -> 500: %d ticks to 450 (cap %.1f), want <= 20", n, q.rate*8/1e6)
	}
	t.Logf("250 -> 500: cap past 450 after %d ticks", n)
}

// hl225: one probe tick over qProbeGrow x seen is a fixed path's noise (lab
// hl224 wS2 d1b: +10 % to 514.6, 113 ms): qUpFast; two in a row: qUpFirst.
func TestQueueCtlProbeGrowTwoTicks(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	q := &qCtl{rate: mb(qSeenFrac * 462), seen: mb(462), seenAt: now, path: 462, pathAt: now}
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	for i := 0; i < qProbeTicks; i++ {
		tick(4*time.Millisecond, q.rate*8/1e6)
	}
	r0 := q.rate * 8 / 1e6
	if r := tick(4*time.Millisecond, 467.1); math.Abs(r-r0*qUpFast) > 0.1 {
		t.Fatalf("one tick over 1.01 x seen: %.1f, want %.1f (qUpFast)", r, r0*qUpFast)
	}
	r0 = q.rate * 8 / 1e6
	if r := tick(4*time.Millisecond, 471); math.Abs(r-r0*qUpFirst) > 0.1 {
		t.Fatalf("two ticks over 1.01 x seen: %.1f, want %.1f (qUpFirst)", r, r0*qUpFirst)
	}
}

// hl225: a probe that overshoots the same path waits twice as long before the
// next one; a new burst (links under half the cap) probes at qProbeTicks again.
func TestQueueCtlProbeBackoffFixedPath(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	now := time.Unix(1e9, 0)
	q := &qCtl{rate: mb(qSeenFrac * 478), seen: mb(478), seenAt: now, path: 478, pathAt: now}
	tick := func(d time.Duration, g float64) float64 {
		now = now.Add(healthTick)
		r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
		return r * 8 / 1e6
	}
	// a fixed 478 path: 8 downloads deliver min(cap, 470); a queue when the cap passes 482
	run := func(n int) (rises int) {
		for i := 0; i < n; i++ {
			r := q.rate * 8 / 1e6
			d := 4 * time.Millisecond
			if r > 482 {
				d = 40 * time.Millisecond
			}
			if tick(d, math.Min(r, 470)) > r*1.015 {
				rises++
			}
		}
		return
	}
	first := run(30)
	later := run(60)
	if q.probeWait < 8 {
		t.Fatalf("probe wait after repeated same-path overshoots: %d, want >= 8", q.probeWait)
	}
	if later > first*2 {
		t.Fatalf("probe rises: first 30 ticks %d, next 60 ticks %d: no back-off", first, later)
	}
	if q.seen*8/1e6 > 478*qProbeGrow {
		t.Fatalf("seen %.1f moved off a fixed path", q.seen*8/1e6)
	}
	tick(4*time.Millisecond, 100) // idle: a new burst
	if q.probeWait != 0 {
		t.Fatalf("probe wait %d after an idle tick, want 0", q.probeWait)
	}
}

// hl226 item 1: with no path or seen known, the onset's rate is the path's
// guess: the cap rises qUpFast to qSeenFrac of it, not qUpFirst (lab hl225
// u1a 476 -> 452.5 -> 497.7 -> 547.5, 96-128 ms). On a fixed 476 path the
// cap never goes 3 % over it and ends within 3 % under it; a ramp onset
// (BBR at 300 of a 476 path) still reaches the path in a few ticks.
func TestQueueCtlOnsetGuess(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	run := func(onset, path float64, n int) (q *qCtl, maxCap, maxQ float64, ticks int) {
		now := time.Unix(1e9, 0)
		q = &qCtl{}
		tick := func(d time.Duration, g float64) float64 {
			now = now.Add(healthTick)
			r, _ := q.step(qIn{now: now, delay: d, g: mb(g)})
			return r * 8 / 1e6
		}
		tick(66*time.Millisecond, onset)
		tick(59*time.Millisecond, onset)
		ticks = -1
		for i := 0; i < n; i++ {
			r := q.rate * 8 / 1e6
			d, g := 3*time.Millisecond, math.Min(0.99*r, path)
			if r > 1.01*path { // over the path: a queue that grows with the excess
				d = time.Duration(float64(15*time.Millisecond) * (r/path - 1) / 0.01)
				g = path
			}
			maxQ = math.Max(maxQ, float64(d/time.Millisecond))
			r = tick(d, g)
			maxCap = math.Max(maxCap, r)
			if ticks < 0 && r >= 0.95*path {
				ticks = i + 1
			}
		}
		return
	}
	q, maxCap, maxQ, _ := run(476, 476, 20)
	if maxCap > 1.03*476 || maxQ > 40 {
		t.Fatalf("fixed path: cap went to %.1f (queue %.0f ms), want <= %.1f / <= 40 ms", maxCap, maxQ, 1.03*476)
	}
	if r := q.rate * 8 / 1e6; r < 0.95*476 {
		t.Fatalf("fixed path: cap %.1f at the end, want >= %.1f", r, 0.95*476)
	}
	t.Logf("fixed 476: max cap %.1f, max queue %.0f ms, end %.1f", maxCap, maxQ, q.rate*8/1e6)
	_, _, _, n := run(430, 476, 20)
	if n < 0 || n > 8 {
		t.Fatalf("ramp onset 430 of 476: %d ticks to 0.95 x path, want <= 8", n)
	}
	_, _, _, n2 := run(300, 476, 30)
	if n2 < 0 || n2 > 14 {
		t.Fatalf("ramp onset 300 of 476: %d ticks to 0.95 x path, want <= 14", n2)
	}
	t.Logf("ramp 430 -> 476: %d ticks, 300 -> 476: %d ticks", n, n2)
}

// hl226 item 2: a new burst under a kept cap probes on its first full tick
// (qBurstUp when the links fill qNearFill of it); a single upload on a path
// that grew 250 -> 500 reaches qUpFirst within 4 ticks (rate19 u1hi: 12-16 s).
func TestQueueCtlBurstProbe(t *testing.T) {
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
	set := func(seen float64) {
		q = &qCtl{rate: mb(qSeenFrac * seen), seen: mb(seen), seenAt: now, path: mb(seen), pathAt: now, kept: true}
		tick(0, 1)
		tick(0, 1)
	}
	set(478)
	r0 := q.rate * 8 / 1e6
	if r := tick(2*time.Millisecond, 0.98*r0); math.Abs(r-r0*qBurstUp) > 0.1 {
		t.Fatalf("burst's first full tick: %.1f, want %.1f (qBurstUp)", r, r0*qBurstUp)
	}
	// d8 on a fixed path: the next tick queues -> back to qSeenFrac x seen, seen stays ~478
	if r := tick(20*time.Millisecond, 478.5); r > 478 {
		t.Fatalf("burst probe overshoot: cap %.1f, want back under the path", r)
	}
	// rate19 u1hi: kept at 0.975 x 243, the path now 500, one upload at 0.96 of the cap
	set(243)
	n := 0
	for ; n < 10 && q.path != 0; n++ {
		tick(3*time.Millisecond, math.Min(0.96*q.rate*8/1e6, 500))
	}
	if n > 4 {
		t.Fatalf("250 -> 500: %d ticks to the growth rule, want <= 4", n)
	}
	t.Logf("250 -> 500: growth after %d ticks, cap %.1f", n, q.rate*8/1e6)
}

// hl227: a kept cap at >= qNearPath x path (hl216's "first binding tick does
// not rise") still takes the burst probe on the burst's first full tick when
// seen is fresh; a probeHi tick inside the burst steps qBurstUp and leaves no
// probe wait (lab hl226 yRS2 u1hi: kept 232.4, growth 14 s after the burst).
func TestQueueCtlBurstProbeKeptAtPath(t *testing.T) {
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
	set := func(capF, seen float64) {
		q = &qCtl{rate: mb(capF * seen), seen: mb(seen), seenAt: now, path: mb(seen), pathAt: now, kept: true}
		tick(0, 1)
		tick(0, 1)
	}
	// kept at 0.99 x path (>= qNearPath): the first full tick rises qBurstUp
	set(0.99, 238)
	r0 := q.rate * 8 / 1e6
	if r := tick(2*time.Millisecond, 0.98*r0); math.Abs(r-r0*qBurstUp) > 0.1 {
		t.Fatalf("kept-at-path burst's first full tick: %.1f, want %.1f (qBurstUp)", r, r0*qBurstUp)
	}
	if q.probe < q.probeNeed() {
		t.Fatalf("probe %d after the burst probe, want >= %d", q.probe, q.probeNeed())
	}
	// a probeHi tick inside the burst (links 1.015 x seen): qBurstUp, no wait
	set(0.99, 238)
	q.probe = q.probeNeed()
	r0 = q.rate * 8 / 1e6
	if r := tick(2*time.Millisecond, 1.015*238); math.Abs(r-r0*qBurstUp) > 0.1 {
		t.Fatalf("probeHi tick in a burst: %.1f, want %.1f (qBurstUp)", r, r0*qBurstUp)
	}
	if q.probe < q.probeNeed() || q.burst != 0 {
		t.Fatalf("probeHi in a burst: probe %d burst %d, want probe >= %d, burst 0", q.probe, q.burst, q.probeNeed())
	}
	// 238 -> 480 single upload from a kept cap at 0.99 x path: growth within 4 ticks
	set(0.99, 238)
	n := 0
	for ; n < 10 && q.path != 0; n++ {
		tick(3*time.Millisecond, math.Min(0.97*q.rate*8/1e6+3, 480))
	}
	if n > 4 {
		t.Fatalf("238 -> 480 from a kept cap at the path: %d ticks to growth, want <= 4", n)
	}
	t.Logf("238 -> 480: growth after %d ticks, cap %.1f", n, q.rate*8/1e6)
}

// hl228: a burst probe from a kept cap under qSeenFrac x seen starts at it,
// the probe is not reset right after it, and a clear tick over qSeenGrow x
// seen in the window after it is growth at once (lab hl227 zRS3 d1hi: kept
// 224.3 under a 238.7 seen, burst probe 231.0, then a reset and creep:
// growth 14 s after the burst).
func TestQueueCtlPostBurstGrowth(t *testing.T) {
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
		q = &qCtl{rate: mb(224.3), seen: mb(238.7), seenAt: now, path: mb(238.7), pathAt: now, kept: true, fitAt: now, fitRate: mb(224.3)}
		tick(6*time.Millisecond, 13)
		tick(6*time.Millisecond, 13)
	}
	// path grew to 500: a single download fills 0.997 of the cap
	set()
	want := qSeenFrac * 238.7 * qBurstUp
	if r := tick(5*time.Millisecond, 224.0); math.Abs(r-want) > 0.2 {
		t.Fatalf("burst probe from a kept cap under 0.975 x seen: %.1f, want %.1f", r, want)
	}
	n := 0
	for ; n < 6 && q.path != 0; n++ {
		tick(5*time.Millisecond, math.Min(0.997*q.rate*8/1e6, 480))
	}
	if n > 2 {
		t.Fatalf("post-burst growth after %d ticks, want <= 2", n)
	}
	if r := q.rate * 8 / 1e6; r < 260 {
		t.Fatalf("cap %.1f after growth, want >= 260", r)
	}
	// a clear tick over qSeenGrow x seen right after the burst probe: growth at once
	set()
	tick(5*time.Millisecond, 224.0)
	r0 := q.rate * 8 / 1e6
	if r := tick(4*time.Millisecond, 1.025*238.7); q.path != 0 || math.Abs(r-r0*qUpFirst) > 0.2 {
		t.Fatalf("clear 1.025 x seen after a burst probe: cap %.1f path %.1f, want %.1f and path 0", r, q.path*8/1e6, r0*qUpFirst)
	}
	// fixed path: the links queue at the probe's cap; no growth, window closed
	set()
	tick(5*time.Millisecond, 224.0)
	tick(5*time.Millisecond, 238.0)
	tick(20*time.Millisecond, 239.0)
	if q.path == 0 || q.postBurst != 0 {
		t.Fatalf("fixed path after a burst probe: path %.1f postBurst %d, want path kept, window closed", q.path*8/1e6, q.postBurst)
	}
	if r := q.rate * 8 / 1e6; r > 238.7 {
		t.Fatalf("fixed path: cap %.1f after the probe's queue, want <= seen 238.7", r)
	}
}

// hl228: the edge's (upload) probe step stops at qSeenGrow x seen for a
// tick; the exit's does not. probing() covers the step's tick and the next.
func TestQueueCtlUpProbeStep(t *testing.T) {
	defer func(v, f bool) { queueCtlKeep, queueCtlFast = v, f }(queueCtlKeep, queueCtlFast)
	queueCtlKeep, queueCtlFast = true, true
	mb := func(x float64) float64 { return x * 1e6 / 8 }
	for _, up := range []bool{true, false} {
		now := time.Unix(1e9, 0)
		q := &qCtl{rate: mb(470), seen: mb(470), seenAt: now, path: mb(470), pathAt: now, up: up}
		q.probe = q.probeNeed() + 1
		in := qIn{now: now.Add(healthTick), delay: 3 * time.Millisecond, g: mb(0.96 * 470)}
		if !q.seenRise(in, mb(470)) {
			t.Fatalf("up=%v: no probe step", up)
		}
		r := q.rate * 8 / 1e6
		lim := qSeenGrow * 470
		if up && math.Abs(r-lim) > 0.05 {
			t.Fatalf("upload probe step: %.2f, want %.2f (qSeenGrow x seen)", r, lim)
		}
		if !up && r <= lim {
			t.Fatalf("exit probe step: %.2f, want past %.2f", r, lim)
		}
		if !q.probing(in.now) || !q.probing(in.now.Add(2*healthTick)) || q.probing(in.now.Add(3*healthTick)) {
			t.Fatalf("up=%v: probing window wrong", up)
		}
		if up {
			// at qSeenGrow x seen it steps on (a path that grew still shows)
			in.now = in.now.Add(healthTick)
			in.g = mb(0.96 * lim)
			q.seenRise(in, mb(470))
			if r2 := q.rate * 8 / 1e6; r2 <= lim+0.1 {
				t.Fatalf("upload probe from qSeenGrow x seen: %.2f, want past it", r2)
			}
		}
	}
}
