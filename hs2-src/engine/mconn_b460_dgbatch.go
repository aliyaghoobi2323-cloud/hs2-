package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// b460 (r459 lab1, 12k/24k users at the ZDU drain end): HS2_B460_DGBATCH
// (default off; acts only with HS2_ZDU on and a connection pinned to a peer
// instance, zdu_pin.go).
//
// b459 (HS2_B459_DEADGEN) failed joins pinned to the dead old generation, but
// ~10k of them were already in the HS2_B457_PICKCAP queue when the last old
// link went (its lazy scan ran only once per pool change and counted a link
// as live until DropLink took it out), and one close woke all of them at once:
// ping max in the 2 s after the exit +77 ms, 3/3 pairs. With this key on:
//   - (a) a join is never queued when its pin generation has no Alive() link
//     while a newer generation has one: it fails at once (errMConnNoLink, as
//     the 2 s wait would have ended). The live set is rebuilt at most once per
//     b460Fresh (or on a pool change) by one caller; Alive()==false is gone.
//   - (b) waiters already queued wait on their own generation's channel; one
//     pump goroutine per pool (running only while waiters exist) rescans every
//     b460Tick and, for a dead generation, releases at most b460Batch waiters
//     per tick, so the scheduler is never handed ~10k wake-ups at once.
//
// Pinned to a live generation, no pin, no newer generation (an outage of the
// whole pool: links come back), or the key off: unchanged. No wire change.
var b460DGBatch atomic.Bool

var (
	b460Batch = 128                  // waiters released per tick per dead generation (HS2_B460_DGBATCH_N)
	b460Tick  = time.Millisecond     // pump period
	b460Fresh = 2 * time.Millisecond // max age of the live set for check (a)
)

func init() {
	b460DGBatch.Store(envBoolDefault("HS2_B460_DGBATCH", false))
	if n := envIntDefault("HS2_B460_DGBATCH_N", b460Batch); n > 0 {
		b460Batch = n
	}
}

var b460Stat struct {
	fast     atomic.Int64 // joins failed before the queue: pin generation dead
	released atomic.Int64 // queued waiters released (their generation died)
	ticks    atomic.Int64 // pump ticks that released at least one waiter
}

type b460Snap struct {
	at     int64 // UnixNano of the scan
	epoch  uint64
	insts  []zduInst // generations with an Alive() link
	newest zduInst
}

func (s *b460Snap) has(in zduInst) bool {
	for _, x := range s.insts {
		if x == in {
			return true
		}
	}
	return false
}

func (s *b460Snap) dead(in zduInst) bool {
	return s != nil && s.newest.known() && s.newest.newer(in) && !s.has(in)
}

// b460LM: the LinkManager's live generations and its queued dead-gen waiters.
type b460LM struct {
	snap     atomic.Pointer[b460Snap]
	scanning atomic.Bool
	mu       sync.Mutex
	gens     map[zduInst]*b460Gen // waiters per pin generation
	pumping  bool
}

type b460Gen struct {
	ch chan struct{} // unbuffered: one send releases one waiter
	n  int
}

func (m *LinkManager) b460Scan() *b460Snap {
	s := &b460Snap{at: monoNano(time.Now()), epoch: m.b327.epoch.Load()}
	m.mu.RLock()
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		if in := linkZDUInst(ml.link); in.known() && !s.has(in) {
			s.insts = append(s.insts, in)
			if in.newer(s.newest) {
				s.newest = in
			}
		}
	}
	m.mu.RUnlock()
	m.b460.snap.Store(s)
	return s
}

// b460Live returns a live set at most b460Fresh old (or the last one while
// another caller rescans).
func (m *LinkManager) b460Live() *b460Snap {
	s := m.b460.snap.Load()
	if s == nil || s.epoch != m.b327.epoch.Load() || time.Since(monoTime(s.at)) > b460Fresh {
		if m.b460.scanning.CompareAndSwap(false, true) {
			s = m.b460Scan()
			m.b460.scanning.Store(false)
		}
	}
	return s
}

// b460Watched: the edge's joins are subject to the b460 rule.
func (e *mconnEdge) b460Watched() bool {
	return b460DGBatch.Load() && zduOn.Load() && e.lm != nil && e.zduPin.Load() != nil
}

// b460PinDead: check (a), the edge's pin generation is dead now.
func (e *mconnEdge) b460PinDead() bool {
	if !e.b460Watched() {
		return false
	}
	return e.lm.b460Live().dead(*e.zduPin.Load())
}

// b460Join registers a cap waiter of pin generation in and returns its
// release channel and the deregister func; it starts the pump if needed.
func (m *LinkManager) b460Join(in zduInst) (<-chan struct{}, func()) {
	m.b460.mu.Lock()
	if m.b460.gens == nil {
		m.b460.gens = make(map[zduInst]*b460Gen)
	}
	g := m.b460.gens[in]
	if g == nil {
		g = &b460Gen{ch: make(chan struct{})}
		m.b460.gens[in] = g
	}
	g.n++
	if !m.b460.pumping {
		m.b460.pumping = true
		go m.b460Pump()
	}
	m.b460.mu.Unlock()
	return g.ch, func() {
		m.b460.mu.Lock()
		if g.n--; g.n == 0 && m.b460.gens[in] == g {
			delete(m.b460.gens, in)
		}
		m.b460.mu.Unlock()
	}
}

// b460Pump runs while waiters are queued: every tick it rescans and releases
// at most b460Batch waiters of each dead generation.
func (m *LinkManager) b460Pump() {
	t := time.NewTicker(b460Tick)
	defer t.Stop()
	var dead []*b460Gen
	for range t.C {
		m.b460.mu.Lock()
		if len(m.b460.gens) == 0 {
			m.b460.pumping = false
			m.b460.mu.Unlock()
			return
		}
		m.b460.mu.Unlock()
		s := m.b460Scan()
		dead = dead[:0]
		m.b460.mu.Lock()
		for in, g := range m.b460.gens {
			if s.dead(in) {
				dead = append(dead, g)
			}
		}
		m.b460.mu.Unlock()
		any := false
		for _, g := range dead {
		batch:
			for i := 0; i < b460Batch; i++ {
				select {
				case g.ch <- struct{}{}:
					b460Stat.released.Add(1)
					any = true
				default:
					break batch // no waiter of g is blocked now
				}
			}
		}
		if any {
			b460Stat.ticks.Add(1)
		}
	}
}
