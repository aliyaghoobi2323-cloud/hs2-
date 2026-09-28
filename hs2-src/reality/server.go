package reality

import (
	"context"
	"io"
	"net"
	"time"
)

// Dispatcher is the probe-resistant front door. Every accepted connection is
// peeked at: if it carries a valid auth tag for a known key, it is a real
// client and becomes a tunnel; otherwise it is indistinguishable from a browser
// or a probe and is spliced to the cover site, byte for byte, so the peer sees
// exactly what the cover site would send.
//
// The peek is the security-critical step. It must:
//   - never block forever (a probe that sends nothing must still be forwarded,
//     because a real TLS client that stalls would be too),
//   - never reveal via timing or response whether the key check ran,
//   - forward the ALREADY-READ bytes to the cover site, or the splice would
//     drop the client's ClientHello and the cover site would see a broken flow.
type Dispatcher struct {
	SharedKey []byte
	CoverAddr string // host:port of the real site probes are forwarded to
	OnTunnel  func(conn net.Conn, clientHello []byte, clientRandom []byte)
	Log       func(string, ...any)
}

// tagOffset/tagLen: where in the client's first record we look for the tag. In
// a full Reality this is hidden inside real TLS extension fields; for this
// adversary-testable core the client places [clientRandom(32)][tag(16)] as the
// very first bytes, then speaks the tunnel. A prober/browser never sends this
// shape, so it fails the check and is forwarded. (The uTLS-embedded placement
// is a later refinement; the DISPATCH LOGIC is what we are proving here.)
const peekWait = 5 * time.Second

func (d *Dispatcher) log(f string, a ...any) {
	if d.Log != nil {
		d.Log(f, a...)
	}
}

// Handle takes one accepted connection and routes it.
func (d *Dispatcher) Handle(ctx context.Context, conn net.Conn) {
	// Read the first TLS record (the ClientHello). One short deadline; a peer
	// that stalls is forwarded like any slow client — no distinctive hang.
	conn.SetReadDeadline(time.Now().Add(peekWait))
	buf := make([]byte, 2048)
	n, err := readClientHelloBytes(conn, buf)
	conn.SetReadDeadline(time.Time{})
	if err != nil || n == 0 {
		d.forward(ctx, conn, buf[:n])
		return
	}
	ch, perr := parseClientHello(buf[:n])
	if perr != nil || !verifySessionID(d.SharedKey, ch.clientRandom, ch.sessionID) {
		// A browser, a scanner, or the censor's probe: an ordinary ClientHello
		// whose session-id does not verify. Forward the WHOLE thing to the cover
		// site so it completes a real TLS handshake and gets a real response.
		d.forward(ctx, conn, buf[:n])
		return
	}
	d.log("reality: authorised client (signalled ClientHello)")
	if d.OnTunnel != nil {
		// Hand over the connection and the ClientHello bytes we consumed, so the
		// tunnel can replay them into a real TLS server handshake (Path A).
		d.OnTunnel(conn, ch.raw, ch.clientRandom)
	}
}

// readClientHelloBytes reads exactly one TLS record: the 5-byte header tells us
// the length, then we read that many more. This returns the complete
// ClientHello (or whatever arrived, for forwarding) without over-reading into
// the next record.
func readClientHelloBytes(conn net.Conn, buf []byte) (int, error) {
	if _, err := io.ReadFull(conn, buf[:5]); err != nil {
		// too short to be a TLS record; return what we have for forwarding
		n, _ := conn.Read(buf[:])
		return n, nil
	}
	if buf[0] != 0x16 { // not a handshake record: forward as-is
		return 5, nil
	}
	recLen := int(buf[3])<<8 | int(buf[4])
	if 5+recLen > len(buf) {
		recLen = len(buf) - 5
	}
	m, err := io.ReadFull(conn, buf[5:5+recLen])
	return 5 + m, err
}

// forward splices conn to the cover site, replaying the bytes already read so
// the cover site sees the client's request intact. The peer thus receives a
// genuine response from a genuine site.
func (d *Dispatcher) forward(ctx context.Context, conn net.Conn, already []byte) {
	defer conn.Close()
	up, err := net.DialTimeout("tcp", d.CoverAddr, 8*time.Second)
	if err != nil {
		d.log("reality: cover dial failed: %v", err)
		return
	}
	defer up.Close()
	if len(already) > 0 {
		if _, err := up.Write(already); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, up); done <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-done:
	}
}
