package engine

import (
	"testing"
	"time"
)

// rc15d: the one-copy bytes of a hedged light conn (a burst tail past the
// head) on a primary that dies. rc15c saw "a sub died holding the only
// copy" here with the resend keys as the test binary leaves them (off: the
// envdef table turns HS2_MCONN_STRIPE_RESEND, HS2_MCONN_SUB_RESEND and the
// KEEPSTALL marks on only in loadEnvKeys, which tests do not run). With the
// production keys on, the dead primary's unacked chunks go again on the
// hedge sub: byte exact, no conn error.
func setRC15dResend(t *testing.T, on bool) {
	o1, o2, o3 := mconnSubResend.Load(), stripeResend.Load(), keepStallOn.Load()
	mconnSubResend.Store(on)
	stripeResend.Store(on)
	keepStallOn.Store(on)
	t.Cleanup(func() { mconnSubResend.Store(o1); stripeResend.Store(o2); keepStallOn.Store(o3) })
}

func rc15dOneCopyRun(t *testing.T, kill func(n *tNet)) (err error) {
	setHedge3(t, true)
	setRC15cBurst(t, true, 1024, 30*time.Millisecond)
	n, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	o0 := rc15cStat.oneCopyB.Load()
	for i := 0; i < 8 && err == nil; i++ {
		req := randBytes(6000, uint64(190+i))
		resp := randBytes(7000, uint64(210+i))
		errs := make(chan error, 2)
		go func() { errs <- xfer(iran, req, 700, 2*time.Millisecond) }()
		go func() { errs <- readNTimeout(exit, req, 5*time.Second) }()
		if i == 4 {
			time.Sleep(4 * time.Millisecond) // mid-burst: the tail is in flight on the primary
			kill(n)
		}
		for k := 0; k < 2; k++ {
			if e := <-errs; e != nil && err == nil {
				err = e
			}
		}
		if err != nil {
			break
		}
		go func() { errs <- xfer(exit, resp, 700, 2*time.Millisecond) }()
		go func() { errs <- readNTimeout(iran, resp, 5*time.Second) }()
		for k := 0; k < 2; k++ {
			if e := <-errs; e != nil && err == nil {
				err = e
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rc15cStat.oneCopyB.Load() == o0 {
		t.Fatal("no one-copy tail: the test does not exercise the case")
	}
	return err
}

func rc15dKillPrimary(n *tNet) {
	n.link(0).iran.Close()
	n.link(0).exit.Close()
}

// Production resend keys on: survives the primary's death mid-tail.
func TestRC15dOneCopyResend(t *testing.T) {
	setRC15dResend(t, true)
	r0 := subResendStat.resends.Load()
	if err := rc15dOneCopyRun(t, rc15dKillPrimary); err != nil {
		t.Fatalf("conn error with the resend on: %v", err)
	}
	t.Logf("sub resends %d", subResendStat.resends.Load()-r0)
}

// The resend keys off (the test-binary default): the old rule; logged, not
// asserted (it is the rc15c observation, timing dependent).
func TestRC15dOneCopyNoResend(t *testing.T) {
	setRC15dResend(t, false)
	err := rc15dOneCopyRun(t, rc15dKillPrimary)
	t.Logf("resend off: err=%v", err)
}

func setRC15dBursts(t *testing.T, n uint64, idle time.Duration) {
	ob, oi, obps := rc15dBursts, rc15cBurstIdle, rc15HeavyBps
	rc15dBursts, rc15cBurstIdle = n, idle
	rc15HeavyBps = 1 << 30 // the rate rule never heavy: isolate the rule
	t.Cleanup(func() { rc15dBursts, rc15cBurstIdle, rc15HeavyBps = ob, oi, obps })
}

// The rule alone: the first burst doubled, later bursts one copy; key off
// (0): never one copy here.
func TestRC15dFirstRule(t *testing.T) {
	setRC15dBursts(t, 1, 50*time.Millisecond)
	m := newMConn(94, false, nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rc15dOneCopyLocked() {
		t.Fatal("first burst: one copy")
	}
	m.sendOff += 3000
	if m.rc15dOneCopyLocked() {
		t.Fatal("first burst, later chunk: one copy")
	}
	m.c15Last = time.Now().Add(-time.Second)
	if !m.rc15dOneCopyLocked() {
		t.Fatal("second burst: doubled")
	}
	rc15dBursts = 3
	m.c15Last = time.Now().Add(-time.Second)
	if m.rc15dOneCopyLocked() {
		t.Fatal("N=3, third burst: one copy")
	}
	// FRESH_MS gate: no primary/hedge sub known -> not safe -> doubled
	rc15dBursts = 1
	of := rc15dFresh
	rc15dFresh = 100 * time.Millisecond
	defer func() { rc15dFresh = of }()
	g0 := rc15dStat.gated.Load()
	m.c15Last = time.Now().Add(-time.Second)
	if m.rc15dOneCopyLocked() {
		t.Fatal("gate: one copy with no fresh hedge sub")
	}
	if rc15dStat.gated.Load() != g0+1 {
		t.Fatal("gate not counted")
	}
}

// End to end, both directions, several request/response bursts: byte exact,
// later bursts one copy, the first doubled; then the primary dies mid-burst
// with the production resend keys on: no conn error.
func TestRC15dFirstBytes(t *testing.T) {
	setRC15dResend(t, true)
	setHedge3(t, true)
	setRC15dBursts(t, 1, 30*time.Millisecond)
	n, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	o0, d0 := rc15dStat.oneCopyB.Load(), hedge3Stat.dupB.Load()
	for i := 0; i < 8; i++ {
		if i == 5 {
			rc15dKillPrimary(n)
		}
		req := randBytes(2500, uint64(240+i))
		both(t, func() error { return xfer(iran, req, 800, 0) }, func() error { return readNTimeout(exit, req, 5*time.Second) })
		resp := randBytes(3500, uint64(260+i))
		both(t, func() error { return xfer(exit, resp, 900, 0) }, func() error { return readNTimeout(iran, resp, 5*time.Second) })
		time.Sleep(60 * time.Millisecond)
	}
	if rc15dStat.oneCopyB.Load() == o0 {
		t.Fatal("no one-copy burst")
	}
	if hedge3Stat.dupB.Load() == d0 {
		t.Fatal("first burst not doubled")
	}
}
