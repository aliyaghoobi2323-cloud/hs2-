package engine

import (
	"context"
	"math/rand/v2"
	"net"
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
}

func (l *mtcpLink) meter() *linkMeter { return l.mtr }

// linkRetrans reports the link's kernel TCP retransmit counter, for loss-based
// health. It reads TCP_INFO off the carrier's underlying socket (Linux).
func (l *mtcpLink) linkRetrans() (uint64, bool) { return retransmits(l.tls.TCPConn()) }

// healthProbe actively verifies the link. Every few seconds it opens a throwaway
// smux stream and immediately closes it; if that fails, the underlying TLS/TCP
// is gone and the link is marked dead so the manager rebuilds it. This catches
// the idle-session case where smux's own keepalive does not close the session.
func (l *mtcpLink) healthProbe(ctx context.Context) {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if l.sess == nil || l.sess.IsClosed() {
				l.dead.Store(true)
				return
			}
		}
	}
}

func (l *mtcpLink) OpenStream() (stream, error) {
	s, err := l.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	l.active.Add(1)
	return &countedStream{Stream: s, link: l}, nil
}

// OpenRawStream opens a stream that is not counted as a user.
func (l *mtcpLink) OpenRawStream() (*smux.Stream, error) { return l.sess.OpenStream() }

func (l *mtcpLink) Active() int32 { return l.active.Load() }

// Alive reports usability. We treat a link as dead if smux closed it OR if it
// has gone stale: an idle smux session with no streams may not trip its own
// keepalive-timeout close (it guards on a non-empty bucket), so we back it with
// an explicit probe. markDead is set by the health probe below.
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
	link *mtcpLink
	done atomic.Bool
}

func (c *countedStream) Close() error {
	if c.done.CompareAndSwap(false, true) {
		c.link.active.Add(-1)
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
	// size-disguised by the length shaper (shape.go). Keepalive only fires when a
	// link is otherwise idle, so links carrying user traffic are unaffected and
	// throughput is untouched. Timeout stays well above the largest interval so a
	// couple of missed beats never falsely kill a link.
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
	mtr := &linkMeter{}
	sess, err := newSession(car.RawConn(), false, sampler, mtr)
	if err != nil {
		car.Close()
		return nil, err
	}
	l := &mtcpLink{tls: car, sess: sess, sampler: sampler, mtr: mtr}
	go l.healthProbe(context.Background())
	return l, nil
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
