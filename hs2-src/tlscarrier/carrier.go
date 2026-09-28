package tlscarrier

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"sync"
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
	d := net.Dialer{Timeout: 8 * time.Second}
	if bindIP != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(bindIP)}
	}
	raw, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	// Aggressive TCP keepalive so the kernel detects a black-holed link (packets
	// dropped, no RST) and fails reads, which lets smux/health-probe declare the
	// link dead and the manager rebuild it.
	if tc, ok := raw.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(3 * time.Second)
	}
	tuneTCP(raw)
	u := utls.UClient(raw, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, utls.HelloChrome_133)
	if err := u.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	if _, err := u.Write(makeAuthRecord(sharedKey)); err != nil {
		raw.Close()
		return nil, err
	}
	return &Carrier{conn: u}, nil
}

var _ = binary.BigEndian
var _ = rand.Read
var _ = context.Background
