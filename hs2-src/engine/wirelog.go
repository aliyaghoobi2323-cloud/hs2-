package engine

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// The refusals of wire v3 a receiver counts (§4.5): a stream whose SYN lacks
// the v3 payload (smux refuses it and counts it), and a TUN frame without the
// v3 descriptor (badTunFrames: linkToTun drops the side channel it came on).
// They can only come from a broken or foreign peer — a v2 one is refused at
// its auth record — so they are logged, but at most once a minute.

// wireLogEvery is how often one refusal's line may be logged.
const wireLogEvery = time.Minute

// badTunFrames counts the TUN frames dropped for their descriptor.
var badTunFrames atomic.Uint64

// WireCounters returns the SYNs refused for their payload and the TUN frames
// dropped for their descriptor, since start.
func WireCounters() (badSyn, badTun uint64) { return smux.BadSyns(), badTunFrames.Load() }

// logWire logs each refusal's line while its count moves, at most once a
// minute.
func logWire(ctx context.Context, logf func(string, ...any)) { logWireEvery(ctx, logf, 5*time.Second) }

// logWireEvery is logWire looking every tick.
func logWireEvery(ctx context.Context, logf func(string, ...any), tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	lastSyn, lastTun := WireCounters()
	var synAt, tunAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			syn, tun := WireCounters()
			if syn != lastSyn && now.Sub(synAt) >= wireLogEvery {
				logf("stream syn without v3 header")
				lastSyn, synAt = syn, now
			}
			if tun != lastTun && now.Sub(tunAt) >= wireLogEvery {
				logf("tun frame with v2 descriptor")
				lastTun, tunAt = tun, now
			}
		}
	}
}
