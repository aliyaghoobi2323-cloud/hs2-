package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func readHeader(t *testing.T, b []byte) (netip.AddrPort, []byte, error) {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(b))
	src, err := readProxyHeader(r)
	rest, _ := io.ReadAll(r)
	return src, rest, err
}

// proxyV2 builds a version 2 header: cmd 1 PROXY / 0 LOCAL, the family byte,
// the address block and any TLV bytes after it.
func proxyV2(cmd, fam byte, addrs []byte, tlv ...byte) []byte {
	b := append([]byte{}, proxyV2Sig...)
	b = append(b, 0x20|cmd, fam, 0, 0)
	binary.BigEndian.PutUint16(b[14:], uint16(len(addrs)+len(tlv)))
	return append(append(b, addrs...), tlv...)
}

func v2Addrs(src, dst netip.AddrPort) []byte {
	var b []byte
	b = append(b, src.Addr().AsSlice()...)
	b = append(b, dst.Addr().AsSlice()...)
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(b, src.Port()), dst.Port())
}

// Version 1: TCP4 and TCP6 give the client's address; UNKNOWN gives none
// (the connection's own stands); what follows the header is the user's.
func TestProxyV1(t *testing.T) {
	for _, c := range []struct {
		hdr  string
		want string // "" = none
	}{
		{"PROXY TCP4 198.51.100.7 192.0.2.1 40000 443\r\n", "198.51.100.7:40000"},
		{"PROXY TCP6 2001:db8::7 2001:db8::1 40001 443\r\n", "[2001:db8::7]:40001"},
		{"PROXY UNKNOWN\r\n", ""},
		{"PROXY UNKNOWN ffff:f...f:ffff ffff:f...f:ffff 65535 65535\r\n", ""},
	} {
		src, rest, err := readHeader(t, []byte(c.hdr+"GET / HTTP/1.1\r\n"))
		if err != nil || string(rest) != "GET / HTTP/1.1\r\n" {
			t.Fatalf("%q: err %v, left %q", c.hdr, err, rest)
		}
		if c.want == "" && src.IsValid() || c.want != "" && src.String() != c.want {
			t.Fatalf("%q: src %v, want %q", c.hdr, src, c.want)
		}
	}
	for _, bad := range []string{
		"PROXY TCP4 198.51.100.7 192.0.2.1 40000\r\n",       // a field short
		"PROXY TCP4 2001:db8::7 192.0.2.1 40000 443\r\n",    // TCP4 with an IPv6 address
		"PROXY TCP6 198.51.100.7 2001:db8::1 40000 443\r\n", // TCP6 with an IPv4 address
		"PROXY TCP4 198.51.100.7 192.0.2.1 70000 443\r\n",   // no such port
		"PROXY TCP4 198.51.100.7 192.0.2.1 40000 443\n",     // LF alone
		"PROXY UDP4 198.51.100.7 192.0.2.1 40000 443\r\n",
		"GET / HTTP/1.1\r\n",
		"\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03",
	} {
		if _, _, err := readHeader(t, []byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Through a real connection: the user's bytes after the header reach the
	// relay, and RemoteAddr is the client's — or, for UNKNOWN, the socket's.
	for _, c := range []struct{ hdr, want string }{
		{"PROXY TCP4 198.51.100.7 192.0.2.1 40000 443\r\n", "198.51.100.7:40000"},
		{"PROXY UNKNOWN\r\n", ""},
	} {
		app, edge := tcpPair(t)
		app.Write([]byte(c.hdr + "hello"))
		conn, ok, why := acceptProxied(edge, nil)
		if !ok {
			t.Fatalf("%q: refused (%s)", c.hdr, why)
		}
		want := c.want
		if want == "" {
			want = edge.RemoteAddr().String()
		}
		if got := conn.RemoteAddr().String(); got != want {
			t.Fatalf("%q: RemoteAddr %s, want %s", c.hdr, got, want)
		}
		b := make([]byte, 5)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, b); err != nil || string(b) != "hello" {
			t.Fatalf("%q: read %q (%v)", c.hdr, b, err)
		}
	}
}

// Version 2: PROXY over IPv4 and IPv6 give the client's address, TLVs
// skipped; LOCAL and other families give none.
func TestProxyV2(t *testing.T) {
	src4, dst4 := netip.MustParseAddrPort("198.51.100.7:40000"), netip.MustParseAddrPort("192.0.2.1:443")
	src6, dst6 := netip.MustParseAddrPort("[2001:db8::7]:40001"), netip.MustParseAddrPort("[2001:db8::1]:443")
	tlv := []byte{0x04, 0, 2, 'h', 'i'} // a NOOP TLV
	for _, c := range []struct {
		name string
		hdr  []byte
		want string
	}{
		{"PROXY IPv4", proxyV2(1, 0x11, v2Addrs(src4, dst4)), "198.51.100.7:40000"},
		{"PROXY IPv4 with a TLV", proxyV2(1, 0x11, v2Addrs(src4, dst4), tlv...), "198.51.100.7:40000"},
		{"PROXY IPv6", proxyV2(1, 0x21, v2Addrs(src6, dst6), tlv...), "[2001:db8::7]:40001"},
		{"LOCAL IPv4", proxyV2(0, 0x11, v2Addrs(src4, dst4)), ""},
		{"LOCAL IPv6", proxyV2(0, 0x21, v2Addrs(src6, dst6)), ""},
		{"LOCAL unspec", proxyV2(0, 0x00, nil), ""},
		{"PROXY unspec", proxyV2(1, 0x00, nil), ""},
	} {
		src, rest, err := readHeader(t, append(c.hdr, "data"...))
		if err != nil || string(rest) != "data" {
			t.Fatalf("%s: err %v, left %q", c.name, err, rest)
		}
		if c.want == "" && src.IsValid() || c.want != "" && src.String() != c.want {
			t.Fatalf("%s: src %v, want %q", c.name, src, c.want)
		}
	}
	short := proxyV2(1, 0x11, v2Addrs(src4, dst4)[:8]) // an IPv4 block cut short
	badVer := proxyV2(1, 0x11, v2Addrs(src4, dst4))
	badVer[12] = 0x11 // version 1 in a version 2 header
	badCmd := proxyV2(1, 0x11, v2Addrs(src4, dst4))
	badCmd[12] = 0x22
	badSig := proxyV2(1, 0x11, v2Addrs(src4, dst4))
	badSig[11] = 'X'
	for name, b := range map[string][]byte{"short": short, "version": badVer, "command": badCmd, "signature": badSig,
		"truncated": proxyV2(1, 0x11, v2Addrs(src4, dst4))[:20]} {
		if _, _, err := readHeader(t, b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The parser reads no more than a header can be: version 1 is at most 107
// bytes (CRLF included), version 2 at most 16+216.
func TestProxyTooLong(t *testing.T) {
	ok := "PROXY UNKNOWN " + strings.Repeat("x", proxyV1Max-len("PROXY UNKNOWN ")-2) + "\r\n"
	if len(ok) != 107 {
		t.Fatalf("test header of %d bytes", len(ok))
	}
	if _, rest, err := readHeader(t, []byte(ok+"after")); err != nil || string(rest) != "after" {
		t.Fatalf("a 107-byte header: err %v, left %q", err, rest)
	}
	long := "PROXY UNKNOWN " + strings.Repeat("x", proxyV1Max-len("PROXY UNKNOWN ")-1) + "\r\n"
	r := bufio.NewReader(bytes.NewReader([]byte(long + "after")))
	if _, err := readProxyHeader(r); err == nil {
		t.Fatalf("a %d-byte header accepted", len(long))
	}
	if rest, _ := io.ReadAll(r); len(long)+len("after")-len(rest) > proxyV1Max {
		t.Fatalf("read %d bytes of a refused header", len(long)+len("after")-len(rest))
	}
	tooBig := proxyV2(1, 0x11, make([]byte, proxyV2Max+1))
	if _, _, err := readHeader(t, tooBig); err == nil {
		t.Fatal("a version 2 header past 16+216 bytes accepted")
	}
	if _, _, err := readHeader(t, proxyV2(1, 0x11, make([]byte, proxyV2Max))); err != nil {
		t.Fatalf("a 16+216-byte version 2 header: %v", err)
	}
}

// proxyRig is a tunnel whose Iran user port is behind a proxy: the edge reads
// the PROXY header, the exit counts what reaches it.
type proxyRig struct {
	userAddr string
	traffic  *exitTraffic
	mu       sync.Mutex
	logs     []string
}

func (r *proxyRig) logf(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(f, a...))
}

func (r *proxyRig) logged(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// exitTags is the user tags of the connections the exit relays now.
func (r *proxyRig) exitTags() []uint32 {
	r.traffic.mu.Lock()
	defer r.traffic.mu.Unlock()
	var tags []uint32
	for c := range r.traffic.conns {
		tags = append(tags, c.tag)
	}
	return tags
}

func startProxyRig(t *testing.T, proxyFrom []netip.Prefix) *proxyRig {
	t.Helper()
	key := bytes.Repeat([]byte{0x5c}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	peers := &linkPeers{}
	r := &proxyRig{traffic: newExitTraffic(peers)}
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: panel, peers: peers, traffic: r.traffic})
	port := freePort(t)
	var pn int
	fmt.Sscan(port, &pn)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    1, Max: 1, PerLink: 50, ListenIP: "127.0.0.1", Ports: []string{port},
		ProxyPorts: []int{pn}, ProxyFrom: proxyFrom, Log: r.logf,
	})
	r.userAddr = "127.0.0.1:" + port
	for i := 0; i < 100; i++ { // the port says it is open (a test dial would be a refusal)
		if r.logged("user port "+port+" open") > 0 {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("user port never opened")
	return nil
}

// echoThrough sends hdr and "ping" as a proxy would, and returns what comes
// back (the panel echoes) and whether the connection was closed (a FIN, or a
// reset: closed with "ping" unread).
func echoThrough(t *testing.T, addr string, hdr []byte) (string, bool) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(append(append([]byte{}, hdr...), "ping"...))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 4)
	n, err := io.ReadFull(c, b)
	var ne net.Error
	return string(b[:n]), err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// On a proxy port the user is the one the header names: its tag rides the
// SYN to the exit (UserTag of the real client, IPv4 or its IPv6 /64).
func TestProxyUserTagFromClient(t *testing.T) {
	r := startProxyRig(t, nil)
	for _, c := range []struct {
		hdr    []byte
		client string
	}{
		{[]byte("PROXY TCP4 198.51.100.7 127.0.0.1 40000 443\r\n"), "198.51.100.7"},
		{proxyV2(1, 0x21, v2Addrs(netip.MustParseAddrPort("[2001:db8:1:2::77]:40001"), netip.MustParseAddrPort("[::1]:443"))), "2001:db8:1:2::1"},
	} {
		cn, err := net.Dial("tcp", r.userAddr)
		if err != nil {
			t.Fatal(err)
		}
		cn.Write(append(append([]byte{}, c.hdr...), "ping"...))
		cn.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 4)
		if _, err := io.ReadFull(cn, b); err != nil || string(b) != "ping" {
			t.Fatalf("%s: echo %q (%v)", c.client, b, err)
		}
		want := UserTag(netip.MustParseAddr(c.client), classV6Prefix)
		if tags := r.exitTags(); len(tags) != 1 || tags[0] != want {
			t.Fatalf("%s: the exit's connections carry tags %x, want [%x]", c.client, tags, want)
		}
		cn.Close()
		deadline := time.Now().Add(2 * time.Second)
		for len(r.exitTags()) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// On a proxy port a connection without a valid header is closed: nothing of
// it reaches the exit, and the reason is logged once a minute.
func TestProxyNoHeaderClosed(t *testing.T) {
	r := startProxyRig(t, nil)
	for i := 0; i < 2; i++ {
		got, closed := echoThrough(t, r.userAddr, []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		if got != "" || !closed {
			t.Fatalf("a connection without a header: echo %q, closed %v", got, closed)
		}
	}
	got, closed := echoThrough(t, r.userAddr, []byte("PROXY TCP4 198.51.100.7 127.0.0.1 40000\r\n"))
	if got != "" || !closed {
		t.Fatalf("a bad header: echo %q, closed %v", got, closed)
	}
	if tags := r.exitTags(); len(tags) != 0 {
		t.Fatalf("the exit relays %d connection(s) that had no header", len(tags))
	}
	if n := r.logged("without a valid PROXY protocol header"); n != 1 {
		t.Fatalf("logged %d times, want once a minute: %q", n, r.logs)
	}
}

// A header from an address outside proxy_from is not believed: closed.
func TestProxyFromNotAllowed(t *testing.T) {
	r := startProxyRig(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	got, closed := echoThrough(t, r.userAddr, []byte("PROXY TCP4 198.51.100.7 127.0.0.1 40000 443\r\n"))
	if got != "" || !closed {
		t.Fatalf("a header from 127.0.0.1 with proxy_from 10.0.0.0/8: echo %q, closed %v", got, closed)
	}
	if n := r.logged("not in proxy_from"); n != 1 {
		t.Fatalf("logged %d times: %q", n, r.logs)
	}
	for _, c := range []struct {
		from  string
		allow []netip.Prefix
		ok    bool
	}{
		{"127.0.0.1", nil, true},
		{"127.3.2.1", nil, true},
		{"::1", nil, true},
		{"::ffff:127.0.0.1", nil, true},
		{"10.1.2.3", nil, false},
		{"10.1.2.3", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, true},
		{"127.0.0.1", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, false},
	} {
		if got := proxyAllowed(netip.MustParseAddr(c.from), c.allow); got != c.ok {
			t.Errorf("proxyAllowed(%s, %v) = %v", c.from, c.allow, got)
		}
	}
	if p, err := ParseProxyFrom(" 10.0.0.5, 192.0.2.0/24 ,::1,"); err != nil || fmt.Sprint(p) != "[10.0.0.5/32 192.0.2.0/24 ::1/128]" {
		t.Fatalf("ParseProxyFrom: %v %v", p, err)
	}
	if _, err := ParseProxyFrom("10.0.0.0/33"); err == nil {
		t.Fatal("a bad prefix accepted")
	}
}

// The port_map flag: "P/proxy" marks a user port, with or without a target;
// the flag alone is no target, and the canonical form keeps it.
func TestPortMapProxyFlag(t *testing.T) {
	const s = " 2053/proxy, 443 ,8443/proxy=10.0.0.5:443,"
	proxy, err := ParsePortFlags(s)
	if err != nil || fmt.Sprint(proxy) != "[2053 8443]" {
		t.Fatalf("flags %v %v", proxy, err)
	}
	m, err := ParsePortMap(s)
	if err != nil || fmt.Sprint(m) != "map[443:127.0.0.1:443 8443:10.0.0.5:443]" {
		t.Fatalf("targets %v %v", m, err)
	}
	if c, err := CanonicalPortMap(s); err != nil || c != "443,2053/proxy,8443/proxy=10.0.0.5:443" {
		t.Fatalf("canonical %q %v", c, err)
	}
	for _, bad := range []string{"443/tls", "443/", "/proxy", "443/proxy,443"} {
		if _, err := ParsePortFlags(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzProxyHeader(f *testing.F) {
	f.Add([]byte("PROXY TCP4 198.51.100.7 192.0.2.1 40000 443\r\nGET /"))
	f.Add([]byte("PROXY TCP6 2001:db8::7 2001:db8::1 40001 443\r\n"))
	f.Add([]byte("PROXY UNKNOWN\r\n"))
	f.Add(proxyV2(1, 0x11, v2Addrs(netip.MustParseAddrPort("1.2.3.4:5"), netip.MustParseAddrPort("5.6.7.8:9")), 4, 0, 1, 0))
	f.Add(proxyV2(1, 0x21, v2Addrs(netip.MustParseAddrPort("[::1]:5"), netip.MustParseAddrPort("[::2]:9"))))
	f.Add(proxyV2(0, 0x00, nil))
	f.Add([]byte("\r\n\r\n\x00\r\nQUIT\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		under := bytes.NewReader(b)
		r := bufio.NewReader(under)
		src, err := readProxyHeader(r)
		used := len(b) - r.Buffered() - under.Len()
		if used > proxyV2Hdr+proxyV2Max {
			t.Fatalf("read %d bytes for one header", used)
		}
		if err != nil {
			return
		}
		if !bytes.HasPrefix(b, []byte("PROXY ")) && !bytes.HasPrefix(b, proxyV2Sig) {
			t.Fatalf("accepted %q", b[:used])
		}
		if bytes.HasPrefix(b, []byte("PROXY ")) && used > proxyV1Max {
			t.Fatalf("a version 1 header of %d bytes", used)
		}
		if src.IsValid() && src.Addr().Zone() != "" {
			t.Fatalf("a zone in %v", src)
		}
	})
}
