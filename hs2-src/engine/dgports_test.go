package engine

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Per-port routing over the datagram tun (dgports.go). The tun is simulated by
// loopback addresses: the edge's user ports listen on one 127.0.0.x, the
// exit's "tun IP" is another, exactly as the forwarders see the real tun.

func startDgPair(t *testing.T, edgeIP, exitIP string, udp bool, def string, routes map[int]string, ports ...string) (edge, exit func() *PeerRoutes, logs *logSink) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs = &logSink{}
	var err error
	exit, err = StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: def, Routes: routes, UDP: udp, Log: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	edge, err = StartDgPorts(ctx, DgPortsConfig{Edge: true, Ports: ports, UserListenIP: edgeIP, PeerTunIP: exitIP, UDP: udp, Log: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	return edge, exit, logs
}

func dgWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDgPortsRouteEachPort(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.11", "127.0.0.12"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	p1, p2 := freePortOn(t, edgeIP), freePortOn(t, edgeIP)
	n2, _ := strconv.Atoi(p2)
	edge, exit, logs := startDgPair(t, edgeIP, exitIP, true, def, map[int]string{n2: a}, p1, p2)
	dgWait(t, "the edge's probe", func() bool { r := edge(); return r.Known && r.Tags })
	for port, want := range map[string]string{p1: "default", p2: "A"} {
		addr := net.JoinHostPort(edgeIP, port)
		if got := tcpReaches(addr); got != want {
			t.Errorf("tcp %s reached %q, want %q", port, got, want)
		}
		if got := udpReaches(addr); got != want {
			t.Errorf("udp %s reached %q, want %q", port, got, want)
		}
	}
	if r := edge(); !r.Default || fmt.Sprint(r.Ports) != fmt.Sprint([]int{n2}) {
		t.Errorf("edge view of the exit: %+v", r)
	}
	if r := exit(); !r.Known || !r.UDP || len(r.Ports) != 2 {
		t.Errorf("exit view of the edge: %+v", r)
	}
	if logs.count("per-port routing on", 0) != 1 {
		t.Errorf("the edge must say once that per-port routing is on:\n%s", logs)
	}
}

// A newer edge in front of an OLDER exit (it listens only on DgTunPort): the
// probe is refused, TCP falls back without losing the connection, UDP goes
// untagged, and every port reaches the one panel — as before.
func TestDgPortsOlderExit(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.13", "127.0.0.14"
	panel := namedPanel(t, "panel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &logSink{}
	// The previous release's exit: one forwarder, DgTunPort -> panel.
	if err := runForwarder(ctx, forwardTarget(exitIP, DgTunPort), panel, true, tunLeg{listen: true}, logs.logf); err != nil {
		t.Fatal(err)
	}
	p1 := freePortOn(t, edgeIP)
	edge, err := StartDgPorts(ctx, DgPortsConfig{Edge: true, Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP, UDP: true, Log: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort(edgeIP, p1)
	if got := tcpReaches(addr); got != "panel" { // may race the probe: must fall back either way
		t.Fatalf("tcp reached %q through an older exit", got)
	}
	dgWait(t, "the edge marks the exit older", func() bool { return edge().Older })
	if got := udpReaches(addr); got != "panel" {
		t.Fatalf("udp reached %q through an older exit", got)
	}
	if got := tcpReaches(addr); got != "panel" {
		t.Fatalf("tcp (known older) reached %q", got)
	}
}

// An OLDER edge (it sends every user port to DgTunPort) in front of a newer
// exit with a port_map: everything reaches the default panel, as before.
func TestDgPortsOlderEdge(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.15", "127.0.0.16"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	exit, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: def, Routes: map[int]string{n1: a}})
	if err != nil {
		t.Fatal(err)
	}
	// The previous release's edge: user port -> peer tun IP : DgTunPort.
	if err := runForwarder(ctx, net.JoinHostPort(edgeIP, p1), forwardTarget(exitIP, DgTunPort), false, tunLeg{dial: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tcpReaches(net.JoinHostPort(edgeIP, p1)); got != "default" {
		t.Fatalf("an older edge's connection reached %q, want the default panel", got)
	}
	if exit().Known {
		t.Fatal("the exit must not claim to know an older edge's ports")
	}
}

// Something that is not an hs2 exit answering on the tagged port never gets
// a user's bytes: the probe sees no hs2 answer and the edge stays untagged.
func TestDgPortsForeignListenerOnTagPort(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.17", "127.0.0.18"
	panel := namedPanel(t, "panel")
	foreign, err := net.Listen("tcp", net.JoinHostPort(exitIP, DgTagPort))
	if err != nil {
		t.Skip(err)
	}
	defer foreign.Close()
	go func() {
		for {
			c, err := foreign.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
			c.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runForwarder(ctx, forwardTarget(exitIP, DgTunPort), panel, false, tunLeg{listen: true}, nil); err != nil {
		t.Fatal(err)
	}
	p1 := freePortOn(t, edgeIP)
	edge, err := StartDgPorts(ctx, DgPortsConfig{Edge: true, Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP})
	if err != nil {
		t.Fatal(err)
	}
	dgWait(t, "the probe rejects the foreign answer", func() bool { return edge().Older })
	if got := tcpReaches(net.JoinHostPort(edgeIP, p1)); got != "panel" {
		t.Fatalf("reached %q", got)
	}
}

// An exit with port_map entries and no expose refuses an unmapped port and
// says which, once a minute.
func TestDgPortsNoDefault(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.19", "127.0.0.20"
	a := namedPanel(t, "A")
	p1, p2 := freePortOn(t, edgeIP), freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	edge, _, logs := startDgPair(t, edgeIP, exitIP, false, "", map[int]string{n1: a}, p1, p2)
	dgWait(t, "probe", func() bool { return edge().Known })
	if got := tcpReaches(net.JoinHostPort(edgeIP, p1)); got != "A" {
		t.Fatalf("mapped port reached %q", got)
	}
	if got := tcpReaches(net.JoinHostPort(edgeIP, p2)); got != "" {
		t.Fatalf("unmapped port with no default reached %q", got)
	}
	dgWait(t, "the exit names the port", func() bool {
		return logs.count("Iran user port "+p2+" (tcp) has no target on this server", 0) == 1
	})
	if edge().Default {
		t.Fatal("the edge must see that the exit has no default")
	}
}

// The tagged port does not answer at all while the untagged one does (a
// firewall on the kharej server's tun that lets only 28443 through): the probe
// turns per-port routing off instead of leaving every connection to time out,
// and connections reach the default panel.
func TestDgPortsTagPortFiltered(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.21", "127.0.0.22"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	old := dgDialFn
	dgDialFn = func(ctx context.Context, addr string, rcvbuf int, timeout time.Duration) (net.Conn, error) {
		if addr == net.JoinHostPort(exitIP, DgTagPort) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
		}
		return dgDial(ctx, addr, rcvbuf, timeout)
	}
	defer func() { dgDialFn = old }()
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	edge, _, logs := startDgPair(t, edgeIP, exitIP, false, def, map[int]string{n1: a}, p1)
	dgWait(t, "the probe sees the tagged port filtered", func() bool { r := edge(); return r.Filtered && !r.Older })
	if got := tcpReaches(net.JoinHostPort(edgeIP, p1)); got != "default" {
		t.Fatalf("reached %q; with the tagged port filtered it must be the default panel", got)
	}
	if logs.count("filtered there?", 0) != 1 {
		t.Fatalf("the edge must say once why per-port routing is off:\n%s", logs)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// Neither port answers (the tun is not up yet): unknown, nothing decided.
func TestDgPortsTunDownStaysUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &dgEdge{cfg: DgPortsConfig{PeerTunIP: "127.0.0.23", Log: func(string, ...any) {}}, kick: make(chan struct{}, 1),
		dial: func(context.Context, string, int, time.Duration) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
		}}
	if r := e.probe(ctx); r != dgTagUnknown || e.state.Load() != dgTagUnknown {
		t.Fatalf("probe %d, state %d; want unknown", r, e.state.Load())
	}
}

// Something on the tagged port accepts but never answers like an hs2 exit:
// the edge never sends it a user's bytes.
func TestDgPortsSilentListenerOnTagPort(t *testing.T) {
	const exitIP = "127.0.0.24"
	ln, err := net.Listen("tcp", net.JoinHostPort(exitIP, DgTagPort))
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, never answers
		}
	}()
	old := dgProbeAnswer
	dgProbeAnswer = 300 * time.Millisecond
	defer func() { dgProbeAnswer = old }()
	e := &dgEdge{cfg: DgPortsConfig{PeerTunIP: exitIP, Log: func(string, ...any) {}}, kick: make(chan struct{}, 1), dial: dgDial}
	if r := e.probe(context.Background()); r != dgTagUnknown || e.state.Load() != dgTagUnknown {
		t.Fatalf("probe %d, state %d; a listener that never answers decides nothing", r, e.state.Load())
	}
	if e.tagTCP(2053) || !e.undecided.Load() {
		t.Fatal("not known: nothing may be tagged (no user byte to an unproven listener), and connections must not wait for it")
	}
	// An exit that was answering stays "yes" through one slow answer.
	e.state.Store(dgTagYes)
	e.exit = &peerInfo{V2: true, Caps: capPortTags, Flags: flagDefault, Ports: []int{2053}}
	if r := e.probe(context.Background()); r != dgTagUnknown || e.state.Load() != dgTagYes || !e.tagTCP(2053) {
		t.Fatalf("probe %d, state %d: a late answer must not turn a known exit into an older one", r, e.state.Load())
	}
}

// One refused UDP flow (an exit without UDP on the tagged port) never turns
// per-port routing off for the whole edge: it only asks the probe.
func TestDgPortsRefusedUDPFlowKeepsState(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.29", "127.0.0.30" // nothing listens on exitIP
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	e := &dgEdge{cfg: DgPortsConfig{Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP, UDP: true, Log: func(string, ...any) {}},
		dial: dgDial, kick: make(chan struct{}, 1), mine: peerInfo{Caps: capPortTags}}
	e.exit = &peerInfo{V2: true, Caps: capPortTags, Flags: flagDefault, Ports: []int{n1}}
	e.udpTag.Store(true)
	e.state.Store(dgTagYes)
	if err := e.listen(ctx, p1, n1); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp", net.JoinHostPort(edgeIP, p1))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 20; i++ {
		c.Write([]byte("x"))
		time.Sleep(10 * time.Millisecond)
	}
	dgWait(t, "the refusal pokes the probe", func() bool { return len(e.kick) == 1 })
	if e.state.Load() != dgTagYes {
		t.Fatal("a refused UDP flow switched per-port routing off for the whole edge")
	}
}

// A UDP flow that began tagged is redone untagged as soon as the edge learns
// the exit no longer routes tags (and back) — it never stays on a dead path.
func TestDgPortsUDPFlowFollowsState(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.25", "127.0.0.26"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: def, Routes: map[int]string{n1: a}, UDP: true}); err != nil {
		t.Fatal(err)
	}
	e := &dgEdge{cfg: DgPortsConfig{Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP, UDP: true, Log: func(string, ...any) {}},
		dial: dgDial, kick: make(chan struct{}, 1), mine: peerInfo{Caps: capPortTags}}
	if err := e.listen(ctx, p1, n1); err != nil {
		t.Fatal(err)
	}
	e.exit = &peerInfo{V2: true, Caps: capPortTags, Flags: flagDefault, Ports: []int{n1}}
	e.udpTag.Store(true)
	e.state.Store(dgTagYes)
	c, err := net.Dial("udp", net.JoinHostPort(edgeIP, p1))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ask := func() string {
		b := make([]byte, 256)
		for i := 0; i < 10; i++ {
			c.Write([]byte("x"))
			c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			if n, err := c.Read(b); err == nil {
				name, _, _ := strings.Cut(string(b[:n]), "|")
				return name
			}
		}
		return ""
	}
	if got := ask(); got != "A" {
		t.Fatalf("tagged flow reached %q", got)
	}
	e.state.Store(dgTagNo) // e.g. the exit was rolled back
	if got := ask(); got != "default" {
		t.Fatalf("after the exit stopped routing tags the same client reached %q, want the default (flow redone untagged)", got)
	}
	e.state.Store(dgTagYes)
	if got := ask(); got != "A" {
		t.Fatalf("back to tagged: reached %q", got)
	}
}

// The tun comes up between the probe's two looks (the tagged port timed out,
// then the untagged one answered): the tagged port is asked again, and the
// exit is found — never called "filtered" by that race (it used to send every
// port to the default panel for the first seconds of a reverse tunnel).
func TestDgPortsTunUpMidProbe(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.27", "127.0.0.28"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: def, Routes: map[int]string{n1: a}}); err != nil {
		t.Fatal(err)
	}
	tagDials := 0
	e := &dgEdge{cfg: DgPortsConfig{PeerTunIP: exitIP, Log: func(string, ...any) {}}, kick: make(chan struct{}, 1),
		mine: peerInfo{Caps: capPortTags},
		dial: func(ctx context.Context, addr string, rcvbuf int, timeout time.Duration) (net.Conn, error) {
			if addr == net.JoinHostPort(exitIP, DgTagPort) {
				if tagDials++; tagDials == 1 { // the tun was not up yet
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
				}
			}
			return dgDial(ctx, addr, rcvbuf, timeout)
		}}
	if r := e.probe(ctx); r != dgTagYes || tagDials != 2 {
		t.Fatalf("probe %d after %d tagged dial(s); want yes after asking twice", r, tagDials)
	}
}

// The exit keys tagged UDP flows by the edge's source address; when the edge
// reuses a source port for ANOTHER user port, the stale flow is replaced, not
// the new datagrams dropped.
func TestDgPortsExitUDPFlowReuse(t *testing.T) {
	const exitIP = "127.0.0.31"
	a, b := namedPanel(t, "A"), namedPanel(t, "B")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Routes: map[int]string{2053: a, 2083: b}}); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp", net.JoinHostPort(exitIP, DgTagPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ask := func(port int) string {
		buf := make([]byte, 256)
		for i := 0; i < 10; i++ {
			c.Write([]byte{dgTagVer, byte(port >> 8), byte(port), 'x'})
			c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			if n, err := c.Read(buf); err == nil {
				name, _, _ := strings.Cut(string(buf[:n]), "|")
				return name
			}
		}
		return ""
	}
	if got := ask(2053); got != "A" {
		t.Fatalf("port 2053 reached %q", got)
	}
	if got := ask(2083); got != "B" {
		t.Fatalf("the same source address for port 2083 reached %q, want B (stale flow replaced)", got)
	}
}

// An exit with nothing to route still answers the probe, so the edge knows
// exactly that (and tags, so a refused port is named) instead of calling it
// an older hs2; and an edge with no user port still tells the exit so.
func TestDgPortsEmptySides(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.32", "127.0.0.33"
	p1 := freePortOn(t, edgeIP)
	edge, _, logs := startDgPair(t, edgeIP, exitIP, false, "", nil, p1)
	dgWait(t, "probe", func() bool { r := edge(); return r.Known && !r.Default && len(r.Ports) == 0 })
	if got := tcpReaches(net.JoinHostPort(edgeIP, p1)); got != "" {
		t.Fatalf("reached %q with no target anywhere", got)
	}
	dgWait(t, "the exit names the port", func() bool { return logs.count("Iran user port "+p1+" (tcp) has no target", 0) == 1 })

	const edge2, exit2 = "127.0.0.34", "127.0.0.35"
	_, exit, _ := startDgPair(t, edge2, exit2, false, namedPanel(t, "d"), nil)
	dgWait(t, "an edge without user ports still reports", func() bool { r := exit(); return r.Known && len(r.Ports) == 0 })
}

// One silence of the tagged port right after "yes" (a tun flap) is not taken
// for a filter; the second in a row is.
func TestDgPortsFlapIsNotFiltered(t *testing.T) {
	const exitIP = "127.0.0.38"
	tag := net.JoinHostPort(exitIP, DgTagPort)
	ln, err := net.Listen("tcp", net.JoinHostPort(exitIP, DgTunPort)) // the tun answers on 28443
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	e := &dgEdge{cfg: DgPortsConfig{PeerTunIP: exitIP, Log: func(string, ...any) {}}, kick: make(chan struct{}, 1),
		dial: func(ctx context.Context, addr string, rcvbuf int, timeout time.Duration) (net.Conn, error) {
			if addr == tag {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
			}
			return dgDial(ctx, addr, rcvbuf, timeout)
		}}
	e.exit = &peerInfo{V2: true, Caps: capPortTags, Flags: flagDefault}
	e.state.Store(dgTagYes)
	if r := e.probe(context.Background()); r != dgTagUnknown || e.state.Load() != dgTagYes {
		t.Fatalf("first silence after yes: probe %d, state %d; want no change", r, e.state.Load())
	}
	if r := e.probe(context.Background()); r != dgTagNo || !e.routes().Filtered {
		t.Fatalf("second silence in a row: probe %d, routes %+v; want filtered", r, e.routes())
	}
	// Filtered, then the port is refused (an older exit took over): older, not filtered.
	e.dial = func(context.Context, string, int, time.Duration) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	e.probe(context.Background())
	if r := e.routes(); !r.Older || r.Filtered {
		t.Fatalf("after a refusal: %+v; want older, not filtered", r)
	}
}

// The Kharej server is restarted with a NEW port_map; before the edge's next
// probe, a TCP connection on the newly mapped port already reaches its own
// target — the exit decides with the table it has now.
func TestDgPortsTCPFollowsExitTableAtOnce(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.39", "127.0.0.40"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx1, stop1 := context.WithCancel(ctx)
	if _, err := StartDgPorts(ctx1, DgPortsConfig{LocalTunIP: exitIP, Default: def}); err != nil {
		t.Fatal(err)
	}
	edge, err := StartDgPorts(ctx, DgPortsConfig{Edge: true, Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP})
	if err != nil {
		t.Fatal(err)
	}
	dgWait(t, "probe", func() bool { return edge().Known })
	stop1() // the kharej server restarts with port_map {p1: A}
	dgWait(t, "old exit gone", func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(exitIP, DgTagPort), 100*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
	if _, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: def, Routes: map[int]string{n1: a}}); err != nil {
		t.Fatal(err)
	}
	if got := tcpReaches(net.JoinHostPort(edgeIP, p1)); got != "A" {
		t.Fatalf("newly mapped port reached %q, want A without waiting for a probe", got)
	}
}

// Right after the Iran service starts the tun is not up yet: a connection
// waits for the first probe (an untagged dial would wait for the tun just the
// same) instead of going untagged to the default panel.
func TestDgPortsStartWaitsForTun(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.41", "127.0.0.42"
	def, a := namedPanel(t, "default"), namedPanel(t, "A")
	p1 := freePortOn(t, edgeIP)
	n1, _ := strconv.Atoi(p1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: def, Routes: map[int]string{n1: a}}); err != nil {
		t.Fatal(err)
	}
	up := time.Now().Add(3500 * time.Millisecond) // the tun comes up 3.5 s after start
	old := dgDialFn
	dgDialFn = func(ctx context.Context, addr string, rcvbuf int, timeout time.Duration) (net.Conn, error) {
		if time.Now().Before(up) {
			time.Sleep(50 * time.Millisecond)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
		}
		return dgDial(ctx, addr, rcvbuf, timeout)
	}
	defer func() { dgDialFn = old }()
	if _, err := StartDgPorts(ctx, DgPortsConfig{Edge: true, Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP}); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(edgeIP, p1), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	b := make([]byte, 16)
	n, _ := c.Read(b)
	if got, _, _ := strings.Cut(string(b[:n]), "|"); got != "A" {
		t.Fatalf("a connection made before the tun was up reached %q, want A", got)
	}
}
