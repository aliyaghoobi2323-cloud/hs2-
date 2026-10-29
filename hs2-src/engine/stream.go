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

// Flush flushes what the conns under it hold back (the shaper's tail): smux's
// writer, as it runs out of frames (C2).
func (w *watchConn) Flush() error {
	f, ok := w.Conn.(interface{ Flush() error })
	if !ok {
		return nil
	}
	err := f.Flush()
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
// The link has every R1 core this binary has (newSessionCaps).
func newSession(conn net.Conn, server bool, sampler *obfs.LengthSampler, meter *linkMeter) (*smux.Session, func() string, error) {
	return newSessionCaps(conn, server, sampler, meter, tlscarrier.LocalAuthHdr().Caps)
}

// newSessionCaps is newSession for a link whose ends both have caps (the
// carrier's, as negotiated): a core runs on it only when its bit is there
// and its kill switch is not set. C2 (linksched.go): smux's frame scheduler
// in place of its shaper, the shaper's tail carried from frame to frame;
// C3: the host-queue guard under the shaper in place of sendGate; C4-lite:
// the pacing cap of the link socket. The cores' runner steps the link.
func newSessionCaps(conn net.Conn, server bool, sampler *obfs.LengthSampler, meter *linkMeter, caps uint32) (*smux.Session, func() string, error) {
	var sp atomic.Pointer[smux.Session]
	sched := !SchedOff() && caps&tlscarrier.CapSched != 0
	guardOn := !HostQOff() && caps&tlscarrier.CapHostQ != 0
	capOn := !SendCapOff() && caps&tlscarrier.CapSendCap != 0
	tc := tcpUnder(conn)
	var bottom net.Conn
	var gate *sendGate
	var hq *hostQ
	if guardOn {
		hc := &hostQConn{Conn: conn, tcp: tc}
		sys, ok := conn.(hostSys) // (a test's emulated socket)
		if !ok {
			sys = newHostSys(conn)
		}
		if sys != nil {
			sys = gateSys{sys, hc}
		}
		hq = newHostQ(conn, sys)
		if quiet, _ := classOf(caps); quiet && HQGQuietSegs >= 0 {
			hq.gate.segs = HQGQuietSegs // (lab: a QUIET link's own gate variant)
		}
		hc.h = hq
		bottom = hc
	} else {
		gate = newSendGate(conn)
		bottom = gate
	}
	if q, _ := classOf(caps); q && QuietRec != 0 {
		sampler = obfs.NewInteractiveLengthSampler() // (lab: QUIET's records, quiet.go)
	}
	shaped := newShapedConn(bottom, sampler)
	shaped.carry = sched
	var c net.Conn = shaped
	if meter != nil {
		c = &meteredConn{Conn: c, m: meter, gate: gate, hq: hq}
	}
	w := &watchConn{Conn: c}
	w.onErr = func() {
		bottom.Close() // conn's Close, ending a write waiting at the gate
		if s := sp.Load(); s != nil {
			s.Close()
		}
	}
	var sess *smux.Session
	var err error
	cfg := newSmuxConfig()
	var flows *linkFlows
	if sched {
		flows = &linkFlows{}
		cfg.Sched = newSchedAdapter
		cfg.FlowInfo = flows.info
		cfg.BulkFrame = BulkFrameSize
		cfg.LaneFrame = laneData
		cfg.UserFrames = schedUserFrames
		cfg.UserMem = userMems.handle
		cfg.EarlyDone = !EarlyDoneOff() // P10 (b): a stream's next frame queued while one is written
	}
	// β2 (quiet.go): a BULK link's sockets get α's cap in place of
	// C4-lite's; QUIET ones, and links of no class, keep C4-lite
	quietLink, bulkLink := classOf(caps)
	if meter != nil {
		meter.quietLink = quietLink
	}
	if flows != nil {
		flows.bulkLink = bulkLink // its moved connections stay bulk (MovedBulk)
	}
	if sched && caps&tlscarrier.CapMoveBack != 0 {
		cfg.GroupMoves = moveBackMax // a group may move back and on (moveback.go)
	} else if sched && caps&tlscarrier.CapLight != 0 {
		cfg.GroupMoves = lightMoves // to the light QUIET link and on (quietlight.go)
	}
	// γ2 (budget.go): a BULK link's in-flight budget at its writer's bulk
	// lane, in place of α's rate cap (C4-lite's stays, the outer one)
	var lb *linkBudget
	if sched && bulkLink && capOn && !BudgetOff() {
		if lb = newLinkBudget(newOutQ(tc)); lb != nil {
			cfg.BulkGate = lb.gate
		}
	}
	if sched && quietLink && QuietBatch == 0 {
		cfg.FlushEach = true // (lab: γ2's batching measured against none)
	}
	if quietLink {
		shaped.count = true // the QUIET records' tallies (the status line)
		if sched && QuietBatch != 0 && QuietLazy != 0 && hq != nil && hq.sys != nil && hq.fb.Load() == nil {
			shaped.lazy = hq.sys.notsent // a tail waits for the socket's unsent bytes (shape.go)
		}
	}
	if quietLink && sched && QuietCoalesce > 0 {
		// a busy QUIET link's light frames share their padded records
		// within a short jittered hold (quiet.go, QuietCoalesce)
		cfg.Coalesce = time.Duration(QuietCoalesce) * time.Microsecond
		cfg.CoalesceMin = float64(QuietCoalesceMin)
		cfg.CoalesceJitter = QuietCoalJitter != 0
	}
	if bulkLink {
		setLinkSndbuf(tc) // a fixed send buffer: no growth to wait for (linkbuf.go)
	}
	if quietLink && QuietThin != 0 {
		setThinTimeouts(tc) // (best effort: a kernel without it keeps its back-off)
	}
	quietCC := ""
	if quietLink && QuietCC != "" {
		// a QUIET link carries light traffic only: BBR's PROBE_RTT (its
		// window at 4 segments for 200 ms each 10 s) would hold every
		// user's probe that moment, 198 of them a padded record each; the
		// first of the list the kernel takes (quietcc.go: Illinois keeps
		// its window on a path that loses packets at random)
		quietCC = setQuietCC(tc, quietCCList())
	}
	if server {
		sess, err = smux.Server(w, cfg)
	} else {
		sess, err = smux.Client(w, cfg)
	}
	if err != nil {
		return nil, nil, err
	}
	sp.Store(sess)
	g := &sessGuard{w: w}
	g.sess.Store(sess)
	if meter != nil {
		meter.guard = g
	}
	guards.add(g)
	wc := wcores.add(sess, cfg, tc, meter, server, conn.RemoteAddr())
	if wc != nil && meter != nil {
		meter.wc = wc
	}
	// C4-lite on every link socket at both ends, and C3's readings of it
	var scl *sendCapLink
	if capOn || hq != nil {
		if sys := newCapSys(tc); sys != nil {
			scl = newSendCapLink(sys, hq)
			scl.core.linked, scl.core.off = true, !capOn
			scl.core.alpha = bulkLink && !AlphaOff() && lb == nil
			scl.budget = lb
			if meter != nil {
				meter.owd.sys = sys         // a message's stamp: when it leaves the host
				meter.owd.quiet = quietLink // the guard of its readings (owd.go)
			}
			if bulkLink && meter != nil {
				scl.owd = &meter.owd // α's one-way delays (owd.go)
			}
			if meter != nil && SendCapBlocked != 0 {
				scl.blocked = meter.blockedNs
			}
			if wc != nil && capOn {
				wc.sc.Store(scl) // the pacing net hands its cap here: the lesser holds
			}
		} else if hq != nil {
			scl = newSendCapLink(nil, hq) // C3 on the link's written rate alone
		}
	}
	// (a budget whose socket no runner reads keeps its gate open: B 0)
	if scl != nil || flows != nil {
		cl := &coreLink{sess: sess, mtr: meter, sc: scl, hq: hq, flows: flows}
		if quietLink {
			cl.shp = shaped
			if QuietCCLoss != "" && tc != nil && quietCC != QuietCCLoss {
				// the switch to QuietCCLoss under random loss (quietcc.go)
				cl.qcc = newQuietCCWatch(tc, quietCC, QuietCCLoss)
			}
		}
		coreLinks.add(cl)
	}
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
