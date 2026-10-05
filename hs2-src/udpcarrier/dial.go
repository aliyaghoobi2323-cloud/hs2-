package udpcarrier

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
)

// EncapConfig selects the datagram encapsulation the carrier rides on and its
// transport-level options. The zero value is plain UDP with a kernel-chosen
// source, i.e. exactly the carrier's original behaviour, so every existing
// caller keeps working through the udp-defaulting wrappers below.
type EncapConfig struct {
	Kind   string // encap kind: "" / "udp" (default), "icmp", "gre", "ipip", "ipx"
	BindIP string // local source IP (a config's bind_local_ip / listen_ip)
	Proto  int    // IP protocol number for the ipx encapsulation
}

// The raw encapsulations key their framing magic from the tunnel's shared
// secret (encap derives it through HMAC with its own label, so the magic
// reveals nothing about the secret and differs per kind and direction).
func (ec EncapConfig) dialOptions(shared []byte) encap.Options {
	return encap.Options{BindIP: ec.BindIP, ICMPRole: "client", Proto: ec.Proto, Key: shared}
}

func (ec EncapConfig) listenOptions(shared []byte) encap.Options {
	return encap.Options{BindIP: ec.BindIP, ICMPRole: "server", Proto: ec.Proto, Key: shared}
}

// DefaultInnerMTU is the tunnel-side MTU the carrier is sized for. It is
// smaller than the TCP carriers' 1380 to leave room for the datagram overhead
// (CarrierOverhead, 52 bytes, plus the encapsulation header) inside a
// 1500-byte path without fragmenting, with margin for paths below 1500 (see
// InnerMTUFor for the exact limit per encapsulation).
const DefaultInnerMTU = 1280

// staticPair derives the pinned server and client static keys from the tunnel's
// one shared secret, so both ends agree without a second key exchange.
func staticPair(shared []byte) (server, client core.StaticKey, err error) {
	server, err = core.StaticFromSeed(shared, "hs2-udp-responder")
	if err != nil {
		return
	}
	client, err = core.StaticFromSeed(shared, "hs2-udp-initiator")
	return
}

func randID() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

// Dial establishes a UDP carrier as the initiator: it runs the Noise IKpsk2
// handshake over datagrams, then the exporter-bound key confirmation, and
// returns a live Conn. The shared secret is the tunnel's shared_key.
func Dial(ctx context.Context, addr string, shared []byte, innerMTU int) (*Conn, error) {
	return DialFrom(ctx, addr, "", shared, innerMTU)
}

// DialFrom is Dial sending from a chosen local source IP (a config's
// bind_local_ip); empty lets the kernel choose. Every datagram of the carrier
// leaves from that IP, so a multi-IP server whose default address is filtered
// can still egress from its clean one.
func DialFrom(ctx context.Context, addr, bindIP string, shared []byte, innerMTU int) (*Conn, error) {
	return DialCfg(ctx, addr, EncapConfig{BindIP: bindIP}, shared, innerMTU)
}

// DialCfg is the general dialer: it establishes the carrier over the chosen
// encapsulation (udp by default, or a raw icmp/gre/ipip/ipx transport). The
// carrier's Noise handshake, key confirmation, FEC and pacing are identical for
// every encapsulation — only the outer datagram envelope differs — so a path
// that passes only ICMP can carry exactly the same secure tunnel as UDP.
func DialCfg(ctx context.Context, addr string, ec EncapConfig, shared []byte, innerMTU int) (*Conn, error) {
	if innerMTU <= 0 {
		innerMTU = DefaultInnerMTU
	}
	server, client, err := staticPair(shared)
	if err != nil {
		return nil, err
	}
	conn, err := encap.Dial(ec.Kind, addr, ec.dialOptions(shared))
	if err != nil {
		return nil, err
	}
	sess, binding, err := dialHandshake(ctx, conn, client, server.Public, shared)
	if err != nil {
		conn.Close()
		return nil, err
	}

	write := func(b []byte) error { _, e := conn.Write(b); return e }
	c := newConn(sess, write, shared, binding, innerMTU, conn, nil)
	c.kind = ec.Kind // echo-shaping in the pool keys off this (the dial side sends requests)
	// Several datagrams per syscall where the socket can (sendmmsg).
	switch bc := conn.(type) {
	case interface{ WriteBatch([][]byte) error }: // a raw encapsulation's shared socket
		c.setWriteBatch(bc.WriteBatch)
	case *net.UDPConn:
		if wb := udpConnWriteBatch(bc); wb != nil {
			c.setWriteBatch(wb)
		}
	}

	// Read pump: everything after the handshake goes through the FEC path. A
	// raw encapsulation's shared dial socket hands packets over directly (one
	// reader for every carrier to the peer, so it must never block: a carrier
	// whose decode queue is full loses the packet, as on the listener side).
	if r, ok := conn.(interface {
		SetReceiver(func([]byte), func(error))
	}); ok {
		r.SetReceiver(func(b []byte) { c.tryFeed(append([]byte(nil), b...)) }, func(error) { c.Close() })
	} else {
		c.wg.Add(1)
		go c.readPump(conn)
	}

	// Confirmation: prove ourselves and require the server's proof. Both tags
	// are retransmitted because a single one can be lost on a bursty path.
	if err := c.runClientConfirm(ctx, 6*time.Second); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// readPump reads the dial socket until the carrier closes or the socket fails
// — what is waiting in one call (recvmmsg) on a UDP socket.
func (c *Conn) readPump(conn net.Conn) {
	defer c.wg.Done()
	if uc, ok := conn.(*net.UDPConn); ok {
		if rb := udpConnReadBatch(uc); rb != nil {
			for {
				select {
				case <-c.done:
					return
				default:
				}
				conn.SetReadDeadline(time.Now().Add(deadAfter))
				if err := rb(c.feed); err != nil {
					select {
					case <-c.done:
					default:
						c.Close()
					}
					return
				}
			}
		}
	}
	buf := make([]byte, 2048)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(deadAfter))
		n, err := conn.Read(buf)
		if err != nil {
			select {
			case <-c.done:
			default:
				c.Close()
			}
			return
		}
		c.feed(append([]byte(nil), buf[:n]...))
	}
}

// dialHandshake runs Noise message 1/2 with retransmission (message 1 may be
// lost on a lossy UDP path) and returns the session and the transcript binding.
func dialHandshake(ctx context.Context, conn net.Conn, local core.StaticKey, remoteStatic, psk []byte) (*core.Session, []byte, error) {
	ini, err := core.NewInitiator(local, remoteStatic, psk)
	if err != nil {
		return nil, nil, err
	}
	m1, err := ini.WriteMessage1()
	if err != nil {
		return nil, nil, err
	}
	buf := make([]byte, 2048)
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for attempt := 0; attempt < 12 && time.Now().Before(deadline); attempt++ {
		if _, err := conn.Write(m1); err != nil {
			return nil, nil, err
		}
		wait := time.Duration(300+attempt*150) * time.Millisecond
		conn.SetReadDeadline(time.Now().Add(wait))
		n, err := conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // message 1 or 2 lost; retransmit
			}
			return nil, nil, err
		}
		secret, _, err := ini.ReadMessage2Payload(append([]byte(nil), buf[:n]...))
		if err != nil {
			// Not a valid message 2: wrong key or a stray packet. Keep trying
			// until the deadline rather than failing on one bad datagram.
			continue
		}
		conn.SetReadDeadline(time.Time{})
		sess, err := core.NewSession(secret, true, randID())
		if err != nil {
			return nil, nil, err
		}
		return sess, ini.Binding(), nil
	}
	// A server with another shared_key stays silent on purpose (probe
	// resistance), so a key mismatch looks exactly like a blocked path.
	return nil, nil, fmt.Errorf("udpcarrier: no handshake reply from %s (the path drops it, the other server is not running, or its shared_key differs)", conn.RemoteAddr())
}

// confirmResend is how often each side retransmits its confirmation tag while
// it waits for the peer's, so the exchange survives loss on a bursty path.
const confirmResend = 150 * time.Millisecond

// runClientConfirm sends the client's confirmation, resending it periodically,
// and returns once it has verified the server's confirmation.
func (c *Conn) runClientConfirm(ctx context.Context, timeout time.Duration) error {
	mine := clientConfirm(c.shared, c.sess, c.binding)
	want := serverConfirm(c.shared, c.sess, c.binding)
	if err := c.sendControl(core.TypeAuth, mine); err != nil {
		return err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	resend := time.NewTicker(confirmResend)
	defer resend.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errClosed
		case <-deadline.C:
			return errConfirmFail
		case <-resend.C:
			if err := c.sendControl(core.TypeAuth, mine); err != nil {
				return err
			}
		case tag := <-c.confCh:
			if macEqual(tag, want) {
				return nil
			}
		}
	}
}

// runServerConfirm waits for the client's confirmation and answers each one it
// sees with the server's confirmation (the client resends until it gets one).
// It signals acceptance on the first valid client confirmation and then keeps
// answering resends for a short grace period so a lost server confirmation is
// recovered. accept is called at most once.
func (c *Conn) runServerConfirm(grace time.Duration, accept func()) {
	want := clientConfirm(c.shared, c.sess, c.binding)
	mine := serverConfirm(c.shared, c.sess, c.binding)
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	accepted := false
	for {
		select {
		case <-c.done:
			return
		case <-deadline.C:
			if !accepted {
				c.Close()
			}
			return
		case tag := <-c.confCh:
			if !macEqual(tag, want) {
				continue
			}
			if err := c.sendControl(core.TypeAuth, mine); err != nil {
				if !accepted {
					c.Close()
				}
				return
			}
			if !accepted {
				accepted = true
				accept()
				// keep answering resends for the grace period
				deadline.Reset(grace)
			}
		}
	}
}
