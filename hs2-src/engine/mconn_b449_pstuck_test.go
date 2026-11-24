package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// b449 (mconn_b449_pstuck.go): lab b446's heavy stall. A lite connection
// sends one copy (dupOff) on its primary; the primary's stream stops (its
// writes block, no error: a stream whose link left the pool) with the queue
// full. rc12 (HS2_B449_PSTUCK=0 is the rc12 path): Write waits on the full
// primary forever, the other sub never gets a byte. b449: the primary is
// ended after b449StuckFor and the live sub carries every byte, in order,
// exactly once.

func setB449(t *testing.T, on bool) {
	t.Helper()
	old, oldF, oldH := b449On.Load(), b449StuckFor, b449HardFor
	b449On.Store(on)
	b449StuckFor = 600 * time.Millisecond
	b449HardFor = 3 * time.Second
	t.Cleanup(func() { b449On.Store(old); b449StuckFor = oldF; b449HardFor = oldH })
}

// setB449Keep: the sub resend keeps chunks (as the stripe does for a heavy
// connection in production) for the whole test transfer.
//
// The process-wide keep gauge (subResendStat.kept, checked against
// subKeepGlobalMax) starts from 0 for the test: it is shared by every test
// in the binary, and connections of earlier tests can leave it over 32 MiB.
// Then Iran's request writes are not kept (a gap on its sub), and when the
// exit ends its stuck primary Iran sees that sub's EOF with its other sub
// joined < mHoleSafe ago: the old lost-copy rule resets the connection
// ("a sub died holding the only copy", the 1-in-3 flake in -race runs).
// The exit's move itself was lossless in every case (B449_INFLATE repro,
// lab3 2026-11-20: 3/3 resets with the gauge raised by 40 MiB, 0/40 without).
func setB449Keep(t *testing.T) {
	t.Helper()
	oldR, oldS, oldK, oldSc := stripeResend.Load(), mconnSubResend.Load(), keepStallOn.Load(), subResendScope
	leaked := subResendStat.kept.Load() // atomic shift, not a store: stray conns may still add/drop
	stripeResend.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	subResendScope = 64 << 20
	subResendStat.kept.Add(-leaked)
	t.Cleanup(func() {
		stripeResend.Store(oldR)
		mconnSubResend.Store(oldS)
		keepStallOn.Store(oldK)
		subResendScope = oldSc
		subResendStat.kept.Add(leaked)
	})
}

func b449DupOff(ms ...*mConn) {
	for _, m := range ms {
		m.mu.Lock()
		m.dupOff = true
		m.dupStart = time.Now()
		m.dupBytes = 1 << 20
		m.mu.Unlock()
	}
}

// b449Read reads want then trailer from m within d: nil when both arrive
// byte-exact (a duplicate or a hole would corrupt one of them).
func b449Read(m *mConn, want, trailer []byte, d time.Duration) error {
	done := make(chan error, 1)
	go func() {
		if err := readN(m, want); err != nil {
			done <- err
			return
		}
		done <- readN(m, trailer)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return errTimeoutTest
	}
}

// Download (exit -> Iran): the exit's primary stops under a full queue, a
// second live sub exists.
func TestB449ExitPrimaryStuckMoves(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "b449", false: "rc12"}[on], func(t *testing.T) {
			err, moved := b449ExitStuck(t, on)
			if on && (err != nil || !moved) {
				t.Fatalf("b449: err=%v moved=%v, want every byte on the live sub", err, moved)
			}
			if !on {
				if err == nil {
					t.Fatal("rc12 path delivered past a stuck primary: the test does not reproduce the stall")
				}
				t.Logf("rc12 path (the stall): %v", err)
			}
		})
	}
}

func b449ExitStuck(t *testing.T, on bool) (error, bool) {
	setB449(t, on)
	setB449Keep(t)
	setCloseHS(t, true)
	m0 := b449Stat.moves.Load()
	n, iran, exit := newLitePair(t)
	hello := randBytes(300, 61)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	mWaitFor(t, "two subs", mLiteAfter+4*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	b449DupOff(exit) // the download is one copy; Iran's light requests go on both subs
	stop := make(chan struct{})
	defer close(stop)
	go func() { // Iran's request/ack trickle: the exit's second sub reads (fresh)
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := iran.Write([]byte("ping")); err != nil {
				return
			}
		}
	}()
	go func() {
		b := make([]byte, 64)
		for {
			if _, err := exit.Read(b); err != nil {
				return
			}
		}
	}()
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	ts, ok := p.rw.(*tSub)
	if !ok {
		t.Fatalf("exit primary rw %T", p.rw)
	}
	release := ts.hold() // the stream stops: writes block, no error
	t.Cleanup(release)
	_ = n
	data, trailer := randBytes(3<<20, 62), randBytes(5000, 63)
	go func() {
		if xfer(exit, data, 32<<10, 0) == nil {
			xfer(exit, trailer, 5000, 0)
		}
	}()
	err := b449Read(iran, data, trailer, 8*time.Second)
	moved := b449Stat.moves.Load() > m0
	release()
	iran.Close()
	exit.Close()
	return err, moved
}

// Upload (Iran -> exit) with ONE sub: Iran's primary stops; b449 opens a
// replacement sub on another link and moves the bytes there.
func TestB449IranOnlySubStuckOpensAndMoves(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "b449", false: "rc12"}[on], func(t *testing.T) {
			setB449(t, on)
			setB449Keep(t)
			setCloseHS(t, true)
			o0, m0 := b449Stat.opens.Load(), b449Stat.moves.Load()
			n, iran, exit := newLitePair(t)
			lightOnly(iran)
			lightOnly(exit)
			b449DupOff(iran, exit)
			ts := n.link(0).iran
			release := ts.hold()
			t.Cleanup(release)
			data, trailer := randBytes(3<<20, 71), randBytes(5000, 72)
			go func() {
				if xfer(iran, data, 32<<10, 0) == nil {
					xfer(iran, trailer, 5000, 0)
				}
			}()
			err := b449Read(exit, data, trailer, 8*time.Second)
			if on {
				if err != nil || b449Stat.moves.Load() == m0 {
					t.Fatalf("b449: err=%v moves %d opens %d (links %d), want every byte on a replacement sub",
						err, b449Stat.moves.Load()-m0, b449Stat.opens.Load()-o0, n.count(planeLight))
				}
				t.Logf("b449: opens %d moves %d links %d", b449Stat.opens.Load()-o0, b449Stat.moves.Load()-m0, n.count(planeLight))
			} else {
				if err == nil {
					t.Fatal("rc12 path delivered past a stuck only sub: the test does not reproduce the stall")
				}
				t.Logf("rc12 path (the stall): %v", err)
			}
			release()
			iran.Close()
			exit.Close()
		})
	}
}

// A slow primary that keeps writing is never moved (healthy behaviour
// unchanged): 1.5 MB at ~600 KB/s with a full queue the whole time.
func TestB449SlowPrimaryNotMoved(t *testing.T) {
	setB449(t, true)
	setB449Keep(t)
	setCloseHS(t, true)
	n, iran, exit := newLitePair(t)
	hello := randBytes(300, 81)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	mWaitFor(t, "two subs", mLiteAfter+4*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	b449DupOff(iran, exit)
	_ = n
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	p.rw.(*tSub).rate.Store(600 << 10)
	m0 := b449Stat.moves.Load()
	data, trailer := randBytes(1500<<10, 82), randBytes(5000, 83)
	go func() {
		if xfer(exit, data, 32<<10, 0) == nil {
			xfer(exit, trailer, 5000, 0)
		}
	}()
	if err := b449Read(iran, data, trailer, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if b449Stat.moves.Load() != m0 {
		t.Fatalf("a writing primary was moved (%d moves)", b449Stat.moves.Load()-m0)
	}
	exit.mu.Lock()
	same := exit.primary.Load() == p
	exit.mu.Unlock()
	if !same {
		t.Fatal("primary changed")
	}
	iran.Close()
	exit.Close()
}

// Download with ONE sub: the exit's only sub stops (its link went dark), so
// the exit has no sub to ask on. Iran sees its receive silent and the sub's
// link dark and opens one at once; the exit's stuck rule moves every byte
// there within 2.5 s. Off (HS2_B449_DARKOPEN=0): the exit waits (lite's
// duplication cap, held exhausted as on a busy process, refuses the upgrade).
func TestB449DarkOpenExitOnlySub(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "darkopen", false: "off"}[on], func(t *testing.T) {
			setB449(t, true)
			setB449Keep(t)
			setCloseHS(t, true)
			oldD := b449DarkOn.Load()
			b449DarkOn.Store(on)
			t.Cleanup(func() { b449DarkOn.Store(oldD) })
			d0, m0 := b449Stat.darkOpens.Load(), b449Stat.moves.Load()
			n, iran, exit := newLitePair(t)
			lightOnly(iran)
			lightOnly(exit)
			b449DupOff(exit)
			hello := randBytes(300, 91)
			both(t, func() error { return xfer(exit, hello, 300, 0) }, func() error { return readN(iran, hello) })
			l0 := n.link(0)
			var dark atomic.Bool
			h := func(s *msub, _ int64) bool { return dark.Load() && any(s.rw) == any(l0.iran) }
			cs226SilentHook.Store(&h)
			t.Cleanup(func() { cs226SilentHook.Store(nil) })
			stop := make(chan struct{})
			defer close(stop)
			go func() { // Iran's request/ack trickle: a new sub reads at once (fresh)
				for {
					select {
					case <-stop:
						return
					case <-time.After(100 * time.Millisecond):
					}
					if _, err := iran.Write([]byte("ping")); err != nil {
						return
					}
				}
			}()
			go func() {
				b := make([]byte, 64)
				for {
					if _, err := exit.Read(b); err != nil {
						return
					}
				}
			}()
			go func() { // a busy process: lite's duplication cap refuses the upgrade (prod)
				for {
					liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
					select {
					case <-stop:
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
			}()
			release := l0.exit.hold() // the exit's only sub stops: writes block, no error
			t.Cleanup(release)
			dark.Store(true)
			data, trailer := randBytes(3<<20, 92), randBytes(5000, 93)
			go func() {
				if xfer(exit, data, 32<<10, 0) == nil {
					xfer(exit, trailer, 5000, 0)
				}
			}()
			t0 := time.Now()
			err := b449Read(iran, data, trailer, 2500*time.Millisecond)
			took := time.Since(t0)
			opens, moves := b449Stat.darkOpens.Load()-d0, b449Stat.moves.Load()-m0
			if on {
				if err != nil || opens == 0 || moves == 0 {
					t.Fatalf("darkopen: err=%v dark opens %d moves %d links %d, want every byte on Iran's new sub",
						err, opens, moves, n.count(planeLight))
				}
				t.Logf("darkopen: all bytes in %v (dark opens %d, moves %d)", took.Round(time.Millisecond), opens, moves)
			} else {
				if err == nil || opens != 0 {
					t.Fatalf("off: err=%v dark opens %d: the test does not reproduce the wait", err, opens)
				}
				t.Logf("off (the wait): %v", err)
			}
			release()
			iran.Close()
			exit.Close()
		})
	}
}

// A paused origin is not dark: Iran's receive goes silent on a live link
// (the hook says not silent) and no sub is opened.
func TestB449DarkOpenPauseNoOpen(t *testing.T) {
	setB449(t, true)
	h := func(s *msub, _ int64) bool { return false }
	cs226SilentHook.Store(&h)
	t.Cleanup(func() { cs226SilentHook.Store(nil) })
	d0 := b449Stat.darkOpens.Load()
	_, iran, exit := newLitePair(t)
	lightOnly(iran)
	lightOnly(exit)
	hello := randBytes(300, 94)
	both(t, func() error { return xfer(exit, hello, 300, 0) }, func() error { return readN(iran, hello) })
	time.Sleep(3 * time.Second)
	if d := b449Stat.darkOpens.Load() - d0; d != 0 {
		t.Fatalf("a paused download on a live link opened %d subs", d)
	}
	iran.Close()
	exit.Close()
}
