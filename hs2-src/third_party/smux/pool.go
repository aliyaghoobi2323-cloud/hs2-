package smux

import (
	"sync/atomic"
	"time"
)

// Windows past NeedStreamBuffer, and the pool their buffer comes from
// (Config.NeedStreamBuffer, Config.WindowPool).
//
// A stream moves at most about its window per round trip, so a fixed ceiling
// caps one fast connection on a long path: 2 MiB is ~90 Mbit/s at 150 ms. A
// larger ceiling for every stream is what a stream's reader can stop reading
// under: what its window granted still arrives, and enough of it empties the
// session's buffer for every other stream. So past NeedStreamBuffer a window
// grows only as far as the window core lets it (core.go: a ceiling the
// session's owner sets per stream, 0 by default), and, with a WindowPool,
// only by what the pool has left: the session's buffer grows by just as much
// (the stream's claim) while the stream may still be sent that much or holds
// that much unread past NeedStreamBuffer, and gives it back only as far as it
// is free. A stream without a ceiling never holds more than
// NeedStreamBuffer; the sessions sharing a pool hold at most its size past
// their MaxReceiveBuffer; and the other streams keep at least the room they
// would have had with every window within NeedStreamBuffer, while a large
// window's data comes in and after its reader stopped. A stream that idles
// gives its larger window back with an update (sweepClaims), so the next
// transfer cannot land more than NeedStreamBuffer uncovered. A session whose
// buffer runs full anyway (what was in flight when its readers slowed down)
// borrows from the same pool (borrow), whatever its windows.
//
// A window grows past NeedStreamBuffer only as far as its session holds no
// more of the pool than the pool has left for the others (FreeFor: half the
// pool for a session alone). A reader that stops after a fast transfer keeps
// what its larger window let in, and its claim with it, for as long as its
// connection stays open; two such links once held the whole pool, and every
// other link could neither grow a window nor borrow. (A fixed cap of half
// the pool per session does not prevent that: two sessions at the cap leave
// nothing.)

// claimHold: a claim on the pool outlives the window it covered by this many
// window turnarounds (claimTurn): what the peer sent under the larger window
// before our update reached it is still on its way.
const claimHold = 2

// claimTurnDefault is the turnaround claimHold counts while the stream has
// seen none and its session has no reference (core.go).
const claimTurnDefault = 500 * time.Millisecond

// borrowShare: a session that runs full borrows 1/borrowShare of
// MaxReceiveBuffer from the pool at a time (borrow).
const borrowShare = 8

// borrowMax: a session borrows up to borrowMax times MaxReceiveBuffer
// (borrow). What a link has in flight when a dozen of its readers slow down
// at once can be more than its buffer: a 200 Mbit/s path at 300 ms carries
// ~10 MB, and a brake takes another round trip to reach the sender. With
// one MaxReceiveBuffer such a link still ran full and stalled up to 3 s in
// half the runs, with two never.
const borrowMax = 2

// spareRoom: the session gives its spare back only as far as 1/spareRoom
// of MaxReceiveBuffer stays free (returnSpare).
const spareRoom = 4

// claimIdle: a stream that has read nothing for this long gives its window
// past NeedStreamBuffer back, then the claim it covered (sweepClaims).
const claimIdle = 5 * time.Second

// closeGrace: a session that closed gives back what its streams still claim,
// and its spare, this long after, whatever their apps do (dropClaims). Until
// then a claim covers what its stream holds unread past NeedStreamBuffer,
// and the spare what the streams hold past MaxReceiveBuffer and their claims
// (what the session borrowed as it ran full), and each goes back as the
// streams let the data go: the process's memory stays within what the pool
// accounts for while a dead link's relays drain (hs2's relayDieGrace, 5 s);
// a stream its app never closes would keep it for good. A variable for the
// tests.
var closeGrace = 2 * claimIdle

// WindowPool is the excess past Config.NeedStreamBuffer that the windows of
// the sessions sharing it may hold at once (Config.WindowPool), in bytes.
type WindowPool struct{ free atomic.Int64 }

// NewWindowPool returns a pool of size bytes.
func NewWindowPool(size int) *WindowPool {
	p := new(WindowPool)
	p.free.Store(int64(size))
	return p
}

// Free is what the pool has left.
func (p *WindowPool) Free() int { return int(p.free.Load()) }

// FreeFor is what a session that holds held of the pool may still take for
// its windows: as much as leaves it holding no more than the pool then has
// left for the others (held+k ≤ free−k).
func (p *WindowPool) FreeFor(held int) int {
	if k := (p.free.Load() - int64(held)) / 2; k > 0 {
		return int(k)
	}
	return 0
}

// takeFor takes up to n bytes for the windows of a session that holds held
// of the pool, within FreeFor(held), and returns how many it took.
func (p *WindowPool) takeFor(held, n int) int {
	for {
		f := p.free.Load()
		k := (f - int64(held)) / 2
		if int64(n) < k {
			k = int64(n)
		}
		if k <= 0 {
			return 0
		}
		if p.free.CompareAndSwap(f, f-k) {
			return int(k)
		}
	}
}

// take takes up to n bytes from the pool and returns how many it took.
func (p *WindowPool) take(n int) int {
	for {
		f := p.free.Load()
		if f <= 0 || n <= 0 {
			return 0
		}
		k := int64(n)
		if k > f {
			k = f
		}
		if p.free.CompareAndSwap(f, f-k) {
			return int(k)
		}
	}
}

func (p *WindowPool) give(n int) { p.free.Add(int64(n)) }

// needOf is the window a stream grows to on its reader keeping up alone:
// NeedStreamBuffer, or MaxStreamBuffer without one.
func needOf(cfg *Config) uint32 {
	if cfg.NeedStreamBuffer > 0 {
		return uint32(cfg.NeedStreamBuffer)
	}
	return uint32(cfg.MaxStreamBuffer)
}

// setWindow sets the window to w; with a WindowPool, past NeedStreamBuffer
// only as far as the stream's claim on the pool covers it, the claim taking
// what more the pool has left for its session (FreeFor). The caller holds
// bufferLock.
func (s *Stream) setWindow(cfg *Config, w uint32) {
	if pool := cfg.WindowPool; pool != nil && w > uint32(cfg.NeedStreamBuffer) {
		if want := int(w) - cfg.NeedStreamBuffer - s.claim; want > 0 {
			got := 0
			select {
			case <-s.die: // closed: its claim is given back for good
			default:
				got = pool.takeFor(int(atomic.LoadInt32(&s.sess.extra)), want)
			}
			if got > 0 {
				if s.claim == 0 {
					s.readAt = monoNow()
				}
				s.claim += got
				s.sess.grow(got)
			}
			if got < want {
				s.poolShort()
			}
			w -= uint32(want - got)
		}
	}
	if w < s.rcvWin && s.claim > 0 {
		s.claimDown = 0 // the hold runs from the latest step down
	}
	s.rcvWin = w
	s.reclaim(cfg)
}

// reclaim lets go of the part of the stream's claim that neither its window
// nor what it holds unread needs past NeedStreamBuffer, claimHold turnarounds
// after the window last came down (what was sent under a larger one may
// still arrive), into the session's spare. What a stream holds unread past
// NeedStreamBuffer stays its own (a reader that stopped after a fast
// transfer holds what its larger window let in): were it spare, the session
// would give it back while the data is still there, and the other streams
// would have that much less room than with every window within
// NeedStreamBuffer. The caller holds bufferLock.
//
// Once the session closed nothing more can arrive: the claim keeps only what
// the stream holds unread, and lets go of the rest at once, straight back to
// the pool (letGo).
func (s *Stream) reclaim(cfg *Config) {
	if s.claim == 0 {
		return
	}
	closed := s.sess.IsClosed()
	keep := int(s.rcvWin)
	if closed {
		keep = 0
	}
	if s.unread > keep {
		keep = s.unread
	}
	keep -= cfg.NeedStreamBuffer
	if keep < 0 {
		keep = 0
	}
	if s.claim <= keep {
		s.claimDown = 0
		return
	}
	if !closed {
		now := monoNow()
		if s.claimDown == 0 {
			s.claimDown = now + claimHold*int64(s.claimTurn())
		}
		if now < s.claimDown {
			return
		}
	}
	s.claimDown = 0
	s.sess.letGo(s.claim - keep)
	s.claim = keep
}

// sweepClaims takes the windows of the streams that have read nothing for
// claimIdle down: a connection kept open after a fast transfer must not keep
// the pool from the others. The peer may still send such a stream all its
// window granted, and the next transfer may find its reader slow or gone,
// so the window comes down to NeedStreamBuffer with an update first (as a
// brake's, Stream.checkLag), its claim covering what is on its way for
// claimHold turnarounds; a later sweep lets go of the claim, into the spare,
// all but what the stream holds unread past NeedStreamBuffer (reclaim). Its
// next updates take from the pool again what it then needs. It runs at the
// keepalive tick.
func (s *Session) sweepClaims() {
	if atomic.LoadInt32(&s.extra) == 0 {
		return // no claims
	}
	now := monoNow()
	var down []*Stream
	s.streamLock.Lock()
	for _, st := range s.streams {
		st.bufferLock.Lock()
		if st.claim > 0 && now-st.readAt >= int64(claimIdle) {
			if st.rcvWin > uint32(s.config.NeedStreamBuffer) {
				st.setWindow(s.config, uint32(s.config.NeedStreamBuffer))
				st.incr, st.cyc = 0, 0
				down = append(down, st)
			} else {
				st.reclaim(s.config)
			}
		}
		st.bufferLock.Unlock()
	}
	s.streamLock.Unlock()
	for _, st := range down {
		go st.windowDown()
	}
	s.returnSpare()
}

// windowDown announces a window sweepClaims took down, as a brake does:
// not bound by the reader's deadline, but not for long either.
func (s *Stream) windowDown() {
	select {
	case <-s.die:
	default:
		t := time.NewTimer(brakeWait)
		s.writeWindowUpdate(t.C)
		t.Stop()
	}
}

// dropClaim lets go of the whole claim, as the stream lets go of its buffers
// (recycleTokens; with die closed: it takes no more), or closeGrace after its
// session closed (dropClaims). The caller holds bufferLock.
func (s *Stream) dropClaim() {
	if s.claim > 0 {
		s.sess.letGo(s.claim)
		s.claim = 0
	}
}

// letGo takes n bytes of a stream's claim off it: into the session's spare,
// or, the session closed, straight back to the pool, its buffer with it
// (nothing more comes in: the spare is only what still holds data,
// returnSpare).
func (s *Session) letGo(n int) {
	if n <= 0 {
		return
	}
	s.spareLock.Lock()
	defer s.spareLock.Unlock()
	if s.IsClosed() {
		atomic.AddInt32(&s.extra, -int32(n))
		atomic.AddInt32(&s.bucket, -int32(n))
		s.config.WindowPool.give(n)
		return
	}
	s.spareUp(n)
}

// dropClaims gives back what the streams of a closed session still claim,
// and its spare, closeGrace after it closed.
func (s *Session) dropClaims() {
	s.streamLock.Lock()
	for _, st := range s.streams {
		st.bufferLock.Lock()
		st.dropClaim()
		st.bufferLock.Unlock()
	}
	s.streamLock.Unlock()
	s.dropSpare()
}

// grow grows the session's buffer by n bytes a stream took from the pool.
func (s *Session) grow(n int) {
	atomic.AddInt32(&s.extra, int32(n))
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// spareUp adds n bytes a stream let go of to the session's spare: buffer
// past MaxReceiveBuffer that no window covers any more, given back to the
// pool as far as it is free (returnSpare).
func (s *Session) spareUp(n int) { atomic.AddInt32(&s.spare, int32(n)) }

// returnSpare gives the session's spare back to the pool as far as the
// buffer keeps 1/spareRoom of MaxReceiveBuffer free without it: what came in
// under a larger window stays in the buffer it came into until it is read,
// and the other streams keep room to move meanwhile -- more than they would
// have had if those windows had stayed within NeedStreamBuffer, as that
// data would then have emptied a buffer of MaxReceiveBuffer. Once the
// session closed nothing more comes in: all that is free goes back, and
// the rest as the streams let their data go (read, or closed).
func (s *Session) returnSpare() {
	if atomic.LoadInt32(&s.spare) <= 0 {
		return
	}
	s.spareLock.Lock()
	defer s.spareLock.Unlock()
	room := int32(s.config.MaxReceiveBuffer / spareRoom)
	if s.IsClosed() {
		room = 0
	}
	for {
		sp := atomic.LoadInt32(&s.spare)
		free := atomic.LoadInt32(&s.bucket)
		k := sp
		if free-room < k {
			k = free - room
		}
		if k <= 0 {
			return
		}
		if atomic.CompareAndSwapInt32(&s.bucket, free, free-k) {
			atomic.AddInt32(&s.spare, -k)
			atomic.AddInt32(&s.extra, -k)
			s.config.WindowPool.give(int(k))
			return
		}
	}
}

// borrow grows the session's buffer, as it runs full, by 1/borrowShare of
// MaxReceiveBuffer from the pool, while its spare (what it holds past
// MaxReceiveBuffer that no window covers) is less than borrowMax times
// MaxReceiveBuffer, and reports whether it did. What it takes is spare:
// given back as it is free again (returnSpare). It is for what was already
// in flight when readers slowed down (Stream.checkLag can only stop more): a
// dozen of them on a link carrying several MB in flight emptied the buffer
// and stalled every stream for seconds. It does not depend on the windows:
// a session of windows within NeedStreamBuffer borrows just the same. The
// claims of its streams' larger windows do not count: they cover what those
// windows let in, and a link carrying a fast stream is the one with the most
// in flight for the others too (counted with them, a fast stream's claim
// kept the session from borrowing at all, and such a link stalled 10 s).
// Readers that do not read at all (frozen apps) fill it all the same, a
// buffer later: then the session waits as it did, and the wedge guard
// (hs2's) sees it. A closed session borrows nothing: nothing more comes in,
// and its spare only goes back from then on (returnSpare, under the same
// lock).
func (s *Session) borrow() bool {
	s.spareLock.Lock()
	defer s.spareLock.Unlock()
	if s.IsClosed() || int(atomic.LoadInt32(&s.spare)) >= borrowMax*s.config.MaxReceiveBuffer {
		return false
	}
	got := s.config.WindowPool.take(s.config.MaxReceiveBuffer / borrowShare)
	if got == 0 {
		return false
	}
	s.spareUp(got)
	s.grow(got)
	atomic.AddUint32(&s.borrows, 1)
	return true
}

// dropSpare gives all of the session's spare back to the pool, closeGrace
// after it closed (dropClaims). As it closes, the spare goes back as far as
// it is free (returnSpare), its streams' claims keep only what they hold
// unread (sessionClose), and both go back as the streams let it go.
func (s *Session) dropSpare() {
	s.spareLock.Lock()
	defer s.spareLock.Unlock()
	if sp := atomic.SwapInt32(&s.spare, 0); sp > 0 {
		atomic.AddInt32(&s.extra, -sp)
		s.config.WindowPool.give(int(sp))
	}
}

// Extra is how far the session's buffer is past MaxReceiveBuffer now: what
// its streams' windows hold from the WindowPool, what it borrowed, and what
// came in under larger windows and has not been read yet.
func (s *Session) Extra() int { return int(atomic.LoadInt32(&s.extra)) }
