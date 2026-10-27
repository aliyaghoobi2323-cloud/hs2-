package engine

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// β2's tests (quiet.go): a QUIET and a BULK link over emulated links that
// share one bottleneck each way, the edge's pool and mover as RunIran has
// them, an exit that serves its streams as serveStream does.

// classPair is one emulated link of a class: the edge's mtcpLink and the
// exit's session.
type classPair struct {
	edge *mtcpLink
	exit *smux.Session
	xmtr *linkMeter
}

// newClassPair builds a link of caps over wires up and down.
func newClassPair(t *testing.T, up, down *angleWire, caps uint32) *classPair {
	t.Helper()
	emtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	es, _, err := newSessionCaps(&angleEnd{in: down, out: up}, false, nil, emtr, caps)
	if err != nil {
		t.Fatal(err)
	}
	xmtr := &linkMeter{}
	xs, _, err := newSessionCaps(&angleEnd{in: up, out: down}, true, nil, xmtr, caps)
	if err != nil {
		t.Fatal(err)
	}
	l := &mtcpLink{sess: es, mtr: emtr}
	l.setCaps(caps)
	t.Cleanup(func() { es.Close(); xs.Close() })
	return &classPair{edge: l, exit: xs, xmtr: xmtr}
}

// quietRig is an edge pool of one QUIET and one BULK link of rate each way,
// and its exit. fifo: the two share one drop-tail bottleneck each way (a
// QUIET byte leaves after the BULK bytes before it); else each has its own
// departure clock, as fq_codel serves a sparse flow next to a backlogged
// one.
type quietRig struct {
	t      *testing.T
	lm     *LinkManager
	q      *quietSet
	quiet  *classPair
	bulk   *classPair
	wires  []*angleWire
	upRx   sync.Map // test port -> *atomic.Uint64 (what the exit read of uploads)
	cancel context.CancelFunc
	ctx    context.Context
}

const (
	qpDown  = 1 // the exit writes for good
	qpUp    = 2 // the exit reads into upRx
	qpEcho  = 3 // the exit echoes
	qpUp2   = 4 // as qpUp, another counter
	qpTotal = 5
)

func classCaps(class uint32) uint32 {
	return tlscarrier.LocalAuthHdr().Caps | class
}

func newQuietRig(t *testing.T, rate float64, delay time.Duration, fifo bool) *quietRig {
	t.Helper()
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	r := &quietRig{t: t}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	t.Cleanup(r.cancel)
	clkUp, clkDn := &wireClock{}, &wireClock{}
	mk := func() (*angleWire, *angleWire) {
		up, dn := newAngleWire(rate, delay), newAngleWire(rate, delay)
		if fifo {
			up.clk, dn.clk = clkUp, clkDn
		}
		r.wires = append(r.wires, up, dn)
		return up, dn
	}
	qu, qd := mk()
	bu, bd := mk()
	r.quiet = newClassPair(t, qu, qd, classCaps(tlscarrier.CapQuiet))
	r.bulk = newClassPair(t, bu, bd, classCaps(tlscarrier.CapBulk))
	t.Cleanup(func() {
		for _, w := range r.wires {
			w.close()
		}
	})
	if !r.quiet.edge.quiet || !r.bulk.edge.bulk {
		t.Fatalf("classes: quiet link %v, bulk link %v", r.quiet.edge.quiet, r.bulk.edge.bulk)
	}
	r.lm = NewLinkManager(nil, 1, 1, 8, t.Logf)
	r.q = r.lm.enableQuiet()
	r.q.place(r.quiet.edge, "")
	addManaged(r.lm, r.bulk.edge)
	go r.q.runMover(r.ctx)
	for _, p := range []*classPair{r.quiet, r.bulk} {
		go r.serve(p)
	}
	return r
}

// serve is the exit's side of link p, as serveStream for groups, then by
// the test port in the stream's header.
func (r *quietRig) serve(p *classPair) {
	for {
		st, err := p.exit.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			var conn exitStream = st
			if id, role, _ := st.GroupInfo(); id != 0 {
				if role == smux.RoleMove {
					if !exitGroups.attach(st, p.xmtr) {
						st.Close()
					}
					return
				}
				eg := exitGroups.add(st, p.xmtr)
				if eg == nil {
					st.Close()
					return
				}
				defer exitGroups.remove(eg)
				conn = eg
			}
			defer conn.Close()
			var h [3]byte
			if _, err := io.ReadFull(conn, h[:]); err != nil || h[0] != kindTCPPort {
				return
			}
			port := int(h[1])<<8 | int(h[2])
			b := make([]byte, 32<<10)
			switch port {
			case qpDown:
				for {
					if _, err := conn.Write(b); err != nil {
						return
					}
				}
			case qpUp, qpUp2:
				c, _ := r.upRx.LoadOrStore(port, new(atomic.Uint64))
				for {
					n, err := conn.Read(b)
					c.(*atomic.Uint64).Add(uint64(n))
					if err != nil {
						return
					}
				}
			case qpEcho:
				io.Copy(conn, conn)
			}
		}()
	}
}

// open opens a user connection of user tag to the test port, as a user's
// connection to a user port is opened (openStreamOn).
func (r *quietRig) open(tag uint32, port int) stream {
	r.t.Helper()
	ctx := context.WithValue(r.ctx, userTagKey{}, tag)
	st, _, _, ok := openStreamOn(ctx, r.lm, false, port)
	if !ok {
		r.t.Fatal("no stream opened")
	}
	return st
}

func (r *quietRig) upCount(port int) *atomic.Uint64 {
	c, _ := r.upRx.LoadOrStore(port, new(atomic.Uint64))
	return c.(*atomic.Uint64)
}

// laneFrames is the latency-lane frames a session's scheduler took since
// the last call.
func laneFrames(s *smux.Session) uint64 {
	st, _ := s.SchedStats()
	return st.LaneN
}

// The probe of §5.1 on a QUIET link next to 4 downloads and 2 uploads on a
// 10 Mbit/s pipe each way, the bottleneck as fq_codel serves a sparse flow
// (the round-2 diagnosis: +1 to +4 ms on a TCP connection of its own):
// every bulk connection opens on the QUIET link and moves once to the BULK
// link within its first second or so; the probe stays; its p99 is within
// RTT + 20 ms; the pipe stays full both ways; and no latency-lane frame of
// a user goes on the BULK link (control and the lane on QUIET). On a
// shared drop-tail bottleneck the probe waits behind the BULK link's queue
// besides: what α holds to 5 ms on a real socket (the emulated one has no
// TCP_INFO), here the host-queue guard's mark; logged.
func TestQuietProbeOnQuietLink(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links, 30 s")
	}
	quietProbe(t, false)
	t.Run("fifo", func(t *testing.T) { quietProbe(t, true) })
}

func quietProbe(t *testing.T, fifo bool) {
	r := newQuietRig(t, angleRateBps, angleDelay, fifo)
	movedAt := exitGroups.attached.Load()
	var down atomic.Uint64
	for i := 0; i < 4; i++ {
		st := r.open(7, qpDown)
		go func() {
			defer st.Close()
			b := make([]byte, 32<<10)
			for r.ctx.Err() == nil {
				n, err := st.Read(b)
				down.Add(uint64(n))
				if err != nil {
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		st := r.open(7, qpUp)
		go func() {
			defer st.Close()
			b := make([]byte, 32<<10)
			for r.ctx.Err() == nil {
				if _, err := st.Write(b); err != nil {
					return
				}
			}
		}()
	}
	p := &angleProbe{from: time.Now().Add(time.Hour)}
	pst := r.open(7, qpEcho)
	go func() {
		defer pst.Close()
		b := make([]byte, 32)
		tk := time.NewTicker(200 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-tk.C:
			}
			t0 := time.Now()
			binary.BigEndian.PutUint64(b, uint64(t0.UnixNano()))
			if _, err := pst.Write(b); err != nil {
				return
			}
			if _, err := io.ReadFull(pst, b); err != nil {
				return
			}
			p.mu.Lock()
			if t0.After(p.from) {
				p.rtts = append(p.rtts, time.Since(t0))
			}
			p.mu.Unlock()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.q.moved.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := r.q.moved.Load(); n != 6 {
		t.Fatalf("%d of the 6 bulk connections moved to the BULK link (%s)", n, r.q.line())
	}
	// their move streams reach the exit a link's delay later
	waitQuiet(t, "the exit attached the 6 move streams", func() bool { return exitGroups.attached.Load()-movedAt == 6 })
	if gs := pst.(*groupStream); gs.g.Moved() != nil {
		t.Fatal("the probe moved")
	}
	time.Sleep(time.Second)
	laneFrames(r.bulk.edge.sess)
	laneFrames(r.bulk.exit)
	laneFrames(r.quiet.edge.sess)
	p.on()
	rates := angleRate(10*time.Second, &down, r.upCount(qpUp))
	p99, n := p.pct(0.99)
	p50, _ := p.pct(0.5)
	bulkLane := laneFrames(r.bulk.edge.sess) + laneFrames(r.bulk.exit)
	quietLane := laneFrames(r.quiet.edge.sess)
	t.Logf("probe on QUIET: p50 %v p99 %v (%d); down %.2f up %.2f Mbit/s; latency-lane frames on BULK %d, on QUIET %d; %s",
		p50, p99, n, rates[0]*8/1e6, rates[1]*8/1e6, bulkLane, quietLane, r.q.line())
	if n < 40 {
		t.Fatalf("only %d probes", n)
	}
	if !fifo && p99 > angleBound {
		t.Fatalf("probe p99 %v, want <= %v (RTT %v + 20 ms)", p99, angleBound, angleRTT)
	}
	for i, x := range rates {
		if x < 0.95*angleRateBps {
			t.Fatalf("direction %d carried %.2f Mbit/s of 10", i, x*8/1e6)
		}
	}
	if bulkLane != 0 {
		t.Fatalf("%d latency-lane frames on the BULK link", bulkLane)
	}
	if quietLane == 0 {
		t.Fatal("no latency-lane frame on the QUIET link: the probe went elsewhere")
	}
}

// A connection moves once: a moved one is never ticked again, its main
// stream is let go at both ends, and its bytes arrive in order and whole
// both ways across the switch (an echo of 4 MiB checked byte by byte, the
// QUIET link stalling 300 ms just as it moves: its tail arrives after the
// BULK link's first frames).
func TestQuietMoveOnceInOrder(t *testing.T) {
	angleCores(t)
	r := newQuietRig(t, 4*angleRateBps, 10*time.Millisecond, true)
	st := r.open(9, qpEcho)
	gs := st.(*groupStream)
	const total = 4 << 20
	errs := make(chan error, 2)
	var got atomic.Int64
	go func() {
		b := make([]byte, 7000)
		var off int64
		for off < total {
			n, err := st.Read(b)
			for i := 0; i < n; i++ {
				if b[i] != byte((off+int64(i))*13+(off+int64(i))>>9) {
					errs <- io.ErrUnexpectedEOF
					return
				}
			}
			off += int64(n)
			got.Store(off)
			if err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	stalled := false
	b := make([]byte, 16<<10)
	var off int64
	for off < total {
		for i := range b {
			b[i] = byte((off+int64(i))*13 + (off+int64(i))>>9)
		}
		if _, err := st.Write(b); err != nil {
			t.Fatal(err)
		}
		off += int64(len(b))
		if !stalled && r.q.moved.Load() == 0 && off >= 256<<10 {
			// stall the QUIET link's wires around the move
			for _, w := range r.wires[:2] {
				w.mu.Lock()
				w.holdTill = time.Now().Add(300 * time.Millisecond)
				w.mu.Unlock()
			}
			stalled = true
		}
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("read %d of %d: %v", got.Load(), total, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("read %d of %d", got.Load(), total)
	}
	if gs.g.Moved() == nil || r.q.moved.Load() != 1 {
		t.Fatalf("moved %v (%d moves)", gs.g.Moved() != nil, r.q.moved.Load())
	}
	// the main stream is let go at both ends once both moved
	waitQuiet(t, "the main stream let go", func() bool {
		return r.quiet.edge.sess.NumStreams() == 0 && r.quiet.exit.NumStreams() == 0
	})
	s1 := gs.g.Moved()
	r.q.move(gs, "up") // the group refuses: done with it, never moved again
	gs.mu.Lock()
	noMove := gs.noMove
	gs.mu.Unlock()
	if r.q.moved.Load() != 1 || gs.g.Moved() != s1 || !noMove {
		t.Fatalf("a second move: %d moves, move stream changed %v, given up %v", r.q.moved.Load(), gs.g.Moved() != s1, noMove)
	}
	st.Close()
	if r.bulk.edge.Active() != 0 || r.quiet.edge.Active() != 0 {
		t.Fatalf("counted streams left: bulk %d, quiet %d", r.bulk.edge.Active(), r.quiet.edge.Active())
	}
}

func waitQuiet(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Light and interactive connections never move: a probe, and bursts of
// 64 KiB with a pause between (no send backlog); a connection that sends
// 64 KiB in a backlog does.
func TestMoveRule(t *testing.T) {
	tick := int64(moveTick)
	run := func(tx, rx []uint64, busy []float64, txBusy bool) (bool, int) {
		var r moveRule
		var cTx, cRx uint64
		var cBusy time.Duration
		now := int64(time.Hour)
		r.tick(0, 0, 0, now, txBusy)
		for i := range tx {
			now += tick
			cTx += tx[i]
			cRx += rx[i]
			cBusy += time.Duration(busy[i] * float64(moveTick))
			if due, _ := r.tick(cTx, cRx, cBusy, now, txBusy); due {
				return true, i
			}
		}
		return false, 0
	}
	rep := func(n int, v uint64) []uint64 {
		out := make([]uint64, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	busyAll := func(n int, f float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = f
		}
		return out
	}
	// an upload with a backlog: 40 KiB a tick, its writer always waiting
	if due, at := run(rep(10, 40<<10), rep(10, 0), busyAll(10, 1), true); !due || at > 2 {
		t.Fatalf("an upload with a backlog: due %v at tick %d", due, at)
	}
	// a burst of 64 KiB written at once and gone, every 3 s: never
	burst := make([]uint64, 60)
	bb := make([]float64, 60)
	for i := 0; i < 60; i += 30 {
		burst[i], bb[i] = 64<<10, 0.3
	}
	if due, _ := run(burst, rep(60, 0), bb, true); due {
		t.Fatal("an interactive burst moved")
	}
	// a page load: 200 KiB in 2 ticks downward, then nothing
	pg := make([]uint64, 30)
	pg[0], pg[1] = 100<<10, 100<<10
	if due, _ := run(rep(30, 200), pg, busyAll(30, 0), true); due {
		t.Fatal("a page load moved")
	}
	// a download: 30 KiB a tick downward
	if due, at := run(rep(10, 300), rep(10, 30<<10), busyAll(10, 0), true); !due || at > 3 {
		t.Fatalf("a download: due %v at tick %d", due, at)
	}
	// the probe: 32 B each 2 ticks both ways
	pr := make([]uint64, 60)
	for i := range pr {
		if i%2 == 0 {
			pr[i] = 32
		}
	}
	if due, _ := run(pr, pr, busyAll(60, 0.01), true); due {
		t.Fatal("the probe moved")
	}
	// a TUN flow (no writer to watch) sending 30 KiB a tick: due by bytes
	if due, at := run(rep(10, 30<<10), rep(10, 0), busyAll(10, 0), false); !due || at > 3 {
		t.Fatalf("a TUN upload: due %v at tick %d", due, at)
	}
}

// QUIET links count toward max_links, at least one BULK link: N=1 runs one
// QUIET and one BULK link, N=8 two and six; a level 1 peer turns the
// classes off and the pool's ceiling is max_links again.
func TestQuietCounts(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	for _, c := range []struct{ max, q, bulk, total int }{
		{1, 1, 1, 2}, {2, 1, 1, 2}, {3, 1, 2, 3}, {4, 2, 2, 4}, {8, 2, 6, 8}, {300, 2, 298, 300},
	} {
		lm := NewLinkManager(nil, 1, c.max, 8, nil)
		q := lm.enableQuiet()
		if q.want != c.q || lm.bulkMax() != c.bulk || QuietTotal(c.max) != c.total || lm.ctlTarget()-lm.Target() != c.q {
			t.Fatalf("max_links %d: QUIET %d, BULK max %d, total %d, asked of the exit past the target %d; want %d %d %d",
				c.max, q.want, lm.bulkMax(), QuietTotal(c.max), lm.ctlTarget()-lm.Target(), c.q, c.bulk, c.total)
		}
		q.noteCaps(tlscarrier.CapSched) // a link of a level 1 peer
		if lm.bulkMax() != c.max || lm.quietWant() != 0 || lm.ctlTarget() != lm.Target() {
			t.Fatalf("max_links %d with a level 1 peer: BULK max %d, QUIET %d", c.max, lm.bulkMax(), lm.quietWant())
		}
		if _, _, ok := q.pick(nil); ok {
			t.Fatal("a QUIET pick with a level 1 peer")
		}
	}
}

// α (sendcap.go) on a BULK link's socket: its cap is a factor of the
// largest delivery of the last 10 s, which grows under 5 ms of queue (to
// 1.25), holds from 5 to 8 ms, and backs off past 8 ms in proportion to the
// excess (at most 0.1 a reading), never under 0.75: a direction whose
// reading stays past the band (its acks behind the other direction's
// backlog) keeps 0.75 of what it delivered, never starves; never under the
// floor; a QUIET link keeps C4-lite's 1.25 × of 2 s.
func TestAlphaCap(t *testing.T) {
	const ms = time.Millisecond
	feed := func(c *sendCap, n int, at *time.Duration, dlv float64, srtt, minRTT time.Duration, acked *uint64) {
		for i := 0; i < n; i++ {
			*at += 100 * ms
			*acked += uint64(dlv / 10)
			c.Update(capSample{Now: *at, Delivery: dlv, Acked: *acked, Written: *acked, Segs: uint32(*acked / 1000), SRTT: srtt, MinRTT: minRTT})
		}
	}
	const rate = 25e6 / 8
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-6*b+1 }
	{
		c := &sendCap{linked: true, alpha: true}
		at, acked := time.Hour, uint64(0)
		feed(c, 3, &at, rate, 72*ms+2*ms, 72*ms, &acked) // under 5 ms: grows
		if !near(c.aK, 1+2*alphaGrow) || !near(c.aRate, c.aK*rate) || uint64(c.aRate) != c.own {
			t.Fatalf("an empty queue: factor %.3f cap %.0f (own %d), want %.3f × %.0f", c.aK, c.aRate, c.own, 1+2*alphaGrow, rate)
		}
		feed(c, 40, &at, rate, 72*ms+1*ms, 72*ms, &acked)
		if !near(c.aK, alphaKMax) {
			t.Fatalf("a long empty queue: factor %.3f, want the ceiling %.2f", c.aK, alphaKMax)
		}
		feed(c, 1, &at, rate, 72*ms+6*ms, 72*ms, &acked) // in the band: holds
		if !near(c.aK, alphaKMax) {
			t.Fatalf("6 ms of queue: factor %.3f, want it held", c.aK)
		}
		feed(c, 1, &at, rate, 72*ms+12*ms, 72*ms, &acked) // 4 ms over: 0.04 off
		if !near(c.aK, alphaKMax-0.04) {
			t.Fatalf("12 ms of queue: factor %.3f, want %.3f", c.aK, alphaKMax-0.04)
		}
		feed(c, 1, &at, rate, 72*ms+100*ms, 72*ms, &acked) // far over: 0.1 at most
		if !near(c.aK, alphaKMax-0.14) {
			t.Fatalf("100 ms of queue: factor %.3f, want %.3f", c.aK, alphaKMax-0.14)
		}
		// the reading stays past the band while what it delivers falls (its
		// acks behind the other direction): held at 0.75 of the 10 s maximum
		feed(c, 30, &at, rate/2, 72*ms+20*ms, 72*ms, &acked)
		if !near(c.aK, alphaKMin) || !near(c.aRate, alphaKMin*rate) {
			t.Fatalf("a stuck reading: factor %.3f cap %.0f, want %.2f × %.0f (the 10 s maximum)", c.aK, c.aRate, alphaKMin, rate)
		}
		// 10 s on, the maximum is what it delivered since
		feed(c, 110, &at, rate/2, 72*ms+20*ms, 72*ms, &acked)
		if !near(c.aRate, alphaKMin*rate/2) {
			t.Fatalf("10 s later: cap %.0f, want %.0f", c.aRate, alphaKMin*rate/2)
		}
		feed(c, 40, &at, 10e3, 72*ms+300*ms, 72*ms, &acked) // a collapse of the path
		feed(c, 100, &at, 10e3, 72*ms+300*ms, 72*ms, &acked)
		if c.aRate != alphaFloor {
			t.Fatalf("collapse: cap %.0f, want the floor %d", c.aRate, alphaFloor)
		}
		if !hasPrefix(c, "alpha cap ") {
			t.Fatal("status line lacks alpha")
		}
	}
	{
		c := &sendCap{linked: true} // QUIET: C4-lite
		at, acked := time.Hour, uint64(0)
		feed(c, 3, &at, rate, 72*ms+55*ms, 72*ms, &acked)
		if !near(float64(c.own), wcNetKOf()*rate) {
			t.Fatalf("C4-lite: cap %d, want %.0f", c.own, wcNetKOf()*rate)
		}
	}
	{
		c := &sendCap{linked: true, alpha: true} // no min_rtt reading: 1.25 ×
		at, acked := time.Hour, uint64(0)
		feed(c, 3, &at, rate, 0, 0, &acked)
		if !near(c.aRate, alphaKMax*rate) {
			t.Fatalf("no queue reading: cap %.0f, want %.0f", c.aRate, alphaKMax*rate)
		}
	}
}

func hasPrefix(c *sendCap, s string) bool {
	l := &sendCapLink{core: *c}
	line := l.line(1)
	return len(line) > len("sendcap link 1: ") && line[len("sendcap link 1: "):][:len(s)] == s
}

// The TUN with classes: a new flow and a ping go on a QUIET link; a flow
// that moves bulk (here downward: what the exit's frames bring back) moves
// once to a BULK link, and only once; the exit's replies go where the
// flow's packets arrive, under the key of the edge's side (flowKeyRev).
func TestQuietTunPlacement(t *testing.T) {
	angleCores(t)
	edge := &l3Set{}
	q1, q2 := &l3Link{id: 1, class: l3Quiet}, &l3Link{id: 2, class: l3Quiet}
	b1, b2 := &l3Link{id: 3, class: l3Bulk}, &l3Link{id: 4, class: l3Bulk}
	for _, l := range []*l3Link{q1, q2, b1, b2} {
		edge.add(l)
	}
	req := udpPacketN(5, 40000, 1200)
	h, _, _ := flowKey(req, false)
	if l := edge.linkForPkt(h, len(req), false); l.class != l3Quiet {
		t.Fatalf("a new flow on class %d", l.class)
	}
	ping := echoPacket(5, 1)
	ph, _, _ := flowKey(ping, false)
	// the reply of the download: what the exit's frames bring back
	reply := append([]byte(nil), req...)
	copy(reply[12:16], req[16:20])
	copy(reply[16:20], req[12:16])
	binary.BigEndian.PutUint16(reply[20:], 5001)
	binary.BigEndian.PutUint16(reply[22:], 40000)
	if rk, _, _ := flowKeyRev(reply, false); rk != h {
		t.Fatalf("flowKeyRev of the reply %x, the request's key %x", rk, h)
	}
	moved := 0
	var last *l3Link
	for i := 0; i < 60; i++ {
		for k := 0; k < 30; k++ {
			edge.noteRx(h, len(reply)) // 36 KiB a tick down
		}
		l := edge.linkForPkt(h, 60, false) // its acks
		if lp := edge.linkForPkt(ph, 64, true); lp.class != l3Quiet {
			t.Fatal("a ping left QUIET")
		}
		if last != nil && l != last {
			moved++
		}
		last = l
		time.Sleep(moveTick)
		if moved > 0 && i > 20 {
			break
		}
	}
	if moved != 1 || last.class != l3Bulk || edge.moves.Load() != 1 {
		t.Fatalf("the download moved %d times, now on class %d (%d moves)", moved, last.class, edge.moves.Load())
	}
	// the exit: replies where the flow's packets arrive
	exit := &l3Set{toUser: true}
	xq, xb := &l3Link{id: 11, class: l3Quiet}, &l3Link{id: 12, class: l3Bulk}
	exit.add(xq)
	exit.add(xb)
	exit.arrive(h, xq)
	rk, _, _ := flowKeyRev(reply, true)
	if exit.arrival(rk) != xq {
		t.Fatal("the reply does not follow the request's arrival")
	}
	exit.arrive(h, xb)
	if exit.arrival(rk) != xb {
		t.Fatal("the reply does not follow the flow's move")
	}
	if !exit.arrivals() || exit.arrival(rk+1) != nil {
		t.Fatal("an arrival for a flow that never arrived")
	}
}

// flowKeyRev is flowKey of the packet with its addresses and ports
// swapped, for any packet.
func FuzzFlowKeyRev(f *testing.F) {
	f.Add(tcpPacket(40000, 443))
	f.Add(udpPacketN(3, 1234, 100))
	f.Add(echoPacket(2, 7))
	f.Add(ipv6Packet("2001:db8::1", "2001:db8::2", 6, 1, 2))
	f.Add(ipv6Packet("2001:db8::1", "2001:db8::2", 17, 1, 2, extHdr{0, 8}, extHdr{60, 8}))
	f.Fuzz(func(t *testing.T, pkt []byte) {
		pkt = pkt[:len(pkt):len(pkt)]
		sw := swapEnds(pkt)
		if sw == nil {
			return
		}
		a, ua, oka := flowKeyRev(sw, true)
		b, ub, okb := flowKey(pkt, false)
		if a != b || oka != okb || ua != ub {
			t.Fatalf("rev of the swapped packet %x/%v, the packet's key %x/%v", a, ua, b, ub)
		}
	})
}

// swapEnds is pkt with its addresses (and TCP/UDP ports) swapped (nil: not
// an IP packet flowKey reads ports of).
func swapEnds(pkt []byte) []byte {
	p := append([]byte(nil), pkt...)
	switch {
	case len(p) >= 20 && p[0]>>4 == 4:
		ihl := int(p[0]&0x0f) * 4
		if ihl < 20 {
			return nil // its "ports" overlap the header
		}
		var a [4]byte
		copy(a[:], p[12:16])
		copy(p[12:16], p[16:20])
		copy(p[16:20], a[:])
		if (p[9] == 6 || p[9] == 17) && len(p) >= ihl+4 {
			p[ihl], p[ihl+1], p[ihl+2], p[ihl+3] = p[ihl+2], p[ihl+3], p[ihl], p[ihl+1]
		}
		return p
	case len(p) >= 40 && p[0]>>4 == 6:
		var a [16]byte
		copy(a[:], p[8:24])
		copy(p[8:24], p[24:40])
		copy(p[24:40], a[:])
		proto, l4 := ipv6Transport(p)
		if (proto == 6 || proto == 17) && l4 > 0 && len(p) >= l4+4 {
			p[l4], p[l4+1], p[l4+2], p[l4+3] = p[l4+2], p[l4+3], p[l4], p[l4+1]
		}
		return p
	}
	return nil
}

// P10 (b) on the throttled pipe, the fix's two variants side by side: the
// user of one stream next to one of 16 gets >= 0.9 of its floor (half the
// link) with the early Done; without it (HS2_TUNE_EARLYDONE=0) the number
// is logged.
func TestEarlyDoneOneVsSixteen(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link, 20 s")
	}
	const c = angleRateBps
	share := func(early int) float64 {
		defer func(v int) { TuneEarlyDone = v }(TuneEarlyDone)
		TuneEarlyDone = early
		l := newAngleLink(t, c, angleDelay)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var a, b atomic.Uint64
		l.download(ctx, 1, 1, &a)
		l.download(ctx, 2, 16, &b)
		time.Sleep(3 * time.Second)
		r := angleRate(6*time.Second, &a, &b)
		t.Logf("early Done %d: 1 stream %.2f, 16 streams %.2f, total %.2f Mbit/s: %.3f of its floor",
			early, r[0]*8/1e6, r[1]*8/1e6, (r[0]+r[1])*8/1e6, r[0]/(c/2))
		if r[0]+r[1] < 0.97*c {
			t.Errorf("early Done %d: the two took %.3f of the link, want >= 0.97", early, (r[0]+r[1])/c)
		}
		return r[0] / (c / 2)
	}
	off := share(0)
	on := share(1)
	t.Logf("the user of one stream: %.3f of its floor with the early Done, %.3f without", on, off)
	if on < 0.9 {
		t.Fatalf("with the early Done the user of one stream got %.3f of its floor, want >= 0.9", on)
	}
}
