package engine

import (
	"testing"
	"time"
)

// پنجرهٔ کوتاه کمینهٔ srtt: افزایش پایدار پایهٔ مسیر پس از یک پنجره دیگر صف شمرده نمی‌شود.
func TestStripeQWinMin(t *testing.T) {
	old := stripeQWin
	stripeQWin = 4 * time.Second
	defer func() { stripeQWin = old }()
	var w qwinMin
	t0 := time.Unix(1000, 0)
	if got := w.note(t0, 80000); got != 80000 {
		t.Fatalf("first: %d", got)
	}
	// مسیر ۸ms بلندتر شد
	for i := 1; i <= 50; i++ {
		got := w.note(t0.Add(time.Duration(i)*100*time.Millisecond), 88000)
		if i < 20 && got != 80000 {
			t.Fatalf("i=%d: early forget %d", i, got)
		}
		if i == 50 && got != 88000 {
			t.Fatalf("not relearned: %d", got)
		}
	}
}

func TestStripeDiagGateSnapshot(t *testing.T) {
	StripeObserved() // صفر کردن بازه
	stripeDiagGate(gwCut, 0, 2, 9*time.Millisecond, 5*time.Millisecond, 4*time.Millisecond, 3)
	stripeDiagGate(gwGrow, 2, 2, time.Millisecond, 0, 4*time.Millisecond, 3)
	s := StripeObserved()
	if s.Decisions != 2 || s.WMax != 2 || s.WAvg != 1 || s.LiveAvg != 2 || s.QRawMax != 9000 || s.QRawMinUs != 1000 {
		t.Fatalf("snap %+v", s)
	}
	if s2 := StripeObserved(); s2.Decisions != 0 || s2.WMax != 0 || s2.QRawMax != 0 {
		t.Fatalf("interval not reset %+v", s2)
	}
}
