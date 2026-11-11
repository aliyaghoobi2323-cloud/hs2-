package engine

import (
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// Start dial (HS2_START_DIAL=1, exit side; docs/reality-architecture.md 8.13).
//
// After a tunnel start the edge's refill hold (refill.go) queues every new
// user connection until a link with room under the fair-share cap is up. The
// exit opens its links through the process's dial gate (dialgate.go) at about
// 20 a second (HS2_FAST_LINK; 10 without), and once the crowd's connections
// arrive the handshakes slow down with the load: in the lab 43 of 100 links
// were up when the crowd started 2.2 s after the first link, the last ones
// came 8 s later, and 2,700 of 3,000 connections waited up to 2.3 s — the
// crowd's p99 of the first 5 s was ~1.3 s. Letting waiters leave the hold
// early (HS2_REFILL_WAIT_MS) fixes the start but puts the leavers on the few
// links up then, up to twice the fair share, where they stay for the rest of
// the run (8.10: ping p99 +14 ms over the whole run).
//
// With the key on, the exit's first pool after a start goes up faster: for
// startDialFor after the pool starts — and again when a link is back after an
// outage with none up (the edge restarted), at most once per startDialRearm —
// its slots take their turns from a start gate of their own (startInflight
// handshakes at a time, starts startGapMin..startGapMax apart, ~45 a second,
// still jittered: never one burst of identical hellos), at most the pool's
// ceiling of turns. Everything else is unchanged: the TLS fingerprint, the
// auth, the wire, the refill hold, the placement. A redial in steady state
// (a link that ended, a target raised later) uses the ordinary gate. So the
// links the hold waits for are there before most users are, the hold is
// short, and nobody is placed past the fair share.
var startDial bool // loadEnvKeys: HS2_START_DIAL

var (
	startDialFor   = 4 * time.Second
	startDialRearm = time.Minute
	startInflight  = 32
	startGapMin    = 10 * time.Millisecond
	startGapMax    = 35 * time.Millisecond
)

// startGate is the start window's gate (a variable so tests can swap it).
var startGate = newDialGate(startInflight, startGap)

func startGap() time.Duration {
	return startGapMin + time.Duration(rand.Int64N(int64(startGapMax-startGapMin)))
}

// startDialTurns counts dial turns taken from the start gate (the monitor and
// the lab: they must stop when the window ends).
var startDialTurns atomic.Int64

// armStartLocked opens the start window at now unless one was opened within
// startDialRearm — except the first link of a pool that never had one (the
// exit started before its edge: the window opened at the pool's start was
// spent on refused dials). Caller holds p.mu.
func (p *exitPool) armStartLocked(now time.Time, first bool) {
	if !startDial {
		return
	}
	if !first && !p.startArmed.IsZero() && now.Sub(p.startArmed) < startDialRearm {
		return
	}
	_, hi := p.bounds()
	p.startArmed, p.startUntil, p.startLeft = now, now.Add(startDialFor), hi
	p.startArms++
}

// gateFor is the gate slot s dials through: the start gate while the window
// is open and has turns left, else the pool's own gate.
func (p *exitPool) gateFor() *dialGate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if startDial && p.startLeft > 0 && time.Now().Before(p.startUntil) {
		p.startLeft--
		startDialTurns.Add(1)
		return startGate
	}
	return p.gate
}
