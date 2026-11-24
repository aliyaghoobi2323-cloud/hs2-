package engine

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// b458 (r458 lab1): counters for HS2_B457_PICKCAP and the join delay, plus
// HS2_B458_CAPLAT (default on; acts only with the cap on): a latency-class
// connection's join picks do not take a cap slot, as a new connection's first
// sub does not.
//
// Latency class: the connection was call-like (both directions in the call
// band, mconn_call.go callLikeSample; a 20-50 ms game stream is in it too) or
// asked for a call upgrade within callRescueTTL. The mConn stamps b458LatAt
// where it sets callLikeAt / callUp; the edge reads it through b458MC without
// the mConn's lock.
//
// Counters (status.json pickcap_* / join_*; a "b458:" log line every 30 s
// while any moved):
//   - waits: picks that found no free slot, their total and longest wait,
//     the number waiting now and the most at once;
//   - exempt: latency-class picks that skipped the cap;
//   - join delay: from the open call (the request for an extra or rescue
//     sub) to its install, for joins that installed (rescue = er94 fresh or
//     rr156 rst joins; extra = the rest; lat = latency-class joins of either
//     kind), and the time to give up for joins that failed.
var b458CapLat atomic.Bool

func init() {
	b458CapLat.Store(envBoolDefault("HS2_B458_CAPLAT", true))
	go b458LogLoop()
}

type b458Dur struct {
	n, sumUs, maxUs atomic.Int64
}

func (d *b458Dur) add(x time.Duration) {
	us := x.Microseconds()
	d.n.Add(1)
	d.sumUs.Add(us)
	for {
		m := d.maxUs.Load()
		if us <= m || d.maxUs.CompareAndSwap(m, us) {
			return
		}
	}
}

// snap: count, total ms, max ms.
func (d *b458Dur) snap() (int64, int64, int64) {
	return d.n.Load(), d.sumUs.Load() / 1000, d.maxUs.Load() / 1000
}

var b458Stat struct {
	wait          b458Dur      // cap waits (found no free slot)
	waiting, peak atomic.Int64 // waiting now, most at once
	exempt        atomic.Int64 // latency-class picks that skipped the cap
	joinExtra     b458Dur      // request -> install, extra joins
	joinRescue    b458Dur      // request -> install, rescue/rst joins
	joinFail      b458Dur      // request -> give up, failed joins
	joinLat       b458Dur      // request -> install, latency-class joins
	lastSum       atomic.Int64
}

func b458WaitStart() {
	n := b458Stat.waiting.Add(1)
	for {
		p := b458Stat.peak.Load()
		if n <= p || b458Stat.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

// b458Latency: the edge's connection is latency-class now (see above).
func (e *mconnEdge) b458Latency() bool {
	m := e.b458MC.Load()
	if m == nil {
		return false
	}
	at := m.b458LatAt.Load()
	return at != 0 && time.Since(monoTime(at)) < callRescueTTL
}

// b458Stamp: where callLikeAt or callUp is set.
func (m *mConn) b458Stamp(now time.Time) { m.b458LatAt.Store(monoNano(now)) }

// b458Join records a join open's result (openNoLock's exit).
func b458Join(t0 time.Time, ok, rescue, lat bool) {
	d := time.Since(t0)
	switch {
	case !ok:
		b458Stat.joinFail.add(d)
		return
	case rescue:
		b458Stat.joinRescue.add(d)
	default:
		b458Stat.joinExtra.add(d)
	}
	if lat {
		b458Stat.joinLat.add(d)
	}
}

// B458Counts (status.json).
type B458Counts struct {
	CapWaits, CapWaitMs, CapWaitMaxMs, CapWaiting, CapPeak, CapExempt int64
	JoinExtraN, JoinExtraMs, JoinExtraMaxMs                           int64
	JoinRescueN, JoinRescueMs, JoinRescueMaxMs                        int64
	JoinFailN, JoinFailMs, JoinFailMaxMs                              int64
	JoinLatN, JoinLatMs, JoinLatMaxMs                                 int64
	DeadGenFast, DeadGenDropped, DeadGenWakes                         int64 // b459 (mconn_b459_deadgen.go)
	DGBatchFast, DGBatchReleased, DGBatchTicks                        int64 // b460 (mconn_b460_dgbatch.go)
}

// B458Snapshot (status.json).
func B458Snapshot() B458Counts {
	var c B458Counts
	s := &b458Stat
	c.CapWaits, c.CapWaitMs, c.CapWaitMaxMs = s.wait.snap()
	c.CapWaiting, c.CapPeak, c.CapExempt = s.waiting.Load(), s.peak.Load(), s.exempt.Load()
	c.JoinExtraN, c.JoinExtraMs, c.JoinExtraMaxMs = s.joinExtra.snap()
	c.JoinRescueN, c.JoinRescueMs, c.JoinRescueMaxMs = s.joinRescue.snap()
	c.JoinFailN, c.JoinFailMs, c.JoinFailMaxMs = s.joinFail.snap()
	c.JoinLatN, c.JoinLatMs, c.JoinLatMaxMs = s.joinLat.snap()
	c.DeadGenFast, c.DeadGenDropped, c.DeadGenWakes = b459Stat.fast.Load(), b459Stat.dropped.Load(), b459Stat.wakes.Load()
	c.DGBatchFast, c.DGBatchReleased, c.DGBatchTicks = b460Stat.fast.Load(), b460Stat.released.Load(), b460Stat.ticks.Load()
	return c
}

func b458Line() string {
	c := B458Snapshot()
	avg := func(n, ms int64) int64 {
		if n == 0 {
			return 0
		}
		return ms / n
	}
	return fmt.Sprintf("b458: capwait n=%d avg=%dms max=%dms waiting=%d peak=%d exempt=%d | join extra n=%d avg=%dms max=%dms rescue n=%d avg=%dms max=%dms fail n=%d avg=%dms max=%dms lat n=%d avg=%dms max=%dms | deadgen fast=%d dropped=%d wakes=%d | dgbatch fast=%d released=%d ticks=%d",
		c.CapWaits, avg(c.CapWaits, c.CapWaitMs), c.CapWaitMaxMs, c.CapWaiting, c.CapPeak, c.CapExempt,
		c.JoinExtraN, avg(c.JoinExtraN, c.JoinExtraMs), c.JoinExtraMaxMs,
		c.JoinRescueN, avg(c.JoinRescueN, c.JoinRescueMs), c.JoinRescueMaxMs,
		c.JoinFailN, avg(c.JoinFailN, c.JoinFailMs), c.JoinFailMaxMs,
		c.JoinLatN, avg(c.JoinLatN, c.JoinLatMs), c.JoinLatMaxMs,
		c.DeadGenFast, c.DeadGenDropped, c.DeadGenWakes, c.DGBatchFast, c.DGBatchReleased, c.DGBatchTicks)
}

// b458Sum moves whenever a counter does.
func b458Sum() int64 {
	s := &b458Stat
	return s.wait.n.Load() + s.waiting.Load() + s.exempt.Load() + s.joinExtra.n.Load() + s.joinRescue.n.Load() + s.joinFail.n.Load() + b459Stat.fast.Load() + b459Stat.dropped.Load() + b460Stat.fast.Load() + b460Stat.released.Load()
}

var b458LogOnce sync.Once

// b458LogLoop: the line every 30 s while a counter moved or a pick waits.
func b458LogLoop() {
	b458LogOnce.Do(func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			if v := b458Sum(); v != b458Stat.lastSum.Swap(v) || b458Stat.waiting.Load() > 0 {
				log.Print(b458Line())
			}
		}
	})
}
