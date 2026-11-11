package engine

import (
	"errors"
	"testing"
	"time"
)

// Tests of HS2_MCONN_STRIPE_RESEND (mconn_stripe_resend.go).

// stripeLinkDeath: a striped download (exit -> Iran) whose stripe sub's link
// first swallows what it is given (the link's buffers, lost at its reset)
// and then dies. Returns the download's outcome.
func stripeLinkDeath(t *testing.T, resend bool) error {
	t.Helper()
	old := stripeResend
	stripeResend = resend
	t.Cleanup(func() { stripeResend = old })
	setStripeRate(t, 200<<10)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(61)}
	rateLinks(n, 2<<20)
	iran, err := newMConnIran(61, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(3)
	data := randBytes(12<<20, 61)
	go func() {
		mWaitFor(t, "stripe", 10*time.Second, func() bool { return stripeSubs(iran) == 2 })
		time.Sleep(300 * time.Millisecond)
		l := n.link(1)
		l.exit.drop.Store(true) // written, never delivered
		time.Sleep(300 * time.Millisecond)
		l.exit.Close()
		l.iran.Close()
	}()
	errs := make(chan error, 2)
	go func() { errs <- xfer(n.exit, data, 32<<10, 0) }()
	go func() { errs <- readN(iran, data) }()
	var first error
	for range 2 {
		select {
		case err := <-errs:
			if err != nil && first == nil {
				first = err
			}
		case <-time.After(25 * time.Second):
			first = errors.New("download hung")
		}
		if first != nil {
			break
		}
	}
	iran.abort()
	n.exit.abort()
	return first
}

// The bug (u8 in the lab, r31 in production): the old rule resets the
// connection when a stripe sub dies after its link swallowed bytes.
func TestStripeLinkDeathOldRuleResets(t *testing.T) {
	if err := stripeLinkDeath(t, false); err == nil {
		t.Fatal("download survived a stripe sub's lost bytes without the resend: the test no longer reproduces the bug")
	} else {
		t.Logf("old rule: %v", err)
	}
}

// With the resend the download completes, every byte in order.
func TestStripeLinkDeathResend(t *testing.T) {
	r0 := stripeResendStat.resends.Load()
	if err := stripeLinkDeath(t, true); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if stripeResendStat.resends.Load() == r0 {
		t.Fatal("no resend counted")
	}
}
