package engine

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// l488 FRESHBACK (HS2_L488_FRESHBACK, default on since l488 (=0 off); no wire change; Iran edge
// picks only). Lab3 l487b (14 ABBA pairs, probe 22/s): every light sample
// over 2 s fell inside a blackhole window or up to ~2 s after it ended. After
// the flush 16..17 links stayed in TCP retransmit backoff for up to ~4.2 s:
// the peer's retransmit got through first, so the link READ again (NEWPICK's
// l480LinkSilent no longer left it out) while this side's own writes still
// waited for its next exponential RTO. A probe whose first sub and hedge sub
// both landed on such links took 2..3.5 s.
//
// A link whose read resumes after it was silent while pending for
// l478SilMin (600 ms) is "recovering" from that read on, until its socket's
// TCP_INFO shows no retransmit backoff (backoff 0, ca_state not Loss), or at
// most l488RecMax. Without a socket to ask, it recovers for l488RecNoTCP.
// While recovering, a new light conn's first sub (l480FirstPick) and hedge
// sub (l480PendAvoid) leave it out like a silent link; with no other link
// the pick is unchanged (both paths already fall back).
var l488FreshBack atomic.Bool

var (
	l488RecMax   = time.Duration(envIntDef("HS2_L488_REC_MAX_MS", 5000)) * time.Millisecond
	l488RecNoTCP = time.Duration(envIntDef("HS2_L488_REC_NOTCP_MS", 1500)) * time.Millisecond
)

// l488: default on (lab3, 15 ABBA pairs).
func init() { l488FreshBack.Store(envBoolDefault("HS2_L488_FRESHBACK", true)) }

var l488Stat struct {
	marked  atomic.Uint64 // reads that resumed after a pending silence
	cleared atomic.Uint64 // recoveries ended by TCP_INFO (no backoff)
	expired atomic.Uint64 // ... by l488RecMax / l488RecNoTCP
	held    atomic.Uint64 // picks that found a link still recovering
}

func l488LogPart() string {
	return fmt.Sprintf(" · l488 freshback %v marked %d cleared %d expired %d held %d", l488FreshBack.Load(),
		l488Stat.marked.Load(), l488Stat.cleared.Load(), l488Stat.expired.Load(), l488Stat.held.Load())
}

// l488OnRead (meteredConn.Read, before lastRd moves to now): a read after a
// pending silence of l478SilMin starts a recovery.
func l488OnRead(m *linkMeter, now int64) {
	if !l488FreshBack.Load() {
		return
	}
	rd, wr, ps := m.lastRd.Load(), m.lastWr.Load(), m.pendSince.Load()
	if rd != 0 && wr > rd && time.Duration(now-rd) >= l478SilMin && time.Duration(now-ps) >= l478SilMin {
		m.l488Rec.Store(now)
		l488Stat.marked.Add(1)
	}
}

// l488TCPInfoBackoff: test hook (nil: tcpStats on the socket).
var l488TCPInfoBackoff func(tc *net.TCPConn) (backoff, caState uint8, ok bool)

func l488Backoff(tc *net.TCPConn) (uint8, uint8, bool) {
	if l488TCPInfoBackoff != nil {
		return l488TCPInfoBackoff(tc)
	}
	st, ok := tcpStats(tc)
	return st.backoff, st.caState, ok
}

// l488Recovering: the link is recovering (see above). Cheap when it is not:
// one atomic load; the socket is asked only while a recovery is open.
func l488Recovering(m *linkMeter, nowc int64) bool {
	if m == nil || !l488FreshBack.Load() {
		return false
	}
	at := m.l488Rec.Load()
	if at == 0 {
		return false
	}
	age := time.Duration(nowc - at)
	tc := m.l488Tcp
	lim := l488RecMax
	if tc == nil && l488TCPInfoBackoff == nil {
		lim = l488RecNoTCP
	}
	if age >= lim || age < 0 {
		if m.l488Rec.CompareAndSwap(at, 0) {
			l488Stat.expired.Add(1)
		}
		return false
	}
	if tc != nil || l488TCPInfoBackoff != nil {
		if bo, ca, ok := l488Backoff(tc); ok && bo == 0 && ca != 4 { // 4 = TCP_CA_Loss
			if m.l488Rec.CompareAndSwap(at, 0) {
				l488Stat.cleared.Add(1)
			}
			return false
		}
	}
	return true
}

// l488Avoid: l480's pending-silence test, plus recovering links when on.
func l488Avoid(m *linkMeter, nowc int64) bool {
	if l480LinkSilent(m, nowc) {
		return true
	}
	if l488Recovering(m, nowc) {
		l488Stat.held.Add(1)
		return true
	}
	return false
}
