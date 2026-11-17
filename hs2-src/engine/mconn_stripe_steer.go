package engine

import (
	"errors"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// st149: HS2_LB_STRIPE_STEER (Iran/edge; both sides, default on since rel68b).
//
// A cut path (weight 0.05, by the exit's judge or HS2_LB_UPLOSS) kept 5-22 %
// of a heavy striped upload's packets in the up148/st149 labs (5 paths,
// qdisc counters in the middle namespace). Two holes, both outside the path
// steering (HS2_LB_STEER) that pickPlaneTiers applies to new subs:
//
//   - placement: the stripe's own link picks (pickStripe: the set-aside
//     link, a free link, the shared light link, the cross-direction link)
//     go over every path, so every extra slot (DL_FAIR, FAST, the raw
//     upgrade's first stripe sub) may land on a cut path;
//   - the sender: stripeSendLocked spreads chunks over every live stripe
//     sub by queue length, so a sub placed before the cut (the upload judge
//     cuts only once the upload's own loss shows) keeps its share for the
//     connection's whole life.
//
// With the key a link on a path the steering reports cut (or a cut path's
// link turned off for new connections) ranks after every link on a path not
// cut in the stripe's free-link and cross-direction picks (the shared pick,
// a light user's link, is never taken just to leave a cut path's free link),
// and the Iran sender puts no new chunk on a cut path's sub while a sub on a
// path not cut is in the stripe (it waits for that sub's room instead). When
// only cut-path links or subs exist, the stripe uses them as before: the
// steering never stalls a stripe.
//
// st150: the same key on the exit (the judge itself) steers the download
// sender: each judge reading marks the links on the paths its judge holds
// cut (never all of them), and stripeSendLocked keeps new chunks off those
// subs while a sub on a path not cut is in the stripe; the scavenger's width
// counts only subs not cut (a cut sub never takes a healthy one's slot).
// Purely local: the receiver reorders whatever sub a chunk comes on, so a
// peer of any version is unaffected. And Iran, when its only free links for
// a new stripe sub are on cut paths while the connection already has a sub
// on a path not cut (the cut sub would get nothing), asks the pool for a
// link (the exit dials it on a path its balancer weighs, a cut one only at
// the cut weight) instead of opening one there; with no such sub it still
// takes the cut link at once (no stall) and asks as well.
var lbStripeSteer bool

// stripeSteerStat: picks the key moved off a cut path, and picks that took a
// cut path because no other link could (diagnostics, tests).
var stripeSteerStat struct {
	moved, fallback atomic.Int64
	sendCut         atomic.Int64 // chunks the sender put on a cut path's sub (no other)
	// st150: picks refused (a cut link only, the connection has a sub on a
	// path not cut) with a link asked from the pool; chunks the sender kept
	// off a cut path's sub (a sub not cut took them)
	askDial, sendOff atomic.Int64
	// sd151: refused picks whose ask was dropped (one already out), and
	// stripe opens a refusal put on hold (no fast retry, a growing wait)
	askSup, held atomic.Int64
	// ou180: new links on a cut path the stripe's want did not set aside
	adoptSkipCut atomic.Int64
}

// sd151: a refused pick (stripeHasHealthyLocked) asked the pool every time,
// and the connection tried again at once: openStripe's fast retries (6,
// 250-500 ms apart), then stRetryAt 250-500 ms, then the next 1 s tick —
// about two asks a second for each heavy connection, for as long as the cut
// held (lab2 soak: asked-dial 3554 against 157 picks in 36 min). Each ask
// added one to the pool's stripeWant (up to stripeWantCap) and renewed it,
// so the pool's target stayed raised by links nobody took. Now a refused
// pick asks only when no ask is out (stripeWant 0 or lapsed: about once each
// stripeWantFor while the cut holds, at most once each stripeSteerAskGap;
// the rest counted as asked-dial-dropped), and the connection waits
// stripeSteerHold, doubling to stripeSteerHoldMax, before its next try —
// it already has a sub on a path not cut, so nothing stalls.
const (
	stripeSteerAskGap   = 2 * time.Second
	stripeSteerHold     = 2 * time.Second
	stripeSteerHoldMax  = 8 * time.Second
	stripeSteerHoldJitt = 250 * time.Millisecond
)

// errStripeSteerHeld: the edge's stripe open refused by the steering (see
// stripeSteerAskLocked); openStripe backs off instead of retrying.
var errStripeSteerHeld = errors.New("mconn: stripe pick held off a cut path")

// stripeSteerAskLocked (pickStripe, a refused pick): ask the pool for a
// link unless the steering's own last ask is still out (stSteerWantUntil,
// stripeWantFor) or less than stripeSteerAskGap old. Caller holds m.mu.
//
// ou180: the ask is the steering's own — one link on top of whatever the
// stripe already wants. sd151 deduped against stripeWant itself, but every
// stripe pick that finds no link renews that want (stripeWantRaiseLocked),
// so it never lapsed: production sent 3 asks and dropped 4488 in 3h46m while
// the v6 path was held cut for 2h45m, and the held connections retried every
// 8 s without ever getting a link. Merged into the shared want, the ask was
// also consumed by the first other stripe. Bounded: one ask per stripeWantFor
// per pool (not per connection), within stripeWantCap.
func (m *LinkManager) stripeSteerAskLocked(now time.Time, want int) {
	out := !m.stSteerWantUntil.IsZero() && now.Before(m.stSteerWantUntil)
	if out || !m.stSteerAskAt.IsZero() && now.Sub(m.stSteerAskAt) < stripeSteerAskGap {
		stripeSteerStat.askSup.Add(1)
		stripeSteerLogAsk()
		return
	}
	m.stSteerAskAt = now
	m.stSteerWantUntil = now.Add(stripeWantFor)
	stripeSteerStat.askDial.Add(1)
	if now.After(m.stripeWantUntil) || m.stripeWant <= 0 {
		m.stripeWant, m.stSteerOut = 0, 0 // what stripeExtra releases at the next tick
	}
	if m.stSteerOut == 0 {
		want = max(want, m.stripeWant+1) // one more than the stripe already wants
		m.stSteerOut = 1
	}
	m.stripeAskLocked(now, max(want, 1)) // renews the want either way
	stripeSteerLogAsk()
}

// stripeSteerHoldLocked (openStripe, the pick refused): when the next
// stripe open may run. Caller holds m.mu (the connection's).
func (m *mConn) stripeSteerHoldLocked(now time.Time) time.Time {
	stripeSteerStat.held.Add(1)
	d := stripeSteerHold << min(m.stSteerHeldN, 2)
	m.stSteerHeldN++
	return now.Add(min(d, stripeSteerHoldMax) + rand.N(stripeSteerHoldJitt))
}

// stripeCutLocked: ml is on a path the steering has cut (HS2_LB_STRIPE_STEER
// on, a fresh report). Caller holds m.mu.
func (m *LinkManager) stripeCutLocked(ml *managedLink) bool {
	if !lbStripeSteer || ml == nil {
		return false
	}
	st := m.steer
	if st == nil || !st.active {
		return false
	}
	return st.repCut[mlPath(ml)] || ml.steer.off
}

// stripeSteerLog (HS2_LB_STRIPE_STEER_LOG=1, or the key itself): every 10 s
// at most, a line with the stripe picks so far and how many went to a path
// the steering had cut — the key's effect, and the old spread with it off.
var stripeSteerLog bool

var stripeSteerPicks stripeSteerPickLog

type stripeSteerPickLog struct {
	mu                 sync.Mutex
	total, onCut, last int64
	asks               int64 // sd151: asked + dropped at the last line
	at                 time.Time
}

// stripeSteerNoteLocked counts pick ml (the steering's view, key or not).
// Caller holds m.mu.
func (m *LinkManager) stripeSteerNoteLocked(ml *managedLink) {
	if !lbStripeSteer && !stripeSteerLog {
		return
	}
	cut := false
	if st := m.steer; st != nil && st.active {
		cut = st.repCut[mlPath(ml)] || ml.steer.off
	}
	p := &stripeSteerPicks
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total++
	if cut {
		p.onCut++
	}
	stripeSteerLogLocked(p)
}

// stripeSteerLogAsk (a refused pick, sd151): the line also when only the
// asks moved (a cut path's spin shows with no pick between).
func stripeSteerLogAsk() {
	if !lbStripeSteer && !stripeSteerLog {
		return
	}
	p := &stripeSteerPicks
	p.mu.Lock()
	defer p.mu.Unlock()
	stripeSteerLogLocked(p)
}

// stripeSteerLogLocked: the line, every 10 s at most, when a count moved.
// Caller holds p.mu.
func stripeSteerLogLocked(p *stripeSteerPickLog) {
	now := time.Now()
	asks := stripeSteerStat.askDial.Load() + stripeSteerStat.askSup.Load()
	if now.Sub(p.at) < 10*time.Second || p.total == p.last && asks == p.asks {
		return
	}
	p.at, p.last, p.asks = now, p.total, asks
	log.Printf("stripe steer: key=%v picks=%d on-cut-path=%d moved-off-cut=%d cut-fallback=%d asked-dial=%d asked-dial-dropped=%d steer-held=%d send-cut-only=%d send-kept-off=%d adopt-skip-cut=%d", lbStripeSteer, p.total, p.onCut, stripeSteerStat.moved.Load(), stripeSteerStat.fallback.Load(), stripeSteerStat.askDial.Load(), stripeSteerStat.askSup.Load(), stripeSteerStat.held.Load(), stripeSteerStat.sendCut.Load(), stripeSteerStat.sendOff.Load(), stripeSteerStat.adoptSkipCut.Load())
}

// stripeSteerMarkLocked (each steer tick): the links' steerCut flags, which
// the stripe sender reads without the manager's lock. Caller holds m.mu.
func (m *LinkManager) stripeSteerMarkLocked() {
	for _, ml := range m.links {
		if ml.mtr != nil {
			ml.mtr.steerCut.Store(m.stripeCutLocked(ml))
		}
	}
}

// stripeSubCutLocked (the stripe sender, either side): s rides a link on a
// cut path (Iran: the steering's report; the exit: its own judge).
func (m *mConn) stripeSubCutLocked(s *msub) bool {
	if !lbStripeSteer || s == nil {
		return false
	}
	mt := m.subMeterOf(s)
	return mt != nil && mt.steerCut.Load()
}

// stripeHasHealthyLocked (Iran, pickStripe): one of the connection's own
// links (avoid) is alive on a path not cut. Caller holds m.mu.
func (m *LinkManager) stripeHasHealthyLocked(avoid []any) bool {
	if len(avoid) == 0 {
		return false
	}
	for _, ml := range m.links {
		if ml.link.Alive() && avoided(ml.link, avoid) && mlPath(ml) != "" && !m.stripeCutLocked(ml) {
			return true
		}
	}
	return false
}

// stripeSteerMarkLocked (the exit, after each judge reading): the held
// links' steerCut flags from the judge's cut paths — none when every judged
// path is cut (nowhere better). Caller holds b.mu.
func (b *LB) stripeSteerMarkLocked() {
	if !lbStripeSteer || b.judge == nil {
		return
	}
	var cut map[int]bool
	all := true
	for j, p := range b.paths {
		q := b.judge.p[p.Name()]
		if q == nil || !q.have {
			continue
		}
		if !q.cut {
			all = false
			continue
		}
		if cut == nil {
			cut = map[int]bool{}
		}
		cut[j] = true
	}
	if all {
		cut = nil
	}
	n := 0
	for k, h := range b.hooks {
		if h.mtr == nil {
			continue
		}
		j, ok := b.held[k]
		c := ok && cut[j]
		h.mtr.steerCut.Store(c)
		if c {
			n++
		}
	}
	if stripeSteerLog && b.log != nil && (n > 0 || stripeSteerStat.sendOff.Load() > 0) {
		b.log("stripe steer exit: cut paths %d, links marked %d, chunks kept off a cut sub %d, on a cut sub (no other) %d", len(cut), n, stripeSteerStat.sendOff.Load(), stripeSteerStat.sendCut.Load())
	}
}
