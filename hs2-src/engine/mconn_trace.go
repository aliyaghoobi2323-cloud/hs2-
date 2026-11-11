package engine

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// mconn trace (HS2_MCONN_TRACE=1, off by default): the events of an mConn's
// life with a relative clock, to see where a connection waits. Lines are
// written only for connections that look like a call — the busier direction
// moved between mTraceMinRate and mTraceMaxRate over the first mTraceWindow
// (events before the decision are kept in a small buffer and written then) —
// or, with HS2_MCONN_TRACE_PORT=<port>, for the connections of that port only.
// HS2_MCONN_TRACE_GAME=1 (rel52, lab) also traces game-like connections: a
// light two-way stream (mTraceGameMin..mTraceGameMax in the window, the smaller
// direction at least half the larger, so crowd request/reply pairs stay out).
// When off, an mConn carries a nil *mtrace and every hook is one nil check.

var (
	mtraceOn   = os.Getenv("HS2_MCONN_TRACE") == "1"
	mtracePort = func() int { n, _ := strconv.Atoi(os.Getenv("HS2_MCONN_TRACE_PORT")); return n }()
	mtraceGame = os.Getenv("HS2_MCONN_TRACE_GAME") == "1"
)

const (
	mTraceWindow  = 10 * time.Second
	mTraceMinRate = 1_000_000 / 8 * 10 // bytes in the window (1 Mbit/s)
	mTraceMaxRate = 4_000_000 / 8 * 10 // 4 Mbit/s
	mTraceGameMin = 8_000 / 8 * 10     // 8 kbit/s
	mTraceGameMax = 200_000 / 8 * 10   // 200 kbit/s
	mTraceReport  = time.Second
	mTraceSlow    = 50 * time.Millisecond
	mTraceBufMax  = 64
)

type mtrace struct {
	mu       sync.Mutex
	t0       time.Time
	id       uint64
	iran     bool
	port     int
	state    int // 0 undecided, 1 on, -1 off
	buf      []string
	sent     atomic.Int64 // payload bytes written by the app
	recvd    atomic.Int64 // payload bytes delivered to the app
	lastRep  time.Time
	maxWait  time.Duration // longest Write wait since the last report
	maxGap   time.Duration // oldest reassembly gap seen since the last report
	gapSince time.Time     // under m.r.mu: since when ooo is non-empty
}

func newMTrace(id uint64, iran bool) *mtrace {
	if !mtraceOn {
		return nil
	}
	now := time.Now()
	return &mtrace{t0: now, id: id, iran: iran, lastRep: now}
}

func (t *mtrace) setPort(port int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.port = port
	if mtracePort != 0 {
		if port == mtracePort {
			t.enableLocked("port")
		} else {
			t.state = -1
			t.buf = nil
		}
	}
	t.mu.Unlock()
}

func (t *mtrace) side() string {
	if t.iran {
		return "iran"
	}
	return "kharej"
}

func (t *mtrace) enableLocked(why string) {
	t.state = 1
	log.Printf("mtrace %s id=%d port=%d: tracing (%s, sent=%d recvd=%d in %v)", t.side(), t.id, t.port, why,
		t.sent.Load(), t.recvd.Load(), time.Since(t.t0).Round(time.Millisecond))
	for _, l := range t.buf {
		log.Print(l)
	}
	t.buf = nil
}

// decideLocked settles whether this connection is traced once the window is
// over (volume rule; the port rule decided in setPort).
func (t *mtrace) decideLocked(now time.Time) {
	if t.state != 0 || mtracePort != 0 || now.Sub(t.t0) < mTraceWindow {
		return
	}
	sent, recvd := t.sent.Load(), t.recvd.Load()
	v := max(sent, recvd)
	if v >= mTraceMinRate && v <= mTraceMaxRate {
		t.enableLocked("call-like volume")
	} else if mtraceGame && v >= mTraceGameMin && v <= mTraceGameMax && 2*min(sent, recvd) >= v {
		t.enableLocked("game-like volume")
	} else {
		t.state = -1
		t.buf = nil
	}
}

// ev records one event (printf style).
func (t *mtrace) ev(format string, args ...any) {
	if t == nil {
		return
	}
	now := time.Now()
	line := fmt.Sprintf("mtrace %s id=%d +%7.3fs ", t.side(), t.id, now.Sub(t.t0).Seconds()) + fmt.Sprintf(format, args...)
	t.mu.Lock()
	t.decideLocked(now)
	switch t.state {
	case 1:
		log.Print(line)
	case 0:
		if len(t.buf) < mTraceBufMax {
			t.buf = append(t.buf, line)
		}
	}
	t.mu.Unlock()
}

// noteWait records how long one Write waited for queue room.
func (t *mtrace) noteWait(d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	t.mu.Lock()
	if d > t.maxWait {
		t.maxWait = d
	}
	t.mu.Unlock()
}

// noteGap records the age of the reassembly gap seen now.
func (t *mtrace) noteGap(d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	t.mu.Lock()
	if d > t.maxGap {
		t.maxGap = d
	}
	t.mu.Unlock()
}

// report writes the periodic line (once a second, when something was slow or
// a queue is standing): the longest Write wait, the oldest gap, and the
// primary/bulk queue size. Called from the hot paths with m.mu NOT held;
// q is the current send queue (bytes) of the sub the direction rides on.
func (t *mtrace) report(q int, qSubs string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	t.decideLocked(now)
	if t.state != 1 || now.Sub(t.lastRep) < mTraceReport {
		t.mu.Unlock()
		return
	}
	t.lastRep = now
	w, g := t.maxWait, t.maxGap
	t.maxWait, t.maxGap = 0, 0
	t.mu.Unlock()
	if w > mTraceSlow || g > mTraceSlow || q > 0 {
		t.ev("report: max_write_wait=%v max_gap_age=%v sendq=%d subq=[%s] sent=%d recvd=%d",
			w.Round(time.Millisecond), g.Round(time.Millisecond), q, qSubs, t.sent.Load(), t.recvd.Load())
	}
}

// subsLocked describes the subs' queues (under m.mu).
func (m *mConn) traceSubsLocked() string {
	s := ""
	for i, sub := range m.subs {
		if i > 0 {
			s += " "
		}
		tag := "L"
		if sub.bulk.Load() {
			tag = "B"
		}
		if sub.retired {
			tag += "r"
		}
		s += fmt.Sprintf("%s%d:%d", tag, sub.idx, sub.queueLocked())
	}
	return s
}
