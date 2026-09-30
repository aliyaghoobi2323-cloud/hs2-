package engine

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/xtaci/smux"
)

// Stream mode is the one data path every TLS carrier uses (tls, mtcp,
// l3mtcp). It is what made mtcp fast, applied everywhere:
//
//   - A user's TCP connection ends on the Iran server and its BYTES ride one
//     smux stream to the kharej server, which opens its own TCP connection to
//     the panel. There is never TCP inside TCP, so the user's congestion
//     control sees the real path and nothing multiplies retransmissions.
//   - smux flow control pushes back on the sender instead of queueing, so a
//     download cannot bury everything else under seconds of buffered data.
//   - Each link is one real TLS session; streams of many users share it.
//
// The first byte of every stream says what it carries. The TUN (hs0), when a
// mode has one, is a side channel for non-TCP traffic (ping, UDP to 10.77.x)
// carried as packets on a dedicated stream per link.

const (
	kindTCP  byte = 1 // a user TCP connection to the panel
	kindUDP  byte = 2 // one UDP flow to the panel, length-prefixed datagrams
	kindL3   byte = 3 // TUN packets, tlscarrier frame format
	kindCtrl byte = 4 // per-link health control channel (ping/pong stats)
	kindPool byte = 5 // reverse pool-control: edge tells the exit its desired link count
	kindStats byte = 6 // exit -> edge per-link send-side stats (download pressure)
)

// kindTimeout bounds how long the kharej side waits for a new stream's kind.
const kindTimeout = 10 * time.Second

var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// relay copies both ways between a and b and returns when either direction
// ends, closing both so the other direction unblocks.
func relay(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		bp := copyBufs.Get().(*[]byte)
		io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *bp)
		copyBufs.Put(bp)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

// watchConn closes the smux session over it on the first read or write
// error. smux itself only notices a dead connection at its keepalive timeout,
// which would leave every stream on a broken link hanging for 15 seconds.
type watchConn struct {
	net.Conn
	once  sync.Once
	onErr func()
	// why records the first read/write error, so the log can say why a link
	// went down instead of only that it did.
	why atomic.Pointer[string]
}

func (w *watchConn) fail(op string, err error) {
	w.once.Do(func() {
		s := op + ": " + describeNetErr(err)
		w.why.Store(&s)
		go w.onErr()
	})
}

// reason returns why the connection failed, or "" if it has not.
func (w *watchConn) reason() string {
	if s := w.why.Load(); s != nil {
		return *s
	}
	return ""
}

func (w *watchConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if err != nil {
		w.fail("read", err)
	}
	return n, err
}

func (w *watchConn) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if err != nil {
		w.fail("write", err)
	}
	return n, err
}

// describeNetErr turns the usual socket errors into words an operator can act
// on; anything else is passed through.
func describeNetErr(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, io.EOF):
		return "closed by the other server"
	case errors.Is(err, net.ErrClosed):
		return "closed locally"
	case errors.As(err, &ne) && ne.Timeout():
		return "timed out (path stalled)"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection reset"):
		return "reset by the network or the other server"
	case strings.Contains(s, "broken pipe"):
		return "broken pipe (the other side went away)"
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return "network unreachable"
	}
	return s
}

// newSession starts an smux session over conn that dies with conn. The session
// is length-shaped: smux runs over a shapedConn so the TLS records it produces
// follow an HTTPS-like size distribution instead of smux's own framing. Both
// ends build their session here, so the shaping is symmetric. A nil sampler gets
// a default HTTPS sampler. When meter is non-nil (edge links), a meteredConn
// above the shaper counts real payload/stalls for health-aware routing. The
// returned func reports why the connection failed ("" while it is healthy).
func newSession(conn net.Conn, server bool, sampler *obfs.LengthSampler, meter *linkMeter) (*smux.Session, func() string, error) {
	var sp atomic.Pointer[smux.Session]
	var c net.Conn = newShapedConn(conn, sampler)
	if meter != nil {
		c = &meteredConn{Conn: c, m: meter}
	}
	w := &watchConn{Conn: c}
	w.onErr = func() {
		conn.Close()
		if s := sp.Load(); s != nil {
			s.Close()
		}
	}
	var sess *smux.Session
	var err error
	if server {
		sess, err = smux.Server(w, newSmuxConfig())
	} else {
		sess, err = smux.Client(w, newSmuxConfig())
	}
	if err != nil {
		return nil, nil, err
	}
	sp.Store(sess)
	return sess, w.reason, nil
}

// streamPkt carries L3 frames over one smux stream (see pktConn).
type streamPkt struct {
	st   *smux.Stream
	hdr  [7]byte
	rbuf []byte
}

func newStreamPkt(st *smux.Stream) *streamPkt { return &streamPkt{st: st} }

func (s *streamPkt) WriteRaw(b []byte) error {
	s.st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := s.st.Write(b)
	return err
}

func (s *streamPkt) ReadFrameReuse() (byte, []byte, error) {
	if _, err := io.ReadFull(s.st, s.hdr[:]); err != nil {
		return 0, nil, err
	}
	h := s.hdr
	n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	pad := int(h[4])<<16 | int(h[5])<<8 | int(h[6])
	if n > 1<<16 || pad > 1<<16 {
		return 0, nil, errors.New("engine: oversized L3 frame")
	}
	if cap(s.rbuf) < n+pad {
		s.rbuf = make([]byte, n+pad)
	}
	body := s.rbuf[:n+pad]
	if _, err := io.ReadFull(s.st, body); err != nil {
		return 0, nil, err
	}
	return h[0], body[:n], nil
}

func (s *streamPkt) SetReadDeadline(t time.Time) { s.st.SetReadDeadline(t) }
func (s *streamPkt) Close() error                { return s.st.Close() }

// UDP datagrams on a stream: [len:2][payload].
const maxDatagram = 65535

func writeDatagram(w io.Writer, p []byte) error {
	if len(p) > maxDatagram {
		return nil // cannot be framed; drop like an oversized datagram
	}
	b := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(b, uint16(len(p)))
	copy(b[2:], p)
	_, err := w.Write(b)
	return err
}

func readDatagram(r io.Reader, buf []byte) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(h[:]))
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// udpIdle closes a UDP flow after this long without traffic either way.
const udpIdle = 2 * time.Minute

// relayUDPConn pumps datagrams between a stream and a connected UDP socket
// (kharej side: the socket is connected to the panel).
func relayUDPConn(st io.ReadWriteCloser, c net.Conn) {
	defer st.Close()
	defer c.Close()
	go func() {
		defer st.Close()
		defer c.Close()
		buf := make([]byte, maxDatagram)
		for {
			c.SetReadDeadline(time.Now().Add(udpIdle))
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if writeDatagram(st, buf[:n]) != nil {
				return
			}
		}
	}()
	buf := make([]byte, maxDatagram)
	for {
		p, err := readDatagram(st, buf)
		if err != nil {
			return
		}
		c.Write(p)
	}
}
