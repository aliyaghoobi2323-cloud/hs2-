package tlscarrier

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Carrier is a live TLS session carrying hs2 frames directly inside TLS records
// (no second encryption layer). Frame inside TLS: [ftype:1][len:3][payload].
type Carrier struct {
	conn   net.Conn // *tls.Conn (server) or *utls.UConn (client)
	sendMu sync.Mutex
	rbuf   []byte // body buffer reused by ReadFrameReuse
}

// writeTimeout bounds one write; a link that cannot drain for this long is
// treated as dead.
const writeTimeout = 5 * time.Second

// NewCarrier wraps an already-authenticated connection as a Carrier.
func NewCarrier(conn net.Conn) *Carrier { return &Carrier{conn: conn} }

// AppendFrame appends one unpadded frame to dst in the wire format SendFrame
// uses, so a caller can coalesce several frames into one WriteRaw.
func AppendFrame(dst []byte, ftype byte, payload []byte) []byte {
	n := len(payload)
	dst = append(dst, ftype, byte(n>>16), byte(n>>8), byte(n), 0, 0, 0)
	return append(dst, payload...)
}

// WriteRaw writes frames built with AppendFrame in a single write.
func (c *Carrier) WriteRaw(b []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write(b)
	return err
}

// Frame inside TLS: [ftype:1][reallen:3][padlen:3][payload(reallen)][pad(padlen)]
// pad lets the on-wire record size be drawn from a distribution independent of
// the real payload; the receiver returns only the real payload.
func (c *Carrier) SendFrame(ftype byte, payload []byte) error {
	return c.writeFrame(ftype, payload, 0)
}

// SendFramePadded sends ftype with the real payload, padded so the body reaches
// len(padded) bytes. padded must be >= len(payload).
func (c *Carrier) SendFramePadded(ftype byte, payload, padded []byte) error {
	pad := len(padded) - len(payload)
	if pad < 0 {
		pad = 0
	}
	return c.writeFrame(ftype, payload, pad)
}

func (c *Carrier) writeFrame(ftype byte, payload []byte, pad int) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	body := len(payload) + pad
	buf := make([]byte, 7+body)
	buf[0] = ftype
	buf[1], buf[2], buf[3] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	buf[4], buf[5], buf[6] = byte(pad>>16), byte(pad>>8), byte(pad)
	copy(buf[7:], payload)
	// pad bytes are left zero; they are inside TLS so unobservable and harmless
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write(buf)
	return err
}

func (c *Carrier) ReadFrame() (byte, []byte, error) {
	return c.readFrame(nil)
}

// ReadFrameReuse is ReadFrame without a per-frame allocation: the returned
// payload is only valid until the next ReadFrameReuse call.
func (c *Carrier) ReadFrameReuse() (byte, []byte, error) {
	ft, p, err := c.readFrame(c.rbuf)
	if cap(p) > cap(c.rbuf) {
		c.rbuf = p[:cap(p)]
	}
	return ft, p, err
}

// readFrame reads one frame, using buf for the body when it is large enough.
func (c *Carrier) readFrame(buf []byte) (byte, []byte, error) {
	var hdr [7]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	reallen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	pad := int(hdr[4])<<16 | int(hdr[5])<<8 | int(hdr[6])
	if reallen > 1<<20 || pad > 1<<20 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	body := buf
	if cap(body) < reallen+pad {
		body = make([]byte, reallen+pad)
	}
	body = body[:reallen+pad]
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body[:reallen], nil
}

func (c *Carrier) Close() error { return c.conn.Close() }

// RawConn returns the underlying authenticated TLS connection, for callers that
// want to run their own multiplexer (smux) on top instead of hs2 framing. After
// this is used, do not also call SendFrame/ReadFrame on the carrier.
func (c *Carrier) RawConn() net.Conn { return c.conn }

// TCPConn returns the underlying *net.TCPConn beneath TLS, or nil if it is not a
// TCP socket. It lets the engine read kernel TCP stats (retransmits) for per-link
// health without a second measurement path.
func (c *Carrier) TCPConn() *net.TCPConn {
	type netConner interface{ NetConn() net.Conn }
	// Unwrap net.Conn wrappers until the kernel socket is reached — through ANY
	// number of them, not just tls.Conn's one. A wrapper between tls.Conn and
	// the socket (the old probe peek) once made a single unwrap stop short and
	// silently zeroed TCP_INFO (retransmits/rwnd) for the autopilot; walking the
	// whole chain keeps a future wrapper from doing that again. Bounded so a
	// self-referential wrapper cannot loop.
	cur := c.conn
	for i := 0; i < 8 && cur != nil; i++ {
		if tc, ok := cur.(*net.TCPConn); ok {
			return tc
		}
		nc, ok := cur.(netConner)
		if !ok {
			break
		}
		cur = nc.NetConn()
	}
	return nil
}

// SetReadDeadline lets the engine bound reads for keepalive/dead detection.
func (c *Carrier) SetReadDeadline(t time.Time) { c.conn.SetReadDeadline(t) }

// Dial (client side): real uTLS handshake to the domain, then send the auth
// record inside TLS. Returns a live Carrier.
func Dial(addr, sni string, sharedKey []byte) (*Carrier, error) {
	return DialFrom(addr, sni, sharedKey, "")
}

// DialFrom is Dial with an optional local source IP (bindIP). Empty bindIP uses
// the OS default source. This lets the tunnel egress from a chosen IP on a
// multi-IP server instead of the main IP.
func DialFrom(addr, sni string, sharedKey []byte, bindIP string) (*Carrier, error) {
	return DialFromTimeout(addr, sni, sharedKey, bindIP, 8*time.Second)
}

// DialFromTimeout is DialFrom with its own TCP connect timeout. A short one
// keeps a dial's SYNs close together (the kernel spaces a hanging connect's
// retransmits 1, 2, 4 s apart), for a caller that retries anyway.
func DialFromTimeout(addr, sni string, sharedKey []byte, bindIP string, connect time.Duration) (*Carrier, error) {
	return dialTLS(addr, sni, sharedKey, bindIP, connect, nil, nil)
}

// ErrTargetAnswered: a tls dial to a reality-capable listener got the
// reality target's certificate (a name of the profile's server_names, not
// our SNI): the listener is pure reality now and spliced the hello to the
// target. Failing at the handshake saves waiting for a proof that never
// comes (the 10 s auth deadline, rel45).
var ErrTargetAnswered = errors.New("tlscarrier: tls dial answered by the reality target (the listener is pure reality now)")

// dialTLS is DialFromTimeout; targetNames (the path's reality server_names,
// nil for a plain tls path) make a certificate for one of them that is not
// for sni fail the handshake with ErrTargetAnswered.
func dialTLS(addr, sni string, sharedKey []byte, bindIP string, connect time.Duration, targetNames []string, peerNames func() []string) (*Carrier, error) {
	d := net.Dialer{Timeout: connect}
	if bindIP != "" {
		ip := net.ParseIP(bindIP)
		if ip == nil {
			// Never fall back to the default source: on a multi-IP server that may be
			// the filtered address, and a typo would look like a dead path.
			return nil, fmt.Errorf("tlscarrier: invalid bind_local_ip %q", bindIP)
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	raw, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	// Aggressive TCP keepalive so the kernel detects a black-holed link (packets
	// dropped, no RST) and fails reads, which lets smux/health-probe declare the
	// link dead and the manager rebuild it.
	// (HS2_QUIET_IDLE: 45 s like Chrome; TCP_USER_TIMEOUT still fails a link
	// whose data goes unacknowledged, see dpi.go.)
	if tc, ok := raw.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(dialKeepAlive())
	}
	tuneTCP(raw)
	cfg := &utls.Config{ServerName: sni, InsecureSkipVerify: true}
	if len(targetNames) > 0 {
		cfg.VerifyPeerCertificate = targetCertCheck(sni, targetNames)
	} else if peerNames != nil {
		// rel47: a plain tls path to a peer known to run reality on some IP
		// (announced or remembered names, read when the certificate arrives:
		// a dial sent just before the first announcement still gets them).
		cfg.VerifyPeerCertificate = func(raw [][]byte, ch [][]*x509.Certificate) error {
			if n := peerNames(); len(n) > 0 {
				return targetCertCheck(sni, n)(raw, ch)
			}
			return nil
		}
	}
	u, err := newClientConn(wrapBatch(raw), cfg, addr, clientHello)
	if err != nil {
		raw.Close()
		return nil, err
	}
	raw.SetDeadline(time.Now().Add(authTimeout))
	if err := u.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	// The certificate is not validated (the Iran side may lack CA roots, and a
	// domain may be fronted); instead both sides prove the shared key bound to
	// this TLS session, which defeats interception. See auth.go.
	ekm, err := clientEKM(u, cfg)
	if err != nil {
		raw.Close()
		return nil, err
	}
	rec, nonce := makeClientAuth(sharedKey, ekm)
	if _, err := u.Write(rec); err != nil {
		raw.Close()
		return nil, err
	}
	stop := watchPeerNames(u, raw, sni, targetNames, peerNames)
	err = readServerProof(u, sharedKey, nonce, ekm)
	if stop() {
		raw.Close()
		return nil, ErrTargetAnswered
	}
	if err != nil {
		raw.Close()
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	return &Carrier{conn: u}, nil
}

// watchPeerNames (rel47): a plain tls dial whose certificate arrived before
// the peer's reality names were known (the first announcement lands while
// the dial waits for its proof) re-checks the leaf against them every 100 ms
// and ends the wait at once when it turns out to be the target's. stop ends
// the watch and reports whether it fired.
func watchPeerNames(u *utls.UConn, raw net.Conn, sni string, targetNames []string, peerNames func() []string) (stop func() bool) {
	if len(targetNames) > 0 || peerNames == nil {
		return func() bool { return false }
	}
	pc := u.ConnectionState().PeerCertificates
	if len(pc) == 0 {
		return func() bool { return false }
	}
	leaf := [][]byte{pc[0].Raw}
	var fired atomic.Bool
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if n := peerNames(); len(n) > 0 && errors.Is(targetCertCheck(sni, n)(leaf, nil), ErrTargetAnswered) {
					fired.Store(true)
					raw.SetDeadline(time.Now()) // ends the proof read
					return
				}
			}
		}
	}()
	return func() bool {
		close(done)
		return fired.Load()
	}
}

// clientEKM exports the channel-binding secret of a finished uTLS handshake.
// The Chrome preset advertises renegotiation support, which makes uTLS refuse
// to export; TLS 1.3 has no renegotiation at all, so once 1.3 is confirmed
// the flag can be cleared safely. Links are refused on anything older.
func clientEKM(u *utls.UConn, cfg *utls.Config) ([]byte, error) {
	cs := u.ConnectionState()
	if cs.Version != utls.VersionTLS13 {
		return nil, errors.New("tlscarrier: server did not negotiate TLS 1.3")
	}
	cfg.Renegotiation = utls.RenegotiateNever
	cs = u.ConnectionState()
	return exportEKM(&cs)
}

var _ = binary.BigEndian
var _ = rand.Read
var _ = context.Background

// targetCertCheck: the leaf is for one of targetNames and not for sni.
func targetCertCheck(sni string, targetNames []string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return nil
		}
		c, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return nil
		}
		if sni != "" && c.VerifyHostname(sni) == nil {
			return nil
		}
		for _, n := range targetNames {
			if n != "" && !strings.EqualFold(n, sni) && c.VerifyHostname(n) == nil {
				return ErrTargetAnswered
			}
		}
		return nil
	}
}
