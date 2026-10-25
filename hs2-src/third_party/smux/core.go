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

// The cover: a window past the need for a stream the window holds back, so
// that the need is what it has in flight (Cover, RanUp, CoverNext).
//
// A reader announces its window at each step it reads (StepOf: half the
// window up to the need), and the peer may run the window past the consumed
// count of the latest update it has. When an update leaves, the reader has
// just read the step it announces, so a peer that sends no faster than it
// is read has the window less a step in flight, never more: a stream at the
// need of 2 MiB moves 1 MiB per round trip, not 2 (WINDOW_CORE.fa.md calls
// it the half-window lock). A peer that bursts each update's credit at many
// times the rate gets more (two steps in flight at once); one that paces at
// its delivery rate, as BBR does once it left STARTUP, settles exactly there
// (the window less a step per round trip is its only fixed point). The
// field showed that on the Iran-abroad path, round trip 74 to 82 ms: single
// downloads without the window core 101, 102 and 109 Mbit/s, four at once
// 112, 112, 102 and 112 -- 1 MiB per round trip each -- and every cut of the
// core to its 2 MiB floor the same rate.
//
// So a stream whose cycles run up to its window -- its data paused at the
// limit the peer had: the window held it, not its source and not the path
// -- is announced the need plus a step: 1.5 times the need, 3 MiB, whose
// step is still need/2 (StepOf), so the peer has the need in flight and
// updates leave no more often per byte than at the need, one per MiB under
// 2 MiB (each leaves as one length-shaped TLS record of about 1 KB:
// writeWindowUpdate, WINDOW_CORE.fa.md §8.2). What it holds past the need
// comes from the WindowPool as the window core's growth does (setWindow: a
// claim within its session's share, FreeFor, the session's buffer growing
// by just as much, back after claimHold turnarounds once the window comes
// down, reclaim); where the pool has less left the window gets what it has,
// down to the need (the window as it was), and without a pool, or with
// Config.NoCover, there is no cover. It is the windows' own without the
// window core, and the core's floor: a ceiling bounds it as any window
// (FitCore), and the core takes the covers from its session's streams once
// it found the path full, or while other cores of its peer run (SetCover):
// its floor is then the need as it was (engine wcore.go, cover).
//
// A stream gets its cover at an update ending a cycle that ran up to a
// window of the need or more (RanUp), while its reader keeps up (adaptWindow
// asks for more than the need) and the session may grow (the window core's
// growOK and turnaround brake hold it back as they hold growth past the
// need; without the core both always allow). It keeps it while its reader
// keeps up and until coverMiss updates in a row end cycles that did not run
// up to the window (its source slowed down, or the path is full with the
// need in flight: then the data flows on past each limit), and it loses it
// with its claim when it idles (sweepClaims). No frame is sent for it: a
// window it changes goes out with the stream's next update.

// coverFrames: for the cover a cycle ran up to the window (RanUp) when its
// data paused at the limit for this many of the stream's own frames' time at
// its rate over the cycle, if that is more than its hit gap. Without the
// window core no stream has a hit gap of its own, and 2 ms is a 16 KiB
// frame's time at 65 Mbit/s: a slower stream that flows on would run up to
// its window at every cycle. (The runner gives the core's hits the same
// rule, wcHitFrames.) A peer held by the window pauses for what its pacing
// gains on the rate: BBR's probing phase (1.25 times) leaves a fifth of a
// round trip at each step, 15 ms at 75 ms; three frames at 100 Mbit/s are
// 4 ms.
const coverFrames = 3

// coverMiss: a stream keeps its cover until this many updates in a row end
// cycles that did not run up to the window. A sender that paces at its
// delivery rate pauses at the limit only where its pacing runs ahead of
// what the window lets through: BBR in PROBE_BW does that one round trip in
// eight, two updates in sixteen of a covered window (an update each half a
// round trip); 32 is two of its cycles. A source that slowed down holds the
// cover, half a need from the pool, for 32 steps of reads more.
const coverMiss = 32

// Cover is the window that keeps need in flight for a peer held by it: need
// plus that window's step (StepOf: need/2 past the need, up to 4·need),
// within maxWin.
func Cover(need, maxWin uint32) uint32 {
	w := uint64(need) + uint64(need/2)
	if w > uint64(maxWin) {
		w = uint64(maxWin)
	}
	return uint32(w)
}

// coverTurn: a pause at a limit is the window's (RanUp) only when the data
// past it came back within coverTurn times the stream's previous turnaround
// of the update that let it go (CoverPause). A sender held by the window
// sends on as soon as the update reaches it, so that turnaround is the
// stream's usual one; a sender that stopped for its own reasons -- BBR's
// PROBE_RTT (four packets for 200 ms), a timeout's resend, its source --
// comes back far later. In the model such a stall on 100/20 (2 MiB far past
// its BDP: no cycle held by the window) paused 15 ms at a limit with a
// turnaround of 1.33 s on a 20 ms path: it was covered, the cover's F
// (engine wcore.go, measure) let the core start where base's never did, and
// the core drained for the queue the larger window made.
const coverTurn = 2

// CoverGains says whether a cycle of cycle whose sender paused pause at the
// stream's limit shows a cover worth granting under Config.CoverGain n: the
// pause an n-th of the cycle or more, so that unpaused the sender would have
// sent cycle/(cycle-pause) times as much, n/(n-1) at least (n 5: a quarter
// more); any run-up for n 0.
func CoverGains(n int, pause, cycle time.Duration) bool {
	return n <= 0 || pause > 0 && time.Duration(n)*pause >= cycle
}

// CoverPause says whether a pause at a limit counts for the cover: the
// turnaround of the update that ended it, turn, within coverTurn times the
// stream's previous one, prev (0: none yet, it counts).
func CoverPause(turn, prev time.Duration) bool {
	return prev <= 0 || turn <= coverTurn*prev
}

// RanUp reports whether a cycle ran up to its window, for the cover: its
// data paused at the limit the peer had for pause, at least gap (the
// stream's hit gap) and coverFrames frames' time at the rate it read over
// the cycle (read bytes in cycle, frames of frameSize).
func RanUp(pause, gap, cycle time.Duration, read uint64, frameSize int) bool {
	if pause <= 0 || pause < gap {
		return false
	}
	if read == 0 {
		return true
	}
	return float64(pause) >= coverFrames*float64(frameSize)*float64(cycle)/float64(read)
}

// CoverNext is a stream's cover after a window update, from what it was
// (covered, and miss updates in a row that did not run up to the window
// since it last did): old is the window of the previous update, want what
// adaptWindow made of it, need NeedStreamBuffer, ranUp whether the cycle
// that ends ran up to the window (RanUp), growOK and turnOK the window
// core's growth brakes (FitCore).
func CoverNext(covered bool, miss uint8, old, want, need uint32, ranUp, growOK, turnOK bool) (bool, uint8) {
	if want <= need {
		return false, 0 // its reader does not keep up (or its window is still on its way to the need)
	}
	if ranUp {
		return covered || old >= need && growOK && turnOK, 0
	}
	if !covered || miss+1 >= coverMiss {
		return false, 0
	}
	return true, miss + 1
}

// CoverHold is a stream's cover across the window core's SetCover, at a
// window update, before CoverNext: allowed is whether the core lets covers
// now. A cover the core takes is held (covered false, held the updates
// since); if the core lets covers again within coverMiss updates it comes
// back at once (back), for CoverNext to judge as any cover held. The core
// takes covers when a cut finds the path full, and lets them again when it
// takes that cut back (engine wcore.go undoCut: holes of a path that
// reorders, no loss): the covers are back as the cut found them, not after
// each stream's next run-up to its window. Past coverMiss updates it is
// gone, as a cover that did not run up for as long.
func CoverHold(covered bool, held uint8, allowed bool) (bool, uint8, bool) {
	switch {
	case allowed:
		return covered || held > 0, 0, !covered && held > 0
	case covered:
		return false, 1, false
	case held > 0 && held < coverMiss:
		return false, held + 1, false
	}
	return false, 0, false
}

// FitCore is the window of a window update: want is what adaptWindow made
// of the previous window old, ceil the core's ceiling (0: none), need the
// stream's own window (NeedStreamBuffer, or its cover while it holds one:
// Stream.ownWin), minWin MinStreamBuffer, unread what the stream holds.
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
	lagged   bool   // the cycle that just ended left more than StreamLagTarget/4 unread throughout
	turnSeen int64  // the latest turnaround, ns (0: none yet)
	rxAt     int64  // monoNow of the latest frame received (0: none yet)
	pause    int64  // the longest pause at a limit since our latest update, ns (the cover's RanUp)
	updAt    int64  // monoNow of our latest update (0: none yet)
	updRead  uint64 // readTot at our latest update
	covered  bool   // the stream holds its cover (CoverNext)
	miss     uint8  // updates in a row since it last ran up to its window, while covered
	held     uint8  // updates since the window core took its cover, while it may give it back (CoverHold; 0: none)

	// written under bufferLock, read without it (CoreSnapshot)
	readTot   atomic.Uint64 // bytes read in all (numRead wraps)
	updN      atomic.Uint32 // window updates
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

// SetCover lets the session's streams hold their cover (the need plus a
// step while the window holds a stream back: Cover), the default, or takes
// it from them: the window core does once it found the path full, or while
// other cores of its peer run, its floor NeedStreamBuffer again, and a
// stream without a ceiling then stays within it from its next update on.
// A stream whose cover it took gets it back at its first update after the
// core lets covers again, within coverMiss updates (CoverHold: a cut the
// core takes back). It takes no lock.
func (s *Session) SetCover(ok bool) { s.noCover.Store(!ok) }

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
	big := needOf(s.config) / 4
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
// core allows (FitCore) past the stream's own window (its cover, CoverNext,
// or NeedStreamBuffer), and sets it (setWindow). The caller holds
// bufferLock.
func (s *Stream) fitCore(cfg *Config, old uint32) {
	growOK, turnOK := s.sess.growOK.Load(), s.turnOK()
	c := &s.core
	was := c.covered
	cfgOK := cfg.WindowPool != nil && cfg.NeedStreamBuffer > 0 && !cfg.NoCover
	allowed := cfgOK && !s.sess.noCover.Load()
	if cfgOK {
		var back bool
		if c.covered, c.held, back = CoverHold(c.covered, c.held, allowed); back {
			s.sess.cover.back.Add(1)
			was = true // (given back as the core took it: no grant)
		}
	} else {
		c.covered, c.held = false, 0
	}
	if allowed {
		ran := s.ranUp()
		if ran && !c.covered && !CoverGains(cfg.CoverGain, time.Duration(c.pause), time.Duration(monoNow()-c.updAt)) {
			ran = false // (no cover where it gains less: CoverGain)
		}
		c.covered, c.miss = CoverNext(c.covered, c.miss, old, s.rcvWin, needOf(cfg), ran, growOK, turnOK)
	} else {
		c.miss = 0
	}
	if c.covered != was {
		why := coverTaken
		if allowed {
			why = coverMissed
			if s.rcvWin <= needOf(cfg) {
				why = coverReader
			}
		}
		s.coverChanged(c.covered, why)
	}
	w := FitCore(old, s.rcvWin, s.core.ceil.Load(), s.ownWin(cfg), uint32(cfg.MinStreamBuffer),
		uint32(s.unread), growOK, turnOK)
	s.rcvWin = old
	s.setWindow(cfg, w)
}

// ownWin is as far as the stream's window grows on its reader keeping up,
// without a ceiling of the window core: its cover while it holds one, else
// NeedStreamBuffer. The caller holds bufferLock.
func (s *Stream) ownWin(cfg *Config) uint32 {
	if s.core.covered {
		return Cover(needOf(cfg), uint32(cfg.MaxStreamBuffer))
	}
	return needOf(cfg)
}

// ranUp reports whether the cycle that ends with the update about to be
// queued ran up to the window, for the cover (RanUp). The caller holds
// bufferLock.
func (s *Stream) ranUp() bool {
	c := &s.core
	if c.updAt == 0 {
		return false
	}
	gap := c.hitGap.Load()
	if gap < int64(hitGap) {
		gap = int64(hitGap)
	}
	return RanUp(time.Duration(c.pause), time.Duration(gap), time.Duration(monoNow()-c.updAt),
		c.readTot.Load()-c.updRead, s.frameSize)
}

// coreCap is w within what the window core allows a window that is not
// growing on its own (a braked reader's restored window): its ceiling, or
// its own window without one (ownWin: NeedStreamBuffer, or its cover).
func (s *Stream) coreCap(cfg *Config, w uint32) uint32 {
	lim := s.core.ceil.Load()
	if lim == 0 {
		lim = s.ownWin(cfg)
	}
	if w > lim {
		return lim
	}
	return w
}

// uncover takes the stream's cover away, for why (an idle stream:
// sweepClaims; a stream or session that closed). The caller holds
// bufferLock.
func (s *Stream) uncover(why coverWhy) {
	s.core.held = 0
	if s.core.covered {
		s.core.covered, s.core.miss = false, 0
		s.coverChanged(false, why)
	}
}

// coverWhy is why a stream's cover went (CoverCounters).
type coverWhy uint8

const (
	coverReader coverWhy = iota // its reader fell behind
	coverMissed                 // coverMiss updates in a row did not run up to the window
	coverTaken                  // the window core took it (SetCover), or the config has none
	coverIdle                   // the stream idled (sweepClaims)
	coverClosed                 // the stream or its session closed
)

// coverStats counts what a session's covers did (CoverStats).
type coverStats struct {
	grants, reader, miss, taken, idle, closed, capped, back atomic.Uint32
	now                                                     atomic.Int32
}

// coverChanged counts a cover granted (on) or gone, for why, in the
// session's counters. The caller holds bufferLock.
func (s *Stream) coverChanged(on bool, why coverWhy) {
	st := &s.sess.cover
	if on {
		st.grants.Add(1)
		return
	}
	switch why {
	case coverReader:
		st.reader.Add(1)
	case coverMissed:
		st.miss.Add(1)
	case coverTaken:
		st.taken.Add(1)
	case coverIdle:
		st.idle.Add(1)
	default:
		st.closed.Add(1)
	}
}

// coverCapped counts a window the covers' share of the pool could not cover
// in full (takeCover). The caller holds bufferLock.
func (s *Stream) coverCapped() { s.sess.cover.capped.Add(1) }

// coverHeld counts a stream that starts (on) or stops holding pool memory
// for its cover (coverClaim), in its session's and the pool's count of
// covers held. The caller holds bufferLock.
func (s *Stream) coverHeld(on bool) {
	d := int64(-1)
	if on {
		d = 1
	}
	s.sess.cover.now.Add(int32(d))
	if pool := s.sess.config.WindowPool; pool != nil {
		pool.coverN.Add(d)
	}
}

// CoverCounters is what a session's covers did (Cover), cumulative but Now.
// A cover granted holds pool memory as far as the pool and the covers'
// share have it (setWindow); Now counts the streams that hold some (a claim
// outlives its window by claimHold turnarounds).
type CoverCounters struct {
	Grants uint32 // covers granted
	Reader uint32 // gone as the reader fell behind (it asked the need or less)
	Miss   uint32 // gone after coverMiss updates in a row that did not run up to the window
	Taken  uint32 // taken by the window core (SetCover)
	Back   uint32 // of those, given back as the core let covers again (CoverHold: a cut taken back)
	Idle   uint32 // gone with an idle stream's claim (sweepClaims)
	Closed uint32 // gone with the stream or its session
	Capped uint32 // windows the covers' share of the pool could not cover in full (coverShare)
	Now    int    // streams holding pool memory for a cover now
}

// Lost is how many covers went, for any reason (a cover the core took and
// gave back counted in Taken and Back).
func (c CoverCounters) Lost() uint32 { return c.Reader + c.Miss + c.Taken + c.Idle + c.Closed }

// CoverStats is what the session's covers did. It takes no lock.
func (s *Session) CoverStats() CoverCounters {
	st := &s.cover
	return CoverCounters{Grants: st.grants.Load(), Reader: st.reader.Load(), Miss: st.miss.Load(),
		Taken: st.taken.Load(), Back: st.back.Load(), Idle: st.idle.Load(), Closed: st.closed.Load(),
		Capped: st.capped.Load(), Now: int(st.now.Load())}
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

// updStep is how much the reader reads between two window updates
// (StepOf). The caller holds bufferLock.
func (s *Stream) updStep(cfg *Config) uint32 { return StepOf(needOf(cfg), s.rcvWin) }

// announced records the window update about to be queued, of window past
// the stream's reads: it ends a cycle (updN, and hitN and lagN for what the
// cycle was; the cover's pause starts over), and when it raises the highest
// limit the peer has had, the turnaround of that limit starts. The caller
// holds bufferLock.
func (s *Stream) announced(window uint32) {
	c := &s.core
	now := monoNow()
	c.winNow.Store(window)
	c.updN.Add(1)
	if c.hit {
		c.hitN.Add(1)
		c.hit = false
	}
	if c.lagged {
		c.lagN.Add(1)
		c.lagged = false
	}
	c.pause, c.updAt, c.updRead = 0, now, c.readTot.Load()
	lim := c.updRead + uint64(window)
	if lim <= c.limHigh {
		return // nothing the peer could not send before
	}
	if c.n < turnRing {
		c.ring[(c.head+c.n)%turnRing] = turnMark{at: now, lim: c.limHigh}
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
			c.hit = true
		}
		d := now - k.at
		if p := now - k.reach; p > c.pause && CoverPause(time.Duration(d), time.Duration(c.turnSeen)) {
			c.pause = p
		}
		c.head = (c.head + 1) % turnRing
		c.n--
		c.turnSeen = d
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
