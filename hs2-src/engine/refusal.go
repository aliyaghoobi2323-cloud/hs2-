package engine

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// A peer that refuses this server's protocol (wire v3, §4.5) — a v2 hs2, no
// protocol level in common, or, what a v2 server looks like from here, its
// cover website (tlscarrier.PeerRefused) — would refuse every dial the same
// way: the side that dials (the direct edge's pool, the reverse exit's slots)
// does not redial it at the pool's pace. Its dials wait refuseBackoffMin,
// doubling to refuseBackoffMax, with jitter; an authenticated link resets it.
// The refusal is logged at most once a minute.
const (
	refuseBackoffMin = 8 * time.Second
	refuseBackoffMax = 60 * time.Second
	refuseLogEvery   = time.Minute
)

// refusalBackoff is that wait, for one pool.
type refusalBackoff struct {
	mu    sync.Mutex
	next  time.Duration // the next wait before jitter (0: no refusal since the last link)
	until time.Time     // no dial before this
	logAt time.Time     // the last refusal logged
	// jitter spreads a wait (nil: ±10%, jitterTenth; tests: none)
	jitter func(time.Duration) time.Duration
}

// refused notes a refusal at now and returns how long the pool's dials wait.
// A refusal while they already wait (a dial that was under way) changes
// nothing. logIt is true for at most one refusal a minute.
func (b *refusalBackoff) refused(now time.Time) (wait time.Duration, logIt bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.until) {
		return b.until.Sub(now), false
	}
	if b.next == 0 {
		b.next = refuseBackoffMin
	}
	wait = jitterTenth(b.next)
	if b.jitter != nil {
		wait = b.jitter(b.next)
	}
	b.until = now.Add(wait)
	b.next = min(2*b.next, refuseBackoffMax)
	if b.logAt.IsZero() || now.Sub(b.logAt) >= refuseLogEvery {
		b.logAt, logIt = now, true
	}
	return wait, logIt
}

// holding is how long dials must still wait at now (0: none).
func (b *refusalBackoff) holding(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.until) {
		return b.until.Sub(now)
	}
	return 0
}

// ok notes an authenticated link: the next refusal waits refuseBackoffMin
// again (its line still waits for its minute).
func (b *refusalBackoff) ok() {
	b.mu.Lock()
	b.next, b.until = 0, time.Time{}
	b.mu.Unlock()
}

// jitterTenth returns a duration within ±10% of d.
func jitterTenth(d time.Duration) time.Duration {
	if d < 10 {
		return d
	}
	return d - d/10 + time.Duration(rand.Int64N(int64(d/5)+1))
}

// refusalLine is the dialing side's words for a refusal.
func refusalLine(err error, wait time.Duration) string {
	return fmt.Sprintf("mtcp: %v — dialing it again in %s (the wait doubles up to %s while it refuses)",
		err, fmtDur(wait), fmtDur(refuseBackoffMax))
}
