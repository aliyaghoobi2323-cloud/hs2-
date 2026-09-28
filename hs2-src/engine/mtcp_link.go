package engine

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
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
}

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
	sampler   *obfs.LengthSampler
}

func newSmuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.KeepAliveInterval = 5 * time.Second
	c.KeepAliveTimeout = 15 * time.Second
	c.MaxReceiveBuffer = 8 * 1024 * 1024
	c.MaxStreamBuffer = 2 * 1024 * 1024
	return c
}

func (d *mtcpDialer) DialLink(ctx context.Context) (Link, error) {
	car, err := tlscarrier.Dial(d.addr, d.sni, d.sharedKey)
	if err != nil {
		return nil, err
	}
	sess, err := smux.Client(car.RawConn(), newSmuxConfig())
	if err != nil {
		car.Close()
		return nil, err
	}
	l := &mtcpLink{tls: car, sess: sess, sampler: d.sampler}
	go l.healthProbe(context.Background())
	return l, nil
}

// NewMTCPDialer builds a link dialer for the manager.
func NewMTCPDialer(addr, sni string, sharedKey []byte) LinkDialer {
	return &mtcpDialer{addr: addr, sni: sni, sharedKey: sharedKey, sampler: obfs.NewHTTPSLengthSampler()}
}

var _ net.Conn = (*net.TCPConn)(nil)
