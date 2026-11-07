package engine

import (
	"math"
	"testing"
	"time"
)

// qPlant models the path for the controller: a bottleneck of capacity c
// bytes/s; uncapped (or capped at or above c) the links' BBR keep a standing
// queue of bbrQ there; capped under c the queue drains and the cap's own
// flow queueing holds ~5 ms here instead. ext is a queue others keep at the
// bottleneck whatever the tunnel does. demand caps what the users want.
type qPlant struct {
	c, demand float64
	bbrQ, ext time.Duration
}

func (p qPlant) tick(capRate float64) (delay time.Duration, g float64) {
	want := p.demand
	if capRate > 0 && capRate < want {
		want = capRate
	}
	g = math.Min(want, p.c)
	switch {
	case want >= p.c:
		delay = p.bbrQ
	case capRate > 0 && want >= capRate:
		delay = 5 * time.Millisecond
	}
	return delay + p.ext, g
}

type qRun struct {
	capped, n      int
	gSum, delaySum float64
	notes          []string
	lastCap, lastG float64
	lastDelay      time.Duration
}

func runQ(q *qCtl, p func(i int) qPlant, ticks int, from int) qRun {
	var r qRun
	now := time.Unix(1e9, 0)
	capRate := 0.0
	for i := 0; i < ticks; i++ {
		now = now.Add(healthTick)
		pl := p(i)
		d, g := pl.tick(capRate)
		var note string
		capRate, note = q.step(qIn{now: now, delay: d, g: g})
		if note != "" {
			r.notes = append(r.notes, note)
		}
		if i >= from {
			r.n++
			r.gSum += g / pl.c
			r.delaySum += d.Seconds() * 1000
			if capRate > 0 {
				r.capped++
			}
		}
		r.lastCap, r.lastG, r.lastDelay = capRate, g, d
	}
	return r
}

const qC = 12.5e6 // 100 Mbit/s

// The tunnel's own queue: capped, it drains, and the cap settles just under
// the path — the delay near the empty path's, at least 93% of the path used.
func TestQueueCtlDrainsOwnQueue(t *testing.T) {
	r := runQ(&qCtl{}, func(int) qPlant { return qPlant{c: qC, demand: math.Inf(1), bbrQ: 95 * time.Millisecond} }, 900, 60)
	util, delay := r.gSum/float64(r.n), r.delaySum/float64(r.n)
	if util < 0.93 || delay > 30 {
		t.Fatalf("own queue: %.1f%% of the path used, mean delay %.1f ms; want >= 93%% and <= 30 ms (notes %q)", 100*util, delay, r.notes)
	}
	if r.capped < r.n*9/10 {
		t.Fatalf("capped %d of %d ticks: it should hold the cap while the downloads run", r.capped, r.n)
	}
}

// Nothing to drain: under light load it never caps.
func TestQueueCtlLightLoadNeverCaps(t *testing.T) {
	r := runQ(&qCtl{}, func(int) qPlant { return qPlant{c: qC, demand: 0.3 * qC, bbrQ: 95 * time.Millisecond} }, 600, 0)
	if r.capped != 0 || len(r.notes) != 0 {
		t.Fatalf("light load capped %d ticks: %q", r.capped, r.notes)
	}
}

// Others' queue: capping does not drain it, so the controller lets go and
// stays off for a while — the tunnel keeps its share of the path.
func TestQueueCtlReleasesOthersQueue(t *testing.T) {
	r := runQ(&qCtl{}, func(int) qPlant {
		return qPlant{c: qC, demand: math.Inf(1), bbrQ: 95 * time.Millisecond, ext: 60 * time.Millisecond}
	}, 900, 0) // 30 minutes
	util := r.gSum / float64(r.n)
	if util < 0.95 || r.capped > r.n/10 {
		t.Fatalf("others' queue: capped %d of %d ticks, %.1f%% of the path used; want <= 10%% capped, >= 95%% used (notes %q)", r.capped, r.n, 100*util, r.notes)
	}
	found := false
	for _, n := range r.notes {
		if len(n) > 0 && contains(n, "not the tunnel's own") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no log line says the queue is not the tunnel's: %q", r.notes)
	}
}

// The path's rate halves: the cap follows it down; the delay stays low.
func TestQueueCtlFollowsCapacityDrop(t *testing.T) {
	r := runQ(&qCtl{}, func(i int) qPlant {
		c := qC
		if i >= 300 {
			c = qC / 2
		}
		return qPlant{c: c, demand: math.Inf(1), bbrQ: 95 * time.Millisecond}
	}, 900, 400)
	util, delay := r.gSum/float64(r.n), r.delaySum/float64(r.n)
	if util < 0.90 || delay > 35 {
		t.Fatalf("after the drop: %.1f%% used, mean delay %.1f ms (notes %q)", 100*util, delay, r.notes)
	}
}

// Demand stops: the cap goes, so the next burst of traffic is not held back.
func TestQueueCtlRemovesCapWhenIdle(t *testing.T) {
	r := runQ(&qCtl{}, func(i int) qPlant {
		d := math.Inf(1)
		if i >= 120 {
			d = 0.1 * qC
		}
		return qPlant{c: qC, demand: d, bbrQ: 95 * time.Millisecond}
	}, 200, 0)
	if r.lastCap != 0 {
		t.Fatalf("cap %.0f left after demand stopped (notes %q)", r.lastCap, r.notes)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// A lossy path: the links deliver 3% less than the cap lets onto the wire
// (retransmits). The cap must still settle at the path's rate in its own
// units — within 30 s, and on average no more than ~4% under it after.
func TestQueueCtlClimbsToPathUnderLoss(t *testing.T) {
	q := &qCtl{}
	now := time.Unix(1e9, 0)
	capRate, sum, n := 0.0, 0.0, 0
	for i := 0; i < 150; i++ { // five minutes
		now = now.Add(healthTick)
		wire, d := qC, 95*time.Millisecond // uncapped, or capped at or over the path
		if capRate > 0 && capRate < qC {
			wire, d = capRate, 5*time.Millisecond
		}
		capRate, _ = q.step(qIn{now: now, delay: d, g: 0.97 * wire})
		if i == 15 && capRate < 0.95*qC {
			t.Fatalf("30 s in, the cap is %.1f Mbit/s on a %.1f Mbit/s path", mbitps(capRate), mbitps(qC))
		}
		if i >= 15 {
			sum += math.Min(capRate, qC) / qC
			n++
		}
	}
	if u := sum / float64(n); u < 0.96 {
		t.Fatalf("after the first 30 s the cap held the path at %.1f%% of its rate on average, want >= 96%%", 100*u)
	}
}

// A path losing over qMaxLoss: no cap, whatever its queue — and a cap that
// was set comes off once the loss shows for qLossTicks.
func TestQueueCtlStepsAsideOnLossyPath(t *testing.T) {
	q := &qCtl{}
	now := time.Unix(1e9, 0)
	for i := 0; i < 20; i++ {
		now = now.Add(healthTick)
		if r, _ := q.step(qIn{now: now, delay: 95 * time.Millisecond, g: qC, loss: 0.03}); r != 0 {
			t.Fatalf("tick %d: capped a path losing 3%%", i)
		}
	}
	q = &qCtl{}
	capRate := 0.0
	for i := 0; i < 10; i++ { // a cap is set on a clean path
		now = now.Add(healthTick)
		d := 95 * time.Millisecond
		if capRate > 0 {
			d = 5 * time.Millisecond
		}
		capRate, _ = q.step(qIn{now: now, delay: d, g: qC * 0.9, loss: 0.001})
	}
	if capRate == 0 {
		t.Fatal("no cap on the clean path")
	}
	var note string
	for i := 0; i < qLossTicks; i++ { // then the path starts losing
		now = now.Add(healthTick)
		capRate, note = q.step(qIn{now: now, delay: 5 * time.Millisecond, g: capRate, loss: 0.04})
	}
	if capRate != 0 || !contains(note, "loses") {
		t.Fatalf("cap %.0f kept on a path losing 4%% (note %q)", capRate, note)
	}
}

// A download starts: the queue forms while BBR still ramps up, at half the
// path. The cap must reach the path within ~20 s — and after a quiet spell
// the next download's cap starts near the path's rate seen before.
func TestQueueCtlShortDownloadStartsFast(t *testing.T) {
	q := &qCtl{}
	now := time.Unix(1e9, 0)
	capRate := 0.0
	tick := func(g float64, d time.Duration) {
		now = now.Add(healthTick)
		capRate, _ = q.step(qIn{now: now, delay: d, g: g})
	}
	plant := func() {
		if capRate > 0 && capRate < qC {
			tick(capRate, 5*time.Millisecond)
		} else {
			tick(qC, 95*time.Millisecond)
		}
	}
	tick(0.5*qC, 95*time.Millisecond) // the ramp's queue, at half the path
	tick(0.5*qC, 95*time.Millisecond)
	if capRate == 0 || capRate > 0.6*qC {
		t.Fatalf("first cap %.1f Mbit/s, want ~0.475 of the path", mbitps(capRate))
	}
	for i := 0; i < 10; i++ {
		plant()
	}
	if capRate < 0.9*qC {
		t.Fatalf("20 s into the download the cap is %.1f Mbit/s of %.1f", mbitps(capRate), mbitps(qC))
	}
	for i := 0; i < 30; i++ { // overshoots settle it; then the download ends
		plant()
	}
	for i := 0; i < qIdleTicks+1; i++ {
		tick(0.05*qC, -1)
	}
	if capRate != 0 {
		t.Fatal("cap kept after the download ended")
	}
	tick(0.5*qC, 95*time.Millisecond) // the next download
	tick(0.5*qC, 95*time.Millisecond)
	if capRate < 0.9*qC {
		t.Fatalf("the next download's cap starts at %.1f Mbit/s; the path's %.1f was seen a minute ago", mbitps(capRate), mbitps(qC))
	}
}
