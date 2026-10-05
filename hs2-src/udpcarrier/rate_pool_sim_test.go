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
// apart), and the pool's fair share (Governor.Share) recomputed every 500 ms
// from what the carriers using their allowance sent. It shows what one
// carrier's simulator cannot: carriers reading each other's queue.

type poolSender struct {
	start    time.Duration // joins the pool then (a late carrier)
	appBps   float64       // 0: always has data; else offers this much
	fbPhase  float64       // ms: feedback phase within the 100 ms interval
	ownBps   float64       // > 0: its own path of this capacity, not the shared one (a pool over several IPs)
	stopAt   time.Duration // > 0: stops sending then
	bulkFrom time.Duration // > 0: offers appBps until then, then always has data
}

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
	}
	ss := make([]*st, len(senders))
	for i, s := range senders {
		ss[i] = &st{rc: newRateControl(), nextFb: float64(s.start/time.Millisecond) + 50 + s.fbPhase}
		ss[i].rc.fair = rules
	}
	busyUntil := 0.0
	var toPeer []pkt_
	var qs []float64
	drops := 0
	shareB, meanB, nextShare := 0.0, 0.0, 500.0
	for now := 0.0; now < endMs; now += step {
		for i, s := range senders {
			c := ss[i]
			if now < float64(s.start/time.Millisecond) || (s.stopAt > 0 && now >= float64(s.stopAt/time.Millisecond)) {
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
				toPeer = append(toPeer, pkt_{from: i, arrive: *busy + ow, bytes: pkt, sendMs: now})
			}
		}
		sort.SliceStable(toPeer, func(a, b int) bool { return toPeer[a].arrive < toPeer[b].arrive })
		for len(toPeer) > 0 && toPeer[0].arrive <= now {
			d := toPeer[0]
			toPeer = toPeer[1:]
			c := ss[d.from]
			if o := now + peerOffset - d.sendMs; !c.haveOWD || o < c.owdMin {
				c.owdMin, c.haveOWD = o, true
			}
			c.rx += uint64(d.bytes)
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
			for _, c := range ss {
				sent := c.rc.sent.Load()
				r := float64(sent-c.sentPrev) / 0.5
				c.sentPrev = sent
				if r >= govActiveRate {
					na++
					asum += r
				}
				if r >= govActiveRate && c.rc.pushing.Load() {
					n++
					sum += r
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
			} else {
				shareB = 0
			}
		}
		for i, s := range senders {
			c := ss[i]
			if now < float64(s.start/time.Millisecond) {
				continue
			}
			if now >= c.nextFb {
				c.nextFb += 100
				r := report{arrive: now + ow, rx: c.rx}
				if c.haveOWD {
					r.owd, r.haveOWD = uint32(int64(c.owdMin*8)), true
					c.haveOWD = false
				}
				c.toUs = append(c.toUs, r)
			}
			for len(c.toUs) > 0 && c.toUs[0].arrive <= now {
				r := c.toUs[0]
				c.toUs = c.toUs[1:]
				c.rc.setShare(shareB, meanB)
				c.rc.onFeedback(at(now), r.rx, 2*ow/1000, 0, 0, r.owd, r.haveOWD)
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
// costs it some queue — ~20 ms more at 30 ms RTT, up to ~50 ms at 160 ms —
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
// fast as before the pool rules.
func TestPoolSimLightCarrierLaterRamp(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	for _, ow := range []time.Duration{15 * ms, 60 * ms} {
		for _, app := range []float64{1.2e6, 2e6, 4e6} {
			s := []poolSender{{fbPhase: 0, stopAt: 20 * sec}, {start: sec, fbPhase: 37, stopAt: 20 * sec}, {start: sec, appBps: app, fbPhase: 71, bulkFrom: 22 * sec}}
			w := poolWin{22000, 27000}
			off := runPoolSimWin(false, 30e6, ow, 200*ms, s, 35*sec, 10*sec, w)
			on := runPoolSimWin(true, 30e6, ow, 200*ms, s, 35*sec, 10*sec, w)
			t.Logf("one way %v, light %.1f Mbit/s: its bulk got %.1f Mbit/s in its first 5 s (rules off %.1f)", ow, app/1e6, on.win[2], off.win[2])
			if on.win[2] < 0.9*off.win[2] {
				t.Errorf("one way %v, light %.1f Mbit/s: ramp %.1f against %.1f with the rules off", ow, app/1e6, on.win[2], off.win[2])
			}
		}
	}
}

// Pool carriers converge on the bottleneck quickly, also on a narrow path
// and on a long one: fairness over seconds 10-25 after the first carrier.
func TestPoolSimConvergence(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	for _, c := range []struct {
		cap      float64
		ow, buf  time.Duration
		early    float64 // Jain over seconds 10-25
		late     float64 // Jain over 30-60
		wantNote string
	}{
		{8e6, 10 * ms, 100 * ms, 0.9, 0.95, "8 Mbit/s"},
		{30e6, 10 * ms, 100 * ms, 0.9, 0.95, "30 Mbit/s"},
		{100e6, 80 * ms, 200 * ms, 0.45, 0.85, "100 Mbit/s, 80 ms one way"},
	} {
		var s []poolSender
		for i := 0; i < 4; i++ {
			s = append(s, poolSender{start: time.Duration(i) * sec, fbPhase: float64(i * 23 % 100)})
		}
		r := runPoolSimWin(true, c.cap, c.ow, c.buf, s, 60*sec, 30*sec, poolWin{10000, 25000})
		sum, s2 := 0.0, 0.0
		for _, v := range r.win {
			sum += v
			s2 += v * v
		}
		early := sum * sum / (float64(len(r.win)) * s2)
		t.Logf("%s: Jain %.2f (10-25 s), %.2f (30-60 s): %s", c.wantNote, early, r.jain, r)
		if early < c.early || r.jain < c.late || r.drops > 0 {
			t.Errorf("%s: Jain %.2f early, %.2f late, %d drops", c.wantNote, early, r.jain, r.drops)
		}
	}
}

// A carrier that ran fast (no bottleneck) and then meets a narrow one while
// its users' TCP fills it: its capacity follows what gets through, not a
// share of the old peak — before, it kept 0.4x of 900 Mbit/s as its
// capacity and paced nothing (seen with mixed versions behind a 30 Mbit/s
// tbf: ping 122/475 ms, 5% lost).
func TestRateSimBottleneckAppearsAfterFastPeriod(t *testing.T) {
	for _, ow := range []time.Duration{5 * time.Millisecond, 40 * time.Millisecond} {
		p := simPath{capBps: 900e6, capAfter: 30e6, changeAt: 15 * time.Second, appRateBps: 500e6, appAfterBps: 1.05 * 30e6, oneWay: ow, buffer: 120 * time.Millisecond}
		p.rulesOff = true
		off := runRateSim(p, 45*time.Second, 25*time.Second, 7)
		p.rulesOff = false
		on := runRateSim(p, 45*time.Second, 25*time.Second, 7)
		t.Logf("one way %v\n  rule off: %s\n  rule on:  %s", ow, off, on)
		if on.qMean > 20 || on.tailDrops > 0 {
			t.Errorf("one way %v: %s", ow, on)
		}
	}
}

// The shared probe clock keeps the monotonic reading, so a wall-clock step
// cannot hold the probes back; the next probe is always ahead, at most one
// period away.
func TestNextProbeMonotonic(t *testing.T) {
	r := newRateControl()
	r.fair = true
	now := time.Now()
	n := r.nextProbe(now)
	if !strings.Contains(n.String(), "m=") {
		t.Errorf("next probe %v carries no monotonic reading", n)
	}
	for _, at := range []time.Time{now, now.Add(3 * time.Second), now.Add(-17 * time.Hour), time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000, 0).Add(baseProbeEvery)} {
		d := r.nextProbe(at).Sub(at)
		if d <= 0 || d > baseProbeEvery {
			t.Errorf("at %v: next probe in %v", at, d)
		}
	}
	a, b := newRateControl(), newRateControl()
	a.fair, b.fair = true, true
	if !a.nextProbe(now).Equal(b.nextProbe(now.Add(time.Second))) && !a.nextProbe(now).Add(baseProbeEvery).Equal(b.nextProbe(now.Add(time.Second))) {
		t.Error("two carriers do not share the probe clock")
	}
}
