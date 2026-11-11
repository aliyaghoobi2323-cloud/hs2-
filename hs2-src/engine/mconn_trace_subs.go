package engine

import (
	"fmt"
	"strings"
	"time"
)

// Per-sub trace (HS2_MCONN_TRACE, docs/reality-architecture.md 8.12): which
// copy of a call arrives first, and how late the other one is. On receive,
// each sub counts the bytes it delivered first and the bytes that were
// already there (the other copy won); for those, the time since the first
// copy of the same bytes arrived (the copy's lag). Once a second the manager
// writes one line per connection: per sub its socket's addresses, first/dup
// KB, the worst lag, its send queue and the socket's TCP_INFO. Nothing of
// this runs with the trace off (m.tr nil).

const mTraceRing = 1024

// mtraceRx (under m.r.mu): first-arrival times of recent byte ranges.
type mtraceRx struct {
	ends  [mTraceRing]uint64
	ats   [mTraceRing]int64
	n     int // entries written (ring index n % mTraceRing)
	repAt time.Time
}

// mtraceSub: per-sub counters (under m.r.mu).
type mtraceSub struct {
	first, dup int64
	lagMax     time.Duration
}

// traceFirstLocked (m.r.mu): bytes up to end arrived first on s now.
func (m *mConn) traceFirstLocked(s *msub, end uint64, n int) {
	x := m.trRx
	if x == nil {
		return
	}
	i := x.n % mTraceRing
	x.ends[i], x.ats[i] = end, time.Now().UnixNano()
	x.n++
	s.tr.first += int64(n)
}

// traceDupLocked (m.r.mu): [off,end) arrived on s after another copy.
func (m *mConn) traceDupLocked(s *msub, off, end uint64, n int) {
	x := m.trRx
	if x == nil {
		return
	}
	s.tr.dup += int64(n)
	// the first entry whose end covers end (ends grow with time)
	lo := max(0, x.n-mTraceRing)
	for j := lo; j < x.n; j++ {
		k := j % mTraceRing
		if x.ends[k] >= end {
			if lag := time.Duration(time.Now().UnixNano() - x.ats[k]); lag > s.tr.lagMax {
				s.tr.lagMax = lag
			}
			return
		}
	}
}

// traceSubsTick (manager, m.mu NOT held): the per-sub line, once a second.
func (m *mConn) traceSubsTick(now time.Time) {
	if m.tr == nil || m.trRx == nil {
		return
	}
	m.mu.Lock()
	subs := append([]*msub(nil), m.subs...)
	q := make([]int, len(subs))
	for i, s := range subs {
		q[i] = s.queueLocked()
	}
	m.mu.Unlock()
	r := &m.r
	r.mu.Lock()
	if now.Sub(m.trRx.repAt) < time.Second {
		r.mu.Unlock()
		return
	}
	m.trRx.repAt = now
	type row struct {
		f, d int64
		lag  time.Duration
	}
	rows := make([]row, len(subs))
	for i, s := range subs {
		rows[i] = row{s.tr.first, s.tr.dup, s.tr.lagMax}
		s.tr = mtraceSub{}
	}
	r.mu.Unlock()
	var b strings.Builder
	for i, s := range subs {
		if i > 0 {
			b.WriteString(" | ")
		}
		addr, info := "?", ""
		if tc := subTCP(s); tc != nil {
			addr = fmt.Sprintf("%v>%v", tc.LocalAddr(), tc.RemoteAddr())
			info = tcpTraceInfo(tc)
		}
		p := callPathOf(s.key)
		fmt.Fprintf(&b, "s%d %s path=%s first=%dK dup=%dK lag=%v q=%d %s", s.idx, addr, p,
			rows[i].f>>10, rows[i].d>>10, rows[i].lag.Round(time.Millisecond), q[i], info)
	}
	m.tr.ev("subs: %s", b.String())
}
