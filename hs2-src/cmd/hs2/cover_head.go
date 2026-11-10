package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"regexp"
	"strconv"
	"time"
)

// Active-probe coherence of the nginx-style cover (HS2_COVER_NGINX).
//
// nginx parses the request line as the bytes arrive and answers
// "400 Bad Request" (Connection: close) at the first byte that cannot be part
// of one. net/http's reader instead waits for a whole line, so a probe that
// sent random bytes without a '\n' (about a third of 300-byte random blobs)
// got NO answer at all — a silence no nginx produces. nginxCheckHead closes
// that gap; it only looks at bytes that already reached the cover, i.e. a
// connection the TLS front door has already judged NOT to be a tunnel, so
// authenticated tunnel traffic never passes through it.

const (
	nginxLineMax = 8 << 10  // one large_client_header_buffers buffer (4 8k)
	nginxHeadMax = 32 << 10 // all four of them
)

const nginxTooLarge = "Request Header Or Cookie Too Large"

// nginxCheckHead waits until the buffered bytes hold a whole request head, or
// prove it bad, WITHOUT consuming anything: code 0 means go on and parse it.
// It follows nginx's request-line parser where a probe can tell: the method is
// [A-Z_-], then spaces, then a URI starting '/' (or a scheme letter), then an
// optional " HTTP/x.y"; anything else is a 400 at the byte it appears. A
// request line over 8k is a 414, a header line over 8k or a head over 32k is
// nginx's 400 "Request Header Or Cookie Too Large". err is the read error
// (timeout, EOF) on which nginx closes silently.
func nginxCheckHead(br *bufio.Reader) (code int, detail string, err error) {
	const (
		sStart = iota
		sMethod
		sSpBeforeURI
		sURI
		sAfterURI
		sProto
		sProtoEnd
		sHeaders
	)
	st, i, lineStart, proto := sStart, 0, 0, 0
	for {
		b, perr := br.Peek(i + 1)
		if perr != nil {
			if errors.Is(perr, bufio.ErrBufferFull) {
				return 400, nginxTooLarge, nil
			}
			return 0, "", perr
		}
		if n := br.Buffered(); n > len(b) {
			b, _ = br.Peek(n)
		}
		for ; i < len(b); i++ {
			ch := b[i]
			switch st {
			case sStart:
				switch {
				case ch == '\r' || ch == '\n':
					lineStart = i + 1
				case ch >= 'A' && ch <= 'Z' || ch == '_' || ch == '-':
					st = sMethod
				default:
					return 400, "", nil
				}
			case sMethod:
				switch {
				case ch == ' ':
					st = sSpBeforeURI
				case ch >= 'A' && ch <= 'Z' || ch == '_' || ch == '-':
				default:
					return 400, "", nil
				}
			case sSpBeforeURI:
				switch {
				case ch == ' ':
				case ch == '/' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
					st = sURI
				default:
					return 400, "", nil
				}
			case sURI:
				switch ch {
				case ' ':
					st = sAfterURI
				case '\r', '\n':
					return 0, "", nil // HTTP/0.9 style: left to the parser
				case 0:
					return 400, "", nil
				}
			case sAfterURI:
				switch ch {
				case ' ':
				case 'H':
					st, proto = sProto, 1
				case '\r', '\n':
					return 0, "", nil
				default:
					st = sURI // nginx tolerates spaces inside the URI
				}
			case sProto:
				if proto < 5 {
					if ch != "HTTP/"[proto] {
						return 400, "", nil
					}
					proto++
					break
				}
				switch {
				case ch >= '0' && ch <= '9' || ch == '.':
				case ch == ' ' || ch == '\r':
					st = sProtoEnd
				case ch == '\n':
					st, lineStart = sHeaders, i+1
				default:
					return 400, "", nil
				}
			case sProtoEnd:
				switch ch {
				case ' ', '\r':
				case '\n':
					st, lineStart = sHeaders, i+1
				default:
					return 400, "", nil
				}
			case sHeaders:
				if ch == '\n' {
					l := b[lineStart:i]
					if len(l) > 0 && l[len(l)-1] == '\r' {
						l = l[:len(l)-1]
					}
					if len(l) == 0 {
						return 0, "", nil // the end of the head
					}
					lineStart = i + 1
				}
			}
			if i-lineStart >= nginxLineMax {
				if st < sHeaders {
					return 414, "", nil
				}
				return 400, nginxTooLarge, nil
			}
		}
	}
}

// coverFile is one static file of the nginx-style cover.
type coverFile struct {
	body  []byte
	ctype string
	etag  string
}

// nginxCoverFiles is what the cover serves: the page at "/" and
// "/index.html", plus the /robots.txt and /favicon.ico nearly every real small
// site has (404 on both stood out). Both derive from the page itself, so they
// are stable per install and differ between installs; the page bytes (and
// their stability contract, cover.go) are untouched.
func nginxCoverFiles(page []byte, modtime time.Time) map[string]*coverFile {
	mk := func(b []byte, ct string) *coverFile {
		return &coverFile{body: b, ctype: ct, etag: nginxETag(modtime, len(b))}
	}
	p := mk(page, "text/html")
	return map[string]*coverFile{
		"/":            p,
		"/index.html":  p,
		"/robots.txt":  mk(coverRobots(page), "text/plain"),
		"/favicon.ico": mk(coverFaviconICO(page), "image/x-icon"),
	}
}

// coverRobots is one of the common hand-written robots.txt shapes.
func coverRobots(page []byte) []byte {
	h := sha256.Sum256(append([]byte("hs2-cover-robots\x00"), page...))
	switch h[0] % 4 {
	case 0:
		return []byte("User-agent: *\nDisallow:\n")
	case 1:
		return []byte("User-agent: *\nAllow: /\n")
	case 2:
		return []byte("User-agent: *\nDisallow:\n\nSitemap: /sitemap.xml\n")
	}
	return []byte("# robots.txt\nUser-agent: *\nDisallow:\n")
}

// the inline favicon's colour, '%' escaped as in its data: URI (cover.go)
var coverHSL = regexp.MustCompile(`fill='hsl\((\d+) (\d+)%25 (\d+)%25\)'`)

// coverFaviconICO is a 16x16 32-bit .ico in the page's own favicon colour (a
// rounded square), so /favicon.ico agrees with the page's inline icon.
func coverFaviconICO(page []byte) []byte {
	r, g, b := uint8(0x3b), uint8(0x6e), uint8(0xc4)
	if m := coverHSL.FindSubmatch(page); m != nil {
		hh, _ := strconv.Atoi(string(m[1]))
		ss, _ := strconv.Atoi(string(m[2]))
		ll, _ := strconv.Atoi(string(m[3]))
		r, g, b = hslRGB(float64(hh), float64(ss)/100, float64(ll)/100)
	}
	const n = 16
	var buf bytes.Buffer
	le := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	px := n * n * 4
	mask := n * 4 // 1-bit AND mask rows padded to 32 bits
	le(uint16(0))
	le(uint16(1))
	le(uint16(1)) // ICONDIR: one image
	buf.Write([]byte{n, n, 0, 0})
	le(uint16(1))
	le(uint16(32))
	le(uint32(40 + px + mask))
	le(uint32(6 + 16)) // ICONDIRENTRY
	le(uint32(40))
	le(int32(n))
	le(int32(2 * n))
	le(uint16(1))
	le(uint16(32))
	le(uint32(0))
	le(uint32(px + mask))
	le(int32(0))
	le(int32(0))
	le(uint32(0))
	le(uint32(0)) // BITMAPINFOHEADER
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			a := uint8(255)
			cx, cy := math.Min(float64(x), float64(n-1-x)), math.Min(float64(y), float64(n-1-y))
			if cx < 2 && cy < 2 && (2-cx)*(2-cx)+(2-cy)*(2-cy) > 5 { // rounded corners
				a = 0
			}
			buf.Write([]byte{b, g, r, a})
		}
	}
	buf.Write(make([]byte, mask))
	return buf.Bytes()
}

// hslRGB converts CSS hsl() to 8-bit RGB.
func hslRGB(h, s, l float64) (uint8, uint8, uint8) {
	c := (1 - math.Abs(2*l-1)) * s
	hp := math.Mod(h, 360) / 60
	x := c * (1 - math.Abs(math.Mod(hp, 2)-1))
	var r, g, b float64
	switch {
	case hp < 1:
		r, g = c, x
	case hp < 2:
		r, g = x, c
	case hp < 3:
		g, b = c, x
	case hp < 4:
		g, b = x, c
	case hp < 5:
		r, b = x, c
	default:
		r, b = c, x
	}
	m := l - c/2
	cv := func(v float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, v+m)) * 255)) }
	return cv(r), cv(g), cv(b)
}

// nginxLinger is nginx's lingering close after an error response: shut the
// write side (the response is out, then FIN) and read and drop what the
// client still sends, up to lingering_timeout (5 s) of silence and
// lingering_time (30 s) in all. Closing with unread bytes instead would reset
// the connection, which can destroy the response before the client reads it.
func nginxLinger(c net.Conn) {
	cw, ok := c.(interface{ CloseWrite() error })
	if !ok {
		return
	}
	cw.CloseWrite()
	end := time.Now().Add(30 * time.Second)
	buf := make([]byte, 4096)
	for time.Now().Before(end) {
		d := time.Now().Add(5 * time.Second)
		if d.After(end) {
			d = end
		}
		c.SetReadDeadline(d)
		if _, err := c.Read(buf); err != nil {
			return
		}
	}
}
