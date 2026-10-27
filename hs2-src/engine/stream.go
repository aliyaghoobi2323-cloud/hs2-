package engine

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
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
// carried as packets on a dedicated stream per link. Every stream's SYN
// carries the v3 payload (smux.EncodeSyn): a user stream its user's tag
// (UserTag), a control stream tag 0.

const (
	// The untagged user kinds of wire v2, refused since v3 (§4.2): the
	// numbers stay taken.
	kindTCP   byte = 1
	kindUDP   byte = 2
	kindL3    byte = 3 // TUN packets, tlscarrier TUN frames (AppendTunFrame)
	kindCtrl  byte = 4 // per-link health control channel (ping/pong stats)
	kindPool  byte = 5 // reverse pool-control: edge tells the exit its desired link count
	kindStats byte = 6 // exit -> edge per-link send-side stats (download pressure)
	kindInfo  byte = 7 // one exchange per link: each side's link-pool ceiling (display only)

	// A user connection, tagged with the Iran user port it came in on (0:
	// none), so the exit routes it by its own table (routes.go): [kind][port
	// u16], the only user header of wire v3 (userStreamHeader). A TCP
	// connection, or one UDP flow of length-prefixed datagrams.
	kindTCPPort byte = 8
	kindUDPPort byte = 9
)

// kindTimeout bounds how long the kharej side waits for a new stream's kind.
const kindTimeout = 10 * time.Second

var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// relay copies both ways between a and b and returns when either direction
// ends, closing both so the other direction unblocks. (A user connection over
// a smux stream uses relayStream, wedge.go, which also guards the link.)
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
	// rdCalls / inRead let the wedge guard (wedge.go) see, without a syscall,
	// that smux's reader has stopped asking for data (its bucket is empty).
	rdCalls atomic.Uint64
	inRead  atomic.Bool
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
	w.rdCalls.Add(1)
	w.inRead.Store(true)
	n, err := w.Conn.Read(p)
	w.inRead.Store(false)
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

// writeHold, flushHeld: the shaper's under it (holdWriter; the link
// scheduler's writer), watched as Write.
func (w *watchConn) writeHold(p []byte, hold bool) (int, error) {
	h, ok := w.Conn.(holdWriter)
	if !ok {
		return w.Write(p)
	}
	n, err := h.writeHold(p, hold)
	if err != nil {
		w.fail("write", err)
	}
	return n, err
}

func (w *watchConn) flushHeld() error {
	h, ok := w.Conn.(holdWriter)
	if !ok {
		return nil
	}
	err := h.flushHeld()
	if err != nil {
		w.fail("write", err)
	}
	return err
}

// reasonPeerClosed is describeNetErr's words for a clean close by the peer: EOF
// at a TLS record boundary — after its close_notify (how the edge ends a link it
// retires), or after a bare TCP FIN (crypto/tls reports both as io.EOF, so they
// cannot be told apart). reasonLocalClosed: our side closed the connection.
const (
	reasonPeerClosed  = "closed by the other server"
	reasonLocalClosed = "closed locally"
)

// describeNetErr turns the usual socket errors into words an operator can act
// on; anything else is passed through.
func describeNetErr(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, io.EOF):
		return reasonPeerClosed
	case errors.Is(err, net.ErrClosed):
		return reasonLocalClosed
	case errors.As(err, &ne) && ne.Timeout():
		return "timed out (path stalled)"
	case errors.Is(err, syscall.ECONNABORTED):
		// The kernel tore the socket down on THIS server — `ss -K`, a
		// conntrack flush or a local firewall rule — rather than the peer or
		// the path. Reads then fail with "software caused connection abort".
		return "aborted on this server (socket killed, e.g. ss -K or a local firewall)"
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
// Every session is watched by the wedge guard (wedge.go); its guard is left in
// meter.guard for the relays on the link. A session that can grow windows
// past NeedStreamBuffer gets a window core (wcore_run.go), left in meter.wc.
//
// R1's cores run on it as far as this side's kill switches and caps, the
// caps both ends have (tlscarrier.Carrier.Caps; newSession: this binary's),
// allow (r1run.go): the link scheduler (C2, linksched.go) writes the
// session's frames in place of smux's own writer, C3's guard (hostq.go)
// takes the send gate's place under the shaper, and C4-lite (sendcap.go)
// caps the socket's pacing.
func newSession(conn net.Conn, server bool, sampler *obfs.LengthSampler, meter *linkMeter) (*smux.Session, func() string, error) {
	return newSessionCaps(conn, server, sampler, meter, tlscarrier.LocalAuthHdr().Caps)
}

// newSessionCaps is newSession for a link whose ends both have caps.
func newSessionCaps(conn net.Conn, server bool, sampler *obfs.LengthSampler, meter *linkMeter, caps uint32) (*smux.Session, func() string, error) {
	var sp atomic.Pointer[smux.Session]
	tcp := tcpUnder(conn)
	var low net.Conn // what the shaper writes to: C3's guard, or the send gate
	var tally writeTally
	var hq *hostQ
	if !HostQOff() && caps&tlscarrier.CapHostQ != 0 {
		hq = newHostQ(conn, newHostSys(conn))
		low, tally = &hostQConn{Conn: conn, hq: hq, tcp: tcp}, hq
	} else {
		gate := newSendGate(conn)
		low, tally = gate, gate
	}
	var c net.Conn = newShapedConn(low, sampler)
	if meter != nil {
		c = &meteredConn{Conn: c, m: meter, gate: tally}
	}
	w := &watchConn{Conn: c}
	w.onErr = func() {
		low.Close() // conn's Close, ending a write waiting at the gate
		if s := sp.Load(); s != nil {
			s.Close()
		}
	}
	var sess *smux.Session
	var err error
	cfg := newSmuxConfig()
	var ls *linkSched
	if !SchedOff() && caps&tlscarrier.CapSched != 0 {
		ls = newLinkSched(w)
	}
	switch {
	case ls != nil && server:
		sess, err = smux.ServerQueued(w, cfg, ls)
	case ls != nil:
		sess, err = smux.ClientQueued(w, cfg, ls)
	case server:
		sess, err = smux.Server(w, cfg)
	default:
		sess, err = smux.Client(w, cfg)
	}
	if err != nil {
		return nil, nil, err
	}
	sp.Store(sess)
	if ls != nil {
		ls.start(sess)
	}
	g := &sessGuard{w: w}
	g.sess.Store(sess)
	if meter != nil {
		meter.guard = g
	}
	guards.add(g)
	wc := wcores.add(sess, cfg, tcp, meter, server, conn.RemoteAddr())
	if wc != nil && meter != nil {
		meter.wc = wc
	}
	r1Links.add(sess, ls, hq, tcp, meter, wc, caps)
	return sess, w.reason, nil
}

// streamPkt carries L3 frames over one smux stream (see pktConn).
type streamPkt struct {
	st  *smux.Stream
	tun tlscarrier.TunReader
}

func newStreamPkt(st *smux.Stream) *streamPkt {
	return &streamPkt{st: st, tun: tlscarrier.TunReader{R: st}}
}

func (s *streamPkt) WriteRaw(b []byte) error {
	s.st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := s.st.Write(b)
	return err
}

func (s *streamPkt) ReadTunFrame() (byte, []byte, uint32, uint32, error) { return s.tun.Read() }

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
