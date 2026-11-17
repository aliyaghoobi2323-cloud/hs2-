package engine

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// rs169 lab rt (retry-heavy A/B): every retried raw RESCUE was a fresh one
// (er94 role 4: the join opened the conn on the exit). The exit served it
// but did not remember the answer, so the retry's RESCUE on a newer join was
// refused (protocol error: conn reset), 3-11 times per run.

// freshRetryPair: Iran's first sub lost in a blackhole (the exit never sees
// it); j1 (if set) shapes join k (1: the fresh rescue join; false: the exit
// never sees it); fresh[k]: open k carried the role-4 mark.
func freshRetryPair(t *testing.T, id uint64, j1 func(k int, ia, eb *tSub) bool) (*mConn, *mConn, []byte, func(int) bool) {
	t.Helper()
	setRescue(t, true)
	setLite(t, true)
	setKey145(t, &rescueRetryOn, true)
	resetLiteBudget()
	exit := newMConnExit(id)
	exit.mu.Lock()
	exit.rscFresh = true
	exit.mu.Unlock()
	var mu sync.Mutex
	opens := 0
	marks := map[int]bool{}
	open := func(p mPlane, avoid []any) (mSub, any, error) {
		a, b := net.Pipe()
		ia, eb := newTSub(a), newTSub(b)
		mu.Lock()
		key := opens
		opens++
		_, marks[key] = takeRescueFreshMark(avoid)
		mu.Unlock()
		if key == 0 || key > 0 && j1 != nil && !j1(key, ia, eb) {
			ia.raw = key == 0
			go io.Copy(io.Discard, eb)
			return ia, key, nil
		}
		exit.attach(eb, mconnRoleJoin)
		return ia, key, nil
	}
	iran, err := newMConnIran(id, open)
	if err != nil {
		t.Fatal(err)
	}
	iran.rescueEnable()
	req := randBytes(6000, 41)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	return iran, exit, req, func(k int) bool {
		mu.Lock()
		defer mu.Unlock()
		return marks[k]
	}
}

func ackLost(k int, ia, eb *tSub) bool {
	if k == 1 {
		eb.drop.Store(true)
	}
	return true
}

// The fresh RESCUE is served on join 1, its ACK lost; Iran retries on join
// 2: the exit answers again (not refused), the response it wrote on join 1
// comes again from its records ring, the prefix [0, Bi) Iran resends with
// the second answer and the records it resent with the retry are delivered
// once, the conn goes on byte-exact.
func TestRS169FreshRetryAfterAckLost(t *testing.T) {
	iran, exit, req, _ := freshRetryPair(t, 41, ackLost)
	ref0, rep0 := rescueStat.refused.Load(), rs169Stat.repeat.Load()
	rescueNow(t, iran)
	mWaitFor(t, "served", 2*time.Second, func() bool { return exitServed(exit) })
	resp := randBytes(9000, 42) // rides join 1 out: lost
	if err := exitWrite(exit, resp); err != nil {
		t.Fatal(err)
	}
	mid := randBytes(5000, 43)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	silentRetry(t, iran)
	more, more2 := randBytes(20000, 44), randBytes(20000, 45)
	both(t,
		seqF(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seqF(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	if n := rescueStat.refused.Load() - ref0; n != 0 {
		t.Fatalf("the retried fresh RESCUE was refused %d times", n)
	}
	if rs169Stat.repeat.Load() == rep0 {
		t.Fatal("no repeat answer counted")
	}
	closeAndWait(t, iran, exit)
}

// freshRetryFinish: Iran retries now; every byte arrives once, in order.
func freshRetryFinish(t *testing.T, iran, exit *mConn, req, mid, resp []byte) {
	t.Helper()
	silentRetry(t, iran)
	more, more2 := randBytes(20000, 54), randBytes(20000, 55)
	both(t,
		seqF(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seqF(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		func() error { return xfer(iran, more, 4000, 0) },
		func() error { return xfer(exit, more2, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// The fresh join was only slow: its ACK reaches Iran after the retry (Iran
// resends [0, Bi) on both joins), the exit answers the retry's RESCUE too
// (Iran ignores it) and resends what it wrote: each byte delivered once.
func TestRS169FreshLateAck(t *testing.T) {
	iran, exit, req, _ := freshRetryPair(t, 42, func(k int, ia, eb *tSub) bool {
		switch k {
		case 1:
			eb.stall(300 * time.Millisecond)
		case 2: // the retry's RESCUE reaches the exit after the late ACK reached Iran
			ia.stall(900 * time.Millisecond)
		}
		return true
	})
	ref0 := rescueStat.refused.Load()
	rescueNow(t, iran)
	mWaitFor(t, "served", 2*time.Second, func() bool { return exitServed(exit) })
	resp, mid := randBytes(9000, 52), randBytes(5000, 53)
	if err := exitWrite(exit, resp); err != nil {
		t.Fatal(err)
	}
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	la0, dup0 := rs169Stat.lateAns.Load(), rs169Stat.dupAns.Load()
	freshRetryFinish(t, iran, exit, req, mid, resp)
	if n := rescueStat.refused.Load() - ref0; n != 0 {
		t.Fatalf("refused %d", n)
	}
	mWaitFor(t, "late ACK taken, repeat ignored", 2*time.Second, func() bool {
		return rs169Stat.lateAns.Load() > la0 && rs169Stat.dupAns.Load() > dup0
	})
}

// The fresh join never reached the exit (no conn there): the retry goes as
// role 4 too and opens it.
func TestRS169FreshRetryOpensConn(t *testing.T) {
	iran, exit, req, mark := freshRetryPair(t, 43, func(k int, ia, eb *tSub) bool { return k != 1 })
	rescueNow(t, iran)
	if !mark(1) {
		t.Fatal("the rescue join was not role 4")
	}
	mid := randBytes(5000, 63)
	if _, err := iran.Write(mid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	silentRetry(t, iran)
	if !mark(2) {
		t.Fatal("the retry of a fresh RESCUE was not role 4")
	}
	mWaitFor(t, "served", 2*time.Second, func() bool { return exitServed(exit) })
	resp, more, more2 := randBytes(9000, 62), randBytes(20000, 64), randBytes(20000, 65)
	both(t,
		seqF(func() error { return readN(exit, req) }, func() error { return readN(exit, mid) }, func() error { return readN(exit, more) }),
		seqF(func() error { return readN(iran, resp) }, func() error { return readN(iran, more2) }),
		seqF(func() error { return exitWrite(exit, resp) }, func() error { return xfer(exit, more2, 4000, 0) }),
		func() error { return xfer(iran, more, 4000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// HS2_MCONN_RESCUE_IDEM=0: rel66 (the repeat is refused, no role-4 retry).
func TestRS169FreshRetryIdemOff(t *testing.T) {
	setKey145(t, &rs169Fix, false)
	iran, exit, _, mark := freshRetryPair(t, 44, ackLost)
	ref0 := rescueStat.refused.Load()
	rescueNow(t, iran)
	mWaitFor(t, "served", 2*time.Second, func() bool { return exitServed(exit) })
	silentRetry(t, iran)
	if mark(2) {
		t.Fatal("role-4 retry with IDEM=0")
	}
	mWaitFor(t, "refused", 2*time.Second, func() bool { return rescueStat.refused.Load() > ref0 })
	exit.mu.Lock()
	set := exit.rscSrv.set
	exit.mu.Unlock()
	if set {
		t.Fatal("answer kept with IDEM=0")
	}
	iran.Close()
	exit.Close()
}
