package reality

import (
	"crypto/tls"
	"net"

	utls "github.com/refraction-networking/utls"
)

// This file completes Path A: after the signal is verified, BOTH sides run a
// real TLS 1.3 handshake and then send hs2 frames DIRECTLY inside the TLS
// records. There is no second encryption layer — security comes from TLS
// itself. This is what avoids the TLS-in-TLS fingerprint that makes nested
// tunnels detectable: on the wire there is exactly one layer of TLS, the same
// as an ordinary HTTPS visit.
//
// prefixConn replays bytes already read from the socket (the ClientHello the
// dispatcher peeked) back into the TLS state machine, so the standard TLS
// server sees the handshake from its first byte.
type prefixConn struct {
	net.Conn
	head []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.head) > 0 {
		n := copy(b, p.head)
		p.head = p.head[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// ServerTLS wraps an authorised connection in a TLS server handshake using the
// cover certificate, and returns the established tls.Conn. hs2 frames then flow
// inside it with no further encryption.
//
// alreadyRead is the ClientHello bytes the dispatcher consumed; they are
// replayed so the handshake completes. cert should be for the SNI the client
// signalled (the cover domain), so the certificate the client sees matches the
// name it asked for — exactly as the real site would answer.
func ServerTLS(conn net.Conn, alreadyRead []byte, cert tls.Certificate) (*tls.Conn, error) {
	tconn := tls.Server(&prefixConn{Conn: conn, head: alreadyRead}, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tconn.Handshake(); err != nil {
		return nil, err
	}
	return tconn, nil
}

// ClientTLS completes the client side of the TLS handshake on a connection that
// has already had its signalled ClientHello sent by DialSignalled. Because
// DialSignalled sent a genuine uTLS ClientHello, we continue with uTLS so the
// rest of the handshake keeps the Chrome fingerprint.
//
// NOTE: DialSignalled currently sends the ClientHello and returns the raw conn.
// To complete the handshake we need uTLS to own the whole flow. ClientDialTLS
// below does both in one shot and is what callers should use.
func clientHelloRecord(hello []byte) []byte {
	rec := make([]byte, 5+len(hello))
	rec[0], rec[1], rec[2] = 0x16, 0x03, 0x01
	rec[3], rec[4] = byte(len(hello)>>8), byte(len(hello))
	copy(rec[5:], hello)
	return rec
}

// ClientDialTLS dials, sends a genuine Chrome ClientHello whose session-id
// carries the signal, and completes the full uTLS handshake. It returns the
// established uTLS connection; hs2 frames flow inside it. This supersedes
// DialSignalled for real use (DialSignalled remains for signal-only tests).
func ClientDialTLS(addr, sni string, sharedKey []byte) (*utls.UConn, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(conn, &utls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, // cover cert won't chain; we authenticate via the signal
	}, utls.HelloChrome_120)
	if err := uconn.BuildHandshakeState(); err != nil {
		conn.Close()
		return nil, err
	}
	cr := uconn.HandshakeState.Hello.Random
	nonce := randomNonce()
	uconn.HandshakeState.Hello.SessionId = makeSessionID(sharedKey, cr, nonce)
	if err := uconn.MarshalClientHello(); err != nil {
		conn.Close()
		return nil, err
	}
	// Handshake() will send the (now signalled) ClientHello and finish TLS.
	if err := uconn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return uconn, nil
}
