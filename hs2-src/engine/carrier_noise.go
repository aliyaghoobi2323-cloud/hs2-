package engine

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// noiseCarrier implements Carrier over the hs2 Noise core on a raw TCP stream.
// It is the existing, tested path (handshake + Session + masked framing) moved
// behind the Carrier interface. Used for carriers where there is no TLS layer
// and we want our own encryption.
type noiseCarrier struct {
	conn   net.Conn
	sess   *core.Session
	reader *core.StreamReader
	sendMu sync.Mutex
}

func (c *noiseCarrier) SendFrame(ftype byte, payload []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	f, err := c.sess.Seal(ftype, 0, payload, core.PadTarget(len(payload)))
	if err != nil {
		return err
	}
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.conn.Write(f)
	return err
}

func (c *noiseCarrier) ReadFrame() (byte, []byte, error) {
	c.conn.SetReadDeadline(time.Now().Add(deadAfter))
	ft, _, payload, err := c.reader.ReadFrame(c.conn)
	return ft, payload, err
}

func (c *noiseCarrier) Close() error { return c.conn.Close() }

// noiseDialer dials a raw TCP carrier and runs the Noise initiator handshake.
type noiseDialer struct {
	addr         string
	local        core.StaticKey
	remoteStatic []byte
	psk          []byte
}

func (d *noiseDialer) Dial(ctx context.Context) (Carrier, error) {
	sess, conn, err := dialCarrier(ctx, d.addr, d.local, d.remoteStatic, d.psk, 1)
	if err != nil {
		return nil, err
	}
	return &noiseCarrier{conn: conn, sess: sess, reader: sess.NewStreamReader()}, nil
}

// noiseListener accepts raw TCP carriers and runs the Noise responder.
type noiseListener struct {
	ln    net.Listener
	resp  *core.Responder
	idSeq uint64
}

func newNoiseListener(addr string, local core.StaticKey, psk []byte) (*noiseListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &noiseListener{ln: ln, resp: core.NewResponder(local, psk)}, nil
}

func (l *noiseListener) Accept(ctx context.Context) (Carrier, error) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return nil, err
		}
		l.idSeq++
		sess, err := acceptCarrier(conn, l.resp, l.idSeq)
		if err != nil {
			conn.Close() // failed handshake; try next
			continue
		}
		return &noiseCarrier{conn: conn, sess: sess, reader: sess.NewStreamReader()}, nil
	}
}

func (l *noiseListener) Close() error { return l.ln.Close() }
