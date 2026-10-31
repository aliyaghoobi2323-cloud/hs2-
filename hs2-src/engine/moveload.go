package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// R1 fix 1, the move by the QUIET links' load (the main servers' first days
// on build e8a84ccb17fa).
//
// Measured there: 42 to 73 % of the download went on the 4 QUIET links,
// each 18 to 44 Mbit/s (about 8 by design), 7200 to 9600 connections a
// minute opened on them and 19 to 78 moved; the QUIET sockets held 10 to
// 44 KiB unsent and the big tunnel's ping tail grew 10 to 22 ms. The users'
// VLESS connections are short and of a few hundred KiB, and the move rules
// judge each connection by itself (moveload_test.go has the numbers):
//
//   - the exit's hint wants the connection's writer inside big writes at
//     least half of each of hintRun ticks in a row, after a tick that only
//     starts its rule: 300 to 400 ms of backlog, then the hint's trip, the
//     mover's tick and the move stream's trip; a connection fed by a remote
//     server at its own rate is never backlogged a whole tick, and one that
//     is ends before its move;
//   - the edge's own rules (the upload's, and the download's with no hint
//     stream) want a backlog run (busyRun, rxRun) as well as moveBytes;
//   - TwoWay does not hold them: a download's request is one tick, and its
//     download is 16 times its request within its first 64 KiB
//     (TwoWayRatio).
//
// So a hundred connections a second that are each no backlog fill the link
// together. MoveLoad looks at the link: while a QUIET link is loaded in a
// direction — it carried MoveLoadMbps or more that way over the last second
// (the edge's reading and writing of it), or, for what the edge sends, its
// writer waited on the socket at least half of that second (the path is the
// limit, whatever its rate) — a group connection on it moves at the mover's
// next tick once it carried MoveLoadKiB that way over the last second and
// still does (moveRxMin in the latest tick: C1's light span, 64 KiB, in a
// second), with no backlog run and no hint. A
// call stays: the mover's TwoWay gate holds every move of a connection that
// is two-way or too young to say (twoway.go), these too. While the link is
// not loaded the rules are as before: a QUIET link with room costs a light
// flow nothing, and a move costs a stream and the BULK link's queue.
//
// The exit runs the same rule on what it sends (a download) and says so
// with its hint (hint.go: the wire as before, so an older edge moves on it;
// an older exit's edge moves on its own reading), and the edge on what its
// QUIET link carries each way.
//
// MoveLoad 1 (HS2_TUNE_MOVE_LOAD, the default): on; 0: off (as before).
// Each end's own.
var MoveLoad = newTuneInt(1)

// MoveLoadMbps (HS2_TUNE_MOVE_LOAD_MBPS): a QUIET link that carried this
// much in a direction over the last second is loaded that way (about what
// the light users of a pool carry on it by design).
var MoveLoadMbps = newTuneInt(8)

// MoveLoadKiB (HS2_TUNE_MOVE_LOAD_KIB): what a connection on a loaded QUIET
// link carries that way over a second before it moves (a web page's small
// objects, a game, a probe and a chat stay under it).
var MoveLoadKiB = newTuneInt(64)

// loadUp, loadDown: a QUIET link loaded in the edge's sending, receiving.
const (
	loadUp uint8 = 1 << iota
	loadDown
)

// quietLoad is one QUIET link's load as the mover reads it (the mover's
// goroutine only): its bytes each way and its writer's wait on the socket,
// each move tick over the last second.
type quietLoad struct {
	rd, wr, blk byteRing
}

// tick reads mtr at now (ctrlNow) and says how the link is loaded.
func (l *quietLoad) tick(mtr *linkMeter, now int64) uint8 {
	if mtr == nil {
		return 0
	}
	l.rd.tick(mtr.rdBytes.Load(), now)
	l.wr.tick(mtr.wrBytes.Load(), now)
	l.blk.tick(uint64(max(mtr.wrBlocked.Load(), 0)), now)
	win := float64(twoWayWin) * moveTick.Seconds()
	lim := uint64(float64(max(MoveLoadMbps.Get(), 0)) * 1e6 / 8 * win)
	var f uint8
	if MoveLoadMbps.Get() > 0 && l.rd.sum() >= lim {
		f |= loadDown
	}
	if MoveLoadMbps.Get() > 0 && l.wr.sum() >= lim || l.blk.sum() >= uint64(twoWayWin)*uint64(moveTick)/2 {
		f |= loadUp
	}
	return f
}

// sum is the ring's bytes over the ticks it holds.
func (r *byteRing) sum() uint64 {
	var s uint64
	for _, v := range r.slots {
		s += uint64(v)
	}
	return s
}

// loadTick reads every QUIET link's load (the mover's tick).
func (q *quietSet) loadTick(now int64) map[*mtcpLink]uint8 {
	if MoveLoad.Get() == 0 {
		return nil
	}
	out := map[*mtcpLink]uint8{}
	for _, ql := range q.quietLinks() {
		if f := ql.load.tick(ql.link.mtr, now); f != 0 {
			out[ql.link] = f
		}
	}
	return out
}

// loadDue is the load rule of a connection whose QUIET link is loaded up
// or down (after tick at the same now): it carried MoveLoadKiB that way
// over the last second and still carries.
func (r *moveRule) loadDue(up, down bool) (bool, string) {
	if !r.ticked || r.seen == 0 {
		return false, ""
	}
	w := min(r.seen, twoWayWin)
	switch {
	case down && loadHeavy(r.rx[:], r.pos, w):
		return true, "down"
	case up && loadHeavy(r.tx[:], r.pos, w):
		return true, "up"
	}
	return false, ""
}

// loadHeavy says whether ring (bytes a move tick, pos its latest slot) has
// MoveLoadKiB over its latest w slots and moveRxMin in the latest.
func loadHeavy(ring []uint32, pos, w int) bool {
	if len(ring) == 0 || ring[pos] < moveRxMin {
		return false
	}
	var s uint64
	for i := 0; i < w; i++ {
		s += uint64(ring[((pos-i)%len(ring)+len(ring))%len(ring)])
	}
	return s >= uint64(max(MoveLoadKiB.Get(), 1))<<10
}

// ---- the exit's end: the same rule for its hint (hint.go) ----

// exitLoads is the exit's QUIET links' load, by meter (the hint loop's; a
// mutex for the tests' loops of rigs that end).
var exitLoads struct {
	mu sync.Mutex
	m  map[*linkMeter]*exitLoad
}

type exitLoad struct {
	l    quietLoad
	seen int64 // the latest tick it was read at (ctrlNow)
	f    uint8
}

// exitLoaded says whether the exit's sending on the QUIET link of mtr (a
// download) is loaded at now, reading it once a tick.
func exitLoaded(mtr *linkMeter, now int64) bool {
	if mtr == nil || !mtr.quietLink {
		return false
	}
	exitLoads.mu.Lock()
	defer exitLoads.mu.Unlock()
	if exitLoads.m == nil {
		exitLoads.m = map[*linkMeter]*exitLoad{}
	}
	e := exitLoads.m[mtr]
	if e == nil {
		e = &exitLoad{}
		exitLoads.m[mtr] = e
	}
	if e.seen/int64(moveTick) != now/int64(moveTick) || e.seen == 0 {
		e.f = e.l.tick(mtr, now)
		e.seen = now
	}
	for m, x := range exitLoads.m {
		if now-x.seen > int64(10*time.Second) {
			delete(exitLoads.m, m) // a link that ended
		}
	}
	// the exit's sending is the link's "up" as quietLoad reads its meter
	return e.f&loadUp != 0
}

// exitLoadDue: the exit's load rule for a group connection on a QUIET link
// (tx: what it sent each tick, the hint rule's ring).
func exitLoadDue(mtr *linkMeter, tx *byteRing, now int64) bool {
	if MoveLoad.Get() == 0 || tx.seen == 0 || !exitLoaded(mtr, now) {
		return false
	}
	return loadHeavy(tx.slots[:], tx.pos, min(tx.seen, twoWayWin))
}

// loadMoved: the moves the load rule made (the placement line).
var loadMoved atomic.Int64
