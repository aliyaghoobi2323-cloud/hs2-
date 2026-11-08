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
	stNoGate bool // the last decision found nothing to measure: no gate until the next
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
	target := time.Duration(stripeQTarget.Load())
	if target <= 0 || m.iran {
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
		if mtr := subMeter(s); mtr != nil {
			own = append(own, mtr)
		}
	}
	q, measured := stripePathQ.delay(now, own)
	if m.stNoGate = !measured; m.stNoGate {
		return -1 // no exit link tracked (nothing to measure the path with)
	}
	m.stQ = q
	live := 0
	for _, s := range m.subs {
		if s.stripe && s != p && !s.retired && !s.dead {
			live++
		}
	}
	switch {
	case q > target:
		if m.stW > 0 {
			m.stW /= 2
			stripeStat.cuts.Add(1)
			m.tr.ev("stripe: queue %v > %v, width -> %d", q, target, m.stW)
		}
		m.stGrowAt = now
		stripeCut(now)
	case q < target/2 && now.Sub(m.stGrowAt) >= stripeQGrow && m.stW < live && stripeGrowToken(now):
		m.stW++
		m.stGrowAt = now
	}
	m.stW = min(m.stW, live)
	if stripeLog && now.Sub(m.stLogAt) >= time.Second {
		m.stLogAt = now
		log.Printf("stripe gate %x: q=%v w=%d live=%d", m.id&0xffffff, q.Round(100*time.Microsecond), m.stW, live)
	}
	return m.stW
}

// stripeGlobal: the growth of every stripe of the process together. Each
// connection's own controller would add a sub every stripeQGrow, so N
// stripes (a busy server: several long downloads) grew N times as fast and
// overshot the path's room before the queue showed; and after one queue,
// another stripe's growth refilled it. So: one sub more per stripeQGrow for
// the whole process, none for stripeCutHold after any cut.
var stripeGlobal struct {
	mu            sync.Mutex
	growAt, cutAt time.Time
}

// stripeCutHold: no stripe grows for this long after a queue.
const stripeCutHold = 2 * time.Second

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
	q     time.Duration
}

// stripeLinkQ: one link at the last measurement.
type stripeLinkQ struct {
	acked uint64
	q     time.Duration
	busy  bool // acknowledged stripeBusyBytes since the look before
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
			st, ok := tcpStats(mtr.tcp)
			if !ok {
				l.busy = false
				continue
			}
			l.busy = !first && l.acked != 0 && st.bytesAcked >= l.acked+stripeBusyBytes && st.minRttUs > 0
			l.acked = st.bytesAcked
			l.q = 0
			if st.rttUs > st.minRttUs {
				l.q = time.Duration(st.rttUs-st.minRttUs) * time.Microsecond
			}
		}
	}
	var ds []time.Duration
	for mtr, l := range p.links {
		if l.busy && !slices.Contains(own, mtr) {
			ds = append(ds, l.q)
		}
	}
	if len(ds) == 0 {
		return 0, true
	}
	slices.Sort(ds)
	return ds[len(ds)/2], true
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
