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
	// flowTau / flowingRate / flowRecent: a user stream is "flowing" (real
	// traffic, counted for sizing) while its rate EWMA with time constant
	// flowTau is at least flowingRate and it moved a byte within flowRecent,
	// or while it moves data steadily (flowSteadyRate).
	// Handshakes and keepalives stay far below the rate; the recency cut ends
	// a finished burst (a page load) at once instead of along the EWMA's tail.
	flowTau     = 10 * time.Second
	flowingRate = 2 << 10 // bytes/s (16 kbit/s)
	flowRecent  = 6 * time.Second
	// flowSteadyRate: a stream that moved at least this much in each of the
	// last 3 samples is flowing whatever its average — many flows sharing a
	// severely throttled link each get very little, but they never pause,
	// unlike a handshake (one sample) or a keepalive (one every half minute).
	flowSteadyRate = 256 // bytes/s
	// blockedMin: a Write shorter than this is CPU work (copying/encrypting a
	// 16 KiB frame takes microseconds), not a wait for the network; only longer
	// waits count as the link being path-limited. Without this filter a writer
	// that always has data reads as ~90% "blocked" even on an unlimited path.
	blockedMin = time.Millisecond
	// maxDrain bounds how long a degraded link is kept for its existing users
	// before it is force-closed (they reconnect onto a healthy link).
	maxDrain = 45 * time.Second

	// warmStartLinks: the pool comes up at this size (clamped to the envelope)
	// rather than at min, so a burst of connections arriving right after start
	// spreads across enough links to beat per-connection throttling immediately —
	// user connections pin to a link for life, so links that appear only later
	// cannot rescue a flow that already landed on a crowded one. The autopilot
	// then shrinks toward min when the tunnel turns out to be idle, or grows
	// toward max under sustained load: sized right at the start, adjusted after.
	warmStartLinks = 8
	// retireAfterDrop: after the reverse edge lowers its target it waits this
	// long before closing an idle link, so the exit has already learned the lower
	// target (pool-control sends within ~1s) and retires that slot rather than
	// redialing it.
	retireAfterDrop = 4 * time.Second

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
	// wrBlocked is the cumulative time the link's single smux writer spent
	// waiting for the socket to accept data (only waits > blockedMin count).
	// Divided by elapsed time it is the fraction of time the PATH, not the
	// application, was the limit: with TCP_NOTSENT_LOWAT set, sendmsg waits once
	// ~32 KiB is queued unsent, so a link whose network is not taking data
	// shows ~100%, and one with spare capacity ~0% however busy it is. Measured
	// on the loopback: unlimited path 0%, a 2 MB/s bottleneck 99.8%.
	wrBlocked atomic.Int64 // nanoseconds

	// Exit-side download stats (edge only; see stats.go). statsPoll carries the
	// sampler's "poll now" (cap 1); statsState is statsPending/OK/Unsupported;
	// peer holds the latest record received from the exit.
	statsPoll  chan struct{}
	statsState atomic.Int32
	statsSeq   atomic.Uint32
	peer       atomic.Pointer[statsRec]

	// peerMax is the exit's link-pool ceiling as it told us over kindInfo
	// (peerinfo.go); 0 = not known (older exit, or not exchanged yet).
	peerMax atomic.Uint32

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
	t0 := time.Now()
	n, err := c.Conn.Write(p)
	if d := time.Since(t0); d > blockedMin {
		c.m.wrBlocked.Add(int64(d))
	}
	if n > 0 {
		c.m.wrBytes.Add(uint64(n))
	}
	if err != nil {
		c.m.stalls.Add(1)
	}
	return n, err
}

// tcpStat is one TCP_INFO snapshot of a link socket (see tcpStats).
type tcpStat struct {
	retrans      uint64 // cumulative retransmitted segments
	busyUs       uint64 // time with data in flight or queued (chrono)
	rwndUs       uint64 // part of busy limited by the peer's receive window
	sndbufUs     uint64 // part of busy limited by our send buffer
	deliveryRate uint64 // kernel delivery-rate estimate, bytes/s
	notsent      uint32 // bytes queued but not yet sent
	chronoValid  bool   // the kernel reports chrono counters (busy > 0)
}

// metered is implemented by a link that carries a linkMeter and can read its
// socket's TCP_INFO. Links that do not implement it never soft-degrade and never
// count as pressed (they still reap on hard death).
type metered interface {
	meter() *linkMeter
	tcpStats() (tcpStat, bool)
}

// linkMeterOf returns the meter of a Link, or nil if it has none.
func linkMeterOf(l Link) *linkMeter {
	if m, ok := l.(metered); ok {
		return m.meter()
	}
	return nil
}

// linkTCPStatsOf returns a Link's TCP_INFO snapshot, or (zero,false).
func linkTCPStatsOf(l Link) (tcpStat, bool) {
	if m, ok := l.(metered); ok {
		return m.tcpStats()
	}
	return tcpStat{}, false
}
