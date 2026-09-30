package udpcarrier

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"testing"
	"time"
)

// A deterministic path simulator for the rate controller: the real
// rateControl (onSent/onFeedback/pacingRate) drives a saturating sender into a
// modelled path — loss before the bottleneck (i.i.d. or time-based bursty), a
// FIFO bottleneck of fixed capacity with a tail-drop buffer, propagation delay
// each way — and receives the same feedback the carrier sends (every 100 ms,
// with the one-way delay of our last report measured on a peer clock that is
// offset from ours). It runs in simulated time, so a 60 s scenario takes
// milliseconds and every run is reproducible.

type simPath struct {
	capBps      float64       // bottleneck, bits/s
	oneWay      time.Duration // propagation, each way
	buffer      time.Duration // bottleneck buffer before tail drop
	lossIID     float64       // pre-bottleneck i.i.d. loss
	burstLoss   float64       // bursty: loss in the bad state
	burstGoodMs float64
	burstBadMs  float64
	appRateBps  float64 // 0 = saturating sender; else the app offers this much
	appUntilMs  float64 // if > 0, appRateBps applies only before this time; saturating after
	capAfter    float64 // if > 0, capacity changes to this at changeAt
	policeBps   float64 // if > 0, a per-flow token-bucket policer (DPI throttling) before the bottleneck
	policeBurst float64 // bytes
	crossFrac   float64 // if > 0, cross-traffic takes this fraction of the bottleneck while active
	crossOnMs   float64 // cross-traffic on/off period (ms); it builds a queue we did not build
	changeAt    time.Duration
}

type simResult struct {
	util      float64 // delivered / capacity, after warm-up
	qMean     float64 // ms, bottleneck queueing delay seen by data
	qP95      float64
	tailDrops int // after warm-up
	rateCV    float64
	finalRate float64 // Mbit/s
}

func (r simResult) String() string {
	return fmt.Sprintf("util=%.0f%% queue mean=%.1fms p95=%.1fms tail-drops=%d rate-cv=%.2f final=%.1fMbit",
		r.util*100, r.qMean, r.qP95, r.tailDrops, r.rateCV, r.finalRate)
}

var (
	simTrace      func(string)
	simTraceUntil = 30 * time.Second
)

type ge struct {
	bad   bool
	until float64 // ms
}

func runRateSim(p simPath, dur, warm time.Duration, seed uint64) simResult {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	rc := newRateControl()
	base := time.Unix(1_700_000_000, 0)
	at := func(ms float64) time.Time { return base.Add(time.Duration(ms * float64(time.Millisecond))) }
	const pkt = 1200.0
	const step = 0.25        // ms
	peerOffset := 123456.789 // ms: the peer's clock runs this far ahead of ours

	capB := p.capBps / 8 / 1000 // bytes per ms
	var g ge
	lost := func(now float64) bool {
		if p.burstBadMs > 0 {
			for now >= g.until {
				g.bad = !g.bad
				m := p.burstGoodMs
				if g.bad {
					m = p.burstBadMs
				}
				g.until += rng.ExpFloat64() * m
			}
			if g.bad {
				return rng.Float64() < p.burstLoss
			}
			return rng.Float64() < p.lossIID
		}
		return p.lossIID > 0 && rng.Float64() < p.lossIID
	}

	type inflight struct {
		arrive float64
		bytes  float64
		fb     bool    // our feedback frame (carries sendMs)
		sendMs float64 // our clock
		seq    uint64  // data wire sequence
	}
	// Bottleneck: departures computed from a busy-until clock.
	busyUntil := 0.0
	var toPeer []inflight // ordered by arrival (FIFO bottleneck keeps order)

	// Peer (receiver) state.
	var rxBytes, wireSeq, wireHigh, wireRecv uint64
	var lastHigh, lastRecv uint64
	owdMin, haveOWD := 0.0, false              // this interval's min stamped one-way delay, ms
	var peerLastFbSend, peerLastFbRecv float64 // ms, ours / peer clock
	havePeerFb := false
	// Reports in flight back to us (reverse path uncongested).
	type report struct {
		arrive                       float64
		rx                           uint64
		lossPPM                      uint32
		echo, echoDelay, owd, sentAt float64
		haveEcho                     bool
		owdTicks                     uint32
		haveOWD                      bool
	}
	var toUs []report

	tokens := 0.0
	appTokens := 0.0
	var qs []float64
	var rates []float64
	delivered := 0.0
	tailDrops := 0
	lastFb := 0.0
	nextPeerFb := 50.0
	endMs := float64(dur / time.Millisecond)
	warmMs := float64(warm / time.Millisecond)

	polTokens, polLast := p.policeBurst, 0.0
	policed := func(now, bytes float64) bool {
		if p.policeBps <= 0 {
			return false
		}
		polTokens = math.Min(p.policeBurst, polTokens+(now-polLast)*p.policeBps/8/1000)
		polLast = now
		if polTokens < bytes {
			return true
		}
		polTokens -= bytes
		return false
	}
	send := func(now float64, bytes float64, fb bool) {
		var seq uint64
		if !fb {
			wireSeq++
			seq = wireSeq
		}
		if lost(now) || policed(now, bytes) {
			return
		}
		start := math.Max(now, busyUntil)
		if start-now > float64(p.buffer/time.Millisecond) {
			if now >= warmMs && !fb {
				tailDrops++
			}
			return
		}
		busyUntil = start + bytes/capB
		if !fb && now >= warmMs {
			qs = append(qs, start-now)
		}
		toPeer = append(toPeer, inflight{arrive: busyUntil + float64(p.oneWay/time.Millisecond), bytes: bytes, fb: fb, sendMs: now, seq: seq})
	}

	for now := 0.0; now < endMs; now += step {
		if p.capAfter > 0 && now >= float64(p.changeAt/time.Millisecond) {
			capB = p.capAfter / 8 / 1000
		}
		if p.crossFrac > 0 && p.crossOnMs > 0 {
			base := p.capBps
			if p.capAfter > 0 && now >= float64(p.changeAt/time.Millisecond) {
				base = p.capAfter
			}
			// square wave: cross-traffic present for the first half of each period
			if int(now/p.crossOnMs)%2 == 0 {
				capB = base * (1 - p.crossFrac) / 8 / 1000
			} else {
				capB = base / 8 / 1000
			}
		}
		// Sender: pace data.
		rate := rc.pacingRate(at(now)) / 1000 // bytes per ms
		tokens = math.Min(tokens+rate*step, math.Max(2*pkt, rate*2))
		appLimited := p.appRateBps > 0 && (p.appUntilMs == 0 || now < p.appUntilMs)
		if appLimited {
			appTokens = math.Min(appTokens+p.appRateBps/8/1000*step, 64*pkt)
		}
		for tokens >= pkt && (!appLimited || appTokens >= pkt) {
			tokens -= pkt
			appTokens -= pkt
			rc.onSent(int(pkt))
			send(now, pkt, false)
		}
		// Sender: our feedback frame every 100 ms (raw, unpaced).
		if now-lastFb >= 100 {
			lastFb = now
			send(now, 60, true)
		}
		// Deliveries to the peer.
		for len(toPeer) > 0 && toPeer[0].arrive <= now {
			d := toPeer[0]
			toPeer = toPeer[1:]
			if d.fb {
				peerLastFbSend, peerLastFbRecv, havePeerFb = d.sendMs, now+peerOffset, true
				continue
			}
			wireRecv++
			if d.seq > wireHigh {
				wireHigh = d.seq
			}
			if o := now + peerOffset - d.sendMs; !haveOWD || o < owdMin {
				owdMin, haveOWD = o, true
			}
			rxBytes += uint64(d.bytes)
			if now >= warmMs {
				delivered += d.bytes
			}
		}
		// Peer: report every 100 ms over the reverse path.
		if now >= nextPeerFb {
			nextPeerFb += 100
			span := wireHigh - lastHigh
			recv := wireRecv - lastRecv
			lastHigh, lastRecv = wireHigh, wireRecv
			var ppm uint32
			if span > 0 && recv < span {
				ppm = uint32(float64(span-recv) / float64(span) * 1e6)
			}
			r := report{arrive: now + float64(p.oneWay/time.Millisecond), rx: rxBytes, lossPPM: ppm}
			if haveOWD {
				r.owdTicks, r.haveOWD = uint32(int64(owdMin*8)), true
				haveOWD = false
			}
			if havePeerFb {
				r.haveEcho = true
				r.echo = peerLastFbSend
				r.echoDelay = (now + peerOffset) - peerLastFbRecv
				r.owd = peerLastFbRecv - peerLastFbSend
			}
			if !lost(now) {
				toUs = append(toUs, r)
			}
		}
		for len(toUs) > 0 && toUs[0].arrive <= now {
			r := toUs[0]
			toUs = toUs[1:]
			var rtt float64
			var echo int64
			if r.haveEcho {
				rtt = (now - r.echo - r.echoDelay) / 1000
				echo = at(r.echo).UnixNano()
			}
			rc.onFeedback(at(now), r.rx, rtt, r.lossPPM, echo, r.owdTicks, r.haveOWD)
			if simTrace != nil && now < float64(simTraceUntil/time.Millisecond) {
				simTrace(fmt.Sprintf("t=%6.0fms rate=%7.2f bl=%4.2f fl=%4.2f qs=%6.1f busy=%6.1f st=%v cap=%.1f btl=%.1f loss=%d",
					now, rc.rate*8/1e6, rc.baseLoss, rc.fullLoss, rc.queue*1000, math.Max(0, busyUntil-now), rc.startup, rc.capEst*8/1e6, rc.btlBw*8/1e6, r.lossPPM))
			}
			if now >= warmMs {
				rates = append(rates, rc.pacingRate(at(now))*8/1e6)
			}
		}
	}
	res := simResult{tailDrops: tailDrops, finalRate: rc.pacingRate(at(endMs)) * 8 / 1e6}
	capWin := capB * (endMs - warmMs)
	if p.capAfter > 0 {
		capWin = p.capAfter / 8 / 1000 * (endMs - warmMs)
	}
	res.util = delivered / capWin
	if len(qs) > 0 {
		sort.Float64s(qs)
		s := 0.0
		for _, q := range qs {
			s += q
		}
		res.qMean = s / float64(len(qs))
		res.qP95 = qs[int(0.95*float64(len(qs)-1))]
	}
	if len(rates) > 1 {
		m, v := 0.0, 0.0
		for _, x := range rates {
			m += x
		}
		m /= float64(len(rates))
		for _, x := range rates {
			v += (x - m) * (x - m)
		}
		res.rateCV = math.Sqrt(v/float64(len(rates))) / m
	}
	return res
}

type simCase struct {
	name     string
	p        simPath
	minUtil  float64
	maxQp95  float64 // ms
	maxDrops int
}

func rateSimCases() []simCase {
	ms := time.Millisecond
	var cs []simCase
	for _, c := range []struct {
		mbit float64
		ow   time.Duration
	}{{2, 20 * ms}, {10, 40 * ms}, {20, 25 * ms}, {50, 60 * ms}, {100, 10 * ms}, {100, 150 * ms}, {500, 20 * ms}} {
		name := fmt.Sprintf("%gmbit-rtt%dms", c.mbit, 2*c.ow/ms)
		base := simPath{capBps: c.mbit * 1e6, oneWay: c.ow, buffer: 300 * ms}
		cs = append(cs, simCase{name + "-clean", base, 0.95, 30, 0})
		iid := base
		iid.lossIID = 0.05
		cs = append(cs, simCase{name + "-iid5", iid, 0.95, 45, 0})
		// ~26% bursty loss (the real target path's profile): arrivals at
		// the bottleneck swing with the bursts, so the queue random-walks
		// around the target; it must still stay bounded, far from the buffer.
		b := base
		b.lossIID, b.burstLoss, b.burstGoodMs, b.burstBadMs = 0.02, 0.88, 30, 12
		cs = append(cs, simCase{name + "-bursty26", b, 0.90, 150, 0})
	}
	// A shallow bottleneck buffer: 15 ms, clean and with 5% loss. Tail drops
	// here are the carrier's own doing; keep them rare.
	for _, lossP := range []float64{0, 0.05} {
		p := simPath{capBps: 30e6, oneWay: 30 * ms, buffer: 15 * ms, lossIID: lossP}
		cs = append(cs, simCase{fmt.Sprintf("30mbit-rtt60ms-buf15ms-loss%g", lossP), p, 0.9, 16, 1000})
	}
	return cs
}

// The controller fills the bottleneck while keeping its queue near the target
// band — at low and high bandwidth, short and long RTT, clean, 5% i.i.d. and
// 26% bursty loss — and never overflows a 300 ms buffer once it has settled.
func TestRateSimMatrix(t *testing.T) {
	for _, c := range rateSimCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := runRateSim(c.p, 60*time.Second, 15*time.Second, 1)
			t.Logf("%s: %s", c.name, r)
			if r.util < c.minUtil {
				t.Errorf("utilisation %.0f%% < %.0f%%", r.util*100, c.minUtil*100)
			}
			if r.qP95 > c.maxQp95 {
				t.Errorf("queue p95 %.1f ms > %.0f ms", r.qP95, c.maxQp95)
			}
			if r.tailDrops > c.maxDrops {
				t.Errorf("%d tail drops after settling", r.tailDrops)
			}
		})
	}
}

// When the path's capacity halves (or doubles) mid-run, the controller follows
// it: no standing queue after the drop, and it grows into the new capacity.
func TestRateSimCapacityChange(t *testing.T) {
	ms := time.Millisecond
	down := simPath{capBps: 40e6, oneWay: 30 * ms, buffer: 300 * ms, capAfter: 20e6, changeAt: 20 * time.Second}
	r := runRateSim(down, 60*time.Second, 25*time.Second, 2)
	t.Logf("40->20 Mbit: %s", r)
	if r.util < 0.95 || r.qP95 > 30 {
		t.Errorf("after a capacity drop: %s", r)
	}
	up := simPath{capBps: 10e6, oneWay: 30 * ms, buffer: 300 * ms, capAfter: 60e6, changeAt: 20 * time.Second}
	r = runRateSim(up, 60*time.Second, 35*time.Second, 3)
	t.Logf("10->60 Mbit: %s", r)
	if r.util < 0.95 || r.qP95 > 30 {
		t.Errorf("after a capacity rise: %s", r)
	}
}

// An application-limited sender (the tunnel is idle or light) must not let
// its allowance run away: when traffic later saturates, the first burst meets a
// rate near what the path was last shown to carry, not an inflated one.
func TestRateSimAppLimited(t *testing.T) {
	ms := time.Millisecond
	p := simPath{capBps: 50e6, oneWay: 20 * ms, buffer: 300 * ms, appRateBps: 5e6}
	r := runRateSim(p, 30*time.Second, 10*time.Second, 4)
	t.Logf("app-limited 5/50 Mbit: %s", r)
	if r.finalRate > 60 {
		t.Errorf("idle allowance grew to %.0f Mbit/s", r.finalRate)
	}
	if r.qP95 > 10 {
		t.Errorf("app-limited sender built a queue: %s", r)
	}
}

func TestRateSimTrace(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	simTrace = func(s string) { t.Log(s) }
	defer func() { simTrace = nil }()
	ms := time.Millisecond
	p := simPath{capBps: 20e6, oneWay: 25 * ms, buffer: 300 * ms}
	switch os.Getenv("SIMTRACE") {
	case "rtt300":
		p = simPath{capBps: 100e6, oneWay: 150 * ms, buffer: 300 * ms}
	case "police":
		p = simPath{capBps: 100e6, oneWay: 20 * ms, buffer: 300 * ms, policeBps: 5e6, policeBurst: 64 << 10}
	case "bursty":
		p = simPath{capBps: 20e6, oneWay: 25 * ms, buffer: 300 * ms, lossIID: 0.02, burstLoss: 0.88, burstGoodMs: 30, burstBadMs: 12}
	}
	runRateSim(p, 30*time.Second, 5*time.Second, 5)
}

// Regression tests for the rate controller's startup and steady-state guards
// (found by adversarial review of the 2026-09 rewrite).

// A very slow path (well under 4×minRate) with no delay signal must not let
// startup ramp the rate to maxRate: the round-count backstop ends startup.
func TestRateSimSlowPathNoRunaway(t *testing.T) {
	ms := time.Millisecond
	// 300 kbit/s, a big buffer, and NO queue signal modelled by giving the
	// path a huge buffer so overdriving barely raises delay within the run.
	r := runRateSim(simPath{capBps: 300e3, oneWay: 20 * ms, buffer: 5 * time.Second}, 40*time.Second, 20*time.Second, 6)
	t.Logf("slow 300kbit: %s", r)
	if r.finalRate > 5 { // Mbit/s — must stay near 0.3, never near maxRate
		t.Errorf("rate ran away on a slow path: %.1f Mbit/s", r.finalRate)
	}
	if r.util < 0.7 {
		t.Errorf("slow path under-utilised: %.0f%%", r.util*100)
	}
}

// An app-limited start (the tunnel opens carrying light traffic) must not
// leave the controller stuck at a tiny rate: when real traffic arrives it
// must ramp. Models 2 Mbit/s of app traffic for 15 s, then saturating.
func TestRateSimAppLimitedStartThenBurst(t *testing.T) {
	ms := time.Millisecond
	r := runRateSim(simPath{capBps: 50e6, oneWay: 20 * ms, buffer: 300 * ms, appRateBps: 2e6, appUntilMs: 15000}, 40*time.Second, 20*time.Second, 7)
	t.Logf("app-limited start then saturate: %s", r)
	if r.util < 0.9 {
		t.Errorf("did not ramp after the app-limited start: util %.0f%%", r.util*100)
	}
}

// Cross-traffic that periodically takes half the bottleneck (building a queue
// the carrier did not build) must not collapse the carrier's rate to the floor:
// when the cross-traffic ebbs, the carrier must use the freed capacity again.
func TestRateSimCrossTraffic(t *testing.T) {
	ms := time.Millisecond
	p := simPath{capBps: 50e6, oneWay: 20 * ms, buffer: 300 * ms, crossFrac: 0.5, crossOnMs: 3000}
	r := runRateSim(p, 60*time.Second, 20*time.Second, 8)
	t.Logf("cross-traffic 50%%/3s: %s", r)
	// Average available capacity is ~75% of 50 Mbit = 37.5 Mbit. The carrier
	// should deliver a healthy share of what is available, far above the floor,
	// and not have spiralled down (final rate well above minRate).
	if r.finalRate < 10 {
		t.Errorf("rate collapsed under cross-traffic: final %.1f Mbit/s", r.finalRate)
	}
	if r.util < 0.5 {
		t.Errorf("carrier used only %.0f%% of the bottleneck under cross-traffic", r.util*100)
	}
}
