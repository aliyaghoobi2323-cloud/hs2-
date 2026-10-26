package engine

import (
	"context"
	"time"

	"github.com/xtaci/smux"
)

// The refusals of wire v3 a receiver counts (§4.5): a stream whose SYN lacks
// the v3 payload (smux refuses it and counts it). They can only come from a
// broken or foreign peer — a v2 one is refused at its auth record — so they
// are logged, but at most once a minute.

// wireLogEvery is how often one refusal's line may be logged.
const wireLogEvery = time.Minute

// WireCounters returns the SYNs refused for their payload since start.
func WireCounters() (badSyn uint64) { return smux.BadSyns() }

// logWire logs each refusal's line while its count moves, at most once a
// minute.
func logWire(ctx context.Context, logf func(string, ...any)) { logWireEvery(ctx, logf, 5*time.Second) }

// logWireEvery is logWire looking every tick.
func logWireEvery(ctx context.Context, logf func(string, ...any), tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	lastSyn, synAt := WireCounters(), time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n := WireCounters(); n != lastSyn && now.Sub(synAt) >= wireLogEvery {
				logf("stream syn without v3 header")
				lastSyn, synAt = n, now
			}
		}
	}
}
