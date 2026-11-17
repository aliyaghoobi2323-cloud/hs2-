package engine

import (
	"fmt"
	"sync/atomic"
)

// cs226: every lite second sub off its primary's path; a lite connection
// whose subs ride a silent path is moved off it at once (HS2_CS226_LITEPATH,
// default on; =0 gives back cs225).
//
// lab2 rel68d K2: a game froze 21.5 s while path 55.1 was blackholed and
// resumed exactly when Kharej's exit links on 55.1 timed out (stream.go read/
// write deadline): its data sat on 55.1 subs with no copy elsewhere until the
// sub died and SUB_RESEND moved it. Two gaps let that happen:
//
//  1. Placement. cs225 put a CALL's copy on another healthy path, but a
//     non-call lite conn's second sub (the lite upgrade mconn.go, the raw
//     upgrade mconn_raw.go: pings, games, echoes) goes through pickPlane,
//     whose path rule (dup_paths.go) only runs with HS2_DUP_PATHS=1 (off by
//     default). The pick was the least loaded light link anywhere — often
//     another link on the primary's path. With the key on, pickPlaneLockedFP
//     runs dup_paths.go's order for every light sub placed beside live links:
//     a link with no download on another healthy path; else a link with no
//     download on any path (a light copy behind a download waits in its
//     queue: lab note in dup_paths.go, so the light-link rule beats the path
//     rule); else any usable link on another path; else the usual pick. A
//     path the balancer marked lowered/cut (rescueLow) does not count as
//     healthy, so an off-path pick never lands on a path LB is draining.
//  2. Recovery. darkcall (mconn_darkcall.go) detects a dark sub link in ~1 s
//     and moves the primary with a resend from the records ring, but only
//     for calls. With the key on it also visits a lite (non-call, not heavy)
//     connection while one of its live subs rides a link the balancer marks
//     lowered/cut (Iran: rescueLow) or whose link has received nothing for
//     darkcallAfter (both sides — Kharej has no rescue marks; this is the
//     per-link silence a path's "silent" verdict is built from). Such a visit
//     acts like a call's: a dark primary switches to a lit sub on another
//     path not marked lowered and resends what the peer had not consumed;
//     a dark copy's chunks the others lack go to a lit sub (cs223); no such
//     sub (all copies on the dark path): Iran starts a records rescue (er97
//     join + RREC: both sides resend from their rings onto a fresh sub). The
//     in-flight cap stays calls-only (a game's bursts must not wait on it).
//     Cost: a sub's TCP_INFO read per tick only while its link is silent or
//     marked; trace lines for these visits read "darkcall ltrace".
//
// HS2_CS226_LITEPATH; an atomic: the darkcall timer reads it while tests flip it.
var cs226LitePathKey atomic.Bool

func init() { cs226LitePathKey.Store(envBoolDefault("HS2_CS226_LITEPATH", true)) }

func cs226LitePath() bool { return cs226LitePathKey.Load() }

var cs226Stat struct {
	visits atomic.Uint64 // lite (non-call) darkcall visits
	sw     atomic.Uint64 // ... dark primary switched to another path
	rrec   atomic.Uint64 // ... Iran records rescues started
	noCand atomic.Uint64 // ... dark primary with no lit sub on another path
}

// cs226Silent: s's link received nothing for darkcallAfter (tests replace
// cs226SilentHook; an atomic, the darkcall timer reads it).
var cs226SilentHook atomic.Pointer[func(*msub, int64) bool]

func cs226Silent(s *msub, nowc int64) bool {
	if h := cs226SilentHook.Load(); h != nil {
		return (*h)(s, nowc)
	}
	mtr := dcSubMeter(s)
	if mtr == nil {
		return false
	}
	// cs228: silent while pending — nothing heard for darkcallAfter AND a
	// write unanswered for darkcallAfter (a quiet idle link is not silent)
	return cs228SilentAt(mtr.lastRd.Load(), mtr.lastWr.Load(), mtr.pendSince.Load(), nowc)
}

// cs226LowLink: the balancer marks l's path lowered or cut (rescueLow).
func cs226LowLink(l any) bool { return cs226LitePath() && rescueLowKey(l) }

// cs226LiteVisitLocked (under m.mu): darkcall visits this non-call lite
// connection now.
func (m *mConn) cs226LiteVisitLocked(nowc int64) bool {
	if !cs228Visit() || !darkcallOn.Load() || m.heavy || m.wantBulk || m.mode != planeLight || m.stN > 0 {
		return false
	}
	m.dc.liteWalks++ // audit246 L3 test: one walk per timer visit
	if !m.dc.checkAt.IsZero() {
		return true // an action's outcome is due
	}
	for _, s := range m.subs {
		if s.dead || s.retired {
			continue
		}
		if s.dc.isDark {
			return true // a visit clears its dark state
		}
		if !cs228InFlight(s) {
			continue // cs228: an idle conn is not dark
		}
		if rescueLowKey(s.key) && cs228Pending(s, nowc) || cs226Silent(s, nowc) {
			return true
		}
	}
	return false
}

func cs226LogPart() string {
	return fmt.Sprintf(" · cs226 lite visits %d switched %d rrec %d nocand %d · lite 2nd sub otherpath %d samepath %d"+
		" · cs228 unlocked %d qual known %d no %d drop %d",
		cs226Stat.visits.Load(), cs226Stat.sw.Load(), cs226Stat.rrec.Load(), cs226Stat.noCand.Load(),
		obs.mconnPathOther.Load(), obs.mconnPathSame.Load(),
		cs228Stat.pre.Load(), cs228Stat.qualKnown.Load(), cs228Stat.qualNo.Load(), cs228Stat.qualDrop.Load()) + cs229LogPart()
}
