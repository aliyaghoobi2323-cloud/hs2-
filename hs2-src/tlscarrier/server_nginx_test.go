package tlscarrier

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

func withCoverNginx(t *testing.T) {
	old := CoverNginx
	CoverNginx = true
	t.Cleanup(func() { CoverNginx = old })
}

// The nginx-style idle handling (60 s, quiet close) and quiet forward close
// only touch connections that never authenticated: an authorised link is
// unaffected — it reaches the tunnel and can then stay idle past any probe
// timer without being closed.
func TestCoverNginxTunnelUnaffected(t *testing.T) {
	withCoverNginx(t)
	if firstReadTimeout() != 60*time.Second {
		t.Fatalf("firstReadTimeout %v", firstReadTimeout())
	}
	key := bytes.Repeat([]byte{0x22}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	got := make(chan *Carrier, 1)
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { got <- c })
	defer stop()
	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	var sc *Carrier
	select {
	case sc = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("client never reached tunnel")
	}
	defer sc.Close()
	time.Sleep(1500 * time.Millisecond) // idle after auth: nothing closes it
	car.SendFrame(1, []byte("after-idle"))
	if ft, p, err := sc.ReadFrame(); err != nil || ft != 1 || string(p) != "after-idle" {
		t.Fatalf("after idle: %d %q %v", ft, p, err)
	}
	sc.SendFrame(1, []byte("back"))
	if ft, p, err := car.ReadFrame(); err != nil || ft != 1 || string(p) != "back" {
		t.Fatalf("back: %d %q %v", ft, p, err)
	}
}

// readCountConn counts the raw bytes read once armed.
type readCountConn struct {
	net.Conn
	armed bool
	n     int
}

func (c *readCountConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.armed {
		c.n += n
	}
	return n, err
}

// A cover that closes without a word yields a quiet close (no close_notify
// alert) — nginx's shutdown on a header timeout; a cover that answers is
// followed by the usual close_notify. TLS 1.2 so nothing else (tickets)
// arrives after the handshake: the raw bytes after it are only the alert.
func TestCoverNginxQuietCloseOnSilentBackend(t *testing.T) {
	withCoverNginx(t)
	for _, answer := range []bool{false, true} {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					b := make([]byte, 64)
					c.Read(b)
					time.Sleep(200 * time.Millisecond)
					if answer {
						c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
					}
					c.Close()
				}()
			}
		}()
		key := bytes.Repeat([]byte{0x23}, 32)
		addr, stop := startServer(t, key, testCert(t, "vpn.example.com"), ln.Addr().String(), func(c *Carrier) { c.Close() })
		raw, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		cc := &readCountConn{Conn: raw}
		tc := tls.Client(cc, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			t.Fatal(err)
		}
		cc.armed = true
		tc.Write([]byte("GET / HTT"))
		tc.SetReadDeadline(time.Now().Add(3 * time.Second))
		all, err := io.ReadAll(tc)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("answer=%v: not closed", answer)
		}
		alertBytes := cc.n - len(all)
		if answer && (string(all) != "HTTP/1.1 400 Bad Request\r\n\r\n" || alertBytes <= 0) {
			t.Fatalf("answered: %q, %d bytes beyond it (want a close_notify)", all, alertBytes)
		}
		if !answer && cc.n != 0 {
			t.Fatalf("silent: %d raw bytes after the handshake (want a quiet close)", cc.n)
		}
		tc.Close()
		stop()
		ln.Close()
	}
}
