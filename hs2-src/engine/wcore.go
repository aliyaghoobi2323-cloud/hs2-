package engine

import (
	"math"
	"sort"
	"time"
)

// wcore is the pure decision core of the window core (Phase Z5,
// docs/architecture/WINDOW_CORE.fa.md): one per link and per direction its
// end receives (the edge's downloads, the exit's uploads). A measurement
// interval in, the link's window budget and each fast stream's ceiling out.
// It holds no locks, opens no sockets and never reads the clock (time and
// randomness come in through wcIn and c.rnd), so its whole behaviour is
// exercised by the link model in wcore_model_test.go; the actuator (the
// runner, one per link, with wcBuilder) comes at integration.
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
	sndSeen    bool // the exit sends its record (an edge with a new exit)

	// paths
	tBase    time.Duration // T_base: the link socket's min_rtt (or the fallbacks)
	srttMins wcMinWin      // fallback: least srtt over 60 s
	turnMins wcMinWin      // turnBase: least window turnaround over 60 s
	tbOff    time.Duration // turnBase − T_base while both agreed (route change)
	routeLog bool

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

	ceils map[uint32]uint32
	cnt   wcCounters
	last  wcMeas // the latest interval's measurements (lab trace)
}

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
	wcLossCongAbs    = 0.003 // congestive: +0.3 points ...
	wcLossCongRel    = 2.0   // ... or twice the HOLD level, whichever is more
	wcOooCongAbs     = 0.01  // the same on Rcv_ooopack (a share of packets, not of losses)
	wcOooClean       = 0.10  // ooo is a congestion signal only on a path that is otherwise clean
	wcLossRand       = 0.0005
	wcLossRandN      = 3
	wcSndSpan        = 2500 * time.Millisecond // a record covers the exit's last stats tick
	wcRhatWin        = 10 * time.Second
	wcRhatMinN       = 8
	wcTauAlpha       = 0.25
	wcRefAlpha       = 0.25
	wcProbeMin       = 4 * time.Second
	wcProbeMax       = 8 * time.Second
	wcProbeRTTs      = 10
	wcProbeBackMax   = 32 * time.Second
	wcQcapTTL        = 10 * time.Minute
	wcTurnWin        = 60 * time.Second
	wcKPrevTTL       = 30 * time.Second
	wcOffAfter       = 3
	wcCtrlQMin       = 30 * time.Millisecond
	wcCtrlQShare     = 1.0 / 3
	wcRateDrop       = 0.3
	wcHoldSlack      = 0.85
	wcStage2Min      = uint32(256 << 10)
	wcParkHold       = 2
	wcHitShare       = 0.5
	wcLagShare       = 0.25
	wcUseShare       = 0.5
	wcPeerShare      = 0.25 // a stream joins F on use alone only at this share of the fastest
	wcDemandGain     = 1.25
	wcResettle       = 0.05 // a change of B by more than this share settles first
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
	wcOffWait   = 8                // OFF waits this many intervals at most for the exit's first loss level
	wcCapStale  = 15 * time.Second // a cap confirmed by losses this recently is no sign of a faster path
	// wcRefreshMin, wcRefreshMax: how often HOLD lets its standing queue
	// empty for an interval. BBRv1 keeps the least round trip of 10 s and,
	// when no sample reached it in that time, sends nothing past 4 packets
	// for 200 ms (PROBE_RTT); a sender held by its window a standing queue
	// past the knee never sees the bare round trip, and after its PROBE_RTT
	// it sent all the credit it had gathered at 2.89 times its rate.
	wcRefreshMin = 7 * time.Second
	wcRefreshMax = 9 * time.Second
)

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
	from uint64 // B before
	B    uint64
	n    int
	R    float64 // bytes/s
	T    time.Duration
	q    time.Duration
	loss float64
}

// wcCounters are the link's totals (linkStallNote).
type wcCounters struct {
	grow, probeOK, probeFail     int
	drainQ, drainLoss, drainRate int
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
// window each.
type wcMinWin struct {
	b  [6]time.Duration
	ep [6]int64 // the bucket's epoch (now / (win/6))
}

func (w *wcMinWin) add(now time.Time, v, win time.Duration) {
	if v <= 0 {
		return
	}
	e := now.UnixNano() / int64(win/6)
	i := int(e % 6)
	if w.ep[i] != e || w.b[i] == 0 || v < w.b[i] {
		if w.ep[i] != e {
			w.b[i] = v
		} else {
			w.b[i] = min(w.b[i], v)
		}
		w.ep[i] = e
	}
}

func (w *wcMinWin) min(now time.Time, win time.Duration) time.Duration {
	e := now.UnixNano() / int64(win/6)
	m := time.Duration(0)
	for i, v := range w.b {
		if v > 0 && e-w.ep[i] < 6 && (m == 0 || v < m) {
			m = v
		}
	}
	return m
}

func newWcore(cfg wcConfig, rnd func() float64) *wcore {
	if cfg.stage < 1 {
		cfg.stage = 1
	}
	return &wcore{cfg: cfg, rnd: rnd, inF: map[uint32]bool{}, prevWin: map[uint32]uint32{}, ceils: map[uint32]uint32{}}
}

// wcStep is the window update step of a window w: half of it, or, past
// need, need/2 or an eighth of it if that is more, never more than half.
// It is smux's StepOf (third_party/smux), which the fast arm uses; at
// integration this calls it.
func wcStep(need, w uint32) uint32 {
	half := w / 2
	if need == 0 || w <= need {
		return half
	}
	s := need / 2
	if f := w / 8; f > s {
		s = f
	}
	if s > half {
		s = half
	}
	return s
}

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
	ooo      float64
	oooOK    bool
	spread   time.Duration
	spreadOK bool
	ctrlUp   bool
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
	if c.accOK {
		wcMerge(&c.acc, in)
	} else {
		c.acc = wcCopy(in)
		c.accOK = true
	}
	if !c.evalAt.IsZero() && now.Before(c.evalAt) {
		c.fastStop(in)
		c.ramp()
		return c.out(c.acc, now, nil)
	}
	in, c.accOK = c.acc, false
	stopQ := c.stopQ
	c.stopQ = false
	var ev []wcEvent
	dt := 0.0
	if !c.lastAt.IsZero() {
		dt = now.Sub(c.lastAt).Seconds()
	}
	c.lastAt = now
	c.notePaths(in, &ev)
	if in.snd.fresh && (in.snd.srtt > 0 || in.snd.segsOut > 0) {
		c.sndSeen = true // (an old exit's record has no TCP_INFO in it)
	}
	m := c.measure(in, dt)
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

	switch {
	case c.state == wcOff:
		c.off(in, m, &ev)
	case c.fEmpty >= wcOffAfter:
		c.toOff()
	case len(m.f) == 0:
		// the fast streams paused: keep the state, judge nothing (a probe
		// is over: it measured nothing, back to where it started)
		c.settling = false
		if c.state == wcProbe {
			c.toHold(in, m)
			c.setB(in, m, min(c.B, c.probeFrom))
		}
	case c.settling:
		c.settled(in, m, &ev)
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
func (c *wcore) fastStop(in wcIn) {
	if c.state == wcOff || c.bPub == 0 {
		return
	}
	if in.oooOK && in.segsIn > 0 && c.oooRef.ok && c.oooRef.v < wcOooClean &&
		c.oooRef.past(float64(in.ooo)/float64(in.segsIn), wcOooCongAbs) && !c.lossDrain {
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
	// the queue held less than the standing queue and the bursts on it:
	// keep half the standing queue from now on
	q := c.qTarget(now)
	c.qcap, c.qcapAt, c.qcapOK = q*3/2, now, true
	c.pathFull = true
	c.state = wcDrain
	c.refreshing = false
	c.lossDrain, c.lossDrainN = true, 0
	n := uint64(max(len(c.inF), 1))
	c.B = max(from/2, n*uint64(c.cmin()))
	c.bPub = c.B
	c.cuts++
	// judge from a turnaround on
	c.evalAt = now.Add(c.tBase + wcRampTick)
	c.settling = false
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

// wcCopy copies an input (its slices too).
func wcCopy(in wcIn) wcIn {
	in.streams = append([]wcStream(nil), in.streams...)
	in.pongs = append([]time.Duration(nil), in.pongs...)
	return in
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
			o.win, o.braked = s.win, s.braked
			break
		}
		if !found {
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
	if t > 0 {
		switch {
		case tb > 0 && float64(tb) <= 1.3*float64(t):
			c.tbOff = tb - t
			c.routeLog = false
		case tb > 0 && c.tbOff > 0 && float64(tb) > 1.3*float64(t) && c.qSnd <= c.dq():
			// the path got longer: min_rtt keeps the old floor for up to
			// 5 minutes, turnaround has moved
			t = tb - c.tbOff
			if !c.routeLog {
				c.routeLog = true
				*ev = append(*ev, wcEvent{kind: wcEvRoute, T: t})
			}
		}
		c.tBase = t
	}
	if in.snd.fresh && in.snd.minRTT > 0 && in.snd.srtt >= in.snd.minRTT {
		c.qSnd, c.qSndAt = in.snd.srtt-in.snd.minRTT, now
	}
}

// measure classifies the streams and takes the interval's estimators.
//
// A stream joins F (the fast streams) when its window is at least need/2,
// its reader keeps up, and the sender used all of its window in half its
// update cycles or more (or its window is in use, R·turnaround ≥ win/2, and
// it is about as fast as the fastest stream of the link: a slow source also
// "uses" its window, filling it as slowly as it is written). It stays in F
// while it reads and its reader keeps up: right after B grows the sender has
// a window it has not filled yet, which is no reason to take the stream's
// ceiling away. It leaves when its source got slow: no update of an interval
// in a window-limited cycle, every turnaround past twice turnBase, a rate
// under a quarter of what its window allows in a round trip, and (on the
// edge) the exit's record showing no queue.
func (c *wcore) measure(in wcIn, dt float64) wcMeas {
	m := wcMeas{dt: dt}
	if dt <= 0 {
		return m
	}
	need := c.cfg.need
	maxR := 0.0
	for _, s := range in.streams {
		maxR = math.Max(maxR, float64(s.read)/dt)
	}
	newF := map[uint32]bool{}
	var turns, spreads []time.Duration
	sndBare := c.cfg.side == wcExit || !c.qSndAt.IsZero() && in.now.Sub(c.qSndAt) <= wcSndSpan && c.qSnd <= c.dq()
	hits, upds := 0.0, 0.0
	for i, s := range in.streams {
		r := float64(s.read) / dt
		if s.upd == 0 || r <= 0 || s.braked || float64(s.lag) > wcLagShare*float64(s.upd) {
			continue
		}
		if tb := c.turnMins.min(in.now, wcTurnWin); c.inF[s.id] && s.hit == 0 && s.upd >= 2 && tb > 0 && s.turnMin > 2*tb &&
			r*tb.Seconds() < float64(s.win)/4 && sndBare {
			// its source got slow: never all of the window in use, a
			// turnaround that is how long the source takes to fill it, a
			// rate far under what the window allows, and no queue at the
			// exit to explain it (a path that slowed down queues there)
			continue
		}
		if !c.inF[s.id] {
			hit := float64(s.hit) >= wcHitShare*float64(s.upd)
			turn := s.turnLast
			if turn <= 0 {
				turn = s.turnMin
			}
			used := turn > 0 && r*turn.Seconds() >= wcUseShare*float64(s.win)
			// by use only with no standing queue in its turnaround: a
			// window the path is too full for is in use too
			tb := c.turnMins.min(in.now, wcTurnWin)
			bare := tb > 0 && s.turnMin > 0 && s.turnMin <= tb+c.dq()
			if s.win < need/2 || !(hit || used && bare && r >= wcPeerShare*maxR) {
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
		if s.turnMin > 0 {
			turns = append(turns, s.turnMin)
			if s.turnLast >= s.turnMin {
				spreads = append(spreads, s.turnLast-s.turnMin)
			}
		}
	}
	m.sameF = len(newF) == len(c.inF)
	for id := range newF {
		if !c.inF[id] {
			m.sameF = false
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
	if tb := c.turnMins.min(in.now, wcTurnWin); tb > 0 && len(turns) > 0 {
		m.qTurn, m.qOK = max(0, pct25(turns)-tb), true
	}
	if len(spreads) > 0 {
		m.spread, m.spreadOK = pct50(spreads), true
	}
	if in.srtt > 0 && c.tBase > 0 {
		m.qOwn, m.qOwnOK = max(0, in.srtt-c.tBase), true
	}
	if !c.qSndAt.IsZero() && in.now.Sub(c.qSndAt) <= wcSndSpan {
		m.qSnd, m.qSndOK = c.qSnd, true
	}
	if in.snd.fresh && in.snd.segsOut > 0 {
		m.snd, m.sndOK = float64(in.snd.retrans)/float64(in.snd.segsOut), true
	}
	if in.oooOK && in.segsIn > 0 {
		m.ooo, m.oooOK = float64(in.ooo)/float64(in.segsIn), true
	}
	// the control ping: two pongs in a row well over the channel's floor,
	// on a link with no loss to explain it (a pong waits behind holes)
	thr := max(wcCtrlQMin, time.Duration(float64(c.tBase)*wcCtrlQShare))
	for _, p := range in.pongs {
		if in.ctrlMin > 0 && p-in.ctrlMin > thr {
			c.pongHi++
		} else {
			c.pongHi = 0
		}
	}
	lossFree := (!m.sndOK || m.snd < wcLossRand) && (!m.oooOK || m.ooo < wcLossRand) && !c.lossy
	m.ctrlUp = c.pongHi >= 2 && lossFree
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
	b := uint64(c.knee(r, m) + h*r*t)
	if c.bCap > 0 && now.Sub(c.bCapAt) < wcQcapTTL {
		b = min(b, c.bCap)
	}
	// past what was shown to pay only by the margin: τ_hold and R̂ alone
	// (the delay behind holes the window itself may cause) never take B
	// into budgets nothing has tried
	lim := uint64(float64(c.bGood) * (1 + h))
	return min(b, max(lim, uint64(max(len(m.f), 1))*uint64(c.cmin())))
}

// noteCap remembers that losses of our own came at budget b.
func (c *wcore) noteCap(now time.Time, b uint64) {
	cp := uint64(wcCapBack * float64(b))
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
// window pool can cover, and whether the change must settle first.
func (c *wcore) setB(in wcIn, m wcMeas, b uint64) {
	n := uint64(max(len(m.f), 1))
	lo := n * uint64(c.cmin())
	hi := n * uint64(c.cfg.maxWin)
	if in.poolFree >= 0 {
		held := 0.0
		for _, i := range m.f {
			if w := in.streams[i].win; w > c.cfg.need {
				held += float64(w - c.cfg.need)
			}
		}
		if p := n*uint64(c.cfg.need) + uint64(held) + uint64(in.poolFree); p < hi {
			hi = p
		}
	}
	hi = max(hi, lo)
	b = min(max(b, lo), hi)
	c.capped = b >= hi && hi > lo
	if c.B == 0 || math.Abs(float64(b)-float64(c.B)) > wcResettle*float64(c.B) {
		c.settling = true
	}
	c.B = b
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
// trip: it is judged only on a path that is otherwise clean.
func (c *wcore) lossVerdict(m wcMeas, growth bool) (cong, jump bool, src wcLossSrc) {
	if m.sndOK && c.sndRef.past(m.snd, wcLossCongAbs) {
		_, hi := c.bSpan()
		if growth || float64(hi) > 1.05*float64(c.sndRef.b) {
			return true, false, wcSrcSnd
		}
		jump, src = true, wcSrcSnd
	}
	if m.oooOK && c.oooRef.ok && c.oooRef.v < wcOooClean && c.oooRef.past(m.ooo, wcOooCongAbs) {
		// c.B is still the budget the interval was measured under
		if growth || float64(c.B) > 1.05*float64(c.oooRef.b) {
			return true, false, wcSrcOoo
		}
		jump, src = true, wcSrcOoo
	}
	return false, jump, src
}

// noteLoss keeps the loss levels of an interval that was not judged
// congestive, with the budget they were measured at, and the random-loss
// flag.
func (c *wcore) noteLoss(m wcMeas) {
	if m.sndOK {
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
	w := (m.wF + m.wPrev) / 2
	s := time.Duration(((w-m.hStep)/m.rF - c.tBase.Seconds()) * float64(time.Second))
	s = min(max(s, 0), 2*c.tBase)
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

// off: no ceilings. A fast stream with T_base known starts STARTUP.
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
	if len(m.f) == 0 || in.apProbing || math.Abs(m.wF-m.wPrev) > 0.1*m.wF {
		c.offN = 0
		return
	}
	// the windows without the core are what STARTUP's first step is judged
	// against: wait until they stopped growing, and (on the edge) for the
	// exit's first resend share with them
	if c.offN++; c.offN < 2 || c.sndSeen && !c.sndRef.ok && c.offN < wcOffWait {
		return
	}
	// the windows without the core already make a queue at the exit: the
	// knee is below them, there is nothing for STARTUP to find (in stage 1
	// nothing under them is ours to change)
	// (a standing queue, or loss with a queue behind it: random loss
	// leaves no queue)
	if c.cfg.stage < 2 && m.qSndOK && (m.qSnd > c.qTarget(in.now)+c.dq() ||
		c.sndRef.ok && c.sndRef.v > wcLossCongAbs && m.qSnd >= c.dq()) {
		c.offN = 0
		return
	}
	c.offN = 0
	c.bGood = 0
	c.state = wcStartup
	c.backoff = 0
	c.pathFull = false
	c.flat = 0
	c.gear = 1
	c.fullR, c.prevR = m.rF, m.rF
	c.qRef = m.qTurn
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

func (c *wcore) toOff() {
	c.lossDrain = false
	if c.state == wcHold || c.state == wcProbe {
		c.kPrev, c.kPrevAt = c.B, c.lastAt
	}
	c.state = wcOff
	c.B = 0
	c.settling = false
	c.refPending = false
	c.capped = false
	c.lossTest = wcSrcNone
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
				c.probeFailed(in, m, ev, true)
			} else {
				c.endStartup(in, m, ev, true, true)
			}
		} else if in.apProbing {
			c.toHold(in, m)
		}
	case wcDrain:
		if c.lossTest == wcSrcNone && m.qOK && m.qTurn < c.qRef+c.dq()/2 {
			c.toHold(in, m)
		}
	}
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
	qUp := m.qOK && m.qTurn > c.qRef+c.dq()
	if qUp && m.oooOK && m.ooo >= wcOooClean && m.qSndOK {
		// data waits behind holes (a lossy path): a longer turnaround is
		// no queue level; the exit's next record tells whether there is
		// one (keep B until it comes)
		if !m.sndOK {
			return
		}
		if m.qSnd <= c.dq()/2 {
			qUp = false
		}
	}
	cong, jump, _ := c.lossVerdict(m, qUp)
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
	if tracked && !qUp && !cong {
		c.bGood = max(c.bGood, c.B)
	}
	if m.qOK && !qUp && m.qTurn < c.qRef {
		c.qRef = m.qTurn
	}
	if cong || c.flat >= wcFullRounds || (qUp && c.flat >= 1) || in.apProbing || (c.capped && c.flat >= 1) {
		c.endStartup(in, m, ev, cong || qUp, cong)
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
		*ev = append(*ev, wcEvent{kind: wcEvCapped, B: from, n: len(m.f), T: c.tBase})
	}
	if drain {
		why := wcWhyQueue
		if cong {
			why = wcWhyLoss
			c.learnQcap(in.now, m)
			c.noteCap(in.now, from)
		}
		c.toDrain(in, m, ev, why, false)
		return
	}
	c.toHold(in, m)
}

func (c *wcore) learnQcap(now time.Time, m wcMeas) {
	q := max(0, m.qTurn-c.qRef)
	if !c.qcapKnown(now) || q < c.qcap {
		c.qcap = q
	}
	c.qcapAt, c.qcapOK = now, true
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
// hole, released at once, no longer overflows the queue).
func (c *wcore) toDrain(in wcIn, m wcMeas, ev *[]wcEvent, why wcDrainWhy, rate bool) {
	from := c.B
	c.state = wcDrain
	c.refreshing = false
	c.schedRefresh(in.now)
	t := c.tBase.Seconds()
	b := 0.9*m.rF*t + m.rF*c.tau.Seconds() + m.hStep
	if rate {
		c.rhat = append(c.rhat[:0], wcRate{in.now, m.rLink})
		c.backoff = 0
	} else if c.tau > c.dq() && why == wcWhyQueue {
		c.tau /= 2 // a queue HOLD itself built: τ_hold may hold a queue
	}
	switch why {
	case wcWhyLoss:
		c.cnt.drainLoss++
		b = math.Min(b, float64(from)/2)
		c.lossDrain, c.lossDrainN = true, 0
	case wcWhyRate:
		c.cnt.drainRate++
	default:
		c.cnt.drainQ++
	}
	c.pathFull = true
	c.B = 0 // always settle into a drain
	c.setB(in, m, uint64(b))
	*ev = append(*ev, wcEvent{kind: wcEvDrain, why: why, from: from, B: c.B, n: len(m.f), R: m.rF, T: c.tBase, q: m.qTurn - c.qRef, loss: m.snd})
}

func (c *wcore) hold(in wcIn, m wcMeas, ev *[]wcEvent) {
	now := in.now
	dq := c.dq()
	if c.lossDrain {
		// after a drain for loss, stay at its budget while the loss goes
		// on: a run of losses that keeps itself going stops only once
		// what was held behind its holes is through
		lossy := m.oooOK && c.oooRef.ok && c.oooRef.v < wcOooClean && c.oooRef.past(m.ooo, wcOooCongAbs) ||
			m.sndOK && c.sndRef.past(m.snd, wcLossCongAbs)
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
		if m.oooOK && c.oooRef.ok && c.oooRef.v < wcOooClean && c.oooRef.past(m.ooo, wcOooCongAbs) && c.holdN < 2 {
			c.holdN++
			return
		}
		c.refPending = false
		if m.qOK {
			c.qRef = m.qTurn
		}
		c.qSndRef, c.qSndRefOK = m.qSnd, m.qSndOK
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
	if !c.qSndRefOK && m.qSndOK {
		c.qSndRef, c.qSndRefOK = m.qSnd, true
	}
	qUp := m.qOK && m.qTurn > c.qRef+dq
	sndUp := m.qSndOK && c.qSndRefOK && m.qSnd > c.qSndRef+dq
	// the exit sees the download queue itself (its srtt has no wait behind
	// holes in it): where its latest record shows none, a longer turnaround
	// is data held behind holes, not a queue
	bare := m.qSndOK && m.qSnd <= max(c.qSndRef, 0)+dq/2
	if bare {
		qUp = false
	}
	growth := qUp || sndUp || m.ctrlUp
	strong := m.qOK && m.qTurn >= c.qRef+3*dq && !bare
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
			why = wcWhyRate
		case cong:
			why = wcWhyLoss
			c.learnQcap(now, m)
		}
		if !drop {
			c.rhat = append(c.rhat[:0], wcRate{now, m.rLink})
		}
		c.toDrain(in, m, ev, why, drop)
		return
	case cong:
		// loss that came with B (a probe or STARTUP whose loss the exit
		// reported late): back off, and probe less
		c.learnQcap(now, m)
		_, hi := c.bSpan()
		c.noteCap(now, hi)
		c.backoff++
		c.schedProbe(now)
		c.toDrain(in, m, ev, wcWhyLoss, false)
		return
	case jump:
		// loss past its level at this very budget: ours if it stops at
		// the floor (bursts that keep overflowing the queue; in stage 1 the
		// floor is the behaviour without the core), the path's if not
		c.lossTest, c.lossTestAt, c.lossTestB = src, now, c.B
		c.toDrain(in, m, ev, wcWhyLoss, false)
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
		if m.qOK {
			c.qRef += time.Duration(wcRefAlpha * float64(m.qTurn-c.qRef))
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
	wl := (m.hit >= wcHitShare || empty) && !growth && m.sameF
	if wl {
		c.wlN++
		if m.rF < wcHoldSlack*c.rhatF(now, m) || c.wlN >= 2 || empty {
			c.noteTau(m)
		}
		if empty && !cong && !jump {
			c.bGood = max(c.bGood, c.B) // as far as it went, no queue came
		}
	} else {
		c.wlN = 0
	}
	if c.wlN >= 3 && !(m.qOK && m.qTurn > c.qRef+dq/2) && (!m.oooOK || m.ooo < wcOooCongAbs) && now.Sub(c.bCapAt) >= wcCapStale {
		// held by its window with no queue, interval after interval: the
		// path has room (it got faster, or a competitor left), and what
		// was learned on it before (the cap, the backoff) is stale
		c.bCap, c.backoff = 0, 0
	}
	if c.backoff == 0 && (c.wlN >= 2 || (m.qOK && c.qRef > dq && m.qTurn < c.qRef-dq)) && c.probeAt.After(now) {
		c.probeAt = now
	}
	hold := c.bHold(now, m)
	if !now.Before(c.refreshAt) && c.holdN >= 2 && !testing && now.Before(c.probeAt) {
		c.B = hold
		c.refresh(in, m)
		return
	}
	if !now.Before(c.probeAt) && !in.apProbing && !c.capped && c.holdN >= 2 && m.sameF && !testing {
		pr := c.bProbe(now, m, hold)
		if in.poolFree < 0 || uint64(in.poolFree) >= pr-hold {
			c.state = wcProbe
			c.probeFrom = hold
			c.probeBase = (c.holdR[0] + c.holdR[1]) / 2
			c.B = 0
			c.setB(in, m, pr)
			return
		}
		*ev = append(*ev, wcEvent{kind: wcEvPool, B: hold, n: len(m.f)})
		c.probeAt = now.Add(wcProbeMin)
	}
	c.setB(in, m, hold)
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
		if c.sndRef.past(m.snd, wcLossCongAbs) {
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

func (c *wcore) probe(in wcIn, m wcMeas, ev *[]wcEvent) {
	now := in.now
	if !m.sameF || in.poolShort > 0 {
		// inconclusive: F changed, or the pool gave less than asked
		if in.poolShort > 0 {
			*ev = append(*ev, wcEvent{kind: wcEvPool, B: c.B, n: len(m.f)})
		}
		c.toHold(in, m)
		return
	}
	qUp := m.qOK && m.qTurn > c.qRef+c.dq()
	cong, jump, _ := c.lossVerdict(m, qUp)
	if !cong && !jump {
		c.noteRate(now, m.rLink)
	}
	exp := float64(c.B-min(c.probeFrom, c.B)) / math.Max(float64(c.probeFrom)-m.hStep, 1)
	need := math.Max(wcPayMin, wcPayShare*exp)
	gain := m.rF/math.Max(c.probeBase, 1) - 1
	if cong || jump || gain < need || (qUp && gain < 2*need) {
		c.probeFailed(in, m, ev, cong)
		return
	}
	c.cnt.probeOK++
	c.backoff = 0
	c.pathFull = false
	c.bGood = max(c.bGood, c.B)
	if c.bCap > 0 && c.B > c.bCap {
		c.bCap = c.B // as clean as far as the probe went
	}
	c.noteTau(m)
	c.schedProbe(now)
	if gain >= wcSuccStartup*exp && gain >= 2*need && !qUp {
		// the path has room: grow on, from the smallest step
		c.state = wcStartup
		c.fullR, c.prevR, c.prevB, c.flat, c.gear = m.rF, m.rF, c.B, 0, 0
		c.setB(in, m, uint64(wcGears[0]*math.Min(float64(c.B), c.knee(m.rF, m))))
		return
	}
	c.toHold(in, m)
}

func (c *wcore) probeFailed(in wcIn, m wcMeas, ev *[]wcEvent, cong bool) {
	c.cnt.probeFail++
	c.pathFull = true
	c.backoff++
	c.schedProbe(in.now)
	if cong {
		c.learnQcap(in.now, m)
		c.noteCap(in.now, c.B)
		c.toDrain(in, m, ev, wcWhyLoss, false)
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
		o.qBrake = max(c.probeQ(now), c.dq()) + 2*c.spread
		o.growOK = c.state != wcDrain && !in.apProbing && c.parkHold == 0
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
	if b.have {
		in.segsIn = wcDelta(s.segsIn, b.prev.segsIn)
		in.ooo = wcDelta(s.ooo, b.prev.ooo)
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
		if !b.sndAt.IsZero() {
			in.snd.retrans = wcDelta(s.sndRetrans, b.sndRet)
			in.snd.segsOut = wcDelta(s.sndSegsOut, b.sndSegs)
		}
		b.sndAt, b.sndRet, b.sndSegs = s.sndAt, s.sndRetrans, s.sndSegsOut
	}
	b.prev, b.have = s, true
	return in
}
