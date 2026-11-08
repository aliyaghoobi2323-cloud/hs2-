package smux

// hs2 addition to smux v1.5.24: an adaptive per-stream receive window
// (Config.AutoWindow, protocol version 2 only). With AutoWindow nil every
// line below is skipped and the session is upstream smux, byte for byte.
//
// Why: the receiver advertises its window in every UPD frame (cmdUPD carries
// consumed + window), and upstream always advertises MaxStreamBuffer and sends
// the UPD only after MaxStreamBuffer/2 was read. One stream therefore moves at
// most about MaxStreamBuffer/2 per round trip: 2 MiB at 80 ms is ~100-150
// Mbit/s, whatever the link could carry.
//
// What: each stream's window starts at MaxStreamBuffer (light streams stay
// exactly where they were) and doubles, up to AutoWindow.Max, each time the
// stream proves that the window — not the link, not the reader — held it back:
//
//   - Loop RTT probe. When a UPD moves the limit the sender may write up to,
//     the old limit and the time are remembered (one probe at a time). The
//     sender cannot write past the old limit before this UPD reached it, so
//     the time until the first byte past the old limit arrives is at least the
//     loop round trip (UPD out, data back, both links' queues included), and
//     equal to it when the sender sat at the limit waiting: window-bound.
//     The session keeps the smallest sample (refreshed every rttFresh).
//   - Grow when a probe of the CURRENT window (the old limit was advertised
//     with the window the stream has now) comes back within 1.25x the
//     session's minimum (+2 ms) while the reader keeps up (at most a quarter
//     window unread). A link-bound stream's probes come back late (its data
//     waits in the link's queues), an app-bound one never reaches the limit,
//     and a slow reader leaves data unread: none of them grows.
//   - The window may only grow by what the process-wide WindowBudget grants,
//     and the same amount is added to the session's token bucket, so a grown
//     stream can never starve the link's other streams of the bucket
//     (MaxReceiveBuffer keeps protecting them exactly as before).
//   - Decay: a sweeper (one goroutine per budget, only while some stream is
//     grown) halves a stream's window once it went holdFor without proving
//     window-bound and without using half its window per round trip; under
//     budget pressure faster, and the largest windows down to a fair share.
//     The shrunken window is sent to the peer at once. Closing a stream (or
//     its session) returns everything.
//   - UPD cadence: with AutoWindow a UPD goes out every window/UpdateDiv
//     read (upstream: /2), so a window-bound sender waits less per round trip.
//
// Wire: nothing new. An older peer's sender honours whatever window a UPD
// carries (that is upstream's own rule), so either side may run this alone:
// an Iran side with it speeds up downloads, a kharej side uploads.
//
// Memory: the window is credit, not allocation. What a stream can hold
// unread is bounded by its window, so the process-wide worst case beyond
// upstream's is the budget's limit (plus the allocator's rounding), reached
// only when grown streams' readers all stall.

import (
	"sync"
	"sync/atomic"
	"time"
)

// AutoWindow switches on the adaptive per-stream window (see above). One
// value is shared by every session of a process.
type AutoWindow struct {
	// Max is the largest window one stream may grow to (bytes).
	Max int
	// UpdateDiv: a UPD is sent every window/UpdateDiv bytes read (min 2).
	UpdateDiv int
	// Budget bounds the growth of all streams together (required).
	Budget *WindowBudget
}

// Timings (variables only so tests can shorten them).
var (
	rttFresh      = 10 * time.Second       // the session's min RTT is re-learnt after this
	holdFor       = 2 * time.Second        // a grown window idle this long halves
	holdPressured = 300 * time.Millisecond // ... while the budget is under pressure
	sweepEvery    = 250 * time.Millisecond
	rttDefault    = 100 * time.Millisecond // until a session has a sample
)

// WindowBudget is the process-wide allowance for window growth beyond each
// stream's base window, in bytes.
type WindowBudget struct {
	limit int64
	used  atomic.Int64
	peak  atomic.Int64

	grows, denied, shrinks atomic.Uint64

	mu      sync.Mutex
	streams map[*Stream]struct{}
	running bool

	// timings, from the package defaults (tests set them before use)
	hold, holdP, every time.Duration
}

// NewWindowBudget makes a budget of limit bytes.
func NewWindowBudget(limit int64) *WindowBudget {
	return &WindowBudget{limit: limit, streams: make(map[*Stream]struct{}),
		hold: holdFor, holdP: holdPressured, every: sweepEvery}
}

// WindowBudgetStats is a snapshot of a budget.
type WindowBudgetStats struct {
	Limit, Used, Peak      int64
	Streams                int // streams holding grown windows
	Grows, Denied, Shrinks uint64
}

// Stats returns the budget's figures now.
func (b *WindowBudget) Stats() WindowBudgetStats {
	b.mu.Lock()
	n := len(b.streams)
	b.mu.Unlock()
	return WindowBudgetStats{Limit: b.limit, Used: b.used.Load(), Peak: b.peak.Load(), Streams: n,
		Grows: b.grows.Load(), Denied: b.denied.Load(), Shrinks: b.shrinks.Load()}
}

func (b *WindowBudget) take(n int64) bool {
	for {
		u := b.used.Load()
		if u+n > b.limit {
			b.denied.Add(1)
			return false
		}
		if b.used.CompareAndSwap(u, u+n) {
			for {
				p := b.peak.Load()
				if u+n <= p || b.peak.CompareAndSwap(p, u+n) {
					break
				}
			}
			return true
		}
	}
}

func (b *WindowBudget) give(n int64) { b.used.Add(-n) }

// pressured: at least 7/8 of the budget is out.
func (b *WindowBudget) pressured() bool { return b.used.Load()*8 >= b.limit*7 }

func (b *WindowBudget) track(s *Stream) {
	b.mu.Lock()
	b.streams[s] = struct{}{}
	if !b.running {
		b.running = true
		go b.sweep()
	}
	b.mu.Unlock()
}

func (b *WindowBudget) untrack(s *Stream) {
	b.mu.Lock()
	delete(b.streams, s)
	b.mu.Unlock()
}

// sweep runs while some stream holds a grown window.
func (b *WindowBudget) sweep() {
	t := time.NewTicker(b.every)
	defer t.Stop()
	var list []*Stream
	for range t.C {
		b.mu.Lock()
		if len(b.streams) == 0 {
			b.running = false
			b.mu.Unlock()
			return
		}
		list = list[:0]
		for s := range b.streams {
			list = append(list, s)
		}
		b.mu.Unlock()
		b.sweepOnce(list, time.Now())
	}
}

func (b *WindowBudget) sweepOnce(list []*Stream, now time.Time) {
	pressured := b.pressured()
	var fair uint32
	if pressured && len(list) > 0 {
		fair = uint32(b.limit / int64(len(list)))
	}
	for _, s := range list {
		if s.awTick(now, pressured, fair) {
			b.untrack(s)
		}
	}
}

// streamAW is one stream's adaptive window state, guarded by its bufferLock.
type streamAW struct {
	cfg   *AutoWindow
	base  uint32 // MaxStreamBuffer
	win   uint32 // the window advertised now
	extra uint32 // win - base: taken from the budget and added to the bucket
	div   uint32

	numRecv  uint32 // bytes received (pushBytes)
	limit    uint32 // the last UPD's consumed + window (the sender's newest limit)
	limitWin uint32 // the window that UPD carried

	probeOn    bool
	probeLimit uint32
	probeWin   uint32
	probeAt    time.Time

	pendingUPD bool      // the window changed: send a UPD at the next read
	limitedAt  time.Time // last proof of being window-bound (or last change)
	usedAt     time.Time // last sweep that saw the stream use half its window per RTT
	tickRead   uint32
	tickAt     time.Time
	closed     bool
}

func newStreamAW(cfg *AutoWindow, base int) *streamAW {
	div := uint32(cfg.UpdateDiv)
	if div < 2 {
		div = 2
	}
	now := time.Now()
	return &streamAW{cfg: cfg, base: uint32(base), win: uint32(base), div: div,
		limit: initialPeerWindow, limitWin: initialPeerWindow,
		limitedAt: now, usedAt: now, tickAt: now}
}

// awConsumed is the AutoWindow version of the read side's UPD rule, called
// with bufferLock held after numRead/incr were advanced by n. It returns the
// consumed count and window to send (consumed 0: no UPD now).
func (s *Stream) awConsumed(n uint32) (uint32, uint32) {
	a := s.aw
	if !(s.incr >= a.win/a.div || s.numRead == n || (a.pendingUPD && n > 0)) {
		return 0, 0
	}
	s.incr = 0
	if s.numRead == 0 {
		return 0, 0
	}
	a.pendingUPD = false
	if !a.probeOn && a.limit != s.numRead+a.win {
		a.probeOn, a.probeLimit, a.probeWin, a.probeAt = true, a.limit, a.limitWin, time.Now()
	}
	a.limit, a.limitWin = s.numRead+a.win, a.win
	return s.numRead, a.win
}

// awPushed accounts n received bytes (recvLoop, bufferLock held) and, when
// they complete a probe, grows the window if the probe shows it bound.
func (s *Stream) awPushed(n uint32) {
	a := s.aw
	a.numRecv += n
	if !a.probeOn || int32(a.numRecv-a.probeLimit) <= 0 {
		return
	}
	a.probeOn = false
	now := time.Now()
	sample := now.Sub(a.probeAt)
	min, ok := s.sess.awRTT(sample, now)
	if !ok || a.closed || a.probeWin < a.win || sample > min+min/4+2*time.Millisecond {
		return
	}
	if unread := a.numRecv - s.numRead; unread > a.win/4 {
		return // the reader is behind: the window is doing its job
	}
	a.limitedAt = now
	if a.win >= uint32(a.cfg.Max) {
		return
	}
	nw := a.win * 2
	if nw > uint32(a.cfg.Max) || nw < a.win {
		nw = uint32(a.cfg.Max)
	}
	d := nw - a.win
	if !a.cfg.Budget.take(int64(d)) {
		return
	}
	a.cfg.Budget.grows.Add(1)
	a.win, a.extra, a.pendingUPD = nw, a.extra+d, true
	s.sess.returnTokens(int(d)) // the bucket grows with the window
	a.cfg.Budget.track(s)
}

// awTick is the sweeper's look at one grown stream; true: stop tracking it.
func (s *Stream) awTick(now time.Time, pressured bool, fair uint32) bool {
	select {
	case <-s.die:
		s.awRelease()
		return true
	default:
	}
	if s.sess.IsClosed() {
		s.awRelease()
		return true
	}
	rtt := s.sess.awRTTNow()
	s.bufferLock.Lock()
	a := s.aw
	if a.closed || a.extra == 0 {
		s.bufferLock.Unlock()
		return true
	}
	// usage: bytes read per round trip over the last tick, doubled (the
	// window a stream needs to keep that pace with room to grow)
	if dt := now.Sub(a.tickAt); dt > 0 {
		perRTT := float64(s.numRead-a.tickRead) * float64(rtt) / float64(dt)
		if 2*perRTT >= float64(a.win/2) {
			a.usedAt = now
		}
	}
	a.tickRead, a.tickAt = s.numRead, now
	b := a.cfg.Budget
	hold := b.hold
	if pressured {
		hold = b.holdP
	}
	shrink := now.Sub(a.limitedAt) > hold && now.Sub(a.usedAt) > hold
	if pressured && a.win > a.base+fair && now.Sub(a.limitedAt) > b.holdP {
		shrink = true // the largest windows give way first
	}
	nw := a.win / 2
	if nw < a.base {
		nw = a.base
	}
	if !shrink || a.numRecv-s.numRead > nw/2 {
		s.bufferLock.Unlock()
		return false
	}
	d := a.win - nw
	a.win, a.extra, a.limitedAt, a.usedAt = nw, a.extra-d, now, now
	a.probeOn, a.pendingUPD = false, false
	consumed := s.numRead
	a.limit, a.limitWin = consumed+nw, nw
	s.incr = 0
	done := a.extra == 0
	s.bufferLock.Unlock()

	b.give(int64(d))
	b.shrinks.Add(1)
	atomic.AddInt32(&s.sess.bucket, -int32(d))
	if consumed > 0 {
		go s.sendWindowUpdate(consumed, nw) // the peer learns the smaller window now
	}
	return done
}

// awRelease returns a stream's growth (stream or session closing).
func (s *Stream) awRelease() {
	if s.aw == nil {
		return
	}
	s.bufferLock.Lock()
	a := s.aw
	d := a.extra
	a.extra, a.win, a.closed = 0, a.base, true
	s.bufferLock.Unlock()
	if d > 0 {
		a.cfg.Budget.give(int64(d))
		atomic.AddInt32(&s.sess.bucket, -int32(d))
	}
	a.cfg.Budget.untrack(s)
}

// sessAW is a session's loop-RTT minimum (AutoWindow only).
type sessAW struct {
	mu  sync.Mutex
	min time.Duration
	at  time.Time
}

// awRTT folds a probe sample into the session's minimum. ok is false while
// the minimum is (re)learnt from this very sample.
func (s *Session) awRTT(sample time.Duration, now time.Time) (time.Duration, bool) {
	s.aw.mu.Lock()
	defer s.aw.mu.Unlock()
	if s.aw.min == 0 || now.Sub(s.aw.at) > rttFresh {
		s.aw.min, s.aw.at = sample, now
		return sample, false
	}
	if sample < s.aw.min {
		s.aw.min, s.aw.at = sample, now
	}
	return s.aw.min, true
}

func (s *Session) awRTTNow() time.Duration {
	s.aw.mu.Lock()
	defer s.aw.mu.Unlock()
	if s.aw.min == 0 {
		return rttDefault
	}
	return s.aw.min
}
