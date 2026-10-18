package engine

import (
	"math"
	"time"
)

// The pacing net (WINDOW_CORE §5.6): a safety net under the exit's link
// sockets, the senders of the downloads.
//
// A BBRv1 sender held by its peer's windows is app-limited: it never leaves
// STARTUP, and its bandwidth filter never drops (an app-limited sample under
// the max is ignored), so it paces at ~3.9 times the highest rate it ever
// delivered. Once the edge's window core took that rate up, a path that gets
// worse later (its rate drops, a policer comes or tightens, a shallow queue,
// several downloads on it) takes every window update's credit, and the
// whole window after each PROBE_RTT, in bursts far past what it accepts:
// even windows of the base size then resend 12 to 35%, and the loss rule
// drains the link (red team, round 2: 131 of 6478 model runs, none without
// the core). The receiver cannot stop those bursts (it announced the credit
// before it could see the loss); only the sender's pace can.
//
// Capping every link socket's pacing at all times (the lab's WcorePaceCap)
// does it, but slowed a window-limited sender on an empty path by 15 to 30%
// (z12, P2–P4). So the net engages only when the socket's own counters show
// the trouble, and only as long as they do (each rule with its evidence at
// its constant below):
//
//   - engage: its resend share over the last wcNetWin is wcNetLoss or more
//     (well under the loss rule's 12%) and its pacing runs past wcNetRatio
//     times the largest delivery rate of the last wcNetSpan (a BBR in
//     PROBE_BW or a CUBIC paces near what it delivers: no burst to tame).
//     In its warm-up, and while its delivery rate still grows, only past
//     wcNetWarmLoss; past the path's own level a cap did not lower, only
//     past twice that;
//   - the cap: SO_MAX_PACING_RATE at wcNetK times that delivery rate, never
//     under wcPaceFloor. It follows the delivery rate down only while the
//     socket still resends past wcNetLoss with a queue at it (the path
//     slowed down); otherwise it only rises (a sender held by its cap and
//     its windows delivers less, and following that down ratchets);
//   - it must pay: still resending as much after wcNetJudge, it is lifted
//     (random loss, windows past what the path holds: not the bursts');
//   - lift: once the socket was clean for wcNetClean (longer each time the
//     trouble comes back soon), or at once when it goes idle.
//
// A clean socket is never touched. It applies to every exit link socket,
// whatever the edge runs: base windows past a shallow queue or a policer
// gain from it too. In the link model (pacenet_model_test.go, the red team's
// round-2 grid of 2240 runs, base and core each with the net off and on):
// the links the loss rule drains under the core and not under base 30 → 1,
// the base's own drains 321 → 15, the core's 343 → 16; on clean paths the
// mean rate of base 163.3 → 165.4 and of the core 236.9 → 252.9 Mbit/s.

// Knobs (cmd/hs2 applyTuning: HS2_TUNE_PACENET*), read as a session
// registers (PaceNet) and at each step (the others).
var (
	// PaceNet 0 turns the net off: no link socket's pacing is touched.
	PaceNet = 1
	// PaceNetK (> 0): the cap's factor over the delivery rate (wcNetK).
	PaceNetK = 0.0
	// PaceNetLoss (> 0): the resend share that engages it (wcNetLoss).
	PaceNetLoss = 0.0
)

var (
	// wcNetK: in the grid, ×1.25 left 1 drain under the core and not under
	// base, ×1.5 7 (×2, an early variant: 27 of the 131 drained cells of
	// round 2, against 20 at ×1.25), and the clean paths' mean rate was
	// 252.9 at ×1.25, 250.2 at ×1.5 (core) Mbit/s. A burst at 1.5 times a
	// shallow queue's rate still overflows it. (Linux's delivery_rate runs
	// past what the path carries with ACKs that come in clumps: the rig
	// judges how much of that margin is real.)
	wcNetK = 1.25
	// wcNetLoss: 1%, 2% and 3% left 23, 22 and 22 of the 131 drained cells
	// (the drains are of 12 to 35% resent); 2% stays clear of most random
	// loss of a path.
	wcNetLoss  = 0.02
	wcNetRatio = 2.0
	// wcNetWin: the resend share is judged over this span of readings, of
	// at least wcNetMinSegs segments sent.
	wcNetWin     = time.Second
	wcNetMinSegs = uint64(100)
	// wcNetSpan: the cap follows the largest delivery rate of this span.
	wcNetSpan = 2 * time.Second
	// wcNetCleanShare: a reading whose resend share over wcNetWin is under
	// this is clean (random loss of the path resends a little all along).
	wcNetCleanShare = 0.005
	// wcNetClean: lifted once the socket was clean for this long, twice as
	// long each time the trouble comes back within wcNetRecur of a lift
	// (at most wcNetBackMax times).
	wcNetClean   = 10 * time.Second
	wcNetRecur   = 30 * time.Second
	wcNetBackMax = 3
	// wcNetIdle: lifted when the socket had less than wcPaceIdle
	// acknowledged over this span (the next transfer starts free).
	wcNetIdle = time.Second
	// wcNetMove: a cap moves (a setsockopt) only by more than this share.
	wcNetMove = 0.05
	// wcNetQueue: the socket's srtt past its min_rtt by more than this is a
	// queue on the path (the cap follows the delivery rate down only then).
	wcNetQueue = wcDqMin
	// wcNetJudge, wcNetPaid: a cap must pay. Once it has held for
	// max(wcNetJudge, 3·srtt), a socket that still resends past wcNetLoss
	// and past wcNetPaid of the share that set it off is losing what pacing
	// does not cause (random loss of the path, windows past what the path
	// holds): the cap is lifted, and that share is the path's level, which
	// the net engages past only at wcNetLvlRel times for wcNetLvlTTL
	// (under 3% random loss a held cap cost the model 20 to 40% of the rate:
	// it paced away the catching up after each recovery).
	wcNetJudge  = 3 * time.Second
	wcNetPaid   = 0.5
	wcNetLvlRel = 2.0
	wcNetLvlTTL = 2 * time.Minute
	// wcNetWarm, wcNetWarmLoss: for max(wcNetWarm, 5·srtt) (at most
	// wcNetWarmMax) after a socket begins to send (or sends again after
	// idling) it engages only past wcNetWarmLoss. In its first STARTUP the
	// delivery rate doubles each round trip, and a cap at k times it ends
	// that STARTUP at a fraction of the path: the model's base under 3%
	// random loss went 13.5 → 8.4 Mbit/s for the rest of the run (capped at
	// 0.6 s, judged unpaid at 3.6 s, its BBR out of STARTUP at a third of
	// its rate). A policer or a shallow queue the first STARTUP runs into
	// loses far more, and waiting for it left 80 of 2240 runs of the grid
	// to the loss rule (base 321 → 76 drains with the cap at once, → 140
	// after a plain 3 s wait).
	wcNetWarm     = 3 * time.Second
	wcNetWarmMax  = 10 * time.Second
	wcNetWarmLoss = 0.1
	// wcNetGrow: a socket whose delivery rate of the last second is past
	// this many times the one of the second before is still growing (its
	// STARTUP, longer than wcNetWarm on a long round trip): it engages only
	// past wcNetWarmLoss then, as in its warm-up (the model's 3% random loss
	// at 300 ms: 7.5 → 6.1 Mbit/s, capped at 3 s in a STARTUP that was
	// still doubling; not engaging at all while it grew left a policer the
	// STARTUP runs into to the loss rule: 32 → 16 drains at 150 ms behind
	// one at 60% of the path, 0 with the warm-up's threshold).
	wcNetGrow = 1.25
)

// wcNetIn is one reading of a link socket's TCP_INFO.
type wcNetIn struct {
	now              time.Time
	retrans, segsOut uint64  // cumulative segments (resent; sent, resends included)
	acked            uint64  // cumulative bytes acknowledged
	dlv              float64 // tcpi_delivery_rate, bytes/s
	pace             float64 // tcpi_pacing_rate, bytes/s (0: unknown)
	srtt, minRTT     time.Duration
}

type wcNetAct uint8

const (
	wcNetNone    wcNetAct = iota
	wcNetEngage           // the cap is set
	wcNetMoveCap          // the cap moves
	wcNetRelease          // the cap is lifted
)

// wcNetOut is a step's decision: the socket's cap (0: none) when act says
// it changes, and on engaging what set it off.
type wcNetOut struct {
	act   wcNetAct
	cap   uint64  // bytes/s
	share float64 // the resend share over wcNetWin
	ratio float64 // pacing over the delivery rate
	k     float64
	dlv   float64 // the largest delivery rate of wcNetSpan, bytes/s
	idle  bool    // a release for idleness
}

type wcNetPt struct {
	at               time.Time
	rtx, segs, acked uint64
}

// wcNet is the net of one link socket: pure, its readings in, the cap out.
type wcNet struct {
	pts    []wcNetPt // the readings of the last max(wcNetWin, wcNetIdle), and the one before
	dlvs   []wcRate  // delivery rates of the last wcNetSpan
	cap    uint64    // the cap in force (0: none)
	rtxAt  time.Time // the latest reading that was not clean
	relAt  time.Time // the latest lift
	back   int       // doublings of wcNetClean
	onAt   time.Time // when the cap was set
	share0 float64   // ... and the largest resend share around then
	judged bool      // the cap was judged (wcNetJudge)
	lvl    float64   // the path's resend level a cap did not lower (0: none)
	lvlAt  time.Time
	busyAt time.Time // since when the socket sends (zero: idle)
	// counters (the link-end note)
	n        int
	maxShare float64
}

func wcNetKOf() float64 {
	if PaceNetK > 0 {
		return PaceNetK
	}
	return wcNetK
}

func wcNetLossOf() float64 {
	if PaceNetLoss > 0 {
		return PaceNetLoss
	}
	return wcNetLoss
}

// since is the reading at or before now − d (the oldest kept if none is).
func (n *wcNet) since(now time.Time, d time.Duration) wcNetPt {
	p := n.pts[0]
	for _, x := range n.pts {
		if now.Sub(x.at) < d {
			break
		}
		p = x
	}
	return p
}

// step takes one reading and says what to do with the socket's cap.
func (n *wcNet) step(in wcNetIn) wcNetOut {
	now := in.now
	n.pts = append(n.pts, wcNetPt{at: now, rtx: in.retrans, segs: in.segsOut, acked: in.acked})
	keep := max(wcNetWin, wcNetIdle)
	cut := 0
	for cut < len(n.pts)-1 && now.Sub(n.pts[cut+1].at) >= keep {
		cut++
	}
	n.pts = n.pts[cut:]
	if in.dlv > 0 {
		n.dlvs = append(n.dlvs, wcRate{now, in.dlv})
	}
	cut = 0
	for cut < len(n.dlvs) && now.Sub(n.dlvs[cut].at) > wcNetSpan {
		cut++
	}
	n.dlvs = n.dlvs[cut:]
	if len(n.pts) < 2 {
		return wcNetOut{}
	}
	dlv, last, prev := 0.0, 0.0, 0.0
	for _, x := range n.dlvs {
		dlv = math.Max(dlv, x.r)
		if now.Sub(x.at) < time.Second {
			last = math.Max(last, x.r)
		} else {
			prev = math.Max(prev, x.r)
		}
	}
	growing := last > wcNetGrow*prev
	w := n.since(now, wcNetWin)
	segs, rtx := in.segsOut-w.segs, in.retrans-w.rtx
	share := 0.0
	if segs > 0 {
		share = float64(rtx) / float64(segs)
	}
	if rtx > 0 && share >= wcNetCleanShare {
		n.rtxAt = now
	}
	idle := in.acked-n.since(now, wcNetIdle).acked < wcPaceIdle
	switch {
	case idle:
		n.busyAt = time.Time{}
	case n.busyAt.IsZero():
		n.busyAt = now
	}
	k, loss := wcNetKOf(), wcNetLossOf()
	target := uint64(math.Max(k*dlv, wcPaceFloor))
	if n.cap == 0 {
		need := loss
		if n.lvl > 0 && now.Sub(n.lvlAt) < wcNetLvlTTL {
			need = math.Max(need, wcNetLvlRel*n.lvl)
		}
		if growing || now.Sub(n.busyAt) < min(max(wcNetWarm, 5*in.srtt), wcNetWarmMax) {
			need = math.Max(need, wcNetWarmLoss)
		}
		if idle || segs < wcNetMinSegs || share < need || dlv <= 0 || in.pace <= wcNetRatio*dlv {
			return wcNetOut{}
		}
		if !n.relAt.IsZero() && now.Sub(n.relAt) < wcNetRecur {
			n.back = min(n.back+1, wcNetBackMax)
		} else {
			n.back = 0
		}
		n.cap, n.rtxAt, n.onAt, n.share0, n.judged = target, now, now, share, false
		n.n++
		n.maxShare = math.Max(n.maxShare, share)
		return wcNetOut{act: wcNetEngage, cap: n.cap, share: share, ratio: in.pace / dlv, k: k, dlv: dlv}
	}
	if share >= loss {
		n.maxShare = math.Max(n.maxShare, share)
	}
	if now.Sub(n.onAt) <= wcNetWin {
		// (the trouble the cap is judged against: the resends of the
		// losses before it came, over the first wcNetWin)
		n.share0 = math.Max(n.share0, share)
	}
	if !n.judged && now.Sub(n.onAt) >= max(wcNetJudge, 3*in.srtt) && segs >= wcNetMinSegs {
		n.judged = true
		if share >= loss && share >= wcNetPaid*n.share0 {
			// the cap did not pay: the loss is not the bursts'
			n.cap, n.relAt, n.lvl, n.lvlAt = 0, now, share, now
			return wcNetOut{act: wcNetRelease, share: share}
		}
	}
	if idle || now.Sub(n.rtxAt) >= wcNetClean<<n.back {
		n.cap, n.relAt = 0, now
		if idle {
			n.back = 0
		}
		return wcNetOut{act: wcNetRelease, idle: idle}
	}
	c := max(n.cap, target)
	if share >= loss && in.minRTT > 0 && in.srtt-in.minRTT > wcNetQueue {
		// still in trouble with a queue at the socket: the path, not the
		// cap, holds the delivery rate (a path that slowed down), and the
		// cap follows it down. Otherwise it only rises: a sender held by
		// its cap and its windows delivers less (its window takes longer
		// to send), and following that down would ratchet it to a third
		// of its rate at ×1.5 (under random loss past wcNetLoss: the model
		// lost 20% at 3%)
		c = target
	}
	if dlv <= 0 || math.Abs(float64(c)-float64(n.cap)) <= wcNetMove*float64(n.cap) {
		return wcNetOut{}
	}
	n.cap = c
	return wcNetOut{act: wcNetMoveCap, cap: c, k: k, dlv: dlv}
}
