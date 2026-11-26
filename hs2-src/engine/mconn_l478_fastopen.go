package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// l478 HS2_L478_FASTOPEN (default off, lab A/B; no wire change), with
// HS2_L477_TAILRES:
//
// Trace l477 x1D (lab3, 1/4 links blackholed 5 s): b449's dark open came at
// ~1.1 s of silence in 67 of 76 cases (not ~3 s), but (1) every byte written
// after the tail resend still went to the dark primary's kernel buffer (in
// lite the primary is never moved; its queue stays empty while the socket
// buffer has room, so b449's stuck rule never arms) and waited for the dead
// link's own TCP retransmission after the blackhole (5.3 s / 9.2 s = RTO
// backoff), and (2) the one fresh sub per silence could land on another
// blackholed link (sample w=0: s2 on the dead group) with no second try.
//
// Now: (a) when a lite conn's dark primary resends its tail on a live sub
// (l477TargetOK: its link read within 400 ms), that sub becomes the primary
// (both sides; new writes go to the live link); (b) Iran's dark open looks
// after max(l478SilMin, 3 x srtt) of silence while pending instead of
// darkcallAfter (1 s), and a fresh sub whose own link is silent too gets one
// more open (at most l478MaxOpens per silence); every open respects the
// global bound l478Global in flight, and none during a ZDU drain.
var l478FastOpen atomic.Bool

func init() { l478FastOpen.Store(envBoolDefault("HS2_L478_FASTOPEN", false)) }

const (
	l478SilMin   = 600 * time.Millisecond
	l478MaxOpens = 2
)

var (
	l478Global   = int64(envIntDef("HS2_L478_GLOBAL", 256)) // fresh opens in flight, process-wide
	l478Inflight atomic.Int64
	l478Stat     struct {
		moves  atomic.Uint64 // dark primaries replaced by a live sub
		opens  atomic.Uint64 // early / second dark opens
		second atomic.Uint64 // ... of them second opens in one silence
		capped atomic.Uint64 // opens refused by l478Global
		drain  atomic.Uint64 // opens refused during a ZDU drain
	}
)

func l478LogPart() string {
	return fmt.Sprintf(" · l478 fastopen on %v moves %d opens %d second %d capped %d drain %d", l478FastOpen.Load(),
		l478Stat.moves.Load(), l478Stat.opens.Load(), l478Stat.second.Load(), l478Stat.capped.Load(), l478Stat.drain.Load())
}

// l478SilHook: tests replace the early silence check.
var l478SilHook atomic.Pointer[func(*msub, int64) bool]

// l478Silent: s's link heard nothing and a write of ours waited unanswered
// for max(l478SilMin, 3 x srtt).
func l478Silent(s *msub, nowc int64) bool {
	if h := l478SilHook.Load(); h != nil {
		return (*h)(s, nowc)
	}
	mtr := dcMeterOf(s)
	if mtr == nil {
		return false
	}
	th := max(l478SilMin, 3*s.subSrtt)
	rd, wr, ps := mtr.lastRd.Load(), mtr.lastWr.Load(), mtr.pendSince.Load()
	return rd != 0 && wr > rd && time.Duration(nowc-rd) >= th && time.Duration(nowc-ps) >= th
}

// l478MovePrimaryLocked (under m.mu, from l477TailResLocked): s is dark with
// bytes in flight and b is a live target.
func (m *mConn) l478MovePrimaryLocked(s, b *msub) {
	if !l478FastOpen.Load() || b == nil || b == s || m.primary.Load() != s || b.bulk.Load() {
		return
	}
	m.prefPrimary = b
	m.setPrimaryLocked()
	if m.primary.Load() != b {
		return
	}
	l478Stat.moves.Add(1)
	m.l480ArmLocked(s, b) // l480 STICK (mconn_l480_stick.go)
	m.tr.ev("l478: dark primary sub %d, live sub %d is the primary", s.idx, b.idx)
	m.sendCond.Broadcast()
}

// l478OpenOK: a fresh open may start now (global bound, no ZDU drain); it
// takes a slot released by l478OpenDone.
func l478OpenOK() bool {
	if ZDUDraining() {
		l478Stat.drain.Add(1)
		return false
	}
	if l478Inflight.Add(1) > l478Global {
		l478Inflight.Add(-1)
		l478Stat.capped.Add(1)
		return false
	}
	return true
}

func l478OpenDone() { l478Inflight.Add(-1) }
