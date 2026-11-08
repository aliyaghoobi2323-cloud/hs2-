package engine

import (
	"runtime"
	"testing"
	"time"
)

func (t *tSub) mconnCloseHS() bool { return true } // the test exit has this code

func setCloseHS(t *testing.T, on bool) {
	old := mconnCloseHS.Load()
	mconnCloseHS.Store(on)
	t.Cleanup(func() { mconnCloseHS.Store(old) })
}

// hold blocks this end's writes until the returned release is called.
func (t *tSub) hold() func() {
	g := make(chan struct{})
	t.gate.Store(&g)
	return func() {
		if t.gate.CompareAndSwap(&g, nil) {
			close(g)
		}
	}
}

func subIdx(m *mConn, idx int) *msub {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subs {
		if s.idx == idx {
			return s
		}
	}
	return nil
}

// crossing sets up the race: Kharej's first sub is stuck (loss recovery:
// a record held in its writer, its queue full), so Kharej puts the next
// chunk on the second sub as the only copy, held there in flight, while Iran
// — which has seen nothing on that sub for mLiteIdle — runs its idle rule on
// it. It returns the bytes Kharej wrote and the releases of the two holds.
// Everything stays under mDupMaxRate: Kharej keeps duplicating.
func crossing(t *testing.T, n *tNet, iran, exit *mConn, seed uint64) (sent []byte, rel0, rel1 func()) {
	t.Helper()
	l0, l1 := n.link(0), n.link(1)
	rel0 = l0.exit.hold()
	e0, e1 := subIdx(exit, 0), subIdx(exit, 1)
	const piece, tailN = 4 << 10, 2 << 10
	write := func(k int) {
		b := randBytes(k, seed+uint64(len(sent)))
		if err := xfer(exit, b, k, 0); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, b...)
	}
	write(piece)
	mWaitFor(t, "a record held in Kharej's first sub", time.Second, func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return e0.cur != nil
	})
	full := func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return !e0.fits(tailN)
	}
	for i := 0; i < 10 && !full(); i++ {
		write(piece)
	}
	if !full() {
		t.Fatal("Kharej's first sub never filled")
	}
	mWaitFor(t, "Kharej's second sub drained", time.Second, func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return len(e1.q) == 0 && e1.cur == nil
	})
	rel1 = l1.exit.hold()
	write(tailN)
	mWaitFor(t, "the only copy held on the second sub", time.Second, func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		c := e1.cur
		return c != nil && c.kind == mRecData && c.copies == 1
	})
	i1 := subIdx(iran, 1)
	iran.mu.Lock()
	i1.lastPush = time.Now().Add(-time.Hour)
	i1.lastRecv.Store(time.Now().Add(-time.Hour).UnixNano())
	iran.mu.Unlock()
	iran.kickMgr()
	mWaitFor(t, "Iran's idle rule on the second sub", time.Second, func() bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		return i1.retired
	})
	return sent, rel0, rel1
}

func newCrossPair(t *testing.T) (*tNet, *mConn, *mConn, []byte) {
	n, iran, exit := newLitePair(t)
	req := randBytes(600, 201)
	both(t, func() error { return xfer(iran, req, 600, 0) }, func() error { return readN(exit, req) })
	mWaitFor(t, "two subs", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	return n, iran, exit, req
}

// The crossing Iran's unilateral idle close could not survive. With the close
// handshake Iran only asks (CLOSE-REQ); Kharej's chunk goes out ahead of its
// CLOSE-ACK, Iran reads both and only then closes (CLOSE-DONE). Without it
// (key off) the chunk dies with the sub and Kharej resets the connection.
func TestMConnLiteCloseHandshakeCrossing(t *testing.T) {
	setCloseHS(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit, req := newCrossPair(t)
	sent, rel0, rel1 := crossing(t, n, iran, exit, 202)
	time.Sleep(100 * time.Millisecond) // the close (or CLOSE-REQ) reaches Kharej
	rel1()
	rel0()
	got := make(chan error, 1)
	go func() { got <- readN(iran, sent) }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Iran's read: %v (iran: %v, exit: %v)", err, iran.err, exit.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Iran's read stuck")
	}
	mWaitFor(t, "the second sub closed on both sides", 3*time.Second, func() bool {
		return liveSubs(iran) == 1 && liveSubs(exit) == 1
	})
	select {
	case <-iran.GetDieCh():
		t.Fatalf("Iran ended: %v", iran.err)
	case <-exit.GetDieCh():
		t.Fatalf("exit ended: %v", exit.err)
	default:
	}
	more := randBytes(16<<10, 204)
	both(t, func() error { return xfer(exit, more, 4<<10, 0) }, func() error { return readN(iran, more) })
	both(t, func() error { return xfer(iran, req, 600, 0) }, func() error { return readN(exit, req) })
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Kharej's second sub dies inside the handshake, before CLOSE-DONE, holding
// the only copy of a chunk: an ordinary death (reset), never a silent loss.
func TestMConnLiteCloseHandshakeDeathBeforeDone(t *testing.T) {
	setCloseHS(t, true)
	n, iran, exit, _ := newCrossPair(t)
	_, rel0, rel1 := crossing(t, n, iran, exit, 212)
	defer rel0()
	defer rel1()
	e1 := subIdx(exit, 1)
	mWaitFor(t, "CLOSE-REQ read by Kharej", time.Second, func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return e1.closeHS
	})
	n.link(1).exit.Close() // the link dies with the only copy still on it
	select {
	case <-exit.GetDieCh():
		if exit.err != errMConnLost {
			t.Fatalf("exit ended with %v, want %v", exit.err, errMConnLost)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Kharej took the death of a sub holding the only copy as a clean close")
	}
	iran.Close()
}
