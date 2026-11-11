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

// HS2_MCONN_CALL_PICK: the two subs whose links are not repairing a loss
// and hold the least unsent come first; ties go in turn.
func TestMConnCallPickScores(t *testing.T) {
	m := newMConn(1, true, nil)
	m.lite = true
	defer m.end(errMConnClosed, false)
	var live []*msub
	for i := 0; i < 4; i++ {
		live = append(live, &msub{m: m, idx: i})
	}
	set := func(sc ...int64) {
		m.scoreAt, m.scoreN = time.Now(), len(live)
		for i := range live {
			m.scoreSub[i], m.score[i] = live[i], sc[i]
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set(mCallStalled, 500, mCallStalled+10, 0)
	if p, k := m.callPickLocked(live, 4, 1000); k != 2 || p[0] != live[3] || p[1] != live[1] {
		t.Fatalf("picked %v/%v, want subs 3 and 1", p[0].idx, p[1].idx)
	}
	set(0, 0, 0, 0)
	m.callPairAt = 2
	if p, k := m.callPickLocked(live, 4, 1000); k != 2 || p[0] != live[2] || p[1] != live[3] {
		t.Fatalf("ties: picked %v/%v, want 2 and 3", p[0].idx, p[1].idx)
	}
	live[3].qBytes = mSubQueueLite // full
	set(mCallStalled, mCallStalled, mCallStalled, 0)
	if p, k := m.callPickLocked(live, 4, 1000); k != 2 || p[0] == live[3] || p[1] == live[3] {
		t.Fatal("picked a full sub")
	}
}

// HS2_MCONN_CALL_DIR=down: Kharej copies the download direction, Iran does
// not copy the upload one.
func TestMConnCallDownOnly(t *testing.T) {
	setCall(t, "1")
	mconnCallDown = true
	t.Cleanup(func() { mconnCallDown = false })
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat atomic.Int64
	callStream(iran, exit, 2500, stop, &wg, &lat)
	mWaitFor(t, "exit copies", mLiteAfter+8*time.Second, func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return exit.callOn
	})
	w1i, w1e := n.link(1).iran.wrote.Load(), n.link(1).exit.wrote.Load()
	time.Sleep(time.Second)
	iran.mu.Lock()
	on := iran.callOn
	iran.mu.Unlock()
	di, de := n.link(1).iran.wrote.Load()-w1i, n.link(1).exit.wrote.Load()-w1e
	if on || di > 60_000 || de < 150_000 {
		t.Fatalf("down only: iran copies=%v, second sub carried %d up / %d down in 1 s", on, di, de)
	}
	close(stop)
	wg.Wait()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// The avoid mark is stripped from the list and changes nothing with the key off.
func TestCallAvoidMark(t *testing.T) {
	m := &mConn{}
	m.callUp = true
	if got := m.callAvoidLocked([]any{1}); len(got) != 1 {
		t.Fatal("mark added with the key off")
	}
	mconnCallAvoid = true
	t.Cleanup(func() { mconnCallAvoid = false })
	got := m.callAvoidLocked([]any{1})
	if _, ok := got[len(got)-1].(callAvoidMark); !ok || len(got) != 2 {
		t.Fatal("no mark with the key on")
	}
}
