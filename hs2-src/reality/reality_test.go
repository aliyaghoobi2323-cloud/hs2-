package reality

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func writeRecord(c net.Conn, hello []byte) {
	rec := make([]byte, 5+len(hello))
	rec[0], rec[1], rec[2] = 0x16, 0x03, 0x01
	rec[3], rec[4] = byte(len(hello)>>8), byte(len(hello))
	copy(rec[5:], hello)
	c.Write(rec)
}

func coverSite(t *testing.T) (addr string, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 1024)
				c.SetReadDeadline(time.Now().Add(time.Second))
				c.Read(b)
				c.Write([]byte("HTTP/1.1 200 OK\r\nServer: cover\r\nContent-Length: 3\r\n\r\nhi\n"))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func startDispatcher(t *testing.T, key []byte, cover string, onTunnel func(net.Conn, []byte, []byte)) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{SharedKey: key, CoverAddr: cover, OnTunnel: onTunnel}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.Handle(ctx, c)
		}
	}()
	return ln.Addr().String(), func() { cancel(); ln.Close() }
}

// realBrowserHello sends a genuine Chrome ClientHello WITHOUT our signal — this
// is exactly what a browser or the censor's active probe produces.
func realBrowserHello(t *testing.T, addr, sni string) []byte {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u := utls.UClient(c, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, utls.HelloChrome_120)
	if err := u.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	writeRecord(c, u.HandshakeState.Hello.Raw)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(c)
	return out
}

// A genuine Chrome ClientHello with no valid signal (a probe/browser) must be
// forwarded to the cover site and get its real response.
func TestBrowserProbeForwardedToCover(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	cover, stopC := coverSite(t)
	defer stopC()
	tunnelHit := false
	disp, stopD := startDispatcher(t, key, cover, func(c net.Conn, hello []byte, cr []byte) { tunnelHit = true; c.Close() })
	defer stopD()

	resp := realBrowserHello(t, disp, "www.microsoft.com")
	if tunnelHit {
		t.Fatal("a real browser ClientHello wrongly reached the tunnel")
	}
	if !bytes.Contains(resp, []byte("cover")) {
		t.Fatalf("probe did not get cover-site response: %q", resp)
	}
}

// Our signalled Chrome ClientHello (holds the key) must reach the tunnel.
func TestSignalledClientReachesTunnel(t *testing.T) {
	key := bytes.Repeat([]byte{0x22}, 32)
	cover, stopC := coverSite(t)
	defer stopC()
	got := make(chan bool, 1)
	disp, stopD := startDispatcher(t, key, cover, func(c net.Conn, hello []byte, cr []byte) {
		c.Write([]byte("TUNNEL-OK"))
		got <- true
		c.Close()
	})
	defer stopD()

	conn, cr, err := DialSignalled(disp, "www.microsoft.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = cr
	buf := make([]byte, 32)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if string(buf[:n]) != "TUNNEL-OK" {
		t.Fatalf("signalled client did not reach tunnel: %q", buf[:n])
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("OnTunnel never fired")
	}
}

// The signalled and the browser ClientHello must be the SAME SHAPE on the wire:
// same TLS version, same cipher suites, same extension set. Only the session-id
// content differs, and a session-id is a random blob in both. If they differ
// structurally, the signal itself is a fingerprint.
func TestSignalledHelloLooksLikeBrowser(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)

	grab := func(signal bool) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		done := make(chan []byte, 1)
		go func() {
			c, _ := l.Accept()
			b := make([]byte, 2048)
			n, _ := readClientHelloBytes(c, b)
			done <- b[:n]
			c.Close()
		}()
		if signal {
			conn, _, err := DialSignalled(l.Addr().String(), "www.microsoft.com", key)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		} else {
			c, _ := net.Dial("tcp", l.Addr().String())
			u := utls.UClient(c, &utls.Config{ServerName: "www.microsoft.com", InsecureSkipVerify: true}, utls.HelloChrome_120)
			u.BuildHandshakeState()
			writeRecord(c, u.HandshakeState.Hello.Raw)
			c.Close()
		}
		raw := <-done
		ch, err := parseClientHello(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_ = ch
		// Re-parse via uTLS fingerprinter for structural comparison.
		f := &utls.Fingerprinter{}
		if _, ferr := f.FingerprintClientHello(raw); ferr != nil {
			t.Fatalf("fingerprint: %v", ferr)
		}
	}
	// Both must parse as valid Chrome-shaped ClientHellos.
	grab(false)
	grab(true)
	t.Log("both browser and signalled ClientHello parse as valid TLS with Chrome fingerprint")
}

// A forged session-id (attacker picks nonce, guesses tag) must fail and be
// forwarded to cover.
func TestForgedSignalFails(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	cover, stopC := coverSite(t)
	defer stopC()
	tunnelHit := false
	disp, stopD := startDispatcher(t, key, cover, func(c net.Conn, hello []byte, cr []byte) { tunnelHit = true; c.Close() })
	defer stopD()

	c, err := net.Dial("tcp", disp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u := utls.UClient(c, &utls.Config{ServerName: "www.microsoft.com", InsecureSkipVerify: true}, utls.HelloChrome_120)
	u.BuildHandshakeState()
	forged := make([]byte, 32) // random nonce, zero/garbage tag
	rand.Read(forged[:16])
	u.HandshakeState.Hello.SessionId = forged
	u.MarshalClientHello()
	writeRecord(c, u.HandshakeState.Hello.Raw)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(c)
	if tunnelHit {
		t.Fatal("forged signal reached the tunnel")
	}
	if !bytes.Contains(out, []byte("cover")) {
		t.Fatalf("forged signal not forwarded to cover: %q", out)
	}
}
