package engine

import (
	"net"
	"sync/atomic"
	"time"
)

// Per-link health: the edge pool watches each link's real throughput and error
// rate so it can (a) route new users onto healthy links with headroom, and (b)
// spot a link that has gone soft-bad — alive but slow or stalling — and heal it
// by dialing a replacement first, then draining the bad one, without dropping
// the users already on it. All of this is LOCAL to the dialing side, so it needs
// no protocol change and works against an un-upgraded peer.

const (
	// healthTick is how often the pool samples per-link stats and re-evaluates.
	healthTick = 2 * time.Second
	// gpAlpha is the EWMA weight for per-link goodput (higher = more reactive).
	// Goodput is used for logging/diagnostics, not for the degrade decision.
	gpAlpha = 0.4
	// A link is judged only when it moved at least activeBytes in the sample, so
	// idle links (whose low throughput is not the link's fault) are never
	// condemned. This is what keeps a healthy link with idle users safe.
	activeBytes = 96 << 10
	// mss approximates bytes per packet, to turn a byte delta into a packet count
	// for the loss fraction.
	mss = 1400
	// lossFrac: a link retransmitting more than this fraction of its packets while
	// active is treated as degraded (real path loss, not idle users).
	lossFrac = 0.12
	// degradeStreak: consecutive bad samples before a link is declared degraded,
	// so a brief loss burst is not enough.
	degradeStreak = 3
	// maxDrain bounds how long a degraded link is kept for its existing users
	// before it is force-closed (they reconnect onto a healthy link).
	maxDrain = 45 * time.Second

	// Throughput autoscaling (phase 2):
	// probeGain: a speculative link must raise aggregate goodput by at least this
	// fraction within a tick to be judged "it helped" and justify growing more.
	probeGain = 0.08
	// reprobeGain: after settling at a plateau, only probe again once aggregate
	// goodput climbs this much above the plateau (demand genuinely grew).
	reprobeGain = 0.20
	// probeCooldownDur: minimum quiet time after a plateau before re-probing.
	probeCooldownDur = 20 * time.Second
	// scaleDownAfter: how long load must stay at/under the floor before a link is
	// retired (slow shrink, so a brief lull does not thrash the pool).
	scaleDownAfter = 30 * time.Second

	// Control channel (phase 3): the edge opens one control stream per link and
	// exchanges a tiny ping/pong with the exit every controlInterval to learn the
	// download-direction retransmits and the round-trip time.
	controlInterval = 3 * time.Second
)

// linkMeter holds the raw counters for one link. It is deliberately dumb: it
// sums bytes (split by direction) and stalls; the manager turns deltas into an
// EWMA goodput on its own cadence. peerRetrans and rttMicros are filled by the
// per-link control channel (phase 3) with the far side's download-path
// retransmits and the measured round-trip time.
type linkMeter struct {
	rdBytes atomic.Uint64 // payload bytes read (download, from the peer)
	wrBytes atomic.Uint64 // payload bytes written (upload, to the peer)
	stalls  atomic.Uint64 // write errors / timeouts

	peerRetrans atomic.Uint64 // exit-side cumulative TCP retransmits (download loss)
	rttMicros   atomic.Uint64 // last control round-trip time, microseconds
	peerSeen    atomic.Bool   // a control response has been received at least once
}

// meteredConn counts the bytes a link carries, by direction. It sits above the
// length shaper so it measures real payload, not padding.
type meteredConn struct {
	net.Conn
	m *linkMeter
}

func (c *meteredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.m.rdBytes.Add(uint64(n))
	}
	return n, err
}

func (c *meteredConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.m.wrBytes.Add(uint64(n))
	}
	if err != nil {
		c.m.stalls.Add(1)
	}
	return n, err
}

// metered is implemented by a link that carries a linkMeter and can report its
// kernel TCP retransmit counter, so the manager can judge path loss. Links that
// do not implement it never soft-degrade (they still reap on hard death).
type metered interface {
	meter() *linkMeter
	// linkRetrans returns the cumulative TCP retransmits and whether the platform
	// supports the reading (false disables loss-based degradation for the link).
	linkRetrans() (uint64, bool)
}

// linkMeterOf returns the meter of a Link, or nil if it has none.
func linkMeterOf(l Link) *linkMeter {
	if m, ok := l.(metered); ok {
		return m.meter()
	}
	return nil
}

// linkRetransOf returns a Link's cumulative TCP retransmits, or (0,false).
func linkRetransOf(l Link) (uint64, bool) {
	if m, ok := l.(metered); ok {
		return m.linkRetrans()
	}
	return 0, false
}
