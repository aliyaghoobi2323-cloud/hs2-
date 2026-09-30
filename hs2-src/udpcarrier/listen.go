package udpcarrier

import (
	"bytes"
	"context"
	"crypto/subtle"
	"net"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
)

// Listener is the responder side of the UDP carrier. It owns one shared
// net.UDPConn and demultiplexes datagrams by source address: a datagram from a
// known peer is a data/control shard for that peer's Conn; a datagram from a
// new address is treated as a Noise message 1 and, if it authenticates AND
// carries the pinned initiator static key, starts a new carrier. Anything that
// fails is dropped silently, so a probe with a random or stale first message
// gets no reply — the same probe resistance the TCP carriers have.
type Listener struct {
	conn      *packetSocket
	shared    []byte
	innerMTU  int
	cliStatic core.StaticKey
	resp      *core.Responder

	probe probeResponder

	mu     sync.Mutex
	conns  map[string]*peerLink
	accept chan *Conn
	done   chan struct{}
	closed sync.Once
	wg     sync.WaitGroup
}

// peerLink is one client address's carrier plus the exact handshake it was
// created from. The responder handshake runs once per address; a client that
// retransmits the same message 1 (because message 2 was lost) gets the cached
// message 2 resent, so the replay-protected handshake is never re-run and there
// is always exactly one session per address.
type peerLink struct {
	c      *Conn
	m1, m2 []byte
	local  net.IP // the local address this peer targeted; replies leave from it (udp pktinfo)
}

// Listen binds a UDP socket and serves carriers derived from the shared secret.
func Listen(addr string, shared []byte, innerMTU int) (*Listener, error) {
	return ListenCfg(addr, EncapConfig{}, shared, innerMTU)
}

// ListenCfg is the general listener: it serves carriers over the chosen
// encapsulation (udp by default, or a raw icmp/gre/ipip/ipx transport). The
// per-peer demux, responder handshake, key confirmation, FEC and pacing are
// identical for every encapsulation; only the outer socket differs.
func ListenCfg(addr string, ec EncapConfig, shared []byte, innerMTU int) (*Listener, error) {
	if innerMTU <= 0 {
		innerMTU = DefaultInnerMTU
	}
	server, client, err := staticPair(shared)
	if err != nil {
		return nil, err
	}
	pc, err := encap.Listen(ec.Kind, addr, ec.listenOptions(shared))
	if err != nil {
		return nil, err
	}
	l := &Listener{
		conn:      newPacketSocket(pc), // wildcard UDP: reply from the IP each peer targeted
		shared:    shared,
		innerMTU:  innerMTU,
		cliStatic: client,
		resp:      core.NewResponder(server, shared),
		probe:     probeResponder{shared: shared},
		conns:     make(map[string]*peerLink),
		accept:    make(chan *Conn, 8),
		done:      make(chan struct{}),
	}
	l.wg.Add(1)
	go l.serve()
	return l, nil
}

// LocalAddr is the bound address (useful when the port was chosen as :0).
func (l *Listener) LocalAddr() net.Addr { return l.conn.localAddr() }

func (l *Listener) serve() {
	defer l.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, dst, err := l.conn.readFrom(buf)
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			// transient read error: keep serving
			continue
		}
		if addr == nil {
			continue
		}
		key := addr.String()
		l.mu.Lock()
		pl := l.conns[key]
		l.mu.Unlock()
		pkt := append([]byte(nil), buf[:n]...)
		if pl != nil {
			// A retransmitted first message (message 2 was lost) is answered
			// from cache; everything else is carrier traffic.
			if bytes.Equal(pkt, pl.m1) {
				l.send(pl.m2, addr, pl.local)
			} else {
				pl.c.feed(pkt)
			}
			continue
		}
		// A datagram from a new address is a probe or a handshake. Probes are
		// answered on this same port so the selector can measure the real data
		// path; anything else is a candidate first message.
		if l.probe.handle(pkt, func(b []byte) { l.send(b, addr, dst) }) {
			continue
		}
		l.tryHandshake(pkt, addr, dst)
	}
}

// send writes one datagram to addr from source src (nil: kernel's choice).
func (l *Listener) send(b []byte, addr net.Addr, src net.IP) error {
	return l.conn.writeTo(b, addr, src)
}

// tryHandshake runs the responder handshake for a datagram from a new address.
// dst is the local address the peer sent to; every reply to this peer leaves
// from it, so the peer's connected socket accepts them.
func (l *Listener) tryHandshake(m1 []byte, addr net.Addr, dst net.IP) {
	hs, _, err := l.resp.ReadMessage1Payload(m1)
	if err != nil {
		return // not a valid first message: silent drop (probe resistance)
	}
	// Pin the initiator: only the client key derived from OUR shared secret is
	// accepted. This is the mutual half of the auth — a peer with the psk but a
	// different static key cannot open a carrier.
	if subtle.ConstantTimeCompare(core.PeerStatic(hs), l.cliStatic.Public) != 1 {
		return
	}
	m2, secret, err := l.resp.WriteMessage2(hs)
	if err != nil {
		return
	}
	if err := l.send(m2, addr, dst); err != nil {
		return
	}
	sess, err := core.NewSession(secret, false, randID())
	if err != nil {
		return
	}
	binding := core.HandshakeBinding(hs)

	write := func(b []byte) error { return l.send(b, addr, dst) }
	key := addr.String()
	var c *Conn
	c = newConn(sess, write, l.shared, binding, l.innerMTU, nil, func() {
		l.mu.Lock()
		if pl := l.conns[key]; pl != nil && pl.c == c {
			delete(l.conns, key)
		}
		l.mu.Unlock()
	})
	l.mu.Lock()
	if old := l.conns[key]; old != nil {
		old.c.Close()
	}
	l.conns[key] = &peerLink{c: c, m1: m1, m2: m2, local: dst}
	l.mu.Unlock()

	// Confirmation: require the client's proof, answer with ours, then yield.
	// The exchange is retransmitted (both ways) to survive a bursty path.
	go c.runServerConfirm(3*time.Second, func() {
		select {
		case l.accept <- c:
		case <-l.done:
			c.Close()
		}
	})
}

// Accept returns the next authenticated carrier.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, errClosed
	}
}

// Close stops the listener and tears down every live carrier.
func (l *Listener) Close() error {
	l.closed.Do(func() {
		close(l.done)
		l.conn.close()
		// Collect the carriers under the lock, then close them WITHOUT holding
		// it: Conn.Close calls back into onClose, which locks l.mu.
		l.mu.Lock()
		conns := l.conns
		l.conns = map[string]*peerLink{}
		l.mu.Unlock()
		for _, pl := range conns {
			pl.c.Close()
		}
	})
	l.wg.Wait()
	return nil
}
