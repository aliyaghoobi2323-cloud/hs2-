package engine

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// b457 (r457 lab1, 12k user conns at the ZDU drain end): HS2_B457_PICKCAP=N
// (default 2 since rel69r-rc14; 0 = off) caps how many join-sub link picks (openNoLock's
// pickPlaneCall) run at once in this process.
//
// When the old Kharej process exits at the drain end, thousands of its links
// die together; on Iran every connection that had a sub there opens a new one
// at once (rescue joins, rr156 rst joins, light subs). Each pick scans every
// link (pickPlaneTiersFP, dupPathsCoreLocked, steerDraw, Alive, avoided), and a
// pick that finds nothing retries every mconnPickGap until mconnNextWait. The
// Iran CPU profile across the exit (r457 457p) put 47 % of 4.7 cores in
// mconnEdge.open's picks, with the process at 8-9 of 10 cores for ~2 s and the
// TUN/ping path waiting behind it.
//
// With the key on, at most N picks run at once; the rest wait their turn (or
// their connection's end). The pick itself, the open, the header and the
// install are unchanged, and the first sub's open (a new user connection) does
// not take a slot. No wire change.
var b457PickCap atomic.Pointer[chan struct{}]

func init() { b457SetPickCap(b457PickCapEnv(os.Getenv("HS2_B457_PICKCAP"))) }

// b457PickCapEnv: unset or not a number = the default 2 (rel69r-rc14); "0"
// (or below) turns the cap off (envIntDefault would read 0 as the default).
func b457PickCapEnv(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 2
	}
	return n
}

// b457SetPickCap sets the cap (n <= 0: off). Tests and init only: callers that
// hold a slot of the old cap give it back to that one.
func b457SetPickCap(n int) {
	if n <= 0 {
		b457PickCap.Store(nil)
		return
	}
	c := make(chan struct{}, n)
	b457PickCap.Store(&c)
}

var b457PickStat struct {
	waited atomic.Int64 // picks that had to wait for a slot
}

func b457Noop() {}

// b457PickAcquire takes a pick slot (a no-op with the key off) and returns
// its release. A done ctx returns without a slot: the caller's pick goes on
// (it ends at once on endedA or the deadline anyway).
func b457PickAcquire(ctx context.Context) func() { return b457PickAcquireEx(ctx, false) }

// b457PickAcquireEx: exempt (b458 HS2_B458_CAPLAT, a latency-class
// connection) skips the cap and is counted.
func b457PickAcquireEx(ctx context.Context, exempt bool) func() {
	rel, _ := b457PickAcquireDG(ctx, exempt, nil)
	return rel
}

// b457PickAcquireDG is b457PickAcquireEx for edge e's join (nil: none).
// dead (b459 HS2_B459_DEADGEN, mconn_b459_deadgen.go): e's pin generation
// died while the pick waited; no slot was taken and the join gives up.
func b457PickAcquireDG(ctx context.Context, exempt bool, e *mconnEdge) (rel func(), dead bool) {
	p := b457PickCap.Load()
	if p == nil {
		return b457Noop, false
	}
	if exempt {
		b458Stat.exempt.Add(1)
		return b457Noop, false
	}
	c := *p
	select {
	case c <- struct{}{}:
		return func() { <-c }, false
	default:
	}
	b457PickStat.waited.Add(1)
	rc15cCapWaited() // rc15c HS2_RC15C_CAPQUIET_MS (mconn_rc15c_burst.go)
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	t0 := time.Now() // b458 counters (mconn_b458_capstat.go)
	b458WaitStart()
	defer func() { b458Stat.waiting.Add(-1); b458Stat.wait.add(time.Since(t0)) }()
	if e != nil { // b449e CAPWAIT (mconn_b449_slowopen.go): the slow-open clock skips this wait
		if mc := e.b458MC.Load(); mc != nil {
			defer mc.b449CapWait()()
		}
	}
	if e != nil && e.b460Watched() { // HS2_B460_DGBATCH (mconn_b460_dgbatch.go): released in batches
		rc, unreg := e.lm.b460Join(*e.zduPin.Load())
		defer unreg()
		select {
		case c <- struct{}{}:
			return func() { <-c }, false
		case <-done:
			return b457Noop, false
		case <-rc:
			return b457Noop, true
		}
	}
	for {
		var wake <-chan struct{} // nil: never
		if e != nil && e.b459Watched() {
			wake = e.lm.b459WakeCh() // taken before the check: a death after it closes this one
			if e.b459PinDead() {
				b459Stat.dropped.Add(1)
				return b457Noop, true
			}
		}
		select {
		case c <- struct{}{}:
			return func() { <-c }, false
		case <-done:
			return b457Noop, false
		case <-wake:
		}
	}
}
