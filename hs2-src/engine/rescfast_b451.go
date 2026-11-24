package engine

import (
	"os"
	"strings"
	"sync/atomic"
)

// b451 rescue placement on the fast path (HS2_RESC_FASTPICK). In an open
// storm (the HH:01 reconnect wave, a crowd start: thousands of users in ~2 s)
// the links' load rises, the balancer lowers paths, and ~1 s later almost
// every new lite connection's upgrade is a rescue copy (rescueAvoidLocked:
// its raw sub rides a lowered link). pickPlaneCallOne placed those under
// LinkManager.mu taken for WRITING (pickRescueLocked, a full tiers scan of
// every link, with rescueCallLinkOn's own lock per link) — the path
// lm_fastpick.go left on the write lock as "rare". In a storm it is not rare:
// lab1 b450 runtime trace (6000 users, crowd start) put 58 s of summed
// run-queue wait in 4 s on goroutines waking the readers queued behind those
// writers (rwmutex Unlock waking every pickHeldFast / pickPlaneFast parked on
// RLock), the same convoy shape b450 removed from the close path; the TUN's
// L3 links (same sessions, same Ps) waited behind it: one-way 99-187 ms.
//
// Now the rescue scan runs under the read lock with the fast path's
// compare-and-swap placement (placeFast), exactly as pickPlaneFast: the very
// same filter and tiers, so the same link a serial pick would choose. Lost
// races (fastPickTries) fall back to the old write-locked pick; a scan that
// finds no rescue link skips the write-locked retry (it would find none
// either) and goes on to the ordinary pick, as before.
//
//	HS2_RESC_FASTPICK unset / 1  on (whenever HS2_LM_FASTPICK is on)
//	HS2_RESC_FASTPICK=0          off (the b450 behaviour)
var rescFastPick atomic.Bool

// rescFastStat: placements made on the fast path / fell back to the lock.
var rescFastStat struct{ fast, fallback atomic.Uint64 }

func init() { rescFastPick.Store(rescFastOn(os.Getenv("HS2_RESC_FASTPICK"))) }

func rescFastOn(v string) bool { return strings.TrimSpace(v) != "0" }

// pickRescueFast is pickRescueLocked + placeLocked under the read lock.
// done=false: the caller takes the write-locked path (races lost).
func (m *LinkManager) pickRescueFast(avoid []any, pool *peerInfo) (l Link, release func(), ok, done bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for try := 0; try < fastPickTries; try++ {
		ml, fp := m.pickRescueLockedFP(avoid, pool)
		if ml == nil {
			return nil, nil, false, true
		}
		if m.placeFast(ml, fp) {
			rescFastStat.fast.Add(1)
			return ml.link, m.releaseFor(ml), true, true
		}
		fastPickStat.retry.Add(1)
	}
	rescFastStat.fallback.Add(1)
	return nil, nil, false, false
}
