package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// HS2_COVER_NGINX=1: the built-in cover answers like a stock nginx serving one
// static index page (server_tokens off), instead of like Go's net/http — the
// active probe saw Go's 404 body ("404 page not found"), Go's header order, no
// Server header and Go's text/plain 400 on a malformed request:
//
//   - "Server: nginx" and nginx's header order on every response;
//   - nginx's own HTML error pages (404, 400, 405, 416) with its reason
//     phrases; a malformed request gets a 400 and the connection closes;
//   - "/" and "/index.html" are the page; GET and HEAD, conditional requests
//     (304) and ranges (206) as nginx's static module; POST to the page is
//     405, any other method 405;
//   - an nginx ETag ("mtime-size" in hex), Expires beside Cache-Control (what
//     "expires 1h" sends), keep-alive with nginx's 65 s idle and 60 s header
//     timeouts.
//
// It is not a disguise against a determined prober: the TLS in front is still
// Go's (one 7-day ticket, its cipher order, http/1.1 only). For the strongest
// cover point backend_addr at a real nginx (README, Security).
var coverNginx = tlscarrier.CoverNginx

const (
	nginxHeaderTimeout    = 60 * time.Second // client_header_timeout
	nginxKeepaliveTimeout = 65 * time.Second // keepalive_timeout
)

// serveNginxCover serves page on ln the way nginx would.
func serveNginxCover(ln net.Listener, page []byte, modtime time.Time) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go nginxConn(c, page, modtime)
	}
}

// nginxETag is nginx's ETag: last-modified seconds and size, in hex.
func nginxETag(modtime time.Time, size int) string {
	return fmt.Sprintf("\"%x-%x\"", modtime.Unix(), size)
}

func nginxConn(c net.Conn, page []byte, modtime time.Time) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 8<<10)
	etag := nginxETag(modtime, len(page))
	first := true
	for {
		wait := nginxKeepaliveTimeout
		if first {
			wait = nginxHeaderTimeout
		}
		first = false
		c.SetReadDeadline(time.Now().Add(wait))
		req, err := http.ReadRequest(br)
		if err != nil {
			var ne net.Error
			if errors.Is(err, io.EOF) || errors.As(err, &ne) && ne.Timeout() {
				return // idle or gone: nginx just closes
			}
			io.WriteString(c, tlscarrier.NginxError(400, "", time.Now(), true))
			return
		}
		c.SetReadDeadline(time.Now().Add(nginxHeaderTimeout))
		resp := nginxRespond(req, page, modtime, etag, time.Now())
		// whatever body the request carried is not ours to keep
		io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))
		req.Body.Close()
		if _, err := c.Write(resp); err != nil || req.Close {
			return
		}
	}
}

// nginxRespond builds the whole response to one request.
func nginxRespond(req *http.Request, page []byte, modtime time.Time, etag string, now time.Time) []byte {
	isPage := req.URL.Path == "/" || req.URL.Path == "/index.html"
	closeConn := req.Close
	switch {
	case req.Method != http.MethodGet && req.Method != http.MethodHead && req.Method != http.MethodPost:
		return []byte(nginxErrorKA(405, now, closeConn))
	case !isPage:
		return []byte(nginxErrorKA(404, now, closeConn))
	case req.Method == http.MethodPost:
		return []byte(nginxErrorKA(405, now, closeConn))
	}
	rec := &coverRecorder{h: http.Header{}}
	rec.h.Set("Content-Type", "text/html")
	rec.h.Set("ETag", etag)
	rec.h.Set("Cache-Control", "max-age=3600")
	rec.h.Set("Expires", now.Add(time.Hour).UTC().Format(http.TimeFormat))
	http.ServeContent(rec, req, "", modtime, bytes.NewReader(page))
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	if rec.code == http.StatusRequestedRangeNotSatisfiable {
		body := tlscarrier.NginxErrorPage(416, "")
		return []byte(nginxHead(416, now, closeConn, [][2]string{
			{"Content-Type", "text/html"},
			{"Content-Length", strconv.Itoa(len(body))},
			{"Content-Range", fmt.Sprintf("bytes */%d", len(page))},
		}) + body)
	}
	if rec.code == http.StatusNotModified { // Go drops it when an ETag is set; nginx keeps it
		rec.h.Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	}
	var hs [][2]string
	for _, k := range []string{"Content-Type", "Content-Length", "Last-Modified"} {
		if v := rec.h.Get(k); v != "" && !(rec.code == http.StatusNotModified && k != "Last-Modified") {
			hs = append(hs, [2]string{k, v})
		}
	}
	hs = append(hs, [2]string{"\x00conn", ""})
	for _, k := range []string{"ETag", "Expires", "Cache-Control", "Content-Range"} {
		if v := rec.h.Get(k); v != "" {
			hs = append(hs, [2]string{k, v})
		}
	}
	if rec.code == http.StatusOK {
		hs = append(hs, [2]string{"Accept-Ranges", "bytes"})
	}
	return append([]byte(nginxHead(rec.code, now, closeConn, hs)), rec.body.Bytes()...)
}

// nginxHead is a status line and headers in nginx's order: Server, Date, the
// given ones with Connection where "\x00conn" stands (else after them).
func nginxHead(code int, now time.Time, closeConn bool, hs [][2]string) string {
	conn := "keep-alive"
	if closeConn {
		conn = "close"
	}
	var b strings.Builder
	reason := http.StatusText(code)
	switch code {
	case 405:
		reason = "Not Allowed"
	case 416:
		reason = "Requested Range Not Satisfiable"
	}
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\nServer: nginx\r\nDate: %s\r\n", code, reason, now.UTC().Format(http.TimeFormat))
	placed := false
	for _, h := range hs {
		if h[0] == "\x00conn" {
			fmt.Fprintf(&b, "Connection: %s\r\n", conn)
			placed = true
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	if !placed {
		fmt.Fprintf(&b, "Connection: %s\r\n", conn)
	}
	b.WriteString("\r\n")
	return b.String()
}

// nginxErrorKA is an nginx error response that keeps the connection (404, 405).
func nginxErrorKA(code int, now time.Time, closeConn bool) string {
	return tlscarrier.NginxError(code, "", now, closeConn)
}

// coverRecorder captures what http.ServeContent writes.
type coverRecorder struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (r *coverRecorder) Header() http.Header { return r.h }
func (r *coverRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *coverRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}
