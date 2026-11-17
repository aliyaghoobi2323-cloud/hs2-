package engine

import "testing"

func TestCutNoNewDraw(t *testing.T) {
	m, _ := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
	st := newSteerState()
	st.active = true
	st.repShare = map[string]float64{"a>x": 45, "b>x": 45, "c>x": 10}
	st.repFactor = map[string]float64{"a>x": 1, "b>x": 1, "c>x": 0.1}
	st.repCut = map[string]bool{"c>x": true}
	st.repLoss = map[string]float64{"a>x": 0.002, "b>x": 0.002, "c>x": 1}
	m.steer = st
	draw := func() map[string]int {
		cnt := map[string]int{}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.steerGen.Add(1) // the steer state changed outside steerTickLocked: drop the HS2_LM_FASTPICK draw cache
		for i := 0; i < 20000; i++ {
			cnt[m.steerDrawLocked(false, func(*managedLink) bool { return true })]++
		}
		return cnt
	}
	o := lbCutNoNew
	t.Cleanup(func() { lbCutNoNew = o })
	lbCutNoNew = false
	off := draw()
	lbCutNoNew = true
	on := draw()
	t.Logf("cut path drawn: off %d, on %d of 20000", off["c>x"], on["c>x"])
	if off["c>x"] == 0 || on["c>x"] != 0 || on["a>x"] == 0 || on["b>x"] == 0 {
		t.Fatalf("draws off %v on %v", off, on)
	}
	// Every path cut: nothing drawn (no steering), as before.
	st.repCut = map[string]bool{"a>x": true, "b>x": true, "c>x": true}
	if d := draw(); d[""] != 20000 {
		t.Fatalf("all cut: want no draw, got %v", d)
	}
}

// TestCutNoNewReadmit: the cut path is drawn again as soon as the dialing
// side's report has it uncut (CUT_NONEW never blocks a recovered path).
func TestCutNoNewReadmit(t *testing.T) {
	m, _ := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
	st := newSteerState()
	st.active = true
	st.repShare = map[string]float64{"a>x": 40, "b>x": 40, "c>x": 20}
	st.repFactor = map[string]float64{"a>x": 1, "b>x": 1, "c>x": 1}
	st.repCut = map[string]bool{"c>x": true}
	st.repLoss = map[string]float64{"a>x": 0.002, "b>x": 0.002, "c>x": 1}
	m.steer = st
	o := lbCutNoNew
	t.Cleanup(func() { lbCutNoNew = o })
	lbCutNoNew = true
	draw := func() int {
		n := 0
		m.mu.Lock()
		defer m.mu.Unlock()
		m.steerGen.Add(1) // the steer state changed outside steerTickLocked: drop the HS2_LM_FASTPICK draw cache
		for i := 0; i < 20000; i++ {
			if m.steerDrawLocked(false, func(*managedLink) bool { return true }) == "c>x" {
				n++
			}
		}
		return n
	}
	if n := draw(); n != 0 {
		t.Fatalf("cut: drawn %d", n)
	}
	st.repCut = map[string]bool{}
	st.repLoss["c>x"] = 0.002
	if n := draw(); n < 2000 {
		t.Fatalf("uncut: drawn only %d of 20000 (share 20 %%)", n)
	}
}

// TestCutNoNewBeforePressure: with the key on, a link of a cut path sorts
// after a pressed link of an uncut path (fl107: the cut path's probe links
// were the only unpressed ones under a crowd's churn); key off: unchanged.
func TestCutNoNewBeforePressure(t *testing.T) {
	cut := pickKey{cutNew: true, path: 2}
	pressed := pickKey{pressed: true, path: 1, load: 50}
	if !pressed.less(cut) || cut.less(pressed) {
		t.Fatal("cut path link not after a pressed uncut one")
	}
	off := pickKey{path: 2}
	if !off.less(pressed) {
		t.Fatal("key off: an unpressed link must still win")
	}
}
