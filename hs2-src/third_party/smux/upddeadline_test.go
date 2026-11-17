package smux

import (
	"net"
	"sync"
	"testing"
	"time"
)

// fx168 (audit rel66 M6): aw163 queues a stream's UPDs under one lock, held
// until the frame is in the shaper. On a one-way blackhole (the socket write
// never returns, the peer's frames still arrive) the shaper fills
// (maxShaperSize) and an AutoWindow shrink's UPD, sent from its own goroutine
// without a deadline, waits there forever holding the lock. A Read that then
// owes a UPD waited for the lock past its read deadline, forever.

// gateConn's Write blocks while the gate is shut (until released).
type gateConn struct {
	net.Conn
	mu      sync.Mutex
	shut    chan struct{} // nil: open
	release chan struct{}
}

func (c *gateConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	shut := c.shut
	c.mu.Unlock()
	if shut != nil {
		<-c.release
	}
	return c.Conn.Write(b)
}

func TestReadDeadlineWithUPDBlocked(t *testing.T) {
	a, b := delayPair(t, 0)
	g := &gateConn{Conn: b, release: make(chan struct{})}
	cli, err := Client(a, awConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Server(g, awConfig(newAW(1<<20, 1<<20)))
	if err != nil {
		t.Fatal(err)
	}
	var relOnce sync.Once
	release := func() { relOnce.Do(func() { close(g.release) }) }
	t.Cleanup(func() { release(); cli.Close(); srv.Close() })

	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	rs, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}

	// the server's socket writes block from here: a one-way blackhole
	g.mu.Lock()
	g.shut = make(chan struct{})
	g.mu.Unlock()
	if _, err := cs.Write(make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		rs.bufferLock.Lock()
		defer rs.bufferLock.Unlock()
		n := 0
		for _, x := range rs.buffers {
			n += len(x)
		}
		return n >= 32<<10
	}, "the data at the receiver")

	// fill the shaper: control frames without a deadline, given up on at
	// the end
	cut := make(chan struct{})
	t.Cleanup(func() { close(cut) })
	for i := 0; i < maxShaperSize+64; i++ {
		go srv.writeFrameCut(newFrame(byte(srv.config.Version), cmdNOP, 0), nil, cut, CLSCTRL)
	}
	time.Sleep(300 * time.Millisecond)

	// a shrink's UPD from its own goroutine (no read deadline), as
	// autowin.go / floor.go send it: it now waits in the full shaper
	rs.bufferLock.Lock()
	seq := rs.nextUPDSeqLocked()
	rs.bufferLock.Unlock()
	go rs.sendWindowUpdateSeq(10, 32<<10, seq)
	time.Sleep(100 * time.Millisecond)

	rs.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	t0 := time.Now()
	go func() {
		n, err := rs.Read(make([]byte, 32<<10))
		done <- res{n, err}
	}()
	select {
	case r := <-done:
		t.Logf("Read returned after %v: n=%d err=%v", time.Since(t0).Round(time.Millisecond), r.n, r.err)
		if d := time.Since(t0); d > 1500*time.Millisecond {
			t.Fatalf("Read returned %v after its 300 ms deadline", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read blocked 3 s past its 300 ms deadline (waiting for the UPD lock)")
	}
}
