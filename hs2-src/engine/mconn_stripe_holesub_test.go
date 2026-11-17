package engine

import (
	"testing"
	"time"
)

// hs119: the hs117 s7/s8 deadlock. The other subs fill the reorder buffer,
// the gap room and their own share for an old hole; the sub carrying the
// resent hole chunk has a backlog (its smux window, written before the
// resend) beyond the buffer's span in front of it. Without the per-sub hole
// room every reader blocks and the hole never closes; with it the target's
// backlog is taken, the resend arrives and everything drains.
func TestHoleSubRoomDeadlock(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) { holeSubCase(t, on) })
	}
}

func holeSubCase(t *testing.T, on bool) {
	setKeepStall(t, true)
	old := holeSubOn
	holeSubOn = on
	t.Cleanup(func() { holeSubOn = old })
	const ck = 16 << 10
	const total = 48 << 20
	const x0, x1 = 44 << 20, 44<<20 + 3<<19 // the target's backlog (1.5 MiB)
	data := randBytes(total, 119)
	m := newMConn(1190, false, nil)
	others, target := &msub{m: m}, &msub{m: m}
	put := func(s *msub, o int) {
		c := getChunk(mRecData)
		copy(c.buf[:], data[o:o+ck])
		m.deliver(s, uint64(o), c, ck)
	}
	readErr := make(chan error, 1)
	go func() { readErr <- readAllCheck(m, data) }()

	// the other subs: everything but the hole and the target's backlog
	put(others, ck)
	m.r.mu.Lock()
	m.r.lastProgress = time.Now().Add(-2 * time.Second) // the hole is old
	m.r.mu.Unlock()
	g1 := make(chan struct{})
	go func() {
		defer close(g1)
		for o := 2 * ck; o < total; o += ck {
			if o >= x0 && o < x1 {
				continue
			}
			put(others, o)
		}
	}()
	// wait until they block on the full buffer
	last, still := -1, 0
	for still < 5 {
		time.Sleep(40 * time.Millisecond)
		m.r.mu.Lock()
		b := m.r.oooBytes
		m.r.mu.Unlock()
		if b == last {
			still++
		} else {
			last, still = b, 0
		}
	}
	select {
	case <-g1:
		t.Fatal("the other subs never blocked: the buffer is not full")
	default:
	}
	// the target: its backlog beyond the span, then the resent hole chunk
	g2 := make(chan struct{})
	go func() {
		defer close(g2)
		for o := x0; o < x1; o += ck {
			put(target, o)
		}
		put(target, 0)
	}()
	select {
	case <-g2:
		if !on {
			t.Fatal("without the hole room the target was not held (no deadlock to fix)")
		}
	case <-time.After(2 * time.Second):
		if on {
			t.Fatal("deadlock: the resent hole chunk never reached the receiver")
		}
		// the hs117 deadlock reproduced; end it
		m.end(errMConnClosed, false)
		<-g1
		<-g2
		<-readErr
		return
	}
	select {
	case <-g1:
	case <-time.After(5 * time.Second):
		t.Fatal("the other subs stayed blocked after the hole closed")
	}
	m.recvFin(total)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	m.end(errMConnClosed, false)
}
