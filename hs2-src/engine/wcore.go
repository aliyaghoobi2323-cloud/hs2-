package engine

import (
	"math"
	"sort"
	"time"

	"github.com/xtaci/smux"
)

// wcore is the pure decision core of the window core (Phase Z5,
// docs/architecture/WINDOW_CORE.fa.md): one per link and per direction its
// end receives (the edge's downloads; the exit's uploads only with the lab
// knob WcoreUpload, until step 3). A measurement interval in, the link's
// window budget and each fast stream's ceiling out. The edge's core leaves
// OFF only once the exit sent an extended record (srtt and min_rtt != 0,
// sndSeen).
// It holds no locks, opens no sockets and never reads the clock (time and
// randomness come in through wcIn and c.rnd), so its whole behaviour is
// exercised by the link model in wcore_model_test.go; the actuator is the
// runner (wcore_run.go), which feeds it each link through wcBuilder.
//
// What it is for. A stream moves at most about its window per round trip,
// so the 2 MiB window caps one connection on a long path. A larger window is
// what overflowed the path in the first round (N4: three round trips of the
// reader's pace let the exit's BBRv1 reach its own 2·BDP cwnd and fill a
// shallow queue for as long as it ran), and a window sized from the pace
// (fin) shrinks under random loss, since data held behind a resend hole
// makes a turn of the window take longer than the round trip. So the core
// does not compute the window the path needs from a formula; it finds it:
//
//   - The knee is the least window budget B (for all the link's fast
//     streams together) that fills the path. STARTUP grows B by ×1.25–2
//     per interval (wcGears) while the link's delivery rate grows with it
//     and stops when the rate stops growing, the queue grows or loss
//     correlates with B. Between evaluations a ramp moves the published
//     budget up in small steps (a window-limited BBR sender sends new credit
//     at 2.89 times its rate), and loss on a clean path halves it at once.
//   - HOLD keeps B a little past the knee: K = R̂·(T_base+τ_hold) + H_step,
//     plus h·R̂·T_base, a standing queue of qTarget (15 ms). τ_hold is how
//     long data waits in the receiver behind holes (0 on a clean path, up to
//     a round trip under 0.1% random loss at 1 Gbit/s), learned from
//     intervals in which the windows were the limit: what fin lacked.
//   - PROBE adds probeQ (20 ms) of queue for one interval every 4–8 s and
//     keeps it only if the rate grew; a failed probe backs off to 32 s.
//   - DRAIN shrinks B below the knee for one interval when the queue grew
//     for two intervals, or with loss that correlates with it, or the path
//     slowed down.
//
// Loss that does not move with B (random loss) never backs B off: it is
// covered by τ_hold. Signals are relative: the queue is the window turnaround
// above its 60 s floor and above the level it had when HOLD began (q_ref),
// which absorbs a standing delay that resends add.
//
// The fast arm in smux (FitCore) applies the ceilings at each window update,
// holds growth back while turnaround runs past turnRef+qBrake, and grows no
// window while growOK is false; ceil 0 is the behaviour without the core.
type wcore struct {
	cfg wcConfig
	rnd func() float64

	state      wcState
	B          uint64    // budget decided (0 in OFF)
	bPub       uint64    // budget published: up to B by the ramp, down to it at once
	settling   bool      // the interval that ends at the next evaluation lets a change of B reach the sender
	lastAt     time.Time // the previous evaluation
	evalAt     time.Time // the next one
	acc        wcIn      // what came since the previous evaluation
	accOK      bool
	bHist      []wcRate // B over the last wcSndSpan (a sender record covers it)
	bCap       uint64   // a budget past which losses of our own came (0: none known)
	bCapAt     time.Time
	bGood      uint64   // the largest budget shown to pay: a STARTUP step the rate grew with, a probe that paid
	stopQ      bool     // a ramp stopped for the queue since the previous evaluation
	pubHist    []wcRate // the published budget at each wake of the last second
	lossDrain  bool     // a drain for loss: HOLD stays at its budget until the loss is gone
	lossDrainN int
	cuts       int  // halvings for loss between evaluations
	offN       int  // OFF: intervals in a row with steady fast streams
	sndSeen    bool // an extended record of the exit came (srtt and min_rtt != 0: an edge with a new exit on Linux 4.6 or later); the edge leaves OFF only then
	sndAny     bool // a record of the exit came (an old exit's too)
	sndAnyAt   time.Time
	sndLastAt  time.Time         // the latest record of the exit (blind: none for wcSndQuiet)
	qSnds      [wcSndHist]wcRate // q_snd of the latest records (r: seconds), newest first
	chgAt      time.Time         // the latest drain, cut or start: a record from before it judges no B of now
	nfPrev     int               // n_F of the previous evaluation
	slowN      map[uint32]int    // evaluations in a row a stream of F looked like a slow source
	idleN      map[uint32]int    // ... in which a stream of F was sent nothing
	ctrlOffs   wcMinWin          // pong − T_base while the link's own queue was empty (q_ctrl's floor)
	capPool    bool              // capped by the window pool (not by MaxStreamBuffer)
	holeyPrev  bool              // the previous evaluation's packets waited behind holes
	burstHeld  bool              // B is held where a burst after the sender's PROBE_RTT fits Q_cap
	blindN     int               // evaluations in a row that were blind (blind)
	// sndCarry: the resends of a record of the exit that came in the settle
	// part of a HOLD or DRAIN interval (settled judges no loss there), for
	// the next measured interval to judge; sndCarryAt: when it came
	sndCarry   wcSnd
	sndCarryAt time.Time
	// the previous runner wake, and how long the packets of the wakes
	// before waited behind holes (ooo/segs_in of each one's length) and how
	// many did (fastStop)
	wakeAt    time.Time
	holeWakes []wcHoleWake // the wakes before, newest last (lossHole: T_base/4 of them)
	holeSeen  bool         // a loss's hole since the previous evaluation (lossHole)
	holeMax   float64      // ... the largest share of a wake's packets behind one
	policedAt time.Time    // the latest policer's loss (policerLoss)
	cleanRecs int          // the exit's latest records in a row that showed no queue and no resends
	// prov: what holes alone did to B where the exit's records judge loss
	// (sndJudges), until one does (judgeCut), with what the core had before,
	// to take back; weak: one that stood on a small loss of the sender's
	// (judgeCut); oooRo: the largest share of a wake's packets behind holes
	// that a record showed were no loss (the path reorders packets): holes
	// within it cut no more, until wcQcapTTL after the latest such record
	// (oooRoAt; roLevel), a route change or OFF
	prov, weak wcProv
	oooRo      float64
	oooRoAt    time.Time
	// movedAt: the latest drain for a queue or a slower path, whether it
	// moved B or found it at the floor (undoCut takes back nothing past it)
	movedAt time.Time

	// paths
	tBase    time.Duration // T_base: the link socket's min_rtt (or the fallbacks)
	srttMins wcMinWin      // fallback: least srtt over 60 s
	turnMins wcMinWin      // turnBase: least window turnaround over 60 s
	tbOff    time.Duration // turnBase − T_base while both agreed (route change)
	tbOK     bool          // ... they agreed at least once
	routeLog bool
	// the exit's srtt by its latest record, and when (sndAllows)
	sndRTT   time.Duration
	sndRTTAt time.Time
	// after a route change the exit's min_rtt keeps the old floor too:
	// qSndRaw is srtt − min_rtt of its records (their floor over 60 s in
	// qSndMins), and sndOff what of it is the longer path (q_snd is the rest)
	qSndMins wcMinWin
	sndOff   time.Duration

	// rate
	rhat    []wcRate // R_link samples for R̂ (windowed max)
	tau     time.Duration
	tauOK   bool
	spread  time.Duration // turnLast − turnMin of the fast streams (HOL noise), smoothed
	inF     map[uint32]bool
	fEmpty  int
	prevWin map[uint32]uint32

	// queue references, set by the first measured HOLD interval
	refPending bool
	qRef       time.Duration
	qSndRef    time.Duration
	qSndRefOK  bool
	qOwnRef    time.Duration
	rLinkHold  float64
	holdR      [2]float64 // R_F of the last two HOLD intervals, newest first
	rAvg       float64    // R_F smoothed over the evaluations (wcRAvgAlpha): the knee's rate with other cores on the peer
	holdN      int
	growthN    int
	wlN        int
	pongHi     int
	qSnd       time.Duration
	qSndAt     time.Time

	// loss: each level with the budget it was measured at
	sndRef, oooRef wcLossRef
	lossTest       wcLossSrc // a drain is testing whether a jump of loss was ours (0: none)
	lossTestAt     time.Time
	lossTestB      uint64
	lossyN         int
	lossy          bool
	qcap           time.Duration
	qcapAt         time.Time
	qcapOK         bool
	parkHold       int
	pathFull       bool
	capped         bool

	// STARTUP
	fullR, prevR float64
	prevB        uint64
	flat         int
	gear         int // index into wcGears: the next step's growth
	kPrev        uint64
	kPrevAt      time.Time

	// a refresh: B a queue target under HOLD for an interval, so the queue
	// empties and the sender's BBR sees its round trip without it
	refreshAt  time.Time
	refreshing bool

	// PROBE
	probeAt   time.Time
	backoff   int
	probeFrom uint64
	probeBase float64
	probeQ0   time.Duration // q_ref as the probe began (the queue it is judged against)
	probeOwn0 time.Duration // ... and q_ownRef
	probeWins int           // probes in a row none of whose window queued

	ceils map[uint32]uint32
	cnt   wcCounters
	last  wcMeas // the latest interval's measurements (lab trace)

	// peer: what the cores of the links to the same peer share (wcPeer; nil:
	// none, the link is alone)
	peer *wcPeer
}

// wcRAvgAlpha: R_F's smoothing for the knee with other cores on the peer
// (rAvg).
var wcRAvgAlpha = 0.25

// wcLossRef is a loss level and the budget it was measured at: loss past
// it at a larger budget came with B (congestive); at the same budget it is
// either the path turning lossy or losses that keep themselves going (a
// burst overflows the queue, the data held behind the hole is released at
// once, its window updates leave together and the sender sends their credit
// as one more burst), and a drain tells which.
type wcLossRef struct {
	v  float64
	b  uint64
	ok bool
}

type wcLossSrc uint8

const (
	wcSrcNone wcLossSrc = iota
	wcSrcSnd            // the exit's resend share (edge, a record each ~2 s)
	wcSrcOoo            // Rcv_ooopack of the link's own socket
)

// note keeps the level of an interval that was not judged congestive.
func (r *wcLossRef) note(v float64, b uint64, alpha float64) {
	switch {
	case !r.ok:
		r.v, r.b, r.ok = v, b, true
	case float64(b) <= 1.05*float64(r.b):
		r.v += alpha * (v - r.v)
		r.b = b
	default:
		// as clean at a larger budget: the level holds there too
		r.v += alpha / 3 * (v - r.v)
		r.b = b
	}
}

// past says whether v is past the level by max(+abs, ×rel).
func (r *wcLossRef) past(v, abs float64) bool {
	return r.ok && v >= math.Max(r.v+abs, wcLossCongRel*r.v)
}

type wcState uint8

const (
	wcOff     wcState = iota // no ceilings: the behaviour without the core
	wcStartup                // growing B while the rate grows with it
	wcHold                   // B a little past the knee
	wcProbe                  // B + probeQ of queue for one interval
	wcDrain                  // B below the knee for one interval
)

func (s wcState) String() string {
	switch s {
	case wcStartup:
		return "startup"
	case wcHold:
		return "hold"
	case wcProbe:
		return "probe"
	case wcDrain:
		return "drain"
	default:
		return "off"
	}
}

type wcSide uint8

const (
	wcEdge wcSide = iota // receives downloads: has the exit's stats record and the control ping
	wcExit               // receives uploads: only its own signals
)

// wcConfig is fixed per link: the stream window sizes of the session's smux
// config and the stage.
type wcConfig struct {
	side   wcSide
	stage  int    // 1: no ceiling below need; 2: down to wcStage2Min once the path is full
	need   uint32 // smux NeedStreamBuffer (2 MiB)
	maxWin uint32 // smux MaxStreamBuffer (16 MiB)
	minWin uint32 // smux MinStreamBuffer (64 KiB)
}

// Constants of the core (§5.2), variables so the calibration (step 0) and
// the lab knobs can set them.
var (
	wcTickMin        = 400 * time.Millisecond // an interval (settle + measure) is at least this
	wcTickMax        = 2 * time.Second        // ... and at most this
	wcSettleRTTs     = 1.5                    // from a change of B to the first snapshot
	wcMeasureRTTs    = 2.5                    // the measuring part of an interval ...
	wcMeasureMin     = 300 * time.Millisecond // ... at least this
	wcQTarget        = 15 * time.Millisecond  // standing queue HOLD keeps (Q_cap/3 if less)
	wcProbeQ         = 20 * time.Millisecond  // queue a probe adds on a full path (Q_cap/2 if less)
	wcQTargetExit    = 10 * time.Millisecond  // the exit side has no sender record: more careful
	wcProbeQExit     = 15 * time.Millisecond
	wcHMin, wcHMax   = 0.03, 0.25
	wcGpMin, wcGpMax = 1.04, 1.25
	wcStartFast      = 0.6  // the rate grew by this share of B's growth: next step ×2
	wcFullGrowth     = 0.25 // STARTUP: the rate must grow this much ...
	wcFullRounds     = 3    // ... within this many intervals, or the pipe is full
	wcPayShare       = 1.0 / 3
	wcPayMin         = 0.03
	wcSuccStartup    = 2.0 / 3 // a probe that paid this share of what it could: back to STARTUP
	wcDqMin          = 5 * time.Millisecond
	wcDqShare        = 0.1
	wcLossCongAbs    = 0.003 // congestive (the exit's resends, OFF's do-no-harm rule): +0.3 points ...
	// wcLossCongMin, wcLossCongSig: a record's resend share is past its level
	// by max(wcLossCongMin, wcLossCongSig·sqrt(level/segments)) and ×2 (one
	// record's spread under 0.1% random loss is 0.015 to 0.025 points: 0.3
	// points was 12σ at 300 Mbit/s and under 1σ on a record of a few
	// hundred segments)
	wcLossCongMin = 0.001
	wcLossCongSig = 5.0
	wcLossCongRel = 2.0  // ... or twice the HOLD level, whichever is more
	wcOooCongAbs  = 0.01 // the same on Rcv_ooopack (a share of packets, not of losses)
	wcOooClean    = 0.10 // ooo is a congestion signal only on a path that is otherwise clean
	// wcOooHole, wcOooMinSegs: between evaluations, packets behind holes
	// past their level cut B only where the holes of the wake and the one
	// before kept packets waiting wcOooHole of T_base or more, with
	// wcOooMinSegs segments or more behind them. A loss's hole stays open a
	// round trip and more; a path that reorders packets by a few ms opens
	// holes that short (Linux's RACK calls a packet lost only once it is
	// reordered by more than min_rtt/4), and each was a cut: 0.05% of the
	// packets held 2 ms took 1000/100 from 736 to 97 Mbit/s (red team,
	// round 1).
	wcOooHole    = 0.25
	wcOooMinSegs = 16
	// wcRoMargin, wcRoMax: a cut for holes that the exit's record took back
	// (undoCut) sets the path's reordering level (oooRo) to this many times
	// the largest share of a wake's packets behind its holes, at most
	// wcRoMax: a wake whose holes were a loss's is mostly behind them (at
	// 72 ms, 90% of a 100 ms wake waits on a hole a round trip and RACK's
	// quarter of one long), the field's reordering kept up to 49% of a 0.5 s
	// span waiting. On the field's profile at 1000/72 (wcore_s2_reorder_test,
	// RACK, the means of 3 seeds) the core carried 614 Mbit/s at ×1, 670 at
	// ×1.25, 665 at ×1.5, 678 at ×2 (base 174, the core without reordering
	// 798, the core before these rules 280); behind a 60% policer on the
	// same reordering path it resent 0.14 to 0.49% at ×1.25 over 4 seeds
	// (base windows 0.00–0.02%, the core before the rules 0.00–0.08%), no
	// 2 s window past 2.5% (the loss rule drains at 12%).
	wcRoMargin = 1.25
	wcRoMax    = 0.75
	// wcPoliceQ, wcPoliceBackoff: a loss of ours whose record from the exit
	// shows no more queue than wcPoliceQ at its socket is a policer's
	// (policerLoss): its loss point (bCap) holds for wcQcapTTL, not until a
	// few intervals without a queue (a policed path shows none below the
	// policer's rate), probes back off this far at once, a loss that goes on
	// at the budget a drain left is a loss point too, and the drain goes to
	// the floor. What the windows announced before a loss is sent once its
	// hole fills, past the policer's bucket again: in the model a 60% policer
	// at 300 ms lost whole bursts again after each loss, the sender timed out
	// and crawled at a few Mbit/s for 20 s (red team, round 1: 4 to 8 of 96
	// runs a streak of resends past lossFrac, none without the core).
	wcPoliceQ       = wcDqMin
	wcPoliceBackoff = 3
	// wcLongRTT: past this T_base (paths a DPI box holds back, satellite
	// links) the core is outside what it was calibrated on: Δq (T_base/10)
	// and q_ctrl's threshold (T_base/3) outgrow a shallow queue, a measuring
	// part of T_eval's 2 s is under a round trip and two, and a window
	// update's step outgrows the queue at the sender's STARTUP pacing. In
	// the red team's sweep 7 of 24 runs at 500 ms and 39 of 120 at 600 ms to
	// 2 s drained or resent past base + 3 points. There the edge grows B (a
	// STARTUP step, a probe) only on a fresh record of the exit that shows no
	// queue (q_snd up to wcDqMin) and no new resends (longGrow), and leaves
	// OFF only after wcLongClean such records in a row; a core with no such
	// record (the exit's side, an edge with an old exit) stays OFF
	// (longBlind). Past wcLongGear STARTUP takes its smallest gear (×1.25)
	// only: at 600 ms to 2 s ×1.5 left 19 of 120 runs, ×1.25 5; at 500 ms
	// ×1.25 on records 2.5 s apart took the table's 1000/500 q50 to 56 Mbit/s
	// in its 24 s (target 108.5; ×1.5: 133). The rest is the sender's: a
	// window-limited BBR's STARTUP pacing only rises, and on a shallow queue
	// a window much past the one it started at loses a share of each round
	// trip's flight (TestWcoreFixLongRTTModel). In the sweep 0 of 48 runs at
	// 450 and 500 ms and 5 of 120 at 600 ms to 2 s stay, and the core keeps
	// about half of its gain there (base 12.8, core 46.7 before, 30.4 Mbit/s
	// now). (From 400 ms one run at 400 that resends 3.7–3.9% from before
	// these rules went too, but the mean there fell from 128 to 83 Mbit/s.)
	wcLongRTT      = 450 * time.Millisecond
	wcLongClean    = 3
	wcLongGear     = 600 * time.Millisecond
	wcLossRand     = 0.0005
	wcLossRandN    = 3
	wcSndSpan      = 2500 * time.Millisecond // a record covers the exit's last stats tick
	wcSndQuiet     = 2 * wcSndSpan           // no record for this long: the records stopped (blind)
	wcRhatWin      = 10 * time.Second
	wcRhatMinN     = 8
	wcTauAlpha     = 0.25
	wcRefAlpha     = 0.25
	wcProbeMin     = 4 * time.Second
	wcProbeMax     = 8 * time.Second
	wcProbeRTTs    = 10
	wcProbeBackMax = 32 * time.Second
	wcQcapTTL      = 10 * time.Minute
	wcTurnWin      = 60 * time.Second
	wcKPrevTTL     = 30 * time.Second
	wcOffAfter     = 3
	wcCtrlQMin     = 30 * time.Millisecond
	wcCtrlQShare   = 1.0 / 3
	wcRateDrop     = 0.3
	wcHoldSlack    = 0.85
	wcStage2Min    = uint32(256 << 10)
	wcParkHold     = 2
	wcHitShare     = 0.5
	wcLagShare     = 0.25
	wcUseShare     = 0.9  // a window is in use: R·turnaround ≥ this share of it less half a step
	wcPeerShare    = 0.25 // a stream joins F on use alone only at this share of the fastest
	wcDemandGain   = 1.25
	wcResettle     = 0.05 // a change of B by more than this share settles first
	// wcRampQ, wcRampShare: a ceiling rises by at most the larger of this
	// much queue at R̂ and this share of the budget per runner wake
	// (wcRampTick). A window-limited BBR sender stays in its STARTUP (its
	// rounds are app-limited) and sends new credit at 2.89 times its rate:
	// a ceiling that rose in one step was a burst into the queue as large as
	// the rise, and on a shallow queue it was the loss that started a run
	// of them (wcLossRef).
	wcRampQ     = 15 * time.Millisecond
	wcRampShare = 1.0 / 8
	wcRampTick  = 100 * time.Millisecond
	wcCapBack   = 0.9              // past a budget that brought losses of our own, HOLD stays at this share of it
	wcCapStale  = 15 * time.Second // a cap confirmed by losses this recently is no sign of a faster path
	// wcDqOwn: where data waits behind holes (Rcv_ooopack past wcOooClean),
	// the window turnaround holds that wait (its slope under 0.1% loss was
	// three times the queue's, its noise 8 to 130 ms) and the queue is judged
	// by the link socket's own srtt above T_base (q_own, noise 2.5 to 5 ms)
	// growing past its reference by this much.
	wcDqOwn = 10 * time.Millisecond
	// wcOffLoss: the base windows lose this share (the exit's record) or
	// more: the core stays OFF (do no harm, stage 1).
	wcOffLoss = 0.01
	// wcPastKnee: the base windows hold this many times what their rate
	// needs in a round trip and a step (R_F·T_base + H_step) or more: they
	// are past the knee already. With no record of the exit to tell a queue
	// from data held behind holes (the exit's side, an old exit), the core
	// stays OFF (stage 1). (A window held by its window keeps about
	// R_F·T_base + H_step in use: 1.0 to 1.1; 100/100 with 2 MiB, 1.3.)
	wcPastKnee = 1.25
	// wcDrainShrink: a drain takes B to at most this share of where it was.
	wcDrainShrink = 0.9
	// wcDipShare: an interval whose rate fell under this share of R̂ (the
	// sender's PROBE_RTT, 200 ms of 4 packets) teaches τ_hold nothing.
	wcDipShare = 0.5
	// wcBurstShare: after a PROBE_RTT the sender sends all of its window at
	// its STARTUP pace (3.9 times the path's rate): about this share of it
	// becomes queue at the bottleneck (the rig: 0.5 to 0.73). With Q_cap
	// known and wcBurstShare > 0, B is held where that burst fits it. 0 (no
	// such cap) by default: the receiver's Q_cap reads a burst's loss far
	// under the queue it overflowed (the rig's 300/150 behind a 50 ms queue:
	// q_own and q_snd at eight cuts, median 13.6 ms), and the cap held B at
	// the floor there (the model: 176 → 103 Mbit/s). Lab knob
	// HS2_TUNE_WCORE_BURST.
	wcBurstShare = 0.0
	// wcQcapMin: the least Q_cap learned (a policer's loss comes with no
	// queue at all).
	wcQcapMin = 10 * time.Millisecond
	// wcSlowN: evaluations in a row that make a stream of F a slow source.
	wcSlowN = 2
	// wcIdleKeep: evaluations a stream of F stays in F while its sender
	// sends nothing and its reader is not behind (the sender's PROBE_RTT).
	wcIdleKeep = 3
	// wcCtrlCalm, wcCtrlOffWin: q_ctrl's floor is T_base plus the least
	// pong − T_base seen while q_own was at most wcCtrlCalm, over
	// wcCtrlOffWin (the control channel's own rttMin is the least of eight
	// pongs, ~24 s: a standing queue becomes its floor).
	wcCtrlCalm   = 3 * time.Millisecond
	wcCtrlOffWin = 5 * time.Minute
	// wcRefreshMin, wcRefreshMax: how often HOLD lets its standing queue
	// empty for an interval. BBRv1 keeps the least round trip of 10 s and,
	// when no sample reached it in that time, sends nothing past 4 packets
	// for 200 ms (PROBE_RTT); a sender held by its window a standing queue
	// past the knee never sees the bare round trip, and after its PROBE_RTT
	// it sends all the credit it gathered at its STARTUP pacing (≈3.9× the
	// path on the rig). A refresh does NOT keep PROBE_RTT away: Linux keeps
	// min_rtt in µs and takes only a strictly lower sample, so an emptied
	// queue renews it now and then at best (the staler the floor, the less
	// often); the core must take the burst after it (noteTau skips the dip),
	// not count on avoiding it. The refresh stays for the knee's estimate.
	wcRefreshMin = 7 * time.Second
	wcRefreshMax = 9 * time.Second
)

// wcSndHist: the do-no-harm rule takes the largest q_snd of this many of
// the exit's latest records (one record's spread is 3 to 17 ms).
const wcSndHist = 3

// wcGears are STARTUP's steps of B: from OFF it starts at ×1.5, after a
// probe that paid at ×1.25; each interval in which the rate grew with B
// moves one gear up (to ×2), any other one down.
var wcGears = [...]float64{1.25, 1.5, 2}

// wcStream is one stream of the link over the interval (wcBuilder).
type wcStream struct {
	id       uint32
	read     uint64        // bytes its reader took
	upd      uint32        // window updates sent
	hit      uint32        // ... of them in cycles where the sender used all of the window
	lag      uint32        // ... in cycles whose reader left over StreamLagTarget/4 unread
	turnMin  time.Duration // least window turnaround (0: no sample)
	turnLast time.Duration // latest window turnaround (0: none)
	win      uint32        // its window now
	braked   bool          // checkLag's brake is on
	// turns: each runner wake's turnMin and turnLast over the interval
	// (wcMerge): q_turn is their 25th percentile, not the interval's least
	turns []time.Duration
}

// wcSnd is the edge's view of the exit's socket (the 96-byte stats record).
type wcSnd struct {
	fresh        bool // a new record came during the interval; the rest is from it
	srtt, minRTT time.Duration
	retrans      uint64 // resent segments since the previous record
	segsOut      uint64 // data segments sent since the previous record
}

// wcIn is one interval of the link as the runner (or the model) saw it:
// counters as deltas since the previous step.
type wcIn struct {
	now       time.Time
	minRTT    time.Duration // the link socket's tcpi_min_rtt (0: unknown)
	srtt      time.Duration // the link socket's tcpi_rtt (0: unknown)
	segsIn    uint64        // data segments received
	ooo       uint64        // ... that arrived behind a hole (Rcv_ooopack)
	oooOK     bool
	linkBytes uint64 // bytes the session received
	streams   []wcStream
	parks     uint32          // the session's reader waited for buffer space
	brakes    uint32          // checkLag braked a stream
	poolFree  int64           // what the window pool has left (< 0: no pool)
	poolShort uint32          // windows the pool gave less than asked
	ctrlMin   time.Duration   // edge: the control channel's rttMin (0: none)
	pongs     []time.Duration // edge: pong round trips in the interval, oldest first
	snd       wcSnd           // edge: the exit's record
	apProbing bool            // edge: the autopilot is probing the pool size (freeze)
}

type wcCeil struct {
	id   uint32
	ceil uint32 // 0: no ceiling (the behaviour without the core)
}

type wcEventKind uint8

const (
	wcEvFilled wcEventKind = iota + 1 // STARTUP ended (or the knee moved by > 25%)
	wcEvDrain
	wcEvLossy
	wcEvPool
	wcEvCapped
	wcEvRoute
	wcEvUndo // a cut for holes taken back: the exit's record showed no loss (undoCut)
)

type wcDrainWhy uint8

const (
	wcWhyQueue wcDrainWhy = iota
	wcWhyLoss
	wcWhyRate
)

type wcEvent struct {
	kind wcEventKind
	why  wcDrainWhy
	sig  wcSig  // a drain: the signal that set it off (q its value past its reference)
	from uint64 // B before
	B    uint64
	n    int
	R    float64 // bytes/s
	T    time.Duration
	q    time.Duration
	loss float64
	pool bool // capped: by the window pool
}

// wcSig is the signal a drain came from.
type wcSig uint8

const (
	wcSigTurn wcSig = iota // the window turnaround (q_turn above q_ref)
	wcSigOwn               // the link socket's own srtt (q_own above its reference: data waits behind holes)
	wcSigSnd               // the exit's record (q_snd above its reference)
	wcSigCtrl              // the control ping (q_ctrl)
	wcSigLoss              // the exit's resends (a record)
	wcSigOoo               // packets behind holes (Rcv_ooopack)
	wcSigRate              // the path slowed down
)

// wcCounters are the link's totals (linkStallNote).
type wcCounters struct {
	grow, probeOK, probeFail     int
	drainQ, drainLoss, drainRate int
	undo                         int // what holes alone did taken back (undoCut): B with its loss point, or the loss point alone
	poolShort                    int
	peakB                        uint64
	peakR                        float64
	lossy                        time.Duration
}

// wcOut is what the actuator publishes until the next step.
type wcOut struct {
	state   wcState
	B       uint64
	ceils   []wcCeil // one per input stream
	turnRef time.Duration
	qBrake  time.Duration
	growOK  bool
	next    time.Time
	events  []wcEvent
}

type wcRate struct {
	at time.Time
	r  float64
}

// wcMinWin is a windowed minimum kept in six buckets of a sixth of the
// window each. Its epochs count from its first sample by the monotonic clock
// (now.Sub), as the rest of the core counts time: on the wall clock
// (UnixNano) a step of it (NTP, a VM's migration) took the whole window
// away, and the next sample alone, with the queue in it, was turnBase.
type wcMinWin struct {
	b  [6]time.Duration
	ep [6]int64  // the bucket's epoch ((now − t0) / (win/6))
	t0 time.Time // the first sample's time
}

// epoch is now's bucket epoch and its bucket.
func (w *wcMinWin) epoch(now time.Time, win time.Duration) (int64, int) {
	if w.t0.IsZero() {
		w.t0 = now
	}
	e := int64(now.Sub(w.t0) / (win / 6))
	return e, int((e%6 + 6) % 6)
}

func (w *wcMinWin) add(now time.Time, v, win time.Duration) {
	if v <= 0 {
		return
	}
	e, i := w.epoch(now, win)
	switch {
	case w.ep[i] != e || w.b[i] == 0:
		// (an empty bucket too: the first epoch is 0, as an unset one)
		w.b[i], w.ep[i] = v, e
	case v < w.b[i]:
		w.b[i] = v
	}
}

func (w *wcMinWin) min(now time.Time, win time.Duration) time.Duration {
	if w.t0.IsZero() {
		return 0
	}
	e, _ := w.epoch(now, win)
	m := time.Duration(0)
	for i, v := range w.b {
		if v > 0 && e-w.ep[i] < 6 && e-w.ep[i] >= 0 && (m == 0 || v < m) {
			m = v
		}
	}
	return m
}

func newWcore(cfg wcConfig, rnd func() float64) *wcore {
	if cfg.stage < 1 {
		cfg.stage = 1
	}
	return &wcore{cfg: cfg, rnd: rnd, inF: map[uint32]bool{}, prevWin: map[uint32]uint32{}, ceils: map[uint32]uint32{},
		slowN: map[uint32]int{}, idleN: map[uint32]int{}}
}

// wcStep is the window update step of a window w (smux.StepOf, which the
// fast arm uses): half of it, or, past need, need/2 or an eighth of it if
// that is more, never more than half.
func wcStep(need, w uint32) uint32 { return smux.StepOf(need, w) }

// wcSplit shares budget among streams max-min fairly by window (the streams
// of one link see one round trip, so equal windows are equal rates): each
// gets min(its demand, a common level), the level chosen so the shares add
// up to budget, or every demand is met. Demands are first held within
// [lo, hi]; with budget ≥ len·lo every share is at least lo, and the shares
// never add up to more than budget.
func wcSplit(budget uint64, demand []float64, lo, hi uint32) []uint32 {
	n := len(demand)
	out := make([]uint32, n)
	if n == 0 {
		return out
	}
	d := make([]float64, n)
	order := make([]int, n)
	for i, v := range demand {
		d[i] = math.Min(math.Max(v, float64(lo)), float64(hi))
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return d[order[a]] < d[order[b]] })
	left := float64(budget)
	for k, i := range order {
		share := math.Floor(math.Min(d[i], left/float64(n-k)))
		if share < 0 {
			share = 0
		}
		out[i] = uint32(share)
		left -= share
	}
	return out
}

// wcMeas is what one interval measured.
type wcMeas struct {
	dt       float64 // seconds
	f        []int   // indexes of the fast streams (F) in the input
	sameF    bool    // F is the set it was in the previous interval
	rF       float64 // bytes/s read by F
	rLink    float64 // bytes/s the session received
	rOther   float64
	wF       float64 // Σ window of F now
	wPrev    float64 // ... at the previous snapshot
	hStep    float64 // Σ step/2 of F (H_step)
	hit      float64 // share of F's window updates in window-limited cycles
	qTurn    time.Duration
	qOK      bool
	qOwn     time.Duration
	qOwnOK   bool
	qSnd     time.Duration
	qSndOK   bool
	snd      float64 // the exit's resend share (fresh record)
	sndOK    bool
	sndSegs  uint64    // ... over this many segments
	sndAt    time.Time // when that record came (a record carried from a settle part came before at)
	ooo      float64
	oooOK    bool
	oooLoss  bool    // a loss's hole came in the interval (lossHole)
	holeMax  float64 // ... the largest share of a wake's packets behind one
	spread   time.Duration
	spreadOK bool
	ctrlUp   bool
	ctrlQ    time.Duration // the longest pong past q_ctrl's floor
	holey    bool          // data waits behind holes (ooo ≥ wcOooClean): q_own judges the queue
	qSnd3    time.Duration // the largest q_snd of the latest wcSndHist records (with other cores on: the median of every link's)
	qSnd3OK  bool
	grp      bool // another core of the peer is on (grouped)
	// qPath: with other cores on the peer (grp), the path's queue by the
	// exit's records of every link to it (pathQ): what the links' cores hold
	// there together
	qPath   time.Duration
	qPathOK bool
	// qStand: with other cores on the peer, the lower quartile of the
	// records of the last two spans (what of the queue stands)
	qStand   time.Duration
	qStandOK bool
	idleF    bool // a stream of F was sent nothing (kept in F, nothing judged)
	at       time.Time
}

func (c *wcore) dq() time.Duration {
	return max(wcDqMin, time.Duration(float64(c.tBase)*wcDqShare))
}

func (c *wcore) qTarget(now time.Time) time.Duration {
	q := wcQTarget
	if c.cfg.side == wcExit {
		q = wcQTargetExit
	}
	if c.qcapKnown(now) && c.qcap/3 < q {
		q = c.qcap / 3
	}
	return q
}

func (c *wcore) probeQ(now time.Time) time.Duration {
	q := wcProbeQ
	if c.cfg.side == wcExit {
		q = wcProbeQExit
	}
	if c.qcapKnown(now) && c.qcap/2 < q {
		q = c.qcap / 2
	}
	return q
}

// longRTT: T_base is past wcLongRTT.
func (c *wcore) longRTT() bool {
	return c.tBase >= wcLongRTT
}

// longBlind: past wcLongRTT with no record of the exit's socket to grow on
// (the exit's side; an edge whose exit sends no TCP_INFO in its record, or
// none yet): the core stays OFF.
func (c *wcore) longBlind() bool {
	return c.longRTT() && (c.cfg.side == wcExit || !c.sndSeen)
}

// longGrow says, past wcLongRTT, whether the interval has a fresh record of
// the exit (judged: it came in the interval, after the latest drain, cut or
// start) and whether it shows no queue at the exit's socket and no new
// resends (clean). Under wcLongRTT every interval is judged clean.
func (c *wcore) longGrow(m wcMeas) (judged, clean bool) {
	if !c.longRTT() {
		return true, true
	}
	if c.cfg.side != wcEdge || !c.sndSeen || !m.sndOK || c.sndStale(m.sndAt) {
		return false, false
	}
	return true, m.qSndOK && m.qSnd <= wcDqMin && m.snd == 0
}

// policed: a policer's loss (noteQcap) came within wcQcapTTL.
func (c *wcore) policed(now time.Time) bool {
	return !c.policedAt.IsZero() && now.Sub(c.policedAt) < wcQcapTTL
}

func (c *wcore) qcapKnown(now time.Time) bool {
	return c.qcapOK && now.Sub(c.qcapAt) < wcQcapTTL
}

// interval is the settle and measure part of an evaluation interval.
func (c *wcore) interval() (settle, measure time.Duration) {
	t := c.tBase
	settle = time.Duration(wcSettleRTTs * float64(t))
	measure = max(time.Duration(wcMeasureRTTs*float64(t)), wcMeasureMin)
	if tot := settle + measure; tot < wcTickMin {
		measure = wcTickMin - settle
	} else if tot > wcTickMax {
		settle = time.Duration(float64(settle) * float64(wcTickMax) / float64(tot))
		measure = wcTickMax - settle
	}
	return settle, measure
}

// cmin is the least ceiling of a fast stream: need in stage 1; in stage 2,
// once the core found the path full, down to max(4·MinStreamBuffer, 256 KiB).
func (c *wcore) cmin() uint32 {
	if c.cfg.stage >= 2 && c.pathFull {
		return min(max(4*c.cfg.minWin, wcStage2Min), c.cfg.need)
	}
	return c.cfg.need
}

// step takes what the runner saw since its previous call. The runner calls
// it at out.next (on its 100 ms wake): between evaluations only to move the
// published budget up the ramp; at an evaluation with all that came since
// the previous one.
func (c *wcore) step(in wcIn) wcOut {
	now := in.now
	hole := c.wakeHoles(in)
	lossHole := c.lossHole(in, hole)
	c.holeSeen = c.holeSeen || lossHole
	defer func() {
		c.wakeAt = now
		c.holeWakes = append(c.holeWakes, wcHoleWake{hole, in.ooo})
		if n := len(c.holeWakes) - c.holeSpan(); n > 0 {
			c.holeWakes = c.holeWakes[:copy(c.holeWakes, c.holeWakes[n:])]
		}
	}()
	if c.accOK {
		wcMerge(&c.acc, in)
	} else {
		c.acc = wcCopy(in)
		c.accOK = true
	}
	// a record of the exit that came in this wake judges what holes before
	// it did to B: it stands, or is taken back (judgeCut)
	jev := c.judgeCut(in)
	share, holeLoss := c.holeLoss(in, lossHole)
	if lossHole {
		c.holeMax = math.Max(c.holeMax, share)
	}
	judged := c.sndJudges(now)
	if ro := c.roLevel(now); judged && ro > 0 && share <= ro {
		holeLoss = false // within the path's reordering level (undoCut)
	}
	if judged && holeLoss && !c.prov.at.IsZero() {
		// a hole past the level while a reaction to holes waits for its
		// record (the drain holds B: no cut, no verdict): the record judges
		// holeLag after the latest one (judgeCut). Only the holes that cut or
		// gave a verdict were counted, and a loss right after a burst of
		// reordering was taken back by a record that came before its resend
		// could show (TestWcoreS2RedHolesAfterCut). (Not their share: the
		// level learns what cut. With it, the loss bursts of
		// TestWcoreS2RedBursts had 13 cuts taken back around drops of the
		// link's own packets, against 7 without it and the angle as the judge
		// took it.)
		c.prov.last = now
	}
	if !c.evalAt.IsZero() && now.Before(c.evalAt) {
		// (where the exit's records come, a cut for holes waits for one:
		// provNote)
		p, prov := c.provSnap(now, judged && holeLoss)
		n := c.cnt.drainLoss
		c.fastStop(in, holeLoss)
		if prov && c.cnt.drainLoss > n {
			c.provNote(p, now, share)
		}
		c.ramp()
		return c.out(c.acc, now, jev)
	}
	in, c.accOK = c.acc, false
	// the record that came in this interval (a carried one was counted in
	// cleanRecs as it came)
	rec := in.snd
	carryAt := time.Time{}
	if c.sndCarry.fresh {
		// (its srtt is left out: notePaths took its q_snd as it came)
		if in.snd.fresh {
			in.snd.retrans += c.sndCarry.retrans
			in.snd.segsOut += c.sndCarry.segsOut
		} else {
			in.snd = c.sndCarry
		}
		carryAt, c.sndCarry = c.sndCarryAt, wcSnd{}
	}
	stopQ := c.stopQ
	c.stopQ = false
	ev := jev
	dt := 0.0
	if !c.lastAt.IsZero() {
		dt = now.Sub(c.lastAt).Seconds()
	}
	c.lastAt = now
	c.notePaths(in, &ev)
	if in.snd.fresh {
		c.sndLastAt = now
	}
	if rec.fresh {
		if !c.sndAny {
			c.sndAny, c.sndAnyAt = true, now
		}
		// (q_snd as notePaths has it: past the exit's stale floor after a
		// route change)
		if rec.segsOut > 0 && rec.retrans == 0 && rec.minRTT > 0 && c.qSnd <= wcDqMin {
			c.cleanRecs++
		} else if rec.segsOut > 0 || rec.srtt > 0 {
			c.cleanRecs = 0
		}
		if rec.srtt > 0 && rec.minRTT > 0 {
			// the sender's fields of a 96-byte record (an old exit's 64
			// bytes, or an exit with no TCP_INFO, read srtt 0 for good; one
			// whose kernel's TCP_INFO ends before tcpi_min_rtt, Linux before
			// 4.6, srtt alone: no q_snd and no resend share for good)
			c.sndSeen = true
		}
	}
	m := c.measure(in, dt)
	m.oooLoss, c.holeSeen = c.holeSeen, false
	m.holeMax, c.holeMax = c.holeMax, 0
	if !carryAt.IsZero() {
		m.sndAt = carryAt
	}
	c.last = m
	if in.poolShort > 0 {
		c.cnt.poolShort += int(in.poolShort)
	}
	if c.parkHold > 0 {
		c.parkHold--
	}
	if in.parks > 0 || in.brakes > 0 {
		c.parkHold = wcParkHold
	}
	if c.tBase <= 0 || dt <= 0 {
		c.evalAt = now.Add(wcTickMin)
		return c.out(in, now, ev)
	}
	if len(m.f) == 0 {
		c.fEmpty++
	} else {
		c.fEmpty = 0
	}

	if c.blind(m) {
		c.blindN++
	} else {
		c.blindN = 0
	}
	if len(m.f) > 0 && !m.idleF {
		if c.rAvg == 0 || !m.sameF {
			c.rAvg = m.rF
		} else {
			c.rAvg += wcRAvgAlpha * (m.rF - c.rAvg)
		}
	}
	// a verdict of holes alone (no record of the interval past its level)
	// where the exit's records come waits for one too (provNote)
	p, prov := c.provSnap(now, c.state != wcOff && c.oooLoss(m) && c.sndJudges(now) &&
		!(m.sndOK && c.sndRef.past(m.snd, c.sndAbs(m))))
	nLoss, test := c.cnt.drainLoss, c.lossTest
	switch {
	case c.state == wcOff:
		c.off(in, m, &ev)
	case c.fEmpty >= wcOffAfter || c.blindN >= wcOffAfter || c.longBlind():
		c.toOff()
	case len(m.f) == 0 || m.idleF:
		// the fast streams paused (or one of them was sent nothing: its
		// sender's PROBE_RTT): keep the state, judge nothing (a probe is
		// over: it measured nothing, back to where it started)
		c.settling = false
		if c.state == wcProbe {
			c.toHold(in, m)
			c.setB(in, m, min(c.B, c.probeFrom))
		}
	case c.settling:
		st := c.state
		c.settled(in, m, &ev)
		if (st == wcHold || st == wcDrain) && m.sndOK {
			// no loss is judged in their settle part: the record's
			// resends wait for the measured part
			c.sndCarry = wcSnd{fresh: true, retrans: in.snd.retrans, segsOut: in.snd.segsOut}
			c.sndCarryAt = m.sndAt
		}
	default:
		switch c.state {
		case wcStartup:
			if stopQ {
				m.qOK, m.qTurn = true, max(m.qTurn, c.qRef+c.dq()+1)
			}
			c.startup(in, m, &ev)
		case wcHold:
			c.hold(in, m, &ev)
		case wcProbe:
			if stopQ {
				m.qOK, m.qTurn = true, max(m.qTurn, c.qRef+c.dq()+1)
			}
			c.probe(in, m, &ev)
		case wcDrain:
			c.toHold(in, m)
		}
	}
	if prov && (c.cnt.drainLoss > nLoss || c.lossTest != test || c.bCapAt.Equal(now)) {
		c.provNote(p, now, m.holeMax)
	}
	if c.peer != nil {
		c.peer.noteCore(c, now, c.state != wcOff && len(m.f) > 0, c.rAvg)
	}
	settle, measure := c.interval()
	c.ramp()
	c.evalAt = now.Add(measure + c.rampLeft())
	if c.state == wcOff {
		c.evalAt = now.Add(wcTickMin)
	} else if c.settling {
		c.evalAt = now.Add(settle + c.rampLeft())
	}
	if c.B > c.cnt.peakB {
		c.cnt.peakB = c.B
	}
	c.nfPrev = len(m.f)
	c.bHist = append(c.bHist, wcRate{now, float64(c.B)})
	cut := 0
	for cut < len(c.bHist)-1 && now.Sub(c.bHist[cut+1].at) >= wcSndSpan {
		cut++
	}
	c.bHist = c.bHist[cut:]
	return c.out(in, now, ev)
}

// fastStop acts between evaluations, on what came in the last wake.
//
// Loss on a path that is otherwise clean (packets arriving behind holes)
// halves B at once. A window-limited sender sends what a window update
// frees at once, and when the data held behind the first hole is released
// a round trip later its updates leave together: with the window still as
// large that credit was one more burst into the full queue, and the losses
// went on by themselves (above about 1.5 queues of window, below the knee
// too). Halved before the release, they stop at the first.
//
// A ramp stops as soon as the queue shows: the latest window turnarounds
// of the fast streams past turnRef+qBrake (the fast arm's own brake). B
// stays where the ramp got to (in STARTUP, where it was a turnaround ago:
// what the queue shows came from that); the evaluation judges.
//
// holeCut: the wake's holes look like a loss's (holeLoss), past the path's
// reordering level where the exit's records judge loss (oooRo); there the
// cut waits for the next record, which keeps it or takes it back (provNote,
// judgeCut).
func (c *wcore) fastStop(in wcIn, holeCut bool) {
	if c.state == wcOff || c.bPub == 0 {
		return
	}
	if holeCut && !c.lossDrain {
		c.cut(in)
		return
	}
	if c.bPub >= c.B {
		return
	}
	stop := false
	if tb := c.turnMins.min(in.now, wcTurnWin); tb > 0 {
		var lasts []time.Duration
		for _, s := range in.streams {
			if c.inF[s.id] && s.turnLast > 0 {
				lasts = append(lasts, s.turnLast)
			}
		}
		if len(lasts) > 0 && pct50(lasts) > tb+c.qRef+max(c.probeQ(in.now), c.dq())+2*c.spread {
			stop = true
		}
	}
	if stop {
		// in STARTUP what the queue shows now came from what was
		// published a turnaround ago; what the ramp added since is past
		// the knee
		b := c.bPub
		if c.state == wcStartup {
			ago := in.now.Add(-(c.tBase + wcRampTick))
			for _, h := range c.pubHist {
				if !h.at.After(ago) {
					b = min(c.bPub, uint64(h.r))
				}
			}
		}
		c.B, c.bPub, c.stopQ = b, b, true
	}
}

// holeLoss says whether a wake's holes look like a loss's on a path that is
// otherwise clean: the share of its packets that arrived behind one is past
// their level (oooRef, under wcOooClean) by wcOooCongAbs, and the holes kept
// them waiting as a loss's does (lossHole). It reports the share too.
func (c *wcore) holeLoss(in wcIn, lossHole bool) (float64, bool) {
	if !in.oooOK || in.segsIn == 0 {
		return 0, false
	}
	v := float64(in.ooo) / float64(in.segsIn)
	return v, lossHole && c.oooRef.ok && c.oooRef.v < wcOooClean && c.oooRef.past(v, wcOooCongAbs)
}

// sndJudges: the edge's core with the exit's records coming (an extended
// record came, and the latest within wcSndQuiet). There packets behind holes
// are no loss by themselves: the exit's resends say whether they were.
//
// Rcv_ooopack counts every packet that arrives behind a hole, the path's
// reordering as much as a loss, and no test of the holes alone tells them
// apart on the real path. Field, 72 ms (Iran edge), single and four-
// connection downloads of 90–120 s at 180–1000 Mbit/s: the core drained 4 to
// 11 times a run, nearly all "packets arrived behind holes on a clean path"
// (fastStop, to the floor: no queue behind them) or "... as the windows grew"
// (oooPast), while the exit's link sockets resent 0.000–0.002%; one download
// swung 180–338 Mbit/s with the core (102 without). Direct TCP on the same
// path: Rcv_ooopack/data_segs_in per 0.5 s 2% at 300 Mbit/s (bursts to
// 38%), 6% at 600 (to 35%), 15% at 1.7 Gbit/s (to 49%), the sender's
// resends ≈ 0 (Linux's RACK resends none of it). Bursts that dense keep a
// wake's packets waiting T_base/4 and more (lossHole's test: at 38%, 38 ms
// of a 100 ms wake against 18 ms), so lossHole passed them as a loss's.
// The exit's record (resends and segments sent, every ~2 s) has no such
// bias: it counts what the sender itself took for lost.
//
// The exit's side (no record of the edge) and an edge whose records stopped
// (sndQuiet: also blind) keep the rules they had: holes cut there.
func (c *wcore) sndJudges(now time.Time) bool {
	return c.cfg.side == wcEdge && c.sndSeen && !c.sndQuiet(now)
}

// wcProv is what holes alone did to B where the exit's records judge loss
// (sndJudges), until a record does (judgeCut): when the first such cut or
// verdict came (at) and the latest (cutAt, last), the largest share of a
// wake's packets behind their holes, and what the core had before them, to
// take back (undoCut).
//
// The reaction itself stays as it was: fastStop's cut between evaluations
// (halved, or to the floor with no queue behind the holes) and the
// evaluation's verdict on Rcv_ooopack (oooLoss). Holding B instead until the
// record came (up to a record span, 2 s, after the holes; tried first) left
// a policer's first losses uncut that long: behind a policer at 30% of
// 1000/100 (the table's "policer 300") the sender's pacing rose from 585 to
// 680 Mbit/s over the second the windows stayed (a window-limited BBR's
// STARTUP pacing only rises) and the run resent 1.71% against 0.46% (base
// windows 0.28%); in the round-2 sweep "policer none>pol60" drained 4 runs
// (1 before) and "pol90>pol40" resent past base + 3 points in 8 (0 before).
// A loss of ours on a shallow queue keeps itself going too (wcLossRef: what
// a hole holds is released a round trip on as one burst), and only a cut
// before that stops it. So the record does not decide whether to cut; it
// decides whether the cut stands.
type wcProv struct {
	at, cutAt, last time.Time
	share           float64
	st              wcState // the state the first one found
	b               uint64  // B before it
	probeFrom       uint64
	bCap            uint64
	bCapAt          time.Time
	qcap            time.Duration
	qcapAt          time.Time
	qcapOK          bool
	backoff         int
	pathFull        bool
	rhat            []wcRate
	// STARTUP's comparison: what the next step is judged against
	fullR, prevR float64
	prevB        uint64
	flat, gear   int
}

// provSnap is what the core has before a reaction to holes alone that a
// record will judge (ok: want); the first one's is kept (provNote).
func (c *wcore) provSnap(now time.Time, want bool) (wcProv, bool) {
	if !want || !c.prov.at.IsZero() {
		return wcProv{}, want
	}
	return wcProv{at: now, st: c.state, b: c.B, probeFrom: c.probeFrom,
		bCap: c.bCap, bCapAt: c.bCapAt, qcap: c.qcap, qcapAt: c.qcapAt, qcapOK: c.qcapOK,
		backoff: c.backoff, pathFull: c.pathFull, rhat: append([]wcRate(nil), c.rhat...),
		fullR: c.fullR, prevR: c.prevR, prevB: c.prevB, flat: c.flat, gear: c.gear}, true
}

// provNote notes a reaction to holes alone (p: provSnap's, before it) that
// the exit's next record judges (judgeCut).
func (c *wcore) provNote(p wcProv, now time.Time, share float64) {
	if c.prov.at.IsZero() {
		c.prov = p
	}
	c.prov.cutAt, c.prov.last, c.prov.share = now, now, math.Max(c.prov.share, share)
}

// holeLag is how long after a hole a record of the exit (s: the one that
// judges) shows its resend: the hole's SACK reaches the sender half a round
// trip on, RACK resends it a quarter of min_rtt after that (up to a round
// trip once it has seen reordering), and the record taken then comes half a
// round trip later; with a wake to spare. The round trip is the larger of
// T_base and the exit's own srtt (its latest record's, sndRTT, and this
// one's): a queue at the exit holds the record back behind the data, and
// RACK's wait grows with srtt, so on T_base alone a record could come
// before the resend it was to show, and take back a cut for a real loss.
func (c *wcore) holeLag(s wcSnd) time.Duration {
	return 2*max(c.tBase, c.sndRTT, s.srtt) + wcRampTick
}

// judgeCut judges what holes alone did to B (wcProv) by a record of the
// exit that came in this wake (in: the wake's own input):
//
//   - it shows a loss of the sender's own (not sndNoLoss): the holes were a
//     loss, and what was done stands, with everything it learned (the loss
//     point, Q_cap, the backoff, a policer's floor), as without records. A
//     loss under what the verdict calls one (sndRef.past by sndAbs) is kept
//     aside (weak): a sender that has not seen the path reorder yet resends
//     what it held (Linux's RACK until its first DSACK: the field's 3 to 26
//     segments in 15 s, all of them then; the model's RACK 96–116 segments of
//     a 40 s run at 1 Gbit/s), and the first burst's cut stood on them; once
//     a later cut for holes as dense is taken back, so is that one (undoCut);
//   - no loss, and it came holeLag or more after the latest hole: they were
//     no loss of ours (the sender resent none of what arrived behind them:
//     the path reordered it), and it is taken back (undoCut);
//   - no loss but too soon after the latest hole: the next record judges.
//
// Where the records stop (sndQuiet) or the core goes OFF, it stands, as
// without records.
//
// Every record that shows a loss worth a verdict, whether a reaction waits
// for it or not, makes a small loss kept aside before it (weak) no more a
// cut to take back: what was learned since is that loss's (its loss point,
// Q_cap, backoff). Only a record that judged a pending reaction did before,
// and a policer's verdict that came at an evaluation with nothing pending was
// wiped, its loss point, Q_cap and backoff back to what they were before the
// small loss, by the next cut for reordering taken back
// (TestWcoreS2RedWeakAfterVerdict).
func (c *wcore) judgeCut(in wcIn) []wcEvent {
	now := in.now
	s := in.snd
	judged := c.sndJudges(now)
	rec := judged && s.fresh && s.segsOut > 0
	loss, small := false, false
	if rec {
		loss = !c.sndNoLoss(s)
		small = loss && c.sndSmall(s)
		if loss && !small {
			c.weak = wcProv{} // a loss worth a verdict: what came since is its
		}
	}
	p := &c.prov
	if p.at.IsZero() {
		return nil
	}
	if !judged || c.state == wcOff {
		*p, c.weak = wcProv{}, wcProv{}
		return nil
	}
	if !rec {
		return nil
	}
	switch {
	case loss:
		if small {
			if c.weak.at.IsZero() {
				c.weak = *p
			}
			c.weak.share = math.Max(c.weak.share, p.share)
		}
		*p = wcProv{}
		return nil
	case now.Sub(p.last) < c.holeLag(s):
		return nil
	}
	return c.undoCut(in, float64(s.retrans)/float64(s.segsOut))
}

// sndNoLoss says whether a record of the exit shows no loss of its own: its
// resent segments no more than its level (sndRef) explains, with its spread
// (wcLossCongSig standard deviations), on a path that resends under
// wcLossRand (the random-loss flag's threshold). Not the verdict's test
// (sndAbs: 0.1% of the record at least, a loss worth a drain): a cut for
// holes stands on any loss the sender saw. With a slack of 1 to 4 segments
// the model's 300/100 behind 50 ms took back a STARTUP's verdict on a
// burst's tail drop (two segments), overshot and drained for the queue: 214
// against 219 Mbit/s and 78% of N4's gain (the table wants 80%); with none,
// as before. And where the level holds losses of its own (a policer's: on
// 1000/150 behind a 200 Mbit/s policer the level took its 1.2%, records at
// it took back 3 of 10 cuts and the run resent 1.03% against 0.53%) holes
// are judged as before: there a hole is as likely a loss.
func (c *wcore) sndNoLoss(s wcSnd) bool {
	exp := 0.0
	if c.sndRef.ok {
		exp = c.sndRef.v * float64(s.segsOut)
	}
	return float64(s.retrans) <= exp+wcLossCongSig*math.Sqrt(exp) && float64(s.retrans) < wcLossRand*float64(s.segsOut)
}

// sndSmall says whether a record's loss is under what the verdict calls one
// (sndRef.past by sndAbs): kept aside (weak), not a verdict of its own.
func (c *wcore) sndSmall(s wcSnd) bool {
	v := float64(s.retrans) / float64(s.segsOut)
	return c.sndRef.ok && !c.sndRef.past(v, c.sndAbsN(s.segsOut))
}

// undoCut takes back what holes alone did to B, the exit's record having
// shown no loss (judgeCut). Every loss point (bCap), Q_cap, backoff and R̂
// reset since the first of them came of them (a record that showed a loss
// would have made them stand first), and goes back to what it was; so does
// B, in the state they found (a probe's: where it began), unless a drain for
// a queue or a slower path came since (movedAt: its verdict stands, and R̂ as
// it reset it) or the core is past HOLD again (a probe, STARTUP: it moves on
// from where it is). The published budget climbs back by the ramp.
//
// (movedAt, not those drains' counters: a drain for a slower path that finds
// B at the floor already, where a cut to it put it, drains nothing and
// counts nothing, but resets R̂ to the slower rate; the record that took the
// cut back restored the budget from before it and the faster R̂, and HOLD
// sized B from that into the slower path's queue,
// TestWcoreS2RedUndoKeepsSlowerRate.)
//
// It also sets the path's reordering level oooRo: the largest share of a
// wake's packets behind the holes taken back, with a margin (wcRoMargin, at
// most wcRoMax), for wcQcapTTL (roLevel). Holes within it cut no more and
// give no verdict (oooLoss).
// Each reordering burst past what was seen cuts once more, so a path whose
// reordering is bursty (the field's 35–49% of a 0.5 s span) cuts for its
// first bursts, not for each; a loss's hole keeps every packet of a round
// trip and more waiting, past all but the densest bursts, and a loss within
// the level still comes to the evaluation in the record's resends
// (lossVerdict). A cut that stood on a small loss (weak) with holes within
// the new level goes back too: its loss point, Q_cap and backoff (not B: it
// is past).
func (c *wcore) undoCut(in wcIn, v float64) []wcEvent {
	p := c.prov
	c.prov = wcProv{}
	now := in.now
	c.cnt.undo++
	c.oooRo, c.oooRoAt = math.Max(c.roLevel(now), math.Min(wcRoMargin*p.share, wcRoMax)), now
	if w := c.weak; !w.at.IsZero() && w.share <= c.oooRo {
		c.weak = wcProv{}
		p.at, p.bCap, p.bCapAt, p.qcap, p.qcapAt, p.qcapOK, p.backoff, p.pathFull =
			w.at, w.bCap, w.bCapAt, w.qcap, w.qcapAt, w.qcapOK, w.backoff, w.pathFull
	}
	if !c.bCapAt.Before(p.at) {
		c.bCap, c.bCapAt = p.bCap, p.bCapAt
	}
	if !c.qcapAt.Before(p.at) {
		c.qcap, c.qcapAt, c.qcapOK = p.qcap, p.qcapAt, p.qcapOK
	}
	if c.backoff != p.backoff {
		c.backoff = p.backoff
		c.schedProbe(now)
	}
	c.pathFull = p.pathFull
	if c.lossTest == wcSrcOoo {
		c.lossTest = wcSrcNone
	}
	moved := !c.movedAt.Before(p.at)
	if len(c.rhat) > 0 && !c.rhat[0].at.Before(p.at) && len(p.rhat) > 0 && !moved {
		// (R̂ was reset since, by the verdict taken back: its samples from
		// before come back)
		c.rhat = append(p.rhat, c.rhat...)
	}
	if c.state != wcDrain && c.state != wcHold || moved {
		return nil
	}
	b := p.b
	if p.st == wcProbe {
		b = min(b, p.probeFrom)
	}
	if b <= c.B {
		return nil
	}
	from := c.bPub
	c.lossDrain, c.refreshing = false, false
	c.state = wcHold
	if p.st == wcStartup {
		// (and the comparison its next step is judged by, as the first
		// verdict found it)
		c.state = wcStartup
		c.fullR, c.prevR, c.prevB, c.flat, c.gear = p.fullR, p.prevR, p.prevB, p.flat, p.gear
	}
	// (within what the window pool covers, n_F·MaxStreamBuffer and the
	// burst cap, as any budget: setB)
	c.setB(in, c.wakeMeas(in), b)
	c.chgAt, c.settling = now, true
	return []wcEvent{{kind: wcEvUndo, from: from, B: c.B, n: len(c.inF), T: c.tBase, loss: v}}
}

// wakeMeas is a runner wake's view of F for setB between evaluations: the
// streams of F in the wake's own input, their windows and H_step, and the
// latest evaluation's rates (R̂ for F, the burst cap).
func (c *wcore) wakeMeas(in wcIn) wcMeas {
	m := wcMeas{at: in.now, rF: c.last.rF, rOther: c.last.rOther, grp: c.last.grp}
	for i, s := range in.streams {
		if c.inF[s.id] {
			m.f = append(m.f, i)
			m.wF += float64(s.win)
			m.hStep += float64(wcStep(c.cfg.need, s.win)) / 2
		}
	}
	return m
}

// roLevel is the path's reordering level (oooRo) as it holds now: for
// wcQcapTTL after the latest record that took a cut back. A path's
// reordering is its route's and its rate's (the field's grew with the rate,
// 2% at 300 Mbit/s, 15% at 1.7 Gbit/s): a level learned once is no licence
// for holes for good (a route change and OFF reset it too).
//
// (Red team: within the level a loss that comes later, a policer's, is seen
// only in the exit's records, ~2 s apart; on the field's path at ~860
// Mbit/s Rcv_ooopack's own level often sits past wcOooClean too, and holes
// are no signal there at all. In the link model's policer sweep,
// TestWcoreS2RedPolicer, the loss rule drained 11 of the 135 runs on the
// field's path and 6 of 135 on the same path without reordering, where the
// core without the angle drained 2 and 6 (it never ran fast there: the
// sender's pace stayed low); 1000/20 behind a policer at 60% resent 9.84%
// against 1.48% without reordering. Not the level's own doing: with no level
// at all, cuts only taken back, 10 drained. Suspending the level while the
// latest record shows a loss took the field's 1000/72 from 670 to 634 Mbit/s
// and drained as many. The larger part was the pacing net's cap held at
// the rate from before the policer (pacenet.go: with it following the
// delivery down, 6 and 4); 1000/20 at 60% still resends 9.95%.)
func (c *wcore) roLevel(now time.Time) float64 {
	if c.oooRo == 0 || now.Sub(c.oooRoAt) >= wcQcapTTL {
		return 0
	}
	return c.oooRo
}

// wakeHoles is how long the packets of a runner wake waited behind holes:
// the wake's length (since the previous one; wcRampTick if that is not
// known) times the share of its segments that arrived behind one.
func (c *wcore) wakeHoles(in wcIn) time.Duration {
	if !in.oooOK || in.segsIn == 0 {
		return 0
	}
	dt := in.now.Sub(c.wakeAt)
	if c.wakeAt.IsZero() || dt <= 0 || dt > wcTickMax {
		dt = wcRampTick
	}
	return time.Duration(float64(dt) * math.Min(float64(in.ooo)/float64(in.segsIn), 1))
}

// wcHoleWake is a runner wake's holes: how long its packets waited behind
// them, and how many did.
type wcHoleWake struct {
	hole time.Duration
	ooo  uint64
}

// wcHoleWakesMax: lossHole sums at most this many wakes (T_base up to
// ~12 s).
const wcHoleWakesMax = 128

// holeSpan is how many wakes before this one lossHole sums: as many as
// wcOooHole of T_base takes at a wake each wcRampTick, less this one, and
// one at least (two wakes: a hole of a loss stays open a round trip and
// more; at most a wake each can hold, and two held at most ~200 ms, under
// T_base/4 past T_base ~800 ms).
func (c *wcore) holeSpan() int {
	n := int(math.Ceil(wcOooHole * float64(c.tBase) / float64(wcRampTick)))
	return min(max(n-1, 1), wcHoleWakesMax)
}

// lossHole says whether the holes of the wake (hole: how long its packets
// waited behind them) and of the ones before it (holeSpan) were a loss's:
// they kept packets waiting wcOooHole of T_base or more, wcOooMinSegs
// segments or more of them. A path that reorders packets by a few ms opens
// holes that short; summed over a whole interval they come to more (1
// Gbit/s, 0.05% held 2 ms: 40 to 65 ms a second), so an interval is judged
// by its wakes (holeSeen).
func (c *wcore) lossHole(in wcIn, hole time.Duration) bool {
	if !in.oooOK {
		return false
	}
	h, n := hole, in.ooo
	w := c.holeWakes
	if k := c.holeSpan(); len(w) > k {
		w = w[len(w)-k:]
	}
	for _, x := range w {
		h += x.hole
		n += x.ooo
	}
	return h >= time.Duration(wcOooHole*float64(c.tBase)) && n >= uint64(wcOooMinSegs)
}

// cut halves the budget for loss between evaluations: the knee is found if
// STARTUP was looking for it, a probe failed, and HOLD stays at the cut
// budget until the loss is gone (lossDrain), then rises to at most
// wcCapBack of where the loss came.
func (c *wcore) cut(in wcIn) {
	now := in.now
	from := c.bPub
	switch c.state {
	case wcStartup:
		c.cnt.grow++
	case wcProbe:
		c.cnt.probeFail++
		c.backoff++
		c.schedProbe(now)
	}
	c.cnt.drainLoss++
	c.noteCap(now, from)
	q, qOK := c.lossQ(in)
	c.noteQcap(now, q)
	c.chgAt = now
	c.pathFull = true
	c.state = wcDrain
	c.refreshing = false
	c.lossDrain, c.lossDrainN = true, 0
	n := uint64(max(len(c.inF), 1))
	c.B = max(from/2, n*uint64(c.cmin()))
	if qOK && q < wcPoliceQ {
		// no queue behind the loss (a policer's, or a burst's whose queue
		// the link socket reads at its floor): to the floor. What the
		// windows announced before the loss is sent once its hole fills;
		// with no queue to take it, half of B lost its next bursts whole
		// behind a 60% policer (the sender timed out and crawled at a few
		// Mbit/s for 20 s: a streak the loss rule drains a link for). Only
		// the exit's record calls it a policer's (policerLoss): packets a
		// path reorders come behind holes with no queue too
		c.B = n * uint64(c.cmin())
	}
	c.bPub = c.B
	c.cuts++
	// judge from a turnaround on
	c.evalAt = now.Add(c.tBase + wcRampTick)
	c.settling = false
}

// lossQ is the queue a loss of the wake came at: the link socket's own srtt
// above T_base now, or the exit's latest record's (a record is up to 2 s
// old), whichever is more; ok: either was read.
func (c *wcore) lossQ(in wcIn) (time.Duration, bool) {
	q, ok := time.Duration(0), false
	if in.srtt > 0 && c.tBase > 0 {
		q, ok = max(q, in.srtt-c.tBase), true
	}
	if !c.qSndAt.IsZero() && in.now.Sub(c.qSndAt) <= wcSndSpan {
		q, ok = max(q, c.qSnd), true
	}
	return q, ok
}

// rampStep is how far the published budget may rise per runner wake from
// pub.
func (c *wcore) rampStep(pub uint64) float64 {
	r := c.last.rF
	if len(c.rhat) > 0 {
		r = math.Max(r, c.rhatF(c.lastAt, c.last))
	}
	s := math.Max(wcRampQ.Seconds()*r, wcRampShare*float64(pub))
	if float64(c.B) < 1.25*float64(pub) {
		s /= 2 // the last stretch, near the knee: half as fast
	}
	return math.Max(s, 1)
}

// ramp moves the published budget toward B: down at once, up by rampStep.
func (c *wcore) ramp() {
	if c.bPub >= c.B || c.bPub == 0 {
		c.bPub = c.B
	} else {
		c.bPub = min(c.B, c.bPub+uint64(c.rampStep(c.bPub)))
	}
	c.pubHist = append(c.pubHist, wcRate{c.acc.now, float64(c.bPub)})
	if len(c.pubHist) > 20 {
		c.pubHist = c.pubHist[len(c.pubHist)-20:]
	}
}

// rampLeft is how long the ramp still takes.
func (c *wcore) rampLeft() time.Duration {
	n := 0
	for p := c.bPub; p < c.B && p > 0 && n < 600; n++ {
		p += uint64(c.rampStep(p))
	}
	return time.Duration(n) * wcRampTick
}

// wcCopy copies an input (its slices too), each stream's turnaround
// samples started from the wake's own.
func wcCopy(in wcIn) wcIn {
	in.streams = append([]wcStream(nil), in.streams...)
	for i := range in.streams {
		in.streams[i].turns = wcWakeTurns(nil, in.streams[i])
	}
	in.pongs = append([]time.Duration(nil), in.pongs...)
	return in
}

// wcWakeTurns appends a wake's turnaround samples of s (its turnMin and
// turnLast, those it has) to v.
func wcWakeTurns(v []time.Duration, s wcStream) []time.Duration {
	if s.turnMin > 0 {
		v = append(v, s.turnMin)
	}
	if s.turnLast > 0 && s.turnLast != s.turnMin {
		v = append(v, s.turnLast)
	}
	return v
}

// wcMerge adds what came in b (later) to a.
func wcMerge(a *wcIn, b wcIn) {
	a.now, a.minRTT, a.srtt, a.oooOK = b.now, b.minRTT, b.srtt, b.oooOK
	a.segsIn += b.segsIn
	a.ooo += b.ooo
	a.linkBytes += b.linkBytes
	a.parks += b.parks
	a.brakes += b.brakes
	a.poolFree, a.ctrlMin, a.apProbing = b.poolFree, b.ctrlMin, a.apProbing || b.apProbing
	a.poolShort += b.poolShort
	a.pongs = append(a.pongs, b.pongs...)
	if b.snd.fresh {
		if a.snd.fresh {
			b.snd.retrans += a.snd.retrans
			b.snd.segsOut += a.snd.segsOut
		}
		a.snd = b.snd
	}
	for _, s := range b.streams {
		found := false
		for i := range a.streams {
			o := &a.streams[i]
			if o.id != s.id {
				continue
			}
			found = true
			o.read += s.read
			o.upd += s.upd
			o.hit += s.hit
			o.lag += s.lag
			if s.turnMin > 0 && (o.turnMin == 0 || s.turnMin < o.turnMin) {
				o.turnMin = s.turnMin
			}
			if s.turnLast > 0 {
				o.turnLast = s.turnLast
			}
			o.turns = wcWakeTurns(o.turns, s)
			o.win, o.braked = s.win, s.braked
			break
		}
		if !found {
			s.turns = wcWakeTurns(nil, s)
			a.streams = append(a.streams, s)
		}
	}
	// a stream that is gone is not part of the link any more
	n := 0
	for _, o := range a.streams {
		for _, s := range b.streams {
			if s.id == o.id {
				a.streams[n] = o
				n++
				break
			}
		}
	}
	a.streams = a.streams[:n]
}

// notePaths keeps T_base, turnBase and the exit's queue.
func (c *wcore) notePaths(in wcIn, ev *[]wcEvent) {
	now := in.now
	c.srttMins.add(now, in.srtt, wcTurnWin)
	t := in.minRTT
	if t <= 0 {
		t = c.srttMins.min(now, wcTurnWin)
		if in.ctrlMin > 0 && (t == 0 || in.ctrlMin < t) {
			t = in.ctrlMin
		}
	}
	for _, s := range in.streams {
		if s.turnMin > 0 && (t <= 0 || s.turnMin >= t/2) {
			c.turnMins.add(now, s.turnMin, wcTurnWin)
		}
	}
	tb := c.turnMins.min(now, wcTurnWin)
	fresh := in.snd.fresh && in.snd.minRTT > 0 && in.snd.srtt >= in.snd.minRTT
	raw := time.Duration(0)
	if fresh {
		raw = in.snd.srtt - in.snd.minRTT
		c.qSndMins.add(now, max(raw, 1), wcTurnWin)
		if c.sndOff > 0 && raw+c.dq() < c.sndOff {
			c.sndOff = 0 // the exit's min_rtt took the path as it is (renewed, or the route back)
		}
		c.qSnd, c.qSndAt = max(raw-c.sndOff, 0), now
		c.sndRTT, c.sndRTTAt = in.snd.srtt, now
	}
	if t > 0 {
		switch {
		case tb > 0 && float64(tb) <= 1.3*float64(t):
			c.tbOff, c.tbOK = tb-t, true
			c.routeLog = false
		case tb > 0 && c.tbOK && float64(tb) > 1.3*float64(t):
			// the path got longer: min_rtt keeps the old floor for up to
			// 5 minutes, turnaround has moved. The exit's own min_rtt is as
			// stale (a new exit's q_snd reads the longer path as queue):
			// where q_snd's floor over the same 60 s rose as much as the
			// turnaround's and its records stay there, that rise is the
			// path's, not a queue's (a queue swings, and the sender's
			// PROBE_RTT empties its own)
			if rise := tb - t - c.tbOff; c.sndOff == 0 && fresh && c.routeSnd(now, rise, raw) {
				c.sndOff = c.qSndMins.min(now, wcTurnWin)
				c.qSnd = max(raw-c.sndOff, 0)
			}
			if c.qSnd <= c.dq() && c.sndAllows(now, tb-c.tbOff, t) {
				t = tb - c.tbOff
				if !c.routeLog {
					c.routeLog = true
					*ev = append(*ev, wcEvent{kind: wcEvRoute, T: t})
					c.oooRo, c.oooRoAt = 0, time.Time{} // (a new path reorders as it does)
				}
			}
		}
		c.tBase = t
	}
	if fresh {
		copy(c.qSnds[1:], c.qSnds[:])
		c.qSnds[0] = wcRate{now, c.qSnd.Seconds()}
		if c.peer != nil {
			c.peer.noteRec(c, now, c.qSnd)
		}
	}
}

// sndAllows says whether the exit's own round trip allows a path of p where
// T_base is t. A longer path is longer for the sender as well: with a record
// of the exit's socket within wcSndHist record spans, its srtt (on a stale
// min_rtt or a renewed one) must have come within a quarter of p's rise past
// t. The turnaround floor rises too when the link's streams are all slow at
// their source -- the time a sender takes to reach the limit it had, which
// no socket sees -- and with the exit's srtt flat that is no route change
// (the field: "~2199 ms" on a 72 ms path once a download had ended). While
// the exit's records have stopped, the decision stands as it was: a route
// change already taken is kept (dropped, T_base fell back to the stale
// min_rtt for the length of the gap and the model's runs took a queue drain
// each) and none is taken anew. A side that never had a record of its
// sender decides by the turnaround, as before -- no core runs there (the
// edge's leaves OFF only on the exit's records; the exit's own one gets the
// edge's record in step 3).
func (c *wcore) sndAllows(now time.Time, p, t time.Duration) bool {
	if c.sndRTTAt.IsZero() {
		return true
	}
	if now.Sub(c.sndRTTAt) > wcSndHist*wcSndSpan {
		return c.routeLog
	}
	return c.sndRTT >= p-max(c.dq(), (p-t)/4)
}

// routeSnd says whether the exit's records read a longer path of rise (the
// turnaround floor's rise past T_base) where its stale min_rtt makes q_snd
// read it as queue: q_snd's floor over wcTurnWin within max(Δq, rise/4) of
// rise, and its latest records (raw: this one's) as flat around it.
func (c *wcore) routeSnd(now time.Time, rise, raw time.Duration) bool {
	if rise <= c.dq() {
		return false
	}
	tol := max(c.dq(), rise/4)
	lo := c.qSndMins.min(now, wcTurnWin)
	if lo <= 0 || lo < rise-tol || lo > rise+tol || raw-lo > tol {
		return false
	}
	for _, r := range c.qSnds {
		if r.at.IsZero() || now.Sub(r.at) > wcSndHist*wcSndSpan {
			continue
		}
		if q := time.Duration(r.r * float64(time.Second)); q-lo > tol {
			return false
		}
	}
	return true
}

// qSndMax is the largest q_snd of the latest wcSndHist records, if they
// came within as many record spans.
func (c *wcore) qSndMax(now time.Time) (time.Duration, bool) {
	q, ok := time.Duration(0), false
	for _, r := range c.qSnds {
		if r.at.IsZero() || now.Sub(r.at) > wcSndHist*wcSndSpan {
			continue
		}
		q, ok = max(q, time.Duration(r.r*float64(time.Second))), true
	}
	return q, ok
}

// pathQ is, with other cores on the peer, the path's queue by the exit's
// records: the median of every link's records of the last two record spans
// (a record is a snapshot of the queue, and on a shallow queue each sender's
// bursts at 3.9 times the path's rate swing it from 0 to 40 ms: 300/150
// behind 50 ms in the model).
func (c *wcore) pathQ(now time.Time) (time.Duration, bool) {
	if q, n := c.peer.queue(now, wcPeerSpan, 0.5); n > 0 {
		return q, true
	}
	return 0, false
}

// pathQueue is the path's queue the interval judges by: with other cores on
// the peer the records of every link (qPath), else the link's own latest
// (qSnd).
func (m *wcMeas) pathQueue() (time.Duration, bool) {
	if m.grp {
		return m.qPath, m.qPathOK
	}
	return m.qSnd, m.qSndOK
}

// grouped says whether another core of the peer is on (wcPeer): the path's
// queue is then theirs too.
func (c *wcore) grouped(now time.Time) bool {
	if c.peer == nil {
		return false
	}
	n, _ := c.peer.others(c, now)
	return n > 0
}

// heldQ is the queue the other cores of the peer hold on the path as they
// should (qTarget, while one of them is on): a late core judges its windows
// past it, not past the path's own floor.
func (c *wcore) heldQ(m wcMeas) time.Duration {
	if !m.grp {
		return 0
	}
	return c.qTarget(m.at)
}

// measure classifies the streams and takes the interval's estimators.
//
// A stream joins F (the fast streams) when its window is at least need/2,
// its reader keeps up, it is about as fast as the fastest stream of the
// link (a slow source also "uses" its window, filling it as slowly as it is
// written), no unbiased signal shows a standing queue past the core's own
// target (the exit's record on the edge, the link socket's own srtt
// otherwise: base windows past the knee are nothing the core can grow), and
// its window is in use: R·turnaround ≥ 0.9 of its window less half a step
// (what a sender held by its window keeps in flight), or, as a hint for a
// window far below the knee, half its cycles or more were window-limited
// (hit) with a bare turnaround. A sender that stops at its limit for
// 2 ms is not enough: under ~85 Mbit/s a stream's own frames come that far
// apart, and the runner sets each stream's hit gap to a few frames' time.
//
// It stays in F while it reads and its reader keeps up (right after B grows
// the sender has a window it has not filled yet, which is no reason to take
// the stream's ceiling away), and while its sender sends nothing for up to
// wcIdleKeep evaluations if its reader is not behind (the sender's
// PROBE_RTT: dropped, its ceiling went to 0 and B was cut to one stream's).
// It leaves when its source got slow, two evaluations in a row on a clean
// path: no update of an interval in a window-limited cycle, every turnaround
// past twice turnBase, a rate under a quarter of what its window allows in a
// round trip, and (on the edge) the exit's record showing no queue.
func (c *wcore) measure(in wcIn, dt float64) wcMeas {
	m := wcMeas{dt: dt, at: in.now}
	if dt <= 0 {
		return m
	}
	need := c.cfg.need
	now := in.now
	if in.oooOK && in.segsIn > 0 {
		m.ooo, m.oooOK = float64(in.ooo)/float64(in.segsIn), true
	}
	// (and the interval after one: a tail loss's timeout leaves an interval
	// of few packets, none behind a hole, and turnarounds of the wait)
	m.holey = m.oooOK && m.ooo >= wcOooClean || c.holeyPrev || c.oooRef.ok && c.oooRef.v >= wcOooClean
	c.holeyPrev = m.oooOK && m.ooo >= wcOooClean
	if in.srtt > 0 && c.tBase > 0 {
		m.qOwn, m.qOwnOK = max(0, in.srtt-c.tBase), true
	}
	if !c.qSndAt.IsZero() && now.Sub(c.qSndAt) <= wcSndSpan {
		m.qSnd, m.qSndOK = c.qSnd, true
	}
	m.grp = c.grouped(now)
	m.qSnd3, m.qSnd3OK = c.qSndMax(now)
	if m.grp {
		// the records of every link: the others' bursts and probes are in
		// them, and one record of them is no standing queue (judge, p1)
		if q, n := c.peer.queue(now, wcSndHist*wcSndSpan, 0.5); n > 0 {
			m.qSnd3, m.qSnd3OK = q, true
		}
		// the queue that stands: the lower quartile of the records
		if q, n := c.peer.queue(now, wcPeerSpan, 0.25); n > 0 {
			m.qStand, m.qStandOK = q, true
		}
	}
	if m.grp {
		m.qPath, m.qPathOK = c.pathQ(now)
	}
	if in.snd.fresh && in.snd.segsOut > 0 {
		m.snd, m.sndOK, m.sndSegs, m.sndAt = float64(in.snd.retrans)/float64(in.snd.segsOut), true, in.snd.segsOut, now
	}
	// no standing queue past the core's own target, by a signal with no
	// bias: the exit's record, else the link socket's own srtt
	noQ := true
	switch {
	case m.qSndOK:
		noQ = m.qSnd <= c.qTarget(now)+c.dq()+c.heldQ(m)
	case m.qOwnOK:
		noQ = m.qOwn <= c.qTarget(now)+wcDqOwn
	}
	maxR := 0.0
	for _, s := range in.streams {
		maxR = math.Max(maxR, float64(s.read)/dt)
	}
	tb := c.turnMins.min(now, wcTurnWin)
	newF := map[uint32]bool{}
	var turns, spreads []time.Duration
	sndBare := c.cfg.side == wcExit || !c.qSndAt.IsZero() && now.Sub(c.qSndAt) <= wcSndSpan && c.qSnd <= c.dq()
	hits, upds := 0.0, 0.0
	for i, s := range in.streams {
		r := float64(s.read) / dt
		if s.braked || float64(s.lag) > wcLagShare*float64(s.upd) {
			continue
		}
		if s.upd == 0 || r <= 0 {
			// its sender sent nothing: a stream of F whose reader is not
			// behind stays in it for a while (the sender's PROBE_RTT)
			if c.inF[s.id] && s.lag == 0 && c.idleN[s.id] < wcIdleKeep && s.win >= need/2 {
				c.idleN[s.id]++
				m.idleF = true
				newF[s.id] = true
				m.f = append(m.f, i)
				m.wF += float64(s.win)
				m.wPrev += float64(s.win)
				m.hStep += float64(wcStep(need, s.win)) / 2
			}
			continue
		}
		delete(c.idleN, s.id)
		if c.inF[s.id] {
			if s.hit == 0 && s.upd >= 2 && tb > 0 && s.turnMin > 2*tb && r*tb.Seconds() < float64(s.win)/4 && sndBare && !m.holey {
				// its source got slow: never all of the window in use, a
				// turnaround that is how long the source takes to fill it,
				// a rate far under what the window allows, no queue at the
				// exit to explain it (a path that slowed down queues there)
				// and no holes to hold the data: twice in a row
				if c.slowN[s.id]++; c.slowN[s.id] >= wcSlowN {
					continue
				}
			} else {
				delete(c.slowN, s.id)
			}
		} else {
			turn := s.turnLast
			if len(s.turns) > 0 {
				turn = pct50(append([]time.Duration(nil), s.turns...))
			}
			if turn <= 0 {
				turn = s.turnMin
			}
			step := float64(wcStep(need, s.win))
			used := turn > 0 && r*turn.Seconds() >= wcUseShare*(float64(s.win)-step/2)
			hint := float64(s.hit) >= wcHitShare*float64(s.upd) && tb > 0 && s.turnMin > 0 && s.turnMin <= tb+c.dq()
			if s.win < need/2 || r < wcPeerShare*maxR || !noQ || !(used || hint) {
				continue
			}
		}
		newF[s.id] = true
		m.f = append(m.f, i)
		m.rF += r
		m.wF += float64(s.win)
		if w, ok := c.prevWin[s.id]; ok {
			m.wPrev += float64(w)
		} else {
			m.wPrev += float64(s.win)
		}
		m.hStep += float64(wcStep(need, s.win)) / 2
		hits += float64(s.hit)
		upds += float64(s.upd)
		switch {
		case len(s.turns) > 0:
			turns = append(turns, s.turns...)
		case s.turnMin > 0:
			turns = append(turns, s.turnMin)
		}
		if s.turnMin > 0 && s.turnLast >= s.turnMin {
			spreads = append(spreads, s.turnLast-s.turnMin)
		}
	}
	m.sameF = len(newF) == len(c.inF)
	for id := range newF {
		if !c.inF[id] {
			m.sameF = false
		}
	}
	for id := range c.slowN {
		if !newF[id] {
			delete(c.slowN, id)
		}
	}
	for id := range c.idleN {
		if !newF[id] {
			delete(c.idleN, id)
		}
	}
	c.inF = newF
	clear(c.prevWin)
	for _, s := range in.streams {
		c.prevWin[s.id] = s.win
	}
	if upds > 0 {
		m.hit = hits / upds
	}
	m.rLink = float64(in.linkBytes) / dt
	m.rOther = math.Max(0, m.rLink-m.rF)
	if tb > 0 && len(turns) > 0 {
		// the 25th percentile of every wake's samples: one lucky sample
		// does not hide a standing queue, and HOL scattered over the
		// interval does not lift it
		m.qTurn, m.qOK = max(0, pct25(turns)-tb), true
	}
	if len(spreads) > 0 {
		m.spread, m.spreadOK = pct50(spreads), true
	}
	// the control ping: two pongs in a row well over T_base and the
	// channel's own offset (measured while the link's queue was empty; the
	// channel's rttMin of eight pongs moves with a standing queue), on a
	// link with no loss to explain it (a pong waits behind holes)
	lossFree := (!m.sndOK || m.snd < wcLossRand) && (!m.oooOK || m.ooo < wcLossRand) && !c.lossy
	if c.tBase > 0 && lossFree && m.qOwnOK && m.qOwn <= wcCtrlCalm {
		for _, p := range in.pongs {
			c.ctrlOffs.add(now, max(p-c.tBase, 0)+1, wcCtrlOffWin)
		}
	}
	floor := in.ctrlMin
	if off := c.ctrlOffs.min(now, wcCtrlOffWin); off > 0 && c.tBase > 0 {
		floor = c.tBase + off - 1
	}
	// (a run of high pongs counts across intervals, but only an interval
	// with a pong of the run in it shows it: an interval without pongs
	// after two high ones showed a queue again with no new sample, and the
	// rig's 4 × 1000/100 drained on it, "q=0.0ms")
	thr := max(wcCtrlQMin, time.Duration(float64(c.tBase)*wcCtrlQShare))
	hi := false
	for _, p := range in.pongs {
		if floor > 0 {
			m.ctrlQ = max(m.ctrlQ, p-floor)
		}
		if floor > 0 && p-floor > thr {
			c.pongHi++
			hi = hi || c.pongHi >= 2
		} else {
			c.pongHi = 0
		}
	}
	m.ctrlUp = hi && c.pongHi >= 2 && lossFree
	return m
}

func pct25(v []time.Duration) time.Duration { return pctDur(v, 0.25) }
func pct50(v []time.Duration) time.Duration { return pctDur(v, 0.5) }

// pctDur is the p-quantile of v, interpolated (v is reordered).
func pctDur(v []time.Duration, p float64) time.Duration {
	sort.Slice(v, func(a, b int) bool { return v[a] < v[b] })
	x := p * float64(len(v)-1)
	i := int(x)
	if i+1 >= len(v) {
		return v[len(v)-1]
	}
	return v[i] + time.Duration((x-float64(i))*float64(v[i+1]-v[i]))
}

// rhatF is R̂ less what the link carries for others: what F can have.
func (c *wcore) rhatF(now time.Time, m wcMeas) float64 {
	r := 0.0
	n := 0
	for i := len(c.rhat) - 1; i >= 0; i-- {
		s := c.rhat[i]
		if now.Sub(s.at) > wcRhatWin && n >= wcRhatMinN {
			break
		}
		r = math.Max(r, s.r)
		n++
	}
	return math.Max(r-m.rOther, m.rF)
}

func (c *wcore) noteRate(now time.Time, r float64) {
	c.rhat = append(c.rhat, wcRate{now, r})
	cut := 0
	for cut < len(c.rhat)-wcRhatMinN && now.Sub(c.rhat[cut].at) > wcRhatWin {
		cut++
	}
	if cut > 0 {
		c.rhat = append(c.rhat[:0], c.rhat[cut:]...)
	}
	if r > c.cnt.peakR {
		c.cnt.peakR = r
	}
}

// knee is K = R·(T_base+τ_hold) + H_step for a rate r.
func (c *wcore) knee(r float64, m wcMeas) float64 {
	return r*(c.tBase+c.tau).Seconds() + m.hStep
}

func (c *wcore) bHold(now time.Time, m wcMeas) uint64 {
	r := c.rhatF(now, m)
	t := c.tBase.Seconds()
	h := math.Min(math.Max(c.qTarget(now).Seconds()/t, wcHMin), wcHMax)
	margin := h * r * t
	if f, ok := c.fairShare(now, m); ok {
		// other cores on the peer: the knee at the rate the streams carry
		// (R̂ is its largest of 10 s, and the shares of one bottleneck swing:
		// the sum of the links' R̂ was past the path), and the same margin
		// for every core (qTarget at the fair share): each holds as much in
		// the shared queue, so the links' rates even out (a link's rate is
		// what it holds there over the queue) and the queue adds up to
		// qTarget, not n·qTarget
		r, margin = math.Min(r, c.rAvg), h*f*t
	}
	b := uint64(c.knee(r, m) + margin)
	if c.bCap > 0 && now.Sub(c.bCapAt) < wcQcapTTL {
		b = min(b, c.bCap)
	}
	// past what was shown to pay only by the margin (h·R̂·T_base: the
	// standing queue of qTarget): τ_hold and R̂ alone (the delay behind
	// holes the window itself may cause) never take B into budgets nothing
	// has tried
	lim := c.bGood + uint64(margin)
	return min(b, max(lim, uint64(max(len(m.f), 1))*uint64(c.cmin())))
}

// aboveShare: other cores are on the peer and this one's fast streams carry
// more than wcShareSlack times what each would carry alike. Such a core does
// not probe: on a path with room the others' probes take what is left, and a
// probe sized by its own R̂ (probeQ at its rate) kept the largest share the
// largest (4 links on 1000/100 behind 50 ms: 430 against 140 Mbit/s for a
// minute in the model, the queue too bursty to stand).
func (c *wcore) aboveShare(now time.Time, m wcMeas) bool {
	if !m.grp || c.peer == nil || c.rAvg <= 0 {
		return false
	}
	n, r := c.peer.others(c, now)
	if n == 0 {
		return false
	}
	return c.rAvg > wcShareSlack*(r+c.rAvg)/float64(n+1)
}

// wcShareSlack: aboveShare's margin over the fair share.
var wcShareSlack = 1.2

// fairShare is, with other cores on the peer and a queue that stands on the
// path (the records' lower quartile at qTarget/2 or more), what each one's
// fast streams would carry if they carried alike (their smoothed R_F over
// the cores). Where Q_cap lowers qTarget (a shallow queue: a third of it)
// the shared rule holds only a core past that share, and the others keep
// their own (R̂ and probes): there qTarget is a few ms, under what the
// exit's records read from the senders' bursts alone on a path with room
// (its srtt counts every packet of a burst), and the rule's equilibrium held
// two downloads on 300/150 behind 50 ms at 203 against 230 Mbit/s before
// (z24 rig: records 7–11 ms, qTarget 3–5, the ping through the path 3 ms);
// the drop-tail queue itself bounds what the cores can stack there.
func (c *wcore) fairShare(now time.Time, m wcMeas) (float64, bool) {
	if !m.grp || c.rAvg <= 0 || !m.qStandOK || m.qStand < c.qTarget(now)/2 {
		// (a queue that does not stand is a path with room: there each
		// core grows as alone, on R̂ and its probes; with two links on
		// 300/150 behind 50 ms, whose senders' bursts swing the queue from 0
		// to 40 ms around a mean of 5, the shared rule held each at its
		// floor, 190 against 218 Mbit/s in the model)
		return 0, false
	}
	n, r := c.peer.others(c, now)
	if n == 0 {
		return 0, false
	}
	f := (r + c.rAvg) / float64(n+1)
	if c.qTarget(now) < wcQTarget && c.rAvg <= f {
		return f, false
	}
	return f, true
}

// noteCap remembers that losses of our own came at budget b.
func (c *wcore) noteCap(now time.Time, b uint64) {
	// never under the least budget of the fast streams (a cap there is no
	// cap: the floor is what the windows have without the core)
	cp := max(uint64(wcCapBack*float64(b)), uint64(max(len(c.inF), c.nfPrev, 1))*uint64(c.cmin()))
	if c.bCap == 0 || now.Sub(c.bCapAt) >= wcQcapTTL || cp < c.bCap {
		c.bCap = cp
	}
	c.bCapAt = now
}

// bProbe adds probeQ of queue at R̂ to hold: sized in queue time on the path,
// not as a share of B (under random loss half of B can be data held behind
// holes in the receiver, which a share of it would put on the path too).
func (c *wcore) bProbe(now time.Time, m wcMeas, hold uint64) uint64 {
	t := c.tBase.Seconds()
	g := math.Min(math.Max(c.probeQ(now).Seconds()/t, wcGpMin-1), wcGpMax-1)
	return hold + uint64(g*c.rhatF(now, m)*t)
}

// setB sets the budget within [n_F·c_min, n_F·MaxStreamBuffer] and what the
// window pool can cover, and, with Q_cap known, where the sender's burst
// after its PROBE_RTT fits the queue (burstCap), and whether the change must
// settle first. n_F is the larger of this interval's and the previous one's:
// a stream of F that was sent nothing for an interval (the sender's
// PROBE_RTT) does not cut B to the other streams' ceilings.
func (c *wcore) setB(in wcIn, m wcMeas, b uint64) {
	n := uint64(max(len(m.f), c.nfPrev, 1))
	lo := n * uint64(c.cmin())
	hi := n * uint64(c.cfg.maxWin)
	pool := false
	if in.poolFree >= 0 {
		held := 0.0
		for _, i := range m.f {
			if w := in.streams[i].win; w > c.cfg.need {
				held += float64(w - c.cfg.need)
			}
		}
		if p := n*uint64(c.cfg.need) + uint64(held) + uint64(in.poolFree); p < hi {
			hi, pool = p, true
		}
	}
	hi = max(hi, lo)
	b = min(max(b, lo), hi)
	c.capped = b >= hi && hi > lo
	// the pool held it only if it has less than a step of a window left
	c.capPool = c.capped && pool && in.poolFree < int64(wcStep(c.cfg.need, uint32(min(b/n, math.MaxUint32))))
	c.burstHeld = false
	if bc := c.burstCap(in.now, m); b > max(bc, lo) {
		b = max(bc, lo)
		c.burstHeld = true
	}
	if c.B == 0 || math.Abs(float64(b)-float64(c.B)) > wcResettle*float64(c.B) {
		c.settling = true
	}
	c.B = b
}

// burstCap is the largest budget whose burst after the sender's PROBE_RTT
// fits the queue learned (Q_cap). A window-limited BBR sender (in its
// STARTUP for good) enters PROBE_RTT every ~10 s whatever the receiver does
// (Linux takes a new min_rtt only from a strictly lower sample), and then
// sends all the credit it has at 3.9 times the path's rate: of B less what
// the receiver holds unannounced (H_step), about wcBurstShare becomes queue.
// None without Q_cap (no loss of our own seen: a deep queue).
func (c *wcore) burstCap(now time.Time, m wcMeas) uint64 {
	if !c.qcapKnown(now) || c.tBase <= 0 || wcBurstShare <= 0 {
		return math.MaxUint64
	}
	return uint64(c.qcap.Seconds()*c.rhatF(now, m)/wcBurstShare + m.hStep)
}

// bSpan is the least and the largest budget over the last wcSndSpan (what a
// record of the exit covers), with the one in force now.
func (c *wcore) bSpan() (lo, hi uint64) {
	lo, hi = c.B, c.B
	for _, h := range c.bHist {
		lo, hi = min(lo, uint64(h.r)), max(hi, uint64(h.r))
	}
	return lo, hi
}

// lossVerdict judges the interval's loss against the levels kept at known
// budgets (wcLossRef): cong when it is past the level of a smaller budget
// (it came with B) or came with queue growth; jump when it is past the level
// of the same budget (it came with nothing of ours: a drain tells whether it
// keeps itself going or the path turned lossy). Rcv_ooopack, a share of the
// packets that waited behind a hole, saturates once holes come every round
// trip: it is judged only on a path that is otherwise clean, and not within
// the path's reordering level where the exit's records judge loss (oooLoss).
func (c *wcore) lossVerdict(m wcMeas, growth bool) (cong, jump bool, src wcLossSrc) {
	if m.sndOK && !c.sndStale(m.sndAt) && c.sndRef.past(m.snd, c.sndAbs(m)) {
		_, hi := c.bSpan()
		if growth || float64(hi) > 1.05*float64(c.sndRef.b) {
			return true, false, wcSrcSnd
		}
		jump, src = true, wcSrcSnd
	}
	if c.oooLoss(m) {
		// c.B is still the budget the interval was measured under
		if growth || float64(c.B) > 1.05*float64(c.oooRef.b) {
			return true, false, wcSrcOoo
		}
		jump, src = true, wcSrcOoo
	}
	return false, jump, src
}

// oooPast: the interval's packets behind holes (Rcv_ooopack) are past their
// level on a path that is otherwise clean, and a hole of a loss came in it
// (lossHole): a path that reorders packets by a few ms opens holes too.
func (c *wcore) oooPast(m wcMeas) bool {
	return m.oooOK && c.oooRef.ok && c.oooRef.v < wcOooClean && c.oooRef.past(m.ooo, wcOooCongAbs) && m.oooLoss
}

// oooLoss: packets behind holes past their level (oooPast) are a loss
// verdict of the interval, unless the exit's records judge loss (sndJudges)
// and no wake of the interval had more of its packets behind a loss's hole
// than the path's reordering level (oooRo, undoCut). Where they judge, the
// verdict waits for a record (provNote).
func (c *wcore) oooLoss(m wcMeas) bool {
	ro := c.roLevel(m.at)
	return c.oooPast(m) && !(ro > 0 && m.holeMax <= ro && c.sndJudges(m.at))
}

// sndAbs is how far past its level a record's resend share must be to be
// more than the level's own spread: wcLossCongSig standard deviations of a
// binomial share at the level over the record's segments, at least
// wcLossCongMin.
func (c *wcore) sndAbs(m wcMeas) float64 { return c.sndAbsN(m.sndSegs) }

// sndAbsN is sndAbs for a record of segs segments.
func (c *wcore) sndAbsN(segs uint64) float64 {
	if segs == 0 || !c.sndRef.ok {
		return wcLossCongAbs
	}
	return math.Max(wcLossCongMin, wcLossCongSig*math.Sqrt(c.sndRef.v/float64(segs)))
}

// sndStale: a record of the exit that came now covers time from before the
// latest drain, cut or start (chgAt): its loss belongs to a budget, or a
// path rate, that is no more (a record of a 50 Mbit/s spell cut the budget
// after the path was fast again, and kept its cap fresh).
func (c *wcore) sndStale(now time.Time) bool {
	return !c.chgAt.IsZero() && now.Add(-wcSndSpan).Before(c.chgAt)
}

// noteLoss keeps the loss levels of an interval that was not judged
// congestive, with the budget they were measured at, and the random-loss
// flag.
func (c *wcore) noteLoss(m wcMeas) {
	if m.sndOK && !c.sndStale(m.sndAt) {
		if lo, hi := c.bSpan(); float64(hi) <= 1.05*float64(lo) {
			c.sndRef.note(m.snd, hi, 0.3)
		}
	}
	if m.oooOK {
		c.oooRef.note(m.ooo, c.B, 0.3)
	}
	l, ok := m.snd, m.sndOK
	if !ok && c.cfg.side == wcExit && m.oooOK {
		l, ok = m.ooo/10, true // ~ a hole per ten packets that wait behind it
	}
	if ok {
		if l >= wcLossRand {
			c.lossyN++
		} else {
			c.lossyN = 0
			c.lossy = false
		}
	}
}

// noteTau updates τ_hold from an interval in which the windows were the
// limit: R_F = (W̄_F − H_step)/(T_base + τ_hold).
func (c *wcore) noteTau(m wcMeas) {
	if m.rF <= 0 || math.Abs(m.wF-m.wPrev) > 0.1*m.wF {
		return // the windows moved during the interval
	}
	if len(c.rhat) > 0 && m.rF < wcDipShare*c.rhatF(m.at, m) {
		return // the sender paused (its PROBE_RTT): no wait behind holes
	}
	w := (m.wF + m.wPrev) / 2
	s := time.Duration(((w-m.hStep)/m.rF - c.tBase.Seconds()) * float64(time.Second))
	if m.grp && m.qStandOK {
		// with other cores on the peer the queue that stands on the path is
		// theirs too, and no wait behind holes: a stream held by its window
		// turns in T_base plus that queue, whether its own sender waited at
		// the limit or not (τ_hold took it in, B with it, and the queue grew
		// by the margin each time; red team, round 3). Only what stands (the
		// records' lower quartile): one record is a snapshot, a burst's as
		// often as not, and τ_hold also holds what the receiver keeps
		// unannounced past H_step (300/150 behind 50 ms with two links: 26 ms
		// with 5 ms of queue). (Alone, the queue a sender held by its window
		// meets is its own, and the knee τ_hold made of it is what the link's
		// rate needed: taken out, 1000/100 fell from 778 to 697 Mbit/s over 12
		// model seeds.)
		s -= m.qStand
	}
	s = min(max(s, 0), 3*c.tBase) // (under 0.1% loss on 300/150 τ_hold reached 2.7–2.9·T_base at 2 BDP)
	if c.tauOK {
		c.tau += time.Duration(wcTauAlpha * float64(s-c.tau))
	} else {
		c.tau, c.tauOK = s, true
	}
}

func (c *wcore) schedRefresh(now time.Time) {
	c.refreshAt = now.Add(wcRefreshMin + time.Duration(c.rnd()*float64(wcRefreshMax-wcRefreshMin)))
}

// refresh takes B under the knee by the standing queue and Δq for one
// interval (a drain that is no verdict: nothing is learned from it).
func (c *wcore) refresh(in wcIn, m wcMeas) {
	c.state = wcDrain
	c.refreshing = true
	c.schedRefresh(in.now)
	r := c.rhatF(in.now, m)
	b := float64(c.B) - (c.qTarget(in.now)+c.dq()).Seconds()*r
	c.B = 0
	c.setB(in, m, uint64(math.Max(b, 0)))
}

func (c *wcore) schedProbe(now time.Time) {
	iv := wcProbeMin + time.Duration(c.rnd()*float64(wcProbeMax-wcProbeMin))
	iv <<= min(c.backoff, 4)
	iv = min(iv, wcProbeBackMax)
	iv = max(iv, time.Duration(wcProbeRTTs)*c.tBase)
	c.probeAt = now.Add(iv)
}

// off: no ceilings. A fast stream with T_base known starts STARTUP, unless
// there is nothing for the core to find (do no harm, stage 1).
func (c *wcore) off(in wcIn, m wcMeas, ev *[]wcEvent) {
	if len(m.f) > 0 {
		// the loss of the path with the windows without the core
		if m.sndOK {
			c.sndRef.note(m.snd, uint64(m.wF), 0.3)
		}
		if m.oooOK {
			c.oooRef.note(m.ooo, uint64(m.wF), 0.3)
		}
	}
	if len(m.f) == 0 || in.apProbing || m.idleF || math.Abs(m.wF-m.wPrev) > 0.1*m.wF {
		c.offN = 0
		return
	}
	// the windows without the core are what STARTUP's first step is judged
	// against: wait until they stopped growing, and on the edge for the
	// exit's first record (its queue tells whether they are past the knee
	// already) and its first resend share with them
	// (a resend share takes two records: the first sets the base of the
	// next; wait for it up to two record spans after the first came).
	// The edge waits for an extended record (sndSeen) for as long as the
	// link lives: an old exit (64 bytes, srtt 0 or min_rtt 0 for good) leaves the
	// windows as without the core. Without the sender's record the core
	// cannot see a policer's signature, and its growth lifts the exit's BBR
	// pacing past the policer for good (red team, round 1: 8 of 96 model
	// runs behind a policer at 60% of the path drained the link).
	if c.offN++; c.offN < 2 || c.cfg.side == wcEdge && !c.sndSeen ||
		c.sndSeen && !c.sndRef.ok && in.now.Sub(c.sndAnyAt) < 2*wcSndSpan {
		return
	}
	if c.cfg.stage < 2 && c.harm(m) {
		c.offN = 0
		return
	}
	if _, clean := c.longGrow(m); !clean || c.longRTT() && c.cleanRecs < wcLongClean {
		// past wcLongRTT: wait for wcLongClean records in a row that show
		// the path with no queue and no resends
		return
	}
	c.offN = 0
	c.bGood = 0
	c.state = wcStartup
	c.chgAt = in.now
	if !c.policed(in.now) {
		c.backoff = 0
	}
	c.pathFull = false
	c.flat = 0
	c.gear = 1
	if c.tBase >= wcLongGear {
		c.gear = 0
	}
	c.fullR, c.prevR = m.rF, m.rF
	c.qRef = m.qTurn
	c.qOwnRef = m.qOwn
	c.rhat = c.rhat[:0]
	c.noteRate(in.now, m.rLink)
	b := uint64(m.wF)
	if !c.kPrevAt.IsZero() && in.now.Sub(c.kPrevAt) < wcKPrevTTL {
		b = max(b, c.kPrev/2)
	}
	c.prevB = max(b, uint64(len(m.f))*uint64(c.cmin()))
	c.B = 0
	c.setB(in, m, uint64(float64(c.prevB)*wcGears[c.gear]))
	c.bPub = min(uint64(m.wF), c.B) // up from the windows in force
}

// harm says whether the windows without the core are already past the knee
// (stage 1: nothing under them is the core's to change, and growing them
// can only add loss):
//   - the exit's records show a queue past Δq (the largest of the latest
//     three). The rig (z12, P0–P1) split in two: where the core gains, the
//     windows without it left 0.9–3.2 ms (300/150, 1000/100, with or without
//     0.1% loss, a 200 ms queue, a speed change); where they already fill
//     the path (100/100, 100/50, 4 downloads on 300/150), 6.6 to 52 ms,
//     mostly past 10. Past qTarget+Δq (20 ms) as before, 100/100 started on
//     a 19.7 ms record, crept to 2.5 MiB and lost 26% of an interval's
//     packets for 86 → 90 Mbit/s;
//   - the exit's records show over 1% resent;
//   - with no record to tell a queue from data held behind holes (the
//     exit's side; an edge with an old exit never leaves OFF, off()): the
//     windows hold wcPastKnee times what their rate needs (R_F·T_base +
//     H_step) or more. (Not on the edge with a record: under random loss
//     the windows hold that much behind holes, with no queue at the exit,
//     and that is where the core gains most.)
func (c *wcore) harm(m wcMeas) bool {
	switch {
	case m.qSnd3OK && m.qSnd3 > c.dq()+c.heldQ(m):
		return true
	case c.sndRef.ok && c.sndRef.v > wcOffLoss:
		return true
	case !m.qSnd3OK && m.rF > 0 && m.wF >= wcPastKnee*(m.rF*c.tBase.Seconds()+m.hStep):
		return true
	case c.blind(m), c.longBlind():
		return true
	}
	return false
}

func (c *wcore) toOff() {
	c.lossDrain = false
	c.prov, c.weak = wcProv{}, wcProv{} // (OFF's windows are without the core: nothing to take back)
	c.oooRo, c.oooRoAt = 0, time.Time{}
	if c.state == wcHold || c.state == wcProbe {
		c.kPrev, c.kPrevAt = c.B, c.lastAt
	}
	c.state = wcOff
	c.B = 0
	c.rAvg = 0
	c.settling = false
	c.refPending = false
	c.capped = false
	c.lossTest = wcSrcNone
	c.sndCarry = wcSnd{} // (a record of the core's own budgets is no level of the windows without it)
	clear(c.inF)
}

// settled ends the settle part of an interval: only what cannot wait for
// the measuring part is acted on.
func (c *wcore) settled(in wcIn, m wcMeas, ev *[]wcEvent) {
	c.settling = false
	switch c.state {
	case wcProbe, wcStartup:
		if cong, _, _ := c.lossVerdict(m, false); cong {
			if c.state == wcProbe {
				c.probeFailed(in, m, ev, true, true)
			} else {
				c.endStartup(in, m, ev, true, true)
			}
		} else if in.apProbing {
			c.toHold(in, m)
		}
	case wcDrain:
		if c.lossTest == wcSrcNone && c.queueBack(m) {
			c.toHold(in, m)
		}
	}
}

// queueBack: the queue is back at its HOLD level (a drain may end early):
// q_turn within Δq/2 of q_ref on a clean path; where data waits behind
// holes, q_own within wcDqOwn/2 of its reference.
func (c *wcore) queueBack(m wcMeas) bool {
	if m.holey {
		return m.qOwnOK && m.qOwn < c.qOwnRef+wcDqOwn/2
	}
	return m.qOK && m.qTurn < c.qRef+c.dq()/2
}

// queueGrew says whether the interval's queue is past its HOLD level (q_ref)
// by more than k·Δq on a clean path, by the window turnaround; where data
// waits behind holes (the turnaround holds that wait), by the link socket's
// own srtt past its reference by more than k·wcDqOwn. It reports the signal
// and how far past its reference it is.
func (c *wcore) queueGrew(m wcMeas, k float64) (bool, wcSig, time.Duration) {
	if m.holey {
		if !m.qOwnOK {
			return false, wcSigOwn, 0
		}
		d := m.qOwn - c.qOwnRef
		return d > time.Duration(k*float64(wcDqOwn)), wcSigOwn, d
	}
	if !m.qOK {
		return false, wcSigTurn, 0
	}
	d := m.qTurn - c.qRef
	return d > time.Duration(k*float64(c.dq())), wcSigTurn, d
}

// capRef holds q_ref within qTarget + Δq/2 while other cores are on the
// peer, on a path whose packets do not wait behind holes (there q_turn is
// the path's queue and little else): a reference past it is the others'
// standing queue taken for the path's own floor (their q_ref crept from
// 13–17 to 30–48 ms in two minutes, red team, round 3). Where data waits
// behind holes q_turn holds that wait, and q_own judges the queue.
func (c *wcore) capRef(now time.Time, m wcMeas) {
	if m.holey || !m.grp {
		return
	}
	c.qRef = min(c.qRef, c.qTarget(now)+c.dq()/2)
}

// qSndRefOf is q_snd's HOLD reference as it judges: with other cores on the
// peer never past qTarget, the queue all of them together should hold (each
// took the queue it found as HOLD began, and the others' was in it: 2 and 4
// downloads on separate links held 21–32 ms, red team, round 3).
func (c *wcore) qSndRefOf(now time.Time, m wcMeas) time.Duration {
	if m.grp {
		return min(c.qSndRef, c.qTarget(now))
	}
	return c.qSndRef
}

func (c *wcore) startup(in wcIn, m wcMeas, ev *[]wcEvent) {
	if !m.sameF {
		// a stream joined or left: start the comparison over
		c.prevR, c.prevB, c.fullR, c.flat = m.rF, c.B, m.rF, 0
		c.setB(in, m, max(c.B, uint64(len(m.f))*uint64(c.cmin())))
		return
	}
	gB := float64(c.B)/math.Max(float64(c.prevB), 1) - 1
	gR := m.rF/math.Max(c.prevR, 1) - 1
	qUp, _, _ := c.queueGrew(m, 1)
	if qUp && m.sndOK && m.qSndOK && m.qSnd <= c.dq()/2 {
		qUp = false // the exit's record of this interval shows none
	}
	cong, jump, _ := c.lossVerdict(m, qUp)
	if judged, clean := c.longGrow(m); !cong && !clean {
		if !judged {
			// past wcLongRTT with no fresh record of the exit: B stays
			// where it is and the interval counts for nothing
			return
		}
		// the record shows a queue or resends: the knee
		c.endStartup(in, m, ev, qUp, false)
		return
	}
	if !cong && !jump {
		// what the data held behind a hole adds when it is released at
		// once is no rate of the path
		c.noteRate(in.now, m.rLink)
	}
	tracked := gB > 0 && gR >= wcStartFast*gB
	if m.rF >= c.fullR*(1+wcFullGrowth) {
		c.fullR, c.flat = m.rF, 0
	} else {
		c.flat++
	}
	if (tracked || m.hit >= wcHitShare) && !qUp && !cong {
		c.noteTau(m)
	}
	if (tracked || m.holey && !c.blind(m)) && !qUp && !cong {
		// paid: the rate grew with B, or (where data waits behind holes
		// and the rate of an interval says little) no queue came of it
		c.bGood = max(c.bGood, min(c.B, c.bPub))
	}
	if m.qOK && !qUp && m.qTurn < c.qRef {
		c.qRef = m.qTurn
	}
	if m.qOwnOK && !qUp && m.qOwn < c.qOwnRef {
		c.qOwnRef = m.qOwn
	}
	// past a budget that brought losses of our own (bCap, fresh) STARTUP
	// goes on in the smallest steps only (the rig's 300/150: a STARTUP after
	// two probe wins went 4.4 → 5.5 → 8.2 MiB past a cap of 5.3 and
	// overflowed the queue again, 59% of the interval's packets out of
	// order). It is not stopped there: held at the cap (z12 core2–core5),
	// 300 → 50 → 300 got back to 90% in 3 of 8 runs, against 3 of 3 without
	// (core1), with no fewer losses on the steady paths (300/150: 0.27%
	// resent, 216 Mbit/s against 0.23%, 231)
	pastCap := c.bCap > 0 && in.now.Sub(c.bCapAt) < wcQcapTTL && c.B >= c.bCap
	if cong || c.flat >= wcFullRounds || (qUp && c.flat >= 1) || ((c.capped || c.burstHeld) && c.flat >= 1) {
		c.endStartup(in, m, ev, cong || qUp, cong)
		return
	}
	if in.apProbing {
		// the autopilot probes the pool size: STARTUP stops where it is,
		// with no knee found (no "fill the path" line, and the path is not
		// full: in stage 2 that took c_min under need)
		c.toHold(in, m)
		return
	}
	switch {
	case qUp:
		c.gear = -1 // the sender or the queue holds what B has: wait
	case tracked:
		c.gear = min(c.gear+1, len(wcGears)-1)
	default:
		c.gear = max(c.gear-1, 0)
	}
	if m.holey && !(m.qSndOK && m.qSnd <= c.dq()/2) {
		// where data waits behind holes q_own (an srtt) shows a queue only
		// a while after it formed: the smallest steps, unless the exit's
		// latest record (no wait behind holes in it) shows the path queue
		// empty. (Always the smallest, z12 P1: the rate of a ×1.25 step
		// never grew the 25% STARTUP asks, so each STARTUP ended after
		// wcFullRounds; 1000/100 with 0.1% loss crept from 2 to 14 MiB in
		// 24 s, 228 Mbit/s against 363 before, with q_snd 1.2–2 ms all along.)
		c.gear = min(c.gear, 0)
	}
	if pastCap || c.tBase >= wcLongGear {
		c.gear = min(c.gear, 0)
	}
	g := 1.0
	if c.gear >= 0 {
		g = wcGears[c.gear]
	} else {
		c.gear = 0
	}
	k := c.knee(m.rF, m)
	c.prevR, c.prevB = m.rF, c.B
	c.setB(in, m, max(c.B, uint64(g*math.Min(float64(c.B), k))))
}

// endStartup: the knee is found. To DRAIN if the queue grew or loss came
// with B, else to HOLD.
func (c *wcore) endStartup(in wcIn, m wcMeas, ev *[]wcEvent, drain, cong bool) {
	from := c.B
	c.cnt.grow++
	if !c.capped {
		c.pathFull = true
	}
	r := c.rhatF(in.now, m)
	*ev = append(*ev, wcEvent{kind: wcEvFilled, from: c.prevB, B: c.bHold(in.now, m), n: len(m.f), R: r, T: c.tBase, q: c.qTarget(in.now)})
	if c.capped {
		*ev = append(*ev, wcEvent{kind: wcEvCapped, B: from, n: len(m.f), T: c.tBase, pool: c.capPool})
	}
	if drain {
		why := wcWhyQueue
		_, sig, q := c.queueGrew(m, 1)
		if cong {
			why, sig, q = wcWhyLoss, wcSigLoss, 0
			c.learnQcap(in.now, m)
			c.noteCap(in.now, from)
		}
		c.toDrain(in, m, ev, why, sig, q, false)
		return
	}
	c.toHold(in, m)
}

// learnQcap learns the queue's capacity from an interval that lost packets
// of our own: the queue measured as they were lost, by the signals that do
// not hold the wait behind holes (q_own, q_snd), or q_turn above q_ref on a
// clean path with neither.
func (c *wcore) learnQcap(now time.Time, m wcMeas) {
	q, ok := time.Duration(0), false
	if m.qOwnOK {
		q, ok = m.qOwn, true
	}
	if m.qSndOK {
		q, ok = max(q, m.qSnd), true
	}
	if !ok && m.qOK {
		q = max(0, m.qTurn-c.qRef)
	}
	if c.policerLoss(m) {
		c.notePoliced(now)
	}
	c.noteQcap(now, q)
}

// policerLoss: the exit's record of the interval shows resends past their
// level and no queue at its socket (q_snd under wcPoliceQ; nor at the link
// socket's own): a policer's signature, when the loss is ours (it came with
// B, or went on at the budget a drain left). Only the exit's record tells:
// Rcv_ooopack also counts packets a path reorders, and q_own reads the
// queue's floor (a tail drop of a burst read 13.6 ms of a 50 ms queue on
// the rig).
func (c *wcore) policerLoss(m wcMeas) bool {
	return m.sndOK && !c.sndStale(m.sndAt) && c.sndRef.past(m.snd, c.sndAbs(m)) &&
		m.qSndOK && m.qSnd < wcPoliceQ && (!m.qOwnOK || m.qOwn < wcPoliceQ)
}

// notePoliced: a policer's loss came now. Its loss point holds for
// wcQcapTTL (policed), probes back off, and the drain it brings goes to the
// floor (toDrain).
func (c *wcore) notePoliced(now time.Time) {
	c.policedAt = now
	c.backoff = max(c.backoff, wcPoliceBackoff)
	c.schedProbe(now)
}

// noteQcap takes q, the queue measured as packets of our own were lost, as
// the queue's capacity (at least wcQcapMin), averaged with what was known:
// each cut measures it anew, none halves it (a qTarget of a third of it
// made Q_cap = 1.5·qTarget halve with every cut: 22.5 ms to 0.7 ms in the
// rig's 300 → 50 → 300).
func (c *wcore) noteQcap(now time.Time, q time.Duration) {
	q = max(q, wcQcapMin)
	if c.qcapKnown(now) {
		q = max(wcQcapMin, (c.qcap+q)/2)
	}
	c.qcap, c.qcapAt, c.qcapOK = q, now, true
}

func (c *wcore) toHold(in wcIn, m wcMeas) {
	c.state = wcHold
	c.refPending = true
	c.refreshing = false
	c.growthN, c.wlN = 0, 0
	c.holdN = 0
	if c.probeAt.IsZero() || c.probeAt.Before(in.now) {
		c.schedProbe(in.now)
	}
	if c.refreshAt.IsZero() || c.refreshAt.Before(in.now) {
		c.schedRefresh(in.now)
	}
	if !c.lossDrain {
		c.setB(in, m, c.bHold(in.now, m))
	}
	c.settling = true
}

// toDrain takes B below the knee for an interval: by 10% of a round trip of
// what the path carries now for a queue; to at most half of B for loss
// (losses that keep themselves going stop only once what is held behind a
// hole, released at once, no longer overflows the queue); never above
// wcDrainShrink of where it was (under random loss R·τ_hold is about a round
// trip, and the formula took B up: 3.2 → 5.4 MiB on the rig). sig and q are
// what set it off (its line says so); a drain that cannot shrink B (at the
// floor) makes no event.
func (c *wcore) toDrain(in wcIn, m wcMeas, ev *[]wcEvent, why wcDrainWhy, sig wcSig, q time.Duration, rate bool) {
	from := c.B
	c.state = wcDrain
	c.refreshing = false
	c.schedRefresh(in.now)
	c.chgAt = in.now
	if why != wcWhyLoss {
		c.movedAt = in.now
	}
	t := c.tBase.Seconds()
	b := 0.9*m.rF*t + m.rF*c.tau.Seconds() + m.hStep
	if rate {
		c.rhat = append(c.rhat[:0], wcRate{in.now, m.rLink})
		c.backoff = 0
	} else if c.tau > c.dq() && why == wcWhyQueue {
		c.tau /= 2 // a queue HOLD itself built: τ_hold may hold a queue
	}
	if why == wcWhyLoss {
		b = math.Min(b, float64(from)/2)
		if c.policedAt.Equal(in.now) {
			b = 0 // a policer's loss (learnQcap): to the floor, as cut
		}
		c.lossDrain, c.lossDrainN = true, 0
	}
	b = math.Min(b, wcDrainShrink*float64(from))
	c.pathFull = true
	c.B = 0 // always settle into a drain
	c.setB(in, m, uint64(b))
	if c.B >= from {
		return // at the floor: nothing drained
	}
	switch why {
	case wcWhyLoss:
		c.cnt.drainLoss++
	case wcWhyRate:
		c.cnt.drainRate++
	default:
		c.cnt.drainQ++
	}
	*ev = append(*ev, wcEvent{kind: wcEvDrain, why: why, sig: sig, from: from, B: c.B, n: len(m.f), R: m.rF, T: c.tBase, q: q, loss: m.snd})
}

func (c *wcore) hold(in wcIn, m wcMeas, ev *[]wcEvent) {
	now := in.now
	dq := c.dq()
	if (c.lossDrain || c.refPending) && c.slowed(now, m) {
		// the path got slower, and its loss came first: a cut between
		// evaluations (fastStop) took B to half and HOLD's own test of a
		// slower path below never saw the drop (its rLinkHold is the first
		// measured HOLD interval's, after the cut), so R̂ kept the faster
		// rate for wcRhatWin and the loss drain's end took B back to a
		// budget of that rate (bHold, within bCap) into the standing queue:
		// lost again, cut again, 0.9 of the cap lower each time (red team,
		// round 2: 1000 → 100 Mbit/s behind 200 ms, 7 cuts in 10 s with
		// q_snd past 100 ms, the loss rule's streak 4; base 0%)
		c.lossDrain = false
		c.toDrain(in, m, ev, wcWhyRate, wcSigRate, 0, true)
		return
	}
	if c.lossDrain {
		// after a drain for loss, stay at its budget while the loss goes
		// on: a run of losses that keeps itself going stops only once
		// what was held behind its holes is through
		lossy := c.oooPast(m) ||
			m.sndOK && !c.sndStale(m.sndAt) && c.sndRef.past(m.snd, c.sndAbs(m))
		if lossy && c.policerLoss(m) {
			// a policer's loss going on at the budget the drain left: a
			// loss point too, and down to the floor
			c.notePoliced(now)
			c.noteCap(now, c.B)
			c.setB(in, m, 0)
		}
		if c.lossDrainN++; lossy && c.lossDrainN < 3 {
			return
		}
		c.lossDrain = false
		c.setB(in, m, c.bHold(now, m))
		return
	}
	if c.refPending {
		// the first measured interval of HOLD sets the references, unless
		// data still waited behind holes in it (that is no queue level)
		if c.oooPast(m) && c.holdN < 2 {
			c.holdN++
			return
		}
		c.refPending = false
		if m.qOK {
			c.qRef = m.qTurn
			c.capRef(now, m)
		}
		c.qSndRef, c.qSndRefOK = m.pathQueue()
		c.qOwnRef = m.qOwn
		c.rLinkHold = m.rLink
		c.holdR = [2]float64{m.rF, m.rF}
		c.holdN = 1
		if cong, jump, _ := c.lossVerdict(m, false); m.sameF && !cong && !jump {
			c.noteRate(now, m.rLink)
		}
		// no loss level from right after a drain: what it drained may
		// still be on its way
		c.lossTestDone(in, m, ev)
		c.setB(in, m, c.bHold(now, m))
		return
	}
	pq, pqOK := m.pathQueue()
	if !c.qSndRefOK && pqOK {
		c.qSndRef, c.qSndRefOK = pq, true
	}
	// the queue: q_turn on a clean path, q_own where data waits behind
	// holes (queueGrew); the exit sees the download queue itself (its srtt
	// has no wait behind holes in it): where its latest record shows none,
	// a longer turnaround or srtt is no queue
	qUp, sig, qv := c.queueGrew(m, 1)
	strong, _, _ := c.queueGrew(m, 3)
	sref := c.qSndRefOf(now, m)
	bare := pqOK && pq <= max(sref, 0)+dq/2
	if bare {
		qUp, strong = false, false
	}
	sndUp := pqOK && c.qSndRefOK && pq > sref+dq
	if !qUp && sndUp {
		sig, qv = wcSigSnd, pq-sref
	}
	if !qUp && !sndUp && m.ctrlUp {
		sig, qv = wcSigCtrl, m.ctrlQ
	}
	growth := qUp || sndUp || m.ctrlUp
	if growth {
		c.growthN++
	} else {
		c.growthN = 0
	}
	drop := c.rLinkHold > 0 && m.rLink < (1-wcRateDrop)*c.rLinkHold
	testing := !c.lossTestDone(in, m, ev)
	cong, jump, src := false, false, wcSrcNone
	if !testing {
		cong, jump, src = c.lossVerdict(m, growth)
	}
	own := m.qOwnOK && m.qOwn > c.qOwnRef+dq/2
	switch {
	case strong || c.growthN >= 2 || (growth && (cong || own)):
		why := wcWhyQueue
		switch {
		case drop:
			why, sig = wcWhyRate, wcSigRate
		case cong:
			why = wcWhyLoss
			c.learnQcap(now, m)
		}
		if !drop {
			c.rhat = append(c.rhat[:0], wcRate{now, m.rLink})
		}
		c.toDrain(in, m, ev, why, sig, qv, drop)
		return
	case cong:
		// loss that came with B (a probe or STARTUP whose loss the exit
		// reported late): back off, and probe less
		c.learnQcap(now, m)
		_, hi := c.bSpan()
		c.noteCap(now, hi)
		c.backoff++
		c.schedProbe(now)
		c.toDrain(in, m, ev, wcWhyLoss, wcLossSig(src), 0, false)
		return
	case jump:
		// loss past its level at this very budget: ours if it stops at
		// the floor (bursts that keep overflowing the queue; in stage 1 the
		// floor is the behaviour without the core), the path's if not
		c.lossTest, c.lossTestAt, c.lossTestB = src, now, c.B
		c.toDrain(in, m, ev, wcWhyLoss, wcLossSig(src), 0, false)
		c.B = 0
		c.setB(in, m, 0)
		return
	}
	c.holdN++
	if m.sameF {
		c.noteRate(now, m.rLink)
	}
	if !growth && !testing {
		c.noteLoss(m)
	}
	if c.lossyN >= wcLossRandN && !c.lossy {
		c.lossy = true
		*ev = append(*ev, wcEvent{kind: wcEvLossy, loss: m.snd, T: c.tBase, q: c.tau})
	}
	if c.lossy {
		c.cnt.lossy += time.Duration(m.dt * float64(time.Second))
	}
	if !growth {
		// the references follow the queue within Δq only: what the exit's
		// record (bare) or no growth let pass may still be the turnarounds
		// of the sender's PROBE_RTT (z12 P3, 300/150 q200: one interval's
		// q_turn of 468 ms took q_ref from 13 to 127 ms, the next probe
		// was judged against it, "paid" with a negative rise, and HOLD
		// kept 30–50 ms of standing queue for the rest of the run)
		if m.qOK && m.qTurn <= c.qRef+dq {
			c.qRef += time.Duration(wcRefAlpha * float64(m.qTurn-c.qRef))
			c.capRef(now, m)
		}
		if m.qOwnOK && m.qOwn <= c.qOwnRef+wcDqOwn {
			c.qOwnRef += time.Duration(wcRefAlpha * float64(m.qOwn-c.qOwnRef))
		}
		c.rLinkHold += 0.25 * (m.rLink - c.rLinkHold)
		if m.spreadOK {
			c.spread += time.Duration(0.25 * float64(m.spread-c.spread))
		}
		c.holdR[1], c.holdR[0] = c.holdR[0], m.rF
	}
	// window-limited in HOLD: the knee is past B (τ_hold too small, or the
	// path got faster): re-estimate, and probe without waiting
	// (on the edge, also: the exit's record shows no queue on the path,
	// which holds under random loss where holes hide the sender's pauses)
	empty := m.sndOK && m.qSndOK && m.qSnd <= dq // a record came in this interval
	if m.grp && m.sndOK && m.qPathOK {
		// with other cores on the peer the path holds their queue as it
		// should: empty is no more than that
		empty = m.qPath <= c.heldQ(m)+dq/2
	}
	wl := (m.hit >= wcHitShare || empty) && !growth && m.sameF
	if wl {
		c.wlN++
		if (m.rF < wcHoldSlack*c.rhatF(now, m) || c.wlN >= 2 || empty) && c.tauOK2(m) {
			c.noteTau(m)
		}
		if empty && !cong && !jump {
			c.bGood = max(c.bGood, c.B) // as far as it went, no queue came
		}
	} else {
		c.wlN = 0
	}
	// held by its window with no queue: the path has room (it got faster,
	// or a competitor left), and what was learned on it before (the cap,
	// the backoff) is stale: after three such intervals and wcCapStale since
	// the cap was confirmed; at once if the cap holds B at its floor and a
	// signal with no bias shows the queue empty (the exit's record of this
	// interval, or the link socket's own srtt): after a slower spell of the
	// path the rig sat 12 s at the floor, 85 Mbit/s against 250 before
	// (a path whose packets arrive behind holes at a steady level under
	// wcOooClean, reordering them, is clean while they stay at it: it never
	// showed ooo under wcOooCongAbs, and a cap a cut left held B for good)
	clean := !m.oooOK || m.ooo < wcOooCongAbs ||
		c.oooRef.ok && c.oooRef.v < wcOooClean && !c.oooRef.past(m.ooo, wcOooCongAbs) && !(m.sndOK && m.snd >= wcLossRand)
	unbiased := m.sndOK && m.qSndOK && m.qSnd <= dq/2 || m.qOwnOK && m.qOwn <= dq/2
	floor := uint64(max(len(m.f), 1)) * uint64(c.cmin())
	// (the backoff too where no cap is left: z12 P4, 300 → 50 → 300, the
	// cap was gone, B sat at the floor window-limited with q_snd 1.4 ms,
	// and the backoff of the 50 Mbit/s spell held the next probe 8 s off;
	// 2 of 3 runs never got back to 90%)
	atFloor := (c.bCap == 0 || c.bCap <= floor*21/20) && c.B <= floor*21/20 && (c.bCap > 0 || c.backoff > 0)
	// (not a policer's: below its rate a policed path always looks so)
	if !c.policed(now) && (c.wlN >= 2 && clean && unbiased && atFloor ||
		c.wlN >= 3 && !(m.qOK && m.qTurn > c.qRef+dq/2) && clean && now.Sub(c.bCapAt) >= wcCapStale) {
		c.bCap, c.backoff = 0, 0
	}
	if c.backoff == 0 && (c.wlN >= 2 || (m.qOK && c.qRef > dq && m.qTurn < c.qRef-dq)) && c.probeAt.After(now) {
		c.probeAt = now
	}
	hold := c.bHold(now, m)
	if !now.Before(c.refreshAt) && c.holdN >= 2 && !testing && now.Before(c.probeAt) && !in.apProbing {
		c.B = hold
		c.refresh(in, m)
		return
	}
	// (no probe while the cores of the peer share a queue that stands: the
	// path is full, the shared rule of bHold sets each one's share, and a
	// probe only takes the others' for an interval; its queue rise is a
	// share of probeQ, so it "paid" nearly always: 4 links' cores on 1000/100
	// behind 200 ms probed the shared queue from 15 to 30 ms and drained it
	// back, each every 4 s, in the model)
	_, shared := c.fairShare(now, m)
	if _, clean := c.longGrow(m); !now.Before(c.probeAt) && !in.apProbing && !c.capped && !c.burstHeld && c.holdN >= 2 && m.sameF && !testing && clean && !shared && !c.aboveShare(now, m) {
		pr := c.bProbe(now, m, hold)
		if in.poolFree < 0 || uint64(in.poolFree) >= pr-hold {
			c.state = wcProbe
			c.probeFrom = hold
			c.probeBase = (c.holdR[0] + c.holdR[1]) / 2
			c.probeQ0, c.probeOwn0 = c.qRef, c.qOwnRef
			c.B = 0
			c.setB(in, m, pr)
			if c.burstHeld && c.B <= hold {
				// the burst cap leaves no room to probe into
				c.state = wcHold
				c.probeAt = now.Add(wcProbeMin)
			}
			return
		}
		*ev = append(*ev, wcEvent{kind: wcEvPool, B: hold, n: len(m.f)})
		c.probeAt = now.Add(wcProbeMin)
	}
	c.setB(in, m, hold)
}

// slowed says whether the path got slower than R̂: the link's rate of the
// interval fell past wcRateDrop under the largest of R̂'s samples while a
// signal with no bias shows a standing queue past the core's target (a
// record of the exit that came since the latest drain, cut or start; else
// the link socket's own srtt). A budget cut to half on a path that is no
// slower drains its queue: there the rate fell with the windows, not the
// path.
func (c *wcore) slowed(now time.Time, m wcMeas) bool {
	if !m.sameF || len(c.rhat) == 0 || m.rLink <= 0 {
		return false
	}
	hi, n := 0.0, 0
	for i := len(c.rhat) - 1; i >= 0; i-- {
		if now.Sub(c.rhat[i].at) > wcRhatWin && n >= wcRhatMinN {
			break
		}
		hi = math.Max(hi, c.rhat[i].r)
		n++
	}
	if m.rLink >= (1-wcRateDrop)*hi {
		return false
	}
	switch {
	case m.sndOK && m.qSndOK && !c.sndStale(m.sndAt):
		return m.qSnd > c.qTarget(now)+c.dq()
	case m.qOwnOK:
		return m.qOwn > c.qTarget(now)+wcDqOwn
	}
	return false
}

// wcLossSig is the drain signal of a loss source.
func wcLossSig(src wcLossSrc) wcSig {
	if src == wcSrcOoo {
		return wcSigOoo
	}
	return wcSigLoss
}

// tauOK2: τ_hold learns from the interval only where its wait is no queue:
// on a clean path, or with q_own (no wait behind holes in it) at its
// reference. On the exit's side, with no record of the edge to tell, data
// held in a queue the base windows overflow looked like data held behind
// holes: τ_hold grew and B with it, to the loss rule's cut.
func (c *wcore) tauOK2(m wcMeas) bool {
	return !m.holey || m.qOwnOK && m.qOwn <= c.qOwnRef+wcDqOwn/2
}

// blind: data waits behind holes and no record of the sender's socket
// comes (the exit's core for uploads, WcoreUpload; an edge with an old exit
// never leaves OFF since the decision of step 1, off()). There
// Rcv_ooopack is no congestion signal (it reads the holes, ours or the
// path's: a run of losses of our own keeps its level past wcOooClean), no
// resend share tells them apart, and the only queue signal, the link
// socket's own srtt, reads the queue's floor (§3.3). An absent queue there
// is no sign that more window paid: only the rate is (red team, round 1:
// probes "paid" on q_own alone and took the windows of 300/150 behind a
// 20 ms queue to 13–31% resent, past the loss rule's lossFrac, with the
// windows without the core at 0.05–2%).
//
// The same where the exit's records stopped coming (sndQuiet: its stats
// stream stalled, or its replies wait behind a congested link and it is
// being reopened, statsReopenAfter): an edge that once had them was never
// blind again, and on 1000/100 behind 20 ms with 0.1% loss, records gone
// from 10 to 50 s, its probes and STARTUP took B from 6 to 16 MiB on q_own
// alone, 12-30% resent each second for 16 s (red team, round 2; base
// 0.1%).
func (c *wcore) blind(m wcMeas) bool { return m.holey && (!c.sndSeen || c.sndQuiet(m.at)) }

// sndQuiet: no record of the exit came for wcSndQuiet (it comes every
// ~2 s while the link moves data).
func (c *wcore) sndQuiet(now time.Time) bool {
	return !c.sndLastAt.IsZero() && now.Sub(c.sndLastAt) > wcSndQuiet
}

// lossTestDone resolves a drain that tested a jump of loss at a steady
// budget, once its source has measured after it: still past its level, the
// path turned lossy (the level moves there and it is random from now on);
// back to it, the losses were ours and stopped. It reports false while the
// test waits for its source.
func (c *wcore) lossTestDone(in wcIn, m wcMeas, ev *[]wcEvent) bool {
	switch c.lossTest {
	case wcSrcNone:
		return true
	case wcSrcSnd:
		if !m.sndOK || in.now.Sub(c.lossTestAt) < wcSndSpan {
			return false
		}
		if c.sndRef.past(m.snd, c.sndAbs(m)) {
			c.sndRef = wcLossRef{v: m.snd, b: c.B, ok: true}
			*ev = append(*ev, wcEvent{kind: wcEvLossy, loss: m.snd, T: c.tBase, q: c.tau})
		} else {
			c.noteCap(in.now, c.lossTestB)
		}
	case wcSrcOoo:
		if !m.oooOK {
			return false
		}
		if c.oooRef.past(m.ooo, wcOooCongAbs) {
			c.oooRef = wcLossRef{v: m.ooo, b: c.B, ok: true}
		} else {
			c.noteCap(in.now, c.lossTestB)
		}
	}
	c.lossTest = wcSrcNone
	return true
}

// probe judges a probe by the queue's answer, not the rate's: the rate of an
// interval varies by 11 to 24% (CV), more than a probe's 4 to 7% (of 61
// probes on the rig 34 went back). The probe added probeQ of queue at R̂ to
// the windows: if the path had room for it the queue does not grow (the
// sender sends faster), if not it grows by about probeQ. So it is kept if
// the queue (q_turn above the q_ref it began at; q_own above its own where
// data waits behind holes; none where a record of the exit that came in the
// probe shows none) grew by less than probeQ/2 and no loss of ours came, and
// what of it went into the queue is taken off the budget found to pay
// (bGood); else B goes back to where the probe began. A probe none of whose
// window became queue, with the rate up, goes back to STARTUP.
func (c *wcore) probe(in wcIn, m wcMeas, ev *[]wcEvent) {
	now := in.now
	if !m.sameF || in.poolShort > 0 {
		// inconclusive: F changed, or the pool gave less than asked
		if in.poolShort > 0 {
			*ev = append(*ev, wcEvent{kind: wcEvPool, B: c.B, n: len(m.f), pool: true})
		}
		c.toHold(in, m)
		c.setB(in, m, min(c.B, c.probeFrom))
		return
	}
	rise, ok := time.Duration(0), false
	if m.holey {
		if m.qOwnOK {
			rise, ok = m.qOwn-c.probeOwn0, true
		}
	} else if m.qOK {
		rise, ok = m.qTurn-c.probeQ0, true
	}
	if q, qok := m.pathQueue(); m.sndOK && qok && q <= max(c.qSndRefOf(now, m), 0)+c.dq()/2 {
		rise, ok = min(rise, 0), true // the exit's record of the probe shows no queue
	}
	pq := c.probeQ(now)
	qUp := ok && rise >= pq/2
	cong, jump, _ := c.lossVerdict(m, qUp)
	if judged, clean := c.longGrow(m); !cong && !clean {
		// past wcLongRTT a probe pays only on a fresh record of the exit
		// that shows no queue and no new resends; with none it is no
		// verdict (back, no backoff)
		c.probeFailed(in, m, ev, false, judged)
		return
	}
	if !cong && !jump {
		c.noteRate(now, m.rLink)
	}
	exp := float64(c.B-min(c.probeFrom, c.B)) / math.Max(float64(c.probeFrom)-m.hStep, 1)
	gain := m.rF/math.Max(c.probeBase, 1) - 1
	// on a clean path that has overflowed before (Q_cap known: a shallow
	// queue), the rate must have grown too: there the sender's bursts (3.9
	// times the path's rate) leave the queue empty between them and the
	// first byte past each limit leads its burst, so the turnaround shows no
	// queue until the bursts already overflow (the model: every probe kept
	// up to the next loss, 1.2–1.6% resent where rate verdicts left 0.4–0.9%)
	paid := (m.holey || !c.qcapKnown(now)) && !c.blind(m) || gain >= math.Max(wcPayMin, wcPayShare*exp)
	if cong || jump || !ok || qUp || !paid {
		c.probeFailed(in, m, ev, cong, ok)
		return
	}
	c.cnt.probeOK++
	c.backoff = 0
	c.pathFull = false
	r := c.rhatF(now, m)
	good := float64(c.B) - math.Max(rise.Seconds(), 0)*r
	c.bGood = max(c.bGood, uint64(math.Max(good, 0)))
	if c.bCap > 0 && c.B > c.bCap {
		if floor := uint64(max(len(m.f), 1)) * uint64(c.cmin()); c.bCap <= floor*21/20 && !c.policed(now) {
			// a cap at the floor came from a slower spell of the path (no
			// budget of ours above the windows without the core lost): a
			// probe past it that paid says the spell is over (z12 P2, 300 →
			// 50 → 300: raised to the probe's budget, the cap held STARTUP
			// there and the rate stayed at 100 Mbit/s for the 12 s after the
			// path was fast again)
			c.bCap, c.backoff = 0, 0
		} else {
			c.bCap = c.B // as clean as far as the probe went
		}
	}
	if rise <= c.dq()/2 && c.tauOK2(m) {
		c.noteTau(m)
	}
	c.schedProbe(now)
	if rise >= pq/4 || (!m.holey || c.blind(m)) && gain < math.Max(2*wcPayMin, wcSuccStartup*exp) {
		c.probeWins = 0
		c.toHold(in, m)
		return
	}
	// none of it queued and the rate went up by most of what the probe
	// could bring (where data waits behind holes, the queue alone tells:
	// there the rate of an interval says nothing): probe again soon; twice
	// in a row, the path has room (it got faster, a competitor left, or
	// more window covers more holes): grow on, from the smallest step (one
	// interval's rate says little: its CV is 0.1–0.2; and a probe right at
	// the knee queues little too)
	if c.probeWins++; c.probeWins >= 2 {
		c.probeWins = 0
		c.state = wcStartup
		c.fullR, c.prevR, c.prevB, c.flat, c.gear = m.rF, m.rF, c.B, 0, 0
		c.setB(in, m, uint64(wcGears[0]*math.Min(float64(c.B), c.knee(m.rF, m))))
		return
	}
	c.toHold(in, m)
	settle, measure := c.interval()
	c.probeAt = now.Add(2 * (settle + measure))
}

// probeFailed takes B back to where the probe began (to a drain for loss,
// learning Q_cap), and probes less often. A probe that could not be judged
// (no queue signal at all) goes back without backing off.
func (c *wcore) probeFailed(in wcIn, m wcMeas, ev *[]wcEvent, cong, judged bool) {
	c.cnt.probeFail++
	c.probeWins = 0
	c.pathFull = true
	if judged {
		c.backoff++
	}
	c.schedProbe(in.now)
	if cong {
		c.learnQcap(in.now, m)
		c.noteCap(in.now, c.B)
		c.toDrain(in, m, ev, wcWhyLoss, wcSigLoss, 0, false)
		return
	}
	// back to where it started: what R̂ took from the probe does not keep
	// part of a budget that did not pay
	c.toHold(in, m)
	c.setB(in, m, min(c.B, c.probeFrom))
}

// out builds the ceilings: B shared over F (wcSplit), nothing for the rest.
func (c *wcore) out(in wcIn, now time.Time, ev []wcEvent) wcOut {
	// active, the core sees the link at every runner wake (one TCP_INFO):
	// a ramp moves on, and loss is acted on within a turnaround
	next := c.evalAt
	if c.state != wcOff && now.Add(wcRampTick).Before(next) {
		next = now.Add(wcRampTick)
	}
	o := wcOut{state: c.state, B: c.bPub, next: next, events: ev, growOK: true}
	o.ceils = make([]wcCeil, len(in.streams))
	for i, s := range in.streams {
		o.ceils[i].id = s.id
	}
	if c.state == wcOff || c.bPub == 0 {
		clear(c.ceils)
		return o
	}
	var idx []int
	var demand []float64
	anyHit := false
	for _, s := range in.streams {
		if c.inF[s.id] && s.upd > 0 && float64(s.hit) >= wcHitShare*float64(s.upd) {
			anyHit = true
		}
	}
	for i, s := range in.streams {
		if !c.inF[s.id] {
			continue
		}
		d := math.Inf(1)
		if anyHit && !(s.upd > 0 && float64(s.hit) >= wcHitShare*float64(s.upd)) {
			d = wcDemandGain * float64(max(s.win, c.ceils[s.id]))
		}
		idx = append(idx, i)
		demand = append(demand, d)
	}
	lo := c.cmin()
	share := wcSplit(max(c.bPub, uint64(len(idx))*uint64(lo)), demand, lo, c.cfg.maxWin)
	clear(c.ceils)
	any := false
	for k, i := range idx {
		v := share[k]
		c.ceils[in.streams[i].id] = v
		// at need or below: the behaviour without the core (no brake), until
		// stage 2 found the path full
		if v <= c.cfg.need && !(c.cfg.stage >= 2 && c.pathFull) {
			v = 0
		}
		o.ceils[i].ceil = v
		if v > 0 {
			any = true
		}
	}
	if any {
		o.turnRef = c.turnMins.min(now, wcTurnWin) + c.qRef
		// (a brake is a queue over the least turnaround of the last
		// wcTurnWin: never past that window, whatever round trips and
		// turnarounds the core was handed; TestRedWcoreHostileInputs)
		o.qBrake = min(max(c.probeQ(now), c.dq())+2*c.spread, wcTurnWin)
		// (not held while the autopilot probes the pool size: its freeze is
		// no PROBE and no STARTUP, so B stays within what was shown to pay,
		// bGood; a growth brake for the spell kept each drain, cut or
		// refresh of it for the whole spell: 800 → 160 Mbit/s on 1000/100
		// in the model, and the autopilot measured that)
		o.growOK = c.state != wcDrain && c.parkHold == 0
	}
	return o
}

// wcStreamSnap is one stream's counters as the smux arm keeps them
// (smux.CoreSample: ID, Win, Read, Upds, Hits, Lags, TurnMin, TurnLast,
// Braked): cumulative, except turnMin and turnLast, which the snapshot
// swaps out (what came since the previous one). Session.CoreSnapshot leaves
// out streams under need/4 with no fresh hit: one that comes back counts
// from that snapshot on.
type wcStreamSnap struct {
	id                uint32
	readTot           uint64
	updN, hitN, lagN  uint32
	turnMin, turnLast time.Duration
	win               uint32
	braked            bool
}

// wcLinkSnap is one snapshot of a link: cumulative counters of its socket
// (TCP_INFO), its session (smux.CoreCounters: Parks, Borrows, Brakes,
// PoolShort, Rcvd) and its large streams, the window pool's free bytes
// (WindowPool.Free), the edge's control ping and the exit's latest stats
// record.
type wcLinkSnap struct {
	now                    time.Time
	minRTT, srtt           time.Duration
	segsIn, ooo            uint64
	oooOK                  bool
	tcpNone                bool // TCP_INFO could not be read: no counters of the socket in this one
	rcvd                   uint64
	parks, borrows, brakes uint32 // a borrow from the pool counts as a park too
	poolShort              uint32
	poolFree               int64
	streams                []wcStreamSnap
	ctrlMin                time.Duration
	pongs                  []time.Duration // pongs since the previous snapshot
	sndAt                  time.Time       // when the exit's latest record came (zero: none)
	sndSrtt, sndMinRTT     time.Duration   // srtt 0: an old exit, or no TCP_INFO there (no q_snd)
	sndRetrans, sndSegsOut uint64
	apProbing              bool
}

// wcBuilder turns snapshots into the deltas of wcIn, for the runner and the
// model alike. A stream it has not seen before counts from that snapshot
// on, the first exit record only sets the base of the next.
type wcBuilder struct {
	have            bool
	prev            wcLinkSnap
	tcpHave         bool // tcpSegs, tcpOoo are the last counters TCP_INFO gave
	tcpSegs, tcpOoo uint64
	prevS           map[uint32]wcStreamSnap
	sndAt           time.Time
	sndRet, sndSegs uint64
}

func wcDelta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func (b *wcBuilder) build(s wcLinkSnap) wcIn {
	in := wcIn{now: s.now, minRTT: s.minRTT, srtt: s.srtt, oooOK: s.oooOK, poolFree: s.poolFree,
		ctrlMin: s.ctrlMin, pongs: s.pongs, apProbing: s.apProbing}
	if s.tcpNone {
		// no TCP_INFO this time: nothing of the socket, and the next one
		// counts from the last that had it (from 0, one wake's delta was
		// the link's lifetime)
		in.minRTT, in.srtt, in.oooOK = 0, 0, false
	} else {
		if b.tcpHave {
			in.segsIn = wcDelta(s.segsIn, b.tcpSegs)
			in.ooo = wcDelta(s.ooo, b.tcpOoo)
		}
		b.tcpHave, b.tcpSegs, b.tcpOoo = true, s.segsIn, s.ooo
	}
	if b.have {
		in.linkBytes = wcDelta(s.rcvd, b.prev.rcvd)
		// the reader waited for buffer space: a borrow from the pool
		// counts as a park as well, and is none
		if p, br := s.parks-b.prev.parks, s.borrows-b.prev.borrows; p > br {
			in.parks = p - br
		}
		in.brakes = s.brakes - b.prev.brakes
		in.poolShort = s.poolShort - b.prev.poolShort
	}
	next := make(map[uint32]wcStreamSnap, len(s.streams))
	in.streams = make([]wcStream, 0, len(s.streams))
	for _, st := range s.streams {
		w := wcStream{id: st.id, win: st.win, braked: st.braked, turnMin: st.turnMin, turnLast: st.turnLast}
		if p, ok := b.prevS[st.id]; ok {
			w.read = wcDelta(st.readTot, p.readTot)
			w.upd = st.updN - p.updN
			w.hit = st.hitN - p.hitN
			w.lag = st.lagN - p.lagN
		}
		in.streams = append(in.streams, w)
		next[st.id] = st
	}
	b.prevS = next
	if !s.sndAt.IsZero() && !s.sndAt.Equal(b.sndAt) {
		in.snd = wcSnd{fresh: true, srtt: s.sndSrtt, minRTT: s.sndMinRTT}
		if s.sndSrtt == 0 && s.sndRetrans == 0 && s.sndSegsOut == 0 && b.sndSegs > 0 {
			// the exit's tcpStats failed for this poll (serveStats sends
			// the extended fields 0): its counters are no reading, and the
			// next record counts from the last one that had them (from 0,
			// its resend share was the link's lifetime one)
			b.sndAt = s.sndAt
		} else {
			if !b.sndAt.IsZero() {
				in.snd.retrans = wcDelta(s.sndRetrans, b.sndRet)
				in.snd.segsOut = wcDelta(s.sndSegsOut, b.sndSegs)
			}
			b.sndAt, b.sndRet, b.sndSegs = s.sndAt, s.sndRetrans, s.sndSegsOut
		}
	}
	b.prev, b.have = s, true
	return in
}
