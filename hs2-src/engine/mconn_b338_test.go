package engine

import (
	"testing"
	"time"
)

// b338: a CALLMOVE whose old copy leaves before the fresh sub read (lab/rb337
// CY2: a rescue-cut drop gave it up 350 ms after the trigger) is abandoned
// cleanly: counted and logged, the move state cleared, the fresh sub stays a
// normal copy (never closed, no flags), no new move before the call's gap.
func TestB338CallMoveOldVanishes(t *testing.T) {
	q := map[string]cmPath{"A": {loss: 0.001, ok: true}, "B": {loss: 0.004, ok: true}, "C": {loss: 0.0012, ok: true}}
	withCallMove(t, map[any]string{20: "A", 21: "B", 22: "C"}, q)
	cmCanClose = func(*msub) bool { return true }
	for _, how := range []string{"closeHS", "dead", "gone"} {
		m := &mConn{iran: true, lite: true}
		m.callOn = true
		m.open = func(mPlane, []any) (mSub, any, error) { return nil, nil, errMConnClosed }
		a, b := &msub{key: 20, idx: 0}, &msub{key: 21, idx: 1}
		m.subs = []*msub{a, b}
		now := time.Now()
		m.callMoveLocked([]*msub{a, b}, now)
		m.callMoveLocked([]*msub{a, b}, now.Add(400*time.Millisecond))
		if m.cmKey != 21 || !b.callLate {
			t.Fatalf("%s: no move started", how)
		}
		time.Sleep(20 * time.Millisecond) // the failed open clears lightOpening
		m.mu.Lock()
		m.lightOpening, m.lightRetryAt = false, time.Time{}
		m.mu.Unlock()
		// the fresh sub opened (no data yet), then the old copy left
		fresh := &msub{key: 22, idx: 2}
		fresh.lastRecv.Store(monoNano(now.Add(500 * time.Millisecond)))
		m.subs = append(m.subs, fresh)
		switch how {
		case "closeHS":
			m.closeReqLocked(b) // what rescueCutDropLocked does
		case "dead":
			b.dead, b.retired = true, true
		case "gone":
			m.subs = []*msub{a, fresh}
		}
		ab0 := cmAbandoned.Load()
		live := []*msub{a, fresh}
		m.callMoveLocked(live, now.Add(700*time.Millisecond))
		if m.cmKey != nil || len(m.cmLive) != 0 || m.cmTo != "" || cmAbandoned.Load() != ab0+1 {
			t.Fatalf("%s: move not abandoned: key %v live %v abandoned %d", how, m.cmKey, m.cmLive, cmAbandoned.Load()-ab0)
		}
		if b.callLate && how != "gone" {
			t.Fatalf("%s: old sub keeps callLate", how)
		}
		// the fresh sub is a normal copy: reads data, nothing closes it, no
		// new move within the call's gap (both copies now on A and C)
		fresh.rxEnd = 100
		fresh.rxAny.Store(true)
		mv0 := cmMoves.Load()
		for d := time.Second; d < cmGap; d += 50 * time.Millisecond {
			m.callMoveLocked(live, now.Add(d))
		}
		if fresh.retired || fresh.closeHS || fresh.callLate || a.retired || a.callLate || cmMoves.Load() != mv0 || m.cmKey != nil {
			t.Fatalf("%s: churn after the abandon: fresh retired %v closeHS %v moves %d", how, fresh.retired, fresh.closeHS, cmMoves.Load()-mv0)
		}
	}
}

// b338 damper: no move when every path is about as lossy (worst < 2 x best)
// or the best eligible path is itself >= 1 %; a move still happens on a
// clear relative gap.
func TestB338CallMoveDamper(t *testing.T) {
	q := map[string]cmPath{"A": {loss: 0.030, ok: true}, "B": {loss: 0.036, ok: true}, "C": {loss: 0.029, ok: true}}
	withCallMove(t, map[any]string{30: "A", 31: "B", 32: "C"}, q)
	cmCanClose = func(*msub) bool { return true }
	run := func() (bool, int64) {
		m := &mConn{iran: true, lite: true}
		m.callOn = true
		m.open = func(mPlane, []any) (mSub, any, error) { return nil, nil, errMConnClosed }
		live := []*msub{{key: 30, idx: 0}, {key: 31, idx: 1}}
		m.subs = live
		d0 := cmDamped.Load()
		now := time.Now()
		for d := time.Duration(0); d < 2*time.Second; d += 100 * time.Millisecond {
			m.callMoveLocked(live, now.Add(d))
		}
		time.Sleep(20 * time.Millisecond)
		return m.cmKey != nil || !m.cmAt.IsZero(), cmDamped.Load() - d0
	}
	// all ~3 %: abs gap 0.7 % but 3.6 < 2 x 2.9
	if moved, _ := run(); moved {
		t.Fatal("moved with all paths equally lossy")
	}
	// best >= 1 %: 4 % vs 1.2 %
	q["B"], q["C"] = cmPath{loss: 0.04, ok: true}, cmPath{loss: 0.012, ok: true}
	cmQ.Store(&cmSnap{at: time.Now(), p: q})
	if moved, _ := run(); moved {
		t.Fatal("moved onto a >= 1 % path")
	}
	// clear relative gap, good target: moves
	q["C"] = cmPath{loss: 0.004, ok: true}
	cmQ.Store(&cmSnap{at: time.Now(), p: q})
	if moved, _ := run(); !moved {
		t.Fatal("no move on a clear gap")
	}
}

// b338 backoff: cmGap per call, doubled for each further move in cmWindow,
// at most cmWindow; back to cmGap once the window is clear.
func TestB338CallMoveBackoff(t *testing.T) {
	oG, oW := cmGap, cmWindow
	cmGap, cmWindow = 30*time.Second, 5*time.Minute
	defer func() { cmGap, cmWindow = oG, oW }()
	m := &mConn{}
	t0 := time.Now()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute}
	at := t0
	for i, w := range want {
		if g := m.cmGapLocked(at); g != w {
			t.Fatalf("move %d: gap %v, want %v", i+1, g, w)
		}
		at = at.Add(10 * time.Second) // all within cmWindow
	}
	// the all-path-loss case (rb337): moves every 30 s would be 30,60,120 s apart
	m2 := &mConn{}
	if g := m2.cmGapLocked(t0); g != cmGap {
		t.Fatal(g)
	}
	if g := m2.cmGapLocked(t0.Add(30 * time.Second)); g != 2*cmGap {
		t.Fatal(g)
	}
	// window clear: back to cmGap
	if g := m2.cmGapLocked(t0.Add(30*time.Second + cmWindow)); g != cmGap {
		t.Fatalf("after a clear window: %v", g)
	}
}

// hsTestSub: a sub whose exit reads CLOSE-REQ.
type hsTestSub struct{ mSub }

func (hsTestSub) mconnCloseHS() bool { return true }

// b338 (lab/rb337 CY2): a call with one good copy while the links left sit
// on cut paths: each replacement lands on a cut link and was given up at
// once (~145 subs/s for 13 s). With the per-connection backoff the drops
// over 13 s stay a handful; a connection quiet for rcReset drops at once.
func TestB338RescueCutNoChurn(t *testing.T) {
	oCut, oRes, oHS, oPer := lbRescueCut, lbRescue.Load(), mconnCloseHS.Load(), rescueCutPerSec
	lbRescueCut, rescueCutPerSec = true, 1<<30
	lbRescue.Store(true)
	mconnCloseHS.Store(true)
	type cutKey struct{ n int }
	var marked []any
	defer func() {
		lbRescueCut, rescueCutPerSec = oCut, oPer
		lbRescue.Store(oRes)
		mconnCloseHS.Store(oHS)
		for _, k := range marked {
			rescueCutLinks.Delete(k)
		}
	}()
	m := &mConn{iran: true, lite: true}
	m.mode = planeLight
	good := &msub{key: cutKey{-1}, idx: 0, rw: hsTestSub{}}
	good.lastRecv.Store(monoNano(time.Now()))
	idx := 1
	newBad := func() *msub {
		k := cutKey{idx}
		rescueCutLinks.Store(k, true)
		marked = append(marked, k)
		s := &msub{key: k, idx: idx, rw: hsTestSub{}}
		idx++
		return s
	}
	bad := newBad()
	m.subs = []*msub{good, bad}
	t0 := time.Now()
	drops, damped0 := 0, rescueCutDamped.Load()
	for d := time.Duration(0); d < 13*time.Second; d += 7 * time.Millisecond {
		if bad.retired { // the upgrade's replacement lands on a cut link again
			bad = newBad()
			m.subs = []*msub{good, bad}
		}
		if m.rescueCutDropLocked([]*msub{good, bad}, t0.Add(d)) {
			drops++
		}
	}
	// gaps 0.5, 1, 2, 4, 8 s: drops at 0, 0.5, 1.5, 3.5, 7.5 s
	if drops < 2 || drops > 6 {
		t.Fatalf("drops in 13 s: %d (want a handful, not ~1800)", drops)
	}
	if rescueCutDamped.Load() == damped0 {
		t.Fatal("damper not counted")
	}
	if m.primary.Load() != good {
		t.Fatal("good sub not primary")
	}
	// quiet for rcReset: the next drop is taken at once and the gap restarts
	if bad.retired {
		bad = newBad()
		m.subs = []*msub{good, bad}
	}
	late := t0.Add(13*time.Second + rcReset + rcGapMax)
	if !m.rescueCutDropLocked([]*msub{good, bad}, late) {
		t.Fatal("drop after a quiet rcReset refused")
	}
	if m.rcN != 0 || m.rcNext.Sub(late) != rcGap0 {
		t.Fatalf("backoff not reset: n %d next +%v", m.rcN, m.rcNext.Sub(late))
	}
}
