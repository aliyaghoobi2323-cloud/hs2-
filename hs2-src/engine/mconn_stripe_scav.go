package engine

import (
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
//     (at most once per stripeQEvery); under half the target it grows by one
//     sub every stripeQGrow (500 ms). Width 0 is the connection without the stripe
//     (everything on the primary, as lite's one-copy state).
//   - the Iran side needs no change: a sub the sender stops using carries no
//     rate, so Iran's growth rule (rate per sub) stops opening more, and
//     lite's idle rule closes the cold ones, which frees their links.

// stripeAlone: a stripe sub only on a link no user rides (default on;
// HS2_MCONN_STRIPE_ALONE=0: the first version's rule, fewest users).
var stripeAlone = os.Getenv("HS2_MCONN_STRIPE_ALONE") != "0"

// stripeLog: HS2_MCONN_STRIPE_LOG=1 logs the stripe's link picks and, once a
// second per connection, the sender's width (diagnostics).
var stripeLog = os.Getenv("HS2_MCONN_STRIPE_LOG") == "1"

// stripeQTarget: HS2_MCONN_STRIPE_QMS (ms, default 5; 0: no gate). The busy
// lab without a stripe: the median 1.2 ms, its p95 1.6 ms.
var stripeQTarget atomic.Int64

const (
	stripeQEvery = 100 * time.Millisecond
	stripeQGrow  = 500 * time.Millisecond
)

func init() {
	ms := 5.0
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
	if !measured {
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
	case q < target/2 && now.Sub(m.stGrowAt) >= stripeQGrow && m.stW < live:
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
// look counts (an idle link's smoothed RTT is stale).
const stripeBusyBytes = 8 << 10

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
