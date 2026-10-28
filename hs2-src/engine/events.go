package engine

import (
	"fmt"
	"sync/atomic"
)

// R1's event tallies (the round-3 judge's open item 9; REDESIGN §7): what
// the links' status lines left out, or showed only for the first
// coreLinesMax live links and lost with each link that ended. Their totals
// since the process started make one status line (CoreLines), and the
// cores' runner logs one line a minute with what moved in it, none when
// nothing did: never a line an event.
//
//   - UDP datagrams dropped for their age (UDPLateMs, stream_iran.go);
//   - OWD guard beats that found a link socket dirty (owd.go): a resend, or
//     a backlog of its own, so its direction read no queue for a while;
//   - BULK links' in-flight budget cuts (budget.go);
//   - QUIET links' writes, and those that rode a record already started
//     (shape.go: a record each saved against one a frame);
//   - connections moved to a BULK link and back (quiet.go, moveback.go),
//     TUN flows moved (l3_link.go), and the moves the exit attached;
//   - move hints the exit sent, and the edge heard (hint.go).
type r1Events struct {
	// the runner's tallies of the links' own counters (coreLink.noteEvents)
	owdDirty    atomic.Int64
	budgetCuts  atomic.Uint64
	quietWrites atomic.Uint64
	quietJoined atomic.Uint64
	// last: the totals at the minute line before (the runner's)
	last eventsSnap
}

var r1ev r1Events

// placeSet is the process's QUIET set, the edge pool's (nil: none).
var placeSet atomic.Pointer[quietSet]

// tunMoved counts the TUN flows the edge moved to a BULK link.
var tunMoved atomic.Int64

// eventsEvery: the runner's ticks between two minute lines.
const eventsEvery = 600

// eventsSnap is the tallies at a moment.
type eventsSnap struct {
	udpLate, owdDirty                                     int64
	budgetCuts, writes, joined                            uint64
	moved, movedBack, tun, exitMoved, hintsHeard, hintsTx int64
}

// now is the tallies now: e's of the links, and the process's own.
func (e *r1Events) now() eventsSnap {
	s := eventsSnap{udpLate: udpLateDrops.Load(), owdDirty: e.owdDirty.Load(), budgetCuts: e.budgetCuts.Load(),
		writes: e.quietWrites.Load(), joined: e.quietJoined.Load(), tun: tunMoved.Load(),
		exitMoved: exitGroups.attached.Load(), hintsTx: exitHints.sent.Load()}
	if q := placeSet.Load(); q != nil {
		s.moved, s.movedBack, s.hintsHeard = q.moved.Load(), q.movedBack.Load(), q.hinted.Load()
	}
	return s
}

// since is s less the tallies at o (each counter only grows).
func (s eventsSnap) since(o eventsSnap) eventsSnap {
	return eventsSnap{udpLate: s.udpLate - o.udpLate, owdDirty: s.owdDirty - o.owdDirty, budgetCuts: s.budgetCuts - o.budgetCuts,
		writes: s.writes - o.writes, joined: s.joined - o.joined, moved: s.moved - o.moved, movedBack: s.movedBack - o.movedBack,
		tun: s.tun - o.tun, exitMoved: s.exitMoved - o.exitMoved, hintsHeard: s.hintsHeard - o.hintsHeard, hintsTx: s.hintsTx - o.hintsTx}
}

// zero says whether nothing moved.
func (s eventsSnap) zero() bool { return s == eventsSnap{} }

// line is the status line of the tallies since the process started.
func (e *r1Events) line() string {
	s := e.now()
	return fmt.Sprintf("events since start: udp late drops %d; owd dirty %d; budget cuts %d; quiet writes %d, %d of them on a record already started (records saved); moved to bulk %d (back to quiet %d), tun flows %d, attached at the exit %d; move hints heard %d, sent %d",
		s.udpLate, s.owdDirty, s.budgetCuts, s.writes, s.joined, s.moved, s.movedBack, s.tun, s.exitMoved, s.hintsHeard, s.hintsTx)
}

// logMinute logs (logf) what moved since the line before, if anything did
// (the runner's, each eventsEvery ticks, to coreLogf).
func (e *r1Events) logMinute(logf func(string, ...any)) {
	s := e.now()
	d := s.since(e.last)
	e.last = s
	if d.zero() {
		return
	}
	logf("events in the last minute: udp late drops %d, owd dirty %d, budget cuts %d, quiet records saved %d of %d writes, moved to bulk %d (back %d), tun flows %d, attached at the exit %d, move hints heard %d sent %d",
		d.udpLate, d.owdDirty, d.budgetCuts, d.joined, d.writes, d.moved, d.movedBack, d.tun, d.exitMoved, d.hintsHeard, d.hintsTx)
}

// noteEvents adds what the link's own counters gained since the runner's
// wake before to e, the process's tallies (the runner's, after the step).
func (l *coreLink) noteEvents(e *r1Events) {
	if m := l.mtr; m != nil {
		// (the guard's beats are the link meter's one-way delays', a QUIET
		// link's and a BULK one's alike)
		if d := m.owd.dirties.Load(); d > l.seenDirty {
			e.owdDirty.Add(d - l.seenDirty)
			l.seenDirty = d
		}
	}
	if sc := l.sc; sc != nil {
		if b := sc.budget; b != nil {
			sc.mu.Lock()
			c := b.core.cuts
			sc.mu.Unlock()
			if c > l.seenCuts {
				e.budgetCuts.Add(c - l.seenCuts)
				l.seenCuts = c
			}
		}
	}
	if c := l.shp; c != nil && c.count {
		if w := c.writes.Load(); w > l.seenWrites {
			e.quietWrites.Add(w - l.seenWrites)
			l.seenWrites = w
		}
		if j := c.joined.Load(); j > l.seenJoined {
			e.quietJoined.Add(j - l.seenJoined)
			l.seenJoined = j
		}
	}
}
