package engine

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// This wires the standard-TLS carrier (real cert, in-stream auth, probe
// resistance) to the engine, with the statistical obfuscator applied to frame
// sizes. The engine stays carrier-agnostic; this file is the only place that
// knows the carrier is TLS.

// tlsCarrierAdapter wraps a *tlscarrier.Carrier and shapes each send: the
// payload is padded up to a size sampled from the HTTPS length distribution, so
// the on-wire record sizes match ordinary bulk HTTPS instead of tracking the
// real payload. (Pacing/timing shaping is applied by the engine's send loop via
// the Pacer; this adapter handles length.)
type tlsCarrierAdapter struct {
	c       *tlscarrier.Carrier
	sampler *obfs.LengthSampler
}

func (a *tlsCarrierAdapter) SendFrame(ftype byte, payload []byte) error {
	// Pad payload up to a sampled target so the TLS record size is drawn from
	// the HTTPS distribution. The receiver strips padding by the frame's own
	// length header (payload length is carried in the frame, pad is extra).
	target := a.sampler.Sample()
	if len(payload) < target {
		padded := make([]byte, target)
		copy(padded, payload)
		// mark real length so the peer can trim: we prepend a 3-byte real-len.
		return a.c.SendFramePadded(ftype, payload, padded)
	}
	return a.c.SendFrame(ftype, payload)
}

// ReadFrame bounds every read by deadAfter. Without it a silently black-holed
// path (packets dropped, no RST) would block here forever and the engine would
// never reconnect. Keepalives (every ~5s) keep a healthy link under the limit.
func (a *tlsCarrierAdapter) ReadFrame() (byte, []byte, error) {
	a.c.SetReadDeadline(time.Now().Add(deadAfter))
	return a.c.ReadFrame()
}
func (a *tlsCarrierAdapter) Close() error { return a.c.Close() }

// tlsDialer dials the standard-TLS carrier.
type tlsDialer struct {
	addr, sni string
	sharedKey []byte
	sampler   *obfs.LengthSampler
}

func (d *tlsDialer) Dial(ctx context.Context) (Carrier, error) {
	c, err := tlscarrier.Dial(d.addr, d.sni, d.sharedKey)
	if err != nil {
		return nil, err
	}
	return &tlsCarrierAdapter{c: c, sampler: d.sampler}, nil
}

// tlsListener accepts standard-TLS carriers; probes are handled inside the
// tlscarrier.Server (forwarded to the backend) and never surface here.
type tlsListener struct {
	ln      net.Listener
	srv     *tlscarrier.Server
	accept  chan Carrier
	ctx     context.Context
	cancel  context.CancelFunc
	sampler *obfs.LengthSampler
}

func newTLSListener(addr, backendAddr string, sharedKey []byte, cert tls.Certificate) (*tlsListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &tlsListener{
		ln:      ln,
		srv:     &tlscarrier.Server{SharedKey: sharedKey, Cert: cert, BackendAddr: backendAddr},
		accept:  make(chan Carrier, 8),
		ctx:     ctx,
		cancel:  cancel,
		sampler: obfs.NewHTTPSLengthSampler(),
	}
	go l.serve()
	return l, nil
}

func (l *tlsListener) serve() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.srv.Handle(l.ctx, conn, func(c *tlscarrier.Carrier) {
			select {
			case l.accept <- &tlsCarrierAdapter{c: c, sampler: l.sampler}:
			case <-l.ctx.Done():
				c.Close()
			}
		})
	}
}

func (l *tlsListener) Accept(ctx context.Context) (Carrier, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}
}

func (l *tlsListener) Close() error { l.cancel(); return l.ln.Close() }

// Constructors for cmd.
func NewTLSDialer(addr, sni string, sharedKey []byte) CarrierDialer {
	return &tlsDialer{addr: addr, sni: sni, sharedKey: sharedKey, sampler: obfs.NewHTTPSLengthSampler()}
}
func NewTLSListener(addr, backendAddr string, sharedKey []byte, cert tls.Certificate) (CarrierListener, error) {
	return newTLSListener(addr, backendAddr, sharedKey, cert)
}
