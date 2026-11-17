package engine

import (
	"errors"
	"sync"
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
	// race147: with the grace on, a sub Iran opens after the cut joins only
	// once the exit has seen every sub die (its grace began). Otherwise the
	// order was left to the scheduler: when Iran's new sub reached the exit
	// first, the exit's plain stripe resend carried the chunks and the grace
	// path under test never ran (TestStripeAllDieGraceRejoins, ~1 in 20).
	var (
		openMu sync.Mutex // an open and the cut's last look at the links do not overlap
		cut    bool
	)
	open := func(p mPlane, avoid []any) (mSub, any, error) {
		openMu.Lock()
		if !cut || grace == 0 {
			defer openMu.Unlock()
			return n.open(p, avoid)
		}
		openMu.Unlock()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			n.exit.mu.Lock()
			in := !n.exit.graceUntil.IsZero() || n.exit.ended
			n.exit.mu.Unlock()
			if in {
				break
			}
		}
		return n.open(p, avoid)
	}
	iran, err := newMConnIran(62, open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(3, nil)
	data := randBytes(12<<20, 62)
	go func() {
		mWaitFor(t, "stripe", 10*time.Second, func() bool { return stripeSubs(iran) == 2 })
		// hs115: take every link again right before the close (a link
		// opened between the drop and the close stays out of the cut).
		time.Sleep(300 * time.Millisecond)
		snap := func() []*tLink {
			n.mu.Lock()
			defer n.mu.Unlock()
			return append([]*tLink(nil), n.links...) // every link the connection has
		}
		first := snap()
		for _, l := range first {
			l.exit.drop.Store(true) // written, never delivered
		}
		time.Sleep(300 * time.Millisecond)
		openMu.Lock()
		cut = true // from here Iran's reopen waits for the exit's grace
		ls := snap()
		openMu.Unlock()
		t.Logf("cut: %d stripe subs, %d links, %d more by the close", stripeSubs(iran), len(first), len(ls)-len(first))
		for _, l := range ls {
			l.exit.drop.Store(true)
		}
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
	// race147: stripeAllDie holds Iran's reopen until the exit's grace has
	// begun, so every round takes the grace path (hs115 left that order to
	// the scheduler and accepted it in one of 4 rounds: still ~1 in 20 lost).
	for i := range 2 {
		g0, r0 := stripeResendStat.graces.Load(), stripeResendStat.rejoined.Load()
		if err := stripeAllDie(t, 3*time.Second); err != nil {
			t.Fatalf("round %d: download failed: %v", i, err)
		}
		if g, r := stripeResendStat.graces.Load()-g0, stripeResendStat.rejoined.Load()-r0; g == 0 || r == 0 {
			t.Fatalf("round %d: delivered without the grace path (graces +%d, rejoins +%d)", i, g, r)
		}
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
