package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

func setKey145(t *testing.T, b *atomic.Bool, on bool) {
	old := b.Load()
	b.Store(on)
	t.Cleanup(func() { b.Store(old) })
}

func TestRescue145Helpers(t *testing.T) {
	if pathHost("10.0.0.1>1.2.3.4") != "10.0.0.1" || pathHost("x") != "x" {
		t.Fatal("pathHost")
	}
	av, mk := takeRescueJoinMark([]any{1, rescueJoinMark{paths: map[string]bool{"a>b": true}}, 2})
	if mk == nil || !mk.paths["a>b"] || len(av) != 2 {
		t.Fatal("takeRescueJoinMark")
	}
	if _, mk := takeRescueJoinMark([]any{1}); mk != nil {
		t.Fatal("mark from nothing")
	}
	// budgets: off = base; on = n rings, bounded by RAM %, never below base
	setKey145(t, &rescueScaleOn, false)
	if rescueScaledCap(48<<20, 10000, 32<<10) != 48<<20 {
		t.Fatal("scaled with the key off")
	}
	setKey145(t, &rescueScaleOn, true)
	rescueMemTotal()
	oldT := rescueMemTotalV
	rescueMemTotalV = 4 << 30 // 4 GB: 4 % = 163 MB
	t.Cleanup(func() { rescueMemTotalV = oldT })
	if got := rescueScaledCap(48<<20, 100, 32<<10); got != 48<<20 {
		t.Fatalf("few conns: %d", got)
	}
	if got := rescueScaledCap(48<<20, 3000, 32<<10); got != 3000*32<<10 {
		t.Fatalf("3000 conns: %d", got)
	}
	if got := rescueScaledCap(48<<20, 100000, 32<<10); got != (4<<30)*4/100 {
		t.Fatalf("RAM bound: %d", got)
	}
	// freshness
	var mtr linkMeter
	now := ctrlNow()
	if linkFresh(&mtr, now) || linkFresh(nil, now) {
		t.Fatal("never-read link fresh")
	}
	mtr.lastRd.Store(now - int64(3*time.Second))
	mtr.lastWr.Store(now - int64(time.Second))
	if linkFresh(&mtr, now) {
		t.Fatal("silent link with bytes pending fresh")
	}
	mtr.lastWr.Store(now - int64(4*time.Second))
	if !linkFresh(&mtr, now) {
		t.Fatal("quiet link owing nothing not fresh")
	}
	mtr.lastRd.Store(now - int64(200*time.Millisecond))
	mtr.lastWr.Store(now)
	if !linkFresh(&mtr, now) {
		t.Fatal("hearing link not fresh")
	}
}

// joinDies: the first link opened after arm() carries nothing and is cut
// once the test calls kill (the rescue join on a dying path).
type joinDies struct {
	armed atomic.Bool
	got   atomic.Pointer[tLink]
}

func (j *joinDies) hook(l *tLink) {
	if j.armed.CompareAndSwap(true, false) {
		l.iran.drop.Store(true)
		l.exit.drop.Store(true)
		j.got.Store(l)
	}
}

// The raw rescue join dies before the ACK (its path dark too): under
// HS2_MCONN_RESCUE_RETRY a second join sends RESCUE again with the records
// written since Bi, and everything arrives once, in order.
func TestRescueRetryRawLostJoin(t *testing.T) {
	setKey145(t, &rescueRetryOn, true)
	setKey145(t, &rescueJoinPick, true)
	n, iran, exit := newRescuePair(t)
	jd := &joinDies{}
	n.hook = jd.hook
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
	retry0, lost0 := rs145Stat.retry.Load(), rescueStat.lostJoin.Load()
	jd.armed.Store(true)
	rescueNow(t, iran)
	l1 := jd.got.Load()
	if l1 == nil {
		t.Fatal("no join opened")
	}
	marked := false
	for _, a := range l1.avoid {
		if _, ok := a.(rescueJoinMark); ok {
			marked = true
		}
	}
	if !marked {
		t.Fatal("JOINPICK: no join mark in the join's avoid list")
	}
	mid := randBytes(6000, 5) // rides the doomed join: resent by the retry
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	l1.iran.Close()
	l1.exit.Close()
	mWaitFor(t, "retry", 2*time.Second, func() bool { return rs145Stat.retry.Load() > retry0 })
	if rescueStat.lostJoin.Load() == lost0 {
		t.Fatal("lost join not counted")
	}
	more, more2 := randBytes(20000, 6), randBytes(20000, 7)
	seq := func(fs ...func() error) func() error {
		return func() error {
			for _, f := range fs {
				if err := f(); err != nil {
					return err
				}
			}
			return nil
		}
	}
	both(t,
		seq(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seq(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// rs146: the raw rescue join's link goes silent (not closed) before the ACK:
// the silent set replaces it at once, and everything arrives once, in order.
func TestRescueRetryRawSilentJoin(t *testing.T) {
	setKey145(t, &rescueRetryOn, true)
	setKey145(t, &rescueJoinPick, true)
	n, iran, exit := newRescuePair(t)
	jd := &joinDies{}
	n.hook = jd.hook
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
	retry0, lost0 := rs145Stat.retry.Load(), rescueStat.lostJoin.Load()
	jd.armed.Store(true)
	rescueNow(t, iran)
	l1 := jd.got.Load()
	if l1 == nil {
		t.Fatal("no join opened")
	}
	marked := false
	for _, a := range l1.avoid {
		if _, ok := a.(rescueJoinMark); ok {
			marked = true
		}
	}
	if !marked {
		t.Fatal("JOINPICK: no join mark in the join's avoid list")
	}
	mid := randBytes(6000, 5) // rides the doomed join: resent by the retry
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	// rs146: the join's link stays open but silent (its path went dark right
	// after the join): the rescue watch's silent set names it
	sil0 := rs145Stat.retrySilent.Load()
	iran.mu.Lock()
	iran.rescueRetrySilentLocked(map[any]bool{iran.rscSub.key: true})
	iran.mu.Unlock()
	mWaitFor(t, "retry", 2*time.Second, func() bool { return rs145Stat.retry.Load() > retry0 })
	if rs145Stat.retrySilent.Load() == sil0 {
		t.Fatal("silent retry not counted")
	}
	_ = lost0
	more, more2 := randBytes(20000, 6), randBytes(20000, 7)
	seq := func(fs ...func() error) func() error {
		return func() error {
			for _, f := range fs {
				if err := f(); err != nil {
					return err
				}
			}
			return nil
		}
	}
	both(t,
		seq(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seq(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// The records rescue join dies before the answer: the retry starts RREC
// again on another link and nothing is lost or delivered twice.
func TestRescueRetryRecLostJoin(t *testing.T) {
	setKey145(t, &rescueRetryOn, true)
	n, iran, exit := newRRPair(t)
	jd := &joinDies{}
	n.hook = jd.hook
	set := rrLiveKeys(iran)
	blackhole(n, set)
	req, resp := randBytes(7000, 15), randBytes(9000, 16)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := exit.Write(resp); err != nil {
		t.Fatal(err)
	}
	retry0, lost0 := rs145Stat.retry.Load(), rrStat.lostJoin.Load()
	jd.armed.Store(true)
	rrNow(t, iran, set)
	l1 := jd.got.Load()
	if l1 == nil {
		t.Fatal("no join opened")
	}
	mid := randBytes(6000, 17)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	l1.iran.Close()
	l1.exit.Close()
	mWaitFor(t, "retry", 2*time.Second, func() bool { return rs145Stat.retry.Load() > retry0 })
	if rrStat.lostJoin.Load() == lost0 {
		t.Fatal("lost join not counted")
	}
	more, more2 := randBytes(20000, 18), randBytes(20000, 19)
	seq := func(fs ...func() error) func() error {
		return func() error {
			for _, f := range fs {
				if err := f(); err != nil {
					return err
				}
			}
			return nil
		}
	}
	both(t,
		seq(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seq(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// Retry off: a lost raw join still ends the connection (the old outcome).
func TestRescueRetryOffEnds(t *testing.T) {
	setKey145(t, &rescueRetryOn, false)
	n, iran, exit := newRescuePair(t)
	jd := &joinDies{}
	n.hook = jd.hook
	a := randBytes(3000, 21)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
	)
	n.link(0).iran.drop.Store(true)
	n.link(0).exit.drop.Store(true)
	jd.armed.Store(true)
	rescueNow(t, iran)
	l1 := jd.got.Load()
	l1.iran.Close()
	l1.exit.Close()
	mWaitFor(t, "end", 3*time.Second, func() bool {
		select {
		case <-iran.die:
			return true
		default:
			return false
		}
	})
	closeAndWait(t, iran, exit)
}
