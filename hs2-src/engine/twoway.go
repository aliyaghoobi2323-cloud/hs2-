package engine

import "sync/atomic"

// R1 round 4, the call in the home mix: a two-way steady connection stays
// on its QUIET link (decision T20: a call is delivered at 0.98 "and never
// moves to BULK").
//
// The round-4 rig showed why the call moved anyway (round4/REPORT.fa.md):
// at the start of the home mix every connection opens on QUIET at once; the
// call, bulk to C1, waits behind the latency lane's new flows; its exit
// relay falls behind the panel, reads 32 KiB chunks, and its writer sits in
// big writes: the exit's hint (or QuietCap) reads that as a download's
// backlog, the edge moves the call to BULK, and there, one user of 15 to 17
// on a link, it gets C/N (0.7 to 1.0 Mbit/s of its 1.5) for the rest of the
// run: p99 down 14 to 24 s, delivered 0.68.
//
// What tells a call from a download at either end is the other direction:
// a download's user sends almost nothing (a request), a call's user sends a
// steady stream of its own. TwoWay 1 (HS2_TUNE_TWOWAY=1): a group connection
// whose user sends steadily — at least twoWayTicks of the last twoWayWin
// move ticks with twoWayMin bytes or more, and at most RTMax on average — is
// neither hinted nor capped at the exit (its received bytes are the user's)
// nor moved at the edge (its sent bytes are), on a download hint or on the
// upload rule. A connection younger than twoWayWin ticks whose user sent
// twoWayMin in half its ticks so far or more is pending: its move waits
// until the window says (a call is hinted 300 ms in, before a second of its
// own sending is seen; the exit takes a new group as pending until its
// first tick); one whose user sent less moves as before (a download's
// request is a few hundred bytes, in one tick). And with TuneRT the realtime lane takes
// the frames of a flow whose received direction is steady while what it
// sends stays under RTMax (rtlane.go), so a call held back on QUIET does not
// lose its lane.

// TwoWay: 1 on (HS2_TUNE_TWOWAY), 0 off.
var TwoWay = 0

const (
	twoWayWin   = 10      // move ticks (1 s)
	twoWayTicks = 6       // of them with at least ...
	twoWayMin   = 1 << 10 // ... this many bytes
)

// twoWay states: not a steady stream (decided), too early to say but every
// tick so far but one had one, a steady stream.
const (
	twoWayNo = iota
	twoWayPending
	twoWayYes
)

// twoWayState judges the last ticks of ring (bytes a move tick, pos the
// latest), seen the ticks observed so far (any more than twoWayWin count as
// twoWayWin).
func twoWayState(ring []uint32, pos, seen int) int {
	w := min(seen, twoWayWin, len(ring))
	if w <= 0 {
		return twoWayPending
	}
	n, sum := 0, uint64(0)
	for i := 0; i < w; i++ {
		v := ring[(pos-i+len(ring))%len(ring)]
		sum += uint64(v)
		if v >= twoWayMin {
			n++
		}
	}
	if w < twoWayWin {
		// at least half the ticks so far: a call whose own sending is held
		// back at the start of a busy link still is (round 4's rig: with
		// "all but one tick" the calls of the home mix moved in two runs of
		// three); a download's request is one tick
		if n >= 1 && 2*n >= w {
			return twoWayPending
		}
		return twoWayNo
	}
	if n >= twoWayTicks && float64(sum) <= float64(RTMax)/8*float64(twoWayWin)*moveTick.Seconds() {
		return twoWayYes
	}
	return twoWayNo
}

// twoWay is the edge's state of a move due in direction dir (the bulk one):
// the other direction over the rule's ticks, for "down" what the user sent
// (tx), for "up" what it received (rx).
func (r *moveRule) twoWay(dir string) int {
	if dir == "up" {
		return twoWayState(r.rx[:], r.pos, r.seen)
	}
	return twoWayState(r.tx[:], r.pos, r.seen)
}

// rxRing is the exit's ring of what a group connection received (the
// user's upload) each move tick (the hint loop's).
type rxRing struct {
	ticked bool
	at     int64
	last   uint64
	slots  [twoWayWin]uint32
	pos    int
	seen   int // ticks observed (to twoWayWin)
}

// tick takes the received bytes so far at now (ctrlNow).
func (r *rxRing) tick(rx uint64, now int64) {
	k := now / int64(moveTick)
	if !r.ticked {
		r.ticked, r.at, r.last = true, k, rx
		return
	}
	steps := k - r.at
	if steps <= 0 {
		return
	}
	r.at = k
	for i := int64(0); i < min(steps, twoWayWin); i++ {
		r.pos = (r.pos + 1) % twoWayWin
		r.slots[r.pos] = 0
	}
	r.slots[r.pos] = uint32(min(rx-r.last, 1<<31))
	r.last = rx
	r.seen = min(r.seen+int(steps), twoWayWin)
}

// state is the ring's twoWayState.
func (r *rxRing) state() int { return twoWayState(r.slots[:], r.pos, r.seen) }

// twoWayKept: the moves kept back (the status lines).
var twoWayKept atomic.Int64
