package engine

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The production flap of 2026-11-09 (hs2, the edge, HS2_QUEUE_CTL_UP): a queue
// of ~27 ms at ~79 Mbit/s set a cap; under it cake dropped what the links sent
// over the cap, each drop came back as a retransmit, the "loss" (10–26%) took
// the cap off 10–14 s later, the queue came back and with it the cap: 73 caps
// in 31 minutes, the users' ping up with them.

// qJitter is one tick's busy-link delays on a path with no queue of its own:
// srtt-min_rtt as production saw it (median ~1.7 ms, p75 ~3.5 ms), and now and
// then one link far off (a busy peer).
func qJitter(r *rand.Rand, n int, queue time.Duration) []time.Duration {
	d := make([]time.Duration, n)
	for i := range d {
		v := 1.7 * math.Exp(1.07*r.NormFloat64()) // lognormal: median 1.7, p75 ~3.5
		if r.Intn(20) == 0 {
			v += 20 + 20*r.Float64()
		}
		d[i] = queue + time.Duration(v*float64(time.Millisecond))
	}
	return d
}

// qFlapPlant is the production path: capacity c, the links' BBR offer over it
// (overshoot), a standing queue of bbrQ when nothing holds them under c, a
// small loss of its own; capped under c the shaper (cake) drops the excess.
type qFlapPlant struct {
	c, overshoot, pathLoss float64
	bbrQ                   time.Duration
}

// tick: the input the runner builds for the controller; countDrops false is a
// shaper whose drops cannot be read (the worst case: all its drops look like
// the path's).
func (p qFlapPlant) tick(r *rand.Rand, now time.Time, capRate float64, countDrops bool) (qIn, float64) {
	offered := p.overshoot * p.c
	var queue time.Duration
	wire := p.c
	var drops float64
	if capRate > 0 && capRate < p.c {
		wire = capRate
		drops = offered - capRate // cake drops what goes over the cap
		queue = 5 * time.Millisecond
	} else {
		queue = p.bbrQ
		drops = 0
	}
	segs := offered * healthTick.Seconds() / 1448
	pathRetrans := segs * p.pathLoss
	shaper := segs * drops / offered
	in := qIn{now: now, delay: qDelayOf(qJitter(r, 10, queue)), g: wire * (1 - p.pathLoss)}
	sd := uint64(0)
	if countDrops {
		sd = uint64(shaper)
	}
	in.loss = qLossOf(uint64(pathRetrans+shaper), uint64(segs), sd)
	return in, shaper / segs
}

type qFlapRun struct {
	caps, removals, capped, ticks int
	rawLossMax                    float64
	notes                         []string
}

func runFlap(p func(i int) qFlapPlant, ticks int, countDrops bool, seed int64) qFlapRun {
	r := rand.New(rand.NewSource(seed))
	q := &qCtl{}
	now := time.Unix(1e9, 0)
	var out qFlapRun
	capRate := 0.0
	for i := 0; i < ticks; i++ {
		now = now.Add(healthTick)
		in, raw := p(i).tick(r, now, capRate, countDrops)
		out.rawLossMax = math.Max(out.rawLossMax, raw)
		prev := capRate
		var note string
		capRate, note = q.step(in)
		if note != "" {
			out.notes = append(out.notes, note)
		}
		if prev == 0 && capRate > 0 {
			out.caps++
		}
		if prev > 0 && capRate == 0 {
			out.removals++
		}
		if capRate > 0 {
			out.capped++
		}
		out.ticks++
	}
	return out
}

const qFlapTicks = 31 * 30 // 31 minutes of 2 s ticks

var qProdPath = qFlapPlant{c: 79.1e6 / 8, overshoot: 1.25, pathLoss: 0.005, bbrQ: 27 * time.Millisecond}

// The replay: with the shaper's drops taken out of the loss, the cap set on
// the queue stays on — one cap for the 31 minutes, not 73.
func TestQueueCtlProdReplayHoldsCap(t *testing.T) {
	res := runFlap(func(int) qFlapPlant { return qProdPath }, qFlapTicks, true, 1)
	if res.rawLossMax < 0.10 {
		t.Fatalf("the plant does not reproduce production: the cap's own drops are %.1f%% at most, production saw 10–26%%", 100*res.rawLossMax)
	}
	if res.caps != 1 || res.removals != 0 {
		t.Fatalf("31 minutes: %d caps, %d removals; want the one cap kept (notes %q)", res.caps, res.removals, res.notes)
	}
	if res.capped < res.ticks*9/10 {
		t.Fatalf("capped %d of %d ticks", res.capped, res.ticks)
	}
}

// The worst case: the shaper's drops cannot be read, so the cap's own drops
// look like the path's loss. Hysteresis must still bound the flapping: a cap
// kept qMinCapTicks at least, then off for 1, 2, 4, 8, 10 minutes.
func TestQueueCtlProdReplayBoundedWithoutDrops(t *testing.T) {
	res := runFlap(func(int) qFlapPlant { return qProdPath }, qFlapTicks, false, 2)
	if res.caps > 6 {
		t.Fatalf("31 minutes: %d caps (production: 73); want at most 6 (notes %q)", res.caps, res.notes)
	}
	for _, n := range res.notes {
		if strings.Contains(n, "loses") && !strings.Contains(n, "removed for") {
			t.Fatalf("a loss removal with no hold-off: %q", n)
		}
	}
}

// The path's natural jitter, busy links and all, with now and then a burst
// that holds every link for a tick or two: never a cap.
func TestQueueCtlJitterNeverCaps(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	q := &qCtl{}
	now := time.Unix(1e9, 0)
	for i := 0; i < qFlapTicks; i++ {
		now = now.Add(healthTick)
		var extra time.Duration
		if i%37 == 0 || i%37 == 1 { // a two-tick burst every ~74 s
			extra = 25 * time.Millisecond
		}
		in := qIn{now: now, delay: qDelayOf(qJitter(r, 2+r.Intn(12), extra)), g: 10e6, loss: 0.003}
		if rate, note := q.step(in); rate != 0 {
			t.Fatalf("tick %d: capped at %.1f Mbit/s on jitter alone (%q)", i, mbitps(rate), note)
		}
	}
}

// One or two busy links far off must not read as the path's queue.
func TestQDelayOfLowerQuartile(t *testing.T) {
	ms := func(v ...int) []time.Duration {
		d := make([]time.Duration, len(v))
		for i, x := range v {
			d[i] = time.Duration(x) * time.Millisecond
		}
		return d
	}
	if d := qDelayOf(nil); d != -1 {
		t.Fatalf("no busy link: %v", d)
	}
	if d := qDelayOf(ms(30, 2, 40, 3)); d != 3*time.Millisecond {
		t.Fatalf("two of four links far off: %v, want 3ms", d)
	}
	if d := qDelayOf(ms(27, 29, 31, 26, 28)); d < 15*time.Millisecond {
		t.Fatalf("every link queued: %v", d)
	}
}

// The shaper's own drops are not the path's loss.
func TestQLossOfTakesOutShaperDrops(t *testing.T) {
	if l := qLossOf(260, 1000, 250); math.Abs(l-0.01) > 1e-9 {
		t.Fatalf("loss %.3f, want 0.010", l)
	}
	if l := qLossOf(10, 1000, 50); l != 0 {
		t.Fatalf("more drops than retransmits: %.3f", l)
	}
	if l := qLossOf(5, 0, 0); l != 0 {
		t.Fatalf("no segments: %.3f", l)
	}
}
