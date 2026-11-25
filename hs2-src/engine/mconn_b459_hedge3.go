package engine

import (
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// b459 HS2_LITE_HEDGE3=1 (both sides, lite; default off): F1c, a light-user
// hedge that leaves heavy flows alone and has no +1 RTT join delay. It runs
// on F1b's machinery (mconn_b455_hedge.go: hedge sub at connect, early
// buffer, byte-0 join replay, budget); HS2_LITE_HEDGE2 alone is unchanged.
//
//   - No join delay (the +1 RTT mode): a join sub's open is 0-RTT (an smux
//     stream on an open link), but the exit held a join until the first sub
//     arrived (tbl.join waits mconnJoinWait), so when the segment with the
//     first sub's header was lost the hedge's copy of the request waited for
//     that retransmit too (~1 RTT: the request segment's SACKs). The hedge
//     sub now says mconnRoleHedge: the exit creates the connection from
//     whichever of the two comes first (reserve) and the other joins it.
//     A pre-warmed sub pool is not needed for this: the open costs no RTT.
//   - Heavy flows: once either direction moved more than hedge3Bytes, a
//     hedged connection sends one copy (the primary sub; not a call), so a
//     download or upload never rides two links; Iran closes the hedge sub
//     after hedge3Idle idle both ways and does not reopen a second light sub.
//   - Early buffer (replay at join): hedge3Bytes, hedge3Dur.
var liteHedge3 = os.Getenv("HS2_LITE_HEDGE3") == "1" || os.Getenv("HS2_LITE_HEDGE4") == "1" || os.Getenv("HS2_LITE_HEDGE7") == "1" || liteHedge8Key || liteHedge10 // b460: HEDGE4 runs on HEDGE3 (b465: HEDGE7 too)

// mconnRoleHedge: Iran's hedge sub (first-or-join at the exit).
const mconnRoleHedge byte = 7

const hedge3Idle = 300 * time.Millisecond

// b464 F1h: the window (early buffer and the heavy rule's threshold) is
// tunable: HS2_LITE_HEDGE3_BYTES (default 16384), HS2_LITE_HEDGE3_MS
// (default 400). 0 is honoured: BYTES=0 buffers nothing and makes a hedged
// conn heavy (one copy) after its first chunk; MS=0 closes the early window
// at once (no buffer, no join replay). Unset or unparsable: the default.
var (
	hedge3Bytes = hedge3EnvU64("HS2_LITE_HEDGE3_BYTES", 4096)                               // rc15 K4M2 (b464: 16384)
	hedge3Dur   = time.Duration(hedge3EnvU64("HS2_LITE_HEDGE3_MS", 200)) * time.Millisecond // rc15 K4M2 (b464: 400)
)

func init() {
	if liteHedge3 {
		log.Printf("b464 hedge3: window %d bytes %v", hedge3Bytes, hedge3Dur)
	}
}

// hedge3EnvU64: a non-negative integer env knob; set to "0" means 0.
func hedge3EnvU64(name string, def uint64) uint64 {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		log.Printf("b464: %s=%q not a non-negative integer, default %d", name, v, def)
		return def
	}
	return n
}

// hedgeMark (in an open's avoid list): open the sub with mconnRoleHedge.
type hedgeMark struct{}

func takeHedgeMark(avoid []any) ([]any, bool) {
	for i, a := range avoid {
		if _, ok := a.(hedgeMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), true
		}
	}
	return avoid, false
}

// hedge3Stat: engagement and cost (logged by hedge3Log, both sides).
var hedge3Stat struct {
	hedged     atomic.Uint64 // Iran: conns hedged; Kharej: conns a hedge sub reached
	exitFirst  atomic.Uint64 // Kharej: conns created by the hedge sub (it beat the first)
	exitJoin   atomic.Uint64 // Kharej: hedge subs that joined an existing conn
	firstJoin  atomic.Uint64 // Kharej: first subs that joined a hedge-created conn
	heavy      atomic.Uint64 // hedged conns that crossed hedge3Bytes (one copy from then on)
	dropped    atomic.Uint64 // Iran: hedge subs closed by the heavy rule
	dupB       atomic.Uint64 // bytes sent twice on hedged conns (this side's direction)
	oneCopyB   atomic.Uint64 // bytes sent once by the heavy rule (would have been doubled)
	hedgeClose atomic.Uint64 // Kharej: hedge role refused (key off)
}

// hedgeEarlyDur / hedgeEarlyBytes: the early buffer's window.
func hedgeEarlyDur() time.Duration {
	if liteHedge3 {
		return hedge3Dur
	}
	return hedgeDur
}

func hedgeEarlyBytes() uint64 {
	if liteHedge3 {
		return hedge3Bytes
	}
	return hedgeBytes
}

// hedge3HeavyLocked (under m.mu): the hedged conn moved more than
// hedge3Bytes either way (sticky).
func (m *mConn) hedge3HeavyLocked() bool {
	if !m.h3 {
		return false
	}
	if m.h3Heavy {
		return true
	}
	moved := m.sendOff
	if moved <= hedge3Bytes {
		r := &m.r
		r.mu.Lock()
		moved = max(moved, r.next)
		r.mu.Unlock()
	}
	heavy := moved > hedge3Bytes
	if heavy && rc15HeavyBps > 0 { // rc15b HS2_RC15_HEAVY_KBPS (mconn_rc15b_hedgefix.go)
		r := &m.r
		r.mu.Lock()
		moved = max(m.sendOff, r.next)
		r.mu.Unlock()
		if moved <= m.rc15HeavyLim(time.Now()) {
			rc15bStat.rateHeld.Add(1)
			heavy = false
		} else {
			rc15bStat.rateHeavy.Add(1)
		}
	}
	if heavy {
		m.h3Heavy = true
		hedge3Stat.heavy.Add(1)
		m.b466HeavyLocked() // b466 dbg
		m.tr.ev("hedge3: heavy (sent %d)", m.sendOff)
		if m.iran {
			m.kickMgr() // the hedge sub's close
		}
	}
	return heavy
}

// hedge3OneCopyLocked (sendLocked, general light path): this chunk goes on
// the primary only (a heavy hedged conn, not a call).
func (m *mConn) hedge3OneCopyLocked() bool {
	if !liteHedge3 || !m.h3 || !m.lite {
		return false
	}
	if !m.hedge3HeavyLocked() {
		if rc15dBursts > 0 {
			return m.rc15dOneCopyLocked() // rc15d HS2_RC15D_HEDGE_BURSTS (mconn_rc15d_first.go)
		}
		return m.rc15cBurstOneCopyLocked() // rc15c HS2_RC15C_BURST (mconn_rc15c_burst.go)
	}
	return !m.callKeepLocked()
}

// hedge3IdleLocked (tick, second-sub idle rule): a heavy hedged conn's hedge
// sub closes after hedge3Idle idle both ways.
func (m *mConn) hedge3IdleLocked(s *msub, lim time.Duration) time.Duration {
	if !liteHedge3 || s != m.hedgeSub.Load() || !m.hedge3HeavyLocked() {
		return lim
	}
	return min(lim, hedge3Idle)
}

// hedge3NoReopenLocked (Iran tick): a heavy hedged conn opens no new second
// light sub (it would only carry holes).
func (m *mConn) hedge3NoReopenLocked() bool {
	if !liteHedge3 || !m.h3 || !m.h3Heavy {
		return false
	}
	return true
}

// exitHedge3 (Kharej, serveMConnSub): reserve lost for a first or hedge
// sub: with the key on the other one created the conn; join it.
func exitHedge3Join(tbl *mconnTable, id uint64, role byte, stop <-chan struct{}) *mConn {
	if !liteHedge3 || (role != mconnRoleHedge && role != mconnRoleFirst) {
		return nil
	}
	mc := tbl.join(id, mconnJoinWait, stop)
	if mc == nil {
		return nil
	}
	if role == mconnRoleHedge {
		hedge3Stat.exitJoin.Add(1)
	} else {
		hedge3Stat.firstJoin.Add(1)
		mc.b466FirstJoin() // b466 dbg
	}
	return mc
}

// exitHedge3Mark (Kharej): the conn got its hedge sub.
func (m *mConn) exitHedge3Mark(created bool) {
	m.mu.Lock()
	m.h3 = true
	m.mu.Unlock()
	hedge3Stat.hedged.Add(1)
	if created {
		hedge3Stat.exitFirst.Add(1)
	}
	hedge3Log(time.Now())
}

var hedge3LogAt atomic.Int64

// hedge3Log: the counters, at most every 10 s.
func hedge3Log(now time.Time) {
	b466Log(now)
	if !liteHedge3 {
		return
	}
	n := monoNano(now)
	at := hedge3LogAt.Load()
	if n-at < int64(10*time.Second) || !hedge3LogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("b459 hedge3: hedged=%d exit_first=%d exit_join=%d first_join=%d heavy=%d dropped=%d dup=%dKB onecopy=%dKB refused=%d replays=%d replayed=%dKB capped=%d",
		hedge3Stat.hedged.Load(), hedge3Stat.exitFirst.Load(), hedge3Stat.exitJoin.Load(), hedge3Stat.firstJoin.Load(),
		hedge3Stat.heavy.Load(), hedge3Stat.dropped.Load(), hedge3Stat.dupB.Load()>>10, hedge3Stat.oneCopyB.Load()>>10,
		hedge3Stat.hedgeClose.Load(),
		hedgeStat.replays.Load(), hedgeStat.replayB.Load()>>10, hedgeStat.capped.Load())
	hedge8Log() // b466
	rc15bLog(now)
}
