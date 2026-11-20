package engine

import (
	"testing"
	"time"
)

func setCMSpread(t *testing.T, on bool) {
	t.Helper()
	o := b405CMSpread.Load()
	b405CMSpread.Store(on)
	cmSpread.mu.Lock()
	cmSpread.at = nil
	cmSpread.mu.Unlock()
	t.Cleanup(func() {
		b405CMSpread.Store(o)
		cmSpread.mu.Lock()
		cmSpread.at = nil
		cmSpread.mu.Unlock()
	})
}

func TestB405Default(t *testing.T) {
	if envDefault("HS2_B405_CMSPREAD") == "" && b405CMSpread.Load() {
		t.Fatal("HS2_B405_CMSPREAD must default off")
	}
}

// sk402 el 397: calls crossing cmSustain in the same tick all moved to the
// single best path. With the key on, at most cmSpreadMax moves per target
// path per cmSpreadWin, among the near-best paths; with it off, the old herd.
func TestB405CallMoveSpread(t *testing.T) {
	q := map[string]cmPath{
		"W": {loss: 0.006, ok: true},  // the copies' bad path
		"A": {loss: 0.0004, ok: true}, // the other copy's path (used)
		"B": {loss: 0.0005, ok: true},
		"C": {loss: 0.0007, ok: true},
		"D": {loss: 0.0009, ok: true},
		"E": {loss: 0.0030, ok: true}, // outside the margin
	}
	withCallMove(t, map[any]string{10: "W", 11: "A"}, q)
	cmCanClose = func(*msub) bool { return true }
	run := func(n int, now time.Time) map[string]int {
		got := map[string]int{}
		for i := 0; i < n; i++ {
			m := &mConn{iran: true, lite: true}
			m.callOn = true
			m.open = func(mPlane, []any) (mSub, any, error) { return nil, nil, errMConnClosed }
			live := []*msub{{key: 10, idx: 0}, {key: 11, idx: 1}}
			m.callMoveLocked(live, now)
			m.callMoveLocked(live, now.Add(400*time.Millisecond))
			if m.cmKey != nil {
				got[m.cmTo]++
			}
		}
		return got
	}
	now := time.Now()
	setCMSpread(t, false)
	if got := run(5, now); got["B"] != 5 {
		t.Fatalf("key off: %v, want all 5 on B (unchanged)", got)
	}
	setCMSpread(t, true)
	h0 := cmSpreadHeld.Load()
	got := run(8, now)
	if got["B"] != 2 || got["C"] != 2 || got["D"] != 2 || got["E"] != 0 || cmSpreadHeld.Load()-h0 != 2 {
		t.Fatalf("key on: %v held %d, want B/C/D 2 each, E 0, 2 held", got, cmSpreadHeld.Load()-h0)
	}
	// after cmSpreadWin the targets take moves again, best first
	if got := run(1, now.Add(cmSpreadWin+time.Second)); got["B"] != 1 {
		t.Fatalf("after the window: %v, want B", got)
	}
}

// cmAllow refusing a move gives its spread slot back.
func TestB405SpreadUndo(t *testing.T) {
	setCMSpread(t, true)
	snap := &cmSnap{p: map[string]cmPath{"B": {loss: 0.0005, ok: true}}}
	now := time.Now()
	for i := 0; i < 2; i++ {
		if _, _, ok := cmSpreadTake(snap, nil, 0.006, 0.0005, now.Add(time.Duration(i)*time.Millisecond)); !ok {
			t.Fatal("take refused")
		}
	}
	if _, _, ok := cmSpreadTake(snap, nil, 0.006, 0.0005, now.Add(5*time.Millisecond)); ok {
		t.Fatal("third take within the window")
	}
	cmSpreadUndo("B", now.Add(time.Millisecond))
	if _, _, ok := cmSpreadTake(snap, nil, 0.006, 0.0005, now.Add(6*time.Millisecond)); !ok {
		t.Fatal("slot not given back")
	}
}
