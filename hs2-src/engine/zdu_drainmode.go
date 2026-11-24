package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// ZDU B6 drain mode (proposals/zdu350.md §7 S7): after the commit the old
// process only drains. Switches: the edge advert stops (exactly one edge
// advertises, the new one); the status writer stops (cmd/hs2: it would
// overwrite the new process's status file); the edge's autopilot, heal
// (make-before-break replacement dials) and, in reverse, the reconcile that
// sends the poolctl target stop (LinkManager.Run / runAccept); a Kharej exit
// pool ignores poolctl targets and retires every slot whose link ends or that
// would dial (it no longer holds its slot target, so its links only go).
// LB spares stop (exitPool.addSpares / addSparePast start none); the queue
// controller gives its shaper class slot up without touching the shared root
// (qShaper.zduRelease) and stops; cmd/hs2 writes neither the status nor the
// *.warm file (the new process owns both).
//
// HS2_ZDU off: ZDUEnterDrain does nothing, ZDUDraining is false.
var zduDraining atomic.Bool

// ZDUEnterDrain switches this process into drain mode (old process, at the
// commit). No-op with the key off.
func ZDUEnterDrain() bool {
	if !zduOn.Load() {
		return false
	}
	// Review M1: stamp the L3 clock at the commit, so the drain's first tick
	// waits the grace for TUN packets still in flight on this process's links
	// ("none seen yet" is not "quiet": a TUN-only Iran with users 0 would
	// otherwise exit at once with downloads in flight).
	zduL3RxAt.Store(monoNano(time.Now()))
	zduDraining.Store(true)
	// b437 F5: every later line of this (old) process reads "[draining] ...",
	// so its own lb cut/weight lines during the drain are not taken for a
	// second event next to the new process's (same syslog tag, other pid).
	log.SetFlags(log.Flags() | log.Lmsgprefix)
	log.SetPrefix(zduDrainLogPrefix)
	return true
}

// zduDrainLogPrefix: the log prefix of a draining old process (b437 F5).
const zduDrainLogPrefix = "[draining] "

// ZDUDraining reports whether this process is a draining old process.
func ZDUDraining() bool { return zduDraining.Load() }

// Review issue 6: the drain's end accounts for TUN traffic. A draining old
// Iran still writes to the TUN what the exit queued on its links (the exit
// moves new TUN packets off them only once it marked them older, and packets
// already in flight still arrive), and its user count says nothing about
// that. zduL3RxAt is the last time this draining process took a TUN packet
// from a link (linkToTun, only while draining: one atomic load otherwise);
// ZDUL3Quiet holds the drain end until none came for the grace.
var zduL3RxAt atomic.Int64

// ZDUL3DrainGrace: no TUN packet from a link for this long = L3 drained.
var ZDUL3DrainGrace = 5 * time.Second

func zduNoteL3Rx() {
	if zduDraining.Load() {
		zduL3RxAt.Store(monoNano(time.Now()))
	}
}

// ZDUL3Quiet reports whether no TUN packet came from a link for grace (and
// the time since the last one; 0 = none while draining).
func ZDUL3Quiet(now time.Time, grace time.Duration) (bool, time.Duration) {
	at := zduL3RxAt.Load()
	if at == 0 {
		return true, 0
	}
	d := now.Sub(monoTime(at))
	return d >= grace, d
}
