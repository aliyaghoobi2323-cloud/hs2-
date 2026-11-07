package engine

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ACME HTTP-01 responder on the user ports.
//
// On the Iran server the tunnel's user port 80 (forward_ports with an empty
// user_listen_ip) holds port 80 on EVERY address, so `certbot --standalone`
// cannot bind it: no certificate for an extra lb address, and no automatic
// renewal of the current one. With acme_webroot set, a fresh connection on a
// chosen user port whose first bytes are exactly
//
//	GET /.well-known/acme-challenge/<token> HTTP/1.x\r\n
//
// is answered from <webroot>/.well-known/acme-challenge/<token> (what
// `certbot certonly --webroot -w <webroot>` writes) and closed; everything else
// is forwarded as before.
//
// Safety:
//   - The decision is made by PEEKING at the socket (MSG_PEEK, acme_peek_*.go):
//     no byte is consumed unless the connection is ours, so the normal path
//     receives the untouched *net.TCPConn and the same bytes, with no wrapper.
//   - A first byte other than 'G', or any byte that leaves the prefix, sends the
//     connection to the normal path at once. A client that stalls inside the
//     prefix (sends only "G" and waits) is released after ACMESniffTimeout; each
//     connection sniffs in its own goroutine, so nobody else waits.
//   - At most acmeMaxLine bytes are looked at; tokens are [A-Za-z0-9_-] only;
//     only GET; only regular files (no symlinks) of at most acmeMaxFile bytes.
//     A bad token or a missing file is a 404, never a path outside the webroot.

// ACMESniffTimeout bounds how long a connection on an ACME port may be held
// while its first bytes are inspected.
var ACMESniffTimeout = 500 * time.Millisecond

const (
	acmePrefix   = "GET /.well-known/acme-challenge/"
	acmeMaxLine  = 512
	acmeMaxToken = 255
	acmeMaxFile  = 8 << 10
	acmeMaxHead  = 8 << 10
)

// ACMEResponder answers HTTP-01 challenges on the user ports in Ports.
type ACMEResponder struct {
	Webroot string       // directory holding .well-known/acme-challenge/
	Ports   map[int]bool // user ports to answer on (e.g. {80: true})
	Log     func(string, ...any)
}

// On reports whether port p answers challenges (nil-safe).
func (a *ACMEResponder) On(p int) bool { return a != nil && a.Webroot != "" && a.Ports[p] }

// Intercept inspects a fresh user connection on port p. It returns true when
// the connection was an ACME challenge request: it has been answered and
// closed. false: the connection is untouched (nothing consumed, no deadline
// left) and must take the normal path.
func (a *ACMEResponder) Intercept(c net.Conn, p int) bool {
	if !a.On(p) {
		return false
	}
	head, ok := acmePeekRequestLine(c, ACMESniffTimeout)
	if !ok {
		return false
	}
	token, ok := acmeParseLine(head)
	if !ok {
		return false
	}
	a.serve(c, len(head), token)
	return true
}

// acmeMatchState classifies what has been peeked so far.
type acmeMatchState int

const (
	acmeNeedMore acmeMatchState = iota // still a possible challenge request
	acmeNo                             // not one: normal path
	acmeLine                           // a complete request line ending in \r\n
)

// acmeClassify looks at the peeked bytes b.
func acmeClassify(b []byte) (acmeMatchState, int) {
	n := len(b)
	if n > len(acmePrefix) {
		n = len(acmePrefix)
	}
	if !bytes.Equal(b[:n], []byte(acmePrefix[:n])) {
		return acmeNo, 0
	}
	if len(b) <= len(acmePrefix) {
		return acmeNeedMore, 0
	}
	if i := bytes.Index(b, []byte("\r\n")); i >= 0 {
		return acmeLine, i + 2
	}
	// The rest of the line can only be token chars, then " HTTP/1.x".
	rest := b[len(acmePrefix):]
	for i, ch := range rest {
		if ch == '\r' && i == len(rest)-1 {
			continue // the LF may still be on its way
		}
		if ch < 0x20 || ch == 0x7f {
			return acmeNo, 0
		}
	}
	if len(b) >= acmeMaxLine {
		return acmeNo, 0
	}
	return acmeNeedMore, 0
}

// acmeParseLine checks a full request line and returns the token. A line with
// the prefix but a bad version is not ours; a bad token is (it gets a 404).
func acmeParseLine(line []byte) (string, bool) {
	s := string(line)
	if len(s) < len(acmePrefix)+len(" HTTP/1.0\r\n") {
		return "", false
	}
	tail := s[len(s)-len(" HTTP/1.0\r\n"):]
	if tail != " HTTP/1.0\r\n" && tail != " HTTP/1.1\r\n" {
		return "", false
	}
	return s[len(acmePrefix) : len(s)-len(tail)], true
}

// acmeTokenOK: [A-Za-z0-9_-]{1,255} (base64url, what ACME tokens are).
func acmeTokenOK(t string) bool {
	if t == "" || len(t) > acmeMaxToken {
		return false
	}
	for i := 0; i < len(t); i++ {
		ch := t[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

// readFile returns the challenge file's content, or false (→ 404).
func (a *ACMEResponder) readFile(token string) ([]byte, bool) {
	if !acmeTokenOK(token) {
		return nil, false
	}
	p := filepath.Join(a.Webroot, ".well-known", "acme-challenge", token)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > acmeMaxFile {
		return nil, false
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, acmeMaxFile+1))
	if err != nil || len(b) > acmeMaxFile {
		return nil, false
	}
	return b, true
}

// serve consumes the request (line + headers, bounded), answers and closes.
func (a *ACMEResponder) serve(c net.Conn, lineLen int, token string) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// Consume the request so the close is a clean FIN, not an RST that could
	// destroy the response: the line, then headers up to the blank line.
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for len(buf) < lineLen+acmeMaxHead {
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if bytes.Contains(buf, []byte("\r\n\r\n")) {
			break
		}
		if err != nil {
			break
		}
	}
	body, ok := a.readFile(token)
	var resp []byte
	if ok {
		resp = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: " +
			strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n")
		resp = append(resp, body...)
		if a.Log != nil {
			a.Log("acme: answered challenge %s for %s", token, c.RemoteAddr())
		}
	} else {
		resp = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 10\r\nConnection: close\r\n\r\nnot found\n")
	}
	c.Write(resp)
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		// Give the client a moment to read and close first.
		c.SetReadDeadline(time.Now().Add(time.Second))
		io.Copy(io.Discard, io.LimitReader(c, 64<<10))
	}
}

// acmeServeUser wraps a user-connection handler: a challenge request is
// answered here, anything else goes to serve unchanged.
func acmeServeUser(a *ACMEResponder, c net.Conn, port int, serve func(net.Conn)) {
	if a.Intercept(c, port) {
		return
	}
	serve(c)
}
