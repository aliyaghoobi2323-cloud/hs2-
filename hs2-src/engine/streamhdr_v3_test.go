package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// Wire v3: a user stream's header is always [kind][port u16], for TCP and UDP
// and any port (0: none known — the exit's default panel).
func TestUserStreamHeaderAlways3(t *testing.T) {
	for _, udp := range []bool{false, true} {
		want := kindTCPPort
		if udp {
			want = kindUDPPort
		}
		for _, port := range []int{0, 1, 443, 2053, 8443, 65535} {
			h := userStreamHeader(udp, port)
			if len(h) != 3 || h[0] != want || int(h[1])<<8|int(h[2]) != port {
				t.Errorf("udp=%v port %d: header %x, want [%d][%d BE]", udp, port, h, want, port)
			}
		}
	}
}

// The user tag (§3.1) is a hash of the client's IPv4 address, or of the /64 its
// IPv6 address is in.
func TestUserTagForSyn(t *testing.T) {
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	if UserTag(a, 64) == UserTag(b, 64) || UserTag(a, 64) != UserTag(a, 64) {
		t.Fatal("IPv4 tags are not one per address")
	}
	if UserTag(netip.MustParseAddr("::ffff:192.0.2.1"), 64) != UserTag(a, 64) {
		t.Fatal("an IPv4-mapped address is another user")
	}
	x, y := netip.MustParseAddr("2001:db8:1:2::1"), netip.MustParseAddr("2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	if UserTag(x, 64) != UserTag(y, 64) {
		t.Fatal("two addresses of one /64 are two users")
	}
	if UserTag(x, 64) == UserTag(netip.MustParseAddr("2001:db8:1:3::1"), 64) {
		t.Fatal("two /64s are one user")
	}
}

// End to end: the edge opens a user's stream with the user's tag in the SYN,
// and the exit reads it from there, not from a hash of its own.
func TestUserTagRidesSyn(t *testing.T) {
	key := bytes.Repeat([]byte{0x5b}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	panel := echoPanel(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	peers := &linkPeers{}
	traffic := newExitTraffic(peers)
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: panel, peers: peers, traffic: traffic})
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    1, Max: 1, PerLink: 50, ListenIP: "127.0.0.1", Ports: []string{port},
	})
	var c net.Conn
	for i := 0; i < 100 && c == nil; i++ {
		if c, err = net.Dial("tcp", "127.0.0.1:"+port); err != nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if c == nil {
		t.Fatal("user port never opened")
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo %q (%v)", got, err)
	}
	want := UserTag(netip.MustParseAddr("127.0.0.1"), 64)
	var tags []uint32
	traffic.mu.Lock()
	for ec := range traffic.conns {
		tags = append(tags, ec.tag)
	}
	traffic.mu.Unlock()
	if len(tags) != 1 || tags[0] != want {
		t.Fatalf("the exit's connections carry tags %x, want [%x]", tags, want)
	}
}

// The one-byte user header of wire v2 is gone: the exit closes such a stream
// and never dials the panel for it.
func TestExitRefusesOneByteKind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var dials atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			c.Close()
		}
	}()
	edge, exit, _ := guardedPair(t)
	cfg := KharejConfig{Panel: ln.Addr().String()}
	go func() {
		for {
			st, err := exit.AcceptStream()
			if err != nil {
				return
			}
			go serveStream(context.Background(), st, cfg, nil, nil, nil, nil)
		}
	}()
	for _, k := range []byte{kindTCP, kindUDP} {
		st, err := edge.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		st.Write([]byte{k})
		st.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, err := st.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Fatalf("kind %d: read %d (%v), want the stream closed", k, n, err)
		}
		st.Close()
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("the exit dialed the panel %d times for one-byte headers", n)
	}
}

// A SYN without the v3 payload is logged with the exact line, at most once a
// minute however many come, and counted for the status.
func TestBadSynLogged(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	badSyns := func() uint64 { n, _ := WireCounters(); return n }
	before := badSyns()
	started := make(chan struct{})
	go logWireEvery(ctx, func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}, 10*time.Millisecond)
	go func() { time.Sleep(50 * time.Millisecond); close(started) }()
	<-started
	c1, c2 := net.Pipe()
	defer c1.Close()
	cfg := smux.DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	sess, err := smux.Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go io.Copy(io.Discard, c1) // the FINs that refuse them
	for _, sid := range []byte{3, 5} {
		c1.Write([]byte{2, 0, 0, 0, sid, 0, 0, 0}) // version 2, SYN, no payload
	}
	deadline := time.Now().Add(2 * time.Second)
	for badSyns() < before+2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := badSyns() - before; n != 2 || sess.BadSyn() != 2 {
		t.Fatalf("bad syn counted %d (session %d), want 2", n, sess.BadSyn())
	}
	time.Sleep(100 * time.Millisecond) // several ticks
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "stream syn without v3 header" {
		t.Fatalf("logged %q, want the line once", lines)
	}
}
