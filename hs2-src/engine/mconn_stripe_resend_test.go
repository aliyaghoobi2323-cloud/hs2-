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
	old := stripeResend.Load()
	stripeResend.Store(resend)
	t.Cleanup(func() { stripeResend.Store(old) })
	setStripeRate(t, 200<<10)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(61)}
	rateLinks(n, 2<<20)
	iran, err := newMConnIran(61, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(3, nil)
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

// stripeAllDie: a striped download whose two links both die (the first one
// after swallowing what it was given). Returns the download's outcome.
func stripeAllDie(t *testing.T, grace time.Duration) error {
	t.Helper()
	old, oldG := stripeResend.Load(), stripeGraceDur.Load()
	stripeResend.Store(true)
	stripeGraceDur.Store(int64(grace))
	t.Cleanup(func() { stripeResend.Store(old); stripeGraceDur.Store(oldG) })
	setStripeRate(t, 200<<10)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(62)}
	rateLinks(n, 2<<20)
	iran, err := newMConnIran(62, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(3, nil)
	data := randBytes(12<<20, 62)
	go func() {
		mWaitFor(t, "stripe", 10*time.Second, func() bool { return stripeSubs(iran) == 2 })
		time.Sleep(300 * time.Millisecond)
		n.mu.Lock()
		ls := append([]*tLink(nil), n.links...) // every link the connection has
		n.mu.Unlock()
		for _, l := range ls {
			l.exit.drop.Store(true) // written, never delivered
		}
		time.Sleep(300 * time.Millisecond)
		for _, l := range ls {
			l.exit.Close()
			l.iran.Close()
		}
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

// HS2_MCONN_STRIPE_GRACE_MS=0: every sub died ends the connection.
func TestStripeAllDieNoGraceEnds(t *testing.T) {
	if err := stripeAllDie(t, 0); err == nil {
		t.Fatal("download survived the death of every sub without the grace: the test no longer reproduces it")
	} else {
		t.Logf("no grace: %v", err)
	}
}

// With the grace Iran opens a new sub, the exit keeps the connection and
// resends what the dead subs held: the download completes byte-exact.
func TestStripeAllDieGraceRejoins(t *testing.T) {
	g0, r0 := stripeResendStat.graces.Load(), stripeResendStat.rejoined.Load()
	if err := stripeAllDie(t, 3*time.Second); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if stripeResendStat.graces.Load() == g0 || stripeResendStat.rejoined.Load() == r0 {
		t.Fatal("no grace or no rejoin counted")
	}
}

// The lab u8 gap (rel49): the record whose write FAILS on a dying stripe sub
// was released by the writer before subDied collected the dead sub's chunks,
// so the resend never carried it and the receiver waited behind it (a 22-byte
// or one-chunk hole, mGapTimeout). Now it is kept and resent: a striped
// download whose stripe sub's write fails (and whose link then dies)
// completes byte-exact, several times over.
func TestStripeFailedWriteResent(t *testing.T) {
	old := stripeResend.Load()
	stripeResend.Store(true)
	t.Cleanup(func() { stripeResend.Store(old) })
	for i := range 3 {
		setStripeRate(t, 200<<10)
		setLite(t, true)
		resetLiteBudget()
		id := uint64(70 + i)
		n := &tNet{exit: newMConnExit(id)}
		rateLinks(n, 2<<20)
		iran, err := newMConnIran(id, n.open)
		if err != nil {
			t.Fatal(err)
		}
		iran.setStripe(3, nil)
		data := randBytes(8<<20, id)
		go func() {
			mWaitFor(t, "stripe", 10*time.Second, func() bool { return stripeSubs(iran) == 2 })
			time.Sleep(200 * time.Millisecond)
			l := n.link(1)
			l.exit.failW.Store(true) // the exit's next write on it fails: the link dies
		}()
		errs := make(chan error, 2)
		go func() { errs <- xfer(n.exit, data, 32<<10, 0) }()
		go func() { errs <- readN(iran, data) }()
		for range 2 {
			select {
			case err := <-errs:
				if err != nil {
					t.Fatalf("round %d: %v", i, err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("round %d: download hung (a gap the resend did not fill)", i)
			}
		}
		iran.abort()
		n.exit.abort()
	}
}
