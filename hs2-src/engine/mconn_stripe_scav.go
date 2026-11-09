package engine

import (
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The stripe as a scavenger (HS2_MCONN_STRIPE, the exit side: the sender of
// a download). A stripe adds links, each with its own BBR, to one download;
// together they claim several shares of the path's bottleneck and keep a
// standing queue there that every user waits — a game or a ping on another
// link included (mconn_stripe.go's first version: the busy lab's game p99
// 160 → 220-280 ms). So the stripe only uses capacity nobody else needs:
//
//   - signal: the path's queue as the OTHER users' links see it — the
//     median queueing delay (smoothed RTT over the lowest, TCP_INFO, read
//     every stripeQEvery) of the exit's busy links that carry none of this
//     connection's subs. A bottleneck's queue shows on every link through
//     it; the stripe's own links are left out because they also show their
//     own (BBR probing, loss recovery: 7-12 ms on them with no queue on the
//     path), and alone on the path there is nobody to yield to. In the busy
//     lab a stripe without the gate (and the pool grown to 32 links) kept a
//     31 ms median queue (p95 80 ms) on everyone; the lower quartile as the
//     signal (~1 ms) let the raw path's p95 reach +11 ms.
//   - width (LEDBAT-like): the number of stripe subs the sender may put
//     chunks on besides the primary. Above stripeQTarget it halves at once
//     (at most once per stripeQEvery) and no stripe of the process grows for
//     stripeCutHold; under half the target it grows by one sub, and the whole
//     process by one sub every stripeQGrow (stripeGlobal). Width 0 is the
//     connection without the stripe (everything on the primary, as lite's
//     one-copy state). The target (3 ms) holds the others' queue near the
//     no-stripe level (busy lab: median 1.2 ms, p95 1.6 ms); 5 ms doubled
//     the busy download but put the TUN ping's p90 7 ms up.
//   - the Iran side needs no change: a sub the sender stops using carries no
//     rate, so Iran's growth rule (rate per sub) stops opening more, and
//     lite's idle rule closes the cold ones, which frees their links.

// stripeAlone: a stripe sub only on a link no user rides (default on;
// HS2_MCONN_STRIPE_ALONE=0: the first version's rule, fewest users).
var stripeAlone = os.Getenv("HS2_MCONN_STRIPE_ALONE") != "0"

// stripeLog: HS2_MCONN_STRIPE_LOG=1 logs the stripe's link picks and, once a
// second per connection, the sender's width (diagnostics).
var stripeLog = os.Getenv("HS2_MCONN_STRIPE_LOG") == "1"

// stripeQTarget: HS2_MCONN_STRIPE_QMS (ms, default 3; 0: no gate). The busy
// lab without a stripe: the median 1.2 ms, its p95 1.6 ms.
var stripeQTarget atomic.Int64

const (
	stripeQEvery = 100 * time.Millisecond
	stripeQGrow  = 500 * time.Millisecond
)

func init() {
	ms := 3.0
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_QMS")), 64); err == nil && v >= 0 {
		ms = v
	}
	stripeQTarget.Store(int64(ms * float64(time.Millisecond)))
}

// stripeGate is a connection's sender-side width state (under mConn.mu).
type stripeGate struct {
	stW      int       // stripe subs the sender may use now
	stWInit  bool      // stW was set
	stGateAt time.Time // the last width decision
	stGrowAt time.Time // the last growth (or the last cut)
	stQ      time.Duration
	stLogAt  time.Time
	stNoGate bool  // the last decision found nothing to measure: no gate until the next
	stAskWin int64 // the growth window this connection last asked in (stripeGlobal)
	stActWin int64 // the window this connection last gated in (stripeNoteActive)
}

// subMeter: the link meter of an exit sub (nil: not known).
func subMeter(s *msub) *linkMeter {
	if cs, ok := s.rw.(exitCountedStream); ok && cs.c != nil {
		return cs.c.mtr
	}
	return nil
}

// stripeWidthLocked (sender, under m.mu): the width now, updated from the
// path's queue (stripePathQ). -1: no gate (off, or Iran), every stripe sub
// may be used.
func (m *mConn) stripeWidthLocked(p *msub, now time.Time) int {
	target := m.stripeTargetLocked()
	if target <= 0 || m.iran && !m.stUp { // Iran gates an upload (HS2_MCONN_STRIPE_UP)
		return -1
	}
	if !m.stWInit {
		m.stWInit = true
		m.stW = 0 // a stripe starts narrow and grows while the path has room
		m.stGrowAt = now
	}
	if now.Sub(m.stGateAt) < stripeQEvery {
		if m.stNoGate {
			return -1
		}
		return m.stW
	}
	m.stGateAt = now
	own := make([]*linkMeter, 0, len(m.subs))
	for _, s := range m.subs {
		if mtr := m.subMeterOf(s); mtr != nil {
			own = append(own, mtr)
		}
	}
	pq := stripePathQ
	if m.iran {
		pq = stripeUpPathQ // the upload direction: Iran's own sockets
	}
	q, measured := pq.delay(now, own)
	if m.stNoGate = !measured; m.stNoGate {
		stripeDiag.noMeas.Add(1)
		return -1 // no exit link tracked (nothing to measure the path with)
	}
	var base time.Duration
	qRaw := q
	if stripeRel {
		if stripeBaseMaxMulti >= 0 && !m.iran {
			m.stripeNoteActive(now)
		}
		// the relative gate: only the excess over the path's own noise
		base = pq.base()
		q = max(0, q-base)
	}
	m.stQ = q
	live := 0
	for _, s := range m.subs {
		if s.stripe && s != p && !s.retired && !s.dead {
			live++
		}
	}
	why := gwFull
	switch {
	case q > target:
		why = gwCut
		if m.stW > 0 {
			m.stW /= 2
			stripeStat.cuts.Add(1)
			m.tr.ev("stripe: queue %v > %v, width -> %d", q, target, m.stW)
		}
		m.stGrowAt = now
		if stripeRel {
			m.stGrowAt = now.Add(stripeCutHold - stripeQGrow) // this connection holds
		}
		stripeCut(now)
	case q < target/2 && now.Sub(m.stGrowAt) >= stripeQGrow && m.stW < live && m.stripeGrowTokenLocked(now):
		m.stW++
		m.stGrowAt = now
		why = gwGrow
	default:
		switch {
		case m.stW >= live:
		case q >= target/2:
			why = gwBand
		case now.Sub(m.stGrowAt) < stripeQGrow:
			why = gwHold
		default:
			why = gwToken
		}
	}
	m.stW = min(m.stW, live)
	stripeDiagGate(why, m.stW, live, qRaw, q, base, pq.busyN())
	if stripeLog && now.Sub(m.stLogAt) >= time.Second {
		m.stLogAt = now
		log.Printf("stripe gate %x: q=%v base=%v w=%d live=%d", m.id&0xffffff, q.Round(100*time.Microsecond), base.Round(100*time.Microsecond), m.stW, live)
	}
	return m.stW
}

// stripeGlobal: the growth of every stripe of the process together. Each
// connection's own controller would add a sub every stripeQGrow, so N
// stripes (a busy server: several long downloads) grew N times as fast and
// overshot the path's room before the queue showed; and after one queue,
// another stripe's growth refilled it. So: one sub more per stripeQGrow for
// the whole process, none for stripeCutHold after any cut.
//
// With HS2_MCONN_STRIPE_REL=1 (the relative gate) that lock is loosened: in
// production (Iran-Germany, dozens of heavy users) the path's noise put a
// cut somewhere almost every hold, and every stripe sat at its first width.
// A cut holds only the connection that saw it (stGrowAt) and empties the
// process's growth bucket (about one stripeQGrow of no growth anywhere);
// the bucket refills with one sub per stripeQGrow per stripeRelPer stripes
// that asked to grow in the last second (at least one), so N stripes grow
// about N/stripeRelPer times as fast as one, never N times.
var stripeGlobal struct {
	mu            sync.Mutex
	growAt, cutAt time.Time
	// the relative gate's bucket (HS2_MCONN_STRIPE_REL)
	tokens       float64
	fillAt       time.Time
	win          int64 // the current asking window (unix seconds)
	asked, prior int   // stripes that asked in the window, in the one before
	// stripes that gated (HS2_MCONN_STRIPE_BASE_MAX_MULTI): the window, its
	// count, the one before's
	awin          int64
	act, actPrior int
}

// stripeCutHold: no stripe grows for this long after a queue.
const stripeCutHold = 2 * time.Second

// stripeRel: HS2_MCONN_STRIPE_REL=1, the relative gate (stripeQ.base) and
// the per-connection cut (stripeGlobal). Default off.
var stripeRel = os.Getenv("HS2_MCONN_STRIPE_REL") == "1"

// stripeRelPer: stripes per sub of process growth per stripeQGrow
// (HS2_MCONN_STRIPE_REL_PER, default 4).
var stripeRelPer = envIntDefault("HS2_MCONN_STRIPE_REL_PER", 4)

func envIntDefault(k string, d int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil && v > 0 {
		return v
	}
	return d
}

// stripeGrowTokenLocked (under m.mu): whether m may take one more sub now
// (and takes it).
func (m *mConn) stripeGrowTokenLocked(now time.Time) bool {
	if !stripeRel {
		return stripeGrowToken(now)
	}
	g := &stripeGlobal
	g.mu.Lock()
	defer g.mu.Unlock()
	if w := now.Unix(); w != g.win {
		g.prior = 0
		if w == g.win+1 {
			g.prior = g.asked
		}
		g.win, g.asked = w, 0
	}
	if m.stAskWin != g.win {
		m.stAskWin = g.win
		g.asked++
	}
	n := max(g.asked, g.prior)
	rate := float64(max(1, (n+stripeRelPer-1)/stripeRelPer)) // subs per stripeQGrow
	if g.fillAt.IsZero() {
		g.tokens, g.fillAt = 1, now
	} else if dt := now.Sub(g.fillAt); dt > 0 {
		g.tokens = min(rate, g.tokens+rate*float64(dt)/float64(stripeQGrow))
		g.fillAt = now
	}
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// stripeGrowToken: whether a stripe may take one more sub now (and takes it).
func stripeGrowToken(now time.Time) bool {
	g := &stripeGlobal
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.cutAt) < stripeCutHold || now.Sub(g.growAt) < stripeQGrow {
		return false
	}
	g.growAt = now
	return true
}

// stripeCut notes a queue: no stripe grows for stripeCutHold.
func stripeCut(now time.Time) {
	g := &stripeGlobal
	g.mu.Lock()
	g.cutAt = now
	if stripeRel {
		g.tokens = min(g.tokens, 0)
		g.fillAt = now
	}
	g.mu.Unlock()
}

// diagnostics (HS2_MCONN_STRIPE_LOG=1): where light placements land.
var stripeLightPicks struct {
	n, heavy, stripe, users atomic.Int64
	once                    sync.Once
}

// stripeNoteLight (under the pool lock): a light placement (a first stream
// or a light sub) went to ml.
func stripeNoteLight(ml *managedLink) {
	st := &stripeLightPicks
	st.n.Add(1)
	if ml.heavy > 0 {
		st.heavy.Add(1)
	}
	if stripeOn(ml) {
		st.stripe.Add(1)
	}
	st.users.Add(int64(ml.users.Load()))
	st.once.Do(func() {
		go func() {
			for range time.Tick(5 * time.Second) {
				n := st.n.Swap(0)
				if n == 0 {
					continue
				}
				log.Printf("stripe light picks 5s: n=%d on-heavy=%d on-stripe=%d avg-users=%.1f", n, st.heavy.Swap(0), st.stripe.Swap(0), float64(st.users.Swap(0))/float64(n))
			}
		}()
	})
}

// stripePathQ: the exit's links, measured for the gate (HS2_MCONN_STRIPE;
// tracked only with the key on).
var stripePathQ = &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}

type stripeQ struct {
	mu    sync.Mutex
	links map[*linkMeter]*stripeLinkQ
	at    time.Time // the last measurement
	fixed bool      // tests: every caller reads q
	light bool      // also the active light links (lightQ; Iran's upload gate)
	q     time.Duration
	logAt time.Time // diagnostics (HS2_MCONN_STRIPE_LOG)
	// the relative gate (HS2_MCONN_STRIPE_REL): the path queue every busy
	// link shows (none left out), one per measurement over the last
	// stripeBaseN; bq is their stripeBasePct percentile
	hist  []time.Duration
	histI int
	bq    time.Duration
}

// stripeLinkQ: one link at the last measurement.
type stripeLinkQ struct {
	acked uint64
	q     time.Duration
	busy  bool // acknowledged stripeBusyBytes since the look before
	// active: acknowledged something since the look before (a light user's
	// link too: Iran's upload gate, mconn_stripe_up.go)
	active bool
	// stat reads the link's TCP_INFO (Iran's links, stripeUpTrack); nil:
	// the meter's tcp (the exit's)
	stat func() (tcpStat, bool)
	win  qwinMin // HS2_MCONN_STRIPE_QWIN_MS (mconn_stripe_diag.go)
	lb   linkBase // HS2_MCONN_STRIPE_LINKBASE (mconn_stripe_linkbase.go)
}

// stripeTrack counts an exit link's meter while it is up (the returned func
// forgets it); nothing with the key off.
func stripeTrack(mtr *linkMeter) func() {
	if stripeK.Load() == 0 || mtr == nil || mtr.tcp == nil {
		return func() {}
	}
	p := stripePathQ
	p.mu.Lock()
	p.links[mtr] = &stripeLinkQ{}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.links, mtr)
		p.mu.Unlock()
	}
}

// stripeBusyBytes: a link that had this much acknowledged since the last
// look (stripeQEvery: ~5 Mbit/s) counts — a download's link. An idle link's
// smoothed RTT is stale, and a call's or a game's (a few hundred kbit/s to
// 2 Mbit/s) sits 1-2 ms over its lowest with no queue on the path at all
// (delayed ACKs): counted, it held a stripe alone on the path at 80 Mbit/s
// with a 2 ms target.
const stripeBusyBytes = 64 << 10

// delay: the median queueing delay of the busy links not in own, measured
// at most every stripeQEvery (callers share the measurement); 0 when no
// such link is busy (nobody to yield to). false: no link tracked at all.
func (p *stripeQ) delay(now time.Time, own []*linkMeter) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fixed {
		return p.q, true
	}
	if len(p.links) == 0 {
		return 0, false
	}
	if now.Sub(p.at) >= stripeQEvery {
		first := p.at.IsZero()
		p.at = now
		for mtr, l := range p.links {
			var st tcpStat
			var ok bool
			if l.stat != nil {
				st, ok = l.stat()
			} else {
				st, ok = tcpStats(mtr.tcp)
			}
			if !ok {
				l.busy, l.active = false, false
				continue
			}
			l.busy = !first && l.acked != 0 && st.bytesAcked >= l.acked+stripeBusyBytes && st.minRttUs > 0
			l.active = !first && l.acked != 0 && st.bytesAcked > l.acked && st.minRttUs > 0
			l.acked = st.bytesAcked
			l.q = 0
			lo := st.minRttUs
			if stripeQWin > 0 && st.rttUs > 0 {
				lo = max(lo, l.win.note(now, st.rttUs))
			}
			if st.rttUs > lo {
				l.q = time.Duration(st.rttUs-lo) * time.Microsecond
			}
			if stripeLinkBase && !p.light && l.busy {
				l.lb.note(l.q, p.baseMax())
			}
		}
		if stripeRel {
			p.noteBaseLocked(p.pickLocked(now, nil, false))
		}
	}
	return p.pickLocked(now, own, true), true
}

// busyN (diagnostics): busy links at the last measurement.
func (p *stripeQ) busyN() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, l := range p.links {
		if l.busy {
			n++
		}
	}
	return n
}

// pickLocked: the path queue the links not in own show, from the last
// measurement; logIt: the diagnostics line may be written.
func (p *stripeQ) pickLocked(now time.Time, own []*linkMeter, logIt bool) time.Duration {
	var ds []time.Duration
	if stripeLinkBase && !p.light {
		for mtr, l := range p.links {
			if l.busy && !slices.Contains(own, mtr) {
				if e, ok := l.lb.excess(l.q); ok {
					ds = append(ds, e)
				}
			}
		}
	}
	if len(ds) == 0 {
		for mtr, l := range p.links {
			if l.busy && !slices.Contains(own, mtr) {
				ds = append(ds, l.q)
			}
		}
	}
	var lt []time.Duration
	if p.light {
		// Iran's upload (mconn_stripe_up.go): the light users' links too
		for mtr, l := range p.links {
			if l.active && !l.busy && !slices.Contains(own, mtr) {
				lt = append(lt, l.q)
			}
		}
		slices.Sort(lt)
	}
	slices.Sort(ds)
	if logIt && stripeLog && p.light && now.Sub(p.logAt) >= time.Second {
		p.logAt = now // diagnostics: what Iran's upload gate reads
		log.Printf("stripe upq: links=%d own=%d busy-others=%v light=%v", len(p.links), len(own), ds, lt)
	}
	var q time.Duration
	if len(ds) > 0 {
		q = ds[len(ds)/2]
	}
	if len(lt) > 0 {
		q = max(q, lightQ(lt))
	}
	return q
}

// The relative gate's baseline. Production's Iran-Germany path shows its
// busy links 1.0 ms (p25), 1.7 ms (median), 3-4 ms (p75) over their lowest
// RTT with no queue (BBR keeps the real one near zero): route jitter, not a
// queue. Against the absolute target (grow under 1.5 ms, halve over 3 ms)
// the stripe almost never grew and was cut over and over. The baseline is
// the lower decile of the path queue over the last ~30 s: a standing queue
// the stripes make is cut (over base+target) long before it fills 90% of
// the window, so it does not become the baseline; it is capped at
// stripeBaseMax all the same.
const (
	stripeBaseN   = 300 // measurements (stripeQEvery each: 30 s)
	stripeBaseMin = 30  // fewer: no baseline yet (0)
	stripeBasePct = 10
)

// stripeBaseMax: HS2_MCONN_STRIPE_BASE_MAX (ms, fractions allowed, default 4).
var stripeBaseMax = envMsDefault("HS2_MCONN_STRIPE_BASE_MAX", 4*time.Millisecond)

// stripeUpBaseMax: the cap of Iran's upload gate (HS2_MCONN_STRIPE_UP_BASE_MAX,
// ms, default 2). Iran's upload gate also reads the light users' links
// (lightQ), so the noise it forgives is forgiven to them too. In the jittery
// lab (300us normal netem, 1% loss, 18 interleaved runs) the 4 ms cap cost
// game p99 126 -> 137 ms (p=0.01) under one upload; 2 ms kept it at 128 with
// the same upload speed (468 vs 443 Mbit). The exit (download) keeps 4 ms: there
// 2 ms cost a quarter of the speed (298 vs 395 Mbit) and no light user gained.
var stripeUpBaseMax = envMsDefault("HS2_MCONN_STRIPE_UP_BASE_MAX", 2*time.Millisecond)

// envMsDefault: env k in ms (fractions allowed, >= 0), else d.
func envMsDefault(k string, d time.Duration) time.Duration {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(k)), 64); err == nil && v >= 0 {
		return time.Duration(v * float64(time.Millisecond))
	}
	return d
}

// noteBaseLocked records one measurement's path queue (no busy link: nothing
// to learn).
func (p *stripeQ) noteBaseLocked(q time.Duration) {
	if q <= 0 {
		return
	}
	if len(p.hist) < stripeBaseN {
		p.hist = append(p.hist, q)
	} else {
		p.hist[p.histI] = q
		p.histI = (p.histI + 1) % stripeBaseN
	}
	if len(p.hist) < stripeBaseMin {
		p.bq = 0
		return
	}
	s := slices.Clone(p.hist)
	slices.Sort(s)
	p.bq = min(s[len(s)*stripeBasePct/100], p.baseMax())
}

// baseMax: the baseline's cap (Iran's upload gate: stripeUpBaseMax).
func (p *stripeQ) baseMax() time.Duration {
	if p.light {
		return stripeUpBaseMax
	}
	return stripeBaseMax
}

// base: the path's noise floor (0 until learned).
func (p *stripeQ) base() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.light && stripeBaseMaxMulti >= 0 && stripeActiveN() >= 2 {
		return min(p.bq, stripeBaseMaxMulti)
	}
	return p.bq
}

// stripeBaseMaxMulti: the download baseline's cap while two or more stripes
// gate at once (HS2_MCONN_STRIPE_BASE_MAX_MULTI, ms; unset: -1, no change).
// One stripe forgives the path's noise up to stripeBaseMax (4 ms), and so
// does each of N: the lab's 4 parallel downloads at 1 % loss (shared links,
// HS2_MCONN_STRIPE_SHARE) together held the light users' crowd p95 +7-9 ms
// over production, while a pool budget of shared links (4 or 8 for all)
// cost half the speed and did not bring it back. A lower forgiveness only
// when several stripes run leaves a single download as it was.
var stripeBaseMaxMulti = envMsDefault("HS2_MCONN_STRIPE_BASE_MAX_MULTI", -1)

// stripeNoteActive (sender, under m.mu): m gated in this second.
func (m *mConn) stripeNoteActive(now time.Time) {
	g := &stripeGlobal
	g.mu.Lock()
	defer g.mu.Unlock()
	if w := now.Unix(); w != g.awin {
		g.actPrior = 0
		if w == g.awin+1 {
			g.actPrior = g.act
		}
		g.awin, g.act = w, 0
	}
	if m.stActWin != g.awin {
		m.stActWin = g.awin
		g.act++
	}
}

// stripeActiveN: stripes that gated in this second or the one before.
func stripeActiveN() int {
	g := &stripeGlobal
	g.mu.Lock()
	defer g.mu.Unlock()
	if w := time.Now().Unix(); w != g.awin {
		if w == g.awin+1 {
			return g.act
		}
		return 0
	}
	return max(g.act, g.actPrior)
}

// stripeLogf logs a diagnostic line (callers check stripeLog).
func stripeLogf(format string, args ...any) { log.Printf(format, args...) }

// stripeDescribe (diagnostics): l's state in the pool for a log line.
func (m *LinkManager) stripeDescribe(l Link) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	S, light := 0, 0
	var me *managedLink
	for _, ml := range m.links {
		if ml.serving() {
			S++
			if ml.heavy == 0 && !stripeOn(ml) {
				light++
			}
		}
		if ml.link == l {
			me = ml
		}
	}
	if me == nil {
		return "?"
	}
	return fmt.Sprintf("id=%d heavy=%d stripe=%v users=%d flowing=%d pressed=%v recent=%d age=%v serving=%d light=%d",
		me.id, me.heavy, stripeOn(me), me.users.Load(), me.flowing, me.pressed, me.recentPicks(), time.Since(me.born).Round(time.Second), S, light)
}
