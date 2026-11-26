package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// l476 HS2_L476_JOINRES (default off, lab A/B; no wire change): rc15f's
// join resend for every kept lite connection, on both sides.
//
// Lab l476 (lab3 netns, rc15, a quarter of the links blackholed 5 s every
// 15 s, HS2_MCONN_TRACE on port 2092): in EVERY fault window (not only the
// first) ~17-22 crowd conns had both subs on the dead quarter; that count
// matches the crowd messages late by > 2 s in the window (17-22). Iran's
// b449 dark open fired ~1.2 s into the fault and a fresh sub joined on a live
// link (both sides "sub+ 2 ... sendOff=9600 / 9500"), but the request Iran
// had written just before the fault (offset 9500..9600) stayed only on the
// two dead subs: the fresh sub is given bytes from sendOff on, and rc15f's
// join resend runs only for call conns on Kharej. The request crossed only
// when a dead link's own TCP retransmission got through after the fault:
// latency ~5.3 s or ~9.2 s (RTO backoff), the two modes of the late tail.
//
// Now (key on), as a non-bulk sub joins a lite conn that keeps its chunks
// (call keep or sub-resend keep), every other live sub that is dark while
// pending (cs226Silent: nothing heard for darkcallAfter while a write of ours
// waits for an answer) resends its sequenced chunks the fresh sub lacks on
// it (b270's resend with the cs223 skip rules). Calls keep rc15f's rule.
var l476JoinRes atomic.Bool

func init() { l476JoinRes.Store(envBoolDefault("HS2_L476_JOINRES", false)) }

// HS2_L476_JOINPRIM (default off, needs HS2_L476_JOINRES): the first A/B
// (request resend only) moved the ~9.2 s mode to ~5.3 s: Kharej then read
// the resent request on the fresh sub but wrote the reply on its primary,
// still on the dead link. Now a non-bulk sub that joins a kept lite conn
// whose every other live sub is dark (Iran: cs226Silent; Kharej: nothing
// heard for rc15fSilent while the conn heard within b449DarkRecent) becomes
// the preferred primary on that side.
var l476JoinPrim atomic.Bool

func init() { l476JoinPrim.Store(envBoolDefault("HS2_L476_JOINPRIM", false)) }

var l476Stat struct {
	joins  atomic.Uint64
	res    atomic.Uint64
	chunks atomic.Uint64
	prim   atomic.Uint64 // joins made the preferred primary (HS2_L476_JOINPRIM)
}

func l476LogPart() string {
	return fmt.Sprintf(" · l476 joinres on %v joins %d res %d chunks %d prim on %v %d", l476JoinRes.Load(),
		l476Stat.joins.Load(), l476Stat.res.Load(), l476Stat.chunks.Load(), l476JoinPrim.Load(), l476Stat.prim.Load())
}

// l476KeepLocked: call keep, or with the key a sub-resend kept lite conn.
func (m *mConn) l476KeepLocked() bool {
	return m.callKeepLocked() || (l476JoinRes.Load() && m.lite && m.stN == 0 && m.subKeepLocked())
}

// l476JoinResLocked (addSubLocked, under m.mu, nw just added).
func (m *mConn) l476JoinResLocked(nw *msub, now time.Time) {
	if !l476JoinRes.Load() || nw.bulk.Load() || m.mode != planeLight || m.ended || !m.lite || m.stN != 0 {
		return
	}
	if !m.iran && m.callKeepLocked() && rc15fJoinRes.Load() {
		return // rc15f's rule
	}
	if !m.l476KeepLocked() {
		return
	}
	nowc := ctrlNow()
	found := false
	others, dark, heard := 0, 0, int64(0)
	for _, s := range m.subs {
		if s == nw || s.dead || s.retired || s.bulk.Load() {
			continue
		}
		others++
		lr := s.lastRecv.Load()
		heard = max(heard, lr)
		if !m.iran && now.Sub(monoTime(lr)) >= rc15fSilent {
			dark++
		}
		if !cs226Silent(s, nowc) {
			continue
		}
		if m.iran {
			dark++
		}
		if !found {
			found = true
			l476Stat.joins.Add(1)
		}
		if n := m.b270ResendKeptLocked(s, nw, s.b270End > 0, now); n > 0 {
			l476Stat.res.Add(1)
			l476Stat.chunks.Add(uint64(n))
			m.tr.ev("l476: dark sub %d resent %d chunks on joining sub %d", s.idx, n, nw.idx)
		}
	}
	if l476JoinPrim.Load() && others > 0 && dark == others && heard != 0 && now.Sub(monoTime(heard)) <= b449DarkRecent {
		m.prefPrimary = nw
		m.setPrimaryLocked()
		l476Stat.prim.Add(1)
		m.tr.ev("l476: every other sub dark, joining sub %d is the primary", nw.idx)
	}
}
