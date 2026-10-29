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
// What tells a call from a download or an upload is that its user sends a
// steady stream at a call's rate while something comes the other way: a
// download's user sends a request, an upload's gets nothing back. Each end
// reads the user's own sending where it is not held back by the tunnel —
// the edge what it took from the user (its sent bytes), the exit what
// arrived from the edge (its received bytes) — and the other way as a
// sign of life only (a call held back on a busy link gets its frames in
// bursts).
//
// TwoWay 1 (HS2_TUNE_TWOWAY=1): a group connection whose user sent
// twoWayMin or more in at least twoWayTicks of the last twoWayWin move
// ticks, at most RTMax on average, with the other way active in at least
// twoWayOther of them, is two-way: the exit neither hints nor caps it, the
// edge moves it neither on a hint nor on the upload rule. A connection
// younger than twoWayWin ticks whose user sent twoWayMin in half its ticks
// so far or more, under RTMax, is pending: its move waits until the window
// says (a call is hinted 300 ms in); the exit takes a new group as pending
// until its first tick. A download's request is one tick of a few hundred
// bytes; an upload's sending is past RTMax, or has nothing coming back. And
// with TuneRT the realtime lane takes the frames of a flow whose received
// direction is steady while what it sends stays under RTMax (rtlane.go), so
// a call held back on QUIET does not lose its lane.

// TwoWay: 1 on (HS2_TUNE_TWOWAY), 0 off.
var TwoWay = newTuneInt(1) // the default since R1 round 4 (round4/REPORT.fa.md)

// TwoWayRatio (HS2_TUNE_TWOWAY_RATIO; 0: none): over the window neither way
// of a two-way connection carries more than this many times the other. A
// call sends about what it gets (a gallery view, a webinar with the
// camera on: a few times as much one way); a download whose app
// acknowledges in its own data — QUIC or any UDP inside a proxy
// connection (VLESS/xray), a VPN over TCP inside one — sends 1 to 3 % of
// what it gets, in every tick, under RTMax: it read as a call, so neither
// the exit hinted it nor did the edge move it, and it stayed on its QUIET
// link (cubic) at whatever rate, the light flows of every user behind it
// (the R1 red team's second pass, R2-1). Each end judges its own bytes;
// nothing on the wire.
var TwoWayRatio = newTuneInt(16)

// twoWayLopsided says whether one way carried more than TwoWayRatio times
// the other over the window (a and b: the two ways' sums).
func twoWayLopsided(a, b uint64) bool {
	r := uint64(max(TwoWayRatio.Get(), 0))
	if r == 0 {
		return false
	}
	return a > r*b || b > r*a
}

const (
	twoWayWin   = 10      // move ticks (1 s)
	twoWayTicks = 6       // of them the user sent at least ...
	twoWayMin   = 1 << 10 // ... this many bytes
	twoWayOther = 2       // and the other way at least as much in this many
)

// twoWay states: not a call (decided), too early to say but the user's
// sending looks so far like one, a call.
const (
	twoWayNo = iota
	twoWayPending
	twoWayYes
)

// twoWayState judges the last ticks of user (the user's sending, bytes a
// move tick) and other (the other way), pos the latest slot of each (rings
// of their own lengths, pos counted in ticks), seen the ticks observed so
// far (any more than twoWayWin count as twoWayWin).
func twoWayState(user, other []uint32, pos, seen int) int {
	w := min(seen, twoWayWin, len(user), len(other))
	if w <= 0 {
		return twoWayPending
	}
	n, no, sum, osum := 0, 0, uint64(0), uint64(0)
	for i := 0; i < w; i++ {
		v := user[((pos-i)%len(user)+len(user))%len(user)]
		sum += uint64(v)
		if v >= twoWayMin {
			n++
		}
		o := other[((pos-i)%len(other)+len(other))%len(other)]
		osum += uint64(o)
		if o >= twoWayMin {
			no++
		}
	}
	if float64(sum) > float64(RTMax)/8*float64(w)*moveTick.Seconds() {
		return twoWayNo // past a call's rate: an upload
	}
	if n >= 1 && twoWayLopsided(sum, osum) {
		return twoWayNo // a download (or an upload) acknowledged in its own data: no call
	}
	if w < twoWayWin {
		// at least half the ticks so far: a call whose sending is held back
		// at the start of a busy link still is; a download's request is one
		if n >= 1 && 2*n >= w {
			return twoWayPending
		}
		return twoWayNo
	}
	if n >= twoWayTicks && no >= twoWayOther {
		return twoWayYes
	}
	return twoWayNo
}

// twoWay is the edge's state of a group connection: the user's sending is
// what the edge took from it (tx), the other way what came back (rx).
func (r *moveRule) twoWay() int { return twoWayState(r.tx[:], r.rx[:], r.pos, r.seen) }

// byteRing is a ring of the bytes a counter moved each move tick (the exit
// hint loop's: what a group connection received, and sent).
type byteRing struct {
	ticked bool
	at     int64
	last   uint64
	slots  [twoWayWin]uint32
	pos    int
	seen   int // ticks observed (to twoWayWin)
}

// tick takes the counter at now (ctrlNow).
func (r *byteRing) tick(v uint64, now int64) {
	k := now / int64(moveTick)
	if !r.ticked {
		r.ticked, r.at, r.last = true, k, v
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
	r.slots[r.pos] = uint32(min(v-r.last, 1<<31))
	r.last = v
	r.seen = min(r.seen+int(steps), twoWayWin)
}

// twoWayExit is the exit's state of a group connection from its rings: the
// user's sending is what arrived (rx), the other way what the exit sent
// (tx). The rings tick together.
func twoWayExit(rx, tx *byteRing) int {
	return twoWayState(rx.slots[:], tx.slots[:], rx.pos, rx.seen)
}

// twoWayKept: the moves kept back (the status lines).
var twoWayKept atomic.Int64
