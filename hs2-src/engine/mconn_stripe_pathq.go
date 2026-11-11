package engine

import (
	"net"
	"slices"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_UP_PATHQ=1 (Iran, with HS2_MCONN_STRIPE_UP): the upload
// gate reads the light users' queue PATH BY PATH (up57).
//
// The gate (stripeQ.pickLocked) took one median over every active light
// link of the process. With the load balancer's several paths (each its own
// bottleneck) an upload stripe whose subs sit on one path fills that path's
// queue only: a third of the light links (3 paths) shows the queue, the
// median does not move, and the gate keeps the stripe wide. The up56 lab
// (HS2_MCONN_STRIPE_FAST, 3 paths of 1 Gbit/s, crowd 3000) measured exactly
// that: crowd p99 +15 ms under one 889 Mbit/s upload, the light links of
// the stripe's path queued while the overall median stayed under the target.
// With the key the light queue is also taken per path (Iran's local IP of
// the link), and the gate reads the worst path's (the same percentile,
// lightQ) among paths with at least stripePathQMin active light links; the
// overall rule stays as it was (the larger wins), so the gate only ever
// closes earlier.
var stripeUpPathQOn atomic.Bool

// stripePathQMin: active light links a path needs before its own median
// counts (fewer: one stale srtt would hold the gate shut).
const stripePathQMin = 4

// linkPathOf: the local IP of an edge link's socket ("" when unknown).
func linkPathOf(l *mtcpLink) string {
	if l == nil || l.tls == nil {
		return ""
	}
	c := l.tls.TCPConn()
	if c == nil {
		return ""
	}
	if a, ok := c.LocalAddr().(*net.TCPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// pathLightQLocked (under p.mu, Iran's upload gate): the worst path's light
// queue (0: key off, or no path with enough active light links).
func (p *stripeQ) pathLightQLocked(own []*linkMeter) time.Duration {
	if !stripeUpPathQOn.Load() {
		return 0
	}
	by := map[string][]time.Duration{}
	for mtr, l := range p.links {
		if l.path != "" && l.active && !l.busy && !slices.Contains(own, mtr) {
			by[l.path] = append(by[l.path], l.q)
		}
	}
	var q time.Duration
	for _, lt := range by {
		if len(lt) < stripePathQMin {
			continue
		}
		slices.Sort(lt)
		q = max(q, lightQ(lt))
	}
	stripePathQStat.last.Store(int64(q))
	return q
}

var stripePathQStat struct {
	last atomic.Int64 // the last per-path light queue (ns)
}
