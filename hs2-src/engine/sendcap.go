package engine

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// C4-lite, the pacing cap on every link socket at both ends (REDESIGN
// §3.4, R1): SO_MAX_PACING_RATE = 1.25 × the largest delivery rate of the
// last 2 s, never under 1 MiB/s, from the socket's first delivery-rate
// sample that is not app-limited, lifted when it goes idle. No in-flight
// budget, no probing, no drain (R3's C4 brings those).
//
// C2 and C3 bound only the host's queues; a byte that reached the link's
// TCP waits at the bottleneck behind all that TCP has in flight. A BBRv1
// sender held by windows never leaves STARTUP and paces at ~3.9 times the
// path (D3), filling the bottleneck's queue to its window: the probe on
// the same link waits there whatever C2 does. Capped at 1.25 times what the
// path delivered, that overshoot is cut at the source; it is the pacing
// net's cap (pacenet.go) made unconditional (not only past 2% resent) and
// symmetric (the edge too, for uploads).
//
// One writer for the cap: on the exit the window core and the pacing net
// stay on in R1; the net no longer sets the socket itself but hands its cap
// here (wcLink.netSet), and the lesser of the two is in force.
//
// "Idle" is the pacing net's quiet socket (pacenet.go): less than
// wcNetQuietBytes acknowledged and written and less than wcNetQuietSegs
// segments over the idle span, max(wcNetIdle, 2·srtt) up to wcNetIdleMax;
// a quiet socket is read once a wcNetQuiet (due), at once when its meter
// has wcNetQuietBytes written to it. The cap's factor is the net's
// (wcNetKOf, HS2_TUNE_PACENET_K), its floor wcPaceFloor, and it moves only
// by more than wcNetMove.

// TuneSendCap 0 (cmd/hs2 applyTuning: HS2_TUNE_SENDCAP=0) turns C4-lite's
// own cap off: the pacing net's alone, on the exit only, as before.
var TuneSendCap = 1

// SendCapOff says whether C4-lite is off (HS2_TUNE_SENDCAP=0).
func SendCapOff() bool { return TuneSendCap == 0 }

// SendCapBlocked 1 (HS2_TUNE_SENDCAP_BLOCKED=1; lab, for the judge, angle
// A): a link whose writer waited on its socket since the reading before
// (the link meter's wrBlocked: C3's gate or the write) is not app-limited
// to C4-lite, whatever the kernel's bit (§3.3: C4 reads C3's "blocked
// behind the socket"; §3.4 has C4-lite on the bit alone, the default).
// Under the document's gate (T12) the bit is set on every sample and the
// cap never comes; with this it does.
var SendCapBlocked = 0

const (
	// capSpan: the cap follows the largest delivery rate of this span.
	capSpan = 2 * time.Second
	// capDlvN, capPtN: the readings kept for the span and for the idle span
	// (one each capGap at most: a reading each 100 ms keeps 3.2 s and 6.4 s).
	capDlvN = 32
	capPtN  = 64
	capGap  = 100 * time.Millisecond
)

// capSample is one reading of a link socket's TCP_INFO.
type capSample struct {
	Now        time.Duration
	Delivery   float64 // tcpi_delivery_rate, bytes/s
	AppLimited bool    // tcpi_delivery_rate_app_limited: that sample's
	// Acked: bytes acknowledged (tcpi_bytes_acked); Written: bytes written
	// to the socket (its link meter); both cumulative
	Acked, Written uint64
	Segs           uint32        // data segments sent (tcpi_data_segs_out), cumulative
	SRTT           time.Duration // the socket's srtt (0: unknown): the idle span
	MinRTT         time.Duration // the socket's min_rtt (0: unknown): α's queue delay, SRTT - MinRTT
	Received       uint64        // bytes received on the socket (tcpi_bytes_received), cumulative: α's other direction
	// OWD: the queue delay of this end's sending direction from the link's
	// one-way delays (owd.go; OWDok false: none), α's in place of
	// SRTT - MinRTT
	OWD   time.Duration
	OWDok bool
	// OWDwait: the link has one-way delays (OWD on) but none heard yet:
	// α's start holds (srtt − min_rtt holds the other direction's queue)
	OWDwait bool
	// OWN: the queue delay of this end's sending direction on the BULK
	// link's own one-way delays (γ1's second signal, AlphaOwn: each BULK
	// link trades its own messages too; OWNok false: none heard lately)
	OWN   time.Duration
	OWNok bool
}

type capRate struct {
	at time.Duration
	r  float64
}

type capPt struct {
	at           time.Duration
	acked, wrote uint64
	segs         uint32
}

// sendCap is one link socket's C4-lite: pure (its readings in, the cap
// out).
type sendCap struct {
	dlv   [capDlvN]capRate // the delivery rates of the last capSpan (ring)
	dHead int
	dN    int
	pts   [capPtN]capPt // readings of the idle span (ring), one each capGap
	pHead int
	pN    int
	last  capPt // the latest reading
	n     uint64
	on    bool   // its own cap: from the first sample not app-limited to idleness
	own   uint64 // ... bytes/s
	pn    uint64 // the pacing net's (0: none)
	set   uint64 // the cap in force (0: none)
	quiet bool   // the latest reading found the socket quiet
	// full: the delivery rate of the latest sample that was not app-limited
	// (0: none yet), for C3 (hostRate)
	full float64
	// linked: off was set for the link as it came up (its caps and the
	// switch then: the cores' runner never reads the switch again); off: no
	// cap of its own on this link, as SendCapOff for the process
	linked, off bool
	// alpha: a BULK link's socket (β2): its own cap is α's (alphaStep), not
	// C4-lite's 1.25 × the delivery rate; aRate is the cap, aQ the queue
	// delay of the latest reading, aDlv the delivery rates of its latest
	// readings (ring), aMax one delivery maximum a second over alphaSpan
	// (ring, from aSec)
	alpha bool
	aRate float64
	aQ    time.Duration
	aQ0   time.Duration // the reading before's
	aUp   int           // readings in a row q grew past alphaLow
	aAck0 uint64        // acked and received at the reading before
	aRx0  uint64
	aDlv  [alphaRecent]float64
	aDi   int
	aMax  [alphaSpan]float64
	aSec  int64
	// aStart: no queue of its own seen since α armed (the judge's fix 1:
	// the cap is C4-lite's till then); aOWD: the latest q is the one-way
	// delay (fix 2), not srtt − min_rtt
	aStart, aOWD bool
	// aCutFrom: the cap before its latest cut (0: none yet), the plateau's;
	// aFree: readings in a row its own queue stayed under a quarter of the
	// target (free growth)
	aCutFrom float64
	aFree    int
	// aLong: the 10 s delivery maximum of the latest reading
	aLong float64
	// γ1's second cap (AlphaOwn, ownStep): bRate the cap from the link's
	// own one-way queue (0: none, no reading of it), bQ the latest queue
	// past the shared one and bTgt its target then
	bRate float64
	bQ    time.Duration
	bTgt  time.Duration
	// probe (AlphaProbe): readings in a row with no queue sign while the
	// cap bound, of each cap
	aCalm, bCalm int
	// tallies (the status line)
	appLim, lifts uint64
	aCuts, aGrows uint64
	bCuts, bGrows uint64
}

// TuneAlpha 0 (HS2_TUNE_ALPHA=0, lab): BULK links keep C4-lite's cap.
var TuneAlpha = 1

// AlphaHalf, AlphaPlateau 0 (HS2_TUNE_ALPHA_HALF=0, HS2_TUNE_ALPHA_PLATEAU=0;
// lab): the judge's FIFO tuning off. On a shared FIFO a light flow's round
// trip crosses both directions' queues, and with the one-way delay each
// direction answered for its own at CoDel's 5 ms: the probe of the deep
// FIFO cell (25 Mbit/s, 600 packets) at +27 ms over its floor against +23
// on srtt, whose sum both ends held near 5 ms together. So with the
// one-way delay each direction holds half the target (alphaLow/2, and
// alphaBig/2 for a big one), and the cap grows by alphaGrowNear only
// within alphaNear of the rate it was cut from last (the path's rate, as
// far as it knows): the few % a reading past it are the queue's peaks.
var AlphaHalf, AlphaPlateau = 1, 1

// AlphaFree 0 (HS2_TUNE_ALPHA_FREE=0, lab): the judge's free growth off.
// The start ends at the first queue of its own, and BBR's start-up can make
// one while it still delivers a tenth of the path: then the cap climbed by
// 3% a reading from there (a download beside an upload on a deep FIFO at
// 6, 8, 9, 11, 12, 14, 16, 18, 20 Mbit/s second by second, 77 at second
// 19). With the one-way delay, a queue under a quarter of the target for
// alphaFreeN readings in a row means the path has room: the cap grows by
// alphaGrowFree a reading (C4-lite's ceiling at most) and the rate it was
// cut from no longer holds it back.
//
// Off by default: in the two-way loop model it made the cap overshoot after
// every drained cut (a queue to 9.8 ms and 0.97 of the paths, against 5.9
// ms and 0.99 without); the start's own rule (alphaStartGrow) took its
// place.
var AlphaFree = 0

const (
	alphaFreeN    = 5
	alphaGrowFree = 0.1
	// alphaStartGrow: the start does not end while the delivery rate grew
	// by this over the last alphaRecent−1 readings (BBR's start-up: it
	// doubles each round trip, and its queue then is no sign of the path's
	// rate; in F2 a start ended at 12 Mbit/s of 100 and the cap stayed
	// there 10 s)
	alphaStartGrow = 1.25
)

const (
	alphaGrowNear = 0.005
	alphaNear     = 0.95
)

// TuneAlphaStart 0 (HS2_TUNE_ALPHA_START=0, lab): α starts from the
// delivery rate it armed at and grows a few % a reading, as β2 had it. The
// start is judged on the link's one-way delay (fix 2) alone: with
// HS2_TUNE_OWD=0 α is β2's (srtt − min_rtt holds the other direction's
// queue, and β2's two-way loop test held its queue under 15 ms that way).
//
// The judge's fix 1 (R1 round 2): β2's α armed at the first delivery
// sample of a BULK socket that flows had just moved to (4 to 10 Mbit/s) and
// grew by alphaGrowFast a reading, 1.34 times a second: a 100 Mbit/s path
// took 7 s to fill (one_vs_16 on the rig: 10, 14, 18, 25, 33, 45, 60, 78
// Mbit/s, second by second; the sum 72.6 against 90.1). Till the socket's
// own queue first passes alphaLow the cap is now C4-lite's, 1.25 times the
// 2 s delivery maximum, which BBR's start-up fills in about a second; the
// first queue ends the start and α takes over.
var TuneAlphaStart = 1

// AlphaOff says whether α is off.
func AlphaOff() bool { return TuneAlpha == 0 }

// α, the rate cap of a BULK link's socket (R1 round 2, decision T18): the
// round-2 diagnosis found the bulk link's own standing queue (BBR's
// cwnd_gain, 85 to 175 ms on a deep FIFO) the one thing a light flow on a
// TCP connection of its own still waits behind, and CoDel's drops at its
// 5 ms target a link above it never stops making. Each reading of the
// socket (100 ms), with q = srtt − min_rtt, d the largest delivery rate of
// its last alphaRecent readings, and share this direction's part of the
// bytes the connection carried both ways since the reading before:
//
//   - the path delivered clearly less than the cap (d under alphaKeep × the
//     cap): this direction is at its bottleneck, the queue its own; the cap goes
//     to d less the share of it that drains q past alphaLow (CoDel's 5 ms
//     target) within alphaDrain, at most alphaCutMax;
//   - the greater direction (share ≥ alphaMajor) delivered most but not
//     all of the cap (under alphaMatch: all of it means the cap is under
//     the path), or q passed
//     alphaBig, and q past alphaLow grew since the reading before (by
//     alphaRise): the cap is a little past the path (a delivery sample is too
//     noisy to see a few %: a test at 0.97 cut the cap at every dip and held
//     a download at half the path on the rig); it backs off the same way;
//   - else the queue, if any, is not this socket's doing: the cap grows by
//     alphaGrowFast under alphaLow, by alphaGrow over it, to 1.25 × the 2 s
//     delivery maximum (C4-lite's), so it finds a path that got faster.
//
// Why not q alone: srtt holds both directions' queues (the acks come back
// through the other one, behind that end's data and pacing), so on a link
// that carries both a download and an upload the two ends read the same
// sum. A cap that backed off on q alone held the lighter direction down for
// good (on the rig, the download of a link with an upload on it at 0.3
// Mbit/s of 25, then at 6 with a factor of its own 10 s maximum, then at 16
// with the delivery test alone): it cut for a queue its cuts could not
// drain. So a growing queue is the greater direction's to answer (the
// opposite direction's bytes weigh in, as the lead's T18 has it), and the
// lesser one's own overshoot only the delivery test catches (a tighter one,
// 0.97, cut the lesser direction at every noisy sample: a download at 6 of
// 25 Mbit/s on the rig). The loop tests hold both directions over 0.96 of
// equal paths (TestAlphaTwoWays); with a lesser direction much slower than
// the greater one's path (100 down, 20 up) the greater one is held to about
// the lesser one's rate: an open item. The headroom low rates need (the other direction's
// acks, the TUN's pings and the QUIET link share the bottleneck; a fixed
// 0.95 left 21 to 32 ms there) comes from the queue: whatever else crosses
// the bottleneck shows in q while this direction is at it. Never under
// alphaKMin × the 10 s delivery maximum (a noisy reading never starves
// it), nor alphaFloor. A sample Linux marks app-limited holds the cap,
// unless it delivered about the cap: a cap that binds makes Linux mark
// most samples so (84% of a 100 Mbit/s link's on the rig, whose cap then
// never grew).
const (
	alphaLow      = 5 * time.Millisecond
	alphaDrain    = 100 * time.Millisecond
	alphaCutMax   = 0.1
	alphaGrow     = 0.02
	alphaGrowFast = 0.03
	alphaRise     = 500 * time.Microsecond
	alphaRiseN    = 1
	alphaKeep     = 0.85
	alphaMatch    = 0.98
	alphaBig      = 7500 * time.Microsecond
	alphaMajor    = 0.5
	alphaKMin     = 0.75
	alphaRecent   = 3
	alphaSpan     = 10
	alphaFloor    = 32 << 10
)

// alphaLongMax notes the reading's delivery rate and is the largest of the
// last alphaSpan seconds.
func (c *sendCap) alphaLongMax(s capSample) float64 {
	sec := int64(s.Now / time.Second)
	switch d := sec - c.aSec; {
	case d >= alphaSpan || d < 0:
		c.aMax = [alphaSpan]float64{}
	default:
		for i := int64(1); i <= d; i++ {
			c.aMax[(c.aSec+i)%alphaSpan] = 0
		}
	}
	c.aSec = sec
	if k := sec % alphaSpan; s.Delivery > c.aMax[k] {
		c.aMax[k] = s.Delivery
	}
	mx := 0.0
	for _, v := range c.aMax {
		mx = math.Max(mx, v)
	}
	return mx
}

// AlphaOwn 0 (HS2_TUNE_ALPHA_OWN=0): α reads the shared (QUIET) one-way
// queue alone, version 5 of round 2. On (γ1, R1 round 3, the default):
// each BULK link trades its own one-way delay messages too (silent while
// idle), and α is the lesser of two caps: the shared queue's (alphaStepA:
// version 5's, half the target: the light flows' queue on a shared path)
// and its own queue's (ownStep: CoDel's full target, only to keep CoDel
// from dropping). See ownStep.
var AlphaOwn = 1

// AlphaOwnOn says whether α has its own-queue cap (and the BULK links
// their own loops): AlphaOwn with version 5's shared queue.
func AlphaOwnOn() bool { return AlphaOwn != 0 && TuneOWDQuiet != 0 && !OWDOff() && !AlphaOff() }

// AlphaProbe 0 (HS2_TUNE_ALPHA_PROBE=0): no probing. On (γ1, the default):
// a cap that bound (it delivered about the cap) with no sign of a queue for
// alphaProbeN readings in a row (2 s) grows faster each reading after
// (alphaGrowFast × alphaProbeStep^k, up to alphaProbeMax), past the slow
// growth near its last cut: the path may be faster now (the judge's model:
// a path ten times faster grew ×1.16 in 2 s). The first sign of a queue
// (a quarter of the target) ends the probe.
var AlphaProbe = 1

const (
	alphaProbeN    = 20
	alphaProbeStep = 1.5
	alphaProbeMax  = 0.25
	// ownSegs, ownMSS: the own cap's target is at least ownSegs segments
	// at the rate the link is served (a DRR round each, on fq_codel)
	ownSegs = 2
	ownMSS  = 1448
	// ownCutStand: the most the own cap cuts a reading for a queue past
	// its target that stands (alphaCutMax while it rises)
	ownCutStand = 0.02
)

// probeGrow is the probe's growth k readings into it.
func probeGrow(k int) float64 {
	return math.Min(alphaGrowFast*math.Pow(alphaProbeStep, float64(k)), alphaProbeMax)
}

// alphaStep is α's cap from a reading (c.on: the socket has had a sample
// not app-limited since it was last idle; arming: this is the first): the
// shared queue's (alphaStepA), and with AlphaOwnOn its own queue's
// (ownStep); alphaCap is the lesser.
func (c *sendCap) alphaStep(s capSample, arming bool) {
	c.alphaStepA(s, arming)
	if AlphaOwnOn() {
		c.ownStep(s, arming)
	} else {
		c.bRate = 0
	}
}

// alphaCap is the cap in force of α's two.
func (c *sendCap) alphaCap() float64 {
	if c.bRate > 0 && c.bRate < c.aRate {
		return c.bRate
	}
	return c.aRate
}

// dlvGrowing says whether the delivery of the latest reading rose by
// alphaStartGrow since the oldest of the last alphaRecent (fewer than two
// readings: it has just started, as BBR's start-up).
func (c *sendCap) dlvGrowing(cur float64) bool {
	n, old := 0, 0.0
	for k := 1; k <= alphaRecent; k++ {
		if v := c.aDlv[(c.aDi+k)%alphaRecent]; v > 0 {
			if n++; old == 0 {
				old = v
			}
		}
	}
	return n < 2 || cur >= alphaStartGrow*old
}

// ownStep is γ1's second cap (AlphaOwn, R1 round 3; decision T19), from
// the link's own one-way queue of this direction past the shared (QUIET)
// one, qx: what the link's own flow queue holds that the light flows do
// not wait in. On fq_codel that is its flow queue, where CoDel drops past
// 5 ms (version 5 saw no queue there: CoDel dropped 7% of a link's
// segments in one_vs_16, and (b) fell to 0.36 when a lone flow's window
// collapsed); on a shared FIFO about none (the shared cap answers: no
// double cut). Its target is CoDel's full alphaLow, not the shared cap's
// half, or ownSegs segments at the rate the link is served if that is
// longer: on fq_codel a link served at r waits a DRR round of MSS/r for
// each segment queued, so with many links at a few Mbit/s each no rate
// under the share drains a queue under 5 ms (version 4 cut for that queue
// at half the target, and from what was delivered: with 17 flows on 64
// links the sum fell from 89 to 50 Mbit/s); the target grows as the rate
// falls, which ends such a ratchet. Past its target and rising (by
// alphaRise) the cap goes to the lesser of itself and the delivery, less
// the share that drains the excess within alphaDrain (at most
// alphaCutMax); past it but standing, by at most ownCutStand; not while
// it falls (it drains already) nor while the delivery still grows by
// alphaStartGrow (BBR's start-up: its queue is no sign of the share).
// Under it the cap grows by alphaGrowFast a reading less the share of the
// target its queue holds (at the path the cap settles with its queue under
// the target, not a step past it each time), with no slow growth near its
// last cut (version 5's plateau would hold a link under its share for
// seconds), and the probe (AlphaProbe), to C4-lite's ceiling. Never under alphaKMin × the 10 s
// maximum, nor alphaFloor. No reading of its own (none heard, the link
// idle): no cap of its own.
func (c *sendCap) ownStep(s capSample, arming bool) {
	if !s.OWNok || s.Delivery <= 0 {
		if !s.OWNok {
			c.bRate, c.bQ, c.bCalm = 0, 0, 0
		}
		return
	}
	d := 0.0
	for _, v := range c.aDlv {
		d = math.Max(d, v)
	}
	ceil := math.Max(wcNetKOf()*c.maxDlv(), alphaFloor)
	if arming || c.bRate <= 0 {
		c.bRate, c.bQ, c.bCalm = ceil, 0, 0
	}
	qx := s.OWN
	if s.OWDok {
		qx -= s.OWD
	}
	qx = max(qx, 0)
	tgt := alphaLow
	if d > 0 {
		tgt = max(tgt, time.Duration(float64(ownSegs*ownMSS)/d*float64(time.Second)))
	}
	rising, falling := qx > c.bQ+alphaRise, qx < c.bQ-alphaRise
	c.bQ, c.bTgt = qx, tgt
	switch {
	case qx > tgt && (falling || c.dlvGrowing(s.Delivery)):
		c.bCalm = 0 // draining already, or start-up's queue: it holds
	case qx > tgt:
		cut := math.Max(float64(qx-tgt), 0) / float64(alphaDrain)
		if rising {
			cut = math.Min(cut, alphaCutMax)
		} else {
			cut = math.Min(cut, ownCutStand)
		}
		c.bRate = math.Min(c.bRate, d) * (1 - cut)
		c.bCuts++
		c.bCalm = 0
	case s.AppLimited && d < alphaKeep*c.bRate:
		c.bCalm = 0 // the sender had less to send than the cap let out: it holds
	case rising && qx > tgt/4 && d < alphaMatch*c.bRate:
		// its queue grows while the path took less than the cap: the cap
		// is past the path already (the reading lags a step); no more
		// growth, the cap at what was delivered
		c.bRate = math.Max(d, alphaFloor)
		c.bCalm = 0
	default:
		// the closer its queue to the target, the slower it grows: at
		// the path the cap settles with the queue under the target
		// instead of overshooting by a step each time
		g := alphaGrowFast * (1 - float64(qx)/float64(tgt))
		if AlphaProbe != 0 {
			if qx < tgt/4 && d >= alphaMatch*c.bRate {
				c.bCalm++
			} else {
				c.bCalm = 0
			}
			if c.bCalm > alphaProbeN {
				g = math.Max(g, probeGrow(c.bCalm-alphaProbeN))
			}
		}
		c.bRate = math.Min(c.bRate*(1+g), math.Max(ceil, c.bRate))
		c.bGrows++
	}
	c.bRate = math.Max(c.bRate, math.Max(alphaKMin*c.aLong, alphaFloor))
}

// alphaStepA is α's shared-queue cap (version 5 of round 2; the one cap
// with AlphaOwn off).
func (c *sendCap) alphaStepA(s capSample, arming bool) {
	long := c.alphaLongMax(s)
	c.aLong = long
	if arming || c.aRate <= 0 {
		c.aRate = math.Max(c.maxDlv(), s.Delivery)
		c.aDlv = [alphaRecent]float64{}
		c.aStart = TuneAlphaStart != 0 && !OWDOff()
		c.aCalm = 0
	}
	c.aQ, c.aOWD = 0, false
	switch {
	case s.OWDok:
		c.aQ, c.aOWD = max(s.OWD, 0), true
	case s.SRTT > 0 && s.MinRTT > 0 && s.SRTT > s.MinRTT:
		c.aQ = s.SRTT - s.MinRTT
	}
	// this direction's share of what the connection carried since the
	// reading before: the other one's queue is in q too
	dtx, drx := s.Acked-c.aAck0, s.Received-c.aRx0
	c.aAck0, c.aRx0 = s.Acked, s.Received
	share := 1.0
	if dtx+drx > 0 && s.Received >= drx {
		share = float64(dtx) / float64(dtx+drx)
	}
	if s.Delivery <= 0 {
		return // nothing to judge by: it holds
	}
	c.aDi = (c.aDi + 1) % alphaRecent
	c.aDlv[c.aDi] = s.Delivery
	d := 0.0
	for _, v := range c.aDlv {
		d = math.Max(d, v)
	}
	ceil := wcNetKOf() * c.maxDlv()
	low, big := alphaLow, alphaBig
	if c.aOWD && AlphaHalf != 0 {
		low, big = alphaLow/2, alphaBig/2
	}
	// fix 1: till its own queue first passes alphaLow the cap is C4-lite's,
	// whatever Linux's app-limited bit (under C4-lite's 1.25 x a sender
	// fed by a 20 KiB lowat finds its queue empty on most samples: the
	// judge's first version held the start, and a download's standing
	// queue past 300 ms, for good behind the app-limited hold below); the
	// first queue of its own ends it, the cap at what was delivered less
	// the share that drains the queue past alphaLow
	if c.aStart && (c.aOWD || s.OWDwait) {
		// growing: the delivery rose by alphaStartGrow since the oldest
		// reading of the last alphaRecent (fewer than two since it armed:
		// it has just started, as BBR's start-up)
		growing, n, old := true, 0, 0.0
		for k := 1; k <= alphaRecent; k++ {
			if v := c.aDlv[(c.aDi+k)%alphaRecent]; v > 0 {
				if n++; old == 0 {
					old = v
				}
			}
		}
		if n >= 2 {
			growing = s.Delivery >= alphaStartGrow*old
		}
		if c.aQ <= low || s.OWDwait || growing {
			c.aRate = math.Max(ceil, alphaFloor)
			c.aGrows++
			c.aQ0 = c.aQ
			return
		}
		c.aStart = false
		cut := math.Min(math.Max(float64(c.aQ-low), 0)/float64(alphaDrain), alphaCutMax)
		c.aCutFrom = math.Min(c.aRate, d)
		c.aRate = c.aCutFrom * (1 - cut)
		c.aCuts++
		c.aQ0 = c.aQ
		c.aRate = math.Max(c.aRate, math.Max(alphaKMin*long, alphaFloor))
		return
	}
	if s.AppLimited && d < alphaKeep*c.aRate {
		return // the sender had less to send than the cap let out: it holds
	}
	if c.aOWD && c.aQ < low/4 {
		c.aFree++
	} else {
		c.aFree = 0
	}
	if c.aQ > low && c.aQ > c.aQ0+alphaRise {
		c.aUp++
	} else {
		c.aUp = 0
	}
	c.aQ0 = c.aQ
	cut := math.Min(math.Max(float64(c.aQ-low), 0)/float64(alphaDrain), alphaCutMax)
	// fix 2: a one-way delay is this direction's queue alone, so a growing
	// one is this direction's to answer whatever its share of the bytes
	mine := share >= alphaMajor || c.aOWD
	switch {
	case !c.aOWD && (s.SRTT <= 0 || s.MinRTT <= 0):
		c.aRate = ceil // no queue reading: C4-lite's
	case d < alphaKeep*c.aRate:
		c.aRate = d * (1 - cut)
		if cut > 0 {
			c.aCutFrom = d
			c.aCuts++
		}
		c.aCalm = 0
	case c.aUp >= alphaRiseN && mine && (d < alphaMatch*c.aRate || c.aQ > big):
		c.aCutFrom = math.Min(c.aRate, d)
		c.aRate = c.aCutFrom * (1 - cut)
		c.aCuts++
		c.aCalm = 0
	default:
		g := alphaGrow
		if c.aQ < low {
			g = alphaGrowFast
		}
		if c.aOWD && AlphaPlateau != 0 && c.aCutFrom > 0 && c.aRate >= alphaNear*c.aCutFrom && c.aRate <= c.aCutFrom/alphaNear {
			g = alphaGrowNear // near the rate it was cut from: the path's
		}
		if c.aOWD && AlphaProbe != 0 {
			// γ1's probe: a cap that bound with no queue for alphaProbeN
			// readings grows faster each reading, the plateau over
			if c.aQ < low/4 && d >= alphaMatch*c.aRate {
				c.aCalm++
			} else {
				c.aCalm = 0
			}
			if c.aCalm > alphaProbeN {
				g = math.Max(g, probeGrow(c.aCalm-alphaProbeN))
				c.aCutFrom = 0
			}
		}
		if c.aOWD && AlphaFree != 0 && c.aFree >= alphaFreeN {
			g = math.Max(g, alphaGrowFree) // no queue for a while: room on the path
			c.aCutFrom = 0
		}
		c.aRate = math.Min(c.aRate*(1+g), math.Max(ceil, c.aRate))
		c.aGrows++
	}
	c.aRate = math.Max(c.aRate, math.Max(alphaKMin*long, alphaFloor))
}

// Update takes a reading and says the cap in force (0: none), whether to
// set it on the socket now, or to lift it.
func (c *sendCap) Update(s capSample) (rate uint64, apply, lift bool) {
	fresh := c.n > 0 && s.Acked > c.last.acked
	c.n++
	if s.AppLimited {
		c.appLim++
	} else if s.Delivery > 0 {
		c.full = s.Delivery
	}
	pt := capPt{at: s.Now, acked: s.Acked, wrote: s.Written, segs: s.Segs}
	c.last = pt
	// the delivery rates of the span (a reading within capGap of the latest
	// kept one joins it)
	if s.Delivery > 0 {
		if k := (c.dHead + c.dN - 1) % capDlvN; c.dN > 0 && s.Now-c.dlv[k].at < capGap/2 {
			c.dlv[k].r = math.Max(c.dlv[k].r, s.Delivery)
		} else {
			if c.dN == capDlvN {
				c.dHead, c.dN = (c.dHead+1)%capDlvN, c.dN-1
			}
			c.dlv[(c.dHead+c.dN)%capDlvN] = capRate{s.Now, s.Delivery}
			c.dN++
		}
	}
	for c.dN > 0 && s.Now-c.dlv[c.dHead].at > capSpan {
		c.dHead, c.dN = (c.dHead+1)%capDlvN, c.dN-1
	}
	// idle: next to nothing acknowledged, written or sent over the idle
	// span, read from a reading at least that old
	span := min(max(wcNetIdle, 2*s.SRTT), wcNetIdleMax)
	for c.pN > 1 && s.Now-c.pts[(c.pHead+1)%capPtN].at >= span {
		c.pHead, c.pN = (c.pHead+1)%capPtN, c.pN-1
	}
	idle := false
	if c.pN > 0 {
		if r := c.pts[c.pHead]; s.Now-r.at >= span {
			idle = s.Acked-r.acked < wcNetQuietBytes && s.Written-r.wrote < wcNetQuietBytes &&
				s.Segs-r.segs < uint32(wcNetQuietSegs)
		}
	}
	if k := (c.pHead + c.pN - 1) % capPtN; c.pN == 0 || s.Now-c.pts[k].at >= capGap {
		if c.pN == capPtN {
			c.pHead, c.pN = (c.pHead+1)%capPtN, c.pN-1
		}
		c.pts[(c.pHead+c.pN)%capPtN] = pt
		c.pN++
	}
	c.quiet = idle
	arming := false
	switch {
	case c.off || !c.linked && SendCapOff():
		c.on = false
	case c.on && idle:
		c.on = false
		c.lifts++
	case !c.on && !idle && fresh && !s.AppLimited && s.Delivery > 0:
		c.on, arming = true, true
	}
	if c.on {
		if c.alpha {
			c.alphaStep(s, arming)
			c.own = uint64(c.alphaCap())
		} else {
			c.own = uint64(math.Max(wcNetKOf()*c.maxDlv(), wcPaceFloor))
		}
	}
	return c.decide()
}

// maxDlv is the largest delivery rate of the last capSpan.
func (c *sendCap) maxDlv() float64 {
	mx := 0.0
	for i := range c.dN {
		mx = math.Max(mx, c.dlv[(c.dHead+i)%capDlvN].r)
	}
	return mx
}

// hostRate is C3's rate from these readings (hostq.go; decision T2): the
// least of the largest delivery rate of the last capSpan and the latest
// sample that was not app-limited. A drop shows at its first sample (the
// mark and the gate follow at the next hostqTick), where the pacing cap
// keeps the largest of the span, not to collapse on a passing dip; a
// higher app-limited sample Linux kept raises neither. False before any
// sample that was not app-limited (C3 takes the link meter's written rate
// then).
func (c *sendCap) hostRate() (float64, bool) {
	if c.full <= 0 {
		return 0, false
	}
	if mx := c.maxDlv(); mx > 0 {
		return math.Min(mx, c.full), true
	}
	return c.full, true
}

// SetPacenetCap takes the pacing net's cap (bytes/s; 0: none): the lesser
// of the two is in force from the next decision.
func (c *sendCap) SetPacenetCap(rate uint64) { c.pn = rate }

// decide is the cap in force: its own while on, the net's, the lesser of
// the two; set when it moved by more than wcNetMove, lifted when none is.
func (c *sendCap) decide() (rate uint64, apply, lift bool) {
	want := uint64(0)
	if c.on {
		want = c.own
	}
	if c.pn > 0 && (want == 0 || c.pn < want) {
		want = c.pn
	}
	switch {
	case want == 0:
		lift = c.set != 0
		c.set = 0
		return 0, false, lift
	case c.set == 0 || math.Abs(float64(want)-float64(c.set)) > wcNetMove*float64(c.set):
		c.set = want
		return want, true, false
	}
	return c.set, false, false
}

// due says whether the runner reads the socket at this wake: always, but a
// quiet one only each wcNetQuiet or once wcNetQuietBytes were written to it
// (written: its link meter's count; 0 with none).
func (c *sendCap) due(now time.Duration, written uint64) bool {
	if !c.quiet || c.n == 0 {
		return true
	}
	return written-c.last.wrote >= wcNetQuietBytes || now-c.last.at >= wcNetQuiet-capGap/2
}

// capSys is what C4-lite's runner asks of a link socket (pacecap_linux.go;
// a synthetic one in the tests).
type capSys interface {
	info() (tcpStat, bool)      // TCP_INFO
	setPacing(rate uint64) bool // SO_MAX_PACING_RATE (0 lifts)
}

// sendcapNoDlv: the line for a kernel without tcpi_delivery_rate is written.
var sendcapNoDlv atomic.Bool

// sendCapLink is one link socket's C4-lite with its runner's state: step
// is the runner's wake (each 100 ms), SetPacenetCap the pacing net's
// hand-off (the exit's runner); the lock keeps the two to one cap. The
// same reading moves the link's host-queue guard (C3) each hostqTick.
type sendCapLink struct {
	mu   sync.Mutex
	sys  capSys
	core sendCap
	// hq: the link's C3 (nil: none); hqAt, hqWrote: its latest update and
	// what the link meter had written then (hqSet: one was made)
	hq      *hostQ
	hqAt    time.Duration
	hqWrote uint64
	hqSet   bool
	// blocked: how long the link's writer has waited on the socket so far,
	// at C3's gate or in the write (the link meter's wrBlocked, waits past
	// blockedMin; nil: unknown), and its value at the reading before
	blocked   func() int64
	blockSeen int64
	// owd: the link's one-way delays (a BULK link's, owd.go; nil: none),
	// and the bytes received at the reading before, for its beat
	owd  *owdLink
	rx0  uint64
	rxAt time.Duration
}

// newSendCapLink: the C4-lite of a link socket (sys), and the C3 its
// readings move (hq; nil: none).
func newSendCapLink(sys capSys, hq *hostQ) *sendCapLink { return &sendCapLink{sys: sys, hq: hq} }

// step reads the socket when the core has it due (written: what its link
// meter counted written to it), sets or lifts its cap, and moves C3. A
// kernel without tcpi_delivery_rate gets none of C4-lite's (one line for
// the process), and C3 the link meter's written rate.
func (l *sendCapLink) step(now time.Duration, written uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.core.due(now, written) {
		return
	}
	ts, ok := l.sys.info()
	if !ok {
		return
	}
	app := ts.appLimited
	if l.blocked != nil {
		// (SendCapBlocked) a writer that waited on the socket since the
		// reading before is no app-limited sender, whatever the kernel's
		// bit says: C3's gate keeps under a segment or two unsent, and
		// Linux marks every sample of a socket written to with less than a
		// segment unsent app-limited
		b := l.blocked()
		if b > l.blockSeen {
			app = false
		}
		l.blockSeen = b
	}
	if ts.dlvOK {
		var oq, nq time.Duration
		ook, nok := false, false
		if l.owd != nil {
			// the beat follows the busier direction of the link
			rx := 0.0
			if l.rxAt > 0 && now > l.rxAt && ts.bytesReceived >= l.rx0 {
				rx = float64(ts.bytesReceived-l.rx0) / (now - l.rxAt).Seconds()
			}
			l.rx0, l.rxAt = ts.bytesReceived, now
			r := max(l.core.maxDlv(), rx)
			src := owdShared(l.owd)
			src.noteRate(r)
			own := AlphaOwnOn() && src != l.owd
			if own {
				l.owd.noteRate(r) // its own loop's beat, and whether it is idle (γ1)
			}
			if l.core.alpha {
				oq, ook = src.queue()
				if own {
					nq, nok = l.owd.queue()
				}
			}
		}
		owait := l.owd != nil && l.core.alpha && !ook && !OWDOff()
		l.apply(l.core.Update(capSample{Now: now, Delivery: float64(ts.deliveryRate), AppLimited: app,
			Acked: ts.bytesAcked, Written: written, Segs: ts.segsOut, SRTT: usDur(uint64(ts.rtt)),
			MinRTT: usDur(uint64(ts.minRTT)), Received: ts.bytesReceived, OWD: oq, OWDok: ook, OWDwait: owait,
			OWN: nq, OWNok: nok}))
	} else if sendcapNoDlv.CompareAndSwap(false, true) {
		coreLogf("sendcap: the kernel lacks tcpi_delivery_rate: no pacing cap of C4-lite on the link sockets (the pacing net's alone, as before)")
	}
	l.guard(now, written, ts)
}

// guard moves C3 each hostqTick: its rate is hostRate, or, until the first
// sample not app-limited (or on a kernel without the delivery rate), what
// the link meter had written to the socket since the update before (0, an
// unknown rate, at the first: the gate at its floor); its segment the
// socket's.
func (l *sendCapLink) guard(now time.Duration, written uint64, ts tcpStat) {
	if l.hq == nil || l.hqSet && now-l.hqAt < hostqTick {
		return
	}
	rate, ok := l.core.hostRate()
	if !ok || !ts.dlvOK {
		rate = 0
		if l.hqSet && now > l.hqAt {
			rate = float64(written-l.hqWrote) / (now - l.hqAt).Seconds()
		}
	}
	l.hq.Update(rate, int(ts.sndMSS))
	l.hqAt, l.hqWrote, l.hqSet = now, written, true
}

// SetPacenetCap takes the pacing net's cap (0: lifted) and sets the lesser
// of the two at once; false: the socket refused it.
func (l *sendCapLink) SetPacenetCap(rate uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.core.SetPacenetCap(rate)
	return l.apply(l.core.decide())
}

func (l *sendCapLink) apply(rate uint64, apply, lift bool) bool {
	switch {
	case apply:
		if !l.sys.setPacing(rate) {
			l.core.set = 0 // not capped after all: the next decision sets it again
			return false
		}
	case lift:
		l.sys.setPacing(0)
	}
	return true
}

// line is the link's status line (§3.4).
func (l *sendCapLink) line(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := &l.core
	share := 0.0
	if c.n > 0 {
		share = 100 * float64(c.appLim) / float64(c.n)
	}
	if c.alpha {
		src := "srtt"
		if c.aOWD {
			src = "one-way"
		}
		start := ""
		if c.aStart {
			start = ", start"
		}
		own := ""
		if c.bRate > 0 {
			own = fmt.Sprintf("; own %.1f Mbit/s, queue %.1f ms past the shared, target %.1f ms, cuts %d, grows %d",
				c.bRate*8/1e6, msOf(c.bQ), msOf(c.bTgt), c.bCuts, c.bGrows)
		}
		return fmt.Sprintf("sendcap link %d: alpha cap %.1f Mbit/s (queue %.1f ms %s, target %.0f ms; cuts %d, grows %d%s%s), app-limited %.0f%%, idle-lifted %d",
			n, float64(c.set)*8/1e6, msOf(c.aQ), src, msOf(alphaLow), c.aCuts, c.aGrows, start, own, share, c.lifts)
	}
	return fmt.Sprintf("sendcap link %d: cap %.0f Mbit/s (%g x max delivery 2s), app-limited %.0f%%, idle-lifted %d",
		n, float64(c.set)*8/1e6, wcNetKOf(), share, c.lifts)
}
