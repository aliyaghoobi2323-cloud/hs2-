package reality

import (
	"crypto/rand"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// DialSignalled opens a TCP connection to the reality front door and sends a
// GENUINE Chrome ClientHello whose session-id carries the auth signal. Because
// the ClientHello is built by uTLS, its fingerprint (JA3/JA4, extension order,
// cipher list) is Chrome's, not Go's — removing the TLS-library fingerprint the
// GFW uses to spot proxies. The signal rides in a field that is a random blob
// in an ordinary handshake, so on the wire this is just Chrome visiting a site.
//
// This returns the raw net.Conn positioned right after the ClientHello has been
// sent. v0.2 uses this to prove the signal is accepted; completing the full TLS
// handshake and running hs2 frames inside the TLS session is the next step.
func DialSignalled(addr, sni string, sharedKey []byte) (net.Conn, []byte, error) {
	conn, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		return nil, nil, err
	}
	// Build a Chrome ClientHello with uTLS, but override the session-id with our
	// signal. We first let uTLS generate its client_random, then compute the
	// session-id bound to that random.
	uconn := utls.UClient(conn, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, utls.HelloChrome_120)
	if err := uconn.BuildHandshakeState(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	cr := uconn.HandshakeState.Hello.Random // 32 bytes uTLS chose
	nonce := make([]byte, 16)
	rand.Read(nonce)
	// cr is the client_random uTLS has already fixed in HandshakeState. Do NOT
	// call BuildHandshakeState again — it would regenerate the random and
	// invalidate the tag we bind to it. Compute the signal from THIS random,
	// set the session-id, and marshal exactly once.
	sid := makeSessionID(sharedKey, cr, nonce)
	uconn.HandshakeState.Hello.SessionId = sid
	if err := uconn.MarshalClientHello(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	// Hello.Raw is the bare handshake message (starts 0x01). A real TLS client
	// wraps it in a record-layer header [0x16][version][len] before sending.
	// Wrap it so what goes on the wire is a genuine TLS record, byte-identical
	// in structure to a browser's.
	hello := uconn.HandshakeState.Hello.Raw
	rec := make([]byte, 5+len(hello))
	rec[0] = 0x16               // handshake
	rec[1], rec[2] = 0x03, 0x01 // TLS record version (1.0), as real clients send
	rec[3] = byte(len(hello) >> 8)
	rec[4] = byte(len(hello))
	copy(rec[5:], hello)
	if _, err := conn.Write(rec); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, cr, nil
}
