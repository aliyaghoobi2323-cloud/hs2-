package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// rs169: rel66 audit H1, M1, M3, M5 (docs/audit-rel66.md).

// joinHooks: the n-th link opened after arm(n) gets f (one per armed hook).
type joinHooks struct {
	fs  []func(*tLink)
	i   atomic.Int32
	arm atomic.Bool
}

func (j *joinHooks) hook(l *tLink) {
	if !j.arm.Load() {
		return
	}
	i := int(j.i.Add(1)) - 1
	if i < len(j.fs) && j.fs[i] != nil {
		j.fs[i](l)
	}
}

func seqF(fs ...func() error) func() error {
	return func() error {
		for _, f := range fs {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	}
}

// rs169Setup: a raw rescue pair, warmed up, its first link blackholed with
// a request and a response in flight.
func rs169Setup(t *testing.T, jh *joinHooks) (*tNet, *mConn, *mConn, []byte, []byte) {
	t.Helper()
	setKey145(t, &rescueRetryOn, true)
	n, iran, exit := newRescuePair(t)
	n.hook = jh.hook
	a, b := randBytes(3000, 1), randBytes(5000, 2)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 1000, 0) },
		func() error { return readN(iran, b) },
	)
	l0 := n.link(0)
	l0.iran.drop.Store(true)
	l0.exit.drop.Store(true)
	req, resp := randBytes(7000, 3), randBytes(9000, 4)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	return n, iran, exit, req, resp
}

func exitServed(m *mConn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rscPhase == 2
}

func silentRetry(t *testing.T, iran *mConn) {
	t.Helper()
	r0 := rs145Stat.retry.Load()
	iran.mu.Lock()
	iran.rescueRetrySilentLocked(map[any]bool{iran.rscSub.key: true})
	iran.mu.Unlock()
	mWaitFor(t, "retry", 2*time.Second, func() bool { return rs145Stat.retry.Load() > r0 })
}

func rs169Finish(t *testing.T, iran, exit *mConn, req, mid, resp []byte) {
	t.Helper()
	more, more2 := randBytes(20000, 6), randBytes(20000, 7)
	both(t,
		seqF(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seqF(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// H1 scenario A: RESCUE on join1 reaches the exit and is served, but the
// ACK (and the exit's resend) is lost on join1; the retry's RESCUE on join2
// must be answered again, not refused (rel66: protocol error, reset).
func TestRS169RetryAfterServedAckLost(t *testing.T) {
	jh := &joinHooks{fs: []func(*tLink){func(l *tLink) { l.exit.drop.Store(true) }}}
	_, iran, exit, req, resp := rs169Setup(t, jh)
	jh.arm.Store(true)
	rescueNow(t, iran)
	mWaitFor(t, "served", 2*time.Second, func() bool { return exitServed(exit) })
	mid := randBytes(6000, 5)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	silentRetry(t, iran)
	rs169Finish(t, iran, exit, req, mid, resp)
}

// H1 scenario B: join1 was only slow: its ACK reaches Iran after the retry
// sent RESCUE on join2 (and before the exit answers that one).
func TestRS169LateAckFromOldJoin(t *testing.T) {
	jh := &joinHooks{fs: []func(*tLink){
		func(l *tLink) { l.exit.stall(400 * time.Millisecond) },
		func(l *tLink) { l.iran.stall(1200 * time.Millisecond) },
	}}
	_, iran, exit, req, resp := rs169Setup(t, jh)
	jh.arm.Store(true)
	rescueNow(t, iran)
	mWaitFor(t, "served", 2*time.Second, func() bool { return exitServed(exit) })
	mid := randBytes(6000, 5)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	silentRetry(t, iran)
	rs169Finish(t, iran, exit, req, mid, resp)
}

// M1: after a raw retry the silent join must not stay primary: bytes
// written after the retry would ride it and vanish when it dies.
func TestRS169RetryMovesPrimary(t *testing.T) {
	jh := &joinHooks{fs: []func(*tLink){func(l *tLink) { l.iran.drop.Store(true); l.exit.drop.Store(true) }}}
	n, iran, exit, req, resp := rs169Setup(t, jh)
	jh.arm.Store(true)
	rescueNow(t, iran)
	mid := randBytes(6000, 5)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	silentRetry(t, iran)
	iran.mu.Lock()
	p, sub := iran.primary.Load(), iran.rscSub
	iran.mu.Unlock()
	if p != sub {
		t.Errorf("primary is not the retry join after the retry")
	}
	post := randBytes(8000, 8)
	iran.mu.Lock()
	iran.dupOff = true // a writer over mDupMaxRate: one copy, on the primary only
	iran.mu.Unlock()
	if _, err := iran.Write(post); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	l1 := n.link(1)
	l1.iran.Close()
	l1.exit.Close()
	mid = append(append([]byte(nil), mid...), post...)
	rs169Finish(t, iran, exit, req, mid, resp)
}

// M3: no records ring (HS2_MCONN_RESCUE_REC=0) and records written since Bi:
// the retry cannot resend them. A join that is only slow must then be waited
// for (the pre-RETRY behaviour), not replaced by a sure reset.
func TestRS169RetryNoRingKeepsSlowJoin(t *testing.T) {
	old := rescueRecOn.Load()
	rescueRecOn.Store(false)
	t.Cleanup(func() { rescueRecOn.Store(old) })
	jh := &joinHooks{fs: []func(*tLink){func(l *tLink) { l.iran.stall(600 * time.Millisecond); l.exit.stall(600 * time.Millisecond) }}}
	_, iran, exit, req, resp := rs169Setup(t, jh)
	jh.arm.Store(true)
	rescueNow(t, iran)
	mid := randBytes(6000, 5)
	go iran.Write(mid) // queued on the slow join
	time.Sleep(50 * time.Millisecond)
	iran.mu.Lock()
	iran.rescueRetrySilentLocked(map[any]bool{iran.rscSub.key: true})
	iran.mu.Unlock()
	rs169Finish(t, iran, exit, req, mid, resp)
}

// M5: a raw write cut by the rescue went out whole but was counted short
// (smux rc134 giveUp sends a detached frame and returns 0): the peer read
// past this side's Bi. Iran reports Ri > Bk: nothing to resend, no reset.
func TestRS169UncountedRawTail(t *testing.T) {
	n, iran, exit := newRescuePair(t)
	a, b := randBytes(3000, 1), randBytes(5000, 2)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 1000, 0) },
		func() error { return readN(iran, b) },
	)
	// the exit's last 700 raw bytes went out uncounted: its writer reports
	// n short and Write sends the rest (those 700 bytes first) as records
	const k = 700
	exit.wmu.Lock()
	exit.rawSent.Add(-k)
	exit.wmu.Unlock()
	l0 := n.link(0)
	l0.iran.drop.Store(true)
	l0.exit.drop.Store(true)
	req := randBytes(7000, 3)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	rescueNow(t, iran)
	mWaitFor(t, "served", 2*time.Second, func() bool {
		select {
		case <-exit.die:
			return true
		default:
		}
		return exitServed(exit)
	})
	if _, err := exit.Write(b[len(b)-k:]); err != nil { // the cut write's rest
		t.Fatal(err)
	}
	more, more2 := randBytes(20000, 6), randBytes(20000, 7)
	both(t,
		seqF(func() error { return readN(exit, req) }, func() error { return readN(exit, more) }),
		func() error { return readN(iran, more2) },
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// H1 scenario B, records rescue: the first RREC's answer reaches Iran on
// the join a retry replaced (it was only slow): ignored, not a reset.
func TestRS169RecLateAnswer(t *testing.T) {
	setKey145(t, &rescueRetryOn, true)
	n, iran, exit := newRRPair(t)
	jh := &joinHooks{fs: []func(*tLink){
		func(l *tLink) { l.exit.stall(400 * time.Millisecond) },
		func(l *tLink) { l.iran.stall(1200 * time.Millisecond) },
	}}
	n.hook = jh.hook
	set := rrLiveKeys(iran)
	blackhole(n, set)
	req, resp := randBytes(7000, 15), randBytes(9000, 16)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	jh.arm.Store(true)
	rrNow(t, iran, set)
	mid := randBytes(6000, 17)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	r0 := rs145Stat.retry.Load()
	iran.mu.Lock()
	iran.rescueRetrySilentLocked(map[any]bool{iran.rrSub.key: true})
	iran.mu.Unlock()
	mWaitFor(t, "retry", 2*time.Second, func() bool { return rs145Stat.retry.Load() > r0 })
	rs169Finish(t, iran, exit, req, mid, resp)
}
