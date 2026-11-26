package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// l485 CALLOPEN (HS2_L485_CALLOPEN, default on since lab3 a; no wire change). lab3 l485
// t1Q (3fc3a22b, l475/l476 fault): a stuck call's ~35-60 fresh subs in ~1 s
// all came from openLight2, the b449e slow open's second open. Its tick
// checks only lightOpen2: once a second open joined, lightOpen2 is false
// again while the first open (openLight, blocked in OpenStream on a
// blackholed link) is still pending with the old lightOpenT0, so the next
// manager tick (every call write, ~20 ms) started yet another second open.
// About 30 % of those landed on blackholed links (no LIVEPICK mark on that
// avoid). With the key on:
//   - one second open per pending open (lightOpenT0 it doubled is kept);
//   - every light open of a call (and the slow open's second open) carries
//     LIVEPICK's l480LiveMark: only links that read within l480LiveFor
//     (pool-wide; with none, the pick is unchanged);
//   - a connection with l485FreshMax subs joined within l485FreshFor that
//     received nothing yet opens no more call/slow-open subs until one of
//     them reads or ages out;
//   - the avoid lists already hold every sub's link (dark subs included),
//     so a fresh sub never rides a dark sub's link.
var l485CallOpen atomic.Bool

const (
	l485FreshMax = 3
	l485FreshFor = 2 * time.Second
)

var l485Stat struct {
	soDup  atomic.Uint64 // slow-open ticks that held a repeat second open for the same pending open
	capped atomic.Uint64 // opens held by the fresh-sub cap
}

func init() {
	l485CallOpen.Store(envBoolDefault("HS2_L485_CALLOPEN", true)) // l485: on (lab3 a, 6 ABBA pairs: call99 -2769 [-4919,-619], callmax -4110 [-7304,-915], nothing worse)
}

// l485FreshCapLocked: true when m already has l485FreshMax fresh silent subs
// (joined < l485FreshFor ago, nothing received since) and the key is on.
func (m *mConn) l485FreshCapLocked(now time.Time) bool {
	if !l485CallOpen.Load() {
		return false
	}
	nm := monoNano(now)
	n := 0
	for _, s := range m.subs {
		if s.l485Join != 0 && nm-s.l485Join < int64(l485FreshFor) && s.lastRecv.Load() == s.l485Join {
			n++
		}
	}
	if n >= l485FreshMax {
		l485Stat.capped.Add(1)
		return true
	}
	return false
}

// l485Live: avoid with LIVEPICK's mark when the key is on.
func l485Live(avoid []any) []any {
	if !l485CallOpen.Load() {
		return avoid
	}
	return l480WithLiveAlways(avoid)
}

// l480WithLiveAlways: a copy of avoid with LIVEPICK's mark (once), whatever
// HS2_L480_LIVEPICK says.
func l480WithLiveAlways(avoid []any) []any {
	for _, a := range avoid {
		if _, ok := a.(l480LiveMark); ok {
			return avoid
		}
	}
	return append(avoid[:len(avoid):len(avoid)], l480LiveMark{})
}

func l485LogPart() string {
	return fmt.Sprintf(" · l485 callopen %v sodup %d capped %d", l485CallOpen.Load(), l485Stat.soDup.Load(), l485Stat.capped.Load())
}
