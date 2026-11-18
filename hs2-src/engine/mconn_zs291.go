package engine

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// zs291 (HS2_ZS291=1, diagnosis, default off): every minute the mem line
// also counts the live (not ended) connections with no live sub that are
// older than zsMinAge, and shows a few of them with the state that holds
// them and the last thing that took a sub away. sk286 (rel69l): the exit's
// mconns crept +86/h with msubs flat.
var zsOn atomic.Bool

func init() { zsOn.Store(envBoolDefault("HS2_ZS291", false)) }

const zsMinAge = 60 * time.Second

// zsState (under mu).
type zsState struct {
	zsBorn   time.Time
	zsLast   string // removeSubLocked's caller
	zsBranch string // subGone's outcome
	zsLastAt time.Time
}

func (m *mConn) zsNoteRemoveLocked() {
	if !zsOn.Load() {
		return
	}
	m.zsLastAt = time.Now()
	m.zsLast = "?"
	if pc, _, _, ok := runtime.Caller(2); ok {
		if f := runtime.FuncForPC(pc); f != nil {
			n := f.Name()
			m.zsLast = n[strings.LastIndexByte(n, '.')+1:]
		}
	}
}

func (m *mConn) zsNoteBranchLocked(b string, cause error) {
	if zsOn.Load() {
		m.zsBranch = fmt.Sprintf("%s(%v)", b, cause)
	}
}

// zsLine: " zs291: zero=N oldest=… [samples]".
func zsLine() string {
	if !zsOn.Load() {
		return ""
	}
	memMConns.mu.Lock()
	var ms []*mConn
	for w := range memMConns.m {
		if m := w.Value(); m != nil {
			ms = append(ms, m)
		} else {
			delete(memMConns.m, w)
		}
	}
	memMConns.mu.Unlock()
	now := time.Now()
	n, live := 0, 0
	var oldest time.Duration
	var sm []string
	for _, m := range ms {
		m.mu.Lock()
		if !m.ended {
			live++
			alive := 0
			for _, s := range m.subs {
				if !s.dead {
					alive++
				}
			}
			age := now.Sub(m.zsBorn)
			if alive == 0 && age >= zsMinAge {
				n++
				oldest = max(oldest, age)
				if len(sm) < 4 {
					sm = append(sm, fmt.Sprintf("[%x age=%v iran=%v subs=%d nsubs=%d mode=%v stN=%d lite=%v raw1=%v closing=%v wclosed=%v peerFin=%v hs=%v grace=%v rstHold=%v rstPhase=%d last=%s/%s %v ago]",
						m.id&0xffffff, age.Round(time.Second), m.iran, len(m.subs), m.nsubs, m.mode, m.stN, m.lite, m.raw1 != nil,
						m.closing, m.wclosed, m.peerFin, m.handshaking, !m.graceUntil.IsZero(), m.rstHold, m.rstPhase,
						m.zsLast, m.zsBranch, now.Sub(m.zsLastAt).Round(time.Second)))
				}
			}
		}
		m.mu.Unlock()
	}
	return fmt.Sprintf(" zs291: live=%d zero=%d oldest=%v neverended=%d reslost=%v %s", live, n, oldest.Round(time.Second), zsNeverEnded.Load(), zsResLost(), strings.Join(sm, " "))
}

func zsBranchName(subs int, endErr error, hold, grace bool) string {
	switch {
	case endErr != nil:
		return "end"
	case subs > 0:
		return "keep"
	case hold:
		return "rsthold"
	case grace:
		return "grace"
	}
	return "zero"
}

// zsNeverEnded: connections closed before any sub joined, ended at once by
// Close (zs291). Before, a raw exit conn that lost the reserve race kept
// obs.mconnOpen +1 for good (no manager to see the close).
var zsNeverEnded atomic.Uint64

// zsReserveLost: serveMConnSub first subs that found the id already
// reserved, by role&7 (diagnosis).
var zsReserveLost [8]atomic.Uint64

func zsResLost() []uint64 {
	v := make([]uint64, len(zsReserveLost))
	for i := range zsReserveLost {
		v[i] = zsReserveLost[i].Load()
	}
	return v
}
