package engine

import (
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of HS2_MCONN=lite (adaptive redundancy): one sub at start, a second
// one only for a light connection, never bulk.

// setLite switches the lite mode for a test (restored after it).
func setLite(t *testing.T, on bool) {
	t.Helper()
	old := mconnLite.Load()
	mconnLite.Store(on)
	t.Cleanup(func() { mconnLite.Store(old) })
}

// resetLiteBudget empties the duplication budget's buckets.
func resetLiteBudget() {
	for i := range liteBud.slots {
		s := &liteBud.slots[i]
		s.sec.Store(0)
		s.all.Store(0)
		s.dup.Store(0)
	}
}

// newLitePair: a connected Iran/exit pair in lite mode, one sub up.
func newLitePair(t *testing.T) (*tNet, *mConn, *mConn) {
	t.Helper()
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(9)}
	iran, err := newMConnIran(9, n.open)
	if err != nil {
		t.Fatal(err)
	}
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 || n.count(planeBulk) != 0 {
		t.Fatalf("lite start: %d subs, %d light opens", liveSubs(iran), n.count(planeLight))
	}
	return n, iran, n.exit
}

// A new lite connection has one sub; idle (far under mDupMaxRate) it gets a
// second one after mLiteAfter, on another link, and the upgrade is counted.
func TestMConnLiteStartsSingleThenDual(t *testing.T) {
	before := runtime.NumGoroutine()
	ups := obs.mconnLiteUpgrade.Load()
	n, iran, exit := newLitePair(t)
	time.Sleep(mLiteAfter / 2)
	if liveSubs(iran) != 1 {
		t.Fatal("second sub opened before mLiteAfter")
	}
	mWaitFor(t, "upgrade to two subs", mLiteAfter+2*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2
	})
	if l := n.link(1); l.plane != planeLight || len(l.avoid) == 0 || l.avoid[0] != 0 {
		t.Fatalf("second sub: plane %v avoid %v, want light avoiding link 0", l.plane, l.avoid)
	}
	if obs.mconnLiteUpgrade.Load() != ups+1 {
		t.Fatal("upgrade not counted")
	}
	if obs.mconnLiteDual.Load() < 1 {
		t.Fatal("dual gauge not raised")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Pings: dual after ~1 s, then every ping on both subs; with one sub stalled
// for 500 ms the pings still arrive in time.
func TestMConnLitePingsDualAndStall(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	var w, l atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 150, &w, &l, &wg) // 3 s of pings
	mWaitFor(t, "two subs", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	n.link(1).iran.stall(500 * time.Millisecond)
	wg.Wait()
	t.Logf("write max %v, delay max %v", time.Duration(w.Load()), time.Duration(l.Load()))
	if l.Load() > int64(50*time.Millisecond) {
		t.Fatal("a ping waited behind the stalled sub")
	}
	mWaitFor(t, "both copies", 2*time.Second, func() bool {
		return n.link(1).iran.wrote.Load() > 0 && n.link(0).iran.wrote.Load() > n.link(1).iran.wrote.Load()
	})
	if modeOf(iran) != planeLight || n.count(planeBulk) != 0 {
		t.Fatal("left light mode")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A fast download never gets a second sub (nor a bulk one); bytes intact.
func TestMConnLiteDownloadStaysSingle(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	n.link(0).exit.rate.Store(4 << 20) // ~1.5 s for 6 MiB
	data := randBytes(6<<20, 11)
	errs := make(chan error, 2)
	go func() { errs <- writeRandomly(exit, data, 12, 64<<10) }()
	go func() { errs <- readAllCheck(iran, data) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 || n.count(planeBulk) != 0 || modeOf(iran) != planeLight {
		t.Fatalf("download: %d subs, %d light, %d bulk", liveSubs(iran), n.count(planeLight), n.count(planeBulk))
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A steady 2 Mbit/s call (both ways) never gets a second sub.
func TestMConnLiteCallStaysSingle(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	stream := func(from, to *mConn) {
		defer wg.Done()
		go io.Copy(io.Discard, to)
		p := make([]byte, 2500) // 2500 B / 10 ms = 2 Mbit/s
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := from.Write(p); err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go stream(iran, exit)
	go stream(exit, iran)
	time.Sleep(mLiteAfter + 2*time.Second)
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 || n.count(planeBulk) != 0 {
		t.Fatalf("call: %d subs, %d light, %d bulk", liveSubs(iran), n.count(planeLight), n.count(planeBulk))
	}
	close(stop)
	wg.Wait()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// In dual mode the death of one sub (the first one too) leaves the connection
// on the other; it is counted as survived.
func TestMConnLiteDualSubDeath(t *testing.T) {
	before := runtime.NumGoroutine()
	survived := obs.mconnSubSurvived.Load()
	n, iran, exit := newLitePair(t)
	mWaitFor(t, "two subs", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	var w, l atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 50, &w, &l, &wg)
	pingPong(t, exit, iran, 50, &w, &l, &wg)
	time.Sleep(300 * time.Millisecond)
	n.link(0).iran.Close() // the first (primary) sub dies
	n.link(0).exit.Close()
	wg.Wait()
	select {
	case <-iran.GetDieCh():
		t.Fatal("mconn died with its sub")
	case <-exit.GetDieCh():
		t.Fatal("exit mconn died with its sub")
	default:
	}
	if obs.mconnSubSurvived.Load() == survived {
		t.Fatal("sub death not counted")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// The 5% duplication cap stops new upgrades (and counts the refusals).
func TestMConnLiteCapStopsUpgrade(t *testing.T) {
	before := runtime.NumGoroutine()
	capped := obs.mconnLiteCapped.Load()
	n, iran, exit := newLitePair(t)
	// the process duplicated 10% of its bytes in this second
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	time.Sleep(mLiteAfter + 1500*time.Millisecond)
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 {
		t.Fatalf("upgraded under the cap: %d subs", liveSubs(iran))
	}
	if obs.mconnLiteCapped.Load() == capped {
		t.Fatal("refusal not counted")
	}
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// The exit must not put its only copy of some bytes on a second sub that
// Iran may be closing as idle (Iran decides alone, after mLiteIdle): with
// its first sub's queue full on a lossy link, a burst after a quiet spell
// went to the second sub alone, Iran closed that sub a moment later, the
// first sub had just missed copies, and the exit reset the connection (lab,
// 2% loss: reels cut ~11 s in, at mLiteIdle after the upgrade). Iran, for
// its part, counts a sub that still receives as busy.
func TestMConnLiteIdleCloseNoLoss(t *testing.T) {
	// shorter than in production, still over mHoleSafe (the exit resets a
	// connection whose surviving sub missed a copy within mHoleSafe)
	old := mLiteIdle
	mLiteIdle = mHoleSafe + 2*time.Second
	t.Cleanup(func() { mLiteIdle = old })
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	req := randBytes(600, 101)
	both(t, func() error { return xfer(iran, req, 600, 0) }, func() error { return readN(exit, req) })
	mWaitFor(t, "two subs", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	// quiet until just before Iran's idle rule closes the second sub; then
	// a burst down while the first sub is stalled (loss recovery)
	time.Sleep(mLiteIdle - 700*time.Millisecond)
	n.link(0).exit.stall(400 * time.Millisecond)
	down := randBytes(96<<10, 102)
	both(t,
		func() error { return xfer(exit, down, 8<<10, 0) },
		func() error { return readN(iran, down) },
	)
	time.Sleep(time.Second) // past the moment the old rule closed the sub
	more := randBytes(64<<10, 103)
	both(t,
		func() error { return xfer(exit, more, 8<<10, 0) },
		func() error { return readN(iran, more) },
	)
	select {
	case <-iran.GetDieCh():
		t.Fatalf("Iran ended: %v", iran.err)
	case <-exit.GetDieCh():
		t.Fatalf("exit ended: %v", exit.err)
	default:
	}
	// quiet both ways now: the second sub is closed after mLiteIdle
	mWaitFor(t, "idle second sub closed", mLiteIdle+2*time.Second, func() bool { return liveSubs(iran) == 1 })
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}
