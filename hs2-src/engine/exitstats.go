package engine

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/xtaci/smux"
)

// The exit's own view of the traffic. Every user connection the edge carries
// arrives here as a stream and is relayed to the panel, and every link's bytes
// pass through this server's meters — so the exit can count what the edge
// counts, by the same rules, instead of showing zero: open user connections
// (TCP relays and UDP flows), the ones actively moving data (flowing, as on
// the edge: rate EWMA >= flowingRate within flowRecent, or steady), the
// tunnel's throughput (all links, both directions) and its last minute's
// peak.

// exitConn is one user connection relayed by the exit.
type exitConn struct {
	bytes atomic.Uint64 // payload moved either way (data path: one atomic add)
	mtr   *linkMeter    // the link it rides (nil: unknown); gets its heavyN
	// sub: one sub-stream of a multi-link connection (HS2_MCONN). It counts
	// toward its link's heavy streams only; the connection itself is counted
	// once, by its own exitConn (no link).
	sub bool
	// kill (b468, nil = none): ends this user connection on purpose (its
	// target and its stream). Set before the conn is registered, never after.
	kill func()

	// sampler-only
	prev       uint64
	lastActive time.Time
	ewma       float32
	steady     uint8
}

// exitTraffic counts the exit's user connections and link throughput.
type exitTraffic struct {
	peers *linkPeers // the live links' meters (throughput)

	// b307 (sc307 scale test): the conn set is split into exitShards
	// shards by the conn's address, each with its own lock. At 3x one
	// global t.mu made exitTraffic.add/done the top Kharej lock (mutex
	// delay ~1230 s per 20 s, every exit stream open/close of ~200k
	// goroutines); a shard is held only for its own add/delete or copy.
	sh [exitShards]exitShard

	// sampler state
	lastAt    time.Time
	prevB     map[*linkMeter]uint64
	heavy     map[*linkMeter]int32 // heavy connections per link this tick (reused)
	heavyPrev map[*linkMeter]int32 // ... last tick
	hist      []float64            // bytes/s per tick, newest last (peakTicks)

	snap atomic.Pointer[exitSnap]

	// b296: the sampler's copy of conns (reused); a shard's mu is held only for the
	// copy, the per-conn math runs without it (add/done on new or closing
	// exit streams no longer wait for a full scan).
	scan []*exitConn
}

// exitShards: number of conn-set shards (power of two).
const exitShards = 64

type exitShard struct {
	mu    sync.Mutex
	conns map[*exitConn]struct{}
	_     [40]byte // keep neighbouring shard locks off one cache line
}

// shardOf: c's shard, from its address (Go's heap does not move objects).
func (t *exitTraffic) shardOf(c *exitConn) *exitShard {
	h := uint64(uintptr(unsafe.Pointer(c))) * 0x9E3779B97F4A7C15
	return &t.sh[h>>(64-6)]
}

// exitSnap is the last sample.
type exitSnap struct {
	open, flowing int
	bps, peakBps  float64
}

// peakTicks: the peak covers the last minute, like the edge's.
const peakTicks = 30

func newExitTraffic(peers *linkPeers) *exitTraffic {
	t := &exitTraffic{peers: peers, prevB: map[*linkMeter]uint64{},
		heavy: map[*linkMeter]int32{}, heavyPrev: map[*linkMeter]int32{}}
	for i := range t.sh {
		t.sh[i].conns = map[*exitConn]struct{}{}
	}
	return t
}

// open registers a relayed user connection riding the link metered by mtr
// (nil: unknown); close it with done.
func (t *exitTraffic) open(mtr *linkMeter) *exitConn {
	return t.add(&exitConn{lastActive: time.Now(), mtr: mtr})
}

// openKill is open with a kill for HS2_B468_DRAINFAST (zdu_b468_drainfast.go).
func (t *exitTraffic) openKill(mtr *linkMeter, kill func()) *exitConn {
	return t.add(&exitConn{lastActive: time.Now(), mtr: mtr, kill: kill})
}

func (t *exitTraffic) add(c *exitConn) *exitConn {
	if t == nil {
		return c
	}
	if c.mtr != nil {
		c.mtr.users.Add(1)
	}
	sh := t.shardOf(c)
	sh.mu.Lock()
	sh.conns[c] = struct{}{}
	sh.mu.Unlock()
	return c
}

// openSub registers one sub-stream of a multi-link connection on the link
// metered by mtr: it counts toward the link's heavy streams, not as a user
// connection. Close it with done.
func (t *exitTraffic) openSub(mtr *linkMeter) *exitConn {
	return t.add(&exitConn{lastActive: time.Now(), mtr: mtr, sub: true})
}

func (t *exitTraffic) done(c *exitConn) {
	if t == nil {
		return
	}
	sh := t.shardOf(c)
	sh.mu.Lock()
	_, ok := sh.conns[c]
	delete(sh.conns, c)
	sh.mu.Unlock()
	if ok && c.mtr != nil {
		c.mtr.users.Add(-1)
	}
}

// run samples every healthTick until ctx ends.
func (t *exitTraffic) run(done <-chan struct{}) {
	tk := time.NewTicker(healthTick)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-tk.C:
			t.sample(now)
		}
	}
}

// sample updates every connection's rate and the link throughput.
func (t *exitTraffic) sample(now time.Time) {
	dt := healthTick
	if !t.lastAt.IsZero() {
		dt = now.Sub(t.lastAt)
	}
	t.lastAt = now
	alpha := flowAlpha(dt)
	var s exitSnap
	// b296 (sc4x): copy the set under t.mu, compute outside it. The sampler
	// fields (prev, lastActive, ewma, steady) are the sampler's own; a conn
	// closed after the copy is counted once more this tick (as if it had
	// closed just after the scan).
	scan := t.scan[:0]
	for i := range t.sh {
		sh := &t.sh[i]
		sh.mu.Lock()
		for c := range sh.conns {
			scan = append(scan, c)
		}
		sh.mu.Unlock()
	}
	for _, c := range scan {
		b := c.bytes.Load()
		steady := false
		if b != c.prev {
			if dt > 0 {
				rate := float64(b-c.prev) / dt.Seconds()
				c.ewma += float32(alpha * (rate - float64(c.ewma)))
				steady = rate >= flowSteadyRate
			}
			c.prev, c.lastActive = b, now
		} else if dt > 0 {
			c.ewma -= float32(alpha * float64(c.ewma))
		}
		c.steady = (c.steady<<1 | b2u(steady)) & 7
		if !c.sub {
			s.open++
			if float64(c.ewma) >= flowingRate && now.Sub(c.lastActive) <= flowRecent || c.steady == 7 {
				s.flowing++
			}
		}
		// the edge's rule (mtcpLink.flowStats), per link, for the TUN lane
		if c.mtr != nil && float64(c.ewma) >= heavyRate && now.Sub(c.lastActive) <= flowRecent {
			t.heavy[c.mtr]++
		}
	}
	n := len(scan)
	clear(scan) // a closed conn is not kept alive by the copy
	if cap(scan) > 4*n+1024 {
		scan = nil // the set shrank a lot: let the big copy go
	}
	t.scan = scan[:0]
	t.storeHeavy()

	// Throughput over every live link, as the edge measures it (the bytes its
	// meters saw, both directions, this tick).
	var moved uint64
	seen := map[*linkMeter]bool{}
	if p := t.peers; p != nil {
		p.mu.Lock()
		for m := range p.m {
			b := m.rdBytes.Load() + m.wrBytes.Load()
			if prev, ok := t.prevB[m]; ok && b >= prev {
				moved += b - prev
			}
			t.prevB[m] = b
			seen[m] = true
		}
		p.mu.Unlock()
	}
	for m := range t.prevB {
		if !seen[m] {
			delete(t.prevB, m) // the link is gone
		}
	}
	if dt > 0 {
		s.bps = float64(moved) / dt.Seconds()
	}
	t.hist = append(t.hist, s.bps)
	if len(t.hist) > peakTicks {
		t.hist = t.hist[len(t.hist)-peakTicks:]
	}
	s.peakBps = max(peakOf(t.hist), s.bps) // never below what moves now
	t.snap.Store(&s)
}

// storeHeavy publishes this tick's heavy count of every link to its meter
// (zero for a link whose heavy connections stopped or closed since the last
// tick) and keeps the counts as the next tick's previous ones.
func (t *exitTraffic) storeHeavy() {
	for m, n := range t.heavy {
		m.heavyN.Store(max(n, min(m.stripeN.Load(), 1)))
	}
	for m := range t.heavyPrev {
		if _, ok := t.heavy[m]; !ok {
			m.heavyN.Store(min(m.stripeN.Load(), 1)) // a stripe link stays heavy (HS2_MCONN_STRIPE)
		}
	}
	clear(t.heavyPrev)
	t.heavy, t.heavyPrev = t.heavyPrev, t.heavy
}

// peakOf is the highest two-tick average in h (a single-tick spike is not a
// peak — the edge's rule).
func peakOf(h []float64) float64 {
	peak := 0.0
	for i, g := range h {
		if i > 0 {
			g = (g + h[i-1]) / 2
		}
		if g > peak {
			peak = g
		}
	}
	return peak
}

// fill puts the last sample into a snapshot.
func (t *exitTraffic) fill(st *PoolStats) {
	st.Counted = true
	if t == nil {
		return
	}
	if s := t.snap.Load(); s != nil {
		st.Users, st.Flowing = s.open, s.flowing
		st.MbitPerS, st.PeakMbit = mbitps(s.bps), mbitps(s.peakBps)
	}
}

// exitCountedStream counts a relayed user connection's bytes.
type exitCountedStream struct {
	*smux.Stream
	c *exitConn
}

func (s exitCountedStream) Read(p []byte) (int, error) {
	n, err := s.Stream.Read(p)
	if n > 0 {
		s.c.bytes.Add(uint64(n))
	}
	return n, err
}

func (s exitCountedStream) Write(p []byte) (int, error) {
	n, err := s.Stream.Write(p)
	if n > 0 {
		s.c.bytes.Add(uint64(n))
	}
	return n, err
}

var _ io.ReadWriteCloser = exitCountedStream{}
