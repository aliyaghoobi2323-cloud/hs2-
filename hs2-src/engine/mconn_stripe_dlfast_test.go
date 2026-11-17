package engine

import (
	"testing"
	"time"
)

func dlFastTest(t *testing.T, on bool) {
	oldOn, oldR, oldM, oldK, oldA := stripeDlFast.Load(), stripeDlFastRate.Load(), stripeDlFastMax.Load(), stripeK.Load(), stripeDlActive.Load()
	t.Cleanup(func() {
		stripeDlFast.Store(oldOn)
		stripeDlFastRate.Store(oldR)
		stripeDlFastMax.Store(oldM)
		stripeK.Store(oldK)
		stripeDlActive.Store(oldA)
	})
	stripeDlFast.Store(on)
	stripeDlFastRate.Store(80e6 / 8)
	stripeDlFastMax.Store(1)
	stripeK.Store(8)
	stripeDlActive.Store(0)
}

// TestStripeDlFastSampler: two 200 ms samples in a row at the rate start a
// download's stripe; a burst, a slow one, an upload, an old connection, the
// key off or another fast download do not.
func TestStripeDlFastSampler(t *testing.T) {
	dlFastTest(t, false)
	t0 := time.Now()
	m := &mConn{}
	m.stK = 8
	m.rawBorn = t0
	m.rawW.Store(true)
	step := func(i int, s, r uint64) (bool, float64) {
		return m.stripeDlSampleLocked(t0.Add(time.Duration(i)*200*time.Millisecond), s, r)
	}
	const fast = 100e6 / 8 / 5 // 100 Mbit/s for 200 ms
	if g, _ := step(1, 0, 0); g {
		t.Fatal("off")
	}
	if g, _ := step(2, 0, fast); g || m.stripeDlNextLocked(t0, time.Second) != time.Second || stripeEarlyDl() != 0 {
		t.Fatal("key off")
	}
	stripeDlFast.Store(true)
	if m.stripeDlNextLocked(t0, time.Second) != 200*time.Millisecond || stripeEarlyFirst() != 200*time.Millisecond && stripeEarlyOn() == 0 {
		t.Fatal("sampler period")
	}
	m.stDlT = time.Time{}
	step(1, 0, 0)                    // baseline
	if g, _ := step(2, 0, fast); g { // one sample
		t.Fatal("one sample is not enough")
	}
	if g, _ := step(3, 0, fast+fast/10); g { // slow
		t.Fatal("slow sample")
	}
	if g, _ := step(4, 0, 2*fast); g {
		t.Fatal("the run restarts after a slow sample")
	}
	if g, r := step(5, 0, 3*fast+fast/10); !g || r < 80e6/8 {
		t.Fatalf("two hot samples: %v %v", g, r)
	}
	if g, _ := step(6, 10*fast, 3*fast+fast/10); g || m.stDlHot != 0 {
		t.Fatal("an upload sample")
	}
	step(7, 10*fast, 4*fast)
	if g, _ := step(16, 10*fast, 14*fast); g || m.stripeDlNextLocked(t0.Add(3200*time.Millisecond), time.Second) != time.Second {
		t.Fatal("past the window")
	}
	// solo: another fast download holds the rule
	m.stDlT, m.stDlHot = time.Time{}, 0
	o := &mConn{}
	o.stripeDlCountLocked(true)
	step(1, 0, 0)
	step(2, 0, fast)
	if g, _ := step(3, 0, 2*fast); g || m.stripeDlNextLocked(t0, time.Second) != time.Second {
		t.Fatal("solo: a second download went fast")
	}
	stripeDlFastMax.Store(2)
	step(4, 0, 3*fast)
	if g, _ := step(5, 0, 4*fast); !g {
		t.Fatal("max 2")
	}
	o.stripeDlCountLocked(false)
	if stripeDlActive.Load() != 0 {
		t.Fatal("count")
	}
	m.stN = 1
	if m.stripeDlOnLocked(t0) {
		t.Fatal("striped already")
	}
}

// TestStripeDlFastPlan: the hot-tick gate and the doubling chain.
func TestStripeDlFastPlan(t *testing.T) {
	dlFastTest(t, true)
	m := &mConn{}
	m.stK = 8
	m.stHot = 1
	if m.stripeDlHotOKLocked(false, 100e6/8) {
		t.Fatal("one hot tick")
	}
	m.stHot = 2
	if m.stripeDlHotOKLocked(true, 100e6/8) || m.stripeDlHotOKLocked(false, 50e6/8) || !m.stripeDlHotOKLocked(false, 100e6/8) {
		t.Fatal("hot tick rule")
	}
	if n := m.stripeDlPlanLocked(1, 100e6/8, true); n != 0 {
		t.Fatalf("upload: %d", n)
	}
	if n := m.stripeDlPlanLocked(1, 100e6/8, false); n != 1 || !m.stDlCnt || stripeDlActive.Load() != 1 {
		t.Fatalf("1 -> 3: more %d", n)
	}
	if n := m.stripeDlPlanLocked(3, 80e6/8, false); n != 0 {
		t.Fatalf("3 subs under 30 Mbit/s each: %d", n)
	}
	if n := m.stripeDlPlanLocked(3, 300e6/8, false); n != 2 {
		t.Fatalf("3 -> 6: %d", n)
	}
	if n := m.stripeDlPlanLocked(6, 600e6/8, false); n != 1 {
		t.Fatalf("6 -> 8 (K): %d", n)
	}
	if !m.stripeFastRetryOnLocked() {
		t.Fatal("fast retry for a fast download")
	}
	if !m.stripeChainHintLocked() || !m.stripeChainSoloLocked() {
		t.Fatal("chain hint / solo")
	}
	m.stripeEnded()
	if m.stDlCnt || stripeDlActive.Load() != 0 || m.stripeFastRetryOnLocked() {
		t.Fatal("ended: slot kept")
	}
	stripeDlFast.Store(false)
	if m.stripeDlHotOKLocked(false, 1e9) || m.stripeDlPlanLocked(1, 1e9, false) != 0 {
		t.Fatal("key off")
	}
}

// TestStripeDlFastTick: through stripeTickLocked (raw, 200 ms ticks), a
// download at 100 Mbit/s upgrades at the fourth tick (0.6 s) with the key, never in
// the first second without it.
func TestStripeDlFastTick(t *testing.T) {
	for _, on := range []bool{false, true} {
		dlFastTest(t, on)
		m := &mConn{}
		m.stK = 8
		m.mode = planeLight
		t0 := time.Now()
		m.rawBorn = t0
		m.rawW.Store(true)
		var recvd uint64
		at := -1
		for i := 0; i <= 5 && at < 0; i++ {
			if m.stripeTickLocked(t0.Add(time.Duration(i)*200*time.Millisecond), 0, recvd) {
				at = i
			}
			recvd += 100e6 / 8 / 5 // 100 Mbit/s
		}
		if (at >= 0) != on || on && at != 3 {
			t.Fatalf("key %v: grew at tick %d", on, at)
		}
		if on && (m.stFastMore != 1 || !m.stOpening || !m.stDlCnt) {
			t.Fatalf("chain %d opening %v", m.stFastMore, m.stOpening)
		}
		m.stripeEnded()
	}
}
