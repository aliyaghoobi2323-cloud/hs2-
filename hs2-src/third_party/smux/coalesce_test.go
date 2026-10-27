package smux

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// nop is a WriteFlowAsync done that does nothing.
func nop(error) {}

// stampConn notes when the session's writer flushes it.
type stampConn struct {
	net.Conn
	mu  sync.Mutex
	at  []time.Time
	got chan struct{}
}

func (c *stampConn) Flush() error {
	c.mu.Lock()
	c.at = append(c.at, time.Now())
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
	return nil
}

func (c *stampConn) flushes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.at...)
}

func (c *stampConn) reset() {
	c.mu.Lock()
	c.at = nil
	c.mu.Unlock()
}

// coalescePipe: a client session with Coalesce over net.Pipe whose peer
// reads everything as it comes.
func coalescePipe(t *testing.T, win time.Duration, min float64) (*Session, *stampConn) {
	c1, c2 := net.Pipe()
	sc := &stampConn{Conn: c1, got: make(chan struct{}, 64)}
	cfg := schedConfig()
	cfg.Coalesce, cfg.CoalesceMin = win, min
	cfg.FlowInfo = func(st *Stream) (uint32, uint8) { tag, _ := st.Syn(); return tag, LaneLatency }
	sess, err := Client(sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, c2)
	t.Cleanup(func() { sess.Close(); c2.Close() })
	return sess, sc
}

// settle waits for the writer's flushes to stop (none for quiet).
func settle(sc *stampConn, quiet time.Duration) {
	for {
		select {
		case <-sc.got:
		case <-time.After(quiet):
			return
		}
	}
}

// Light frames within the window share one flush, at the window's end from
// the first of them; a frame of an urgent stream goes out at once; under
// CoalesceMin there is no hold.
func TestSchedCoalesce(t *testing.T) {
	const win = 40 * time.Millisecond
	sess, sc := coalescePipe(t, win, 0)
	st, err := sess.OpenStreamSyn(EncodeSyn(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	settle(sc, 3*win)
	sc.reset()
	t0 := time.Now()
	for i := 0; i < 3; i++ {
		if err := st.WriteFlowAsync([]byte("light frame"), 5, 1, LaneLatency, nop); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	settle(sc, 3*win)
	fl := sc.flushes()
	if len(fl) != 1 {
		t.Fatalf("%d flushes for three light frames within the window, want 1", len(fl))
	}
	if d := fl[0].Sub(t0); d < win-5*time.Millisecond || d > win+30*time.Millisecond {
		t.Fatalf("the flush came %v after the first frame, want about the window (%v)", d, win)
	}
	// an urgent stream's frame: flushed at once
	u, err := sess.OpenStreamSyn(EncodeSyn(6, 0))
	if err != nil {
		t.Fatal(err)
	}
	settle(sc, 3*win)
	u.SetUrgent(true)
	sc.reset()
	t0 = time.Now()
	if _, err := u.Write([]byte("one-way delay")); err != nil {
		t.Fatal(err)
	}
	settle(sc, 3*win)
	fl = sc.flushes()
	if len(fl) != 1 || fl[0].Sub(t0) > win/2 {
		t.Fatalf("urgent: %d flushes, the first %v after the write; want one at once", len(fl), fl[0].Sub(t0))
	}
	// a light frame held, then an urgent one: both go at once with it
	sc.reset()
	t0 = time.Now()
	st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nop)
	time.Sleep(2 * time.Millisecond)
	u.Write([]byte("one-way delay"))
	settle(sc, 3*win)
	fl = sc.flushes()
	if len(fl) != 1 || fl[0].Sub(t0) > win/2 {
		t.Fatalf("light then urgent: %d flushes; want one, before the window ends", len(fl))
	}
}

func TestSchedCoalesceMin(t *testing.T) {
	const win = 40 * time.Millisecond
	sess, sc := coalescePipe(t, win, 50)
	st, err := sess.OpenStreamSyn(EncodeSyn(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	settle(sc, 3*win)
	sc.reset()
	t0 := time.Now()
	st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nop)
	settle(sc, 3*win)
	fl := sc.flushes()
	if len(fl) != 1 || fl[0].Sub(t0) > win/2 {
		t.Fatalf("under CoalesceMin: %d flushes; want one at once", len(fl))
	}
	// 60 frames a second: past it, held
	for i := 0; i < 60; i++ {
		st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nop)
	}
	settle(sc, 3*win)
	sc.reset()
	t0 = time.Now()
	st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nop)
	settle(sc, 3*win)
	fl = sc.flushes()
	if len(fl) != 1 || fl[0].Sub(t0) < win-5*time.Millisecond {
		t.Fatalf("past CoalesceMin: %d flushes, the first %v after the frame; want one after the window", len(fl), fl[0].Sub(t0))
	}
}

func TestSchedCoalesceConfig(t *testing.T) {
	c := DefaultConfig()
	c.Coalesce = time.Millisecond
	if VerifyConfig(c) == nil {
		t.Fatal("Coalesce without a scheduler: want an error")
	}
	c = schedConfig()
	c.Coalesce = -1
	if VerifyConfig(c) == nil {
		t.Fatal("negative Coalesce: want an error")
	}
}

// SetStamp: the writer calls the stream's hook with each data frame's bytes
// just before it writes them (what the hook writes goes on the wire), after
// the frame waited behind the one being written.
func TestSchedStamp(t *testing.T) {
	p := newSchedPipe(t, schedConfig())
	bulk := p.open(1)
	st := p.open(2)
	var calls int32
	var at time.Time
	st.SetStamp(func(b []byte) {
		calls++
		at = time.Now()
		copy(b, "STAMP")
	})
	go bulk.Write(make([]byte, 4000)) // one frame, held in the writer: the peer reads late
	time.Sleep(20 * time.Millisecond)
	queued := time.Now()
	done := make(chan error, 1)
	go func() { _, err := st.Write([]byte("xxxxxxxx")); done <- err }()
	time.Sleep(30 * time.Millisecond) // it waits behind the bulk frame the peer has not read
	var got []byte
	for i := 0; i < 2; i++ {
		var hdr rawHeader
		p.peer.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(p.peer, hdr[:]); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, hdr.Length())
		if _, err := io.ReadFull(p.peer, body); err != nil {
			t.Fatal(err)
		}
		if hdr.StreamID() == st.ID() {
			got = body
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(got) != "STAMPxxx" || calls != 1 {
		t.Fatalf("on the wire %q after %d calls, want the hook's bytes once", got, calls)
	}
	if at.Sub(queued) < 25*time.Millisecond {
		t.Fatalf("stamped %v after it was queued: want after its wait behind the bulk frame", at.Sub(queued))
	}
}

// A WriteFlowAsync frame of an urgent stream ends the coalescing hold.
func TestSchedCoalesceUrgentAsync(t *testing.T) {
	const win = 40 * time.Millisecond
	sess, sc := coalescePipe(t, win, 0)
	st, err := sess.OpenStreamSyn(EncodeSyn(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	settle(sc, 3*win)
	st.SetUrgent(true)
	sc.reset()
	t0 := time.Now()
	st.WriteFlowAsync([]byte("tun"), 5, 1, LaneLatency, nop)
	settle(sc, 3*win)
	fl := sc.flushes()
	if len(fl) != 1 || fl[0].Sub(t0) > win/2 {
		t.Fatalf("an urgent stream's async frame: %d flushes; want one at once", len(fl))
	}
}
