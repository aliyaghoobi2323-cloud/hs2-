package smux

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// laneSched is a test FrameSched: three FIFOs, control first, then the
// latency lane, then bulk (hs2's engine.Sched is the real one).
type laneSched struct {
	q     [3][]interface{}
	ticks int
	rate  float64
}

func (l *laneSched) Push(it interface{}, user uint32, flow uint64, lane uint8, size int) {
	l.q[lane] = append(l.q[lane], it)
}

func (l *laneSched) Next() (interface{}, bool) {
	for i := range l.q {
		if len(l.q[i]) > 0 {
			it := l.q[i][0]
			l.q[i] = l.q[i][1:]
			return it, true
		}
	}
	return nil, false
}

func (l *laneSched) Tick(now time.Duration, rate float64) { l.ticks++; l.rate = rate }

func schedConfig() *Config {
	c := DefaultConfig()
	c.Version = 2
	c.KeepAliveDisabled = true
	c.MaxFrameSize = 4096
	c.Sched = func() FrameSched { return &laneSched{} }
	return c
}

// schedPipe is a client session with a frame scheduler over net.Pipe whose
// peer reads a frame only when the test asks.
type schedPipe struct {
	t    *testing.T
	sess *Session
	peer net.Conn
}

func newSchedPipe(t *testing.T, c *Config) *schedPipe {
	c1, c2 := net.Pipe()
	sess, err := Client(c1, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(); c2.Close() })
	return &schedPipe{t, sess, c2}
}

func (p *schedPipe) read() pipeFrame {
	p.t.Helper()
	var hdr rawHeader
	p.peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(p.peer, hdr[:]); err != nil {
		p.t.Fatal(err)
	}
	body := make([]byte, hdr.Length())
	if _, err := io.ReadFull(p.peer, body); err != nil {
		p.t.Fatal(err)
	}
	return pipeFrame{hdr.Cmd(), hdr.StreamID(), len(body)}
}

func (p *schedPipe) open(tag uint32) *Stream {
	p.t.Helper()
	got := make(chan *Stream, 1)
	go func() {
		s, _ := p.sess.OpenStreamSyn(EncodeSyn(tag, 0))
		got <- s
	}()
	if f := p.read(); f.cmd != cmdSYN {
		p.t.Fatalf("got cmd %d, want a SYN", f.cmd)
	}
	return <-got
}

// until waits for cond on the session's scheduler stats.
func (p *schedPipe) until(what string, cond func(SchedStats) bool) {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.sess.sch.mu.Lock()
		st := SchedStats{Queued: p.sess.sch.queued, Waiting: p.sess.sch.waiting, Blocked: p.sess.sch.blocked}
		p.sess.sch.mu.Unlock()
		if cond(st) {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("%s: never (queued %d, waiting %d)", what, st.Queued, st.Waiting)
		}
		time.Sleep(time.Millisecond)
	}
}

// A control frame (a window update) is never behind data: with the writer
// held up and bulk queued, it leaves right after the frame being written.
func TestSchedCtrlAheadOfData(t *testing.T) {
	p := newSchedPipe(t, schedConfig())
	var bulk []*Stream
	for i := 0; i < 4; i++ {
		bulk = append(bulk, p.open(uint32(i+1)))
	}
	reader := p.open(9)
	for _, s := range bulk {
		go s.Write(make([]byte, 64<<10))
	}
	// one frame in the writer, three queued
	p.until("bulk queued", func(st SchedStats) bool { return st.Queued >= 3 })
	psh := make([]byte, headerSize+100)
	psh[0], psh[1] = byte(p.sess.config.Version), cmdPSH
	binary.LittleEndian.PutUint16(psh[2:], 100)
	binary.LittleEndian.PutUint32(psh[4:], reader.ID())
	go p.peer.Write(psh) // net.Pipe: the session's reader takes it
	go reader.Read(make([]byte, 100))
	p.until("update queued", func(st SchedStats) bool { return st.Queued >= 4 })
	if f := p.read(); f.cmd != cmdPSH {
		t.Fatalf("first frame: cmd %d, want the data frame being written", f.cmd)
	}
	if f := p.read(); f.cmd != cmdUPD || f.sid != reader.ID() {
		t.Fatalf("second frame: cmd %d sid %d, want the update of %d", f.cmd, f.sid, reader.ID())
	}
}

// A user has at most UserFrames data frames queued: the writer of one more
// waits in turn (and is counted), another user's writer does not wait, and
// the one waiting is let in as the user's frames are taken.
func TestSchedUserBound(t *testing.T) {
	c := schedConfig()
	c.UserFrames = 2
	p := newSchedPipe(t, c)
	a1, a2, a3, a4 := p.open(1), p.open(1), p.open(1), p.open(1)
	b1 := p.open(2)
	for _, s := range []*Stream{a1, a2, a3} {
		go s.Write(make([]byte, 4096))
	}
	// one in the writer, two queued: the user's bound
	p.until("two queued", func(st SchedStats) bool { return st.Queued == 2 })
	go a4.Write(make([]byte, 4096))
	p.until("a4 waits", func(st SchedStats) bool { return st.Waiting == 1 && st.Blocked >= 1 })
	go b1.Write(make([]byte, 4096))
	p.until("b1 queued", func(st SchedStats) bool { return st.Queued == 3 && st.Waiting == 1 })
	seen := map[uint32]bool{}
	for i := 0; i < 5; i++ {
		f := p.read()
		if f.cmd != cmdPSH {
			t.Fatalf("frame %d: cmd %d", i, f.cmd)
		}
		seen[f.sid] = true
	}
	for _, s := range []*Stream{a1, a2, a3, a4, b1} {
		if !seen[s.ID()] {
			t.Fatalf("stream %d never wrote", s.ID())
		}
	}
	p.until("no one waits", func(st SchedStats) bool { return st.Waiting == 0 && st.Queued == 0 })
	st, _ := p.sess.SchedStats()
	// a4, and a3 if it came before the writer took its first frame
	if st.Blocked < 1 || st.Blocked > 2 {
		t.Fatalf("blocked writers %d, want 1 or 2", st.Blocked)
	}
}

// A writer that gives up while it waits for its user's room leaves the
// user's FIFO: the one behind it gets in, and nothing of it is written.
func TestSchedUserBoundDeadline(t *testing.T) {
	c := schedConfig()
	c.UserFrames = 1
	p := newSchedPipe(t, c)
	a1, a2, a3, a4 := p.open(1), p.open(1), p.open(1), p.open(1)
	go a1.Write(make([]byte, 4096))
	go a2.Write(make([]byte, 4096))
	p.until("a2 queued", func(st SchedStats) bool { return st.Queued == 1 })
	a3.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	errc := make(chan error, 1)
	go func() { _, err := a3.Write(make([]byte, 4096)); errc <- err }()
	p.until("a3 waits", func(st SchedStats) bool { return st.Waiting == 1 })
	go a4.Write(make([]byte, 4096))
	p.until("a4 waits too", func(st SchedStats) bool { return st.Waiting == 2 })
	if err := <-errc; err != ErrTimeout {
		t.Fatalf("a3: %v, want a timeout", err)
	}
	p.until("a3 gone", func(st SchedStats) bool { return st.Waiting == 1 })
	var sids []uint32
	for i := 0; i < 3; i++ {
		sids = append(sids, p.read().sid)
	}
	first := map[uint32]bool{sids[0]: true, sids[1]: true}
	if !first[a1.ID()] || !first[a2.ID()] || sids[2] != a4.ID() {
		t.Fatalf("frames of %v, want %d and %d, then %d", sids, a1.ID(), a2.ID(), a4.ID())
	}
}

// Frames are cut for their lane: the latency lane's within LaneFrame, the
// bulk lane's within the bulk frame.
func TestSchedFrameSizes(t *testing.T) {
	c := schedConfig()
	c.LaneFrame = 1000
	c.BulkFrame = func(float64) int { return 3000 }
	var light atomic.Uint32
	c.FlowInfo = func(st *Stream) (uint32, uint8) {
		if st.ID() == light.Load() {
			return 1, LaneLatency
		}
		return 2, LaneBulk
	}
	p := newSchedPipe(t, c)
	a, b := p.open(0), p.open(0)
	light.Store(a.ID())
	go a.Write(make([]byte, 2500))
	got := map[uint32][]int{}
	for i := 0; i < 3; i++ {
		f := p.read()
		got[f.sid] = append(got[f.sid], f.n)
	}
	go b.Write(make([]byte, 7000))
	for i := 0; i < 3; i++ {
		f := p.read()
		got[f.sid] = append(got[f.sid], f.n)
	}
	want := map[uint32][]int{a.ID(): {1000, 1000, 500}, b.ID(): {3000, 3000, 1000}}
	for sid, w := range want {
		g := got[sid]
		if len(g) != len(w) {
			t.Fatalf("stream %d frames %v, want %v", sid, g, w)
		}
		for i := range w {
			if g[i] != w[i] {
				t.Fatalf("stream %d frames %v, want %v", sid, g, w)
			}
		}
	}
	if a.TxBytes() != 2500 || b.TxBytes() != 7000 {
		t.Fatalf("tx bytes %d %d", a.TxBytes(), b.TxBytes())
	}
}

// UserMem may refuse a user's next frame: its writer waits until the
// user's frame before it is taken, never when the user has nothing queued.
func TestSchedUserMem(t *testing.T) {
	c := schedConfig()
	var mu sync.Mutex
	held := map[uint32]int{}
	refused := 0
	c.UserMem = func(tag uint32) func(int, bool) bool {
		return func(n int, force bool) bool {
			mu.Lock()
			defer mu.Unlock()
			if n > 0 && !force && held[tag]+n > 5000 {
				refused++
				return false
			}
			held[tag] += n
			return true
		}
	}
	p := newSchedPipe(t, c)
	a1, a2, a3 := p.open(1), p.open(1), p.open(1)
	go a1.Write(make([]byte, 4000)) // into the writer: nothing held
	time.Sleep(50 * time.Millisecond)
	p.until("a1 taken", func(st SchedStats) bool { return st.Queued == 0 })
	go a2.Write(make([]byte, 4000)) // nothing else queued: in
	p.until("a2 queued", func(st SchedStats) bool { return st.Queued == 1 })
	go a3.Write(make([]byte, 4000)) // 8000 > 5000: waits
	p.until("a3 waits", func(st SchedStats) bool { return st.Waiting == 1 })
	for i := 0; i < 3; i++ {
		p.read()
	}
	p.until("all out", func(st SchedStats) bool { return st.Queued == 0 && st.Waiting == 0 })
	mu.Lock()
	defer mu.Unlock()
	if held[1] != 0 || refused == 0 {
		t.Fatalf("held %d at the end, refused %d", held[1], refused)
	}
}

// flushConn counts the writer's flushes.
type flushConn struct {
	net.Conn
	flushes atomic.Int32
}

func (c *flushConn) Flush() error { c.flushes.Add(1); return nil }

// WriteFlowAsync queues frames of any flow on a stream without waiting:
// each one's done comes once it is written; past the peer's window
// ErrWouldBlock; and the frames still queued as the session closes get
// their done with an error.
func TestSchedWriteFlowAsync(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	fc := &flushConn{Conn: c1}
	sess, err := Client(fc, schedConfig())
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
	for i := 0; i < 8; i++ {
		if err := st.WriteFlowAsync(bytes.Repeat([]byte{byte(i)}, 100), 5, uint64(100+i%2), LaneLatency, cb); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		if f := p.read(); f.cmd != cmdPSH || f.n != 100 {
			t.Fatalf("frame %d: cmd %d n %d", i, f.cmd, f.n)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for done.Load() != 8 {
		if time.Now().After(deadline) {
			t.Fatalf("done %d of 8", done.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if fc.flushes.Load() == 0 {
		t.Fatal("the writer never flushed its conn as it ran dry")
	}
	// the peer's window (256 KiB before any update) takes no more
	big := make([]byte, 60000)
	n := 0
	for ; n < 10; n++ {
		if err := st.WriteFlowAsync(big, 5, 1, LaneBulk, cb); err == ErrWouldBlock {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if n != int(initialPeerWindow-800)/60000 {
		t.Fatalf("%d frames of 60000 B went in before ErrWouldBlock", n)
	}
	sess.Close()
	deadline = time.Now().Add(5 * time.Second)
	for done.Load()+failed.Load() != 8+int32(n) {
		if time.Now().After(deadline) {
			t.Fatalf("done %d, failed %d of %d", done.Load(), failed.Load(), 8+n)
		}
		time.Sleep(time.Millisecond)
	}
	if failed.Load() == 0 {
		t.Fatal("no queued frame was told the session closed")
	}
}

// Two sessions with a scheduler each carry many streams both ways, byte for
// byte.
func TestSchedEcho(t *testing.T) {
	c1, c2 := net.Pipe()
	cfg := schedConfig()
	cfg.LaneFrame = 512
	cfg.BulkFrame = func(r float64) int { return 1024 + int(r)%3000 }
	cfg.UserFrames = 3
	cli, err := Client(c1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Server(c2, schedConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	defer srv.Close()
	go func() {
		for {
			s, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go func() { io.Copy(s, s); s.Close() }()
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := cli.OpenStreamSyn(EncodeSyn(uint32(i%3), 0))
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			msg := bytes.Repeat([]byte{byte(i)}, 50000+i*777)
			go s.Write(msg)
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(s, got); err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(got, msg) {
				t.Errorf("stream %d: echo differs", i)
			}
		}(i)
	}
	wg.Wait()
}

// The flow options need a scheduler.
func TestSchedConfigNeedsSched(t *testing.T) {
	c := DefaultConfig()
	c.UserFrames = 64
	if VerifyConfig(c) == nil {
		t.Fatal("user frames without a scheduler accepted")
	}
	c = schedConfig()
	c.LaneFrame = -1
	if VerifyConfig(c) == nil {
		t.Fatal("a negative lane frame accepted")
	}
}
