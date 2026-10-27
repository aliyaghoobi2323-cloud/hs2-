package smux

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// urgentSched is laneSched that can give its control and latency lanes
// alone (UrgentSched).
type urgentSched struct{ laneSched }

func (u *urgentSched) NextUrgent() (interface{}, bool) {
	for i := 0; i < 2; i++ {
		if len(u.q[i]) > 0 {
			it := u.q[i][0]
			u.q[i] = u.q[i][1:]
			return it, true
		}
	}
	return nil, false
}

// gateCounter is a BulkGate the test opens and closes.
type gateCounter struct {
	open  atomic.Bool
	calls atomic.Int32
}

func (g *gateCounter) gate(size int) time.Duration {
	g.calls.Add(1)
	if g.open.Load() {
		return 0
	}
	return 2 * time.Millisecond
}

// A bulk frame the gate holds (BulkGate, γ2's in-flight budget) waits with
// its place kept: latency-lane frames that come meanwhile go past it, then,
// the gate open, it goes, and the bulk frame queued after it only then. A
// frame held as the session closes is told so.
func TestSchedBulkGate(t *testing.T) {
	c := schedConfig()
	c.Sched = func() FrameSched { return &urgentSched{} }
	g := &gateCounter{}
	c.BulkGate = g.gate
	c1, c2 := net.Pipe()
	defer c2.Close()
	fc := &flushConn{Conn: c1}
	sess, err := Client(fc, c)
	if err != nil {
		t.Fatal(err)
	}
	p := &schedPipe{t, sess, c2}
	st := p.open(5)
	var done, failed atomic.Int32
	cb := func(err error) {
		if err != nil {
			failed.Add(1)
		} else {
			done.Add(1)
		}
	}
	bulk := func(n int, b byte) {
		if err := st.WriteFlowAsync(bytes.Repeat([]byte{b}, n), 5, 1, LaneBulk, cb); err != nil {
			t.Fatal(err)
		}
	}
	bulk(1000, 1)
	bulk(1001, 2)
	// the writer holds the first bulk frame at the gate
	deadline := time.Now().Add(5 * time.Second)
	for g.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the gate was never asked again")
		}
		time.Sleep(time.Millisecond)
	}
	f0 := fc.flushes.Load()
	if err := st.WriteFlowAsync(bytes.Repeat([]byte{3}, 50), 5, 2, LaneLatency, cb); err != nil {
		t.Fatal(err)
	}
	if f := p.read(); f.cmd != cmdPSH || f.n != 50 {
		t.Fatalf("first frame out: cmd %d n %d, want the latency frame of 50", f.cmd, f.n)
	}
	deadline = time.Now().Add(5 * time.Second)
	for fc.flushes.Load() == f0 {
		if time.Now().After(deadline) {
			t.Fatal("the latency frame written past the gate was not flushed")
		}
		time.Sleep(time.Millisecond)
	}
	g.open.Store(true)
	if f := p.read(); f.cmd != cmdPSH || f.n != 1000 {
		t.Fatalf("second frame: cmd %d n %d, want the held bulk frame of 1000", f.cmd, f.n)
	}
	if f := p.read(); f.cmd != cmdPSH || f.n != 1001 {
		t.Fatalf("third frame: cmd %d n %d, want the bulk frame of 1001", f.cmd, f.n)
	}
	deadline = time.Now().Add(5 * time.Second)
	for done.Load() != 3 {
		if time.Now().After(deadline) {
			t.Fatalf("done %d of 3", done.Load())
		}
		time.Sleep(time.Millisecond)
	}
	// held as the session closes: told
	g.open.Store(false)
	bulk(500, 4)
	deadline = time.Now().Add(5 * time.Second)
	for n := g.calls.Load(); g.calls.Load() < n+2; {
		if time.Now().After(deadline) {
			t.Fatal("the last bulk frame was not held")
		}
		time.Sleep(time.Millisecond)
	}
	sess.Close()
	deadline = time.Now().Add(5 * time.Second)
	for failed.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the held frame was not told the session closed (failed %d)", failed.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// A FrameSched that cannot give its urgent lanes alone still works with a
// gate: the held frame goes once the gate opens.
func TestSchedBulkGateNoUrgent(t *testing.T) {
	c := schedConfig()
	g := &gateCounter{}
	c.BulkGate = g.gate
	p := newSchedPipe(t, c)
	st := p.open(5)
	if err := st.WriteFlowAsync(make([]byte, 700), 5, 1, LaneBulk, func(error) {}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for g.calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the gate was never asked again")
		}
		time.Sleep(time.Millisecond)
	}
	g.open.Store(true)
	if f := p.read(); f.cmd != cmdPSH || f.n != 700 {
		t.Fatalf("cmd %d n %d, want the bulk frame of 700", f.cmd, f.n)
	}
}

// FlushEach (lab): the writer flushes after each frame that is not bulk,
// with more queued; a bulk frame's tail is not flushed while more come.
func TestSchedFlushEach(t *testing.T) {
	for _, each := range []bool{false, true} {
		c := schedConfig()
		c.FlushEach = each
		c1, c2 := net.Pipe()
		fc := &flushConn{Conn: c1}
		sess, err := Client(fc, c)
		if err != nil {
			t.Fatal(err)
		}
		p := &schedPipe{t, sess, c2}
		st := p.open(5)
		f0 := fc.flushes.Load()
		// the writer is held writing the first frame (net.Pipe): the other
		// two queue behind it
		for i := 0; i < 3; i++ {
			if err := st.WriteFlowAsync(bytes.Repeat([]byte{byte(i)}, 40), 5, uint64(10+i), LaneLatency, func(error) {}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 3; i++ {
			if f := p.read(); f.cmd != cmdPSH || f.n != 40 {
				t.Fatalf("frame %d: cmd %d n %d", i, f.cmd, f.n)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for fc.flushes.Load() == f0 {
			if time.Now().After(deadline) {
				t.Fatal("never flushed")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		n := fc.flushes.Load() - f0
		if each && n < 3 || !each && n >= 3 {
			t.Fatalf("FlushEach %v: %d flushes for three latency frames queued together", each, n)
		}
		sess.Close()
		c2.Close()
	}
}

// BulkGate and FlushEach need a frame scheduler.
func TestSchedGateConfig(t *testing.T) {
	c := DefaultConfig()
	c.BulkGate = func(int) time.Duration { return 0 }
	if VerifyConfig(c) == nil {
		t.Fatal("a bulk gate without a scheduler passed")
	}
	c = DefaultConfig()
	c.FlushEach = true
	if VerifyConfig(c) == nil {
		t.Fatal("flush each without a scheduler passed")
	}
}
