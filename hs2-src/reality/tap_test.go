package reality

import (
	"net"
	"sync"
	"testing"

	utls "github.com/refraction-networking/utls"
)

type tapConn struct {
	net.Conn
	mu       sync.Mutex
	written  []byte
	hsWrites int
}

func (c *tapConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, b...)
	c.mu.Unlock()
	return c.Conn.Write(b)
}

// postHandshakeRecordTypes walks the captured client->server bytes as TLS
// records and returns the record-type byte of records after the first
// application-data record boundary. Simplified: returns types of ALL records
// whose type is app-data or handshake, skipping the initial ClientHello.
func (c *tapConn) postHandshakeRecordTypes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var types []byte
	b := c.written
	first := true
	for len(b) >= 5 {
		rt := b[0]
		ln := int(b[3])<<8 | int(b[4])
		if 5+ln > len(b) {
			break
		}
		if first {
			// skip the ClientHello (handshake record)
			first = false
		} else {
			types = append(types, rt)
		}
		b = b[5+ln:]
	}
	return types
}

func utlsClientOver(conn net.Conn, sni string, key []byte, t *testing.T) *utls.UConn {
	u := utls.UClient(conn, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, utls.HelloChrome_120)
	if err := u.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	cr := u.HandshakeState.Hello.Random
	u.HandshakeState.Hello.SessionId = makeSessionID(key, cr, randomNonce())
	if err := u.MarshalClientHello(); err != nil {
		t.Fatal(err)
	}
	if err := u.Handshake(); err != nil {
		t.Fatal(err)
	}
	return u
}
