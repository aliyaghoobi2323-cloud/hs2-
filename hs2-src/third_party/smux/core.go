package smux

import (
	"sync/atomic"
	"time"
)

// The window core's fast arm.
//
// A window past NeedStreamBuffer is the window core's to give. The core
// itself runs outside smux (hs2's engine): every few hundred milliseconds it
// reads what a session's streams did (CoreSnapshot), decides how much window
// the link's fast streams may have together, and hands each a ceiling
// (Stream.SetCoreCeil) and the session a turnaround reference
// (Session.SetCoreRef). They meet in atomics: the core takes no stream's
// lock, and the session's only to copy its list of streams.
//
// At each window update from a read, after adaptWindow (unchanged), fitCore
// holds the window to what the core allows (FitCore): within
// NeedStreamBuffer without a ceiling, as it always was; under one, down to
// it at once, and past NeedStreamBuffer up only while the session may grow
// and the stream's latest window turnaround is within the session's
// reference and its brake margin (a slower turnaround is a queue building
// up), at most doubling per update. Past NeedStreamBuffer an update leaves
// each step (StepOf) rather than each half window, and never more often per
// byte than under a window of NeedStreamBuffer.
//
// The turnaround of a window update is how long after it was queued the
// first byte past the limit the peer had before it arrives: the update's
// way to the peer, whatever waited ahead of that byte at the peer and on
// the path, and the byte's way back. Around the knee of a path (the least
// window that fills it) it is the round trip plus the time the excess of
// the window over the knee takes at the path's rate: the queue the window
// builds.
//
// No frame, and no update, is sent for the core: a window it shrinks
// reaches the peer with the stream's next update, at most a step of reads
// later, and a stream whose reader stopped is sent nothing past what it was
// granted anyway.

// updShare: past NeedStreamBuffer an update leaves each 1/updShare of the
// window read, if that is more than half of NeedStreamBuffer (StepOf).
const updShare = 8

// hitGap: a cycle ran up to the window (hit) when the data paused at least
// this long at the limit the peer had before the update that ended it: the
// peer sent all it was allowed and waited for the update (the window held
// it). A peer that had credit to spare sends on without a pause, and a
// receiver's own backlog, which reaching the latest limit would show, says
// nothing about the sender. The window core sets a longer gap for a stream
// whose own frames come further apart (SetCoreHitGap): a few frames' time at
// its rate, else data that flows on at 80 Mbit/s already looks like a pause
// (the rig: 0.87 to 1.0 of the cycles of such streams were hits).
const hitGap = 2 * time.Millisecond

// turnRing: updates whose turnaround is not in yet that a stream keeps
// track of. Past it the newest go unsampled until the oldest are in (a 16
// MiB window steps 8 times a turnaround; dropping the oldest instead would
// drop each before its turnaround came in).
const turnRing = 4

// turnIdle: a stream that received nothing this long was idle; the updates
// pending across the silence go unsampled (turned). A window turnaround of
// a second or more is a pathological queue that the link's other signals
// (its own srtt, the exit's record) show as well.
const turnIdle = time.Second

// StepOf is how much a reader reads between two window updates under a
// window of w: half of it up to need (NeedStreamBuffer); past need, need/2
// or w/updShare if that is more, never more than half. So no more updates
// leave per byte read than under a window of need -- one per MiB under 2
// MiB -- and a sender held to the window is handed at most a step at once.
func StepOf(need, w uint32) uint32 {
	half := w / 2
	if w <= need {
		return half
	}
	step := need / 2
	if f := w / updShare; f > step {
		step = f
	}
	if step > half {
		step = half
	}
	return step
}

// The fine step of a held stream.
//
// A stream whose sender the window holds back moves at most what the window
// leaves free a round trip, and with an update each StepOf read that is less
// than the window: the peer may run w past the consumed count of our latest
// update, and that count lags what was read by up to a step. Data that comes
// in evenly (a sender paced at about its own delivery rate, as a
// window-limited BBR sender is) has the peer reach its limit just as each
// update lands, and the stream settles at (w-step)/RTT: 1 MiB a round trip
// under the 2 MiB window, half of it -- the "half-window lock". The field
// (Iran-abroad, 74 to 82 ms, no window core) measured single downloads at
// 101, 102 and 109 Mbit/s, 1 MiB a round trip to within a few percent; the
// rig 84 Mbit/s at 100 ms (WINDOW_CORE.fa.md §5.6); the window-core model's
// base 83.7 on 300/100 (TestWcoreModelTable); and every cut of the window
// core to the 2 MiB floor lands on the same rate.
//
// A held stream steps at FineStepOf instead: under 2 MiB the peer has 7/8 of
// the window free as each update lands, not 1/2 (1.75 MiB a round trip, 196
// instead of 112 Mbit/s at 75 ms), and the credit comes in bursts of 256 KiB
// rather than 1 MiB (the base's 1 MiB ones overflow queues of 0.6 to 2 MB:
// z5-core-controller-notes.fa.md). Each update leaves as one length-shaped
// TLS record of about 1 KB (writeUpdate), so the fine step is bounded per
// byte -- an eighth of the window, never under an eighth of the need, 256
// KiB: at most 4 updates per MiB read, where the coarse step sends 1 under 2
// MiB (WINDOW_CORE.fa.md §8.2) -- and only a stream the window holds takes
// it: a slow reader, a slow source, an interactive stream that never runs up
// to its window, and one that slowed down keep StepOf. Held, a stream sends
// at most updShare updates per window read: about updShare a round trip
// (107 a second at 75 ms), whatever its rate.
//
// The window core's windows past need step at an eighth of the window, or
// need/2 if more (StepOf): a 4 MiB window leaves its peer 3/4 of it, and the
// core's growth from the floor first meets a 1 MiB step at every size up to
// 8 MiB. Held, they step at an eighth too, so a window carries 7/8 of itself
// a round trip at every size, and B and the rate it carries grow together
// (the core's STARTUP takes a rate that did not grow with B for the knee:
// with the fine step at the floor alone, a 2 -> 3.8 MiB growth carried 14%
// more and STARTUP slowed, 781 -> 732 Mbit/s on the model's 1000/100). The
// core reads each stream's step (CoreSample.Step) for its knee's half step.
//
// Held: an update that ends a cycle in which the data paused at a limit for
// hitGap or more (paused: the peer had sent all it was allowed and waited
// for the update; turned) makes the stream held, if
//
//   - its reader kept up (not lagged): a reader that left a backlog all
//     through the cycle is the limit, not the window, and steps no faster;
//   - its window is at the need or past it: a window adaptWindow keeps
//     under it is sized to its reader's pace (a reader that runs dry
//     doubles it each cycle), and finer steps would only bring a backlog
//     that shrinks it again;
//   - and it is fast: it read, over the latest round trip or more, at least
//     fineKeep of what the coarse step lets through a round trip (w-StepOf).
//     The round trip is the least turnaround of its life (turnLow): none is
//     shorter than the round trip, and nearly every stream has one close to
//     it from its start, when it ran up to the 256 KiB the peer starts with.
//     A pause alone does not tell a sender the window holds from a source
//     that sends in clumps with gaps of hitGap or more (under ~65 Mbit/s of
//     16 KiB frames), nor does the turnaround of a stream that comes in
//     evenly: that is (w-step)/rate whether the window holds it or its
//     source does.
//
// The pause is hitGap's, not the gap the window core sets a stream for its
// hits (SetCoreHitGap): the core sets it on the links it watches while it is
// OFF too, and a link the core leaves OFF (the blind sides, a long round
// trip with a clean path) must step as one without the core does
// (TestWcoreStep1ModelBlindSidesAreBase, TestWcoreRedBlindHoley).
//
// It stays held while its reader keeps up, its window stays at the need or
// past it, and it stays fast: at the fine step a stream the window holds
// need not pause at its limit any more (the point of it), so no hit can keep
// it held, but its pace can. One that falls under that (its source or its
// path slowed down, its reader stopped) steps at StepOf again, and one that
// a brake takes back (checkLag) at once.
//
// Only an update that ends a cycle (each StepOf read since the previous one,
// as before; the first, a resend, a restore, a brake, a window the claim
// sweep took down) resizes the window (adaptWindow, fitCore) and counts for
// the core (updN, hitN, lagN). The fine updates in between announce the
// consumed count with the window as it is; a turnaround starts at each, as
// at any update that raises the peer's limit, and a pause at its limit is a
// hit of the cycle it is in (announced). So the adaptive window, the brakes,
// the pool and the window core see the cycles they were tuned on, held or
// not, and a sender that is not held sees no change at all.

// fineKeep: a stream is fast (held) while it reads at least this share of
// w-StepOf(w) a round trip. Under it, the coarse step would hold it back by
// less than a quarter: what it gains is not worth four updates per MiB.
const fineKeep = 0.75

// fineOff holds no stream (the coarse step only, as before the fine one): for
// tests that compare the two.
var fineOff = false

// PacedFast reports whether a stream that read n bytes over el, the least
// turnaround of its life low, is fast under a window of w (paced): at least
// fineKeep of w-StepOf(need, w) a turnaround. (The window-core model's
// streams take it as smux's do.)
func PacedFast(need, w uint32, n uint64, el, low time.Duration) bool {
	return float64(n)*float64(low) >= fineKeep*float64(w-StepOf(need, w))*float64(el)
}

// FineStepOf is the step of a held stream under a window of w: w/updShare,
// but no less than need/updShare (at most updShare updates per need read: 4
// per MiB under a need of 2 MiB), and never more than StepOf. From a window
// of 8 times need/updShare on it is StepOf.
func FineStepOf(need, w uint32) uint32 {
	step := StepOf(need, w)
	f := w / updShare
	if m := need / updShare; f < m {
		f = m
	}
	if f < step {
		return f
	}
	return step
}

// FitCore is the window of a window update: want is what adaptWindow made
// of the previous window old, ceil the core's ceiling (0: none), need
// NeedStreamBuffer, minWin MinStreamBuffer, unread what the stream holds.
//
//   - No ceiling: want, within need (the window as it always was).
//   - Growth past need (want > old and want > need) only while growOK (the
//     session may grow) and turnOK (the stream's turnaround is within the
//     reference), at most to twice old; growth up to need is the reader's
//     own, as without a ceiling.
//   - Past the ceiling: the ceiling at once (a window the core drains), but
//     no lower than minWin and what the stream holds, as a brake does
//     (Stream.checkLag).
//
// It never returns more than want: the core only holds a window back.
func FitCore(old, want, ceil, need, minWin, unread uint32, growOK, turnOK bool) uint32 {
	if ceil == 0 {
		if want > need {
			return need
		}
		return want
	}
	w := want
	if w > old && w > need {
		lim := old
		if lim < need {
			lim = need
		}
		if g := 2 * uint64(old); growOK && turnOK && g > uint64(lim) {
			lim = ^uint32(0)
			if g < uint64(lim) {
				lim = uint32(g)
			}
		}
		if w > lim {
			w = lim
		}
	}
	if w > ceil {
		floor := minWin
		if unread > floor {
			floor = unread
		}
		if floor > w {
			floor = w
		}
		w = ceil
		if w < floor {
			w = floor
		}
	}
	return w
}

// streamCore is a stream's side of the window core.
type streamCore struct {
	ceil   atomic.Uint32 // the core's ceiling for the window (0: none; SetCoreCeil)
	hitGap atomic.Int64  // ns: the pause at the limit that makes a hit, past hitGap (SetCoreHitGap; 0: hitGap)

	// under bufferLock
	ring     [turnRing]turnMark // updates whose turnaround is not in yet, oldest at head
	head, n  int
	limHigh  uint64 // the highest limit announced so far (readTot plus window)
	hit      bool   // a turnaround since our latest update came after a pause at its limit
	paused   bool   // ... after a pause of hitGap or more, whatever the core's gap: since the cycle began (hold)
	lagged   bool   // the cycle that just ended left more than StreamLagTarget/4 unread throughout
	turnSeen int64  // the latest turnaround, ns (0: none yet)
	rxAt     int64  // monoNow of the latest frame received (0: none yet)
	hitAt    int64  // monoNow of the latest pause at a limit (hit; 0: none yet)
	turnLow  int64  // the least turnaround of the stream's life, ns (0: none yet): its round trip, or more
	fast     bool   // over the latest turnLow or more it read at least fineKeep of w-StepOf(w) (paced)
	paceAt   int64  // monoNow when the current measure of its pace began (paced)
	paceRead uint64 // readTot then

	// written under bufferLock, read without it (CoreSnapshot)
	held      atomic.Bool   // the window holds the stream back: it steps at FineStepOf (hold)
	readTot   atomic.Uint64 // bytes read in all (numRead wraps)
	fineN     atomic.Uint32 // fine updates: within a cycle, none of the counts below (announced)
	updN      atomic.Uint32 // window updates that ended a cycle
	hitN      atomic.Uint32 // of those, ones ending a cycle that ran up to the window (hit)
	lagN      atomic.Uint32 // of those, ones ending a cycle whose reader lagged (lagged)
	turnMin   atomic.Int64  // least turnaround since the last snapshot, ns (0: none)
	turnLast  atomic.Int64  // latest one since the last snapshot, ns (0: none)
	winNow    atomic.Uint32 // the window of our latest update
	poolShort atomic.Uint32 // windows the WindowPool could not cover in full
	brakes    atomic.Uint32 // braking updates (checkLag)
	brakeAt   atomic.Int64  // monoNow of the latest (0: none)

	snapHit uint32 // hitN at the last snapshot (under the session's snapLock)
	pwSent  uint32 // numWritten at the last PeerWindows (under the session's pwLock)
}

// turnMark is one update whose turnaround is not in yet.
type turnMark struct {
	at    int64  // monoNow when it was queued
	lim   uint64 // the highest limit announced before it
	reach int64  // monoNow when what was received came within a frame of lim (0: not yet)
}

// SetCoreCeil sets the window core's ceiling for the stream's window, in
// bytes, at most MaxStreamBuffer; 0 (the default) leaves the window within
// NeedStreamBuffer. It takes effect at the stream's next window update from
// a read (FitCore), on an adaptive stream (Config.MinStreamBuffer) only. It
// takes no lock.
func (s *Stream) SetCoreCeil(ceil uint32) {
	if m := uint32(s.sess.config.MaxStreamBuffer); ceil > m {
		ceil = m
	}
	s.core.ceil.Store(ceil)
}

// CoreCeil is the ceiling SetCoreCeil set.
func (s *Stream) CoreCeil() uint32 { return s.core.ceil.Load() }

// SetCoreHitGap sets how long the stream's data must pause at its limit for
// the cycle to count as a hit (CoreSample.Hits): gap, if it is longer than
// hitGap (2 ms). The window core sets a few frames' time at the stream's
// rate. It takes no lock.
func (s *Stream) SetCoreHitGap(gap time.Duration) { s.core.hitGap.Store(int64(gap)) }

// SetCoreRef sets the session's turnaround reference for the window core:
// a stream's window grows past NeedStreamBuffer only while its latest
// turnaround is at most turnRef+qBrake (turnRef 0: no reference, any
// turnaround will do), and only while growOK. It also sets how long a claim
// on the WindowPool outlives a window that came down (claimHold). It takes
// no lock.
func (s *Session) SetCoreRef(turnRef, qBrake time.Duration, growOK bool) {
	s.turnRef.Store(int64(turnRef))
	s.qBrake.Store(int64(qBrake))
	s.growOK.Store(growOK)
}

// CoreSample is what one stream did, as CoreSnapshot found it. The counts
// are cumulative (the core takes differences); the turnarounds are since
// the previous snapshot.
type CoreSample struct {
	Stream    *Stream // for SetCoreCeil
	ID        uint32
	Ceil      uint32        // the ceiling in force (SetCoreCeil)
	Win       uint32        // the window of the stream's latest update
	Read      uint64        // bytes its reader has taken
	Upds      uint32        // window updates sent
	Hits      uint32        // of those, ones ending a cycle in which the peer waited at the window (hitGap)
	Lags      uint32        // of those, ones ending a cycle whose reader left over StreamLagTarget/4 unread throughout
	TurnMin   time.Duration // least turnaround since the previous snapshot (0: none)
	TurnLast  time.Duration // latest turnaround since the previous snapshot (0: none)
	PoolShort uint32        // windows the WindowPool could not cover in full
	Brakes    uint32        // braking updates (a reader that fell behind)
	Braked    bool          // braked within the last brakeDrain
	// Step is what its reader reads between two updates now: FineStepOf
	// while the window holds it (held), StepOf otherwise. The window core's
	// knee holds half a step of each fast stream (H_step): under the 2 MiB
	// floor a held stream's is 128 KiB, not 512.
	Step uint32
}

// CoreCounters is what the whole session did, cumulative.
type CoreCounters struct {
	Parks     uint32 // its reader ran out of buffer (Parks)
	Brakes    uint32 // braking updates (Brakes)
	Borrows   uint32 // of Parks, the buffer grew from the pool instead (Borrows)
	PoolShort uint32 // windows the WindowPool could not cover in full
	Extra     int    // its buffer past MaxReceiveBuffer now (Extra)
	Rcvd      uint64 // data bytes received
}

// CoreSnapshot appends to dst[:0] a sample of each stream whose window is a
// quarter of NeedStreamBuffer or more, or that ran up to its window since
// the previous snapshot, and returns it with the session's counters. Every
// stream's turnarounds restart with it, so the session's snapshots are best
// taken by one caller. It holds the session's stream lock only to copy the
// list of streams, and no stream's lock at all.
func (s *Session) CoreSnapshot(dst []CoreSample) ([]CoreSample, CoreCounters) {
	s.snapLock.Lock()
	defer s.snapLock.Unlock()
	list := s.snapList[:0]
	s.streamLock.Lock()
	for _, st := range s.streams {
		list = append(list, st)
	}
	s.streamLock.Unlock()
	need := needOf(s.config)
	big := need / 4
	now := monoNow()
	dst = dst[:0]
	for i, st := range list {
		c := &st.core
		tmin, tlast := c.turnMin.Swap(0), c.turnLast.Swap(0)
		win, hits := c.winNow.Load(), c.hitN.Load()
		fresh := hits != c.snapHit
		c.snapHit = hits
		list[i] = nil
		if win < big && !fresh {
			continue
		}
		step := StepOf(need, win)
		if c.held.Load() {
			step = s.fineStepOf(need, win)
		}
		at := c.brakeAt.Load()
		dst = append(dst, CoreSample{
			Stream:    st,
			ID:        st.id,
			Ceil:      c.ceil.Load(),
			Win:       win,
			Read:      c.readTot.Load(),
			Upds:      c.updN.Load(),
			Hits:      hits,
			Lags:      c.lagN.Load(),
			TurnMin:   time.Duration(tmin),
			TurnLast:  time.Duration(tlast),
			PoolShort: c.poolShort.Load(),
			Brakes:    c.brakes.Load(),
			Braked:    at != 0 && now-at < int64(brakeDrain),
			Step:      step,
		})
	}
	s.snapList = list[:0]
	return dst, CoreCounters{
		Parks:     atomic.LoadUint32(&s.parks),
		Brakes:    atomic.LoadUint32(&s.brakes),
		Borrows:   atomic.LoadUint32(&s.borrows),
		PoolShort: atomic.LoadUint32(&s.poolShorts),
		Extra:     int(atomic.LoadInt32(&s.extra)),
		Rcvd:      s.rcvd.Load(),
	}
}

// PeerWindows is the sum of the windows the peer grants the streams that
// sent data since the previous call: what this end may have in flight on
// the link. The window core's pacing-cap experiment keeps the link socket's
// pacing at least this much per round trip, so the cap never holds a sender
// under what the receiver's windows allow.
func (s *Session) PeerWindows() uint64 {
	s.pwLock.Lock()
	defer s.pwLock.Unlock()
	var sum uint64
	s.streamLock.Lock()
	for _, st := range s.streams {
		if w := atomic.LoadUint32(&st.numWritten); w != st.core.pwSent {
			st.core.pwSent = w
			sum += uint64(atomic.LoadUint32(&st.peerWindow))
		}
	}
	s.streamLock.Unlock()
	return sum
}

// fitCore holds the window adaptWindow just made of old to what the window
// core allows (FitCore), and sets it (setWindow). The caller holds
// bufferLock.
func (s *Stream) fitCore(cfg *Config, old uint32) {
	w := FitCore(old, s.rcvWin, s.core.ceil.Load(), needOf(cfg), uint32(cfg.MinStreamBuffer),
		uint32(s.unread), s.sess.growOK.Load(), s.turnOK())
	s.rcvWin = old
	s.setWindow(cfg, w)
}

// coreCap is w within what the window core allows a window that is not
// growing on its own (a braked reader's restored window): its ceiling, or
// NeedStreamBuffer without one.
func (s *Stream) coreCap(cfg *Config, w uint32) uint32 {
	lim := s.core.ceil.Load()
	if lim == 0 {
		lim = needOf(cfg)
	}
	if w > lim {
		return lim
	}
	return w
}

// turnOK reports whether the stream's latest turnaround is within its
// session's reference and brake margin (or there is no reference, or no
// turnaround yet). The caller holds bufferLock.
func (s *Stream) turnOK() bool {
	ref := s.sess.turnRef.Load()
	return ref <= 0 || s.core.turnSeen <= ref+s.sess.qBrake.Load()
}

// claimTurn is the turnaround a claim is held for claimHold of after its
// window came down: the stream's latest, or its session's reference if
// longer, claimTurnDefault while there is neither. The caller holds
// bufferLock.
func (s *Stream) claimTurn() time.Duration {
	t := s.core.turnSeen
	if ref := s.sess.turnRef.Load(); ref > t {
		t = ref
	}
	if t <= 0 {
		return claimTurnDefault
	}
	return time.Duration(t)
}

// poolShort counts a window the pool could not cover in full. The caller
// holds bufferLock.
func (s *Stream) poolShort() {
	s.core.poolShort.Add(1)
	atomic.AddUint32(&s.sess.poolShorts, 1)
}

// updStep is how much the reader reads between two updates that end a
// cycle (StepOf). The caller holds bufferLock.
func (s *Stream) updStep(cfg *Config) uint32 { return StepOf(needOf(cfg), s.rcvWin) }

// fineStep is how much a held reader reads between two updates of any kind
// (fineStepOf). The caller holds bufferLock.
func (s *Stream) fineStep(cfg *Config) uint32 { return s.sess.fineStepOf(needOf(cfg), s.rcvWin) }

// fineStepOf is FineStepOf on the dialing side (Client: hs2's edge); on the
// accepting side, past need, StepOf. A window past need is the window core's,
// and the edge's core reads the exit's record of the sender's socket (its
// queue, its resends: q_snd), while the exit's core (uploads, the lab knob
// WcoreUpload only) has no record of the edge's and is blind to the queue a
// window that carries 7/8 of itself instead of 3/4 builds: in the model its
// 300/100 behind a 20 ms queue went from 0.92 to 2.73% resent, past the loss
// rule's drain (TestWcoreRedBlindHoley). Up to need, as without the core.
func (s *Session) fineStepOf(need, w uint32) uint32 {
	if w > need && !s.client {
		return StepOf(need, w)
	}
	return FineStepOf(need, w)
}

// hold judges, at an update that ends a cycle, whether the window holds the
// stream back (held: see "The fine step of a held stream" above): a pause at
// a limit makes a fast stream held whose reader kept up and whose window is
// at the need or past it; a stream that is not all three is not held. The
// caller holds bufferLock, has set lagged, has not yet resized the window,
// and has not announced the update (announced takes the pause).
func (s *Stream) hold(cfg *Config) {
	c := &s.core
	s.paced(cfg)
	switch {
	case c.lagged || !c.fast || s.rcvWin < needOf(cfg) || fineOff:
		c.held.Store(false)
	case c.paused:
		c.held.Store(true)
	}
}

// heldOn keeps a held stream held, at a fine update, while it stays fast. The
// caller holds bufferLock.
func (s *Stream) heldOn(cfg *Config) {
	s.paced(cfg)
	if !s.core.fast {
		s.core.held.Store(false)
	}
}

// paced measures the stream's pace: each time turnLow or more has passed
// since the measure began, whether it read at least fineKeep of w-StepOf(w)
// a turnLow over that time (fast), and the next measure begins. No
// turnaround yet: not fast. The caller holds bufferLock.
func (s *Stream) paced(cfg *Config) {
	c := &s.core
	now := monoNow()
	el := now - c.paceAt
	if c.turnLow <= 0 || el < c.turnLow {
		return
	}
	read := c.readTot.Load()
	c.fast = PacedFast(needOf(cfg), s.rcvWin, read-c.paceRead, time.Duration(el), time.Duration(c.turnLow))
	c.paceAt, c.paceRead = now, read
}

// announced records the window update about to be queued, of window past
// the stream's reads. One that ends a cycle (fine false) counts it (updN,
// and hitN and lagN for what the cycle was); a fine one (a held stream's,
// within a cycle) counts no cycle, and a hit or a lag seen meanwhile counts
// for the cycle it is in. Either, when it raises the highest limit the peer
// has had, starts the turnaround of that limit: a pause at a fine update's
// limit is the cycle's hit. The caller holds bufferLock.
//
// A held stream's cycle is a hit also when its data paused at a limit
// within its latest turnaround before the cycle ends. Handed the window a
// fine step at a time, a sender the window holds sends it in one train a
// round trip, at its path's rate, and pauses at the end of each: once a
// round trip, with two cycles of a 2 MiB window ending in between. Under
// the coarse step each half window was a train of its own with a pause
// after it, and the window core reads its hits per cycle (wcHitShare 0.5:
// TestFineStepHeldStreamStillHits measured 1.0 of the cycles coarse, 0.5
// fine, through a path three times the window's rate).
func (s *Stream) announced(window uint32, fine bool) {
	c := &s.core
	c.winNow.Store(window)
	if fine {
		c.fineN.Add(1)
	} else {
		c.updN.Add(1)
		if c.hit || c.held.Load() && c.hitAt != 0 && monoNow()-c.hitAt < c.turnSeen {
			c.hitN.Add(1)
		}
		c.hit, c.paused = false, false
		if c.lagged {
			c.lagN.Add(1)
			c.lagged = false
		}
	}
	lim := c.readTot.Load() + uint64(window)
	if lim <= c.limHigh {
		return // nothing the peer could not send before
	}
	if c.n < turnRing {
		c.ring[(c.head+c.n)%turnRing] = turnMark{at: monoNow(), lim: c.limHigh}
		c.n++
	}
	c.limHigh = lim
}

// turned takes the turnarounds of the updates whose earlier limit what the
// stream has received now passes -- the peer could not send it before the
// update reached it -- and notes a cycle that ran up to the window: one
// whose data paused at that limit (from within a frame of it, the peer
// sends whole frames) for its hit gap or more before it went on. The caller
// holds bufferLock, just after unread grew.
//
// A stream that received nothing for turnIdle or longer was idle: its
// source paused (a keep-alive connection between transfers, a player
// between segments), its reader stopped, or its path did. The updates
// pending across such a silence go unsampled: their "turnaround" would be
// the silence, however long (the end of a transfer leaves a step or two of
// them pending, their limits past its last byte).
func (s *Stream) turned() {
	c := &s.core
	now := monoNow()
	if c.rxAt != 0 && now-c.rxAt >= int64(turnIdle) {
		c.n = 0
	}
	c.rxAt = now
	if c.n == 0 {
		return
	}
	near := c.readTot.Load() + uint64(s.unread) + uint64(s.frameSize)
	if near < c.ring[c.head].lim {
		return
	}
	recv := near - uint64(s.frameSize)
	gap := c.hitGap.Load()
	if gap < int64(hitGap) {
		gap = int64(hitGap)
	}
	for c.n > 0 {
		k := &c.ring[c.head]
		if k.reach == 0 && near >= k.lim {
			k.reach = now
		}
		if recv <= k.lim {
			return
		}
		if now-k.reach >= gap {
			c.hit, c.hitAt = true, now
		}
		if now-k.reach >= int64(hitGap) {
			c.paused = true
		}
		d := now - k.at
		c.head = (c.head + 1) % turnRing
		c.n--
		c.turnSeen = d
		if c.turnLow == 0 || d < c.turnLow {
			c.turnLow = d
		}
		c.turnLast.Store(d)
		for {
			m := c.turnMin.Load()
			if (m != 0 && m <= d) || c.turnMin.CompareAndSwap(m, d) {
				break
			}
		}
	}
}

// braked notes a braking update (checkLag). The caller holds bufferLock.
func (s *Stream) braked() {
	s.core.brakes.Add(1)
	s.core.brakeAt.Store(monoNow())
}
