package engine

import (
	"strconv"
	"sync/atomic"
	"time"
)

// b296 (sc4x scale): HS2_B296_DCSKIP (default on; =0: rel69l's full visit of
// every conn every tick). darkcallTick visited every records conn every
// 200 ms: m.mu plus a walk of its subs reading each smux stream's ack marks
// (cs228InFlight) — 5-6 % CPU and late ticks at 2x load (~6k conns), almost
// all of it on idle conns that cannot be dark.
//
// A visit that ends with nothing to do for a conn that is not call-like and
// whose live subs are all fully consumed by the peer (marks ok, written ==
// consumed, none dark or capped, no outcome check due, no dc254 episode)
// records the conn's write sequence (m.dcWr). Until it changes, the next
// visits would reach the same verdict (cs226 lite needs a dark sub, a due
// check or bytes in flight; written grows only by a write, which bumps
// dcWr before and after; call-likeness and new subs come only with a write
// or addSubLocked, which bumps it too), so the timer skips the conn without
// m.mu. Call conns are never skipped. Every b296FullEvery-th tick visits all
// conns anyway (a backstop) and only that tick takes proofs (each costs a
// mark read per sub). cs226LiteVisitLocked also tests the link (shared
// meter) before the stream's marks: same verdict, far fewer cache misses.
var b296DCSkip atomic.Bool

func init() { b296DCSkip.Store(envBoolDefault("HS2_B296_DCSKIP", true)) }

const b296FullEvery = 25 // ticks (5 s at 200 ms)

// darkcallMarksOSOn: darkcallMarks reads the real smux marks (a test hook can
// change its answer without a write: no skip then, unless b296Force).
var darkcallMarksOSOn, b296Force atomic.Bool

var b296Stat struct {
	skipped atomic.Uint64
	proofs  atomic.Uint64
}

// dcWrBump: m's subs are about to write / just wrote (or a sub joined).
func (m *mConn) dcWrBump() { m.dcWr.Add(1) }

// b296Skip: the timer may skip m this tick.
func (m *mConn) b296Skip(full bool) bool {
	if !b296DCSkip.Load() || full || !darkcallMarksOSOn.Load() && !b296Force.Load() {
		return false
	}
	at := m.dcIdleAt.Load()
	return at != 0 && at == m.dcWr.Load()+1
}

// b296ProofLocked (under m.mu, after a visit that returned false): records
// the idle proof (seq: m.dcWr read before the visit) or clears it.
func (m *mConn) b296ProofLocked(now time.Time, seq uint64) {
	if !b296DCSkip.Load() {
		return
	}
	if b296Tick.Load()%b296FullEvery != 0 && !b296Force.Load() {
		return // proofs are taken on the backstop tick only (their mark reads cost)
	}
	if m.b296IdleLocked(now) {
		m.dcIdleAt.Store(seq + 1)
		b296Stat.proofs.Add(1)
		return
	}
	m.dcIdleAt.Store(0)
}

func (m *mConn) b296IdleLocked(now time.Time) bool {
	d := &m.dc
	if m.ended || m.closing || m.dcIsCallLocked(now) || !d.checkAt.IsZero() || d.silent || d.txStall {
		return false
	}
	for _, s := range m.subs {
		if s.dc.full {
			return false
		}
		if s.dead || s.retired {
			continue
		}
		if s.dc.isDark || s.dc.txDark {
			return false
		}
		w, c, ok := darkcallMarks(s)
		if !ok || w != c {
			return false
		}
	}
	return true
}

func b296LogPart() string {
	return " · b296 skip " + utoa(b296Stat.skipped.Load()) + " proofs " + utoa(b296Stat.proofs.Load())
}

func utoa(v uint64) string { return strconv.FormatUint(v, 10) }

// dcMeterOf: s's link meter, cached at addSubLocked (b296), else looked up.
func dcMeterOf(s *msub) *linkMeter {
	if s.dcMtr != nil {
		return s.dcMtr
	}
	return dcSubMeter(s)
}
