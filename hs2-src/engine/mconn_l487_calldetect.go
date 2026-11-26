package engine

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// l487 HS2_L487_CALLDETECT (default on after lab3 e, 10 ABBA pairs): darkcall visits every
// call-like conn, not only those in rrConns.
//
// lab3 l487 trace (HS2_L487_DBG): in the bad runs (d3D/d4D: 235..243 call
// samples > 3 s, callmax ~4.9 s = the whole 5 s blackhole, 0 Iran darkcall
// dark lines with primary=true) all 8 call conns were recognised as calls
// (like=true every window) but had rscCap=false: they opened while the lite
// budget had room, so hedgeWantRecords() made their first sub a records
// (hedged, non-raw) sub, and a non-raw first sub never gets rescueEnable.
// rrConns (darkcall's only list) holds only rescue-capable conns
// (rrKeepLocked), so darkcall never looked at those calls: no dark proof on
// their primary, no switch, no b270/dc254/b262 action. In the good runs the
// budget was capped at the calls' open, the first subs were raw+rescue and
// darkcall worked (8..11 primary=true lines, max 1.9 s). The per-run
// "darkcall visits" index of b487/l486 is not the cause: the summary line is
// printed once ~72 s after the process starts, so its cumulative count only
// tells how far into the calls that one line fell.
//
// With the key, a call-like window (callRollLocked) of a conn that is not in
// rrConns registers it in l487Conns, which darkcallTick visits too (same
// visit, same gates). A visit that finds the primary dark with no live
// candidate on a conn without a records ring (no rescue possible) asks the
// manager for a fresh light sub (kickMgr) instead of nothing.
var l487CallDetect atomic.Bool

func init() { l487CallDetect.Store(envBoolDefault("HS2_L487_CALLDETECT", true)) }

var l487Conns sync.Map // *mConn -> struct{}

var l487Stat struct {
	reg   atomic.Uint64 // call conns registered (not in rrConns)
	gone  atomic.Uint64 // registered conns dropped (ended)
	kick  atomic.Uint64 // dark primary, no candidate, no ring: a new sub asked for
	visit atomic.Uint64 // darkcall visits of registered conns
}

// l487RegLocked (under m.mu; callRollLocked on a call-like window).
func (m *mConn) l487RegLocked() {
	if !l487CallDetect.Load() || m.l487Reg {
		return
	}
	if _, ok := rrConns.Load(m); ok {
		return
	}
	m.l487Reg = true
	l487Conns.Store(m, struct{}{})
	l487Stat.reg.Add(1)
	if darkcallRunning() {
		darkcallOnce.Do(func() { go darkcallLoop() })
	}
}

// l487Tick (darkcallTick): visit the registered conns that rrConns lacks.
func l487Tick(now time.Time, nowc int64) {
	if !l487CallDetect.Load() {
		return
	}
	l487Conns.Range(func(k, _ any) bool {
		m := k.(*mConn)
		select {
		case <-m.die:
			l487Conns.Delete(m)
			l487Stat.gone.Add(1)
			return true
		default:
		}
		if _, ok := rrConns.Load(m); ok {
			return true // visited there
		}
		l487Stat.visit.Add(1)
		if m.darkcallVisit(now, nowc) {
			go m.rrStart()
		}
		return true
	})
}

// l487NoCandLocked (darkcallLiteLocked, no live candidate for a dark
// primary): a conn without a records ring cannot be rescued; ask for a sub.
func (m *mConn) l487NoCandLocked(lite bool) {
	if !l487CallDetect.Load() || lite || !m.iran || m.rscCap {
		return
	}
	l487Stat.kick.Add(1)
	if l487Dbg {
		log.Printf("l487 kick %x subs=%d", m.id&0xffffff, m.liveSubsLocked())
	}
	m.kickMgr()
}

func l487LogPart() string {
	if !l487CallDetect.Load() {
		return ""
	}
	return " · l487 reg " + utoa(l487Stat.reg.Load()) + " gone " + utoa(l487Stat.gone.Load()) +
		" visits " + utoa(l487Stat.visit.Load()) + " kick " + utoa(l487Stat.kick.Load())
}
