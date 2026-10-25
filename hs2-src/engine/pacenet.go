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
//     slowed down), paces under the cap (it does not bind: a policer
//     that comes mid-transfer) or delivers under half of what the cap
//     allows (a policer under a cap that binds: stale); otherwise it only
//     rises (a sender held by its cap and its windows delivers less, and
//     following that down ratchets);
//   - it must pay: still resending as much after wcNetJudge, it is lifted
//     (random loss, windows past what the path holds: not the bursts'); a
//     cap that came down to half the one judged or less, or that starts to
//     bind, is judged anew;
//   - lift: once the socket was clean for wcNetClean (longer each time the
//     trouble comes back soon), or at once when it goes idle.
//
// A clean socket is never touched. It applies to every exit link socket,
// whatever the edge runs: base windows past a shallow queue or a policer
// gain from it too. In the link model (pacenet_model_test.go, the red team's
// round-2 grid of 2240 runs, base and core each with the net off and on):
// the links the loss rule drains under the core and not under base 30 → 2,
// the base's own drains 321 → 20, the core's 343 → 18; on clean paths the
// mean rate of base 163.3 → 166.1 and of the core 236.9 → 254.1 Mbit/s.
// Over all of round 2's sweeps and round 1's (7216 runs): 131 → 37, base
// 1547 → 194, core 1630 → 187.

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
	// wcNetK: in the grid (the net as it was before wcNetIdle took the
	// round trip and the delivery rate its acked bytes), ×1.25 left 1
	// drain under the core and not under base, ×1.5 7 (×2, an early
	// variant: 27 of the 131 drained cells of round 2, against 20 at
	// ×1.25), and the clean paths' mean rate was 252.9 at ×1.25, 250.2 at
	// ×1.5 (core) Mbit/s. A burst at 1.5 times a
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
	// acknowledged over max(wcNetIdle, 2·srtt), at most wcNetIdleMax (the
	// next transfer starts free). On a 2 s path a flow that waits for a
	// round trip's ACKs is no idle one: lifted over 1 s, the cap came and
	// went three times in 14 s and 4 downloads on 100/2000 q50 resent 11%
	// for 2.5% without it.
	wcNetIdle    = time.Second
	wcNetIdleMax = 5 * time.Second
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
	// wcNetQuiet, wcNetQuietBytes: a socket with no cap that had less than
	// wcNetQuietBytes acknowledged, and as little written to it (its link
	// meter), over its idle span (wcNetIdle) is quiet: the runner reads it
	// only every wcNetQuiet (due), and at the first wake its meter has
	// wcNetQuietBytes written to it since. Read every 100 ms, the exit's
	// idle links cost one TCP_INFO each (~3000 a second at 300 links: the
	// independent judge of step 1, row پ۵); quiet, one a second, and on a
	// link that holds the smux keepalive's NOP (every 4-8 s) and the control
	// channel's pong (every 9-15 s on an idle link), each padded to at most
	// ~1.4 KB, still quiet. A quiet socket is idle to the net (less than
	// wcPaceIdle over the same span) and has no cap to lift: no reading of
	// it can engage or lift anything, and one that holds a cap is read on
	// every wake, so its lift for idleness comes when it did. A download
	// that starts writes to the socket before its first ACK: the meter has
	// the net read it from that wake on, as it did, and only a socket that
	// sends what was written before its quiet span (a path back from a
	// stall) waits for the next reading, at most wcNetQuiet
	// (TestPaceNetQuietReads; the round-2 sweeps and the model's table do
	// not move).
	wcNetQuiet      = time.Second
	wcNetQuietBytes = uint64(8 << 10)
)

// wcNetIn is one reading of a link socket's TCP_INFO.
type wcNetIn struct {
	now              time.Time
	retrans, segsOut uint64  // cumulative segments (resent; sent, resends included)
	acked            uint64  // cumulative bytes acknowledged
	wrote            uint64  // cumulative bytes written to the socket (its link meter; 0: none)
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
	at                      time.Time
	rtx, segs, acked, wrote uint64
}

// wcNet is the net of one link socket: pure, its readings in, the cap out.
type wcNet struct {
	pts    []wcNetPt // the readings of the last max(wcNetWin, the idle span), and the one before
	dlvs   []wcRate  // delivery rates of the readings of the last wcNetSpan
	cap    uint64    // the cap in force (0: none)
	rtxAt  time.Time // the latest reading that was not clean
	relAt  time.Time // the latest lift
	back   int       // doublings of wcNetClean
	onAt   time.Time // when the cap was set
	share0 float64   // ... and the largest resend share around then
	judged bool      // the cap was judged (wcNetJudge)
	jcap   uint64    // the cap being judged (as set then)
	lvl    float64   // the path's resend level a cap did not lower (0: none)
	lvlAt  time.Time
	busyAt time.Time // since when the socket sends (zero: idle)
	quiet  bool      // the latest reading found it quiet (wcNetQuiet: due)
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

// due says whether the runner reads the socket at this wake (wrote: the
// bytes written to it so far, as its link meter counts them; 0 with none):
// always, but a quiet one (step) only every wcNetQuiet, or at the first wake
// that has wcNetQuietBytes written to it since the latest reading.
func (n *wcNet) due(now time.Time, wrote uint64) bool {
	k := len(n.pts)
	if !n.quiet || k == 0 {
		return true
	}
	p := n.pts[k-1]
	return wrote-p.wrote >= wcNetQuietBytes || now.Sub(p.at) >= wcNetQuiet-wcRampTick/2
}

// step takes one reading and says what to do with the socket's cap.
func (n *wcNet) step(in wcNetIn) wcNetOut {
	now := in.now
	// the delivery rate of this reading: tcpi_delivery_rate, or what was
	// acknowledged since the previous reading if that is less. Linux keeps
	// the latest sample that was not app-limited (or a higher one that
	// was): a sender held by its windows is app-limited, and after the path
	// slowed it read the old rate for seconds (the model's 1000 → 300 Mbit/s
	// under q20: the cap stayed at 1088 Mbit/s for 5 s, and the loss rule
	// drained the link); ACKs that come in clumps make the acknowledged
	// rate the higher one, and tcpi's stands
	dlvNow := in.dlv
	if k := len(n.pts); k > 0 {
		p := n.pts[k-1]
		dt := now.Sub(p.at)
		if n.quiet {
			// the first reading after a quiet spell (due): what it
			// acknowledged since came after it woke up, within the wake when
			// its meter woke the reading (a download's first write), within
			// the spell's last second when it woke up on its own (a path
			// back from a stall acknowledges what was written before it).
			// Taken over one wake, as a reading every wake takes it, and
			// never past tcpi's own rate; over the spell, the first rate
			// read up to 10 times too low and the cap set on it as much
			// lower (the dyn sweep's 300/300 rtt>2000 l0/s2, base: capped
			// at 8 Mbit/s where a reading every wake set 58, 29 after, 12.0
			// → 11.6 Mbit/s)
			dt = min(dt, wcRampTick)
		}
		if dts := dt.Seconds(); dts > 0 && in.acked >= p.acked {
			if a := float64(in.acked-p.acked) / dts; dlvNow <= 0 || a < dlvNow {
				dlvNow = a
			}
		}
	}
	n.pts = append(n.pts, wcNetPt{at: now, rtx: in.retrans, segs: in.segsOut, acked: in.acked, wrote: in.wrote})
	idleSpan := min(max(wcNetIdle, 2*in.srtt), wcNetIdleMax)
	keep := max(wcNetWin, idleSpan)
	cut := 0
	for cut < len(n.pts)-1 && now.Sub(n.pts[cut+1].at) >= keep {
		cut++
	}
	n.pts = n.pts[cut:]
	if dlvNow > 0 {
		n.dlvs = append(n.dlvs, wcRate{now, dlvNow})
	}
	cut = 0
	for cut < len(n.dlvs) && now.Sub(n.dlvs[cut].at) > wcNetSpan {
		cut++
	}
	n.dlvs = n.dlvs[cut:]
	if len(n.pts) < 2 {
		n.quiet = false
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
	ip := n.since(now, idleSpan)
	idle := in.acked-ip.acked < wcPaceIdle
	// quiet (due) once its decision is made, if it holds no cap: a full idle
	// span read, with next to nothing acknowledged or written over it
	still := now.Sub(ip.at) >= idleSpan && in.acked-ip.acked < wcNetQuietBytes && in.wrote-ip.wrote < wcNetQuietBytes
	defer func() { n.quiet = still && n.cap == 0 }()
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
		n.cap, n.rtxAt, n.onAt, n.share0, n.judged, n.jcap = target, now, now, share, false, target
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
	// unbound: the socket paces under the cap (its sender's own estimate
	// came down below it), still far past what it delivers
	unbound := in.pace > 0 && in.pace < (1-wcNetMove)*float64(n.cap) && in.pace > wcNetRatio*dlv
	if share >= loss && (unbound || in.minRTT > 0 && in.srtt-in.minRTT > wcNetQueue) {
		// still in trouble with a queue at the socket: the path, not the
		// cap, holds the delivery rate (a path that slowed down), and the
		// cap follows it down; so too a cap that does not bind (a policer
		// that comes mid-transfer builds no queue: the rig's 180 Mbit/s
		// one on 300/150 under the core left the cap at ~370 Mbit/s, set
		// from the rate before it, the sender pacing at 290 over 45
		// delivered). Otherwise it only rises: a sender held by its cap
		// and its windows delivers less (its window takes longer to send),
		// and following that down would ratchet it to a third of its rate
		// at ×1.5 (under random loss past wcNetLoss: the model lost 20% at
		// 3%)
		c = target
	}
	if share >= loss && last > 0 && k*last < wcNetPaid*float64(c) {
		// stale: what the path delivered over the last second is under half
		// of what the cap was set for, while the resends go on. A policer that
		// comes mid-transfer builds no queue, and while the sender's own pace
		// (a window-limited BBR's STARTUP: ~3.9 times the highest rate it
		// ever delivered) stays past the cap, the cap binds: neither rule
		// above moved it, and it stayed at wcNetK times the rate from before
		// the policer. The bursts at that pace overflowed the policer's
		// bucket at any window, the judge then found the cap unpaid against
		// the smaller trouble that had set it off, lifted it and took the
		// policer's loss for the path's level (wcNetLvlTTL), and the core's
		// own drain to the floor read the loss that went on as the path's
		// (lossTestDone). In the link model (TestWcoreS2RedPolicer: 1000
		// Mbit/s at 20 to 300 ms, a policer at 30, 60 or 90% coming 20, 30 or
		// 40 s in, 3 seeds; 270 runs) the loss rule drained the link under
		// the core and not under base in 20 runs, 10 with this; 1000/150
		// behind a policer at 30% resent 13.6% (field's reordering) and 15.6%
		// (none), 7.5% and 7.1% with this. The round-2 sweeps
		// (TestPaceNetSweep short, red1: 2240 and 738 cells four ways) are
		// unchanged; dyn's core-only drains 15 → 9 ("policer none>pol60"
		// 1 → 0, "pol60>pol30" 3 → 0, "pol90>pol40" 4 → 2). The ratchet the
		// rule above avoids needs no such guard: a sender held by its cap and
		// its windows delivers more than half of it (TestPaceNetEngageRelease:
		// 150 of 240 Mbit/s is held).
		c = uint64(math.Max(k*last, wcPaceFloor))
		//
		// (Against the cap the reading would set, not the one in force: once
		// the rule took the cap down, the next reading's "only rises", on the
		// span's rate from before the policer, took it back up and the rule
		// down again, 125 <-> 1000 Mbit/s each 100 ms for a second, 11
		// setsockopts; the post-merge review, TestPaceNetStaleCapFollows.
		// Short sweep: drains the same, runs slower than without the net
		// 123 -> 135 (base) and 149 -> 163 (core) of 2240; red1's and dyn's
		// drains the same (dyn's core-only 9 -> 8); TestWcoreS2RedPolicer
		// 10 -> 12 of 270 drained, within the model's spread. Seven rows of
		// the model's table move, those where the rule fired under random
		// loss or a rate change and the cap flipped (1000/150 l1 30.2 -> 28.4
		// Mbit/s, l3 13.3 -> 15.0, 300>50>300 145.5 -> 150.5; the passing
		// count the same). Refusing only the rise for a span after the rule
		// fired moves the same rows and slows more runs (150/178, red policer
		// 13), as does dropping the span's rates from before the last second
		// as it fires (161/193).)
	}
	if dlv <= 0 || math.Abs(float64(c)-float64(n.cap)) <= wcNetMove*float64(n.cap) {
		return wcNetOut{}
	}
	if c < n.cap && (unbound || float64(c) <= wcNetPaid*float64(n.jcap)) {
		// a cap that binds from now, or a new one (half the one judged or
		// less), is judged from now (wcNetJudge), not on the resends under
		// the one before: the rig's 180 Mbit/s policer at 15 s on 300/150
		// under the core had the cap at 512 (the stale delivery rate), down
		// to 165 with the queue at 17.6 s, lifted by the judge at 18.1 s on
		// the resends under 512, and 10 to 15% resent after
		n.onAt, n.share0, n.judged, n.jcap = now, share, false, c
	}
	n.cap = c
	return wcNetOut{act: wcNetMoveCap, cap: c, k: k, dlv: dlv}
}
