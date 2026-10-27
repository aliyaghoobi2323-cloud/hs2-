package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// Angle A's tests (R1-PLAN task 15): two smux sessions, an edge's and an
// exit's, built by newSession as the links are, over an emulated link.

// angleWire is one direction of an emulated link: the sending socket and the
// path. The socket sends at rate (a TCP paced at the path's rate, as C4-lite
// holds it) and what it sent arrives delay later; it holds what is written
// and not sent yet (notsent), and takes a write, whole, only while less than
// its low-water mark is unsent (TCP_NOTSENT_LOWAT; 32 KiB, today's fixed
// mark, until the guard sets its own). It is its end's hostSys too.
type angleWire struct {
	mu     sync.Mutex
	cond   *sync.Cond
	rate   float64
	delay  time.Duration
	lowat  int
	depEnd time.Time // when all written so far has left the socket
	q      []wcChunk
	closed bool
	// the most unsent seen at a write
	maxNotsent int
}

func newAngleWire(rate float64, delay time.Duration) *angleWire {
	w := &angleWire{rate: rate, delay: delay, lowat: 32 << 10}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *angleWire) notsentAt(now time.Time) int {
	if d := w.depEnd.Sub(now); d > 0 {
		return int(d.Seconds() * w.rate)
	}
	return 0
}

// sleepUntilBelow waits (mu held, released while asleep) until less than n
// bytes are unsent; false: the wire closed.
func (w *angleWire) sleepUntilBelow(n int) bool {
	for {
		if w.closed {
			return false
		}
		now := time.Now()
		if w.notsentAt(now) < n {
			return true
		}
		d := w.depEnd.Add(-time.Duration(float64(n-1) / w.rate * float64(time.Second))).Sub(now)
		w.mu.Unlock()
		time.Sleep(max(d, 20*time.Microsecond))
		w.mu.Lock()
	}
}

func (w *angleWire) write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lowat > 0 && !w.sleepUntilBelow(w.lowat) || w.closed {
		return 0, io.ErrClosedPipe
	}
	now := time.Now()
	w.maxNotsent = max(w.maxNotsent, w.notsentAt(now)+len(b))
	start := now
	if w.depEnd.After(now) {
		start = w.depEnd
	}
	w.depEnd = start.Add(time.Duration(float64(len(b)) / w.rate * float64(time.Second)))
	w.q = append(w.q, wcChunk{w.depEnd.Add(w.delay), append([]byte(nil), b...)})
	w.cond.Broadcast()
	return len(b), nil
}

func (w *angleWire) read(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.closed {
			return 0, io.EOF
		}
		if len(w.q) == 0 {
			w.cond.Wait()
			continue
		}
		c := &w.q[0]
		if d := time.Until(c.at); d > 0 {
			w.mu.Unlock()
			time.Sleep(d)
			w.mu.Lock()
			continue
		}
		n := copy(b, c.b)
		if c.b = c.b[n:]; len(c.b) == 0 {
			w.q = w.q[1:]
		}
		return n, nil
	}
}

func (w *angleWire) close() {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

// angleEnd is one end of an emulated link: a net.Conn, and the hostSys of
// its socket (the wire it writes to).
type angleEnd struct{ in, out *angleWire }

func (e *angleEnd) Read(b []byte) (int, error)       { return e.in.read(b) }
func (e *angleEnd) Write(b []byte) (int, error)      { return e.out.write(b) }
func (e *angleEnd) Close() error                     { e.in.close(); e.out.close(); return nil }
func (e *angleEnd) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (e *angleEnd) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (e *angleEnd) SetDeadline(time.Time) error      { return nil }
func (e *angleEnd) SetReadDeadline(time.Time) error  { return nil }
func (e *angleEnd) SetWriteDeadline(time.Time) error { return nil }

func (e *angleEnd) setLowat(n int) error {
	e.out.mu.Lock()
	e.out.lowat = n
	e.out.mu.Unlock()
	return nil
}

func (e *angleEnd) notsent() (int, error) {
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	return e.out.notsentAt(time.Now()), nil
}

// waitBelow: until less than n is unsent, or the socket polls writable
// (under half its mark unsent, Linux 5.10 on).
func (e *angleEnd) waitBelow(n int) error {
	w := e.out
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sleepUntilBelow(max(n, w.lowat/2)) {
		return io.ErrClosedPipe
	}
	return nil
}

var _ hostSys = (*angleEnd)(nil)

// angleLink is an edge's session and an exit's over an emulated link of
// rate bytes/s each way, delay each way. The exit serves each stream by its
// first byte: 'D' writes to it for good (a download), 'U' reads it into the
// count of its user (an upload), 'E' echoes it (the probe).
type angleLink struct {
	t          *testing.T
	edge, exit *smux.Session
	down, up   *angleWire
	upRx       sync.Map // user tag -> *atomic.Uint64: upload bytes the exit read
	// onL3 serves a kindL3 stream (set before one is opened)
	onL3 func(*smux.Stream)
}

func newAngleLink(t *testing.T, rate float64, delay time.Duration) *angleLink {
	return newAngleLinkCaps(t, rate, delay, tlscarrier.LocalAuthHdr().Caps)
}

// newAngleLinkCaps: the link's ends have caps in common.
func newAngleLinkCaps(t *testing.T, rate float64, delay time.Duration, caps uint32) *angleLink {
	t.Helper()
	l := &angleLink{t: t, down: newAngleWire(rate, delay), up: newAngleWire(rate, delay)}
	var err error
	l.edge, _, err = newSessionCaps(&angleEnd{in: l.down, out: l.up}, false, nil, &linkMeter{statsPoll: make(chan struct{}, 1)}, caps)
	if err != nil {
		t.Fatal(err)
	}
	l.exit, _, err = newSessionCaps(&angleEnd{in: l.up, out: l.down}, true, nil, &linkMeter{}, caps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.edge.Close(); l.exit.Close(); l.down.close(); l.up.close() })
	go func() {
		for {
			st, err := l.exit.AcceptStream()
			if err != nil {
				return
			}
			go l.serve(st)
		}
	}()
	return l
}

func (l *angleLink) serve(st *smux.Stream) {
	defer st.Close()
	var k [1]byte
	if _, err := io.ReadFull(st, k[:]); err != nil {
		return
	}
	switch k[0] {
	case 'D':
		b := make([]byte, 32<<10)
		for {
			if _, err := st.Write(b); err != nil {
				return
			}
		}
	case 'U':
		tag, _ := st.Syn()
		c, _ := l.upRx.LoadOrStore(tag, new(atomic.Uint64))
		b := make([]byte, 32<<10)
		for {
			n, err := st.Read(b)
			c.(*atomic.Uint64).Add(uint64(n))
			if err != nil {
				return
			}
		}
	case 'E':
		io.Copy(st, st)
	case kindL3:
		if l.onL3 != nil {
			l.onL3(st)
		}
	}
}

func (l *angleLink) open(tag uint32, kind byte) *smux.Stream {
	st, err := l.edge.OpenStreamSyn(smux.EncodeSyn(tag, 0))
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := st.Write([]byte{kind}); err != nil {
		l.t.Fatal(err)
	}
	return st
}

// download opens n downloads of user tag; the edge counts what they read.
func (l *angleLink) download(ctx context.Context, tag uint32, n int, rx *atomic.Uint64) {
	for i := 0; i < n; i++ {
		st := l.open(tag, 'D')
		go func() {
			defer st.Close()
			b := make([]byte, 32<<10)
			for ctx.Err() == nil {
				k, err := st.Read(b)
				rx.Add(uint64(k))
				if err != nil {
					return
				}
			}
		}()
	}
}

// upload opens n uploads of user tag; the exit counts what it reads.
func (l *angleLink) upload(ctx context.Context, tag uint32, n int) *atomic.Uint64 {
	c, _ := l.upRx.LoadOrStore(tag, new(atomic.Uint64))
	for i := 0; i < n; i++ {
		st := l.open(tag, 'U')
		go func() {
			defer st.Close()
			b := make([]byte, 32<<10)
			for ctx.Err() == nil {
				if _, err := st.Write(b); err != nil {
					return
				}
			}
		}()
	}
	return c.(*atomic.Uint64)
}

// angleProbe is the interactive probe: 32 bytes each 200 ms, echoed; its
// round trips once on (from).
type angleProbe struct {
	mu   sync.Mutex
	rtts []time.Duration
	from time.Time
}

func (l *angleLink) probe(ctx context.Context, tag uint32) *angleProbe {
	p := &angleProbe{from: time.Now().Add(time.Hour)}
	st := l.open(tag, 'E')
	go func() {
		defer st.Close()
		b := make([]byte, 32)
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			t0 := time.Now()
			binary.BigEndian.PutUint64(b, uint64(t0.UnixNano()))
			if _, err := st.Write(b); err != nil {
				return
			}
			if _, err := io.ReadFull(st, b); err != nil {
				return
			}
			p.mu.Lock()
			if t0.After(p.from) {
				p.rtts = append(p.rtts, time.Since(t0))
			}
			p.mu.Unlock()
		}
	}()
	return p
}

// on starts the probe's count now.
func (p *angleProbe) on() {
	p.mu.Lock()
	p.from, p.rtts = time.Now(), nil
	p.mu.Unlock()
}

// pct is the probe's q-th percentile round trip so far, and the count.
func (p *angleProbe) pct(q float64) (time.Duration, int) {
	p.mu.Lock()
	r := append([]time.Duration(nil), p.rtts...)
	p.mu.Unlock()
	if len(r) == 0 {
		return 0, 0
	}
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	k := int(q*float64(len(r))+0.999999) - 1
	return r[min(max(k, 0), len(r)-1)], len(r)
}

// angleRate is what each counter took over d (bytes/s).
func angleRate(d time.Duration, cs ...*atomic.Uint64) []float64 {
	a := make([]uint64, len(cs))
	for i, c := range cs {
		a[i] = c.Load()
	}
	time.Sleep(d)
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = float64(c.Load()-a[i]) / d.Seconds()
	}
	return out
}

const (
	angleRateBps = 10e6 / 8 // 10 Mbit/s each way
	angleDelay   = 20 * time.Millisecond
	angleRTT     = 2 * angleDelay
	angleBound   = angleRTT + 20*time.Millisecond // RTT_floor + max(20 ms, 3 MTU/rate) at 10 Mbit/s
)

// The probe on the same link as 4 downloads and 2 uploads, both ways at 10
// Mbit/s (§5.1): its p99 within RTT + 20 ms, the link still full both ways.
func TestAngleSameLinkProbe(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link, 14 s")
	}
	l := newAngleLink(t, angleRateBps, angleDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var down atomic.Uint64
	l.download(ctx, 7, 4, &down)
	up := l.upload(ctx, 7, 2)
	p := l.probe(ctx, 7)
	time.Sleep(2 * time.Second)
	p.on()
	r := angleRate(12*time.Second, &down, up)
	p99, n := p.pct(0.99)
	p50, _ := p.pct(0.5)
	t.Logf("probe p50 %v p99 %v (%d samples); down %.2f up %.2f Mbit/s; most unsent at a write down/up %d/%d B",
		p50, p99, n, r[0]*8/1e6, r[1]*8/1e6, l.down.maxNotsent, l.up.maxNotsent)
	if n < 40 {
		t.Fatalf("only %d probes", n)
	}
	if p99 > angleBound {
		t.Fatalf("probe p99 %v, want <= %v (RTT %v + 20 ms)", p99, angleBound, angleRTT)
	}
	for i, x := range r {
		if x < 0.95*angleRateBps {
			t.Fatalf("direction %d carried %.2f Mbit/s of 10", i, x*8/1e6)
		}
	}
}

// P10 on the same link (§5.1 (a) to (e)), downloads: a lone user of 16
// streams takes the link; a user of one stream next to one of 16 gets its
// floor (C/2) and the other the rest, nothing wasted; a new heavy user
// neither pushes a light user's probe past the bound nor a bulk user under
// 0.9 of its floor.
func TestAngleP10(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link, 30 s")
	}
	const c = angleRateBps
	t.Run("lone16", func(t *testing.T) {
		l := newAngleLink(t, c, angleDelay)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var a atomic.Uint64
		l.download(ctx, 1, 16, &a)
		time.Sleep(2 * time.Second)
		r := angleRate(5*time.Second, &a)
		t.Logf("lone user, 16 streams: %.2f Mbit/s of %.0f", r[0]*8/1e6, c*8/1e6)
		if r[0] < 0.95*c {
			t.Fatalf("lone user of 16 streams took %.3f of the link, want >= 0.95", r[0]/c)
		}
	})
	t.Run("one_vs_16", func(t *testing.T) {
		l := newAngleLink(t, c, angleDelay)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var a, b atomic.Uint64
		l.download(ctx, 1, 1, &a)
		l.download(ctx, 2, 16, &b)
		time.Sleep(3 * time.Second)
		r := angleRate(6*time.Second, &a, &b)
		t.Logf("1 stream %.2f, 16 streams %.2f, total %.2f Mbit/s", r[0]*8/1e6, r[1]*8/1e6, (r[0]+r[1])*8/1e6)
		if r[0] < 0.9*c/2 {
			t.Fatalf("the user of one stream got %.3f of its floor, want >= 0.9", r[0]/(c/2))
		}
		if r[0]+r[1] < 0.97*c {
			t.Fatalf("the two took %.3f of the link, want >= 0.97", (r[0]+r[1])/c)
		}
	})
	t.Run("newheavy", func(t *testing.T) {
		l := newAngleLink(t, c, angleDelay)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var h1, h2 atomic.Uint64
		p := l.probe(ctx, 3) // the light user
		l.download(ctx, 1, 4, &h1)
		time.Sleep(3 * time.Second)
		l.download(ctx, 2, 16, &h2) // the new heavy user
		time.Sleep(time.Second)
		p.on()
		r := angleRate(8*time.Second, &h1, &h2)
		p99, n := p.pct(0.99)
		t.Logf("existing bulk %.2f, new heavy %.2f Mbit/s; light probe p99 %v (%d)", r[0]*8/1e6, r[1]*8/1e6, p99, n)
		if r[0] < 0.9*c/2 {
			t.Fatalf("the existing bulk user got %.3f of its floor, want >= 0.9", r[0]/(c/2))
		}
		if p99 > angleBound {
			t.Fatalf("the light user's p99 %v, want <= %v", p99, angleBound)
		}
		if r[0]+r[1] < 0.97*c {
			t.Fatalf("total %.3f of the link, want >= 0.97", (r[0]+r[1])/c)
		}
	})
}

// C1 on the link's streams, as the runner ticks it: the exit's downloads are
// bulk, the probe's echo light; and the link's status lines (§3.1-§3.4) in
// the documents' words.
func TestAngleClassesAndStatusLines(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link")
	}
	l := newAngleLink(t, angleRateBps, angleDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var down atomic.Uint64
	l.download(ctx, 1, 2, &down)
	l.probe(ctx, 1)
	time.Sleep(2 * time.Second)
	cl := coreLinks.of(l.exit)
	if cl == nil || cl.flows == nil || cl.hq == nil || cl.sc == nil {
		t.Fatal("the exit's link has no cores")
	}
	cl.flows.mu.Lock()
	light, bulk := cl.flows.light, cl.flows.bulk
	cl.flows.mu.Unlock()
	if bulk < 2 || light < 1 {
		t.Fatalf("exit's streams: light %d, bulk %d; want the 2 downloads bulk and the echo light", light, bulk)
	}
	now := time.Duration(ctrlNow())
	lines := []string{cl.schedLine(), cl.flows.line(now), cl.hq.line(cl.id, time.Second)}
	want := []string{fmt.Sprintf("sched link %d: lane p50/p99 ", cl.id), "classes: light ", fmt.Sprintf("hostq link %d: lowat ", cl.id)}
	for i, s := range lines {
		t.Log(s)
		if !strings.HasPrefix(s, want[i]) {
			t.Fatalf("line %q, want it to start %q", s, want[i])
		}
	}
	for _, part := range []string{"bulk frame ", "users ", "(active ", "drops light/bulk ", "blocked-writers ", "users at memory cap "} {
		if !strings.Contains(lines[0], part) {
			t.Fatalf("sched line %q lacks %q", lines[0], part)
		}
	}
	if len(CoreLines()) == 0 {
		t.Fatal("no core lines for the status")
	}
}

// HS2_TUNE_SCHED=0: the old path. Sessions have no scheduler (smux's heap
// and shaper loop), the shaper pads each write's tail as before, the TUN
// keeps its FIFO and writeLoop and picks a link per packet by the hash; the
// link still works both ways. (The whole engine suite runs in this mode
// with HS2_TUNE_SCHED=0 set: TestMain.)
func TestAngleSchedOff(t *testing.T) {
	// the other server's switch: its caps lack CapSched, and the link runs
	// the old path at both ends
	o := newAngleLinkCaps(t, angleRateBps, angleDelay, tlscarrier.LocalAuthHdr().Caps&^tlscarrier.CapSched)
	if o.edge.HasSched() || o.exit.HasSched() {
		t.Fatal("a scheduler on a link without CapSched")
	}
	defer func(v int) { TuneSched = v }(TuneSched)
	TuneSched = 0
	l := newAngleLink(t, angleRateBps, angleDelay)
	if l.edge.HasSched() || l.exit.HasSched() {
		t.Fatal("a scheduler with HS2_TUNE_SCHED=0")
	}
	if cl := coreLinks.of(l.edge); cl != nil && cl.flows != nil {
		t.Fatal("C1's stream table with HS2_TUNE_SCHED=0")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := l.probe(ctx, 5)
	var down atomic.Uint64
	l.download(ctx, 5, 2, &down)
	time.Sleep(time.Second)
	p.on()
	r := angleRate(3*time.Second, &down)
	p50, n := p.pct(0.5)
	t.Logf("old path: down %.2f Mbit/s, probe p50 %v (%d)", r[0]*8/1e6, p50, n)
	if r[0] < 0.5*angleRateBps || n == 0 {
		t.Fatalf("old path carried %.2f Mbit/s, %d probes", r[0]*8/1e6, n)
	}
	st, err := l.edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tl := newStreamL3Link(st, func() bool { return false }, nil)
	if tl.sq != nil {
		t.Fatal("the TUN's per-flow queues with HS2_TUNE_SCHED=0")
	}
	var s l3Set
	a, b := &l3Link{id: 1}, &l3Link{id: 2}
	s.add(a)
	s.add(b)
	if s.linkFor(12345) != s.pickFlow(12345) || len(s.pins) != 0 {
		t.Fatal("pinned flows with HS2_TUNE_SCHED=0")
	}
}

// The writer of a user's 65th data frame waits (blocked-writers), the user
// never has more than 64 frames queued, and another user's probe and the
// link's control frames do not wait for it. (100 Mbit/s: its 100 streams
// are bulk each; at 10 Mbit/s each would be light, C1.)
func TestAngleBackpressure64(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link")
	}
	l := newAngleLink(t, 10*angleRateBps, angleDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := l.probe(ctx, 9)
	up := l.upload(ctx, 1, 100) // 100 streams of one user, each a frame at a time
	time.Sleep(3 * time.Second)
	p.on()
	most, waited := 0, uint64(0)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := l.edge.SchedStats(); ok {
			waited += st.Blocked
			if st.MostPendingUser == 1 {
				most = max(most, st.MostPending)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	// a stream opened now: its SYN (control) goes ahead of the queued data
	t0 := time.Now()
	st := l.open(9, 'E')
	opened := time.Since(t0)
	st.Close()
	p99, n := p.pct(0.99)
	t.Logf("user's most queued %d, writers that waited %d, other user's probe p99 %v (%d), open %v, upload %.2f Mbit/s",
		most, waited, p99, n, opened, float64(up.Load())*8/1e6/7)
	if most != schedUserFrames {
		t.Fatalf("the user had at most %d frames queued, want %d", most, schedUserFrames)
	}
	if waited == 0 {
		t.Fatal("no writer waited for its user's room")
	}
	if p99 > angleBound {
		t.Fatalf("the other user's probe p99 %v, want <= %v", p99, angleBound)
	}
	if opened > 30*time.Millisecond {
		t.Fatalf("opening a stream took %v behind the queued data", opened)
	}
}

// A control frame is never behind data: on a link whose edge has 16 uploads
// queued (16 bulk frames, ~80 ms of it at 10 Mbit/s) and receives 16
// downloads (its window updates), a new stream's SYN leaves after the frame
// being written and the socket's few unsent KiB, not after the queue.
func TestAngleCtrlNeverBehindData(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link")
	}
	l := newAngleLink(t, angleRateBps, angleDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var down atomic.Uint64
	l.download(ctx, 1, 16, &down)
	up := l.upload(ctx, 1, 16)
	time.Sleep(3 * time.Second)
	var worst time.Duration
	for i := 0; i < 30; i++ {
		t0 := time.Now()
		st, err := l.edge.OpenStreamSyn(smux.EncodeSyn(2, 0)) // returns once its SYN is written
		if err != nil {
			t.Fatal(err)
		}
		worst = max(worst, time.Since(t0))
		st.Close()
		time.Sleep(30 * time.Millisecond)
	}
	r := angleRate(2*time.Second, &down, up)
	t.Logf("SYN out within %v at worst, under 16 downloads (%.2f Mbit/s) and 16 uploads (%.2f Mbit/s)", worst, r[0]*8/1e6, r[1]*8/1e6)
	if worst > 15*time.Millisecond {
		t.Fatalf("a SYN took %v to leave, want <= 15 ms (a 5 ms frame and the socket's unsent)", worst)
	}
}

// angleCores skips a test of the new path when a kill switch from the
// environment (TestMain) keeps the old one: TestAngleSchedOff is that one's.
func angleCores(t *testing.T) {
	t.Helper()
	if SchedOff() || HostQOff() || ClassOff() {
		t.Skip("a kill switch is set: the old path")
	}
}
