package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Per-port routing (routes.go): the exit's table, the port tag on user
// streams, the gate that keeps a link out of use until it is known whether
// its exit routes tags, and the end-to-end behaviour over real TLS links in
// both directions, for TCP and UDP, with every older/newer combination.

func TestParsePortMap(t *testing.T) {
	m, err := ParsePortMap(" 2053, 2083=10.0.0.5:443 ,8880=[::1]:80,,")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{2053: "127.0.0.1:2053", 2083: "10.0.0.5:443", 8880: "[::1]:80"}
	if fmt.Sprint(m) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	if got := FormatPortMap(m); got != "2053,2083=10.0.0.5:443,8880=[::1]:80" {
		t.Fatalf("format: %q", got)
	}
	if m, err := ParsePortMap(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	for _, bad := range []string{"0", "65536", "abc", "0443", "+443", "443=", "443=host", "443=:80",
		"443=1.2.3.4:0", "443=1.2.3.4:http", "443,443", "443,443=1.2.3.4:5"} {
		if _, err := ParsePortMap(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRouteTableTarget(t *testing.T) {
	rt := RouteTable{Default: "127.0.0.1:8443", Ports: map[int]string{2053: "127.0.0.1:2053"}}
	for _, c := range []struct {
		port int
		want string
		ok   bool
	}{{2053, "127.0.0.1:2053", true}, {8443, "127.0.0.1:8443", true}, {0, "127.0.0.1:8443", true}} {
		if got, ok := rt.Target(c.port); got != c.want || ok != c.ok {
			t.Errorf("port %d: %q %v, want %q %v", c.port, got, ok, c.want, c.ok)
		}
	}
	strict := RouteTable{Ports: map[int]string{2053: "127.0.0.1:2053"}}
	if _, ok := strict.Target(8443); ok {
		t.Error("no default: an unmapped port must be refused")
	}
	if _, ok := strict.Target(0); ok {
		t.Error("no default: an untagged stream (older edge) must be refused")
	}
	if got, ok := (RouteTable{Default: "a:1"}).Target(2053); got != "a:1" || !ok {
		t.Error("a table without port_map routes everything to the default (the old behaviour)")
	}
}

// The header a user stream opens with: tagged whenever the exit routes tags
// (it decides with its current table); what THIS link's exit said decides, a
// late link goes by the pool's, a refusing (older) exit is never tagged.
func TestUserStreamHeader(t *testing.T) {
	f := newMeteredFake()
	routed := &peerInfo{V2: true, Caps: capPortTags, Flags: flagDefault, Ports: []int{2053, 443}}
	for _, c := range []struct {
		name string
		pi   *peerInfo
		port int
		udp  bool
		want []byte
	}{
		{"no info (older exit)", nil, 2053, false, []byte{kindTCP}},
		{"v1 exit (ceiling only)", &peerInfo{MaxLinks: 48}, 2053, false, []byte{kindTCP}},
		{"v2 exit without tags", &peerInfo{V2: true, Flags: flagDefault, Ports: []int{2053}}, 2053, false, []byte{kindTCP}},
		{"mapped port, tcp", routed, 2053, false, []byte{kindTCPPort, 0x08, 0x05}},
		{"mapped port, udp", routed, 443, true, []byte{kindUDPPort, 0x01, 0xbb}},
		{"unmapped port: tagged too — the exit decides with the table it has now", routed, 8443, false, []byte{kindTCPPort, 0x20, 0xfb}},
		{"no port", routed, 0, true, []byte{kindUDP}},
	} {
		f.m.peerInfo.Store(c.pi)
		if got := userStreamHeader(f, c.udp, c.port, nil); string(got) != string(c.want) {
			t.Errorf("%s: header %v, want %v", c.name, got, c.want)
		}
	}
	// A link whose own answer is late goes by what the pool learned...
	f.m.peerInfo.Store(nil)
	if got := userStreamHeader(f, false, 2053, routed); got[0] != kindTCPPort {
		t.Errorf("late link with the pool's answer: %v, want tagged", got)
	}
	// ...but a link whose exit REFUSED the exchange is an older exit: never.
	f.m.infoRefused.Store(true)
	if got := userStreamHeader(f, false, 2053, routed); got[0] != kindTCP {
		t.Errorf("refused link: %v, want untagged", got)
	}
	// A link without a meter never tags.
	if got := userStreamHeader(&fakeLinkNoMeter{}, false, 2053, routed); string(got) != string([]byte{kindTCP}) {
		t.Errorf("unmetered link: %v", got)
	}
}

type fakeLinkNoMeter struct{}

func (fakeLinkNoMeter) OpenStream() (stream, error) { return nil, io.EOF }
func (fakeLinkNoMeter) Active() int32               { return 0 }
func (fakeLinkNoMeter) Alive() bool                 { return true }
func (fakeLinkNoMeter) Close() error                { return nil }

// With the gate on, a link whose kindInfo exchange is not over takes no user
// connection; it does the moment the exchange ends, however it ended.
func TestPickWaitsForInfo(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 4, false)
	m.gateInfo = true
	f := newMeteredFake()
	addManaged(m, f)
	if _, _, ok := m.Pick(); ok {
		t.Fatal("picked a link whose info exchange is still running")
	}
	f.m.infoDone.Store(true)
	if _, rel, ok := m.Pick(); !ok {
		t.Fatal("the exchange is over: the link must be picked")
	} else {
		rel()
	}
	// An older edge build (gate off) never waits.
	m2, _, _ := newV2Manager(nil, 1, 4, false)
	addManaged(m2, newMeteredFake())
	if _, rel, ok := m2.Pick(); !ok {
		t.Fatal("gate off: picked nothing")
	} else {
		rel()
	}
}

// openInfo always ends the gate — answered, refused by an older exit, or
// given up — and records what the exit said.
func TestOpenInfoEndsGate(t *testing.T) {
	ctx := context.Background()
	t.Run("v2 exit", func(t *testing.T) {
		exit := func(ctx context.Context, _ *smux.Session, st *smux.Stream, mtr *linkMeter) {
			serveStream(ctx, st, KharejConfig{MaxLinks: 48, Panel: "127.0.0.1:1", Routes: map[int]string{2053: "127.0.0.1:2053"}}, nil, nil, nil, mtr)
		}
		p := newStatsPair(t, ctx, nil, exit)
		openInfo(ctx, p.edge, peerInfo{MaxLinks: 64, Caps: capPortTags, Flags: flagUDP, Ports: []int{8443, 2053}})
		pi := p.edge.m.peerInfo.Load()
		if !p.edge.m.infoDone.Load() || !pi.tags() || fmt.Sprint(pi.Ports) != "[2053]" || pi.Flags&flagDefault == 0 {
			t.Fatalf("edge: done %v, info %+v", p.edge.m.infoDone.Load(), pi)
		}
		epi := p.exitMtr.peerInfo.Load()
		if epi == nil || fmt.Sprint(epi.Ports) != "[8443 2053]" || epi.Flags&flagUDP == 0 {
			t.Fatalf("exit learned %+v", epi)
		}
	})
	t.Run("v1 exit (ceiling only)", func(t *testing.T) {
		v1 := func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
			var k [1]byte
			io.ReadFull(st, k[:])
			readInfo(st)
			st.Write([]byte{1, 2, 0, 48}) // the previous release's reply
			st.Close()
		}
		p := newStatsPair(t, ctx, nil, v1)
		openInfo(ctx, p.edge, peerInfo{MaxLinks: 64, Caps: capPortTags})
		if !p.edge.m.infoDone.Load() || p.edge.m.peerInfo.Load().tags() || p.edge.m.peerMax.Load() != 48 {
			t.Fatalf("v1 exit: done %v, info %+v, max %d", p.edge.m.infoDone.Load(), p.edge.m.peerInfo.Load(), p.edge.m.peerMax.Load())
		}
		if got := userStreamHeader(p.edge, false, 2053, nil); got[0] != kindTCP {
			t.Fatalf("a v1 exit must get untagged streams, got %v", got)
		}
	})
	t.Run("older exit (no kindInfo)", func(t *testing.T) {
		older := func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
			var k [1]byte
			io.ReadFull(st, k[:])
			st.Close()
		}
		p := newStatsPair(t, ctx, nil, older)
		openInfo(ctx, p.edge, peerInfo{MaxLinks: 64, Caps: capPortTags})
		if !p.edge.m.infoDone.Load() || p.edge.m.peerInfo.Load() != nil {
			t.Fatal("an older exit must end the gate with no info")
		}
	})
	t.Run("no reply: the gate opens after the first attempt, retries go on", func(t *testing.T) {
		oldRetry, oldTries := infoRetry, infoTries
		infoRetry, infoTries = 20*time.Millisecond, 2
		defer func() { infoRetry, infoTries = oldRetry, oldTries }()
		silent := func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) { io.Copy(io.Discard, st) }
		p := newStatsPair(t, ctx, nil, silent)
		done := make(chan struct{})
		go func() { openInfo(ctx, p.edge, peerInfo{}); close(done) }()
		time.Sleep(100 * time.Millisecond)
		if p.edge.m.infoDone.Load() {
			t.Fatal("the gate opened before the first attempt was over")
		}
		v2Wait(t, 7*time.Second, "the gate opens after the first attempt", func() bool { return p.edge.m.infoDone.Load() })
		select {
		case <-done:
			t.Fatal("openInfo returned after one attempt; it must keep asking")
		default:
		}
		<-done
		if p.edge.m.infoRefused.Load() {
			t.Fatal("no answer is not a refusal: the link must not be marked older")
		}
		if !p.edge.m.infoDone.Load() {
			t.Fatal("given up: the gate must end")
		}
	})
}

// namedPanel is a panel inbound that says who it is: a TCP connection gets
// "<name>|" and then an echo; a UDP datagram comes back as "<name>|<payload>".
func namedPanel(t *testing.T, name string) string {
	t.Helper()
	ln, pc := tcpUDPPair(t)
	t.Cleanup(func() { ln.Close(); pc.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { c.Write([]byte(name + "|")); io.Copy(c, c); c.Close() }()
		}
	}()
	go func() {
		b := make([]byte, 65536)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte(name+"|"), b[:n]...), a)
		}
	}()
	return ln.Addr().String()
}

// tcpReaches connects to a user port and returns the name of the panel inbound
// it reached ("" = refused / closed), after checking the echo works.
func tcpReaches(addr string) string {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	br := bufio.NewReader(c)
	name, err := br.ReadString('|')
	if err != nil {
		return ""
	}
	if _, err := c.Write([]byte("ping\n")); err != nil {
		return ""
	}
	if l, err := br.ReadString('\n'); err != nil || l != "ping\n" {
		return ""
	}
	return strings.TrimSuffix(name, "|")
}

// udpReaches sends a datagram to a user port and returns the panel's name.
func udpReaches(addr string) string {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return ""
	}
	defer c.Close()
	b := make([]byte, 2048)
	for try := 0; try < 10; try++ { // the first datagrams may wait for a link
		c.Write([]byte("hello"))
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if n, err := c.Read(b); err == nil {
			name, rest, ok := strings.Cut(string(b[:n]), "|")
			if ok && rest == "hello" {
				return name
			}
		}
	}
	return ""
}

// The core of the feature, on real TLS links in both directions, TCP and UDP:
// each Iran user port reaches its own inbound; a port without an entry
// reaches the default panel; a port the exit has no target for is refused
// (and the exit says which, once); both servers show the other's ports.
func TestPortRoutesEndToEnd(t *testing.T) {
	for _, direct := range []bool{true, false} {
		name := map[bool]string{true: "direct", false: "reverse"}[direct]
		t.Run(name, func(t *testing.T) {
			def, a, b := namedPanel(t, "default"), namedPanel(t, "A"), namedPanel(t, "B")
			o := v2Opts{direct: direct, min: 2, max: 4, perLink: 8, exitLinks: 2, exitMin: 2, exitMax: 4,
				panel: def, udp: true, userPorts: []int{8443, 2053, 2083},
				routes: map[int]string{2053: a, 2083: b, 9999: a}}
			r := startV2(t, o)
			r.waitFor("links up", 10*time.Second, func() bool { return r.lm.Stats().Links >= 2 })
			for port, want := range map[int]string{8443: "default", 2053: "A", 2083: "B"} {
				if got := tcpReaches(r.portAddr[port]); got != want {
					t.Errorf("tcp user port %d reached %q, want %q", port, got, want)
				}
				if got := udpReaches(r.portAddr[port]); got != want {
					t.Errorf("udp user port %d reached %q, want %q", port, got, want)
				}
			}
			// The untagged listener (port 0, as an older edge's) reaches the default.
			if got := tcpReaches(r.userAddr); got != "default" {
				t.Errorf("untagged connection reached %q, want default", got)
			}
			// Both servers see the other's side of the table.
			er := r.lm.Stats().Routes
			if er == nil || !er.Known || !er.Tags || !er.Default || fmt.Sprint(er.Ports) != "[2053 2083 9999]" {
				t.Errorf("edge view of the exit's table: %+v", er)
			}
			r.waitFor("exit learns the edge's ports", 5*time.Second, func() bool {
				x := r.exitView().Routes
				return x != nil && x.Known && x.UDP && fmt.Sprint(x.Ports) == "[2053 2083 8443]"
			})
		})
	}
}

func TestPortRoutesNoDefaultRefuses(t *testing.T) {
	a := namedPanel(t, "A")
	r := startV2(t, v2Opts{direct: true, min: 2, max: 4, perLink: 8, noPanel: true,
		userPorts: []int{8443, 2053}, routes: map[int]string{2053: a}})
	r.waitFor("links up", 10*time.Second, func() bool { return r.lm.Stats().Links >= 2 })
	if got := tcpReaches(r.portAddr[2053]); got != "A" {
		t.Fatalf("mapped port reached %q", got)
	}
	if got := tcpReaches(r.portAddr[8443]); got != "" {
		t.Fatalf("unmapped port with no default reached %q; it must be refused", got)
	}
	r.waitFor("the exit names the port", 3*time.Second, func() bool {
		return r.exit.count("Iran user port 8443 (tcp) has no target on this server", 0) == 1
	})
	tcpReaches(r.portAddr[8443]) // again within the minute: not logged again
	time.Sleep(200 * time.Millisecond)
	if n := r.exit.count("Iran user port 8443", 0); n != 1 {
		t.Fatalf("logged %d times; once a minute per port", n)
	}
	if er := r.lm.Stats().Routes; er == nil || er.Default {
		t.Fatalf("edge must see that the exit has no default: %+v", er)
	}
}

// Compatibility: a newer Iran server with several user ports in front of an
// OLDER kharej server, and an older Iran server in front of a newer kharej
// server with a port_map — both keep working exactly as before (everything
// reaches the one panel / the default), in both directions.
func TestPortRoutesMixedVersions(t *testing.T) {
	for _, direct := range []bool{true, false} {
		dir := map[bool]string{true: "direct", false: "reverse"}[direct]
		t.Run("older exit "+dir, func(t *testing.T) {
			panel := namedPanel(t, "panel")
			r := startV2(t, v2Opts{direct: direct, min: 2, max: 4, perLink: 8, exitLinks: 2, exitMin: 2, exitMax: 4,
				panel: panel, oldExit: true, userPorts: []int{8443, 2053}})
			r.waitFor("links up", 10*time.Second, func() bool { return r.lm.Stats().Links >= 2 })
			for _, port := range []int{8443, 2053} {
				if got := tcpReaches(r.portAddr[port]); got != "panel" {
					t.Errorf("port %d reached %q through an older exit, want its one panel", port, got)
				}
			}
			r.waitFor("the edge marks the exit older", 3*time.Second, func() bool {
				er := r.lm.Stats().Routes
				return er != nil && er.Older && !er.Known
			})
		})
		t.Run("older edge "+dir, func(t *testing.T) {
			def, a := namedPanel(t, "default"), namedPanel(t, "A")
			r := startV2(t, v2Opts{direct: direct, min: 2, max: 4, perLink: 8, exitLinks: 2, exitMin: 2, exitMax: 4,
				panel: def, oldEdge: true, userPorts: []int{2053}, routes: map[int]string{2053: a}})
			r.waitFor("links up", 10*time.Second, func() bool { return r.lm.Stats().Links >= 2 })
			if got := tcpReaches(r.portAddr[2053]); got != "default" {
				t.Errorf("an older edge's connection reached %q, want the default panel", got)
			}
			if x := r.exitView().Routes; x == nil || x.Known {
				t.Errorf("the exit must not claim to know an older edge's ports: %+v", x)
			}
		})
	}
}

// The pool's remembered exit answer (what a link whose own answer is late
// goes by) is forgotten when no link is left: the next links may reach an exit
// restarted with another table.
func TestExitInfoForgottenWithNoLinks(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 4, false)
	f := newMeteredFake()
	addManaged(m, f)
	m.exitInfo.Store(&peerInfo{V2: true, Caps: capPortTags, Flags: flagDefault, Ports: []int{2053}})
	m.reap()
	if m.exitInfo.Load() == nil {
		t.Fatal("forgotten while a link is still up")
	}
	f.alive.Store(false)
	m.reap()
	if m.exitInfo.Load() != nil {
		t.Fatal("still remembered with no link left")
	}
	g := newMeteredFake()
	m.AddLink(g, "x")
	m.exitInfo.Store(&peerInfo{V2: true})
	m.DropLink(g, "x")
	if m.exitInfo.Load() != nil {
		t.Fatal("reverse: still remembered after the last link was dropped")
	}
}
