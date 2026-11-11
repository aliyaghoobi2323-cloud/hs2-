package engine

import (
	"testing"
	"time"
)

func TestStripeEarlyEnv(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "0": 0, "x": 0, "-5": 0, "10": 50 * time.Millisecond, "200": 200 * time.Millisecond, "9000": 500 * time.Millisecond} {
		if got := stripeEarlyFromEnv(v); got != want {
			t.Errorf("%q: %v want %v", v, got, want)
		}
	}
}

// TestStripeEarlyTicks: early ticks only in the first second of a raw
// connection with FAST and the key on; then the usual period.
func TestStripeEarlyTicks(t *testing.T) {
	oldF, oldE, oldK := stripeFast.Load(), stripeFastEarly.Load(), stripeK.Load()
	defer func() { stripeFast.Store(oldF); stripeFastEarly.Store(oldE); stripeK.Store(oldK) }()
	stripeK.Store(8)
	t0 := time.Now()
	m := &mConn{}
	m.stK = 8
	m.rawBorn = t0

	stripeFast.Store(true)
	stripeFastEarly.Store(0)
	if e := m.stripeEarlyLocked(t0.Add(200 * time.Millisecond)); e != 0 || stripeEarlyFirst() != mLitePoll {
		t.Fatalf("key off: early %v", e)
	}
	stripeFastEarly.Store(int64(200 * time.Millisecond))
	stripeFast.Store(false)
	if e := m.stripeEarlyLocked(t0.Add(200 * time.Millisecond)); e != 0 {
		t.Fatalf("FAST off: early %v", e)
	}
	stripeFast.Store(true)
	if stripeEarlyFirst() != 200*time.Millisecond {
		t.Fatal("first tick")
	}
	e := m.stripeEarlyLocked(t0.Add(200 * time.Millisecond))
	if e != 200*time.Millisecond {
		t.Fatalf("early %v", e)
	}
	if n := m.stripeEarlyNextLocked(t0.Add(200*time.Millisecond), e); n != 200*time.Millisecond {
		t.Fatalf("next %v", n)
	}
	if n := m.stripeEarlyNextLocked(t0.Add(850*time.Millisecond), e); n != 150*time.Millisecond {
		t.Fatalf("next to the second %v", n)
	}
	if n := m.stripeEarlyNextLocked(t0.Add(950*time.Millisecond), e); n != mLitePoll {
		t.Fatalf("after %v", n)
	}
	if e := m.stripeEarlyLocked(t0.Add(1200 * time.Millisecond)); e != 0 {
		t.Fatalf("past the first second: %v", e)
	}
	m.stN = 1
	if e := m.stripeEarlyLocked(t0.Add(200 * time.Millisecond)); e != 0 {
		t.Fatalf("striped already: %v", e)
	}
	if stripeEarlyHotLocked(false, 1e9) || stripeEarlyHotLocked(true, 1e6/8) || !stripeEarlyHotLocked(true, 40e6/8) {
		t.Fatal("hot rule")
	}
}

func TestStripeUpNoShareHint(t *testing.T) {
	old := stripeUpNoShareOn.Load()
	defer stripeUpNoShareOn.Store(old)
	m := &mConn{}
	m.stGrowUp = true
	stripeUpNoShareOn.Store(false)
	if stripeUpNoShare(m.stripeNoShareHintLocked(nil)) {
		t.Fatal("key off")
	}
	stripeUpNoShareOn.Store(true)
	if !stripeUpNoShare(m.stripeNoShareHintLocked(nil)) {
		t.Fatal("upload: no share")
	}
	m.stGrowUp = false
	if stripeUpNoShare(m.stripeNoShareHintLocked(nil)) {
		t.Fatal("download keeps the shared pick")
	}
}
