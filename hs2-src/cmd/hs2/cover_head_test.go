package main

import (
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// oneShot sends b, then reads until close or the deadline; it reports what
// came back and whether the server closed.
func oneShot(t *testing.T, addr string, b []byte, wait time.Duration) (string, bool) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(b)
	c.SetReadDeadline(time.Now().Add(wait))
	all, err := io.ReadAll(c)
	closed := err == nil
	return string(all), closed
}

func is400(s, detail string) bool {
	return strings.HasPrefix(s, "HTTP/1.1 400 Bad Request\r\nServer: nginx\r\n") &&
		strings.Contains(s, "Connection: close\r\n") &&
		strings.HasSuffix(s, tlscarrier.NginxErrorPage(400, detail))
}

// Random bytes — with or without a newline — get nginx's 400 at once and a
// close, never silence (the probe saw silence for blobs without a '\n').
func TestNginxCoverRandomBytes400(t *testing.T) {
	addr, _, _ := startNginxCover(t)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 40; i++ {
		b := make([]byte, 300)
		rng.Read(b)
		if i%2 == 0 {
			b = bytes.ReplaceAll(b, []byte{'\n'}, []byte{'x'})
		}
		if b[0] >= 'A' && b[0] <= 'Z' || b[0] == '\r' || b[0] == '\n' || b[0] == '_' || b[0] == '-' {
			b[0] = 0xff // start-of-method cases are covered below
		}
		got, closed := oneShot(t, addr, b, 2*time.Second)
		if !is400(got, "") || !closed {
			t.Fatalf("blob %d: closed=%v %q", i, closed, got)
		}
	}
	for _, p := range []string{"GET\x01", "GET /\x00", "GET / HTTQ", "G@T / HTTP/1.1\r\n", "GET / HTTP/1.1 x\r\n", "get / HTTP/1.1\r\n", "GET  ?x"} {
		got, closed := oneShot(t, addr, []byte(p), 2*time.Second)
		if !is400(got, "") || !closed {
			t.Fatalf("%q: closed=%v %q", p, closed, got)
		}
	}
}

// A valid partial request line is waited for, as nginx does (no answer).
func TestNginxCoverPartialWaits(t *testing.T) {
	addr, _, _ := startNginxCover(t)
	for _, p := range []string{"GET", "GET /inde", "GET / HTTP/1.", "GET / HTTP/1.1\r\nHost: a\r\n"} {
		got, closed := oneShot(t, addr, []byte(p), 300*time.Millisecond)
		if got != "" || closed {
			t.Fatalf("%q answered: closed=%v %q", p, closed, got)
		}
	}
}

func TestNginxCoverOversized(t *testing.T) {
	addr, _, _ := startNginxCover(t)
	got, closed := oneShot(t, addr, []byte("GET /"+strings.Repeat("a", 9000)+" HTTP/1.1\r\n\r\n"), 2*time.Second)
	if !strings.HasPrefix(got, "HTTP/1.1 414 Request-URI Too Large\r\nServer: nginx\r\n") || !closed {
		t.Fatalf("long URI: %v %.80q", closed, got)
	}
	got, closed = oneShot(t, addr, []byte("GET / HTTP/1.1\r\nHost: a\r\nX-A: "+strings.Repeat("b", 9000)+"\r\n\r\n"), 2*time.Second)
	if !is400(got, "Request Header Or Cookie Too Large") || !closed {
		t.Fatalf("long header: %v %.80q", closed, got)
	}
	var many strings.Builder
	many.WriteString("GET / HTTP/1.1\r\nHost: a\r\n")
	for i := 0; i < 10; i++ {
		many.WriteString("X-B: " + strings.Repeat("c", 4000) + "\r\n")
	}
	many.WriteString("\r\n")
	got, closed = oneShot(t, addr, []byte(many.String()), 2*time.Second)
	if !is400(got, "Request Header Or Cookie Too Large") || !closed {
		t.Fatalf("big head: %v %.80q", closed, got)
	}
	// a normal large-ish head still works
	got, _ = oneShot(t, addr, []byte("GET / HTTP/1.1\r\nHost: a\r\nCookie: "+strings.Repeat("d", 6000)+"\r\nConnection: close\r\n\r\n"), 2*time.Second)
	if !strings.HasPrefix(got, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("6k cookie: %.80q", got)
	}
}

func TestNginxCoverRobotsFavicon(t *testing.T) {
	addr, page, _ := startNginxCover(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	head, body := rawExchange(t, c, br, "GET /robots.txt HTTP/1.1\r\nHost: a\r\n\r\n")
	if head[0] != "HTTP/1.1 200 OK" || !strings.Contains(strings.Join(head, "\n"), "Content-Type: text/plain") || !strings.HasPrefix(strings.TrimPrefix(body, "# robots.txt\n"), "User-agent: *\n") {
		t.Fatalf("robots: %q %q", head, body)
	}
	head, body = rawExchange(t, c, br, "GET /favicon.ico HTTP/1.1\r\nHost: a\r\n\r\n")
	if head[0] != "HTTP/1.1 200 OK" || !strings.Contains(strings.Join(head, "\n"), "Content-Type: image/x-icon") || !strings.HasPrefix(body, "\x00\x00\x01\x00\x01\x00\x10\x10") || len(body) != 6+16+40+1024+64 {
		t.Fatalf("favicon: %q len %d", head, len(body))
	}
	if !coverHSL.Match(page) {
		t.Fatal("page favicon colour not found")
	}
	if bytes.Equal(coverFaviconICO(page), coverFaviconICO([]byte("x"))) {
		t.Fatal("favicon not page-derived")
	}
}
