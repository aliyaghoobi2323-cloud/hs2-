package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func startNginxCover(t *testing.T) (string, []byte, time.Time) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	page, _ := buildCover("seed-x", time.Now())
	mod := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	go serveNginxCover(ln, page, mod)
	return ln.Addr().String(), page, mod
}

// rawExchange sends req and reads one response's head and body.
func rawExchange(t *testing.T, c net.Conn, br *bufio.Reader, req string) (head []string, body string) {
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n := 0
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading head: %v (%q)", err, head)
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		head = append(head, l)
		if strings.HasPrefix(l, "Content-Length: ") {
			for _, ch := range l[len("Content-Length: "):] {
				n = n*10 + int(ch-'0')
			}
		}
	}
	if !strings.HasPrefix(req, "HEAD") && !strings.Contains(head[0], " 304 ") {
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			t.Fatal(err)
		}
		body = string(b)
	}
	return head, body
}

func names(head []string) string {
	var out []string
	for _, h := range head[1:] {
		out = append(out, h[:strings.Index(h, ":")])
	}
	return strings.Join(out, ",")
}

func TestNginxCover(t *testing.T) {
	addr, page, mod := startNginxCover(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	etag := nginxETag(mod, len(page))

	head, body := rawExchange(t, c, br, "GET / HTTP/1.1\r\nHost: a\r\n\r\n")
	if head[0] != "HTTP/1.1 200 OK" || head[1] != "Server: nginx" || body != string(page) {
		t.Fatalf("GET /: %q", head)
	}
	if got := names(head); got != "Server,Date,Content-Type,Content-Length,Last-Modified,Connection,ETag,Expires,Cache-Control,Accept-Ranges" {
		t.Fatalf("header order %s", got)
	}
	for _, want := range []string{"Content-Type: text/html", "Connection: keep-alive", "ETag: " + etag, "Last-Modified: " + mod.UTC().Format(http.TimeFormat)} {
		if !strings.Contains(strings.Join(head, "\n"), want) {
			t.Fatalf("missing %q in %q", want, head)
		}
	}
	// same connection (keep-alive): 404, 304, 206, HEAD, 405
	head, body = rawExchange(t, c, br, "GET /nothing-here.txt HTTP/1.1\r\nHost: a\r\n\r\n")
	if head[0] != "HTTP/1.1 404 Not Found" || body != tlscarrier.NginxErrorPage(404, "") || names(head) != "Server,Date,Content-Type,Content-Length,Connection" {
		t.Fatalf("404: %q %q", head, body)
	}
	head, _ = rawExchange(t, c, br, "GET / HTTP/1.1\r\nHost: a\r\nIf-None-Match: "+etag+"\r\n\r\n")
	if head[0] != "HTTP/1.1 304 Not Modified" || names(head) != "Server,Date,Last-Modified,Connection,ETag,Expires,Cache-Control" {
		t.Fatalf("304: %q", head)
	}
	head, body = rawExchange(t, c, br, "GET / HTTP/1.1\r\nHost: a\r\nRange: bytes=0-9\r\n\r\n")
	if head[0] != "HTTP/1.1 206 Partial Content" || body != string(page[:10]) {
		t.Fatalf("206: %q", head)
	}
	head, _ = rawExchange(t, c, br, "HEAD / HTTP/1.1\r\nHost: a\r\n\r\n")
	if head[0] != "HTTP/1.1 200 OK" {
		t.Fatalf("HEAD: %q", head)
	}
	head, body = rawExchange(t, c, br, "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\n\r\nabc")
	if head[0] != "HTTP/1.1 405 Not Allowed" || !strings.Contains(body, "405 Not Allowed") {
		t.Fatalf("POST: %q", head)
	}

	// garbage: nginx's 400 page, then close
	g, _ := net.Dial("tcp", addr)
	defer g.Close()
	g.Write([]byte("\x16\x03\x01garbage that is no request\r\n\r\n"))
	g.SetReadDeadline(time.Now().Add(3 * time.Second))
	all, _ := io.ReadAll(g)
	if !strings.HasPrefix(string(all), "HTTP/1.1 400 Bad Request\r\nServer: nginx\r\n") || !strings.HasSuffix(string(all), tlscarrier.NginxErrorPage(400, "")) {
		t.Fatalf("garbage: %q", all)
	}
}

// goCover runs a test with the built-in cover in its default (Go-native) form.
func goCover(t *testing.T) {
	old := coverNginx
	coverNginx = false
	t.Cleanup(func() { coverNginx = old })
}
