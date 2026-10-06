package udpcarrier

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// A pool simulator: several carriers' real rateControls share one FIFO
// bottleneck, each with its own stamped feedback (every 100 ms, phases
// apart: the round trip it measured, and the share a full buffer dropped),
// and the pool's fair share (Governor.Share) and its busy carriers' queue
// (Governor.BusyQueue) recomputed every 500 ms from what the carriers using
// their allowance sent and saw. It shows what one
// carrier's simulator cannot: carriers reading each other's queue.

type poolSender struct {
	start    time.Duration // joins the pool then (a late carrier)
	appBps   float64       // 0: always has data; else offers this much
	fbPhase  float64       // ms: feedback phase within the 100 ms interval
	ownBps   float64       // > 0: its own path of this capacity, not the shared one (a pool over several IPs)
	stopAt   time.Duration // > 0: stops sending then
	bulkFrom time.Duration // > 0: offers appBps until then, then always has data
	crossBps float64       // > 0: not a carrier — cross traffic at this fixed rate on the shared path, from start to stopAt
	crossQ   time.Duration // > 0: not a carrier — a window-limited flow that keeps the shared queue this deep (a TCP flow holding a standing queue), from start to stopAt
}

func (s poolSender) cross() bool { return s.crossBps > 0 || s.crossQ > 0 }

type poolResult struct {
	util     float64   // pool delivery / capacity after warm-up
	qMean    float64   // ms
	qP95     float64   // ms
	drops    int       // tail drops after warm-up
	mbit     []float64 // each carrier's delivery after warm-up, Mbit/s
	jain     float64   // Jain's fairness index over the saturating carriers on the shared path
	ownQ95   []float64 // p95 queue of each carrier on its own path (ownBps), ms
	startup  []bool    // each carrier still in startup at the end
	win      []float64 // each carrier's delivery in the window (poolWin), Mbit/s
	rateMbit []float64 // each carrier's final pacing rate
}

func (r poolResult) String() string {
	s := ""
	for i, m := range r.mbit {
		s += fmt.Sprintf(" c%d=%.1f", i, m)
	}
	for _, q := range r.ownQ95 {
		s += fmt.Sprintf(" own-path-q95=%.1fms", q)
	}
	return fmt.Sprintf("util=%.0f%% queue mean=%.1fms p95=%.1fms drops=%d jain=%.2f |%s", r.util*100, r.qMean, r.qP95, r.drops, r.jain, s)
}

// poolTrace, when set, sees each carrier's controller after every report.
var poolTrace func(i int, nowMs float64, rc *rateControl)

// poolProbePhase shifts the pool's shared probe clock against the run's
// start (tests that sweep it run serially).
var poolProbePhase time.Duration

// poolWin, when set, is a window (ms) over which runPoolSim also measures
// each carrier's delivery (poolResult.win).
type poolWin [2]float64

func runPoolSim(capBps float64, oneWay, buffer time.Duration, senders []poolSender, dur, warm time.Duration) poolResult {
	return runPoolSimRules(true, capBps, oneWay, buffer, senders, dur, warm)
}

// runPoolSimRules is runPoolSim with the pool rules on or off.
func runPoolSimRules(rules bool, capBps float64, oneWay, buffer time.Duration, senders []poolSender, dur, warm time.Duration) poolResult {
	return runPoolSimWin(rules, capBps, oneWay, buffer, senders, dur, warm, poolWin{})
}

func runPoolSimWin(rules bool, capBps float64, oneWay, buffer time.Duration, senders []poolSender, dur, warm time.Duration, win poolWin) poolResult {
	const pkt = 1200.0
	const step = 0.25 // ms
	peerOffset := 123456.789
	base := time.Unix(1_700_000_000, 0)
	at := func(ms float64) time.Time { return base.Add(time.Duration(ms * float64(time.Millisecond))) }
	ow := float64(oneWay / time.Millisecond)
	capB := capBps / 8 / 1000
	endMs, warmMs := float64(dur/time.Millisecond), float64(warm/time.Millisecond)

	type pkt_ struct {
		from   int
		arrive float64
		bytes  float64
		sendMs float64
	}
	type report struct {
		arrive  float64
		rtt     float64 // seconds
		loss    uint32  // ppm of the data sent since the last report that a full buffer dropped
		echo    int64
		rx      uint64
		owd     uint32
		haveOWD bool
	}
	type st struct {
		rc        *rateControl
		busyUntil float64 // its own path's (ownBps)
		qs        []float64
		tokens    float64
		app       float64
		rx        uint64
		owdMin    float64
		haveOWD   bool
		nextFb    float64
		toUs      []report
		delivered float64
		inWin     float64
		sentPrev  uint64
		lost, got float64 // bytes dropped at a full buffer / delivered, since the last report
		qSum      float64 // queue estimates since the governor's last tick (Governor.report)
		qN        int
		pushedT   bool // used its allowance in some report since the last tick
	}
	ss := make([]*st, len(senders))
	for i, s := range senders {
		ss[i] = &st{rc: newRateControl(), nextFb: float64(s.start/time.Millisecond) + 50 + s.fbPhase}
		ss[i].rc.fair = rules
		ss[i].rc.epoch = base.Add(poolProbePhase)
	}
	busyUntil := 0.0
	var toPeer []pkt_
	unsorted := false // toPeer got an arrival earlier than its last one (an own path)
	var qs []float64
	drops := 0
	shareB, meanB, busyQ, nextShare := 0.0, 0.0, 0.0, 500.0
	idleBusy := 0
	for now := 0.0; now < endMs; now += step {
		for i, s := range senders {
			c := ss[i]
			if now < float64(s.start/time.Millisecond) || (s.stopAt > 0 && now >= float64(s.stopAt/time.Millisecond)) {
				continue
			}
			if s.crossQ > 0 {
				for busyUntil-now < float64(s.crossQ/time.Millisecond) {
					busyUntil = math.Max(now, busyUntil) + pkt/capB
				}
				continue
			}
			if s.crossBps > 0 {
				for c.app += s.crossBps / 8 / 1000 * step; c.app >= pkt; c.app -= pkt {
					if st := math.Max(now, busyUntil); st-now <= float64(buffer/time.Millisecond) {
						busyUntil = st + pkt/capB
					}
				}
				continue
			}
			rate := c.rc.pacingRate(at(now)) / 1000
			c.tokens = math.Min(c.tokens+rate*step, math.Max(2*pkt, rate*2))
			limitedApp := s.appBps > 0 && (s.bulkFrom == 0 || now < float64(s.bulkFrom/time.Millisecond))
			if limitedApp {
				c.app = math.Min(c.app+s.appBps/8/1000*step, 64*pkt)
			}
			for c.tokens >= pkt && (!limitedApp || c.app >= pkt) {
				c.tokens -= pkt
				c.app -= pkt
				c.rc.onSent(int(pkt))
				busy, cb := &busyUntil, capB
				if s.ownBps > 0 {
					busy, cb = &c.busyUntil, s.ownBps/8/1000
				}
				start := math.Max(now, *busy)
				if start-now > float64(buffer/time.Millisecond) {
					if now >= warmMs {
						drops++
					}
					c.lost += pkt
					continue
				}
				*busy = start + pkt/cb
				if now >= warmMs {
					if s.ownBps > 0 {
						c.qs = append(c.qs, start-now)
					} else {
						qs = append(qs, start-now)
					}
				}
				if n := len(toPeer); n > 0 && *busy+ow < toPeer[n-1].arrive {
					unsorted = true
				}
				toPeer = append(toPeer, pkt_{from: i, arrive: *busy + ow, bytes: pkt, sendMs: now})
			}
		}
		if unsorted {
			sort.SliceStable(toPeer, func(a, b int) bool { return toPeer[a].arrive < toPeer[b].arrive })
			unsorted = false
		}
		for len(toPeer) > 0 && toPeer[0].arrive <= now {
			d := toPeer[0]
			toPeer = toPeer[1:]
			c := ss[d.from]
			if o := now + peerOffset - d.sendMs; !c.haveOWD || o < c.owdMin {
				c.owdMin, c.haveOWD = o, true
			}
			c.rx += uint64(d.bytes)
			c.got += d.bytes
			if now >= warmMs {
				c.delivered += d.bytes
			}
			if win[1] > 0 && now >= win[0] && now < win[1] {
				c.inWin += d.bytes
			}
		}
		// The pool's governor: the fair share every 500 ms (Governor.tick).
		if now >= nextShare {
			nextShare += 500
			n, sum, na, asum := 0, 0.0, 0, 0.0
			var bq []float64
			for j, c := range ss {
				if senders[j].cross() {
					continue
				}
				sent := c.rc.sent.Load()
				r := float64(sent-c.sentPrev) / 0.5
				c.sentPrev = sent
				q, qn, pushed := c.qSum, c.qN, c.pushedT
				c.qSum, c.qN, c.pushedT = 0, 0, false
				if qn == 0 { // no report this interval: not counted (Governor.tick)
					continue
				}
				if r >= govActiveRate {
					na++
					asum += r
				}
				if r >= govActiveRate && pushed {
					n++
					sum += r
					bq = append(bq, q/float64(qn))
				}
			}
			if na > 0 {
				if meanB == 0 {
					meanB = asum / float64(na)
				}
				meanB += 0.5 * (asum/float64(na) - meanB)
			} else {
				meanB = 0
			}
			if n >= 2 {
				if shareB == 0 {
					shareB = sum / float64(n)
				}
				shareB += 0.5 * (sum/float64(n) - shareB)
				idleBusy = 0
			} else if idleBusy++; idleBusy > govShareHold {
				shareB = 0
			}
			if len(bq) > 0 {
				sort.Float64s(bq)
				busyQ = bq[len(bq)/2]
			} else if idleBusy > govShareHold {
				busyQ = 0
			}
		}
		for i, s := range senders {
			c := ss[i]
			if now < float64(s.start/time.Millisecond) {
				continue
			}
			if s.cross() {
				continue
			}
			if now >= c.nextFb {
				c.nextFb += 100
				// The round trip this report measures: the data's way here
				// (propagation and the queue it met) and the report's way back.
				busy := busyUntil
				if s.ownBps > 0 {
					busy = c.busyUntil
				}
				r := report{arrive: now + ow, rx: c.rx, rtt: (2*ow + math.Max(0, busy-now)) / 1000, echo: int64(now*1000)*64 + int64(i) + 1}
				if c.lost+c.got > 0 {
					r.loss = uint32(c.lost / (c.lost + c.got) * 1e6)
				}
				c.lost, c.got = 0, 0
				if c.haveOWD {
					r.owd, r.haveOWD = uint32(int64(c.owdMin*8)), true
					c.haveOWD = false
				}
				c.toUs = append(c.toUs, r)
			}
			for len(c.toUs) > 0 && c.toUs[0].arrive <= now {
				r := c.toUs[0]
				c.toUs = c.toUs[1:]
				c.rc.setShare(shareB, meanB, busyQ)
				c.rc.onFeedback(at(now), r.rx, r.rtt, r.loss, r.echo, r.owd, r.haveOWD)
				c.qSum += c.rc.queueSec()
				c.qN++
				c.pushedT = c.pushedT || c.rc.pushing.Load()
				if poolTrace != nil {
					poolTrace(i, now, c.rc)
				}
			}
		}
	}
	res := poolResult{drops: drops}
	tot := 0.0
	var sat []float64
	for i, c := range ss {
		m := c.delivered * 8 / (endMs - warmMs) / 1000
		res.mbit = append(res.mbit, m)
		res.rateMbit = append(res.rateMbit, c.rc.pacingRate(at(endMs))*8/1e6)
		res.startup = append(res.startup, c.rc.startup)
		if win[1] > 0 {
			res.win = append(res.win, c.inWin*8/(win[1]-win[0])/1000)
		}
		tot += c.delivered
		if senders[i].cross() {
			continue
		}
		if senders[i].ownBps > 0 {
			sort.Float64s(c.qs)
			q := 0.0
			if len(c.qs) > 0 {
				q = c.qs[int(0.95*float64(len(c.qs)-1))]
			}
			res.ownQ95 = append(res.ownQ95, q)
			continue
		}
		if senders[i].appBps == 0 && senders[i].stopAt == 0 {
			sat = append(sat, m)
		}
	}
	own := 0.0
	for _, s := range senders {
		own += s.ownBps
	}
	res.util = tot / ((capB + own/8/1000) * (endMs - warmMs))
	if len(qs) > 0 {
		sort.Float64s(qs)
		s := 0.0
		for _, q := range qs {
			s += q
		}
		res.qMean = s / float64(len(qs))
		res.qP95 = qs[int(0.95*float64(len(qs)-1))]
	}
	if len(sat) > 0 {
		s, s2 := 0.0, 0.0
		for _, x := range sat {
			s += x
			s2 += x * x
		}
		res.jain = s * s / (float64(len(sat)) * s2)
	}
	return res
}

// Carriers of one pool that join at different times end up sharing the
// bottleneck evenly, with the queue near its target. With the pool rules off
// the first carrier keeps most of it (logged for comparison).
func TestPoolSimLateCarriersShare(t *testing.T) {
	ms := time.Millisecond
	late := func(phases ...float64) []poolSender {
		var s []poolSender
		for i, ph := range phases {
			s = append(s, poolSender{start: time.Duration(i) * 2 * time.Second, fbPhase: ph})
		}
		return s
	}
	cases := []struct {
		name   string
		cap    float64
		ow     time.Duration
		buffer time.Duration
		s      []poolSender
	}{
		{"4 carriers 30M 10ms", 30e6, 10 * ms, 100 * ms, late(0, 23, 51, 77)},
		{"4 carriers 30M 60ms", 30e6, 60 * ms, 200 * ms, late(0, 23, 51, 77)},
		{"8 carriers 100M 20ms", 100e6, 20 * ms, 100 * ms, late(0, 11, 23, 37, 51, 63, 77, 89)},
		{"2 carriers 10M 40ms", 10e6, 40 * ms, 150 * ms, late(0, 50)},
	}
	for _, c := range cases {
		off := runPoolSimRules(false, c.cap, c.ow, c.buffer, c.s, 60*time.Second, 30*time.Second)
		on := runPoolSim(c.cap, c.ow, c.buffer, c.s, 60*time.Second, 30*time.Second)
		t.Logf("%s\n  rules off: %s\n  rules on:  %s", c.name, off, on)
		if on.jain < 0.9 {
			t.Errorf("%s: carriers do not share (Jain %.2f): %s", c.name, on.jain, on)
		}
		if on.util < 0.85 {
			t.Errorf("%s: pool uses %.0f%% of the path", c.name, on.util*100)
		}
		if on.qMean > 25 || on.drops > 0 {
			t.Errorf("%s: queue mean %.1f ms, %d drops", c.name, on.qMean, on.drops)
		}
	}
}

// A lightly loaded carrier keeps what it needs while the busy ones split the
// rest (max-min): the fair share is the busy carriers' mean, not the pool's.
func TestPoolSimLightCarrierKeepsItsShare(t *testing.T) {
	s := []poolSender{{fbPhase: 0}, {start: 2 * time.Second, fbPhase: 31}, {start: time.Second, appBps: 2e6, fbPhase: 67}}
	r := runPoolSim(30e6, 15*time.Millisecond, 100*time.Millisecond, s, 60*time.Second, 30*time.Second)
	t.Log(r)
	if r.mbit[2] < 1.8 {
		t.Errorf("the 2 Mbit/s carrier got %.1f Mbit/s", r.mbit[2])
	}
	if r.jain < 0.9 {
		t.Errorf("the busy carriers do not share (Jain %.2f): %.1f and %.1f Mbit/s", r.jain, r.mbit[0], r.mbit[1])
	}
	if r.qMean > 25 || r.drops > 0 {
		t.Errorf("queue mean %.1f ms, %d drops", r.qMean, r.drops)
	}
}

// A source offering more than the path carries (users' traffic above the
// bottleneck, but below the startup allowance) used to keep the carrier in
// startup for good — never "limited", so never paced: the queue sat at the
// bottleneck's buffer limit with tail drops (seen in the lab as ping 60–170
// ms). Startup now ends on the standing queue.
func TestRateSimOfferAboveCapacityLeavesStartup(t *testing.T) {
	for _, app := range []float64{1.02, 1.1, 1.5, 2.0} {
		for _, ow := range []time.Duration{5 * time.Millisecond, 40 * time.Millisecond} {
			p := simPath{capBps: 30e6, oneWay: ow, buffer: 120 * time.Millisecond, appRateBps: app * 30e6}
			p.rulesOff = true
			off := runRateSim(p, 40*time.Second, 10*time.Second, 7)
			p.rulesOff = false
			on := runRateSim(p, 40*time.Second, 10*time.Second, 7)
			t.Logf("offer %.2fx, one way %v\n  rule off: %s\n  rule on:  %s", app, ow, off, on)
			if on.qMean > 15 || on.tailDrops > 0 || on.util < 0.9 {
				t.Errorf("offer %.2fx one way %v: %s", app, ow, on)
			}
		}
	}
	// Leaving startup, capacity is what got through, not the allowance that
	// ran ahead of it: from the first second, no burst of queue.
	for _, ow := range []time.Duration{5 * time.Millisecond, 40 * time.Millisecond} {
		r := runRateSim(simPath{capBps: 30e6, oneWay: ow, buffer: 120 * time.Millisecond, appRateBps: 1.05 * 30e6}, 12*time.Second, 0, 7)
		if r.qP95 > 35 || r.tailDrops > 0 {
			t.Errorf("one way %v, whole run from the start: %s", ow, r)
		}
	}
}

// A carrier on its own, slower path (a pool over several IPs) gets what that
// path carries, and the fair share of the faster one does not push it into
// its buffer: its queue stays near the target, not at the share's demand.
func TestPoolSimSlowSeparatePath(t *testing.T) {
	s := []poolSender{{fbPhase: 0}, {start: time.Second, fbPhase: 37}, {start: 2 * time.Second, fbPhase: 71, ownBps: 2e6}}
	r := runPoolSim(30e6, 15*time.Millisecond, 200*time.Millisecond, s, 60*time.Second, 30*time.Second)
	t.Log(r)
	if r.mbit[2] < 1.7 {
		t.Errorf("the slow path carries %.1f of its 2 Mbit/s", r.mbit[2])
	}
	if r.ownQ95[0] > 40 || r.drops > 0 {
		t.Errorf("the slow path's queue p95 %.1f ms, %d drops", r.ownQ95[0], r.drops)
	}
	if r.jain < 0.9 {
		t.Errorf("the shared path's carriers do not share (Jain %.2f): %.1f and %.1f", r.jain, r.mbit[0], r.mbit[1])
	}
}

// A carrier with only light traffic (TCP acknowledgements for the other
// direction, keepalives) does not take the busy carriers' queue as its cue
// to leave startup: it keeps startup's fast ramp for when its own demand
// comes.
func TestPoolSimLightCarrierStaysInStartup(t *testing.T) {
	s := []poolSender{{fbPhase: 0}, {start: time.Second, fbPhase: 41}, {start: time.Second, appBps: 0.5e6, fbPhase: 83}}
	r := runPoolSim(30e6, 15*time.Millisecond, 100*time.Millisecond, s, 30*time.Second, 10*time.Second)
	t.Log(r)
	if !r.startup[2] {
		t.Error("the 0.5 Mbit/s carrier left startup on the busy carriers' queue")
	}
	if r.startup[0] || r.startup[1] {
		t.Error("a busy carrier is still in startup")
	}
}

// A carrier on its own much slower path, in a pool whose fair share is far
// above it (a pool over several IPs): the share must not drive it into its
// buffer. It cannot tell its queue from the pool's, so its fair-share growth
// costs it some queue — ~20 ms more, at 30 ms RTT and at 160 ms alike —
// but never drops or lost throughput (before the step was capped at its own
// capacity and kept out of queue-free reports, 100-200 ms).
func TestPoolSimSlowPathBigShare(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	for _, own := range []float64{0.5e6, 1e6, 3e6} {
		for _, ow := range []time.Duration{15 * ms, 80 * ms} {
			s := []poolSender{{fbPhase: 0}, {start: sec, fbPhase: 13}, {start: 2 * sec, fbPhase: 29}, {start: 3 * sec, fbPhase: 47}, {start: 4 * sec, fbPhase: 71, ownBps: own}}
			off := runPoolSimRules(false, 100e6, ow, 200*ms, s, 60*sec, 30*sec)
			on := runPoolSimRules(true, 100e6, ow, 200*ms, s, 60*sec, 30*sec)
			t.Logf("own %.1f Mbit/s, one way %v: queue p95 %.0f ms (rules off %.0f), got %.2f", own/1e6, ow, on.ownQ95[0], off.ownQ95[0], on.mbit[4])
			if on.ownQ95[0] > off.ownQ95[0]+55 || on.ownQ95[0] > 80 || on.drops > 0 || on.mbit[4] < 0.9*own/1e6 {
				t.Errorf("own %.1f Mbit/s, one way %v: queue p95 %.0f ms against %.0f with the rules off, %d drops, %.2f Mbit/s", own/1e6, ow, on.ownQ95[0], off.ownQ95[0], on.drops, on.mbit[4])
			}
		}
	}
}

// A light carrier (a call, the other direction's ACKs) next to busy ones
// keeps startup, so when its own bulk comes on an emptied path it ramps as
// fast as before the pool rules (not under 0.7x: what failed here was 10x
// slower) — over eight phases of the shared probe clock; one slow phase per
// case is allowed: a light carrier that happens to use its whole allowance
// when the others' startup queue appears leaves startup by the old rule,
// and then ramps at the ordinary probe's pace, with the rules off as well. The long path with a deep buffer is the case where
// the busy carriers' own startup queue stands past overflowQueue for
// seconds: a carrier that delivers what it sends there is not on a path of
// its own (deepShortfall; without it the bulk ramped at ~2 Mbit/s), and at
// 400 ms round trip, where that queue outlasts any wait and the light
// carrier falls short as well, the busy carriers see the same queue
// (Governor.BusyQueue; without it ~1.6 Mbit/s).
func TestPoolSimLightCarrierLaterRamp(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	defer func() { poolProbePhase = 0 }()
	for _, p := range []struct{ ow, buf time.Duration }{{15 * ms, 300 * ms}, {60 * ms, 300 * ms}, {120 * ms, 300 * ms}, {150 * ms, 600 * ms}, {200 * ms, 1000 * ms}} {
		ow := p.ow
		for _, app := range []float64{1.2e6, 2e6, 4e6} {
			s := []poolSender{{fbPhase: 0, stopAt: 20 * sec}, {start: sec, fbPhase: 37, stopAt: 20 * sec}, {start: sec, appBps: app, fbPhase: 71, bulkFrom: 22 * sec}}
			w := poolWin{22000, 27000}
			off := runPoolSimWin(false, 30e6, ow, p.buf, s, 35*sec, 10*sec, w)
			slow := 0
			got := ""
			for ph := 0; ph < 4000; ph += 500 {
				poolProbePhase = time.Duration(ph) * ms
				on := runPoolSimWin(true, 30e6, ow, p.buf, s, 35*sec, 10*sec, w)
				got += fmt.Sprintf(" %.1f", on.win[2])
				if on.win[2] < 0.7*off.win[2] {
					slow++
				}
			}
			t.Logf("one way %v, buffer %v, light %.1f Mbit/s: its bulk's first 5 s, Mbit/s by probe phase:%s (rules off %.1f)", ow, p.buf, app/1e6, got, off.win[2])
			if slow > 1 {
				t.Errorf("one way %v, buffer %v, light %.1f Mbit/s: %d of 8 phases ramp slower than with the rules off (%.1f):%s", ow, p.buf, app/1e6, slow, off.win[2], got)
			}
		}
	}
}

// Pool carriers converge on the bottleneck quickly, also on a narrow path;
// on a long one (the controller's round-trip scaling slows every step)
// more slowly, but well ahead of the rules off (when the fair-share step
// shrank with the round trip as well, 0.63 at 240 ms against 0.56 off).
// Fairness over seconds 10-25 after the first carrier, and over 30-60.
func TestPoolSimConvergence(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	jain := func(v []float64) float64 {
		s, s2 := 0.0, 0.0
		for _, x := range v {
			s += x
			s2 += x * x
		}
		return s * s / (float64(len(v)) * s2)
	}
	for _, c := range []struct {
		cap         float64
		ow, buf     time.Duration
		early, late float64 // floors; on a long path also: early no worse than the rules off, late 0.1 above them
		long        bool
		note        string
	}{
		{8e6, 10 * ms, 100 * ms, 0.9, 0.95, false, "8 Mbit/s"},
		{30e6, 10 * ms, 100 * ms, 0.9, 0.95, false, "30 Mbit/s"},
		{30e6, 60 * ms, 200 * ms, 0.8, 0.95, false, "30 Mbit/s, 60 ms one way"},
		{100e6, 80 * ms, 200 * ms, 0, 0.75, true, "100 Mbit/s, 80 ms one way"},
		{30e6, 120 * ms, 300 * ms, 0, 0.75, true, "30 Mbit/s, 120 ms one way"},
	} {
		var s []poolSender
		for i := 0; i < 4; i++ {
			s = append(s, poolSender{start: time.Duration(i) * sec, fbPhase: float64(i * 23 % 100)})
		}
		r := runPoolSimWin(true, c.cap, c.ow, c.buf, s, 60*sec, 30*sec, poolWin{10000, 25000})
		off := runPoolSimWin(false, c.cap, c.ow, c.buf, s, 60*sec, 30*sec, poolWin{10000, 25000})
		early, offEarly := jain(r.win), jain(off.win)
		t.Logf("%s: Jain %.2f (10-25 s), %.2f (30-60 s), queue p95 %.0f ms; rules off %.2f, %.2f, %.0f ms", c.note, early, r.jain, r.qP95, offEarly, off.jain, off.qP95)
		minEarly, minLate := c.early, c.late
		if c.long {
			minEarly, minLate = offEarly, math.Max(minLate, off.jain+0.1)
		}
		if early < minEarly || r.jain < minLate || r.drops > 0 || r.qP95 > off.qP95 {
			t.Errorf("%s: Jain %.2f early, %.2f late, queue p95 %.0f, %d drops (rules off: %.2f, %.2f, %.0f)", c.note, early, r.jain, r.qP95, r.drops, offEarly, off.jain, off.qP95)
		}
	}
}

// A carrier that ran fast (no bottleneck) and then meets a narrow one while
// its users' TCP fills it — measured from 2 s after the change: its
// capacity follows what gets through, not a share of the old peak — before, it kept 0.4x of 900 Mbit/s as its
// capacity and paced nothing (seen with mixed versions behind a 30 Mbit/s
// tbf: ping 122/475 ms, 5% lost).
func TestRateSimBottleneckAppearsAfterFastPeriod(t *testing.T) {
	for _, ow := range []time.Duration{5 * time.Millisecond, 40 * time.Millisecond} {
		p := simPath{capBps: 900e6, capAfter: 30e6, changeAt: 15 * time.Second, appRateBps: 500e6, appAfterBps: 1.05 * 30e6, oneWay: ow, buffer: 120 * time.Millisecond}
		p.rulesOff = true
		off := runRateSim(p, 45*time.Second, 17*time.Second, 7)
		p.rulesOff = false
		on := runRateSim(p, 45*time.Second, 17*time.Second, 7)
		t.Logf("one way %v\n  rule off: %s\n  rule on:  %s", ow, off, on)
		if on.qMean > 20 || on.tailDrops > 0 {
			t.Errorf("one way %v: %s", ow, on)
		}
	}
}

// The shared probe clock keeps the monotonic reading, so a wall-clock step
// cannot hold the probes back; the next probe is always 3/4 to 7/4 of a
// period ahead, on the shared ticks.
func TestNextProbeMonotonic(t *testing.T) {
	r := newRateControl()
	r.fair = true
	now := time.Now()
	n := r.nextProbe(now)
	if !strings.Contains(n.String(), "m=") {
		t.Errorf("next probe %v carries no monotonic reading", n)
	}
	for _, at := range []time.Time{now, now.Add(3 * time.Second), now.Add(-17 * time.Hour), time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000, 0).Add(baseProbeEvery)} {
		n := r.nextProbe(at)
		d := n.Sub(at)
		if d < baseProbeEvery*3/4 || d > baseProbeEvery*7/4 || n.Sub(r.epoch)%baseProbeEvery != 0 {
			t.Errorf("at %v: next probe in %v (%v past a tick)", at, d, n.Sub(r.epoch)%baseProbeEvery)
		}
	}
	a, b := newRateControl(), newRateControl()
	a.fair, b.fair = true, true
	if a.nextProbe(now).Sub(b.nextProbe(now.Add(time.Second)))%baseProbeEvery != 0 {
		t.Error("two carriers do not share the probe clock")
	}
}

// A carrier on its own slower path (a pool over several IPs) whose users
// offer more than that path carries, but less than its startup allowance:
// it is light next to the pool, yet the queue is its own — it must leave
// startup on it (a light carrier keeps startup only on a queue the size the
// others' pacing holds), not sit unpaced on a full buffer.
func TestPoolSimLightCarrierOwnPathLeavesStartup(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	for _, own := range []float64{3e6, 8e6} {
		for _, over := range []float64{1.1, 1.5} {
			s := []poolSender{{fbPhase: 0}, {start: sec, fbPhase: 37}, {start: 2 * sec, fbPhase: 71, ownBps: own, appBps: over * own}}
			r := runPoolSimRules(true, 100e6, 15*ms, 200*ms, s, 60*sec, 30*sec)
			t.Logf("own %.0f Mbit/s, offered %.1fx: in startup %v, its queue p95 %.0f ms, %d drops", own/1e6, over, r.startup[2], r.ownQ95[0], r.drops)
			if r.startup[2] || r.ownQ95[0] > 60 || r.drops > 0 {
				t.Errorf("own %.0f Mbit/s, offered %.1fx: in startup %v, its queue p95 %.0f ms, %d drops", own/1e6, over, r.startup[2], r.ownQ95[0], r.drops)
			}
		}
	}
}

// The same on a long path behind a deep buffer (1 s): the wait for its own
// deep queue counts base round trips, not the smoothed RTT — which holds the
// queue it is building, so the wait grew with it until the queue became the
// base delay and the carrier sat in startup on a full buffer for good
// (startupDeepMax alone would also have ended it). Its queue is far deeper
// than what the busy carriers see on the shared path, though theirs is past
// overflowQueue in their own startup: it waited 4.6 s for theirs to drain.
func TestPoolSimLightCarrierDeepOwnBufferLeavesStartup(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	defer func() { poolTrace = nil }()
	for _, own := range []float64{2e6, 4e6} {
		exit := -1.0
		poolTrace = func(i int, now float64, rc *rateControl) {
			if i == 2 && exit < 0 && !rc.startup {
				exit = now - 2000
			}
		}
		s := []poolSender{{fbPhase: 0}, {start: sec, fbPhase: 37}, {start: 2 * sec, fbPhase: 71, ownBps: own, appBps: 1.3 * own}}
		r := runPoolSimRules(true, 30e6, 150*ms, 1000*ms, s, 40*sec, 2*sec)
		t.Logf("own %.0f Mbit/s: left startup %.1f s after joining (-1: never), %d drops", own/1e6, exit/1000, r.drops)
		if exit < 0 || exit > 4000 {
			t.Errorf("own %.0f Mbit/s behind a 1 s buffer: left startup at %.1f s (-1: never)", own/1e6, exit/1000)
		}
	}
}

// A light carrier sharing a queue that a flow outside the pool holds (a TCP
// flow keeping the bottleneck's buffer 60 ms deep) delivers what it sends,
// so the queue is not its own: it keeps startup and its call keeps its rate
// (without deepShortfall it fell to ~0.7 of 1.2 Mbit/s). A queue held deeper
// still squeezes it, as it squeezes every carrier of the pool (a delay-based
// controller next to a loss-based flow; see CHANGELOG).
func TestPoolSimLightCarrierUnderCrossQueue(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	defer func() { poolProbePhase = 0 }()
	const app = 1.2e6
	s := []poolSender{{fbPhase: 0, stopAt: 20 * sec}, {start: sec, fbPhase: 37, stopAt: 20 * sec}, {start: sec, appBps: app, fbPhase: 71}, {start: 3 * sec, stopAt: 18 * sec, crossQ: 60 * ms}}
	got := ""
	low := 0.0
	for ph := 0; ph < 4000; ph += 1000 {
		poolProbePhase = time.Duration(ph) * ms
		r := runPoolSimWin(true, 30e6, 15*ms, 300*ms, s, 20*sec, 10*sec, poolWin{8000, 18000})
		got += fmt.Sprintf(" %.2f", r.win[2])
		if ph == 0 || r.win[2] < low {
			low = r.win[2]
		}
	}
	t.Logf("call over 8-18 s, Mbit/s by probe phase:%s", got)
	if low < 0.9*app/1e6 {
		t.Errorf("the call got%s Mbit/s of its %.1f", got, app/1e6)
	}
}

// Light is against the busy carriers' share when there is one: the pool's
// mean, which the light carriers themselves drag down, would call a
// 4 Mbit/s carrier next to two at 14 and six at 0.3 busy.
func TestRateControlLightAgainstShare(t *testing.T) {
	r := newRateControl()
	r.lastSendRate = 4e6 / 8
	r.setShare(14e6/8, (2*14e6+6*0.3e6)/8/8, 0)
	if !r.light() {
		t.Errorf("4 Mbit/s against a share of 14 is not light")
	}
	r.setShare(0, (2*14e6+6*0.3e6)/8/8, 0)
	if r.light() {
		t.Errorf("4 Mbit/s against a mean of 3.7 with no share is light")
	}
}

// Eight carriers (the icmp ceiling) joining half a second apart share the
// bottleneck evenly and hold the queue short, with the drops of the joining
// carriers' startup reported as loss, as the peer does. A narrower path
// further away (8 carriers on 16-24 Mbit/s at 80 ms) is not here: there the
// joining carriers take the queue they find for the base, its tail drops for
// random loss, and the pool stays on a full buffer — with the rules off
// alike (see CHANGELOG).
func TestPoolSimEightCarriers(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	for _, c := range []struct {
		cap float64
		ow  time.Duration
	}{{16e6, 20 * ms}, {30e6, 60 * ms}, {100e6, 40 * ms}} {
		var s []poolSender
		for i := 0; i < 8; i++ {
			s = append(s, poolSender{start: time.Duration(i) * 500 * ms, fbPhase: float64(i * 37 % 100)})
		}
		r := runPoolSimWin(true, c.cap, c.ow, 200*ms, s, 60*sec, 20*sec, poolWin{30000, 60000})
		s2, sq := 0.0, 0.0
		for _, x := range r.win {
			s2 += x
			sq += x * x
		}
		j := s2 * s2 / (float64(len(r.win)) * sq)
		t.Logf("%.0f Mbit/s, one way %v: Jain %.2f over 30-60 s, queue p95 %.0f ms, %d drops", c.cap/1e6, c.ow, j, r.qP95, r.drops)
		if j < 0.9 || r.qP95 > 30 || r.drops > 0 {
			t.Errorf("%.0f Mbit/s, one way %v: Jain %.2f, queue p95 %.0f ms, %d drops", c.cap/1e6, c.ow, j, r.qP95, r.drops)
		}
	}
}

// RTT samples: a path longer than 8x the starting srtt (any over 400 ms)
// is measured from its first sample, a jump that lasts is taken on its third
// sample, and a lone outlier (a clock step while a report was in flight) is
// dropped. Before, every sample over 400 ms was dropped, for good.
func TestRateControlRTTSamples(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	echo := int64(0)
	feed := func(r *rateControl, rtt float64) {
		now = now.Add(100 * time.Millisecond)
		echo++
		r.onFeedback(now, 0, rtt, 0, echo, 0, false)
	}
	r := newRateControl()
	feed(r, 0.6)
	if r.srtt != 0.6 || r.rtProp != 0.6 {
		t.Fatalf("a 600 ms path: srtt %.3f, rtProp %.3f", r.srtt, r.rtProp)
	}
	r = newRateControl()
	for i := 0; i < 20; i++ {
		feed(r, 0.1)
	}
	feed(r, 20)
	if r.srtt > 0.11 {
		t.Fatalf("one 20 s outlier moved srtt to %.3f", r.srtt)
	}
	for i := 0; i < 3; i++ {
		feed(r, 1.2)
	}
	if r.srtt < 0.3 {
		t.Fatalf("a lasting jump to 1.2 s left srtt at %.3f", r.srtt)
	}
}

// A light carrier whose deep queue the busy carriers do not see (so not the
// pool's) still keeps startup while it delivers what it sends: a flow on its
// own path holds that queue, not its own excess. Delivering short of it, the
// queue grows under it — its own — and it leaves.
func TestRateControlDeepQueueNeedsShortfall(t *testing.T) {
	for _, c := range []struct {
		deliver float64 // share of what it sends that arrives
		leaves  bool
	}{{1.0, false}, {0.8, true}} {
		r := newRateControl()
		r.fair = true
		r.setShare(20e6/8, 15e6/8, 0)
		now := time.Unix(1_700_000_000, 0)
		const sent = 25_000 // bytes per 100 ms report: 2 Mbit/s, under half the share
		rx := 0.0
		for i := 1; i <= 40; i++ {
			now = now.Add(feedbackEvery)
			r.onSent(sent)
			rx += c.deliver * sent
			owd := uint32(0) // ticks of stampTick: the queue in this report
			if i > 3 {
				owd = uint32(100 * time.Millisecond / stampTick)
			}
			r.onFeedback(now, uint64(rx), 0.05, 0, int64(i), owd, true)
		}
		if r.startup == c.leaves {
			t.Errorf("delivering %.0f%% of what it sends behind a 100 ms queue the busy carriers do not see: in startup %v, want %v", c.deliver*100, r.startup, !c.leaves)
		}
	}
}
