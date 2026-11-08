package engine

import (
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of HS2_MCONN_CALL (mconn_call.go): a call-like lite connection keeps
// a copy on a second sub; anything else is as in lite.

func setCall(t *testing.T, v string) {
	t.Helper()
	old, oldB := mconnCall.Load(), mconnCallBudget.Load()
	setMConnCall(v)
	t.Cleanup(func() { mconnCall.Store(old); mconnCallBudget.Store(oldB) })
	resetCallBudget()
}

func resetCallBudget() {
	for i := range callBud.slots {
		s := &callBud.slots[i]
		s.sec.Store(0)
		s.all.Store(0)
		s.dup.Store(0)
	}
}

// callStream: a steady two-way stream, size bytes every 10 ms from each
// side, until stop; it records the largest one-way delay seen on each side.
func callStream(iran, exit *mConn, size int, stop chan struct{}, wg *sync.WaitGroup, maxLat *atomic.Int64) {
	one := func(from, to *mConn) {
		defer wg.Done()
		go func() {
			b := make([]byte, size)
			for {
				if _, err := io.ReadFull(to, b); err != nil {
					return
				}
				sent := int64(0)
				for j := 0; j < 8; j++ {
					sent = sent<<8 | int64(b[j])
				}
				if d := time.Now().UnixNano() - sent; d > maxLat.Load() {
					maxLat.Store(d)
				}
			}
		}()
		p := make([]byte, size)
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			ts := time.Now().UnixNano()
			for j := 7; j >= 0; j-- {
				p[j] = byte(ts)
				ts >>= 8
			}
			if _, err := from.Write(p); err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go one(iran, exit)
	go one(exit, iran)
}

func TestCallLikeSample(t *testing.T) {
	var prev float64
	var streak int
	s := func(sent, got uint64) bool { return callLikeSample(sent, got, time.Second, &prev, &streak) }
	// steady 2 Mbit/s both ways: call-like from the second sample
	if s(250_000, 250_000) || !s(250_000, 240_000) || !s(260_000, 250_000) {
		t.Fatal("steady call not call-like")
	}
	// a one-way stream (requests only back): not
	prev, streak = 0, 0
	for i := 0; i < 4; i++ {
		if s(300_000, 500) {
			t.Fatal("one-way stream call-like")
		}
	}
	// a download: not
	prev, streak = 0, 0
	for i := 0; i < 4; i++ {
		if s(2_000_000, 20_000) {
			t.Fatal("download call-like")
		}
	}
	// bursts (a page, then quiet, then a page): never twice in a row
	prev, streak = 0, 0
	for i := 0; i < 6; i++ {
		b := uint64(20_000)
		if i%2 == 0 {
			b = 350_000
		}
		if s(b, 5_000) {
			t.Fatal("bursty connection call-like")
		}
	}
}

// Key on: a steady 2 Mbit/s call gets a second sub, both sides then copy
// every chunk on both, and a 400 ms stall of the first link does not delay it.
func TestMConnCallDualAndCopies(t *testing.T) {
	setCall(t, "1")
	before := runtime.NumGoroutine()
	ups := callStat.upgrades.Load()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat atomic.Int64
	callStream(iran, exit, 2500, stop, &wg, &lat) // 2500 B / 10 ms = 2 Mbit/s
	mWaitFor(t, "upgrade to two subs", mLiteAfter+4*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2
	})
	if callStat.upgrades.Load() == ups {
		t.Fatal("call upgrade not counted")
	}
	// both sides copy as a call within a few windows
	mWaitFor(t, "copies both ways", 5*time.Second, func() bool {
		iran.mu.Lock()
		a := iran.callOn
		iran.mu.Unlock()
		exit.mu.Lock()
		b := exit.callOn
		exit.mu.Unlock()
		return a && b
	})
	w1i, w1e := n.link(1).iran.wrote.Load(), n.link(1).exit.wrote.Load()
	time.Sleep(time.Second)
	if di, de := n.link(1).iran.wrote.Load()-w1i, n.link(1).exit.wrote.Load()-w1e; di < 150_000 || de < 150_000 {
		t.Fatalf("second sub carried %d / %d bytes in 1 s, want the whole call each way", di, de)
	}
	lat.Store(0)
	n.link(0).iran.stall(400 * time.Millisecond)
	n.link(0).exit.stall(400 * time.Millisecond)
	time.Sleep(time.Second)
	if d := time.Duration(lat.Load()); d > 150*time.Millisecond {
		t.Fatalf("a frame waited %v behind the stalled first link", d)
	}
	close(stop)
	wg.Wait()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// The call budget refuses the upgrade (counted), and a call over it copies
// no more.
func TestMConnCallBudget(t *testing.T) {
	setCall(t, "1")
	before := runtime.NumGoroutine()
	refused := callStat.refused.Load()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat atomic.Int64
	fill := func() {
		now := time.Now().Unix()
		for s := now - mCallBudgetWin + 1; s <= now+1; s++ {
			callBud.add(s, 0, int(mconnCallBudget.Load())*2)
		}
	}
	fill()
	callStream(iran, exit, 2500, stop, &wg, &lat)
	deadline := time.Now().Add(mLiteAfter + 3*time.Second)
	for time.Now().Before(deadline) {
		fill()
		time.Sleep(200 * time.Millisecond)
	}
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 {
		t.Fatalf("upgraded over the budget: %d subs", liveSubs(iran))
	}
	if callStat.refused.Load() == refused {
		t.Fatal("refusal not counted")
	}
	close(stop)
	wg.Wait()
	resetCallBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// HS2_MCONN_CALL_SUBS=4: Iran opens two more subs; then every chunk goes
// on two of the four in turn (each carries about half the call, both ways),
// and the death of one sub leaves the connection whole.
func TestMConnCallPaired(t *testing.T) {
	setCall(t, "1")
	old := mconnCallSubs
	mconnCallSubs = 4
	t.Cleanup(func() { mconnCallSubs = old })
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat atomic.Int64
	callStream(iran, exit, 2500, stop, &wg, &lat)
	mWaitFor(t, "four subs", mLiteAfter+8*time.Second, func() bool {
		return liveSubs(iran) == 4 && liveSubs(exit) == 4
	})
	time.Sleep(500 * time.Millisecond)
	var w0 [4][2]int64
	for i := range w0 {
		w0[i] = [2]int64{n.link(i).iran.wrote.Load(), n.link(i).exit.wrote.Load()}
	}
	time.Sleep(time.Second)
	for i := range w0 {
		di, de := n.link(i).iran.wrote.Load()-w0[i][0], n.link(i).exit.wrote.Load()-w0[i][1]
		// 250 KB/s each way, every sub about half of it
		if di < 80_000 || di > 180_000 || de < 80_000 || de > 180_000 {
			t.Fatalf("sub %d carried %d / %d bytes in 1 s, want about half the call each way", i, di, de)
		}
	}
	lat.Store(0)
	n.link(1).iran.stall(400 * time.Millisecond)
	n.link(1).exit.stall(400 * time.Millisecond)
	time.Sleep(time.Second)
	if d := time.Duration(lat.Load()); d > 150*time.Millisecond {
		t.Fatalf("a frame waited %v behind one stalled link", d)
	}
	time.Sleep(mHoleSafe) // the stall's misses are old now
	n.link(2).iran.Close()
	n.link(2).exit.Close()
	time.Sleep(500 * time.Millisecond)
	select {
	case <-iran.GetDieCh():
		t.Fatalf("Iran ended with one of four subs: %v", iran.err)
	case <-exit.GetDieCh():
		t.Fatalf("exit ended with one of four subs: %v", exit.err)
	default:
	}
	close(stop)
	wg.Wait()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Key on, a one-way medium stream (a video, small requests back): one sub.
func TestMConnCallOneWayStaysSingle(t *testing.T) {
	setCall(t, "1")
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go io.Copy(io.Discard, iran)
	go func() {
		defer close(done)
		p := make([]byte, 2500)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := exit.Write(p); err != nil {
				return
			}
		}
	}()
	time.Sleep(mLiteAfter + 3*time.Second)
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 {
		t.Fatalf("one-way stream: %d subs", liveSubs(iran))
	}
	close(stop)
	<-done
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// On the exit, a join sub carrying a call's copies at a download's rate is
// not a heavy stream of its link with the key on; a first sub still is, and
// with the key off a join sub is too (as before).
func TestMConnCallExitCopyNotHeavy(t *testing.T) {
	run := func(key string, role byte) int32 {
		setCall(t, key)
		tr := newExitTraffic(nil)
		var mtr linkMeter
		c := openExitSub(tr, &mtr, role)
		now := time.Now()
		for i := 1; i <= 6; i++ { // 2 s ticks at 500 KB/s
			c.bytes.Add(1 << 20)
			tr.sample(now.Add(time.Duration(i) * healthTick))
		}
		tr.done(c)
		return mtr.heavyN.Load()
	}
	if n := run("1", mconnRoleJoin); n != 0 {
		t.Fatalf("key on: a copy sub counted heavy (%d)", n)
	}
	if n := run("1", mconnRoleFirst); n != 1 {
		t.Fatalf("key on: a first sub not counted heavy (%d)", n)
	}
	if n := run("", mconnRoleJoin); n != 1 {
		t.Fatalf("key off: a join sub not counted heavy (%d)", n)
	}
}
