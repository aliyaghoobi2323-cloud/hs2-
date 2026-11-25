package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// rc15d HS2_RC15D_HEDGE_BURSTS=N (both sides, each for its own sending
// direction; default 0 = off for the A/B): a hedged light conn (K4M2) that
// is not heavy doubles only its first N bursts; every later burst goes one
// copy on the primary (the dupOff path, as a heavy conn's bytes: the
// receiver reassembles by offset, the hedge sub takes a hole).
//
// Why (lab3 b449j pprof, rc15d step 1): rc15b's ~+10 % CPU on both sides is
// not bytes (Kharej dup ~320 MB in 90 s next to the downloads) but small
// messages: every crowd request (Iran) and response (Kharej) of a 20 s
// light conn is a second smux frame, TLS record and write syscall on a
// second link, and a second read, decrypt and dedupe at the peer. The
// light-user gain is the cold first RTT (the first request and its
// response); later requests of a conn ride a primary that already
// delivered. A burst is as in rc15c (HS2_RC15C_BURST_MS idle with the
// primary drained). Calls (callKeepLocked) are untouched. The hedge sub
// stays (the second-sub idle rule closes it after mLiteIdle idle both ways)
// and a dying primary's one-copy bytes go again on it by the sub resend
// (HS2_MCONN_SUB_RESEND, production default; TestRC15dOneCopyResend). No
// wire change.
var rc15dBursts = hedge3EnvU64("HS2_RC15D_HEDGE_BURSTS", 0)

// rc15d HS2_RC15D_FRESH_MS=F (default 0 = no gate; with HEDGE_BURSTS): a
// later burst's chunk goes one copy only while the primary's link read
// within F ms and is not in an RTO loss and the hedge sub is a fresh resend
// target (subFreshLocked); otherwise it is doubled as before. lab3 b1
// (HEDGE_BURSTS=1 alone): crowd_p99 3066 ms, kh lost-copy 150 (no fresh
// target lost 431): one copy on a primary that goes dark or dies with no
// fresh second sub.
var rc15dFresh = time.Duration(hedge3EnvU64("HS2_RC15D_FRESH_MS", 0)) * time.Millisecond

var rc15dStat struct {
	bursts   atomic.Uint64 // bursts started on hedged light conns
	oneCopyB atomic.Uint64 // bytes sent one copy (a burst past the first N)
	gated    atomic.Uint64 // FRESH_MS: later bursts doubled (primary or hedge not fresh at the burst's start)
}

func init() {
	if liteHedge3 && rc15dBursts > 0 {
		log.Printf("rc15d: hedge only the first %d bursts (idle %v, fresh gate %v)", rc15dBursts, rc15cBurstIdle, rc15dFresh)
	}
}

// rc15dOneCopyLocked (hedge3OneCopyLocked, a hedged light conn not heavy):
// this chunk belongs to a burst past the first rc15dBursts (one copy).
func (m *mConn) rc15dOneCopyLocked() bool {
	if m.callKeepLocked() {
		return false
	}
	now := time.Now()
	if m.c15Last.IsZero() || (now.Sub(m.c15Last) >= rc15cBurstIdle && m.rc15cDrainedLocked()) {
		if m.c15dN < ^uint32(0) {
			m.c15dN++
		}
		rc15dStat.bursts.Add(1)
		// decided once per burst (the gate asks TCP_INFO: not per chunk)
		m.c15dOne = uint64(m.c15dN) > rc15dBursts
		if m.c15dOne && rc15dFresh > 0 && !m.rc15dSafeLocked(now) {
			m.c15dOne = false
			rc15dStat.gated.Add(1)
		}
	}
	m.c15Last = now
	return m.c15dOne
}

// rc15dSafeLocked (HS2_RC15D_FRESH_MS): the primary's link read within
// rc15dFresh and is not in an RTO loss, and the hedge sub is a fresh resend
// target on another link.
func (m *mConn) rc15dSafeLocked(now time.Time) bool {
	p, h := m.primary.Load(), m.hedgeSub.Load()
	if p == nil || h == nil || h == p || h.dead || h.retired || p.dead {
		return false
	}
	fresh := now.Sub(monoTime(p.lastRecv.Load())) <= rc15dFresh
	if !fresh {
		if mtr := subLinkMeter(p); mtr != nil {
			if rd := mtr.lastRd.Load(); rd != 0 && time.Duration(ctrlNow()-rd) <= rc15dFresh {
				fresh = true
			}
		}
	}
	if !fresh {
		return false
	}
	if la, ok := subLinkAck(p); ok && la.rtoLoss {
		return false
	}
	return subLinkKey(h) != subLinkKey(p) && subFreshLocked(h, now)
}

// rc15dLog (from rc15bLog, at most every 10 s).
func rc15dLog() {
	if rc15dBursts > 0 {
		log.Printf("rc15d: bursts=%d first_onecopy=%dKB gated=%d", rc15dStat.bursts.Load(), rc15dStat.oneCopyB.Load()>>10, rc15dStat.gated.Load())
	}
}
