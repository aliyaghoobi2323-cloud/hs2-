package engine

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

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

	// sampler-only
	prev       uint64
	lastActive time.Time
	ewma       float32
	steady     uint8
}

// exitTraffic counts the exit's user connections and link throughput.
type exitTraffic struct {
	peers *linkPeers // the live links' meters (throughput)

	mu    sync.Mutex
	conns map[*exitConn]struct{}

	// sampler state
	lastAt time.Time
	prevB  map[*linkMeter]uint64
	hist   []float64 // bytes/s per tick, newest last (peakTicks)

	snap atomic.Pointer[exitSnap]
}

// exitSnap is the last sample.
type exitSnap struct {
	open, flowing int
	bps, peakBps  float64
}

// peakTicks: the peak covers the last minute, like the edge's.
const peakTicks = 30

func newExitTraffic(peers *linkPeers) *exitTraffic {
	return &exitTraffic{peers: peers, conns: map[*exitConn]struct{}{}, prevB: map[*linkMeter]uint64{}}
}

// open registers a relayed user connection; close it with done.
func (t *exitTraffic) open() *exitConn {
	c := &exitConn{lastActive: time.Now()}
	if t == nil {
		return c
	}
	t.mu.Lock()
	t.conns[c] = struct{}{}
	t.mu.Unlock()
	return c
}

func (t *exitTraffic) done(c *exitConn) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
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
	t.mu.Lock()
	for c := range t.conns {
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
		s.open++
		if float64(c.ewma) >= flowingRate && now.Sub(c.lastActive) <= flowRecent || c.steady == 7 {
			s.flowing++
		}
	}
	t.mu.Unlock()

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
