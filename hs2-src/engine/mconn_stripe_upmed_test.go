package engine

import (
	"os"
	"testing"
	"time"
)

// prod73: Iran's upload gate decides on the median of its last samples, so
// one noisy sample neither cuts nor grows; a held queue still cuts.
func TestStripeUpMed(t *testing.T) {
	old := stripeUpMed
	defer func() { stripeUpMed = old }()
	stripeUpMed = 3
	ms := time.Millisecond
	m := &mConn{}
	got := []time.Duration{}
	for _, q := range []time.Duration{1 * ms, 9 * ms, 1 * ms, 0, 8 * ms, 8 * ms, 8 * ms} {
		got = append(got, m.stripeUpMedLocked(q))
	}
	want := []time.Duration{1 * ms, 9 * ms, 1 * ms, 1 * ms, 1 * ms, 8 * ms, 8 * ms}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v want %v (all %v)", i, got[i], want[i], got)
		}
	}
	stripeUpMed = 1
	if q := m.stripeUpMedLocked(5 * ms); q != 5*ms {
		t.Fatalf("off: got %v", q)
	}
	if os.Getenv("HS2_MCONN_STRIPE_UP_BASE_MAX") == "" && stripeUpBaseMax != 2*ms {
		t.Fatalf("stripeUpBaseMax default %v, want 2ms (prod73: 10 ms opt-in)", stripeUpBaseMax)
	}
	if os.Getenv("HS2_MCONN_STRIPE_UP_MED") == "" && old != 1 {
		t.Fatalf("stripeUpMed default %d, want 1 (off)", old)
	}
}
