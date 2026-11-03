package engine

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
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
	bindIP       string // optional local source IP
	local        core.StaticKey
	remoteStatic []byte
	psk          []byte
}

func (d *noiseDialer) Dial(ctx context.Context) (Carrier, error) {
	sess, conn, err := dialCarrier(ctx, d.addr, d.bindIP, d.local, d.remoteStatic, d.psk, 1)
	if err != nil {
		return nil, err
	}
	return &noiseCarrier{conn: conn, sess: sess, reader: sess.NewStreamReader()}, nil
}

// noiseListener accepts raw TCP carriers and runs the Noise responder. Each
// handshake runs on its own (at most noiseHandshakes at a time), so a client
// that connects and then says nothing — a scanner, a dead path — holds only
// its own slot for the 10 s handshake deadline, not every other carrier's
// accept behind it.
type noiseListener struct {
	ln     net.Listener
	resp   *core.Responder
	idSeq  atomic.Uint64
	accept chan Carrier
	ctx    context.Context
	cancel context.CancelFunc
}

// noiseHandshakes bounds the handshakes in flight; a connection beyond it
// waits in the kernel's accept queue, as every one used to.
const noiseHandshakes = 64

func newNoiseListener(addr string, local core.StaticKey, psk []byte) (*noiseListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &noiseListener{ln: ln, resp: core.NewResponder(local, psk), accept: make(chan Carrier), ctx: ctx, cancel: cancel}
	go l.serve()
	return l, nil
}

func (l *noiseListener) serve() {
	var bo acceptBackoff // a transient EMFILE/ENOBUFS must not close the listener for good
	slots := make(chan struct{}, noiseHandshakes)
	for {
		select {
		case slots <- struct{}{}:
		case <-l.ctx.Done():
			return
		}
		conn, err := l.ln.Accept()
		if err != nil {
			<-slots
			if !bo.wait(l.ctx, err, nil, "noise") {
				l.cancel()
				return
			}
			continue
		}
		bo.ok()
		go func() {
			sess, err := acceptCarrier(conn, l.resp, l.idSeq.Add(1))
			<-slots
			if err != nil {
				conn.Close() // failed handshake; the listener carries on
				return
			}
			c := &noiseCarrier{conn: conn, sess: sess, reader: sess.NewStreamReader()}
			select {
			case l.accept <- c:
			case <-l.ctx.Done():
				c.Close()
			}
		}()
	}
}

func (l *noiseListener) Accept(ctx context.Context) (Carrier, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (l *noiseListener) Close() error {
	l.cancel()
	return l.ln.Close()
}
