package engine

import (
	"sync/atomic"
	"time"
)

// cs221: call deaths at dark-path events (lab3 r68ft X1 at bhA, Z1 at
// flap_f: the call probe's connection reset, "write: broken pipe").
//
// Both followed the same chain:
//  1. X1: the darkcall switch resent what the dark primary had written and
//     the peer had not reported consumed (smux written - consumed: 415-472
//     KB, mostly bytes the peer had read, an210 Q2) onto the new primary,
//     although a call's other sub had been given a copy of every one of
//     those chunks (dual mode). The new primary's queue (mSubQueueLite, 32
//     KB) then held ~15x its bound of duplicates: Write waited until they
//     drained, the receive made no progress on either side (rxKB 0, stalled
//     1.2-2.4 s).
//  2. Iran's darkcall rx rule (a live sub dark and the receive stalled for
//     darkcallAfter) then started a records rescue. rrStart retires every
//     live sub, the healthy one too, and drops what it held in flight; the
//     call's ring did not reach back to the exit's receive offset, so the
//     RREC answer ended the connection (rescue-rec nocover 1,
//     mconn_rescue_rec.go: m.end(errMConnLost)). Z1 took the same end
//     (rrec twice, nocover 1) without a switch.
//
// HS2_CS221_DARKCALL (default on; =0 gives back dlp217):
//   - each sub keeps missEnd, the end offset of the newest DATA chunk it was
//     not given (joined late, a full queue, one-copy chunks, a call pair that
//     did not pick it). The switch resends only [from, min(qStart,
//     to.missEnd)): to already holds every chunk from missEnd on, and the
//     dark sub's queued chunks at or past missEnd are not moved again;
//   - a call pair (3+ subs, callPairLocked) never puts both copies of a
//     chunk on two subs of one path while a sub on another path has room:
//     S1 (dlp217 + SHAREQ) had subs 0 and 1 on the blackholed path and sub
//     2 elsewhere; round robin paired (0,1) for a third of the chunks, which
//     then existed only on the dark path (call stalls 10.1 s and 6.2 s,
//     until the given-up subs died and their kept chunks were resent);
//   - the darkcall rx rescue waits while a live sub that is not dark read a
//     record within darkcallAfter: the connection is moving there (the stall
//     is a queue or a hole the copies fill), and a rescue would retire that
//     sub and reset the call when the ring is short.
var cs221Darkcall atomic.Bool

func init() { cs221Darkcall.Store(envBoolDefault("HS2_CS221_DARKCALL", true)) }

var cs221Stat struct {
	held     atomic.Uint64 // switches whose target held part of the range
	heldB    atomic.Uint64 // bytes not resent: the target held them
	skipQ    atomic.Uint64 // queued chunks not moved: the target held them
	rxLive   atomic.Uint64 // rx rescues not started: a live sub reads
	samePair atomic.Uint64 // call pairs left on one path: no other path had room
}

// noteMiss (under m.mu): s was not given the DATA chunk ending at end.
func (s *msub) noteMiss(end uint64) {
	if end > s.missEnd {
		s.missEnd = end
	}
}

// cs221HeldFrom (under m.mu): the offset from which to holds every DATA
// chunk (^0: unknown, it may lack any).
func (m *mConn) cs221HeldFrom(to *msub) uint64 {
	if !cs221Darkcall.Load() || m.missLost {
		return ^uint64(0)
	}
	return to.missEnd
}

// cs221LiveReadLocked (under m.mu): a live sub that is not dark read a
// record within darkcallAfter.
func (m *mConn) cs221LiveReadLocked(live []*msub, now time.Time) bool {
	if !cs221Darkcall.Load() {
		return false
	}
	for _, s := range live {
		if s.dc.isDark || s.dead || s.retired {
			continue
		}
		if r := s.lastRecv.Load(); r != 0 && time.Duration(monoNano(now)-r) < darkcallAfter() {
			return true
		}
	}
	return false
}

// cs221PathOf: the path a sub's link rides ("" unknown; a var for tests).
var cs221PathOf = func(s *msub) string {
	p := callPathOf(s.key)
	if l, ok := s.key.(Link); ok && p == "" {
		if mtr := linkMeterOf(l); mtr != nil {
			p = mtr.path
		}
	}
	return p
}

// callPairPick (under m.mu): the two subs, in turn from at, that take a
// call chunk of size bytes. cs221: the second is on another path than the
// first when any sub with room is; else (one path only) the old pair.
func callPairPick(cand []*msub, n, at, size int) ([2]*msub, int) {
	var pick [2]*msub
	k := 0
	var p0 string
	var same *msub
	for i := 0; i < n && k < 2; i++ {
		s := cand[(at+i)%n]
		if !s.fits(size) {
			continue
		}
		if cs221Darkcall.Load() {
			if k == 0 {
				p0 = cs221PathOf(s)
			} else if p0 != "" && cs221PathOf(s) == p0 {
				if same == nil {
					same = s
				}
				continue
			}
		}
		pick[k] = s
		k++
	}
	if k == 1 && same != nil {
		pick[1], k = same, 2
		cs221Stat.samePair.Add(1)
	}
	return pick, k
}
