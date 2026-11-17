package engine

import (
	"fmt"
	"sync/atomic"
)

// cs224: lab2 C2 (cs221 final, bh_e120) stalled the call 11.3 s. Call conn
// d0c61723142324c7, subs 0 (path 54.1, blackholed 22:00:16) and 1 (58.1):
//   - 22:00:17.6 darkcall switched the primary 0 -> 1 (resent 0, cs221);
//     sub 1 carried the call alone (~290 KB/s) from then on;
//   - 22:00:24.72 (Iran first, Kharej ~0.2 s later) sub 1 left with no
//     line of its own, in the link sweep's tick that drained 3 stuck links
//     and reclaimed idle connections on 17 retiring ones ("closed 3
//     connection(s) on degraded links that moved no data", "closed 9
//     connection(s) idle on retiring links" in the next minute line). Sub 1
//     had joined as the copy sub, so its stream is copyOnly, and flowStats
//     never moved a copyOnly stream's lastActive (it skips the rate EWMA for
//     copies, lbv81 lb_copyslow81.go): idleStreams (bytes == prevBytes right
//     after the sampler, lastActive = open time) took every copy sub older
//     than the idle bound as idle however much it carried, both for the
//     drain reclaim (15 s) and the retiring-link reclaim (reclaimIdle); the
//     snapshot guard checks the first candidate microseconds later.
//     Both sides lost the healthy primary with no line of their own (Iran:
//     the sub died; Kharej: its read ended), "call resends" 1 -> 2 on both;
//   - the primary fell back to the first sub, dark sub 0, and darkcall
//     switched AGAIN from it: its marks were those of 17.6 (unc 265893
//     both times), so Iran resent 266893 B the peer had long read, plus the
//     moved queue, at the front of the fresh 32 KB-bound sub 2 (ir rx 0 KB,
//     stalled 1.4-2.6 s; call samples up to 11.3 s).
//
// HS2_CS224_COPYIDLE (default on, "0" = old): flowStats moves a copyOnly
// stream's lastActive when its bytes moved (no EWMA, no demand count: the
// copy stays out of flowing/heavy/recent as before). A copy sub is then
// "idle" for the drain reclaim only when it moved nothing for drainStall.
var cs224CopyIdle atomic.Bool

func init() { cs224CopyIdle.Store(envBoolDefault("HS2_CS224_COPYIDLE", true)) }

// HS2_CS224_STALESWITCH (default on, "0" = old): a darkcall switch from a sub
// that an earlier switch already left, dark ever since (it is primary again
// only because the sub the switch chose is gone), resends nothing from the
// ring: its written/consumed marks are frozen at that first switch, which
// already handled them; what the gone sub held comes back by its own death
// resend. Its queued chunks still move to the new primary (the gone sub's
// death resend may have been queued there).
var cs224StaleSwitchOn atomic.Bool

func init() { cs224StaleSwitchOn.Store(envBoolDefault("HS2_CS224_STALESWITCH", true)) }

var cs224Stat struct {
	stale atomic.Uint64 // switches from an already-left sub: ring resend skipped
}

// cs224StaleSwitch (under m.mu): p, the dark primary being switched off,
// was left by an earlier switch and has stayed dark.
func cs224StaleSwitch(p *msub) bool {
	if !cs224StaleSwitchOn.Load() || !p.dcLeft {
		return false
	}
	cs224Stat.stale.Add(1)
	return true
}

func cs224LogPart() string {
	return fmt.Sprintf(" · cs224 staleswitch %d", cs224Stat.stale.Load())
}
