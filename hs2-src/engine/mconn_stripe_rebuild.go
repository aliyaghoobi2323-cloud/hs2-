package engine

import (
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_REBUILD=1 (rel52, off by default): after a mass link loss
// on the edge (the reverse pool falls below half of its peak of the last
// stripeRebuildPeakAge), a striped upload uses at most one stripe sub beside
// its primary for HS2_MCONN_STRIPE_REBUILD_MS (default 5000) while the pool
// rebuilds, so the heavy upload does not spread over the few links left that
// the light users need. The window is per pool (LinkManager): on a server
// with several tunnels only the connections of the collapsed pool narrow.
// docs/reality-architecture.md §11.11.
var (
	// stripeRebuildOn: HS2_MCONN_STRIPE_REBUILD (read in loadEnvKeys,
	// defaults22.go)
	stripeRebuildOn  atomic.Bool
	stripeRebuildFor = func() time.Duration {
		if n, err := strconv.Atoi(os.Getenv("HS2_MCONN_STRIPE_REBUILD_MS")); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
		return 5 * time.Second
	}()
	stripeRebuildHits atomic.Int64 // windows opened, all pools (diagnostics)
)

const (
	stripeRebuildPeakAge = 10 * time.Second
	stripeRebuildMinPeak = 8
)

// poolPeak tracks the reverse pool's recent peak (Iran).
type poolPeak struct {
	mu   sync.Mutex
	n    int
	at   time.Time
	open time.Time // the last window opened
	// until: the window's end (unix nanos; 0: none); read lock-free by
	// the pool's striped senders
	until atomic.Int64
}

// note records the pool's live count after a change; it opens the rebuild
// window when the pool fell under half of its recent peak.
func (p *poolPeak) note(now time.Time, n int) {
	if !stripeRebuildOn.Load() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= p.n || now.Sub(p.at) > stripeRebuildPeakAge {
		p.n, p.at = n, now
		return
	}
	if p.n >= stripeRebuildMinPeak && 2*n < p.n && now.Sub(p.open) > stripeRebuildFor {
		p.open = now
		p.until.Store(now.Add(stripeRebuildFor).UnixNano())
		stripeRebuildHits.Add(1)
		log.Printf("mconn stripe: pool %d -> %d links (under half): uploads use 1 stripe sub for %v (HS2_MCONN_STRIPE_REBUILD)", p.n, n, stripeRebuildFor)
	}
}

// rebuildCap: the width cap of this pool's striped uploads now (-1: none;
// nil p: a connection outside a reverse pool).
func (p *poolPeak) rebuildCap(now time.Time) int {
	if p == nil || !stripeRebuildOn.Load() {
		return -1
	}
	if u := p.until.Load(); u != 0 && now.UnixNano() < u {
		return 1
	}
	return -1
}
