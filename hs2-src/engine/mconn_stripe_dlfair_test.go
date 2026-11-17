package engine

import (
	"testing"
	"time"
)

// TestStripeDlFairLease: with HS2_MCONN_STRIPE_DL_FAIR a download holds the
// fast slot only for stripeDlLease (not while a chain or an open is pending),
// a striped download's later growth never takes it, and a new download gets
// it once the lease is over. Off: the old solo hold (for life).
func TestStripeDlFairLease(t *testing.T) {
	for _, fair := range []bool{false, true} {
		dlFastTest(t, true)
		old := stripeDlFair.Load()
		t.Cleanup(func() { stripeDlFair.Store(old) })
		stripeDlFair.Store(fair)
		const rate = 200e6 / 8
		bg := &mConn{}
		bg.stK = 8
		if more := bg.stripeDlPlanLocked(1, rate, false); !bg.stDlCnt || more != 1 {
			t.Fatalf("fair=%v: first growth takes the slot: cnt=%v more=%d", fair, bg.stDlCnt, more)
		}
		test := &mConn{}
		test.stK = 8
		if test.stripeDlSoloLocked() {
			t.Fatalf("fair=%v: slot busy", fair)
		}
		now := time.Now()
		bg.stFastMore = 1 // chain pending: kept past the lease
		bg.stripeDlLeaseLocked(now.Add(10 * time.Second))
		if !bg.stDlCnt {
			t.Fatalf("fair=%v: released with a chain pending", fair)
		}
		bg.stFastMore = 0
		bg.stripeDlLeaseLocked(now.Add(time.Second))
		if !bg.stDlCnt {
			t.Fatalf("fair=%v: released inside the lease", fair)
		}
		bg.stripeDlLeaseLocked(now.Add(stripeDlLease + time.Second))
		if bg.stDlCnt == fair {
			t.Fatalf("fair=%v: after the lease cnt=%v", fair, bg.stDlCnt)
		}
		if test.stripeDlSoloLocked() != fair {
			t.Fatalf("fair=%v: a new download's slot", fair)
		}
		if !fair {
			bg.stripeEnded()
			continue
		}
		// the striped background download grows 3 -> 4: no slot taken
		if more := bg.stripeDlPlanLocked(3, rate, false); more != 0 || bg.stDlCnt {
			t.Fatalf("later growth took the slot: more=%d cnt=%v", more, bg.stDlCnt)
		}
		if more := test.stripeDlPlanLocked(1, rate, false); !test.stDlCnt || more != 1 {
			t.Fatalf("new download: cnt=%v more=%d", test.stDlCnt, more)
		}
		// while the new one holds it, its own n>1 growth inside the lease keeps the chain rule
		if more := test.stripeDlPlanLocked(2, rate, false); more != 1 {
			t.Fatalf("held slot, growth 2: more=%d", more)
		}
		test.stripeEnded()
		if stripeDlActive.Load() != 0 {
			t.Fatalf("active %d", stripeDlActive.Load())
		}
	}
}

// TestStripeDlFairExtra: past the stripe slot cap only a fast download takes
// one of the extra slots, and only with the key.
func TestStripeDlFairExtra(t *testing.T) {
	dlFastTest(t, true)
	old := stripeDlFair.Load()
	t.Cleanup(func() { stripeDlFair.Store(old) })
	m := &mConn{}
	const fast, slow = 200e6 / 8, 40e6 / 8
	stripeDlFair.Store(false)
	if m.stripeDlFairExtraLocked(stripeMaxConns+1, false, fast) {
		t.Fatal("key off")
	}
	stripeDlFair.Store(true)
	if !m.stripeDlFairExtraLocked(stripeMaxConns+1, false, fast) || !m.stripeDlFairExtraLocked(stripeMaxConns+stripeDlFairExtra, false, fast) {
		t.Fatal("fast download past the cap")
	}
	if m.stripeDlFairExtraLocked(stripeMaxConns+stripeDlFairExtra+1, false, fast) {
		t.Fatal("past the extra slots")
	}
	if m.stripeDlFairExtraLocked(stripeMaxConns+1, false, slow) || m.stripeDlFairExtraLocked(stripeMaxConns+1, true, fast) {
		t.Fatal("slow stream or upload")
	}
}
