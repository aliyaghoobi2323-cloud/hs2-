package udpcarrier

import (
	"bytes"
	"context"
	"crypto/subtle"
	"net"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// Listener is the responder side of the UDP carrier. It owns one shared
// net.UDPConn and demultiplexes datagrams by source address: a datagram from a
// known peer is a data/control shard for that peer's Conn; a datagram from a
// new address is treated as a Noise message 1 and, if it authenticates AND
// carries the pinned initiator static key, starts a new carrier. Anything that
// fails is dropped silently, so a probe with a random or stale first message
// gets no reply — the same probe resistance the TCP carriers have.
type Listener struct {
	conn      *net.UDPConn
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
}

// Listen binds a UDP socket and serves carriers derived from the shared secret.
func Listen(addr string, shared []byte, innerMTU int) (*Listener, error) {
	if innerMTU <= 0 {
		innerMTU = DefaultInnerMTU
	}
	server, client, err := staticPair(shared)
	if err != nil {
		return nil, err
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	l := &Listener{
		conn:      conn,
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
func (l *Listener) LocalAddr() net.Addr { return l.conn.LocalAddr() }

func (l *Listener) serve() {
	defer l.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			// transient read error: keep serving
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
				l.conn.WriteToUDP(pl.m2, addr)
			} else {
				pl.c.feed(pkt)
			}
			continue
		}
		// A datagram from a new address is a probe or a handshake. Probes are
		// answered on this same port so the selector can measure the real data
		// path; anything else is a candidate first message.
		if l.probe.handle(pkt, func(b []byte) { l.conn.WriteToUDP(b, addr) }) {
			continue
		}
		l.tryHandshake(pkt, addr)
	}
}

// tryHandshake runs the responder handshake for a datagram from a new address.
func (l *Listener) tryHandshake(m1 []byte, addr *net.UDPAddr) {
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
	if _, err := l.conn.WriteToUDP(m2, addr); err != nil {
		return
	}
	sess, err := core.NewSession(secret, false, randID())
	if err != nil {
		return
	}
	binding := core.HandshakeBinding(hs)

	write := func(b []byte) error { _, e := l.conn.WriteToUDP(b, addr); return e }
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
	l.conns[key] = &peerLink{c: c, m1: m1, m2: m2}
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
		l.conn.Close()
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
