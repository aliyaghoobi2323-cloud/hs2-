package engine

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

// b455 HS2_LITE_HEDGE2=1 (both sides, lite; default off): hedge from byte 0
// (F1b, lab/b446/run3/F1b-design.txt).
//
// b446's F1 opened a lite connection's second light sub with the
// connection, but the request bytes the first sub wrote before the async
// hedge sub joined were never copied onto it (addSubLocked marks the hole:
// missEnd = sendOff), so a request lost on the first link still waited a
// link RTT/RTO. F1b keeps those bytes:
//
//   - Iran (newMConnIran): the hedge open starts with the connection, async;
//     the first sub writes at once (the first byte never waits).
//   - Early buffer (both sides): while the connection is younger than
//     hedgeDur and has sent at most hedgeBytes, every DATA chunk sendLocked
//     queues is also held here with its own reference (independent of the
//     sub queues and the stripe/keep rules). Past the window it is released.
//   - Join replay: a light sub joining while the buffer still covers every
//     byte sent ([0, sendOff)) is given those chunks first, in order, and no
//     hole is marked (lastHole, missEnd stay zero). The receiver drops
//     duplicate offsets (deliver: end <= r.next / covered).
//   - Kharej does the same for the response when a second light sub joins
//     early (only the hedge makes one that fast): no wire change.
//   - Budget: replayed bytes count as lite duplicates (liteBud, 5 %); no
//     replay and no hedge while it is capped.
//   - Never bulk or striped connections (mode, stripe subs), never raw ones
//     (the raw first sub writes outside sendLocked; with the key Iran opens
//     records for a hedged connection, as F1 did).
//
// A heavy connection cannot be told apart at connect time. Its cost is
// bounded: replay at most hedgeBytes, duplication only under mDupMaxRate,
// the hedge sub of a bulky connection closed after hedgeIdle.
var liteHedge = os.Getenv("HS2_LITE_HEDGE2") == "1" || liteHedge3 // b459: HEDGE3 runs on this machinery

const (
	hedgeDur   = 1500 * time.Millisecond
	hedgeBytes = 32 << 10
	hedgeIdle  = 2 * time.Second
)

// hedgeEarly: the per-connection early buffer (under mConn.mu).
type hedgeEarly struct {
	until  time.Time // the window's end (zero: never buffer)
	end    uint64    // the end offset of the buffered run (== sendOff while complete)
	chunks []*mchunk
	bytes  int
	off    bool // window over or broken: no more buffering
}

// hedgeStat (diagnostics, logged by hedgeLog).
var hedgeStat struct {
	opened, capped, openFail, won, lost, bulkyClose atomic.Uint64
	replays, replayB, replayCap, broken             atomic.Uint64
}

// hedgeStartLocked (Iran, newMConnIran, under m.mu, the first sub added and
// not started yet): open the hedge sub in the background.
func (m *mConn) hedgeStartLocked(firstKey any) {
	if !liteHedge || !m.lite || !m.iran || m.open == nil || m.mode != planeLight {
		return
	}
	if !hedgeKeyOK(firstKey) { // rc15: an old exit (no capExtLiteHedge) gets no hedge (mconn_rc15_hedgecap.go)
		hedgeCapStat.noCap.Add(1)
		return
	}
	now := time.Now()
	hedgeLog(now)
	if liteBud.capped(now.Unix()) {
		hedgeStat.capped.Add(1)
		return
	}
	m.lightOpening = true // the lite upgrade waits for this open
	m.hedgeEnd = now.Add(hedgeEarlyDur())
	m.early.until = m.hedgeEnd
	avoid := make([]any, 0, 2+len(m.deadKeys))
	avoid = append(avoid, avoidKey(firstKey))
	avoid = m.callAvoidLocked(append(avoid, m.deadAvoidLocked()...)) // HS2_MCONN_CALL_AVOID

	// b459: the hedge sub may create the conn at the exit (mconn_b459_hedge3.go)
	if liteHedge3 {
		m.h3 = true
		hedge3Stat.hedged.Add(1)
		hedge3Log(now)
		if liteHedge7 {
			m.h7Born = now // b465 (mconn_b465_hedge7.go)
		}
		avoid = append(avoid, hedgeMark{})
	}
	go m.openHedge(avoid)
}

// hedgeExitInit (Kharej, newMConnExit): the response's early window.
func (m *mConn) hedgeExitInit() {
	if liteHedge && !m.iran {
		m.early.until = m.zsBorn.Add(hedgeEarlyDur())
	}
}

// earlyKeepLocked (sendLocked, light path, c just given its offset): hold c
// while the window is open.
func (m *mConn) earlyKeepLocked(c *mchunk) {
	e := &m.early
	if e.off || e.until.IsZero() {
		return
	}
	end := c.off + uint64(c.n)
	if !m.lite || c.off != e.end || end > hedgeEarlyBytes() || m.mode != planeLight || time.Now().After(e.until) {
		if c.off != e.end {
			hedgeStat.broken.Add(1)
		}
		m.earlyReleaseLocked()
		return
	}
	c.refs.Add(1)
	e.chunks = append(e.chunks, c)
	e.bytes += c.n
	e.end = end
}

// earlyReleaseLocked drops the buffer for good.
func (m *mConn) earlyReleaseLocked() {
	e := &m.early
	for _, c := range e.chunks {
		c.unref()
	}
	e.chunks, e.bytes, e.off = nil, 0, true
}

// earlyTickLocked (manager): release at the window's end; the wait until it.
func (m *mConn) earlyTickLocked(now time.Time) time.Duration {
	e := &m.early
	if e.off || e.until.IsZero() {
		return 0
	}
	if !now.Before(e.until) {
		m.earlyReleaseLocked()
		return 0
	}
	return e.until.Sub(now)
}

// earlyReplayLocked (addSubLocked, a light sub s joining, not started yet):
// give s every byte sent so far from the buffer; true: s lacks nothing.
func (m *mConn) earlyReplayLocked(s *msub, now time.Time) bool {
	e := &m.early
	if e.off || e.until.IsZero() || m.sendOff == 0 || e.end != m.sendOff || len(e.chunks) == 0 {
		return false
	}
	if now.After(e.until) || m.mode != planeLight || m.stripeN() || !m.lite {
		m.earlyReleaseLocked()
		return false
	}
	if liteBud.capped(now.Unix()) {
		hedgeStat.replayCap.Add(1)
		return false
	}
	for _, c := range e.chunks {
		s.push(c)
	}
	s.lastPush = now
	m.budAddLocked(now, 0, e.bytes)
	obs.mconnDupBytes.Add(uint64(e.bytes))
	hedgeStat.replays.Add(1)
	hedgeStat.replayB.Add(uint64(e.bytes))
	m.tr.ev("hedge: replay %d chunks %d bytes on sub %d", len(e.chunks), e.bytes, s.idx)
	if !m.iran {
		hedgeLog(now)
	}
	return true
}

// openHedge is openLight that marks the sub as the hedge.
func (m *mConn) openHedge(avoid []any) {
	sub, key, err := m.open(planeLight, avoid)
	m.mu.Lock()
	m.lightOpening = false
	if err != nil {
		hedgeStat.openFail.Add(1)
		m.mu.Unlock()
		m.kickMgr()
		return
	}
	if m.ended || m.closing || m.mode != planeLight || m.wantBulk || m.peerFin || m.wclosed {
		m.mu.Unlock()
		sub.Close()
		m.kickMgr()
		return
	}
	s := m.addSubLocked(sub, key, false) // replays the early buffer (earlyReplayLocked)
	m.hedgeSub.Store(s)
	hedgeStat.opened.Add(1)
	m.tr.ev("hedge: sub+ %d", s.idx)
	m.sendCond.Broadcast()
	m.mu.Unlock()
	m.startSub(s)
	m.kickMgr()
}

// hedgeBulkyLocked: the connection moved more than hedgeBytes either way.
func (m *mConn) hedgeBulkyLocked() bool {
	if m.sendOff > hedgeBytes {
		return true
	}
	r := &m.r
	r.mu.Lock()
	recvd := r.next
	r.mu.Unlock()
	return recvd > hedgeBytes
}

// hedgeTickLocked (manager, two live subs): the wait until the hedge window
// ends (0: none pending), so the idle rule looks again then.
func (m *mConn) hedgeTickLocked(now time.Time) time.Duration {
	if m.hedgeSub.Load() == nil || m.hedgeEnd.IsZero() {
		return 0
	}
	if now.Before(m.hedgeEnd) {
		return m.hedgeEnd.Sub(now)
	}
	return 0
}

// hedgeIdleLocked: the idle time after which second sub s is closed; lim
// unless s is a bulky connection's hedge sub past its window.
func (m *mConn) hedgeIdleLocked(s *msub, now time.Time, lim time.Duration) time.Duration {
	if s != m.hedgeSub.Load() || now.Before(m.hedgeEnd) || !m.hedgeBulkyLocked() {
		return lim
	}
	if now.Sub(monoTime(s.lastRecv.Load())) >= hedgeIdle && now.Sub(s.lastPush) >= hedgeIdle {
		hedgeStat.bulkyClose.Add(1)
	}
	return min(lim, hedgeIdle)
}

// hedgeNoteFirst (deliver, under m.r.mu): the first chunk was delivered
// from sub s.
func hedgeNoteFirst(m *mConn, s *msub) {
	h := m.hedgeSub.Load()
	if h == nil {
		return
	}
	if s == h {
		hedgeStat.won.Add(1)
	} else {
		hedgeStat.lost.Add(1)
	}
}

var hedgeLogAt atomic.Int64

// hedgeLog: the counters, at most every 10 s.
func hedgeLog(now time.Time) {
	n := monoNano(now)
	at := hedgeLogAt.Load()
	if n-at < int64(10*time.Second) || !hedgeLogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("b455 hedge: opened=%d capped=%d fail=%d first_won=%d first_lost=%d bulky_close=%d replays=%d replayed=%dKB replay_capped=%d broken=%d",
		hedgeStat.opened.Load(), hedgeStat.capped.Load(), hedgeStat.openFail.Load(),
		hedgeStat.won.Load(), hedgeStat.lost.Load(), hedgeStat.bulkyClose.Load(),
		hedgeStat.replays.Load(), hedgeStat.replayB.Load()>>10, hedgeStat.replayCap.Load(), hedgeStat.broken.Load())
}

// hedgeWantRecords (the Iran opener, first lite sub): with the key on and
// the budget open, the first sub is not raw (a raw first sub has no copies
// and writes outside sendLocked).
func hedgeWantRecords() bool {
	if noRaw463 { // b463 diagnostic (mconn_b463_noraw.go)
		return true
	}
	return liteHedge && !liteBud.capped(time.Now().Unix())
}
