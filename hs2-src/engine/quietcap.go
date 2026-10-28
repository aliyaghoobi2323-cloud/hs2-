package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// R1 round 4, the call in the home mix (decision T21 item 2), the first
// approach: a new connection leaves the QUIET link early.
//
// Every user connection opens on a QUIET link and moves to BULK on a
// persistent send backlog: the exit's hint after 300 ms of big writes
// (hint.go), then the edge's mover tick (up to 100 ms), then the move
// stream's trip; the upload rule likewise. Meanwhile the connection's
// bytes go on QUIET as fast as it takes them. In the home mix 16 browsers
// and 16 viewers start a page or a segment every few seconds, so QUIET
// carries the starts of all of them (~42 Mbit/s at N=1, round-3 judge), its
// latency lane and its socket are full, and a 1.5 Mbit/s call there waits
// seconds (p99 down 6.6 s at N=1, 27 s at N=8; delivered 0.68 at N=8).
//
// QuietCap (HS2_TUNE_QUIET_CAP, bytes; 0 off): a group connection that
// writes QuietCap bytes in big writes (≥ hintWriteMin, a backlog's chunks;
// an app-limited flow writes its own frames) within quietCapWin on its QUIET
// link is moved at once:
//
//   - at the exit (a download), the write that would pass the cap waits:
//     the exit sends its hint now (not after 300 ms) and holds the write
//     until the edge's move stream arrives (the group's switch), at most
//     QuietHold ms; then it goes on, on BULK, or on QUIET after the hold;
//   - at the edge (an upload), the write that would pass the cap wakes the
//     mover, which moves the connection at once, and waits for the switch,
//     at most QuietHold ms;
//   - the edge's mover acts on each hint at once, not at its next tick.
//
// A connection carries at most QuietCap (and what was on its way) on QUIET.
// A call's frames are small writes (a key frame of 18.75 KB each 2 s is
// under the cap in any window), so it stays.

// QuietCap: the bytes of big writes within quietCapWin that move a group
// connection off QUIET at once (HS2_TUNE_QUIET_CAP; 0: off).
var QuietCap = 0

// QuietHoldMs: the longest a write waits for the move (HS2_TUNE_QUIET_HOLD).
var QuietHoldMs = 500

// quietCapWin: the window QuietCap is counted over.
const quietCapWin = 500 * time.Millisecond

// capWindow counts a connection's big writes on its QUIET link.
type capWindow struct {
	mu    sync.Mutex
	start int64
	n     int
	fired bool
}

// add counts n bytes about to be written at now (ctrlNow) and says, once,
// whether they pass the cap.
func (w *capWindow) add(n int, now int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired || QuietCap <= 0 {
		return false
	}
	if now-w.start > int64(quietCapWin) {
		w.start, w.n = now, 0
	}
	w.n += n
	if w.n > QuietCap {
		w.fired = true
		return true
	}
	return false
}

// capStats: the cap's tallies (the hint status line).
var capStats struct {
	capped, moved, timeout atomic.Int64
}

// holdFor waits for sw (the group's switch) or done, at most QuietHoldMs:
// whether it switched.
func holdFor(sw, done <-chan struct{}) bool {
	t := time.NewTimer(time.Duration(QuietHoldMs) * time.Millisecond)
	defer t.Stop()
	select {
	case <-sw:
		capStats.moved.Add(1)
		return true
	case <-done:
	case <-t.C:
		capStats.timeout.Add(1)
	}
	return false
}

// capExit is the exit's cap on a write of n bytes of eg (a download): past
// it the hint goes now and the write waits for the move.
func (eg *exitGroup) capExit(n int) {
	m := eg.mtr.Load()
	if m == nil || !m.quietLink || MoveHintOff() || eg.twoWay.Load() || !eg.cap.add(n, ctrlNow()) {
		return
	}
	sw := eg.SwitchCh() // taken before the hint: the move closes it
	if m = eg.mtr.Load(); m == nil || !m.quietLink {
		return
	}
	capStats.capped.Add(1)
	if !exitHints.send(eg.ID(), m) {
		exitHints.fail.Add(1)
		return
	}
	exitHints.sent.Add(1)
	holdFor(sw, eg.CloseCh())
}

// capEdge is the edge's cap on a write of n bytes of gs (an upload): past it
// the mover moves gs now, and the write waits for the move.
func (gs *groupStream) capEdge(n int) {
	if gs.q == nil {
		return
	}
	gs.mu.Lock()
	onQuiet := gs.bcs == nil && !gs.closed
	gs.mu.Unlock()
	if !onQuiet || !gs.cap.add(n, ctrlNow()) {
		return
	}
	sw := gs.g.SwitchCh()
	capStats.capped.Add(1)
	gs.capAt.Store(ctrlNow())
	gs.q.kickNow()
	holdFor(sw, gs.g.CloseCh())
}

// kickCh is what wakes the mover at once (nil: never).
func (q *quietSet) kickCh() <-chan struct{} { return q.kick }

// kickNow wakes the mover (once; a kick pending is enough).
func (q *quietSet) kickNow() {
	if q == nil || q.kick == nil {
		return
	}
	select {
	case q.kick <- struct{}{}:
	default:
	}
}
