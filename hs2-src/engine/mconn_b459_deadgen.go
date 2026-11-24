package engine

import (
	"sync"
	"sync/atomic"
)

// b459 (r458 lab1, 12k/24k users at the ZDU drain end): HS2_B459_DEADGEN
// (default off after r459: ping max +77 ms in the 2 s after the exit, 3/3 pairs;
// acts only with HS2_ZDU on and a connection pinned to a peer
// instance, zdu_pin.go).
//
// When the old Kharej process exits, its links all go, but the Iran
// connections pinned to it still ask for join subs (rescue, rst, extra
// light). Such a join can never be placed: the pin keeps it off the new
// generation's links. It still waited for a HS2_B457_PICKCAP slot, scanned
// the pool, slept mconnPickGap and tried again until mconnNextWait (2 s).
// r458 counted ~10k (12k users) / ~21k (24k users) of them in the cap queue
// right after the exit, failing after 2-4 s and holding up the joins that
// could be placed.
//
// With the key on, the generation of a pin is dead when no live link of it
// is left while a live link of a newer generation is in the pool (the old
// process of a handover: it takes no new links). Then:
//   - a join of a connection pinned to it fails at once (errMConnNoLink,
//     as the 2 s wait would have ended), before taking a cap slot and with
//     no pick and no mconnPickGap retry;
//   - a join already waiting for a cap slot is woken when the generation's
//     last link goes (the pool scan below found it gone) and fails the same
//     way, giving its place in the queue to the others.
//
// Pinned to a live generation, no pin, no newer generation (an outage of the
// whole pool: links come back), or the key off: unchanged. No wire change.
var b459DeadGen atomic.Bool

func init() { b459DeadGen.Store(envBoolDefault("HS2_B459_DEADGEN", false)) }

var b459Stat struct {
	fast    atomic.Int64 // joins failed at once: pin generation dead
	dropped atomic.Int64 // cap waiters dropped: their generation died while they waited
	wakes   atomic.Int64 // wake-ups sent (a generation's last link went)
}

// b459LM: the LinkManager's set of live peer generations.
type b459LM struct {
	mu       sync.Mutex
	snap     atomic.Pointer[b459Snap]
	wake     atomic.Pointer[chan struct{}]
	scanning atomic.Bool
}

type b459Snap struct {
	epoch  uint64
	insts  []zduInst // generations with a live link
	newest zduInst
}

func (s *b459Snap) has(in zduInst) bool {
	for _, x := range s.insts {
		if x == in {
			return true
		}
	}
	return false
}

// b459ScanLocked rebuilds the set; when a generation of the last set has no
// live link now, the cap waiters are woken. Caller holds m.mu (read or write).
func (m *LinkManager) b459ScanLocked() *b459Snap {
	s := &b459Snap{epoch: m.b327.epoch.Load()}
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
	m.b459.mu.Lock()
	old := m.b459.snap.Load()
	m.b459.snap.Store(s)
	lost := false
	if old != nil {
		for _, in := range old.insts {
			if !s.has(in) {
				lost = true
			}
		}
	}
	if lost {
		if p := m.b459.wake.Load(); p != nil {
			close(*p)
			m.b459.wake.Store(nil)
			b459Stat.wakes.Add(1)
		}
	}
	m.b459.mu.Unlock()
	return s
}

// b459WakeCh is closed when a generation's last link goes.
func (m *LinkManager) b459WakeCh() <-chan struct{} {
	if p := m.b459.wake.Load(); p != nil {
		return *p
	}
	m.b459.mu.Lock()
	defer m.b459.mu.Unlock()
	if p := m.b459.wake.Load(); p != nil {
		return *p
	}
	c := make(chan struct{})
	m.b459.wake.Store(&c)
	return c
}

// b459Dead: in has no live link and a newer generation has one. The set is
// rebuilt once per pool change (b327 epoch) by one caller; the others read
// the last one.
func (m *LinkManager) b459Dead(in zduInst) bool {
	s := m.b459.snap.Load()
	if s == nil || s.epoch != m.b327.epoch.Load() {
		if m.b459.scanning.CompareAndSwap(false, true) {
			m.mu.RLock()
			s = m.b459ScanLocked()
			m.mu.RUnlock()
			m.b459.scanning.Store(false)
		} else if s == nil {
			return false
		}
	}
	return s.newest.known() && s.newest.newer(in) && !s.has(in)
}

// b459Watched: the edge's joins are subject to the dead-generation rule.
func (e *mconnEdge) b459Watched() bool {
	return b459DeadGen.Load() && zduOn.Load() && e.lm != nil && e.zduPin.Load() != nil
}

// b459PinDead: the connection is pinned to a dead generation.
func (e *mconnEdge) b459PinDead() bool {
	if !e.b459Watched() {
		return false
	}
	return e.lm.b459Dead(*e.zduPin.Load())
}
