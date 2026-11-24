package engine

import (
	"math/rand/v2"
	"testing"
	"time"
)

func setStripeOverlap(t *testing.T, on, peer bool) {
	t.Helper()
	old, oldPeer := stripeOverlapOn.Load(), stripeOverlapPeerOK
	oldK, oldFix := stripeK.Load(), mconnDeadlineFix.Load()
	// b312: HS2_MCONN_DEADLINE_FIX on, as in production (envdef, both
	// sides). Off, the exit's raw reader could switch to records between
	// setReadLimit's limit and its wake, and the wake's stale deadline killed
	// the first sub mid-handshake ("every sub died", ~0.5-3 % of -race runs;
	// TestB312OverlapStaleDeadline forces that gap).
	mconnDeadlineFix.Store(true)
	stripeOverlapOn.Store(on)
	stripeOverlapPeerOK = func(*msub) bool { return peer }
	stripeK.Store(4)
	t.Cleanup(func() {
		stripeOverlapOn.Store(old)
		stripeOverlapPeerOK = oldPeer
		stripeK.Store(oldK)
		mconnDeadlineFix.Store(oldFix)
	})
}

// overlapStart: a raw pair whose exit has a raw write "in flight" on the first
// sub (its wmu held: what keeps production's handshake on for seconds), and
// Iran's stripe upgrade with a chain of more subs to open.
func overlapStart(t *testing.T, more int) (*tNet, *mConn, *mConn) {
	t.Helper()
	n, iran, exit := newRawPair(t)
	exit.wmu.Lock()
	iran.mu.Lock()
	if iran.rawTimer != nil {
		iran.rawTimer.Stop()
	}
	iran.stK = 4
	iran.stFastMore = more
	iran.stFastRate = 1e12
	iran.stOpening = true
	iran.lightOpening = true
	avoid := []any{iran.raw1.key}
	iran.mu.Unlock()
	go iran.openStripe(avoid, true)
	return n, iran, exit
}

func liveStripe(m *mConn) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveStripeLocked()
}

func handshaking(m *mConn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handshaking
}

// With the key and a peer that announced it, the chain's subs open while the
// handshake is still on; the bytes stay in order across the raw boundary
// while the stripe subs carry the new data, and the raw sub joins at the end.
func TestStripeOverlapOpensDuringHandshake(t *testing.T) {
	setStripeOverlap(t, true, true)
	data := make([]byte, 6<<20)
	for i := range data {
		data[i] = byte(rand.IntN(256))
	}
	_, iran, exit := overlapStart(t, 2)
	done := make(chan error, 2)
	go func() { done <- readN(exit, data) }() // the exit's raw reader is free
	// raw bytes first (they may race the upgrade: either way in order)
	raw := 1 << 20
	if err := xfer(iran, data[:raw], 32<<10, 0); err != nil {
		t.Fatal(err)
	}
	mWaitFor(t, "3 stripe subs while handshaking", 2*time.Second, func() bool {
		return liveStripe(iran) >= 3 && handshaking(iran)
	})
	go func() { done <- xfer(iran, data[raw:], 16<<10, 0) }()
	// the exit reads past the boundary while its UPGRADE is still held back
	time.Sleep(200 * time.Millisecond)
	if !handshaking(iran) {
		t.Fatal("the handshake ended while the exit's raw write was in flight")
	}
	exit.wmu.Unlock()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	mWaitFor(t, "handshake over", 2*time.Second, func() bool { return !handshaking(iran) && !handshaking(exit) })
	iran.Close()
	exit.Close()
}

// Without the key, or toward an older peer, the chain waits for the
// handshake's end (today's order).
func TestStripeOverlapWaitsWithoutKeyOrPeer(t *testing.T) {
	for _, c := range []struct{ on, peer bool }{{false, true}, {true, false}} {
		t.Run("", func(t *testing.T) {
			setStripeOverlap(t, c.on, c.peer)
			_, iran, exit := overlapStart(t, 2)
			mWaitFor(t, "upgrade", 2*time.Second, func() bool { return liveStripe(iran) >= 1 })
			time.Sleep(200 * time.Millisecond)
			if k := liveStripe(iran); k != 1 || !handshaking(iran) {
				t.Fatalf("stripe subs %d handshaking %v, want 1 and true", k, handshaking(iran))
			}
			exit.wmu.Unlock()
			mWaitFor(t, "handshake over", 2*time.Second, func() bool { return !handshaking(iran) && !handshaking(exit) })
			iran.Close()
			exit.Close()
		})
	}
}

// A stripe sub dying while the overlap's handshake is on ends the connection
// (no resend/evac before the boundaries are agreed): never wrong bytes.
func TestStripeOverlapSubDiesInHandshake(t *testing.T) {
	setUpgSafe(t, false) // b449j: the key-off outcome; key on: TestB449jStripeOverlapSubDies
	setStripeOverlap(t, true, true)
	n, iran, exit := overlapStart(t, 2)
	mWaitFor(t, "3 stripe subs", 2*time.Second, func() bool { return liveStripe(iran) >= 3 })
	n.mu.Lock()
	l := n.links[len(n.links)-1]
	n.mu.Unlock()
	l.iran.Close()
	select {
	case <-iran.GetDieCh():
	case <-time.After(2 * time.Second):
		t.Fatal("the connection did not end")
	}
	exit.wmu.Unlock()
	if !stripeGraceOff(iran) {
		t.Fatal("resend grace on while handshaking")
	}
	exit.Close()
}

func stripeGraceOff(m *mConn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ended || m.handshaking || !m.graceOKLocked()
}

// The gate itself: only Iran, only while still handshaking with the key, only
// toward a peer with the capability, never while the first sub is raw.
func TestStripeOverlapGate(t *testing.T) {
	setStripeOverlap(t, true, true)
	m := newMConn(9, true, nil)
	if !m.stripeOverlapLocked() {
		t.Fatal("not handshaking: the chain is free")
	}
	m.handshaking = true
	if m.stripeOverlapLocked() {
		t.Fatal("no raw sub: no overlap")
	}
	m.raw1 = &msub{m: m}
	s := &msub{m: m, stripe: true}
	m.subs = []*msub{m.raw1, s}
	if !m.stripeOverlapLocked() {
		t.Fatal("overlap refused")
	}
	m.rawW.Store(true)
	if m.stripeOverlapLocked() {
		t.Fatal("overlap while still writing raw")
	}
	m.rawW.Store(false)
	m.iran = false
	if m.stripeOverlapLocked() {
		t.Fatal("overlap on the exit")
	}
	if infoExt()&capExtMConnOverlap == 0 && mconnOn.Load() && mconnLite.Load() {
		t.Fatal("capability not announced with the stripe on")
	}
}
