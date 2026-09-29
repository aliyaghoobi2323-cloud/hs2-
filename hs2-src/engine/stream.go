package engine

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
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
	kindTCP byte = 1 // a user TCP connection to the panel
	kindUDP byte = 2 // one UDP flow to the panel, length-prefixed datagrams
	kindL3  byte = 3 // TUN packets, tlscarrier frame format
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
}

func (w *watchConn) fail() { w.once.Do(func() { go w.onErr() }) }

func (w *watchConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if err != nil {
		w.fail()
	}
	return n, err
}

func (w *watchConn) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if err != nil {
		w.fail()
	}
	return n, err
}

// newSession starts an smux session over conn that dies with conn. The session
// is length-shaped: smux runs over a shapedConn so the TLS records it produces
// follow an HTTPS-like size distribution instead of smux's own framing. Both
// ends build their session here, so the shaping is symmetric. A nil sampler gets
// a default HTTPS sampler.
func newSession(conn net.Conn, server bool, sampler *obfs.LengthSampler) (*smux.Session, error) {
	var sp atomic.Pointer[smux.Session]
	w := &watchConn{Conn: newShapedConn(conn, sampler)}
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
		return nil, err
	}
	sp.Store(sess)
	return sess, nil
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
