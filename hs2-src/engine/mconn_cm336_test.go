package engine

import (
	"encoding/binary"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withCallMove: the key on, short timings, link paths and a fixed snapshot.
func withCallMove(t *testing.T, paths map[any]string, q map[string]cmPath) {
	t.Helper()
	old := b336CallMove.Load()
	oS, oG, oU, oF, oC, oP := cmSustain, cmGap, cmGiveUp, cmFresh, cmCanClose, cmPerMin
	b336CallMove.Store(true)
	cmSustain, cmGap, cmGiveUp, cmFresh, cmPerMin = 300*time.Millisecond, 3*time.Second, 3*time.Second, time.Hour, 1000
	for k, p := range paths {
		callLinkPath.Store(k, p)
	}
	cmQ.Store(&cmSnap{at: time.Now(), p: q})
	t.Cleanup(func() {
		waitConnLoops(t.Logf, mCloseLinger+mRSTWait+2*time.Second)
		b336CallMove.Store(old)
		cmSustain, cmGap, cmGiveUp, cmFresh, cmCanClose, cmPerMin = oS, oG, oU, oF, oC, oP
		cmQ.Store(nil)
		callLinkPath.Range(func(k, _ any) bool { callLinkPath.Delete(k); return true })
	})
}

// Unit: hysteresis (delta, sustain), the target off the other copies'
// paths, per-call and global rate limits, rescue guard, give-up.
func TestB336CallMoveLocked(t *testing.T) {
	q := map[string]cmPath{"A": {loss: 0.001, ok: true}, "B": {loss: 0.004, ok: true}, "C": {loss: 0.0012, ok: true}, "D": {loss: 0.0005, ok: false}}
	withCallMove(t, map[any]string{10: "A", 11: "B", 12: "C", 13: "A2"}, q)
	cmCanClose = func(*msub) bool { return true }
	newM := func(keys ...any) (*mConn, []*msub) {
		m := &mConn{iran: true, lite: true}
		m.callOn = true
		m.open = func(mPlane, []any) (mSub, any, error) { return nil, nil, errMConnClosed }
		var live []*msub
		for i, k := range keys {
			live = append(live, &msub{key: k, idx: i})
		}
		return m, live
	}
	reopen := func(m *mConn) {
		time.Sleep(20 * time.Millisecond) // the failed open clears lightOpening
		m.mu.Lock()
		m.lightOpening, m.lightRetryAt = false, time.Time{}
		m.mu.Unlock()
	}
	now := time.Now()
	o0 := cmMoves.Load()
	m, live := newM(10, 11) // A 0.1 %, B 0.4 %: B -> C (0.12 %, D not eligible)
	m.callMoveLocked(live, now)
	m.callMoveLocked(live, now.Add(200*time.Millisecond))
	if cmMoves.Load() != o0 {
		t.Fatal("move before cmSustain")
	}
	m.callMoveLocked(live, now.Add(400*time.Millisecond))
	if cmMoves.Load() != o0+1 || m.cmKey != 11 || m.cmTo != "C" || !live[1].callLate {
		t.Fatalf("no move B->C: %d key %v to %q", cmMoves.Load()-o0, m.cmKey, m.cmTo)
	}
	// the fresh sub never reads: given up after cmGiveUp, the old keeps copies
	g0 := cmGaveUp.Load()
	m.callMoveLocked(live, now.Add(400*time.Millisecond+cmGiveUp))
	if m.cmKey != nil || cmGaveUp.Load() != g0+1 || live[1].callLate {
		t.Fatal("pending move not given up")
	}
	// per-call gap: nothing more within cmGap
	reopen(m)
	for d := time.Second; d < cmGap; d += 100 * time.Millisecond {
		m.callMoveLocked(live, now.Add(d))
	}
	if cmMoves.Load() != o0+1 {
		t.Fatal("second move within cmGap")
	}
	// fresh sub read: the old one is closed (handshake hook) — needs a real sub
	m2, live2 := newM(10, 11)
	m2.callMoveLocked(live2, now)
	m2.callMoveLocked(live2, now.Add(time.Second))
	if m2.cmKey != 11 {
		t.Fatal("no move on m2")
	}
	fresh := &msub{key: 12, idx: 2}
	fresh.lastRecv.Store(monoNano(now.Add(1500 * time.Millisecond)))
	live2[0].lastRecv.Store(monoNano(now.Add(1500 * time.Millisecond)))
	if m2.cmKey = 11; true {
		// an old sub reading is no fresh sub
		m2.callMoveLocked(live2, now.Add(1600*time.Millisecond))
		if m2.cmKey != 11 {
			t.Fatal("released by a sub live at the trigger")
		}
	}
	// opened (lastRecv set) but no data read yet: not fresh
	m2.callMoveLocked(append(live2, fresh), now.Add(1650*time.Millisecond))
	if m2.cmKey != 11 {
		t.Fatal("released by a fresh sub that read no data")
	}
	fresh.rxEnd = 100
	fresh.rxAny.Store(true)
	cmCanClose = func(*msub) bool { return false } // no handshake: kept, pending ends
	m2.callMoveLocked(append(live2, fresh), now.Add(1700*time.Millisecond))
	if m2.cmKey != nil || live2[1].callLate || live2[1].retired {
		t.Fatal("old sub closed without a handshake")
	}
	cmCanClose = func(*msub) bool { return true }

	// similar paths (delta < 0.15 %): nothing
	q["B"] = cmPath{loss: 0.0024, ok: true}
	cmQ.Store(&cmSnap{at: now, p: q})
	m3, live3 := newM(10, 11)
	for d := time.Duration(0); d < 3*time.Second; d += 100 * time.Millisecond {
		m3.callMoveLocked(live3, now.Add(d))
	}
	// broken evidence restarts the clock
	q["B"] = cmPath{loss: 0.004, ok: true}
	m4, live4 := newM(10, 11)
	m4.callMoveLocked(live4, now)
	m4.callMoveLocked(live4, now.Add(200*time.Millisecond))
	q["B"] = cmPath{loss: 0.002, ok: true}
	m4.callMoveLocked(live4, now.Add(250*time.Millisecond))
	q["B"] = cmPath{loss: 0.004, ok: true}
	m4.callMoveLocked(live4, now.Add(300*time.Millisecond))
	m4.callMoveLocked(live4, now.Add(500*time.Millisecond))
	// rescue running: nothing
	m5, live5 := newM(10, 11)
	m5.stuckKeys = []any{10}
	for d := time.Duration(0); d < 2*time.Second; d += 100 * time.Millisecond {
		m5.callMoveLocked(live5, now.Add(d))
	}
	m6, live6 := newM(10, 11)
	m6.dcSwAt = now
	for d := time.Duration(0); d < 2*time.Second; d += 100 * time.Millisecond {
		m6.callMoveLocked(live6, now.Add(d))
	}
	// not a call / key off: nothing
	m7, live7 := newM(10, 11)
	m7.callOn = false
	for d := time.Duration(0); d < 2*time.Second; d += 100 * time.Millisecond {
		m7.callMoveLocked(live7, now.Add(d))
	}
	if cmMoves.Load() != o0+2 {
		t.Fatalf("false moves: %d", cmMoves.Load()-o0-2)
	}
	m4.callMoveLocked(live4, now.Add(700*time.Millisecond))
	if cmMoves.Load() != o0+3 {
		t.Fatal("no move after the restarted sustain")
	}
	// global cap
	cmPerMin = 0
	r0 := cmRefused.Load()
	m8, live8 := newM(10, 11)
	for d := time.Duration(0); d < 2*time.Second; d += 100 * time.Millisecond {
		m8.callMoveLocked(live8, now.Add(d))
	}
	if cmMoves.Load() != o0+3 || cmRefused.Load() == r0 {
		t.Fatal("global cap not applied")
	}
}

// End to end: a call on paths A (0.1 %) and B (0.5 %) moves its B copy to C
// (0.05 %): the fresh sub opens first with the move mark, the B sub is given
// up through the close handshake, the call never has fewer than 2 subs, and
// every record arrives once, in order, both ways.
func TestB336CallMoveNoLossNoReorder(t *testing.T) {
	setCloseHS(t, true)
	setCall(t, "1")
	withCallMove(t, map[any]string{0: "A", 1: "B", 2: "C", 3: "C"},
		map[string]cmPath{"A": {loss: 0.001, ok: true}, "B": {loss: 0.005, ok: true}, "C": {loss: 0.0005, ok: true}})
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	res := seqStream(iran, exit, 2500, stop, &wg)
	mWaitFor(t, "call on two subs", mLiteAfter+4*time.Second, func() bool {
		iran.mu.Lock()
		on := iran.callOn
		iran.mu.Unlock()
		return on && liveSubs(iran) == 2 && liveSubs(exit) == 2
	})
	var low atomic.Bool
	watch := make(chan struct{})
	go func() {
		defer close(watch)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if liveSubs(iran) < 2 {
				low.Store(true)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	d0, m0 := cmDone.Load(), cmMoves.Load()
	mWaitFor(t, "move done", 8*time.Second, func() bool { return cmDone.Load() > d0 })
	mWaitFor(t, "old B sub gone", 5*time.Second, func() bool { return !subOnKeys(iran, 1) })
	l := n.link(2)
	if l == nil || l.plane != planeLight {
		t.Fatal("no fresh light sub")
	}
	marked := false
	for _, a := range l.avoid {
		if c, ok := a.(callMoveMark); ok && c.path == "C" {
			marked = true
		}
	}
	if !marked || !containsKey(l.avoid, 0) || !containsKey(l.avoid, 1) {
		t.Fatalf("fresh sub avoid %v: want the move mark for C and the old links", l.avoid)
	}
	time.Sleep(700 * time.Millisecond)
	close(stop)
	wg.Wait()
	<-watch
	for i, r := range res {
		mWaitFor(t, "all records read", 3*time.Second, func() bool { return r.got.Load() == r.sent.Load() })
		if r.bad.Load() != 0 {
			t.Fatalf("dir %d: %d records out of order or lost", i, r.bad.Load())
		}
		t.Logf("dir %d: %d records, max gap %v", i, r.got.Load(), time.Duration(r.maxGap.Load()))
	}
	if low.Load() {
		t.Fatal("the call fell under two subs during the move")
	}
	if cmMoves.Load() != m0+1 {
		t.Fatalf("%d moves, want 1 (the per-call gap)", cmMoves.Load()-m0)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

type seqRes struct {
	sent, got, bad, maxGap atomic.Int64
}

// seqStream: size-byte records every 10 ms each way, numbered; the reader
// counts records not equal to the next number (loss or reorder).
func seqStream(iran, exit *mConn, size int, stop chan struct{}, wg *sync.WaitGroup) [2]*seqRes {
	res := [2]*seqRes{{}, {}}
	one := func(from, to *mConn, r *seqRes) {
		defer wg.Done()
		go func() {
			b := make([]byte, size)
			var next uint64
			last := time.Now()
			for {
				if _, err := io.ReadFull(to, b); err != nil {
					return
				}
				if binary.BigEndian.Uint64(b) != next {
					r.bad.Add(1)
				}
				next = binary.BigEndian.Uint64(b) + 1
				if g := time.Since(last); int64(g) > r.maxGap.Load() {
					r.maxGap.Store(int64(g))
				}
				last = time.Now()
				r.got.Add(1)
			}
		}()
		p := make([]byte, size)
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for i := uint64(0); ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			binary.BigEndian.PutUint64(p, i)
			if _, err := from.Write(p); err != nil {
				return
			}
			r.sent.Add(1)
		}
	}
	wg.Add(2)
	go one(iran, exit, res[0])
	go one(exit, iran, res[1])
	return res
}
