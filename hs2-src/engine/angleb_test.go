package engine

import (
	"encoding/binary"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The integration tests of angle (b) (R1 task 16): the link scheduler in
// the engine (linksched.go), smux only a framer, between two real sessions
// (newSession) over a modelled link.

// pipeDir is one direction of a modelled link: a bottleneck of rate
// bytes/s and a one-way delay. A write is held until its bytes are all but
// pipeHostQ from leaving the bottleneck: the sender's socket holds no more
// than that (C3's bound, 2 ms of the rate), so what waits, waits in the
// sender's scheduler.
type pipeDir struct {
	mu     sync.Mutex
	cond   *sync.Cond
	rate   float64
	delay  time.Duration
	busy   time.Time
	q      []pipeChunk
	closed bool
	sent   atomic.Int64
}

type pipeChunk struct {
	at time.Time
	b  []byte
}

const pipeHostQ = 2 * time.Millisecond

func newPipeDir(rate float64, delay time.Duration) *pipeDir {
	d := &pipeDir{rate: rate, delay: delay}
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *pipeDir) write(p []byte) (int, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	now := time.Now()
	start := d.busy
	if start.Before(now) {
		start = now
	}
	done := start.Add(time.Duration(float64(len(p)) / d.rate * float64(time.Second)))
	d.busy = done
	d.q = append(d.q, pipeChunk{done.Add(d.delay), append([]byte(nil), p...)})
	d.cond.Broadcast()
	d.mu.Unlock()
	d.sent.Add(int64(len(p)))
	time.Sleep(time.Until(done.Add(-pipeHostQ)))
	return len(p), nil
}

func (d *pipeDir) read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if len(d.q) > 0 {
			c := &d.q[0]
			if wait := time.Until(c.at); wait > 0 {
				d.mu.Unlock()
				time.Sleep(wait)
				d.mu.Lock()
				continue
			}
			n := copy(p, c.b)
			c.b = c.b[n:]
			if len(c.b) == 0 {
				d.q = d.q[1:]
			}
			return n, nil
		}
		if d.closed {
			return 0, io.EOF
		}
		d.cond.Wait()
	}
}

func (d *pipeDir) close() {
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// pipeConn is one end of a modelled link.
type pipeConn struct {
	in, out *pipeDir
}

func (c *pipeConn) Read(p []byte) (int, error)         { return c.in.read(p) }
func (c *pipeConn) Write(p []byte) (int, error)        { return c.out.write(p) }
func (c *pipeConn) Close() error                       { c.in.close(); c.out.close(); return nil }
func (c *pipeConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

// linkPipe is a modelled link of rate bytes/s each way and a one-way delay.
func linkPipe(rate float64, delay time.Duration) (a, b *pipeConn) {
	ab, ba := newPipeDir(rate, delay), newPipeDir(rate, delay)
	return &pipeConn{in: ba, out: ab}, &pipeConn{in: ab, out: ba}
}

// withSched runs f with C2 on or off (HS2_TUNE_SCHED) as sessions are made.
func withSched(on bool, f func()) {
	old := TuneSched
	TuneSched = b2i(on)
	defer func() { TuneSched = old }()
	f()
}

// abRig is an edge and an exit session over a modelled link, the exit
// serving the streams the edge opens: 'D' downloads (the exit writes for
// ever), 'U' uploads (the exit reads), 'P' an echo (a probe). Bytes are
// counted per user tag at the end that reads them.
type abRig struct {
	t          *testing.T
	a, b       *pipeConn
	edge, exit *smux.Session
	mu         sync.Mutex
	down, up   map[uint32]*atomic.Int64
}

func newABRig(t *testing.T, rate float64, delay time.Duration, sched bool) *abRig {
	t.Helper()
	r := &abRig{t: t, down: map[uint32]*atomic.Int64{}, up: map[uint32]*atomic.Int64{}}
	r.a, r.b = linkPipe(rate, delay)
	var err error
	withSched(sched, func() {
		if r.edge, _, err = newSession(r.a, false, nil, &linkMeter{statsPoll: make(chan struct{}, 1)}); err != nil {
			return
		}
		r.exit, _, err = newSession(r.b, true, nil, &linkMeter{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if (r.edge.Queue() != nil) != sched || (r.exit.Queue() != nil) != sched {
		t.Fatalf("sched %v: the sessions' queues %v %v", sched, r.edge.Queue(), r.exit.Queue())
	}
	go r.serve()
	return r
}

func (r *abRig) close() {
	r.edge.Close()
	r.exit.Close()
	r.a.Close()
	r.b.Close()
}

func (r *abRig) counter(m map[uint32]*atomic.Int64, tag uint32) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := m[tag]
	if c == nil {
		c = new(atomic.Int64)
		m[tag] = c
	}
	return c
}

func (r *abRig) serve() {
	for {
		st, err := r.exit.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			defer st.Close()
			var role [1]byte
			if _, err := io.ReadFull(st, role[:]); err != nil {
				return
			}
			tag, _ := st.Syn()
			switch role[0] {
			case 'D':
				b := make([]byte, 32<<10)
				for {
					if _, err := st.Write(b); err != nil {
						return
					}
				}
			case 'U':
				c := r.counter(r.up, tag)
				b := make([]byte, 32<<10)
				for {
					n, err := st.Read(b)
					c.Add(int64(n))
					if err != nil {
						return
					}
				}
			case 'P':
				var b [32]byte
				for {
					if _, err := io.ReadFull(st, b[:]); err != nil {
						return
					}
					if _, err := st.Write(b[:]); err != nil {
						return
					}
				}
			}
		}()
	}
}

func (r *abRig) open(tag uint32, role byte) *smux.Stream {
	st, err := r.edge.OpenStreamSyn(smux.EncodeSyn(tag, 0))
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := st.Write([]byte{role}); err != nil {
		r.t.Fatal(err)
	}
	return st
}

// download opens n downloads of user tag.
func (r *abRig) download(tag uint32, n int) {
	c := r.counter(r.down, tag)
	for range n {
		st := r.open(tag, 'D')
		go func() {
			b := make([]byte, 32<<10)
			for {
				n, err := st.Read(b)
				c.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
	}
}

// upload opens n uploads of user tag.
func (r *abRig) upload(tag uint32, n int) {
	for range n {
		st := r.open(tag, 'U')
		go func() {
			b := make([]byte, 32<<10)
			for {
				if _, err := st.Write(b); err != nil {
					return
				}
			}
		}()
	}
}

// abProbe is a 32-byte echo every gap: its round trips, by when sent.
type abProbe struct {
	mu   sync.Mutex
	at   []time.Time
	rtts []time.Duration
}

func (r *abRig) probe(tag uint32, gap time.Duration) *abProbe {
	p := &abProbe{}
	st := r.open(tag, 'P')
	go func() {
		var b [32]byte
		next := time.Now()
		for {
			t0 := time.Now()
			binary.BigEndian.PutUint64(b[:], uint64(t0.UnixNano()))
			if _, err := st.Write(b[:]); err != nil {
				return
			}
			if _, err := io.ReadFull(st, b[:]); err != nil {
				return
			}
			p.mu.Lock()
			p.at = append(p.at, t0)
			p.rtts = append(p.rtts, time.Since(t0))
			p.mu.Unlock()
			next = next.Add(gap)
			time.Sleep(time.Until(next))
		}
	}()
	return p
}

// since: the round trips of the probes sent in [a, b).
func (p *abProbe) within(a, b time.Time) []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []time.Duration
	for i, at := range p.at {
		if !at.Before(a) && at.Before(b) {
			out = append(out, p.rtts[i])
		}
	}
	return out
}

func abPct(v []time.Duration, q int) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)*q/100, len(s)-1)]
}

func minDur(v []time.Duration) time.Duration {
	m := time.Duration(1<<63 - 1)
	for _, x := range v {
		m = min(m, x)
	}
	return m
}

// rateOver samples c over d: bytes/s.
func rateOver(c *atomic.Int64, d time.Duration) float64 {
	a := c.Load()
	time.Sleep(d)
	return float64(c.Load()-a) / d.Seconds()
}

const (
	abRate  = 10e6 / 8 // the modelled link: 10 Mbit/s each way
	abDelay = 20 * time.Millisecond
	abBound = 20 * time.Millisecond // RTT_floor + max(20 ms, 3 MTU/rate) at 10 Mbit/s (§5.1)
)

// TestAngleBSameLinkProbe (TestAngleSameLinkProbe of the plan): two
// sessions over a link of 10 Mbit/s each way and 40 ms of round trip,
// carrying 4 downloads and 2 uploads of one user, and a 32-byte probe every
// 200 ms of that user and of another: the probes' p99 stays within RTT +
// 20 ms. The old writer (heap and FIFO, HS2_TUNE_SCHED=0) over the same
// link is measured for comparison only.
func TestAngleBSameLinkProbe(t *testing.T) {
	for _, sched := range []bool{true, false} {
		r := newABRig(t, abRate, abDelay, sched)
		own, other := r.probe(1, 200*time.Millisecond), r.probe(9, 200*time.Millisecond)
		t0 := time.Now()
		time.Sleep(1200 * time.Millisecond)
		floor := min(minDur(own.within(t0, time.Now())), minDur(other.within(t0, time.Now())))
		r.download(1, 4)
		r.upload(1, 2)
		time.Sleep(2 * time.Second)
		a := time.Now()
		dn := rateOver(r.counter(r.down, 1), 8*time.Second)
		b := time.Now()
		p99o, p99x := abPct(own.within(a, b), 99), abPct(other.within(a, b), 99)
		up := float64(r.counter(r.up, 1).Load())
		r.close()
		t.Logf("sched %v: RTT floor %v; probe p99 %v (the bulk user's own), %v (another user), %d probes each; downloads %.2f Mbit/s, uploads %.2f Mbit/s overall",
			sched, floor.Round(100*time.Microsecond), p99o.Round(100*time.Microsecond), p99x.Round(100*time.Microsecond),
			len(own.within(a, b)), dn*8/1e6, up*8/1e6/b.Sub(t0).Seconds())
		if !sched {
			continue
		}
		if len(own.within(a, b)) < 30 || len(other.within(a, b)) < 30 {
			t.Fatalf("the probes stalled: %d and %d in 8 s", len(own.within(a, b)), len(other.within(a, b)))
		}
		if p99o > floor+abBound || p99x > floor+abBound {
			t.Fatalf("probe p99 %v / %v past RTT floor %v + %v", p99o, p99x, floor, abBound)
		}
		if dn < 0.5*abRate {
			t.Fatalf("the downloads took %.2f Mbit/s of 10", dn*8/1e6)
		}
	}
}

// TestAngleBP10 (TestAngleP10 of the plan): the scheduler shares the link
// by user and wastes none of it (P10, §5.1), over the modelled link at 10
// Mbit/s. The link's capacity C is what one user of 16 downloads takes with
// the old writer (heap and FIFO) on the same link:
//
//   - lone16: one user of 16 downloads takes ≥ 0.95 C;
//   - one_vs_16: a user of one download next to one of 16 takes ≥ 0.9 of
//     its floor, C/2, and the two ≥ 0.97 C;
//   - newheavy: a user of 16 downloads coming 3 s in keeps an existing light
//     user (a probe every 200 ms) within RTT + 20 ms at p99 and an existing
//     user of one download at ≥ 0.9 of its floor C/2 in every second once
//     settled (5 s after the join: the rig's after_settled; the 5 s of the
//     join are logged).
func TestAngleBP10(t *testing.T) {
	const delay = 10 * time.Millisecond
	measure := func(sched bool, setup func(r *abRig), users ...uint32) []float64 {
		r := newABRig(t, abRate, delay, sched)
		defer r.close()
		setup(r)
		time.Sleep(2 * time.Second)
		cs := make([]*atomic.Int64, len(users))
		at := make([]int64, len(users))
		for i, u := range users {
			cs[i] = r.counter(r.down, u)
			at[i] = cs[i].Load()
		}
		const d = 6 * time.Second
		time.Sleep(d)
		out := make([]float64, len(users))
		for i := range users {
			out[i] = float64(cs[i].Load()-at[i]) / d.Seconds()
		}
		return out
	}
	capOld := measure(false, func(r *abRig) { r.download(1, 16) }, 1)[0]
	t.Logf("C (old writer, one user of 16): %.3f Mbit/s, %.1f%% of the link's 10", capOld*8/1e6, 100*capOld/abRate)

	t.Run("lone16", func(t *testing.T) {
		got := measure(true, func(r *abRig) { r.download(1, 16) }, 1)[0]
		t.Logf("one user of 16: %.3f Mbit/s, %.3f C", got*8/1e6, got/capOld)
		if got < 0.95*capOld {
			t.Fatalf("one user of 16 took %.3f C", got/capOld)
		}
	})
	t.Run("one_vs_16", func(t *testing.T) {
		got := measure(true, func(r *abRig) { r.download(1, 1); r.download(2, 16) }, 1, 2)
		t.Logf("one download %.3f C, sixteen %.3f C, both %.3f C", got[0]/capOld, got[1]/capOld, (got[0]+got[1])/capOld)
		if got[0] < 0.9*capOld/2 {
			t.Fatalf("the user of one download took %.3f C, its floor is 0.5 C", got[0]/capOld)
		}
		if got[0]+got[1] < 0.97*capOld {
			t.Fatalf("the two took %.3f C", (got[0]+got[1])/capOld)
		}
	})
	t.Run("newheavy", func(t *testing.T) {
		r := newABRig(t, abRate, delay, true)
		defer r.close()
		pr := r.probe(7, 200*time.Millisecond)
		t0 := time.Now()
		time.Sleep(time.Second)
		floor := minDur(pr.within(t0, time.Now()))
		r.download(1, 1)
		time.Sleep(3 * time.Second)
		r.download(2, 16)
		a := time.Now()
		c := r.counter(r.down, 1)
		// the join: the heavy user's 16 new connections each queue their
		// first 32 KiB as light (§3.1), within its lane credit (160 KiB/s):
		// at 10 Mbit/s the existing user's share dips for ~3 s (logged); the
		// rig's verdict (e) takes its rate after SETTLE (5 s, after_settled)
		join := 1e18
		for range 5 {
			join = min(join, rateOver(c, time.Second))
		}
		least, sum := 1e18, 0.0
		for range 6 {
			x := rateOver(c, time.Second)
			least, sum = min(least, x), sum+x
		}
		p99 := abPct(pr.within(a, time.Now()), 99)
		t.Logf("existing bulk user: least second %.3f C in the 5 s after the join; settled %.3f C over 6 s, least second %.3f C; light user p99 %v from the join (RTT floor %v)",
			join/capOld, sum/6/capOld, least/capOld, p99.Round(100*time.Microsecond), floor.Round(100*time.Microsecond))
		if p99 > floor+abBound {
			t.Fatalf("the light user's p99 %v past %v + %v", p99, floor, abBound)
		}
		if least < 0.9*capOld/2 {
			t.Fatalf("the existing bulk user took %.3f C in a second, its floor is 0.5 C", least/capOld)
		}
	})
}

// TestAngleBSchedOff (TestAngleSchedOff of the plan): HS2_TUNE_SCHED=0, or
// a link whose other end lacks the cap, keeps the old writer (smux's heap
// and sendLoop), the TUN's FIFO and per-packet rendezvous; the old tests of
// those paths pass as they were.
func TestAngleBSchedOff(t *testing.T) {
	old := TuneSched
	TuneSched = 0
	defer func() { TuneSched = old }()
	a, b := net.Pipe()
	s, _, err := newSession(a, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer b.Close()
	if s.Queue() != nil {
		t.Fatal("HS2_TUNE_SCHED=0: the session has a link scheduler")
	}
	// the TUN: each packet by rendezvous, no pin
	var set l3Set
	l1 := &l3Link{id: 1}
	set.add(l1)
	pins := l3Pins{off: SchedOff()}
	if set.pinned(&pins, 42, ctrlNow()) != l1 || pins.m != nil {
		t.Fatal("HS2_TUNE_SCHED=0: the TUN flow was pinned")
	}
	for name, f := range map[string]func(*testing.T){
		"WriteLoopDeliversAndKeepsAlive":   TestWriteLoopDeliversAndKeepsAlive,
		"WriteLoopDropsStalePackets":       TestWriteLoopDropsStalePackets,
		"PumpNeverBlocksOnStuckLink":       TestPumpNeverBlocksOnStuckLink,
		"StreamL3DroppedWhenSessionSilent": TestStreamL3DroppedWhenSessionSilent,
		"NewSessionShapesAnyStreamCarrier": TestNewSessionShapesAnyStreamCarrier,
		"UDPFlowDoesNotHoldUpThePort":      TestUDPFlowDoesNotHoldUpThePort,
		"SlowReadersDoNotStallLink":        TestSlowReadersDoNotStallLink,
	} {
		t.Run(name, f)
	}
	TuneSched = old
	// a link whose peer lacks the cap: the old writer, C2 on here
	c, d := net.Pipe()
	defer d.Close()
	s2, _, err := newSessionCaps(c, false, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Queue() != nil {
		t.Fatal("a link without CapSched has a link scheduler")
	}
}

// The lab knobs of C3's gate (hostq.go: HS2_TUNE_HQG_SEGS,
// HS2_TUNE_HQG_GATE): a cap of at least that many segments with the mark at
// the cap, or the mark alone without the gate; by default the gate of §3.3.
func TestAngleBHostQLabKnobs(t *testing.T) {
	oldSegs, oldGate := TuneHQGSegs, TuneHQGGate
	defer func() { TuneHQGSegs, TuneHQGGate = oldSegs, oldGate }()
	for _, c := range []struct {
		segs, gate int
		on         bool
		cap, lowat int
	}{
		{0, 1, true, 2896, 1448},
		{4, 1, true, 4 * 1448, 4 * 1448},
		{0, 0, false, 2896, Lowat(3*mbit, 1448)},
	} {
		TuneHQGSegs, TuneHQGGate = c.segs, c.gate
		s := newSkbSock(3*mbit, 1448, 1)
		h := newHostQ(s, s)
		h.Update(3*mbit, 1448)
		if h.on.Load() != c.on || int(h.cap.Load()) != c.cap || s.lowat != c.lowat {
			t.Fatalf("segs %d gate %d: on %v cap %d mark %d, want %v %d %d", c.segs, c.gate, h.on.Load(), h.cap.Load(), s.lowat, c.on, c.cap, c.lowat)
		}
	}
}
