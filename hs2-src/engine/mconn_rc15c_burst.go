package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// rc15c HS2_RC15C_BURST (both sides act on their own sending direction;
// default off for the A/B): hedge per request burst. rc15b keeps a light
// hedged conn doubled for its whole life (the rate rule HS2_RC15_HEAVY_KBPS
// holds it light), so long-lived light conns cost two copies of every byte
// (lab3 b449j: cpu_ir +26, cpu_kh +32 proven, ~10 %). The light-user gain is
// the first RTT of each request and each response, so with the key a
// hedged, not heavy, lite conn doubles only the first burstBytes of each
// burst and sends the rest of the burst one copy on the primary (the dupOff
// path: the receiver reassembles by offset, the other sub takes a hole). A
// burst starts when this side sent nothing for >= burstIdle and the
// primary's queue is drained (nothing of the last burst still waits to be
// written). The hedge sub stays open (each burst's head is pushed on it, so
// the lite idle rule keeps it; Iran reopens a second sub as before). Calls
// (callKeepLocked) are untouched. No wire change (cap bit 18 unchanged).
//
//	HS2_RC15C_BURST=1         the rule
//	HS2_RC15C_BURST_BYTES     doubled head of a burst (default hedge3Bytes)
//	HS2_RC15C_BURST_MS        idle that starts a burst (default 200)
//
// rc15c HS2_RC15C_CAPQUIET_MS (Iran; default 0 = off; arm B: 2000): a hedge
// sub open is skipped while picks wait for a HS2_B457_PICKCAP slot or one
// waited within the last CAPQUIET_MS (a ZDU drain end, a link storm), or
// while this process drains (ZDUDraining): the hedge must never take a slot
// a rescue join needs. Counted as hedge_quiet_skip.
var (
	rc15cBurst      = envBoolDefault("HS2_RC15C_BURST", false)
	rc15cBurstBytes = hedge3EnvU64("HS2_RC15C_BURST_BYTES", hedge3Bytes)
	rc15cBurstIdle  = time.Duration(hedge3EnvU64("HS2_RC15C_BURST_MS", 200)) * time.Millisecond
	rc15cCapQuiet   = time.Duration(hedge3EnvU64("HS2_RC15C_CAPQUIET_MS", 0)) * time.Millisecond
)

var rc15cStat struct {
	bursts    atomic.Uint64 // bursts started on hedged light conns
	oneCopyB  atomic.Uint64 // bytes sent one copy past a burst's head
	quietSkip atomic.Uint64 // Iran: hedge opens skipped by CAPQUIET
	capWaitAt atomic.Int64  // monoNano of the last pick that waited
}

func init() {
	if liteHedge3 && (rc15cBurst || rc15cCapQuiet > 0) {
		log.Printf("rc15c: burst=%v head %d bytes idle %v, capquiet %v", rc15cBurst, rc15cBurstBytes, rc15cBurstIdle, rc15cCapQuiet)
	}
}

// rc15cBurstOneCopyLocked (hedge3OneCopyLocked, a hedged light conn not
// heavy): this chunk is past its burst's head (one copy).
func (m *mConn) rc15cBurstOneCopyLocked() bool {
	if !rc15cBurst || m.callKeepLocked() {
		return false
	}
	now := time.Now()
	if m.c15Last.IsZero() || (now.Sub(m.c15Last) >= rc15cBurstIdle && m.rc15cDrainedLocked()) {
		m.c15Start = m.sendOff
		rc15cStat.bursts.Add(1)
	}
	m.c15Last = now
	return m.sendOff-m.c15Start >= rc15cBurstBytes
}

// rc15cDrainedLocked: the primary has nothing of this side's bytes waiting
// to be written.
func (m *mConn) rc15cDrainedLocked() bool {
	p := m.primary.Load()
	return p == nil || (len(p.q) == 0 && p.cur == nil)
}

// rc15cCapWaited (b457PickAcquireDG): a pick waits for a slot now.
func rc15cCapWaited() {
	if rc15cCapQuiet > 0 {
		rc15cStat.capWaitAt.Store(monoNano(time.Now()))
	}
}

// rc15cHedgeQuiet (openNoLock, a hedge open): skip it (picks wait or waited
// within CAPQUIET, or this process drains).
func rc15cHedgeQuiet() bool {
	if rc15cCapQuiet <= 0 {
		return false
	}
	quiet := b458Stat.waiting.Load() > 0 || ZDUDraining()
	if !quiet {
		if at := rc15cStat.capWaitAt.Load(); at != 0 && monoNano(time.Now())-at < int64(rc15cCapQuiet) {
			quiet = true
		}
	}
	if quiet {
		rc15cStat.quietSkip.Add(1)
	}
	return quiet
}

// rc15cLog (from rc15bLog, at most every 10 s).
func rc15cLog() {
	if rc15cBurst || rc15cCapQuiet > 0 {
		log.Printf("rc15c: bursts=%d burst_onecopy=%dKB hedge_quiet_skip=%d", rc15cStat.bursts.Load(), rc15cStat.oneCopyB.Load()>>10, rc15cStat.quietSkip.Load())
	}
}
