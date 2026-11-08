package tlscarrier

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func withResume(t *testing.T) {
	old := Resume
	Resume = true
	t.Cleanup(func() { Resume = old })
}

func uconnOf(t *testing.T, c *Carrier) *utls.UConn {
	u, ok := c.conn.(*utls.UConn)
	if !ok {
		t.Fatalf("carrier conn is %T", c.conn)
	}
	return u
}

// Dial twice: the first link is a full handshake, the second resumes with
// the first one's ticket, and both pass the channel-bound auth.
func TestResumeSecondDial(t *testing.T) {
	withResume(t)
	key := bytes.Repeat([]byte{0x31}, 32)
	be, stopB := backend(t)
	defer stopB()
	got := make(chan *Carrier, 4)
	addr, stop := startServer(t, key, testCert(t, "vpn.example.com"), be, func(c *Carrier) { got <- c })
	defer stop()

	var ekms [][]byte
	for i := 0; i < 3; i++ {
		car, err := Dial(addr, "vpn.example.com", key)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		cs := uconnOf(t, car).ConnectionState()
		if want := i > 0; cs.DidResume != want {
			t.Fatalf("dial %d: DidResume=%v, want %v", i, cs.DidResume, want)
		}
		ekm, _ := cs.ExportKeyingMaterial(exporterLabel, nil, 32)
		ekms = append(ekms, ekm)
		// the tunnel works on the resumed link too
		car.SendFrame(7, []byte("x"+strconv.Itoa(i)))
		select {
		case sc := <-got:
			if ft, p, err := sc.ReadFrame(); err != nil || ft != 7 || string(p) != "x"+strconv.Itoa(i) {
				t.Fatalf("dial %d: tunnel frame %d %q %v", i, ft, p, err)
			}
			sc.Close()
		case <-time.After(3 * time.Second):
			t.Fatalf("dial %d: no tunnel", i)
		}
		car.Close()
	}
	if bytes.Equal(ekms[0], ekms[1]) || bytes.Equal(ekms[1], ekms[2]) {
		t.Fatal("a resumed connection has the exporter secret of another")
	}
}

// Without the key nothing resumes (today's behaviour): no cache, no PSK.
func TestNoResumeByDefault(t *testing.T) {
	if Resume {
		t.Skip("HS2_TLS_RESUME set in the environment")
	}
	key := bytes.Repeat([]byte{0x32}, 32)
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, key, testCert(t, "vpn.example.com"), be, func(c *Carrier) { c.Close() })
	defer stop()
	for i := 0; i < 2; i++ {
		car, err := Dial(addr, "vpn.example.com", key)
		if err != nil {
			t.Fatal(err)
		}
		if uconnOf(t, car).ConnectionState().DidResume {
			t.Fatal("resumed without HS2_TLS_RESUME")
		}
		car.Close()
	}
}

// handshake dials addr with the link's ClientHello (and, with Resume, the
// shared ticket cache) and returns the TLS conn and its EKM.
func handshake(t *testing.T, addr, sni string) (*utls.UConn, []byte) {
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &utls.Config{ServerName: sni, InsecureSkipVerify: true}
	u, err := newClientConn(raw, cfg, addr, clientHello)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		t.Fatal(err)
	}
	ekm, err := clientEKM(u, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return u, ekm
}

// An auth record recorded on one connection is refused on another, resumed
// with that connection's own ticket: the replayer is served the cover site.
func TestResumeReplayFails(t *testing.T) {
	withResume(t)
	key := bytes.Repeat([]byte{0x33}, 32)
	be, stopB := backend(t)
	defer stopB()
	tunnels := make(chan struct{}, 4)
	addr, stop := startServer(t, key, testCert(t, "vpn.example.com"), be, func(c *Carrier) { tunnels <- struct{}{}; c.Close() })
	defer stop()

	u1, ekm1 := handshake(t, addr, "vpn.example.com")
	rec, nonce := makeClientAuth(key, ekm1)
	if _, err := u1.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := readServerProof(u1, key, nonce, ekm1); err != nil { // also takes in the ticket
		t.Fatal(err)
	}
	<-tunnels
	u1.Close()

	u2, ekm2 := handshake(t, addr, "vpn.example.com")
	defer u2.Close()
	if !u2.ConnectionState().DidResume {
		t.Fatal("second connection did not resume")
	}
	if bytes.Equal(ekm1, ekm2) {
		t.Fatal("resumed connection shares the exporter secret")
	}
	if _, err := u2.Write(rec); err != nil { // the replay
		t.Fatal(err)
	}
	resp := make([]byte, 64)
	n, _ := io.ReadAtLeast(u2, resp, 12)
	if !strings.HasPrefix(string(resp[:n]), "HTTP/1.1 200 OK") {
		t.Fatalf("replayed auth not sent to the cover site: %q", resp[:n])
	}
	select {
	case <-tunnels:
		t.Fatal("replayed auth reached the tunnel")
	case <-time.After(200 * time.Millisecond):
	}
}

// Tickets are single use, newest first; nil clears.
func TestTicketCacheSingleUse(t *testing.T) {
	c := &ticketCache{}
	a, b := &utls.ClientSessionState{}, &utls.ClientSessionState{}
	c.Put("k", a)
	c.Put("k", b)
	if got, ok := c.Get("k"); !ok || got != b {
		t.Fatal("not newest first")
	}
	if got, ok := c.Get("k"); !ok || got != a {
		t.Fatal("second ticket lost")
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("a ticket was handed out twice")
	}
	for i := 0; i < ticketCacheMax+5; i++ {
		c.Put("k", &utls.ClientSessionState{})
	}
	if c.len() != ticketCacheMax {
		t.Fatalf("cache holds %d, cap %d", c.len(), ticketCacheMax)
	}
	c.Put("k", nil)
	if c.len() != 0 {
		t.Fatal("nil did not clear")
	}
	if ticketCacheFor("a:1", "s") == ticketCacheFor("a:2", "s") {
		t.Fatal("two servers share one ticket cache")
	}
}

// helloExts returns the sorted extension types of a built ClientHello.
func helloExts(t *testing.T, u *utls.UConn) []int {
	if err := u.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	b := u.HandshakeState.Hello.Raw
	p := 4 + 2 + 32
	p += 1 + int(b[p])
	p += 2 + int(binary.BigEndian.Uint16(b[p:]))
	p += 1 + int(b[p])
	end := p + 2 + int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	var out []int
	for p+4 <= end {
		typ, l := int(binary.BigEndian.Uint16(b[p:])), int(binary.BigEndian.Uint16(b[p+2:]))
		if typ&0x0f0f != 0x0a0a { // GREASE values differ per hello
			out = append(out, typ)
		}
		p += 4 + l
	}
	sort.Ints(out)
	return out
}

// With Resume on but no ticket yet, the first ClientHello carries exactly the
// preset's extensions (the empty pre_shared_key is left out).
func TestResumeFirstHelloIsPreset(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	plain := helloExts(t, utls.UClient(c1, &utls.Config{ServerName: "a.example"}, clientHello))
	withResume(t)
	u, err := newClientConn(c2, &utls.Config{ServerName: "first.example"}, "nowhere:1", clientHello)
	if err != nil {
		t.Fatal(err)
	}
	res := helloExts(t, u)
	if len(plain) != len(res) {
		t.Fatalf("extensions differ: %v vs %v", plain, res)
	}
	for i := range plain {
		if plain[i] != res[i] || res[i] == 41 {
			t.Fatalf("extensions differ: %v vs %v", plain, res)
		}
	}
}

func TestHelloFromEnv(t *testing.T) {
	for v, want := range map[string]utls.ClientHelloID{
		"":     utls.HelloChrome_120,
		"auto": utls.HelloChrome_Auto,
		"133":  utls.HelloChrome_133,
		"131":  utls.HelloChrome_131,
		"120":  utls.HelloChrome_120,
		"bad":  utls.HelloChrome_120,
	} {
		if got := helloByName(v, utls.HelloChrome_120); got != want {
			t.Fatalf("%q: %v, want %v", v, got, want)
		}
	}
	if utls.HelloChrome_Auto != utls.HelloChrome_133 {
		t.Log("note: the vendored uTLS has a Chrome newer than 133:", utls.HelloChrome_Auto.Str())
	}
	if !Resume && clientHello != utls.HelloChrome_133 {
		t.Fatal("the default link preset changed")
	}
}

// HS2_AUTH_PAD sizes, read back by the unchanged parsers (an older peer's).
func TestAuthPadCompatible(t *testing.T) {
	key := bytes.Repeat([]byte{0x34}, 32)
	ekm := bytes.Repeat([]byte{0x01}, 32)
	for _, on := range []bool{false, true} {
		old := AuthPad
		AuthPad = on
		minC, maxC, minS, maxS := 1<<30, 0, 1<<30, 0
		for i := 0; i < 400; i++ {
			rec, nonce := makeClientAuth(key, ekm)
			minC, maxC = min(minC, len(rec)), max(maxC, len(rec))
			if n, more, ok := parseClientAuth(key, ekm, rec); !ok || more != 0 || !bytes.Equal(n, nonce) {
				t.Fatal("client record not parsed")
			}
			if n, more, ok := parseClientAuth(key, ekm, rec[:authHeadLen+5]); !ok || more != len(rec)-authHeadLen-5 || !bytes.Equal(n, nonce) {
				t.Fatal("split client record not parsed")
			}
			proof := makeServerProof(key, nonce, ekm)
			minS, maxS = min(minS, len(proof)), max(maxS, len(proof))
			r := bytes.NewReader(append(proof, 0xAB))
			if err := readServerProof(r, key, nonce, ekm); err != nil || r.Len() != 1 {
				t.Fatal("server proof not read exactly", err)
			}
		}
		AuthPad = old
		t.Logf("AuthPad=%v client %d..%d server %d..%d bytes", on, minC, maxC, minS, maxS)
		if on && (minC < authHeadLen+padReqMin || maxC > authHeadLen+padReqMax || maxS > proofHeadLen+padHdrMax+padBodyMax || maxS < 1500) {
			t.Fatal("padded sizes out of range")
		}
		if !on && (minC < authHeadLen+clientPadMin || maxC > authHeadLen+clientPadMax || maxS > proofHeadLen+serverPadMax) {
			t.Fatal("default sizes changed")
		}
		if maxC > 16000 {
			t.Fatal("client record no longer fits one TLS record")
		}
	}
}

func TestQuietKeepAlive(t *testing.T) {
	old := QuietIdle
	defer func() { QuietIdle = old }()
	QuietIdle = false
	if dialKeepAlive() != 3*time.Second {
		t.Fatal("default keepalive changed")
	}
	QuietIdle = true
	if dialKeepAlive() != 45*time.Second {
		t.Fatal("quiet keepalive not 45 s")
	}
}

// nginx's own pages (server_tokens off): 404 is 146 bytes (153 with
// "nginx/1.18.0"), the plain-HTTP 400 248 (255 with it); the plain-HTTP
// reply only with HS2_COVER_NGINX.
func TestNginxPages(t *testing.T) {
	if n := len(NginxErrorPage(404, "")); n != 146 {
		t.Fatalf("404 page %d bytes, nginx sends 146", n)
	}
	if n := len(NginxErrorPage(400, "")); n != 150 {
		t.Fatalf("400 page %d bytes, nginx sends 150", n)
	}
	r := NginxError(400, "The plain HTTP request was sent to HTTPS port", time.Unix(0, 0), true)
	if !strings.HasPrefix(r, "HTTP/1.1 400 Bad Request\r\nServer: nginx\r\nDate: Thu, 01 Jan 1970 00:00:00 GMT\r\nContent-Type: text/html\r\nContent-Length: 248\r\nConnection: close\r\n\r\n<html>") {
		t.Fatalf("plain-HTTP reply: %q", r)
	}
	old := CoverNginx
	defer func() { CoverNginx = old }()
	CoverNginx = false
	if plainHTTPReply(time.Now()) != httpToHTTPS400 {
		t.Fatal("default plain-HTTP reply changed")
	}
	CoverNginx = true
	if !strings.Contains(plainHTTPReply(time.Now()), "Server: nginx") {
		t.Fatal("nginx plain-HTTP reply missing")
	}
}

// goNative runs a test with the default (Go-native) plain-HTTP reply.
func goNative(t *testing.T) {
	old := CoverNginx
	CoverNginx = false
	t.Cleanup(func() { CoverNginx = old })
}
