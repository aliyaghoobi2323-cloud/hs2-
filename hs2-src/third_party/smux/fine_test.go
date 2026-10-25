package smux

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// FineStepOf: an eighth of the window, no less than an eighth of the need,
// never more than StepOf.
func TestFineStepOf(t *testing.T) {
	const need = 2 << 20
	for _, tc := range []struct{ w, want uint32 }{
		{128 << 10, 64 << 10},  // StepOf: there is no finer step to take
		{512 << 10, 256 << 10}, // need/8 is StepOf here
		{1 << 20, 256 << 10},   // need/8, not w/8
		{2 << 20, 256 << 10},   // the half-window lock's window: 4 per MiB, not 1
		{4 << 20, 512 << 10},   // w/8 under StepOf's need/2
		{8 << 20, 1 << 20},     // w/8 is StepOf from here on
		{16 << 20, 2 << 20},
	} {
		if got := FineStepOf(need, tc.w); got != tc.want {
			t.Errorf("FineStepOf(%d) = %d, want %d", tc.w, got, tc.want)
		}
		if got, step := FineStepOf(need, tc.w), StepOf(need, tc.w); got > step {
			t.Errorf("FineStepOf(%d) = %d past StepOf %d", tc.w, got, step)
		}
	}
	if got := FineStepOf(need, 0); got != 0 {
		t.Errorf("FineStepOf(2 MiB, 0) = %d, want 0", got)
	}
}

// ---- an emulated path ---------------------------------------------------------

// fpPipe is one direction of an emulated path: a bottleneck of rate bytes/s
// (0: none) with an unbounded queue, then a one-way delay (as the engine's
// wcPipe).
type fpPipe struct {
	mu      sync.Mutex
	cond    *sync.Cond
	q       []fpChunk
	lastDep time.Time
	rate    float64
	delay   time.Duration
	closed  bool
}

type fpChunk struct {
	at time.Time
	b  []byte
}

func newFpPipe(rate float64, delay time.Duration) *fpPipe {
	p := &fpPipe{rate: rate, delay: delay}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *fpPipe) write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	dep := time.Now()
	if p.rate > 0 {
		if p.lastDep.After(dep) {
			dep = p.lastDep
		}
		dep = dep.Add(time.Duration(float64(len(b)) / p.rate * float64(time.Second)))
		p.lastDep = dep
	}
	p.q = append(p.q, fpChunk{dep.Add(p.delay), append([]byte(nil), b...)})
	p.cond.Broadcast()
	return len(b), nil
}

func (p *fpPipe) read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return 0, io.EOF
		}
		if len(p.q) == 0 {
			p.cond.Wait()
			continue
		}
		c := &p.q[0]
		if d := time.Until(c.at); d > 0 {
			p.mu.Unlock()
			time.Sleep(d)
			p.mu.Lock()
			continue
		}
		n := copy(b, c.b)
		if c.b = c.b[n:]; len(c.b) == 0 {
			p.q = p.q[1:]
		}
		return n, nil
	}
}

func (p *fpPipe) close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// fpEnd is one end of a path; tap sees what it writes.
type fpEnd struct {
	in, out *fpPipe
	tap     *fpTap
}

func (e fpEnd) Read(b []byte) (int, error) { return e.in.read(b) }
func (e fpEnd) Write(b []byte) (int, error) {
	if e.tap != nil {
		e.tap.see(b)
	}
	return e.out.write(b)
}
func (e fpEnd) Close() error { e.in.close(); e.out.close(); return nil }

// fpTap counts the window updates in what an end writes, however its writes
// split the frames.
type fpTap struct {
	mu   sync.Mutex
	buf  []byte
	upds atomic.Int64
}

func (t *fpTap) see(b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	for len(t.buf) >= headerSize {
		var h rawHeader
		copy(h[:], t.buf)
		n := headerSize + int(h.Length())
		if len(t.buf) < n {
			return
		}
		if h.Cmd() == cmdUPD {
			t.upds.Add(1)
		}
		t.buf = t.buf[n:]
	}
}

// fineCfg is an adaptive stream without the window core, its need (and
// window) a quarter of hs2's, so a few MB/s over 100 ms are held by it: a
// step of 256 KiB, a fine step of 64 KiB.
func fineCfg() *Config {
	c := adaptiveConfig()
	c.MaxStreamBuffer = 512 << 10
	return c
}

// fpRun is one download over a path of rtt, its bottleneck rate bytes/s (0:
// none), from a sender paced at src bytes/s (0: all it may send at once) to
// a reader that takes read bytes/s (0: all it has at once), for warm then
// dur. It reports the reader's rate over dur and what the stream did
// meanwhile.
type fpResult struct {
	rate             float64 // bytes/s read over dur
	read             uint64  // bytes read over dur
	upds, fine, cycl int64   // updates on the wire, of the stream: fine ones, ones ending a cycle
	hits             int64   // of the cycles, hits
	held             bool    // held at the end
}

func fpRun(t *testing.T, cfg *Config, rtt time.Duration, rate, src, read float64, warm, dur time.Duration) fpResult {
	t.Helper()
	down := newFpPipe(rate*float64(cfg.MaxFrameSize+headerSize)/float64(cfg.MaxFrameSize), rtt/2)
	up := newFpPipe(0, rtt/2)
	tap := &fpTap{}
	edge := fpEnd{in: down, out: up, tap: tap}
	exit := fpEnd{in: up, out: down}
	rx, err := Client(edge, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := Server(exit, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { rx.Close(); tx.Close(); edge.Close() }()
	st, err := rx.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		x, err := tx.AcceptStream()
		if err != nil {
			return
		}
		buf := make([]byte, 16<<10)
		next := time.Now()
		for {
			if src > 0 {
				// a pacer, as BBR's: never faster than src, and no
				// credit for the time a full window kept it waiting
				if d := time.Until(next); d > 0 {
					time.Sleep(d)
				} else {
					next = time.Now()
				}
				next = next.Add(time.Duration(float64(len(buf)) / src * float64(time.Second)))
			}
			if _, err := x.Write(buf); err != nil {
				return
			}
		}
	}()
	var got atomic.Uint64
	go func() {
		buf := make([]byte, 16<<10)
		start := time.Now()
		var n float64
		for {
			if read > 0 {
				if ahead := n - read*time.Since(start).Seconds(); ahead > 0 {
					time.Sleep(time.Duration(ahead / read * float64(time.Second)))
				}
			}
			m, err := st.Read(buf)
			if err != nil {
				return
			}
			n += float64(m)
			got.Add(uint64(m))
		}
	}()
	time.Sleep(warm)
	r0, u0 := got.Load(), tap.upds.Load()
	f0, c0, h0 := st.core.fineN.Load(), st.core.updN.Load(), st.core.hitN.Load()
	t0 := time.Now()
	time.Sleep(dur)
	r1, u1 := got.Load(), tap.upds.Load()
	f1, c1, h1 := st.core.fineN.Load(), st.core.updN.Load(), st.core.hitN.Load()
	el := time.Since(t0)
	st.bufferLock.Lock()
	held := st.core.held.Load()
	st.bufferLock.Unlock()
	return fpResult{rate: float64(r1-r0) / el.Seconds(), read: r1 - r0, upds: u1 - u0,
		fine: int64(f1 - f0), cycl: int64(c1 - c0), hits: int64(h1 - h0), held: held}
}

func (r fpResult) perWindow(w int) float64 { return float64(r.upds) * float64(w) / float64(r.read) }

func (r fpResult) hitShare() float64 { return float64(r.hits) / float64(r.cycl) }

// fpCoarse is fpRun with no stream held (fineOff): the coarse step, as before
// the fine one.
func fpCoarse(t *testing.T, cfg *Config, rtt time.Duration, rate, src, read float64, warm, dur time.Duration) fpResult {
	fineOff = true
	defer func() { fineOff = false }()
	return fpRun(t, cfg, rtt, rate, src, read, warm, dur)
}

// The half-window lock: one download held back by its window moves, with an
// update each half window read, about (w-w/2)/RTT when its data comes in
// evenly and w/(RTT+(w/2)/rate) when it comes in bursts at the path's rate
// -- the field's 1 MiB a round trip under 2 MiB at 74 to 82 ms (101, 102,
// 109 Mbit/s). Held, it steps at FineStepOf (w/8 here) and moves up to 7/8
// of the window a round trip; at most updShare updates per window read.
//
//   - burst: a sender that sends all it may at once, through a bottleneck
//     of w/RTT: coarse 0.67 of the path (w/(RTT+RTT/2)), fine about 0.89;
//   - even: a source at 0.75 w/RTT over a path of no limit (a sender paced
//     at about its own rate, as BBR paces): coarse 0.6 w/RTT
//     (2s/(RTT+s/pace) with s = w/2), fine its pace.
func TestFineStepLiftsTheHalfWindowLock(t *testing.T) {
	cfg := fineCfg()
	w := cfg.MaxStreamBuffer
	const rtt = 100 * time.Millisecond
	wr := float64(w) / rtt.Seconds() // w/RTT, bytes/s
	for _, tc := range []struct {
		name      string
		rate, src float64
		want      float64 // at least, bytes/s
	}{
		{"burst", wr, 0, 0.80 * wr},
		{"even", 0, 0.75 * wr, 0.70 * wr},
	} {
		c := fpCoarse(t, cfg, rtt, tc.rate, tc.src, 0, 1500*time.Millisecond, 3*time.Second)
		r := fpRun(t, cfg, rtt, tc.rate, tc.src, 0, 1500*time.Millisecond, 3*time.Second)
		for _, x := range []struct {
			step string
			r    fpResult
		}{{"coarse", c}, {"fine", r}} {
			t.Logf("%s, %s step: %.2f w/RTT (%.0f KiB/s), %.2f updates per window read (%d fine, %d ending a cycle, %d hits), held %v",
				tc.name, x.step, x.r.rate/wr, x.r.rate/1024, x.r.perWindow(w), x.r.fine, x.r.cycl, x.r.hits, x.r.held)
		}
		if c.rate >= tc.want {
			t.Errorf("%s: the coarse step moves %.2f w/RTT: the rig does not show the half-window lock", tc.name, c.rate/wr)
		}
		if r.rate < tc.want {
			t.Errorf("%s: %.2f w/RTT, want at least %.2f (the half-window lock)", tc.name, r.rate/wr, tc.want/wr)
		}
		if r.fine == 0 || !r.held {
			t.Errorf("%s: %d fine updates, held %v: the window holds this stream", tc.name, r.fine, r.held)
		}
		if pw := r.perWindow(w); pw > updShare+1 {
			t.Errorf("%s: %.1f updates per window read, want at most %d", tc.name, pw, updShare)
		}
	}
}

// A stream the window holds at the fine step still pauses at its limits
// when its path is faster than its window (a sender that is still
// window-limited): its cycles are hits as often as under the coarse step,
// which the window core reads as a window-held stream (CoreSample.Hits,
// wcHitShare). (Its data comes in one train a round trip, at the path's
// rate: the pause is at the train's end, once a round trip, whichever
// update's limit that is: so about half the cycles of a window of two steps
// are hits, either way.)
func TestFineStepHeldStreamStillHits(t *testing.T) {
	cfg := fineCfg()
	const rtt = 100 * time.Millisecond
	wr := float64(cfg.MaxStreamBuffer) / rtt.Seconds()
	c := fpCoarse(t, cfg, rtt, 3*wr, 0, 0, 1500*time.Millisecond, 3*time.Second)
	r := fpRun(t, cfg, rtt, 3*wr, 0, 0, 1500*time.Millisecond, 3*time.Second)
	t.Logf("coarse: %.2f w/RTT, %d of %d cycles hits; fine: %.2f w/RTT, %d fine updates, %d of %d cycles hits",
		c.rate/wr, c.hits, c.cycl, r.rate/wr, r.fine, r.hits, r.cycl)
	if r.fine == 0 || r.cycl < 8 || r.hitShare() < c.hitShare()-0.15 {
		t.Fatalf("fine: %d fine updates, hits %.2f of the cycles, coarse %.2f: want fine updates and as many hits",
			r.fine, r.hitShare(), c.hitShare())
	}
}

// Streams the window does not hold back keep the coarse step, and no more
// updates than before: a source slower than the window allows (once its
// window has grown), and a reader slower than its path.
func TestFineStepOnlyForHeldStreams(t *testing.T) {
	cfg := fineCfg()
	w := cfg.MaxStreamBuffer
	const rtt = 100 * time.Millisecond
	wr := float64(w) / rtt.Seconds()
	for _, tc := range []struct {
		name            string
		rate, src, read float64
	}{
		{"slow source", 0, 0.3 * wr, 0},
		{"slow reader", 0, 0, 0.3 * wr},
	} {
		r := fpRun(t, cfg, rtt, tc.rate, tc.src, tc.read, 2*time.Second, 3*time.Second)
		t.Logf("%s: %.2f w/RTT, %.2f updates per window read (%d fine, %d ending a cycle), held %v",
			tc.name, r.rate/wr, r.perWindow(w), r.fine, r.cycl, r.held)
		if r.fine != 0 || r.held {
			t.Errorf("%s: %d fine updates, held %v: the window does not hold it", tc.name, r.fine, r.held)
		}
	}
}

// A held stream whose source slows down under what the coarse step lets
// through (fineKeep) steps at StepOf again within a few round trips.
func TestFineStepEndsWhenTheSourceSlows(t *testing.T) {
	cfg := fineCfg()
	w := cfg.MaxStreamBuffer
	const rtt = 100 * time.Millisecond
	wr := float64(w) / rtt.Seconds()
	down := newFpPipe(wr*float64(cfg.MaxFrameSize+headerSize)/float64(cfg.MaxFrameSize), rtt/2)
	up := newFpPipe(0, rtt/2)
	edge, exit := fpEnd{in: down, out: up}, fpEnd{in: up, out: down}
	rx, err := Client(edge, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := Server(exit, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { rx.Close(); tx.Close(); edge.Close() }()
	st, err := rx.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	var slow atomic.Bool
	go func() {
		x, err := tx.AcceptStream()
		if err != nil {
			return
		}
		buf := make([]byte, 4<<10)
		for {
			if slow.Load() {
				time.Sleep(10 * time.Millisecond) // 400 KiB/s: 0.08 w/RTT
			}
			if _, err := x.Write(buf); err != nil {
				return
			}
		}
	}()
	go io.Copy(io.Discard, st)
	heldNow := func() bool {
		st.bufferLock.Lock()
		defer st.bufferLock.Unlock()
		return st.core.held.Load()
	}
	time.Sleep(2 * time.Second)
	if !heldNow() {
		t.Fatal("a download held back by its window is not held")
	}
	slow.Store(true)
	time.Sleep(time.Second)
	if heldNow() {
		t.Fatal("still held a second after its source slowed to 0.08 w/RTT")
	}
	f0 := st.core.fineN.Load()
	time.Sleep(time.Second)
	if f := st.core.fineN.Load() - f0; f != 0 {
		t.Fatalf("%d fine updates from a slow source", f)
	}
}

// The update budget per byte read under hs2's need of 2 MiB: a held stream
// sends at most 4 updates per MiB, or what the coarse step already sends
// under a window so small that it has no finer step (StepOf, half of it).
func TestFineStepUpdateBudget(t *testing.T) {
	const need, mib = 2 << 20, 1 << 20
	for w := uint32(64 << 10); w <= 32<<20; w += w / 4 {
		fine, coarse := float64(mib)/float64(FineStepOf(need, w)), float64(mib)/float64(StepOf(need, w))
		if fine > 4 && fine > coarse {
			t.Errorf("window %d: %.1f updates per MiB held, %.1f not, want at most 4", w, fine, coarse)
		}
	}
}

// BenchmarkWindowUpdate is the cost of one window update, queued as a
// control frame and written by the session's writer: a held stream sends up
// to four per MiB read where the coarse step sends one.
func BenchmarkWindowUpdate(b *testing.B) {
	c1, c2 := net.Pipe()
	go io.Copy(io.Discard, c2)
	s, err := Client(c1, fineCfg())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	st, err := s.OpenStream()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.writeWindowUpdate(nil); err != nil {
			b.Fatal(err)
		}
	}
}
