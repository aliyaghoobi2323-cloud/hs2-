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
	// NeighborMax caps a stream's window while another user stream on the
	// same session is receiving (0: no cap).
	NeighborMax int
}

// Timings (variables only so tests can shorten them).
var (
	rttFresh      = 10 * time.Second       // a min-RTT period (the minimum spans two)
	holdFor       = 2 * time.Second        // a grown window idle this long halves
	holdPressured = 300 * time.Millisecond // ... while the budget is under pressure
	sweepEvery    = 250 * time.Millisecond
	rttDefault    = 100 * time.Millisecond // until a session has a sample
	neighbourFor  = time.Second            // a stream that received within this is a live neighbour
)

// rateEpoch / lightEpochBytes: a neighbour is light while it received less
// than lightEpochBytes in its last rateEpoch (8 Mbit/s); a faster one is
// another heavy stream, which caps nobody.
const (
	rateEpoch       = 250 * time.Millisecond
	lightEpochBytes = 256 << 10
)

// awRecent: probe samples the queue rule looks at.
const awRecent = 2

// WindowBudget is the process-wide allowance for window growth beyond each
// stream's base window, in bytes.
type WindowBudget struct {
	limit int64
	used  atomic.Int64
	peak  atomic.Int64

	grows, denied, shrinks  atomic.Uint64
	queueCuts, neighborCuts atomic.Uint64

	mu      sync.Mutex
	streams map[*Stream]struct{}
	running bool

	// timings, from the package defaults (tests set them before use)
	hold, holdP, every time.Duration

	rmu sync.Mutex
	rtt minFilter // the process-wide loop-RTT minimum
}

// NewWindowBudget makes a budget of limit bytes.
func NewWindowBudget(limit int64) *WindowBudget {
	return &WindowBudget{limit: limit, streams: make(map[*Stream]struct{}),
		hold: holdFor, holdP: holdPressured, every: sweepEvery}
}

// WindowBudgetStats is a snapshot of a budget.
type WindowBudgetStats struct {
	Limit, Used, Peak       int64
	Streams                 int // streams holding grown windows
	Grows, Denied, Shrinks  uint64
	QueueCuts, NeighborCuts uint64 // shrinks by the queue rule / for a light neighbour
}

// Stats returns the budget's figures now.
func (b *WindowBudget) Stats() WindowBudgetStats {
	b.mu.Lock()
	n := len(b.streams)
	b.mu.Unlock()
	return WindowBudgetStats{Limit: b.limit, Used: b.used.Load(), Peak: b.peak.Load(), Streams: n,
		Grows: b.grows.Load(), Denied: b.denied.Load(), Shrinks: b.shrinks.Load(),
		QueueCuts: b.queueCuts.Load(), NeighborCuts: b.neighborCuts.Load()}
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
	off        bool // SetAutoWindowOff: upstream rules

	recent  [awRecent]time.Duration // the last probe samples (queue rule)
	nRecent int
	// read by other streams' neighbour checks
	bg         atomic.Bool  // the servers' own control traffic
	grown      atomic.Bool  // extra > 0
	lastRecv   atomic.Int64 // unix ns of the last data received
	epochAt    int64        // receive-rate epoch start (unix ns), own bufferLock
	epochBytes uint32
	epochRate  atomic.Uint32 // bytes received in the last whole epoch
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
	if a.off { // upstream, byte for byte
		if s.incr >= uint32(s.sess.config.MaxStreamBuffer/2) || s.numRead == n {
			s.incr = 0
			return s.numRead, 0
		}
		return 0, 0
	}
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

// awPushed accounts n received bytes (recvLoop: streamLock and bufferLock
// held) and, when they complete a probe, applies the window rules:
//
//   - queue: once the last awRecent probes of a grown window all came back
//     more than qHigh over the path's minimum, the stream's data is
//     queueing somewhere on its links (the sender's socket, the path) — data
//     other users' packets wait behind. The window loses a quarter
//     (Vegas-like). One slow probe (a loss recovery) does not count.
//   - neighbours: while another user stream on the same session (link)
//     received data in the last neighbourFor, the window is capped at
//     AutoWindow.NeighborMax, so what it queues on the shared link stays small.
//   - growth: a probe of the current window that came back within qLow of
//     the minimum, with the reader keeping up, grows the window (x2 up to 4x
//     the base, then +25%; up to the cap, if the budget grants it).
func (s *Stream) awPushed(n uint32) {
	a := s.aw
	a.numRecv += n
	nowNs := time.Now()
	ns := nowNs.UnixNano()
	a.lastRecv.Store(ns)
	if ns-a.epochAt >= int64(rateEpoch) {
		a.epochRate.Store(a.epochBytes)
		a.epochAt, a.epochBytes = ns, 0
	}
	a.epochBytes += n
	if a.off {
		return
	}
	if !a.probeOn || int32(a.numRecv-a.probeLimit) <= 0 {
		return
	}
	a.probeOn = false
	now := nowNs
	sample := now.Sub(a.probeAt)
	min, ok := s.sess.awRTT(sample, now)
	if !ok || a.closed {
		return
	}
	a.recent[a.nRecent%awRecent] = sample
	a.nRecent++
	b := a.cfg.Budget
	if a.extra > 0 && a.nRecent >= awRecent {
		low := a.recent[0]
		for _, r := range a.recent[1:] {
			low = minDur(low, r)
		}
		if low > min+qHigh(min) {
			b.queueCuts.Add(1)
			s.awShrinkLocked(max(a.base, a.win*3/4), now)
			return
		}
	}
	limit := uint32(a.cfg.Max)
	if nb := uint32(a.cfg.NeighborMax); nb > 0 && nb < limit && s.sess.awNeighborsLocked(s, now) {
		limit = max(nb, a.base)
		if a.win > limit {
			b.neighborCuts.Add(1)
			s.awShrinkLocked(limit, now)
			return
		}
	}
	if a.probeWin < a.win || sample > min+qLow(min) {
		return
	}
	if unread := a.numRecv - s.numRead; unread > a.win/4 {
		return // the reader is behind: the window is doing its job
	}
	a.limitedAt = now
	if a.win >= limit {
		return
	}
	// up to 4x the base double per probe (a fast start), then +25%: near the
	// path's capacity a smaller step overshoots into its queue less
	nw := a.win + a.win/4
	if a.win < 4*a.base {
		nw = a.win * 2
	}
	if nw > limit || nw < a.win {
		nw = limit
	}
	d := nw - a.win
	if !b.take(int64(d)) {
		return
	}
	b.grows.Add(1)
	a.win, a.extra, a.pendingUPD = nw, a.extra+d, true
	a.nRecent = 0 // the next verdicts are about the new window
	a.grown.Store(true)
	s.sess.returnTokens(int(d)) // the bucket grows with the window
	b.track(s)
}

// qLow / qHigh: queueing delay over the path's minimum loop RTT under which
// a window may grow, and over which (in each of the last awRecent probes)
// it shrinks by a quarter.
func qLow(min time.Duration) time.Duration  { return time.Millisecond + min/32 }
func qHigh(min time.Duration) time.Duration { return 3*time.Millisecond + min/16 }

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// awShrinkLocked sets the window to nw (< win) at once and tells the peer
// (bufferLock held).
func (s *Stream) awShrinkLocked(nw uint32, now time.Time) {
	a := s.aw
	if nw >= a.win {
		return
	}
	d := a.win - nw
	a.win, a.extra, a.limitedAt, a.usedAt = nw, a.extra-d, now, now
	a.probeOn, a.pendingUPD, a.nRecent = false, false, 0
	a.grown.Store(a.extra > 0)
	consumed := s.numRead
	a.limit, a.limitWin = consumed+nw, nw
	s.incr = 0
	b := a.cfg.Budget
	b.give(int64(d))
	b.shrinks.Add(1)
	atomic.AddInt32(&s.sess.bucket, -int32(d))
	if consumed > 0 {
		go s.sendWindowUpdate(consumed, nw) // the peer learns the smaller window now
	}
}

// awNeighborsLocked: another user stream of the session received data in
// the last neighbourFor at a light user's rate (streamLock held). Streams marked background
// (SetAutoWindowBackground: the servers' own control streams) and grown
// ones (other heavy streams) do not count.
func (sess *Session) awNeighborsLocked(self *Stream, now time.Time) bool {
	since := now.Add(-neighbourFor).UnixNano()
	for _, t := range sess.streams {
		if t == self || t.aw == nil || t.aw.bg.Load() || t.aw.grown.Load() {
			continue
		}
		if t.aw.lastRecv.Load() >= since && t.aw.epochRate.Load() < lightEpochBytes {
			return true
		}
	}
	return false
}

// SetAutoWindowBackground marks a stream as the servers' own traffic (a
// control or stats stream): it never caps another stream's window as a
// light neighbour. A no-op without AutoWindow.
func (s *Stream) SetAutoWindowBackground() {
	if s.aw != nil {
		s.aw.bg.Store(true)
	}
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
	s.sess.streamLock.Lock()
	defer s.sess.streamLock.Unlock()
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	a := s.aw
	if a.closed || a.extra == 0 {
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
	nw := max(a.win/2, a.base)
	shrink := now.Sub(a.limitedAt) > hold && now.Sub(a.usedAt) > hold
	if pressured && a.win > a.base+fair && now.Sub(a.limitedAt) > b.holdP {
		shrink = true // the largest windows give way first
	}
	if nb := uint32(a.cfg.NeighborMax); nb > 0 && a.win > max(nb, a.base) && s.sess.awNeighborsLocked(s, now) {
		shrink, nw = true, max(nb, a.base, a.win/2) // a light user came to this link
		b.neighborCuts.Add(1)
	}
	if !shrink || a.numRecv-s.numRead > nw/2 {
		return false
	}
	s.awShrinkLocked(nw, now)
	return a.extra == 0
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
	a.grown.Store(false)
	s.bufferLock.Unlock()
	if d > 0 {
		a.cfg.Budget.give(int64(d))
		atomic.AddInt32(&s.sess.bucket, -int32(d))
	}
	a.cfg.Budget.untrack(s)
}

// minFilter is a windowed minimum: the smallest sample of the current and
// the previous rttFresh period, so a minimum is re-learnt within two periods
// when the path changes, but never from one sample taken inside a queue.
type minFilter struct {
	cur, prev time.Duration
	at        time.Time // start of the current period
}

func (f *minFilter) add(d time.Duration, now time.Time) {
	if f.at.IsZero() || now.Sub(f.at) > rttFresh {
		f.prev, f.cur, f.at = f.cur, d, now
		return
	}
	if d < f.cur {
		f.cur = d
	}
}

func (f *minFilter) get() time.Duration {
	if f.prev > 0 && f.prev < f.cur {
		return f.prev
	}
	return f.cur
}

// sessAW is a session's loop-RTT minimum (AutoWindow only).
type sessAW struct {
	mu sync.Mutex
	f  minFilter
	n  int // samples seen
}

// awRTT folds a probe sample into the session's minimum and returns the
// path's: the smaller of the session's and the whole process's (every link
// of a tunnel crosses the same path, so a queue one session sits in from its
// first sample on is still seen against the others' minimum). ok is false
// for a session's first samples (no minimum to judge them by yet).
func (s *Session) awRTT(sample time.Duration, now time.Time) (time.Duration, bool) {
	g := s.config.AutoWindow.Budget.rttMin(sample, now)
	s.aw.mu.Lock()
	defer s.aw.mu.Unlock()
	s.aw.f.add(sample, now)
	s.aw.n++
	m := s.aw.f.get()
	if g > 0 && g < m {
		m = g
	}
	return m, s.aw.n > 2
}

func (s *Session) awRTTNow() time.Duration {
	s.aw.mu.Lock()
	defer s.aw.mu.Unlock()
	if m := s.aw.f.get(); m > 0 {
		return m
	}
	return rttDefault
}

// rttMin folds a sample into the process-wide minimum and returns it.
func (b *WindowBudget) rttMin(d time.Duration, now time.Time) time.Duration {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	b.rtt.add(d, now)
	return b.rtt.get()
}

// SetAutoWindowOff returns a stream to upstream's fixed window and update
// cadence (what it already grew is given back). For streams whose sender
// relies on the window as back-pressure, e.g. hs2's striped mconn subs,
// where a sub that drains faster than its siblings takes every chunk and the
// stripe never widens. Best called before data flows. No-op without
// AutoWindow.
func (s *Stream) SetAutoWindowOff() {
	if s.aw == nil {
		return
	}
	s.bufferLock.Lock()
	a := s.aw
	d := a.extra
	a.extra, a.win, a.off, a.probeOn, a.pendingUPD = 0, a.base, true, false, false
	a.grown.Store(false)
	s.bufferLock.Unlock()
	if d > 0 {
		a.cfg.Budget.give(int64(d))
		atomic.AddInt32(&s.sess.bucket, -int32(d))
	}
	a.cfg.Budget.untrack(s)
}
