package smux

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

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
		if err := st.WriteFlowAsync([]byte("light frame"), 5, 1, LaneLatency, nil); err != nil {
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
	st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nil)
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
	st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nil)
	settle(sc, 3*win)
	fl := sc.flushes()
	if len(fl) != 1 || fl[0].Sub(t0) > win/2 {
		t.Fatalf("under CoalesceMin: %d flushes; want one at once", len(fl))
	}
	// 60 frames a second: past it, held
	for i := 0; i < 60; i++ {
		st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nil)
	}
	settle(sc, 3*win)
	sc.reset()
	t0 = time.Now()
	st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nil)
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
