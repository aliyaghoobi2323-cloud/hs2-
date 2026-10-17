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
// exercised by the link model in wcore_model_test.go; wcore_run.go is the
// actuator.
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
//     streams together) that fills the path. STARTUP grows B by ×1.5–2 per
//     interval while the link's delivery rate grows with it and stops when
//     the rate stops growing, the queue grows or loss correlates with B.
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

	state    wcState
	B        uint64    // budget in force (0 in OFF)
	settling bool      // the interval that ends at the next step lets a change of B reach the sender
	lastAt   time.Time // the previous step
	nextAt   time.Time

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

	// loss
	sndBase, oooBase     float64
	sndBaseOK, oooBaseOK bool
	riseAt               time.Time // B last went up
	lossyN               int
	lossy                bool
	qcap                 time.Duration
	qcapAt               time.Time
	qcapOK               bool
	parkHold             int
	pathFull             bool
	capped               bool

	// STARTUP
	fullR, prevR float64
	prevB        uint64
	flat         int
	kPrev        uint64
	kPrevAt      time.Time

	// PROBE
	probeAt   time.Time
	backoff   int
	probeFrom uint64
	probeBase float64

	ceils map[uint32]uint32
	cnt   wcCounters
	last  wcMeas // the latest interval's measurements (lab trace)
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
	wcStartGain      = 1.5
	wcStartGainFast  = 2.0
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
)

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

func (c *wcore) step(in wcIn) wcOut {
	now := in.now
	var ev []wcEvent
	dt := 0.0
	if !c.lastAt.IsZero() {
		dt = now.Sub(c.lastAt).Seconds()
	}
	c.lastAt = now
	c.notePaths(in, &ev)
	m := c.measure(in, dt)
	c.last = m
	c.notePool(in, &ev)
	if c.parkHold > 0 {
		c.parkHold--
	}
	if in.parks > 0 || in.brakes > 0 {
		c.parkHold = wcParkHold
	}
	if c.tBase <= 0 || dt <= 0 {
		return c.out(in, now, now.Add(wcTickMin), ev)
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
		// a fast stream paused: keep the state, judge nothing
		c.settling = false
	case c.settling:
		c.settled(in, m, &ev)
	default:
		switch c.state {
		case wcStartup:
			c.startup(in, m, &ev)
		case wcHold:
			c.hold(in, m, &ev)
		case wcProbe:
			c.probe(in, m, &ev)
		case wcDrain:
			c.toHold(in, m)
		}
	}
	settle, measure := c.interval()
	next := now.Add(measure)
	if c.state == wcOff {
		next = now.Add(wcTickMin)
	} else if c.settling {
		next = now.Add(settle)
	}
	if c.B > c.cnt.peakB {
		c.cnt.peakB = c.B
	}
	return c.out(in, now, next, ev)
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

func (c *wcore) notePool(in wcIn, ev *[]wcEvent) {
	if in.poolShort > 0 {
		c.cnt.poolShort += int(in.poolShort)
	}
}

// measure classifies the streams and takes the interval's estimators.
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
	hits, upds := 0.0, 0.0
	for i, s := range in.streams {
		r := float64(s.read) / dt
		if s.win < need/2 || s.upd == 0 || r <= 0 || s.braked || float64(s.lag) > wcLagShare*float64(s.upd) {
			continue
		}
		hit := float64(s.hit) >= wcHitShare*float64(s.upd)
		turn := s.turnLast
		if turn <= 0 {
			turn = s.turnMin
		}
		used := turn > 0 && r*turn.Seconds() >= wcUseShare*float64(s.win)
		// on use alone only a stream as fast as its peers: a slow source
		// "uses" its window too (it fills in as slowly as it is written)
		if !hit && !(used && (c.inF[s.id] || r >= wcPeerShare*maxR)) {
			continue
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
	// keep the window (and at least wcRhatMinN samples)
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

// knee is K = R̂_F·(T_base+τ_hold) + H_step for a rate r.
func (c *wcore) knee(r float64, m wcMeas) float64 {
	return r*(c.tBase+c.tau).Seconds() + m.hStep
}

func (c *wcore) bHold(now time.Time, m wcMeas) uint64 {
	r := c.rhatF(now, m)
	t := c.tBase.Seconds()
	h := math.Min(math.Max(c.qTarget(now).Seconds()/t, wcHMin), wcHMax)
	return uint64(c.knee(r, m) + h*r*t)
}

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
	if b > c.B {
		c.riseAt = in.now
	}
	if c.B == 0 || math.Abs(float64(b)-float64(c.B)) > wcResettle*float64(c.B) {
		c.settling = true
	}
	c.B = b
}

// cong says whether the interval's loss is congestive: it rose with B or
// with the queue (rise) past max(+0.3 points, ×2) of the HOLD level.
// Loss that does not move with B is random and backs nothing off.
func (c *wcore) cong(m wcMeas, rise, sndRise bool) bool {
	if m.sndOK && c.sndBaseOK && (rise || sndRise) {
		if m.snd >= math.Max(c.sndBase+wcLossCongAbs, wcLossCongRel*c.sndBase) {
			return true
		}
	}
	if m.oooOK && c.oooBaseOK && rise && c.oooBase < wcOooClean {
		if m.ooo >= math.Max(c.oooBase+wcOooCongAbs, wcLossCongRel*c.oooBase) {
			return true
		}
	}
	return false
}

// noteLoss keeps the loss levels of intervals with B steady and no queue
// growth, and the random-loss flag.
func (c *wcore) noteLoss(now time.Time, m wcMeas, steady bool) {
	if !steady {
		c.lossyN = 0
		return
	}
	if m.sndOK && now.Sub(c.riseAt) > wcSndSpan {
		if c.sndBaseOK {
			c.sndBase += 0.3 * (m.snd - c.sndBase)
		} else {
			c.sndBase, c.sndBaseOK = m.snd, true
		}
	}
	if m.oooOK {
		if c.oooBaseOK {
			c.oooBase += 0.3 * (m.ooo - c.oooBase)
		} else {
			c.oooBase, c.oooBaseOK = m.ooo, true
		}
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

func (c *wcore) schedProbe(now time.Time) {
	iv := wcProbeMin + time.Duration(c.rnd()*float64(wcProbeMax-wcProbeMin))
	iv <<= min(c.backoff, 4)
	iv = min(iv, wcProbeBackMax)
	iv = max(iv, time.Duration(wcProbeRTTs)*c.tBase)
	c.probeAt = now.Add(iv)
}

// off: no ceilings. A fast stream with T_base known starts STARTUP.
func (c *wcore) off(in wcIn, m wcMeas, ev *[]wcEvent) {
	c.noteLoss(in.now, m, true)
	if len(m.f) == 0 || in.apProbing {
		return
	}
	c.state = wcStartup
	c.backoff = 0
	c.pathFull = false
	c.flat = 0
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
	c.setB(in, m, uint64(float64(c.prevB)*wcStartGain))
}

func (c *wcore) toOff() {
	if c.state == wcHold || c.state == wcProbe {
		c.kPrev, c.kPrevAt = c.B, c.lastAt
	}
	c.state = wcOff
	c.B = 0
	c.settling = false
	c.refPending = false
	c.capped = false
	clear(c.inF)
}

// settled ends the settle part of an interval: only what cannot wait for
// the measuring part is acted on.
func (c *wcore) settled(in wcIn, m wcMeas, ev *[]wcEvent) {
	c.settling = false
	switch c.state {
	case wcProbe:
		if c.cong(m, true, true) {
			c.probeFailed(in, m, ev, true)
		} else if in.apProbing {
			c.toHold(in, m)
		}
	case wcStartup:
		if c.cong(m, true, true) {
			c.endStartup(in, m, ev, true)
		} else if in.apProbing {
			c.toHold(in, m)
		}
	case wcDrain:
		if m.qOK && m.qTurn < c.qRef+c.dq()/2 {
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
	c.noteRate(in.now, m.rLink)
	gB := float64(c.B)/math.Max(float64(c.prevB), 1) - 1
	gR := m.rF/math.Max(c.prevR, 1) - 1
	qUp := m.qOK && m.qTurn > c.qRef+c.dq()
	cong := c.cong(m, true, true)
	tracked := gB > 0 && gR >= wcStartFast*gB
	if m.rF >= c.fullR*(1+wcFullGrowth) {
		c.fullR, c.flat = m.rF, 0
	} else {
		c.flat++
	}
	if (tracked || m.hit >= wcHitShare) && !qUp {
		c.noteTau(m)
	}
	if m.qOK && !qUp && m.qTurn < c.qRef {
		c.qRef = m.qTurn
	}
	if cong || c.flat >= wcFullRounds || (qUp && c.flat >= 1) || in.apProbing || (c.capped && c.flat >= 1) {
		c.endStartup(in, m, ev, cong || qUp)
		return
	}
	k := c.knee(m.rF, m)
	g := wcStartGain
	if tracked && !qUp {
		g = wcStartGainFast
	}
	if qUp {
		g = 1 // the sender or the queue holds what B has: wait
	}
	c.prevR, c.prevB = m.rF, c.B
	c.setB(in, m, max(c.B, uint64(g*math.Min(float64(c.B), k))))
}

// endStartup: the knee is found. To DRAIN if the queue grew (or loss came
// with B), else to HOLD.
func (c *wcore) endStartup(in wcIn, m wcMeas, ev *[]wcEvent, drain bool) {
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
		if c.cong(m, true, true) && m.qOK {
			c.learnQcap(in.now, m)
		}
		c.toDrain(in, m, ev, wcWhyQueue, false)
		return
	}
	c.toHold(in, m)
}

func (c *wcore) learnQcap(now time.Time, m wcMeas) {
	q := max(0, m.qTurn-c.qRef)
	if !c.qcapKnown(now) || q < c.qcap {
		c.qcap = q
	}
	c.qcap, c.qcapAt, c.qcapOK = max(c.qcap, 0), now, true
}

func (c *wcore) toHold(in wcIn, m wcMeas) {
	c.state = wcHold
	c.refPending = true
	c.growthN, c.wlN = 0, 0
	c.holdN = 0
	if c.probeAt.IsZero() || c.probeAt.Before(in.now) {
		c.schedProbe(in.now)
	}
	c.setB(in, m, c.bHold(in.now, m))
	c.settling = true
}

func (c *wcore) toDrain(in wcIn, m wcMeas, ev *[]wcEvent, why wcDrainWhy, rate bool) {
	from := c.B
	c.state = wcDrain
	t := c.tBase.Seconds()
	b := 0.9*m.rF*t + m.rF*c.tau.Seconds() + m.hStep
	if rate {
		c.rhat = append(c.rhat[:0], wcRate{in.now, m.rLink})
		c.backoff = 0
	} else if c.tau > c.dq() {
		c.tau /= 2 // a queue HOLD itself built: τ_hold may hold a queue
	}
	switch why {
	case wcWhyLoss:
		c.cnt.drainLoss++
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
	if c.refPending {
		// the first measured interval of HOLD sets the references
		c.refPending = false
		if m.qOK {
			c.qRef = m.qTurn
		}
		c.qSndRef, c.qSndRefOK = m.qSnd, m.qSndOK
		c.qOwnRef = m.qOwn
		c.rLinkHold = m.rLink
		c.holdR = [2]float64{m.rF, m.rF}
		c.holdN = 1
		if m.sameF {
			c.noteRate(now, m.rLink)
		}
		c.noteLoss(now, m, true)
		c.setB(in, m, c.bHold(now, m))
		return
	}
	if !c.qSndRefOK && m.qSndOK {
		c.qSndRef, c.qSndRefOK = m.qSnd, true
	}
	qUp := m.qOK && m.qTurn > c.qRef+dq
	sndUp := m.qSndOK && c.qSndRefOK && m.qSnd > c.qSndRef+dq
	growth := qUp || sndUp || m.ctrlUp
	strong := m.qOK && m.qTurn >= c.qRef+3*dq
	if growth {
		c.growthN++
	} else {
		c.growthN = 0
	}
	drop := c.rLinkHold > 0 && m.rLink < (1-wcRateDrop)*c.rLinkHold
	cong := c.cong(m, growth, false)
	own := m.qOwnOK && m.qOwn > c.qOwnRef+dq/2
	if strong || c.growthN >= 2 || (growth && (cong || own)) {
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
	}
	c.holdN++
	if m.sameF {
		c.noteRate(now, m.rLink)
	}
	c.noteLoss(now, m, !growth)
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
	wl := m.hit >= wcHitShare && !growth && m.sameF
	if wl {
		c.wlN++
		if m.rF < wcHoldSlack*c.rhatF(now, m) || c.wlN >= 2 {
			c.noteTau(m)
		}
	} else {
		c.wlN = 0
	}
	if c.wlN >= 2 || (m.qOK && c.qRef > dq && m.qTurn < c.qRef-dq) {
		c.backoff = 0
		if c.probeAt.After(now) {
			c.probeAt = now
		}
	}
	hold := c.bHold(now, m)
	if !now.Before(c.probeAt) && !in.apProbing && !c.capped && c.holdN >= 2 && m.sameF {
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
	c.noteRate(now, m.rLink)
	qUp := m.qOK && m.qTurn > c.qRef+c.dq()
	cong := c.cong(m, true, true)
	exp := float64(c.B-min(c.probeFrom, c.B)) / math.Max(float64(c.probeFrom)-m.hStep, 1)
	need := math.Max(wcPayMin, wcPayShare*exp)
	gain := m.rF/math.Max(c.probeBase, 1) - 1
	if cong || gain < need || (qUp && gain < 2*need) {
		c.probeFailed(in, m, ev, cong)
		return
	}
	c.cnt.probeOK++
	c.backoff = 0
	c.pathFull = false
	c.noteTau(m)
	if gain >= wcSuccStartup*exp {
		c.state = wcStartup
		c.fullR, c.prevR, c.prevB, c.flat = m.rF, m.rF, c.B, 0
		c.schedProbe(now)
		c.setB(in, m, uint64(wcStartGain*math.Min(float64(c.B), c.knee(m.rF, m))))
		return
	}
	c.schedProbe(now)
	c.toHold(in, m)
}

func (c *wcore) probeFailed(in wcIn, m wcMeas, ev *[]wcEvent, cong bool) {
	c.cnt.probeFail++
	c.pathFull = true
	if cong && m.qOK {
		c.learnQcap(in.now, m)
	}
	c.backoff++
	c.schedProbe(in.now)
	c.toHold(in, m)
}

// out builds the ceilings: B shared over F (wcSplit), nothing for the rest.
func (c *wcore) out(in wcIn, now, next time.Time, ev []wcEvent) wcOut {
	c.nextAt = next
	o := wcOut{state: c.state, B: c.B, next: next, events: ev, growOK: true}
	o.ceils = make([]wcCeil, len(in.streams))
	for i, s := range in.streams {
		o.ceils[i].id = s.id
	}
	if c.state == wcOff || c.B == 0 {
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
	share := wcSplit(c.B, demand, lo, c.cfg.maxWin)
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

// wcStreamSnap is one stream's counters as the smux arm keeps them:
// cumulative, except turnMin and turnLast, which the runner swaps out
// (what came since the previous snapshot).
type wcStreamSnap struct {
	id                uint32
	readTot           uint64
	updN, hitN, lagN  uint32
	poolShort         uint32
	turnMin, turnLast time.Duration
	win               uint32
	braked            bool
}

// wcLinkSnap is one snapshot of a link: cumulative counters of its socket
// (TCP_INFO), its session and its large streams, the edge's control ping
// and the exit's latest stats record.
type wcLinkSnap struct {
	now                    time.Time
	minRTT, srtt           time.Duration
	segsIn, ooo            uint64
	oooOK                  bool
	rcvd                   uint64
	parks, brakes          uint32
	poolFree               int64
	streams                []wcStreamSnap
	ctrlMin                time.Duration
	pongs                  []time.Duration // pongs since the previous snapshot
	sndAt                  time.Time       // when the exit's latest record came (zero: none)
	sndSrtt, sndMinRTT     time.Duration
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
		in.parks = s.parks - b.prev.parks
		in.brakes = s.brakes - b.prev.brakes
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
			in.poolShort += st.poolShort - p.poolShort
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
