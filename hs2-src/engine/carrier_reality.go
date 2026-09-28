package engine

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/reality"
)

// realityCarrier implements Carrier over a single real TLS session established
// by the reality package. There is NO Noise here: TLS already provides
// confidentiality, integrity and forward secrecy, so adding our cipher would
// only create the TLS-in-TLS fingerprint we are avoiding. Framing is a plain
// length-prefixed record because it rides inside TLS, which is itself the
// encryption. On the wire an observer sees ordinary TLS application-data.
//
// Frame inside TLS:  [ftype:1][len:3][payload]
type realityCarrier struct {
	conn   net.Conn // a *tls.Conn or *utls.UConn
	sendMu sync.Mutex
	rbuf   []byte
}

func (c *realityCarrier) SendFrame(ftype byte, payload []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	hdr := make([]byte, 4)
	hdr[0] = ftype
	hdr[1] = byte(len(payload) >> 16)
	hdr[2] = byte(len(payload) >> 8)
	hdr[3] = byte(len(payload))
	if err := setWrite(c.conn); err == nil {
		if _, err := c.conn.Write(append(hdr, payload...)); err != nil {
			return err
		}
	}
	return nil
}

func (c *realityCarrier) ReadFrame() (byte, []byte, error) {
	setReadDeadline(c.conn, deadAfter)
	var hdr [4]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	ftype := hdr[0]
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n < 0 || n > 1<<20 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, nil, err
	}
	return ftype, payload, nil
}

func (c *realityCarrier) Close() error { return c.conn.Close() }

func setWrite(c net.Conn) error {
	return c.SetWriteDeadline(time.Now().Add(5 * time.Second))
}
func setReadDeadline(c net.Conn, d time.Duration) {
	c.SetReadDeadline(time.Now().Add(d))
}

var _ = binary.BigEndian

// realityDialer establishes a reality carrier: a genuine Chrome-fingerprinted
// TLS session whose ClientHello carries the auth signal.
type realityDialer struct {
	addr      string
	sni       string
	sharedKey []byte
}

func (d *realityDialer) Dial(ctx context.Context) (Carrier, error) {
	uconn, err := reality.ClientDialTLS(d.addr, d.sni, d.sharedKey)
	if err != nil {
		return nil, err
	}
	return &realityCarrier{conn: uconn}, nil
}

// realityListener runs the reality dispatcher: it forwards probes/browsers to
// the cover site and, for authorised clients, completes the TLS handshake and
// yields a Carrier. Because forwarding happens inside the dispatcher, only
// authorised, TLS-established connections ever reach Accept.
type realityListener struct {
	ln     net.Listener
	disp   *reality.Dispatcher
	cert   tls.Certificate
	accept chan Carrier
	ctx    context.Context
	cancel context.CancelFunc
}

func newRealityListener(addr, coverAddr string, sharedKey []byte, cert tls.Certificate) (*realityListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rl := &realityListener{
		ln:     ln,
		cert:   cert,
		accept: make(chan Carrier, 8),
		ctx:    ctx,
		cancel: cancel,
	}
	rl.disp = &reality.Dispatcher{
		SharedKey: sharedKey,
		CoverAddr: coverAddr,
		OnTunnel: func(conn net.Conn, hello []byte, cr []byte) {
			tconn, err := reality.ServerTLS(conn, hello, cert)
			if err != nil {
				conn.Close()
				return
			}
			select {
			case rl.accept <- &realityCarrier{conn: tconn}:
			case <-rl.ctx.Done():
				tconn.Close()
			}
		},
	}
	go rl.serve()
	return rl, nil
}

func (l *realityListener) serve() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.disp.Handle(l.ctx, conn)
	}
}

func (l *realityListener) Accept(ctx context.Context) (Carrier, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}
}

func (l *realityListener) Close() error {
	l.cancel()
	return l.ln.Close()
}
