package engine

import (
	"context"
	"sync"
	"time"
)

// Link handshakes are paced process-wide.
//
// A link is a TCP connection plus a full TLS handshake plus auth. Opening a
// few hundred of them in one instant — the reverse exit learning a target of
// 300, every slot redialing at once after the edge restarts, the direct edge
// rebuilding after a network loss — is a burst of identical ClientHellos from
// one IP to one IP:port that no browser makes, a spike of handshake CPU, and,
// on a policed path, a burst of loss. Every link dial in the process (the
// direct edge's pool, its heal replacements, the reverse exit's slots, dgtun
// carriers) therefore takes a turn from one gate: at most gateInflight
// handshakes at a time, and successive starts spaced by jitterGap (40–160 ms,
// ~10 a second), so 300 links come up in about half a minute, never as one
// burst, and a dead peer costs a handful of attempts rather than hundreds.
const gateInflight = 8

type dialGate struct {
	sem  chan struct{}
	mu   sync.Mutex
	next time.Time            // earliest start of the next dial
	gap  func() time.Duration // spacing between starts
}

func newDialGate(inflight int, gap func() time.Duration) *dialGate {
	return &dialGate{sem: make(chan struct{}, inflight), gap: gap}
}

// linkGate is the process-wide gate (a variable so tests can swap in a faster
// one).
var linkGate = newDialGate(gateInflight, jitterGap)

// acquire waits for this dial's turn. It returns false if ctx ended first;
// otherwise the caller dials and then calls release.
func (g *dialGate) acquire(ctx context.Context) (release func(), ok bool) {
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	g.mu.Lock()
	now := time.Now()
	at := g.next
	if at.Before(now) {
		at = now
	}
	g.next = at.Add(g.gap())
	g.mu.Unlock()
	if d := at.Sub(now); d > 0 && !sleepCtx(ctx, d) {
		<-g.sem
		return nil, false
	}
	return func() { <-g.sem }, true
}

// inflight is how many dials hold a turn now (for tests and the monitor).
func (g *dialGate) inflight() int { return len(g.sem) }
