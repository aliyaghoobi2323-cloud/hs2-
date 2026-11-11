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
// the light users need. docs/reality-architecture.md §11.11.
var (
	stripeRebuildOn  = os.Getenv("HS2_MCONN_STRIPE_REBUILD") == "1"
	stripeRebuildFor = func() time.Duration {
		if n, err := strconv.Atoi(os.Getenv("HS2_MCONN_STRIPE_REBUILD_MS")); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
		return 5 * time.Second
	}()
	stripeRebuildUntil atomic.Int64 // unix nanos; 0: none
	stripeRebuildHits  atomic.Int64 // windows opened (diagnostics)
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
}

// note records the pool's live count after a change; it opens the rebuild
// window when the pool fell under half of its recent peak.
func (p *poolPeak) note(now time.Time, n int) {
	if !stripeRebuildOn {
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
		stripeRebuildUntil.Store(now.Add(stripeRebuildFor).UnixNano())
		stripeRebuildHits.Add(1)
		log.Printf("mconn stripe: pool %d -> %d links (under half): uploads use 1 stripe sub for %v (HS2_MCONN_STRIPE_REBUILD)", p.n, n, stripeRebuildFor)
	}
}

// stripeRebuildCap: the width cap now (-1: none).
func stripeRebuildCap(now time.Time) int {
	if !stripeRebuildOn {
		return -1
	}
	if u := stripeRebuildUntil.Load(); u != 0 && now.UnixNano() < u {
		return 1
	}
	return -1
}
