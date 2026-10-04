package engine

import (
	"context"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// mtcpLink is one parallel TLS link carrying many user streams via smux. Each
// link is a full real-TLS session (Chrome fingerprint, real cert, in-stream
// auth) — the same DPI-resistant carrier as the single-link path, just N of
// them. smux multiplexes user connections onto the one TLS session so a link
// with many users still looks like a single HTTPS connection on the wire.
type mtcpLink struct {
	tls     *tlscarrier.Carrier
	sess    *smux.Session
	active  atomic.Int32
	dead    atomic.Bool
	sampler *obfs.LengthSampler
	mtr     *linkMeter
	why     func() string // why the underlying connection failed, "" if it has not

	flowMu sync.Mutex
	flows  map[*countedStream]struct{} // open user streams, for activity sampling
}

func (l *mtcpLink) meter() *linkMeter { return l.mtr }

// downReason says why the link is no longer usable, for the log.
func (l *mtcpLink) downReason() string {
	if l.why != nil {
		if r := l.why(); r != "" {
			return r
		}
	}
	if l.dead.Load() {
		return "marked dead"
	}
	return "session ended (keepalive timeout or closed by the other server)"
}

// tcpStats reads the link socket's TCP_INFO (retransmits for loss-based health,
// chrono counters for upload pressure). Linux only; (zero,false) elsewhere.
func (l *mtcpLink) tcpStats() (tcpStat, bool) { return tcpStats(l.tls.TCPConn()) }

func (l *mtcpLink) OpenStream() (stream, error) {
	s, err := l.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	l.active.Add(1)
	cs := &countedStream{Stream: s, link: l, lastActive: time.Now()}
	l.flowMu.Lock()
	if l.flows == nil {
		l.flows = map[*countedStream]struct{}{}
	}
	l.flows[cs] = struct{}{}
	l.flowMu.Unlock()
	return cs, nil
}

// flowSnap summarises a link's user streams for one sampler tick.
type flowSnap struct {
	open    int       // user streams open on the link
	flowing int       // streams whose rate EWMA is >= flowingRate (real traffic)
	recent  int       // streams that moved a byte within the recent window
	last    time.Time // most recent byte on any stream
}

// flowSource is implemented by links that can report per-stream activity (the
// real mtcpLink; test fakes too). A link without it reports open = Active() and
// no flowing streams.
type flowSource interface {
	flowStats(now time.Time, dt, recent time.Duration) flowSnap
}

// flowStats updates each open user stream's rate EWMA (τ = flowTau) from the
// bytes it moved since the last call and reports how many are "flowing". A
// stream counts as flowing at >= flowingRate while it is still moving data
// (within flowRecent): a reconnect handshake (~4 KiB once) peaks well below
// the rate and keepalives never reach it, while any real transfer — even one
// of eight flows sharing a 400 kbit/s throttled link — does. Only the pool's sampler goroutine calls this, so the per-stream
// bookkeeping needs no atomics; the data path only does one atomic add.
func (l *mtcpLink) flowStats(now time.Time, dt, recent time.Duration) flowSnap {
	var fs flowSnap
	alpha := flowAlpha(dt)
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	for cs := range l.flows {
		b := cs.bytes.Load()
		steady := false
		if b != cs.prevBytes {
			if dt > 0 {
				rate := float64(b-cs.prevBytes) / dt.Seconds()
				cs.ewma += float32(alpha * (rate - float64(cs.ewma)))
				steady = rate >= flowSteadyRate
			}
			cs.prevBytes, cs.lastActive = b, now
		} else if dt > 0 {
			cs.ewma -= float32(alpha * float64(cs.ewma))
		}
		cs.steady = (cs.steady<<1 | b2u(steady)) & 7
		fs.open++
		if float64(cs.ewma) >= flowingRate && now.Sub(cs.lastActive) <= flowRecent || cs.steady == 7 {
			fs.flowing++
		}
		if now.Sub(cs.lastActive) <= recent {
			fs.recent++
		}
		if cs.lastActive.After(fs.last) {
			fs.last = cs.lastActive
		}
	}
	return fs
}

// idleCand is a user stream that has been silent for the reclaim window,
// with its byte counter at the time it was chosen.
type idleCand struct {
	cs   *countedStream
	snap uint64
}

// idleStreams returns up to max user streams that have not moved a byte for at
// least idle. The caller closes them outside the lock, and only if their byte
// counter is still unchanged (a stream that woke up in between is spared).
func (l *mtcpLink) idleStreams(now time.Time, idle time.Duration, max int) []idleCand {
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	var out []idleCand
	for cs := range l.flows {
		if len(out) >= max {
			break
		}
		if b := cs.bytes.Load(); b == cs.prevBytes && now.Sub(cs.lastActive) >= idle {
			out = append(out, idleCand{cs: cs, snap: b})
		}
	}
	return out
}

// slowStreams returns up to max user streams that are not flowing (rate EWMA
// below flowingRate and not steady), with their byte counters — what holds a
// link long after its real traffic left: app keepalives and idle sessions
// that still trickle.
func (l *mtcpLink) slowStreams(max int) []idleCand {
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	var out []idleCand
	for cs := range l.flows {
		if len(out) >= max {
			break
		}
		if float64(cs.ewma) < flowingRate && cs.steady != 7 {
			out = append(out, idleCand{cs: cs, snap: cs.bytes.Load()})
		}
	}
	return out
}

// flowAlpha is the EWMA weight for a sample dt apart with time constant flowTau.
func flowAlpha(dt time.Duration) float64 {
	if dt <= 0 {
		return 0
	}
	return 1 - math.Exp(-dt.Seconds()/flowTau.Seconds())
}

// OpenRawStream opens a stream that is not counted as a user.
func (l *mtcpLink) OpenRawStream() (*smux.Stream, error) { return l.sess.OpenStream() }

func (l *mtcpLink) Active() int32 { return l.active.Load() }

// Alive reports usability: the smux session is open and the link was not
// marked dead. (A link that stops receiving is caught by the pool's suspect
// check and the session's keepalive; there is no separate per-link probe.)
func (l *mtcpLink) Alive() bool {
	if l.sess == nil || l.sess.IsClosed() || l.dead.Load() {
		return false
	}
	return true
}
func (l *mtcpLink) Close() error {
	if l.sess != nil {
		l.sess.Close()
	}
	return l.tls.Close()
}

// countedStream decrements the link's active count when a user stream closes,
// so load-based assignment stays accurate.
type countedStream struct {
	*smux.Stream
	link  *mtcpLink
	done  atomic.Bool
	bytes atomic.Uint64 // payload bytes moved either way (data path: atomic add only)

	// sampler-only bookkeeping (see mtcpLink.flowStats)
	prevBytes  uint64
	lastActive time.Time
	ewma       float32 // bytes/s, time constant flowTau
	steady     uint8   // last 3 samples: moved at least flowSteadyRate
}

func (c *countedStream) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	if n > 0 {
		c.bytes.Add(uint64(n))
	}
	return n, err
}

func (c *countedStream) Write(p []byte) (int, error) {
	n, err := c.Stream.Write(p)
	if n > 0 {
		c.bytes.Add(uint64(n))
	}
	return n, err
}

func (c *countedStream) Close() error {
	if c.done.CompareAndSwap(false, true) {
		c.link.active.Add(-1)
		c.link.flowMu.Lock()
		delete(c.link.flows, c)
		c.link.flowMu.Unlock()
	}
	return c.Stream.Close()
}

// mtcpDialer opens mtcp links: dial the TLS carrier, then start a smux client
// session over it.
type mtcpDialer struct {
	addr, sni string
	sharedKey []byte
	bindIP    string // optional local source IP
	sampler   *obfs.LengthSampler
}

// smux tuning, shared by both ends of a link.
var (
	// SmuxFrameSize is the largest data frame. Streams take turns frame by
	// frame, so this bounds how long a small reply waits behind a bulk
	// transfer sharing the link.
	SmuxFrameSize = 16 << 10
	// SmuxStreamBuffer is the per-stream receive window: how far one stream
	// may run ahead of its reader. Flow control keeps a slow reader from
	// piling data into the link.
	SmuxStreamBuffer = 2 << 20
	// SmuxSessionBuffer bounds all streams of one link together.
	SmuxSessionBuffer = 8 << 20
)

func newSmuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	// Randomize the keepalive cadence per session so idle links across the pool
	// (and across servers) do not all emit the same fixed ~5s beat — a
	// cross-session timing fingerprint. The NOP frame itself is already
	// size-disguised by the length shaper (shape.go). smux sends the NOP on
	// every beat, busy or idle (one small frame per 4–8 s per side); the
	// edge's suspect check relies on it reaching an idle link. Timeout stays
	// well above the largest interval so a couple of missed beats never
	// falsely kill a link.
	c.KeepAliveInterval = time.Duration(4000+rand.IntN(4000)) * time.Millisecond // 4–8s
	c.KeepAliveTimeout = 24 * time.Second
	c.MaxFrameSize = SmuxFrameSize
	c.MaxReceiveBuffer = SmuxSessionBuffer
	c.MaxStreamBuffer = SmuxStreamBuffer
	return c
}

func (d *mtcpDialer) DialLink(ctx context.Context) (Link, error) {
	car, err := tlscarrier.DialFrom(d.addr, d.sni, d.sharedKey, d.bindIP)
	if err != nil {
		return nil, err
	}
	return newEdgeLink(car, d.sampler)
}

// newEdgeLink wraps an authenticated TLS carrier as an edge-side mtcp link: a
// smux CLIENT that opens one stream per user connection. It is shared by the
// direct edge (which DIALS the carrier) and the reverse edge (which ACCEPTS it),
// because the edge's smux role is the same either way — only who established the
// TLS connection differs.
func newEdgeLink(car *tlscarrier.Carrier, sampler *obfs.LengthSampler) (*mtcpLink, error) {
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	sess, why, err := newSession(car.RawConn(), false, sampler, mtr)
	if err != nil {
		car.Close()
		return nil, err
	}
	return &mtcpLink{tls: car, sess: sess, sampler: sampler, mtr: mtr, why: why}, nil
}

// closed reports when the link's smux session ends, so a reverse-edge accept
// handler can hold the carrier open for the link's lifetime.
func (l *mtcpLink) closed() <-chan struct{} { return l.sess.CloseChan() }

// NewMTCPDialer builds a link dialer for the manager. bindIP (optional) is the
// local source address links are dialled from.
func NewMTCPDialer(addr, sni string, sharedKey []byte, bindIP string) LinkDialer {
	return &mtcpDialer{addr: addr, sni: sni, sharedKey: sharedKey, bindIP: bindIP, sampler: obfs.NewHTTPSLengthSampler()}
}

var _ net.Conn = (*net.TCPConn)(nil)
