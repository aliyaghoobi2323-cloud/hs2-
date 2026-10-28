package smux

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// The coalescing hold (Config.Coalesce): candidate gamma1's tests of R1
// round 3 (its SetStamp test aside: the judge's merge has no late stamp),
// and the jitter (CoalesceJitter).

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
	return coalescePipeJ(t, win, min, false)
}

func coalescePipeJ(t *testing.T, win time.Duration, min float64, jitter bool) (*Session, *stampConn) {
	c1, c2 := net.Pipe()
	sc := &stampConn{Conn: c1, got: make(chan struct{}, 64)}
	cfg := schedConfig()
	cfg.Coalesce, cfg.CoalesceMin, cfg.CoalesceJitter = win, min, jitter
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

// With CoalesceJitter each hold is a uniform draw in [0.5, 1.5) of the
// window: no fixed spacing of the records (gamma1's capture: 4.26 ms at the
// median with a fixed 4 ms hold).
func TestSchedCoalesceJitter(t *testing.T) {
	const win = 40 * time.Millisecond
	sess, sc := coalescePipeJ(t, win, 0, true)
	st, err := sess.OpenStreamSyn(EncodeSyn(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	settle(sc, 3*win)
	lo, hi := time.Hour, time.Duration(0)
	for i := 0; i < 12; i++ {
		sc.reset()
		t0 := time.Now()
		st.WriteFlowAsync([]byte("light"), 5, 1, LaneLatency, nop)
		settle(sc, 2*win)
		fl := sc.flushes()
		if len(fl) != 1 {
			t.Fatalf("hold %d: %d flushes", i, len(fl))
		}
		d := fl[0].Sub(t0)
		lo, hi = min(lo, d), max(hi, d)
		if d < win/2-5*time.Millisecond || d > win*3/2+30*time.Millisecond {
			t.Fatalf("hold %d: %v, want within [%v, %v)", i, d, win/2, win*3/2)
		}
	}
	if hi-lo < win/4 {
		t.Fatalf("12 holds from %v to %v: no jitter", lo, hi)
	}
	t.Logf("12 jittered holds of a %v window: %v to %v", win, lo, hi)
}

// The coalescer's draw: the hold's length spans [0.5, 1.5) of the window.
func TestCoalescerDraw(t *testing.T) {
	old := coalRand
	defer func() { coalRand = old }()
	for _, v := range []int64{0, 999} {
		coalRand = func(n int64) int64 { return v * n / 1000 }
		c := coalescer{win: 4 * time.Millisecond, jitter: true}
		c.wrote(false, 1)
		want := 2*time.Millisecond + time.Duration(v*int64(4*time.Millisecond)/1000)
		if c.cur != want {
			t.Fatalf("draw %d: hold %v, want %v", v, c.cur, want)
		}
	}
}
