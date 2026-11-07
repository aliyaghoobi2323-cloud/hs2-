//go:build linux

package engine

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// acmeHarness runs a listener whose connections go through acmeServeUser; the
// "normal path" records every byte it receives and the conn type it got.
type acmeHarness struct {
	ln     net.Listener
	normal chan acmeNormal
}

type acmeNormal struct {
	data  []byte
	isTCP bool
	at    time.Time
}

func newACMEHarness(t *testing.T, a *ACMEResponder, port int) *acmeHarness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &acmeHarness{ln: ln, normal: make(chan acmeNormal, 16)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go acmeServeUser(a, c, port, func(c net.Conn) {
				_, isTCP := c.(*net.TCPConn)
				at := time.Now()
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				b, _ := io.ReadAll(c)
				c.Close()
				h.normal <- acmeNormal{data: b, isTCP: isTCP, at: at}
			})
		}
	}()
	return h
}

// roundTrip sends payload, half-closes, and returns what the server answered.
func (h *acmeHarness) roundTrip(t *testing.T, payload []byte) []byte {
	t.Helper()
	c, err := net.Dial("tcp", h.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(payload)
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(c)
	return b
}

func (h *acmeHarness) expectNormal(t *testing.T, want []byte) acmeNormal {
	t.Helper()
	select {
	case n := <-h.normal:
		if !bytes.Equal(n.data, want) {
			t.Fatalf("normal path got %q, want %q", n.data, want)
		}
		if !n.isTCP {
			t.Fatalf("normal path got a wrapped conn, want the untouched *net.TCPConn")
		}
		return n
	case <-time.After(5 * time.Second):
		t.Fatalf("normal path never got the connection (want %q)", want)
	}
	return acmeNormal{}
}

func (h *acmeHarness) expectNoNormal(t *testing.T) {
	t.Helper()
	select {
	case n := <-h.normal:
		t.Fatalf("connection went to the normal path with %q", n.data)
	case <-time.After(100 * time.Millisecond):
	}
}

func acmeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "Tok_en-123"), []byte("Tok_en-123.thumbprint"), 0o644)
	os.WriteFile(filepath.Join(root, "secret"), []byte("SECRET"), 0o644)
	os.Symlink(filepath.Join(root, "secret"), filepath.Join(dir, "link"))
	return root
}

func TestACMEAnswersChallenge(t *testing.T) {
	h := newACMEHarness(t, &ACMEResponder{Webroot: acmeRoot(t), Ports: map[int]bool{80: true}}, 80)
	for _, ver := range []string{"1.1", "1.0"} {
		resp := h.roundTrip(t, []byte("GET /.well-known/acme-challenge/Tok_en-123 HTTP/"+ver+"\r\nHost: t1.example.com\r\nUser-Agent: Mozilla/5.0 (compatible; Let's Encrypt validation server)\r\nAccept: */*\r\n\r\n"))
		s := string(resp)
		if !strings.HasPrefix(s, "HTTP/1.1 200 OK\r\n") || !strings.HasSuffix(s, "\r\n\r\nTok_en-123.thumbprint") ||
			!strings.Contains(s, "Content-Length: 21\r\n") {
			t.Fatalf("HTTP/%s: response %q", ver, s)
		}
		h.expectNoNormal(t)
	}
}

func TestACMEUnsafeOrMissingIs404(t *testing.T) {
	h := newACMEHarness(t, &ACMEResponder{Webroot: acmeRoot(t), Ports: map[int]bool{80: true}}, 80)
	for _, tok := range []string{"../secret", "..%2fsecret", "a.b", "link", "missing", "Tok_en-123/", "x?y", "Tok en", strings.Repeat("a", 256)} {
		resp := h.roundTrip(t, []byte("GET /.well-known/acme-challenge/"+tok+" HTTP/1.1\r\nHost: x\r\n\r\n"))
		if !strings.HasPrefix(string(resp), "HTTP/1.1 404 ") || strings.Contains(string(resp), "SECRET") {
			t.Fatalf("token %q: response %q", tok, resp)
		}
		h.expectNoNormal(t)
	}
}

func TestACMEOtherTrafficForwardedExactly(t *testing.T) {
	h := newACMEHarness(t, &ACMEResponder{Webroot: acmeRoot(t), Ports: map[int]bool{80: true}}, 80)
	rnd := make([]byte, 4096)
	for i := range rnd {
		rnd[i] = byte(i*7 + i>>3)
	}
	cases := [][]byte{
		[]byte("GET /index.html HTTP/1.1\r\nHost: a\r\n\r\n"),
		[]byte("POST /.well-known/acme-challenge/Tok_en-123 HTTP/1.1\r\n\r\n"),
		[]byte("HEAD /.well-known/acme-challenge/Tok_en-123 HTTP/1.1\r\n\r\n"),
		[]byte("GET /.well-known/acme-challenge/Tok_en-123 HTTP/2.0\r\n\r\n"),
		[]byte("GET /.well-known/acme-challengeX/Tok HTTP/1.1\r\n\r\n"),
		[]byte("G"), // a lone G then EOF
		{0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'},
		rnd,
		append([]byte("GET /.well-known/acme-challenge/"), bytes.Repeat([]byte("a"), 600)...), // over the line cap
	}
	for _, p := range cases {
		resp := h.roundTrip(t, p)
		if len(resp) != 0 {
			t.Fatalf("%q: server answered %q itself", p[:min(len(p), 40)], resp)
		}
		h.expectNormal(t, p)
	}
}

func TestACMEOffIsInert(t *testing.T) {
	root := acmeRoot(t)
	req := []byte("GET /.well-known/acme-challenge/Tok_en-123 HTTP/1.1\r\nHost: x\r\n\r\n")
	for name, a := range map[string]*ACMEResponder{
		"nil":        nil,
		"no webroot": {Ports: map[int]bool{80: true}},
		"other port": {Webroot: root, Ports: map[int]bool{8080: true}},
	} {
		h := newACMEHarness(t, a, 80)
		if resp := h.roundTrip(t, req); len(resp) != 0 {
			t.Fatalf("%s: answered %q", name, resp)
		}
		h.expectNormal(t, req)
	}
}

// A client that sends only "G" and waits goes to the normal path after the
// short sniff timeout, with its byte intact; meanwhile other connections are
// served at once.
func TestACMESlowClientDoesNotBlock(t *testing.T) {
	old := ACMESniffTimeout
	ACMESniffTimeout = 300 * time.Millisecond
	defer func() { ACMESniffTimeout = old }()
	h := newACMEHarness(t, &ACMEResponder{Webroot: acmeRoot(t), Ports: map[int]bool{80: true}}, 80)

	slow, err := net.Dial("tcp", h.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	start := time.Now()
	slow.Write([]byte("G"))

	// Others are not held up by the slow one.
	t0 := time.Now()
	resp := h.roundTrip(t, []byte("GET /.well-known/acme-challenge/Tok_en-123 HTTP/1.1\r\n\r\n"))
	if !strings.HasSuffix(string(resp), "Tok_en-123.thumbprint") || time.Since(t0) > 200*time.Millisecond {
		t.Fatalf("challenge behind a slow client: %q after %v", resp, time.Since(t0))
	}
	other := []byte("SSH-2.0-x\r\n")
	t1 := time.Now()
	h.roundTrip(t, other)
	n := h.expectNormal(t, other)
	if d := n.at.Sub(t1); d > 200*time.Millisecond {
		t.Fatalf("non-ACME connection waited %v behind a slow client", d)
	}

	// The slow one reaches the normal path after the timeout, then the rest
	// of its bytes follow.
	time.Sleep(450 * time.Millisecond)
	slow.Write([]byte("ET /x HTTP/1.1\r\n\r\n"))
	slow.(*net.TCPConn).CloseWrite()
	n = h.expectNormal(t, []byte("GET /x HTTP/1.1\r\n\r\n"))
	if d := n.at.Sub(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("slow client reached the normal path after %v, want ~300ms", d)
	}
}

// A non-G first byte is decided without waiting for more.
func TestACMEFastPathNoDelay(t *testing.T) {
	h := newACMEHarness(t, &ACMEResponder{Webroot: acmeRoot(t), Ports: map[int]bool{80: true}}, 80)
	c, err := net.Dial("tcp", h.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t0 := time.Now()
	c.Write([]byte("X")) // no close: the normal path starts reading at once
	c.(*net.TCPConn).CloseWrite()
	n := h.expectNormal(t, []byte("X"))
	if d := n.at.Sub(t0); d > 100*time.Millisecond {
		t.Fatalf("non-G connection delayed %v", d)
	}
}

func TestACMEClassify(t *testing.T) {
	for _, c := range []struct {
		in   string
		want acmeMatchState
	}{
		{"", acmeNeedMore}, {"G", acmeNeedMore}, {"GET /.well-known/", acmeNeedMore},
		{"GET /.well-known/acme-challenge/abc", acmeNeedMore},
		{"GET /.well-known/acme-challenge/abc HTTP/1.1\r", acmeNeedMore},
		{"GET /.well-known/acme-challenge/abc HTTP/1.1\r\n", acmeLine},
		{"X", acmeNo}, {"GE ", acmeNo}, {"GET /.well-known/acme-challenge/a\tb", acmeNo},
		{"GET /.well-known/acme-challenge/a\rb", acmeNo},
	} {
		if got, _ := acmeClassify([]byte(c.in)); got != c.want {
			t.Errorf("%q: %v, want %v", c.in, got, c.want)
		}
	}
}
