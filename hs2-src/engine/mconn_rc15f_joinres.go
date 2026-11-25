package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// rc15f HS2_RC15F_JOINRES (default on; Kharej acts, no wire change): when a
// fresh light sub joins a call connection, the copies stranded on the
// call's receive-silent subs go on the fresh sub at once.
//
// Lab b474 (real WAN lab2<->lab4, a quarter of the links blackholed 5 s
// every 15 s, both arms traced with HS2_MCONN_TRACE_GAME): a call whose two
// subs both sat in the dark quarter stalled 5-9.4 s (rc15 s02-TB call 5:
// Iran's receive gap at rcv next 302240 grew 4.5 s+). Iran's stuck-call
// trigger opened a fresh sub off the dead links 1.3 s into the fault and
// resent its own stranded chunks there (cs223 given-up resend), but its
// CLOSE-REQ for the given-up subs went on those very subs, so Kharej's copies
// of [302240, ...) stayed on the dead subs until the CLOSE-REQ crossed after
// the fault and the RTO backoff (Kharej "cs223: given-up sub 0 resends 91
// chunks" at +49.0 s, 9 s after the move). b270's half-dead resend did not
// fire (edge 3, res 0): with every sub dark there is no live sub to take
// the copies until the fresh one joins, and a join is no b270 edge.
//
// Now on Kharej, as a non-bulk sub joins a lite call conn (callKeep), every
// other live sub that received nothing for rc15fSilent resends its
// sequenced chunks that the fresh sub lacks on the fresh sub (b270's
// resend: cs223 skip rules, gaveTo marks them so a later death resend
// skips them; repeats take only chunks past what was already covered).
// Iran's peer copies are dropped by offset (mrecv overlap). A live call
// sub hears its peer every 20-60 ms, so a 400 ms silence is a dead link.
var rc15fJoinRes atomic.Bool

func init() { rc15fJoinRes.Store(envBoolDefault("HS2_RC15F_JOINRES", true)) }

const rc15fSilent = 400 * time.Millisecond

var rc15fStat struct {
	joins  atomic.Uint64 // joins of a call conn that found a silent sub
	res    atomic.Uint64 // silent subs whose chunks were resent
	chunks atomic.Uint64
}

func rc15fLogPart() string {
	return fmt.Sprintf(" · rc15f joinres on %v joins %d res %d chunks %d", rc15fJoinRes.Load(),
		rc15fStat.joins.Load(), rc15fStat.res.Load(), rc15fStat.chunks.Load())
}

// rc15fJoinResLocked (addSubLocked, under m.mu, nw just added).
func (m *mConn) rc15fJoinResLocked(nw *msub, now time.Time) {
	if !rc15fJoinRes.Load() || m.iran || nw.bulk.Load() || m.mode != planeLight || m.ended || !m.callKeepLocked() {
		return
	}
	found := false
	for _, s := range m.subs {
		if s == nw || s.dead || s.retired || s.bulk.Load() {
			continue
		}
		if now.Sub(monoTime(s.lastRecv.Load())) < rc15fSilent {
			continue
		}
		if !found {
			found = true
			rc15fStat.joins.Add(1)
		}
		if n := m.b270ResendLocked(s, nw, s.b270End > 0, now); n > 0 {
			rc15fStat.res.Add(1)
			rc15fStat.chunks.Add(uint64(n))
			m.tr.ev("rc15f: silent sub %d resent %d chunks on joining sub %d", s.idx, n, nw.idx)
		}
	}
}
