package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Per-port routing over the datagram tun (routes.go has the model).
//
// Over the tun the edge reaches the exit with ordinary TCP/UDP to the exit's
// tun address. Untagged traffic keeps going to DgTunPort (28443), which the
// exit hands to its default panel exactly as before. Port-tagged traffic goes
// to DgTagPort (28444), where every TCP connection and every UDP datagram
// starts with [1][user port u16]:
//
//	edge:  user -> <user_listen_ip>:P  ->  <peer_tun_ip>:28444  [1][P] + bytes
//	exit:  <local_tun_ip>:28444  -> its table: port_map[P], else expose
//
// Once the probe found an exit that routes tags, every TCP connection is
// tagged (3 bytes once per connection) and the exit decides with the table it
// has NOW — a port mapped there a moment ago included. UDP datagrams carry the
// tag only when the exit has a table at all (a port_map, or no default): an
// exit without one gets UDP byte for byte as before, with no 3-byte header on
// datagrams near the tun MTU.
//
// A TCP connection to DgTagPort that says port 0 is the PROBE: the two sides
// swap a kindInfo message on it (what each one has) and close. The edge
// probes when it starts and every dgProbeEvery (also with no user port, so the
// exit learns that too); faster while it does not know yet or just found the
// exit not routing tags. An exit that refuses the port (an older hs2), that
// answers but not like an hs2 exit, or that is silent on it while DgTunPort
// answers (filtered on the kharej server's tun) gets everything untagged, as
// before; one silence right after a "yes" is taken for a flap until the next
// probe says it again. An answer that is only late changes nothing. Until the
// first probe has answered nothing is tagged: a connection waits for it while
// the tun is not up (an untagged dial would wait just the same), and goes
// untagged at once when the tun answers without a verdict.
// A tagged TCP connection that is refused falls back to DgTunPort before any
// user byte is sent; a UDP flow is redone, from its next datagram, whenever
// what is known changes. UDP is always offered on both exit ports, so turning
// UDP on needs only the Iran server.

// DgTagPort is the on-tun port for port-tagged traffic. Like DgTunPort it is
// private to the tunnel and is not a port panels use.
const DgTagPort = "28444"

const (
	dgTagVer       = 1
	dgProbeEvery   = 15 * time.Second
	dgProbeRetry   = 5 * time.Second
	dgPeerStale    = 3 * dgProbeEvery // an exit forgets an edge report this old
	dgFastAfterNo  = 2 * time.Minute
	dgProbeDial    = 3 * time.Second  // a probe's connect over the tun
	dgProbeUnknown = 2 * time.Second  // between probes while the tun has not answered yet
	dgUnknownWait  = 10 * time.Second // a user connection waits at most this long for the first probe (the forwarder's own dial timeout)
)

// dgTagState is what the edge knows of the exit.
const (
	dgTagUnknown int32 = iota // not probed yet (or the tun was down): try tagged, fall back
	dgTagYes                  // the exit answered the probe: tag
	dgTagNo                   // older exit (refused / not an hs2 answer): untagged
)

// DgPortsConfig is one side's port forwarding over the datagram tun.
type DgPortsConfig struct {
	Edge bool
	// Edge: the user ports it opens, the IP they listen on, and the peer's tun IP.
	Ports        []string
	UserListenIP string
	PeerTunIP    string
	// Exit: its tun IP and its table (Default = expose, Routes = port_map).
	LocalTunIP string
	Default    string
	Routes     map[int]string
	UDP        bool
	Log        func(string, ...any)
}

// StartDgPorts starts the forwarders for one side and returns what the other
// side reported about its routing, for the status display.
func StartDgPorts(ctx context.Context, cfg DgPortsConfig) (func() *PeerRoutes, error) {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Edge {
		return startDgEdge(ctx, cfg)
	}
	return startDgExit(ctx, cfg)
}

// ---- exit -------------------------------------------------------------------

type dgExit struct {
	cfg     DgPortsConfig
	table   RouteTable
	noRoute noRouteLog
	addrs   udpAddrs
	mu      sync.Mutex
	edge    *peerInfo // the last probe's report from the edge
	edgeAt  time.Time
}

// udpAddrs resolves UDP targets once and keeps them, so a target given as a
// name never costs a DNS lookup per flow on the one goroutine that reads every
// tagged datagram. A failed lookup is retried after udpAddrRetry.
type udpAddrs struct {
	mu sync.Mutex
	m  map[string]udpAddrEntry
}

type udpAddrEntry struct {
	a  *net.UDPAddr
	at time.Time
}

const udpAddrRetry = 30 * time.Second

func (u *udpAddrs) get(target string) *net.UDPAddr {
	u.mu.Lock()
	e, ok := u.m[target]
	u.mu.Unlock()
	if ok && (e.a != nil || time.Since(e.at) < udpAddrRetry) {
		return e.a
	}
	a, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		a = nil
	}
	u.mu.Lock()
	if u.m == nil {
		u.m = map[string]udpAddrEntry{}
	}
	u.m[target] = udpAddrEntry{a, time.Now()}
	u.mu.Unlock()
	return a
}

// prime resolves the table's targets in the background at start.
func (u *udpAddrs) prime(ctx context.Context, def string, routes map[int]string) {
	go func() {
		if def != "" {
			u.get(def)
		}
		for _, t := range routes {
			if ctx.Err() != nil {
				return
			}
			u.get(t)
		}
	}()
}

func startDgExit(ctx context.Context, cfg DgPortsConfig) (func() *PeerRoutes, error) {
	x := &dgExit{cfg: cfg, table: RouteTable{Default: cfg.Default, Ports: cfg.Routes}}
	x.addrs.prime(ctx, cfg.Default, cfg.Routes)
	if cfg.Default != "" {
		// Untagged (an older edge, or a port without its own target): the
		// default panel, exactly as before.
		addr := forwardTarget(cfg.LocalTunIP, DgTunPort)
		if err := runForwarder(ctx, addr, cfg.Default, false, tunLeg{listen: true}, cfg.Log); err != nil {
			return nil, err
		}
		// UDP is always offered here, so turning UDP on needs only the Iran
		// server; asked for in this config ("udp"), a failure stops the start
		// as it always did, otherwise it is only said.
		if err := listenUDPProxy(ctx, addr, cfg.Default, cfg.Log); err != nil {
			if cfg.UDP {
				return nil, err
			}
			cfg.Log("dg: UDP on tun port %s is not available (%v) — UDP from the Iran server's user ports cannot be forwarded", DgTunPort, err)
		}
		cfg.Log("dg: tun port %s -> panel %s (every user port without its own target)", DgTunPort, cfg.Default)
	}
	// The tagged port is an addition: if it cannot be opened, the tunnel still
	// runs as before (every port reaches the default) and says why. It is
	// opened even with nothing to route, so the probe can tell the Iran server
	// exactly that (and learn its user ports) instead of looking like an
	// older hs2.
	if err := x.listenTagged(ctx); err != nil {
		cfg.Log("dg: per-port routing is OFF: cannot open tun port %s (%v) — every Iran user port reaches the default panel", DgTagPort, err)
		return x.routes, nil
	}
	if len(cfg.Routes) > 0 {
		cfg.Log("dg: tun port %s -> per-port targets (%d port(s) with their own target, others -> %s)", DgTagPort, len(cfg.Routes), orNone(cfg.Default))
	}
	return x.routes, nil
}

// listenUDPProxy forwards UDP arriving on addr to target (one upstream socket
// per client), until ctx ends.
func listenUDPProxy(ctx context.Context, addr, target string, logf func(string, ...any)) error {
	// proxyUDP resolves the target once; if that fails it would stop with
	// its socket still bound, swallowing UDP silently — so check first.
	if _, err := net.ResolveUDPAddr("udp", target); err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); pc.Close() }()
	go proxyUDP(ctx, pc, target, logf)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "refused (no expose)"
	}
	return s
}

// mine is what the exit tells the edge in the probe.
func (x *dgExit) mine() peerInfo {
	pi := peerInfo{Caps: capPortTags, Ports: x.table.MappedPorts()}
	if x.table.Default != "" {
		pi.Flags |= flagDefault
	}
	return pi
}

func (x *dgExit) routes() *PeerRoutes {
	x.mu.Lock()
	defer x.mu.Unlock()
	r := &PeerRoutes{}
	if x.edge != nil && time.Since(x.edgeAt) < dgPeerStale {
		r.Known = true
		r.Ports = append([]int(nil), x.edge.Ports...)
		sort.Ints(r.Ports) // as the stream exit shows them
		r.UDP = x.edge.Flags&flagUDP != 0
		r.Cut = x.edge.Flags&flagCut != 0
	}
	return r
}

func (x *dgExit) listenTagged(ctx context.Context) error {
	addr := forwardTarget(x.cfg.LocalTunIP, DgTagPort)
	ln, err := listenReuseRcvBuf(addr, dgTunRcvBuf)
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", addr) // always offered (see startDgExit)
	if err != nil {
		x.cfg.Log("dg: UDP on tun port %s is not available (%v) — UDP of ports with their own target cannot be forwarded", DgTagPort, err)
		pc = nil
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		if pc != nil {
			pc.Close()
		}
	}()
	go func() {
		var bo acceptBackoff // a transient EMFILE/ENOBUFS must not close the port for good
		for {
			c, err := ln.Accept()
			if err != nil {
				if !bo.wait(ctx, err, x.cfg.Log, "dgtun tagged port") {
					return
				}
				continue
			}
			bo.ok()
			go x.serveTCP(ctx, c)
		}
	}()
	if pc != nil {
		go x.serveUDP(ctx, pc)
	}
	return nil
}

// serveTCP reads one connection's tag and delivers it, or answers a probe.
func (x *dgExit) serveTCP(ctx context.Context, c net.Conn) {
	c.SetReadDeadline(time.Now().Add(kindTimeout))
	var h [3]byte
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != dgTagVer {
		c.Close()
		return
	}
	port := int(h[1])<<8 | int(h[2])
	if port == 0 { // the probe
		defer c.Close()
		c.SetDeadline(time.Now().Add(infoTimeout))
		pi, err := readInfo(c)
		if err != nil {
			return
		}
		x.mu.Lock()
		x.edge, x.edgeAt = &pi, time.Now()
		x.mu.Unlock()
		c.Write(encodeInfo(x.mine()))
		return
	}
	c.SetReadDeadline(time.Time{})
	target, ok := x.table.Target(port)
	if !ok {
		x.noRoute.note(port, "tcp", x.cfg.Log)
		c.Close()
		return
	}
	proxyTCP(ctx, c, target, 0)
}

// serveUDP delivers tagged datagrams: one upstream socket per edge flow (the
// edge uses one source socket per user flow and user port), the target chosen
// from the first datagram's tag; replies go back untagged.
func (x *dgExit) serveUDP(ctx context.Context, pc net.PacketConn) {
	type flow struct {
		up   *net.UDPConn
		port int
		last time.Time
	}
	var mu sync.Mutex
	flows := map[string]*flow{}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			for k, f := range flows {
				if time.Since(f.last) > udpProxyIdle {
					f.up.Close()
					delete(flows, k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, caddr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 3 || buf[0] != dgTagVer {
			continue
		}
		port := int(buf[1])<<8 | int(buf[2])
		key := caddr.String()
		mu.Lock()
		f := flows[key]
		if f != nil && f.port != port {
			// The edge reused this source port for another user port (its
			// old flow ended): this flow is stale, start the new one.
			f.up.Close()
			delete(flows, key)
			f = nil
		}
		mu.Unlock()
		if f == nil {
			target, ok := x.table.Target(port)
			if !ok {
				x.noRoute.note(port, "udp", x.cfg.Log)
				continue
			}
			ua := x.addrs.get(target)
			if ua == nil {
				continue
			}
			up, err := net.DialUDP("udp", nil, ua)
			if err != nil {
				continue
			}
			f = &flow{up: up, port: port, last: time.Now()}
			mu.Lock()
			flows[key] = f
			mu.Unlock()
			go func(f *flow, caddr net.Addr) {
				rb := make([]byte, 65535)
				for {
					f.up.SetReadDeadline(time.Now().Add(udpProxyIdle))
					m, err := f.up.Read(rb)
					if err != nil {
						break
					}
					pc.WriteTo(rb[:m], caddr)
					mu.Lock()
					f.last = time.Now()
					mu.Unlock()
				}
				f.up.Close()
				mu.Lock()
				if flows[key] == f {
					delete(flows, key)
				}
				mu.Unlock()
			}(f, caddr)
		}
		f.up.Write(buf[3:n])
		mu.Lock()
		f.last = time.Now()
		mu.Unlock()
	}
}

// ---- edge -------------------------------------------------------------------

type dgEdge struct {
	cfg      DgPortsConfig
	filtered atomic.Bool // the last "no" was a silent tagged port, not an older exit
	// undecided: the tun answers but the last probe got no verdict (accepted,
	// no answer in time) — a connection then goes untagged at once instead of
	// waiting. silent: tagged-port silences in a row while the tun answered
	// (one alone, from "yes", is taken for a flap, not a filter).
	undecided atomic.Bool
	silent    atomic.Int32
	// udpTag: the exit has a table (port_map, a cut list, or no default), so
	// UDP datagrams carry the tag; without one they go untagged, exactly as
	// before (3 bytes per datagram only where they change the destination).
	udpTag atomic.Bool
	dial   func(ctx context.Context, addr string, rcvbuf int, timeout time.Duration) (net.Conn, error)
	state  atomic.Int32
	since  atomic.Int64 // unixnano of the last state change
	kick   chan struct{}
	mu     sync.Mutex
	exit   *peerInfo // the last probe's answer
	mine   peerInfo
}

func startDgEdge(ctx context.Context, cfg DgPortsConfig) (func() *PeerRoutes, error) {
	e := &dgEdge{cfg: cfg, dial: dgDialFn, kick: make(chan struct{}, 1), mine: peerInfo{Caps: capPortTags}}
	for _, p := range cfg.Ports {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			e.mine.Ports = append(e.mine.Ports, n)
		}
	}
	if cfg.UDP {
		e.mine.Flags |= flagUDP
	}
	// The probe runs even with no user port, so the exit learns that too.
	go e.probeLoop(ctx)
	for _, p := range cfg.Ports {
		port, _ := strconv.Atoi(p)
		if err := e.listen(ctx, p, port); err != nil {
			return nil, err
		}
		cfg.Log("dg: user port %s open, forwarded over the tun to the kharej server", p)
	}
	return e.routes, nil
}

func (e *dgEdge) routes() *PeerRoutes {
	r := &PeerRoutes{}
	switch e.state.Load() {
	case dgTagNo:
		r.Filtered = e.filtered.Load()
		r.Older = !r.Filtered
	case dgTagYes:
		e.mu.Lock()
		if pi := e.exit; pi != nil {
			r = exitRoutes(pi)
		}
		e.mu.Unlock()
	}
	return r
}

func (e *dgEdge) tagAddr() string  { return forwardTarget(e.cfg.PeerTunIP, DgTagPort) }
func (e *dgEdge) dfltAddr() string { return forwardTarget(e.cfg.PeerTunIP, DgTunPort) }

func (e *dgEdge) poke() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// probeLoop learns whether the exit routes tagged traffic: at start, every
// dgProbeEvery, and whenever a connection found the tagged port refused. It
// probes every dgProbeRetry while the tun is not answering, and for
// dgFastAfterNo after the exit stopped routing tags — a kharej server being
// upgraded or restarted is picked up within seconds, not a minute.
func (e *dgEdge) probeLoop(ctx context.Context) {
	for {
		wait := dgProbeEvery
		switch r := e.probe(ctx); {
		case r == dgTagUnknown:
			wait = dgProbeUnknown
		case r == dgTagNo && time.Since(time.Unix(0, e.since.Load())) < dgFastAfterNo:
			wait = dgProbeRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
		case <-time.After(wait):
		}
	}
}

// probe runs one probe and records (and returns) what it found:
//
//   - the tagged port answers like an hs2 exit that routes tags: yes;
//   - it refuses (an older exit does not listen there): no;
//   - something answers, but not like an hs2 exit: no;
//   - something accepts but does not answer in time (a congested tun, or a
//     listener that never answers): nothing decided — the state stays as it
//     was (unknown tags nothing) and it is asked again soon;
//   - nothing answers on it: if the untagged port does not answer either, the
//     tun is not up yet: unknown, state unchanged; if it does, the tagged port
//     is asked once more, and still silent means it is filtered (a firewall
//     on the kharej server's tun?): no.
func (e *dgEdge) probe(ctx context.Context) int32 {
	c, err := e.dial(ctx, e.tagAddr(), 0, dgProbeDial)
	if err != nil && !isRefused(err) && ctx.Err() == nil {
		// No answer. If the untagged port does not answer either, the tun is
		// not carrying (yet): nothing is decided. If it does, the tun carries
		// NOW (it may have come up a moment ago): ask the tagged port again
		// before calling it filtered.
		if !e.answers(ctx, e.dfltAddr()) {
			e.undecided.Store(false)
			return dgTagUnknown
		}
		if c, err = e.dial(ctx, e.tagAddr(), 0, dgProbeDial); err != nil && !isRefused(err) {
			if e.state.Load() == dgTagYes && e.silent.Add(1) < 2 {
				return dgTagUnknown // one silence after "yes": a flap until the next probe says it again
			}
			e.silent.Store(0)
			if !e.filtered.Swap(true) {
				e.cfg.Log("dg: the kharej server's tun answers on port %s but not on %s — filtered there? per-port routing stays off until it answers", DgTunPort, DgTagPort)
			}
			e.setState(dgTagNo)
			return dgTagNo
		}
	}
	if err != nil {
		if isRefused(err) {
			e.older()
			return dgTagNo
		}
		return dgTagUnknown
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(dgProbeAnswer))
	msg := append([]byte{dgTagVer, 0, 0}, encodeInfo(e.mine)...)
	pi, err := peerInfo{}, error(nil)
	if _, err = c.Write(msg); err == nil {
		pi, err = readInfo(c)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		// Accepted, but no answer in time: a congested tun, or a listener
		// that never answers. Nothing is decided — what was known stays
		// (unknown sends nothing tagged, so no user byte goes to a listener
		// that has not proved what it is) — and it is asked again soon.
		e.undecided.Store(true)
		return dgTagUnknown
	}
	if err != nil || !pi.tags() {
		// Something answered on the port, but not like an hs2 exit that
		// routes tags: never send it users' bytes.
		e.older()
		return dgTagNo
	}
	e.mu.Lock()
	e.exit = &pi
	e.mu.Unlock()
	e.udpTag.Store(pi.Flags&flagCut != 0 || pi.Flags&flagDefault == 0 || len(pi.Ports) > 0)
	e.silent.Store(0)
	e.undecided.Store(false)
	e.setState(dgTagYes)
	return dgTagYes
}

// older records an exit that does not route tags (refused the port, or did not
// answer like an hs2 exit): untagged from now on, and said as "older".
func (e *dgEdge) older() {
	e.filtered.Store(false)
	e.silent.Store(0)
	e.undecided.Store(false)
	e.setState(dgTagNo)
}

// answers reports whether addr on the peer's tun answers at all (accepts or
// refuses), i.e. whether the tun carries.
func (e *dgEdge) answers(ctx context.Context, addr string) bool {
	c, err := e.dial(ctx, addr, 0, dgProbeDial)
	if err == nil {
		c.Close()
		return true
	}
	return isRefused(err)
}

// waitKnown waits up to d for the probe to say yes or no.
// It stops early when the probe found the tun answering without a verdict
// (undecided): waiting longer would only delay the connection.
func (e *dgEdge) waitKnown(ctx context.Context, d time.Duration) {
	deadline := time.Now().Add(d)
	for e.state.Load() == dgTagUnknown && !e.undecided.Load() && time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *dgEdge) setState(s int32) {
	if s != dgTagNo {
		e.filtered.Store(false)
		e.silent.Store(0)
	}
	if old := e.state.Swap(s); old != s {
		e.since.Store(time.Now().UnixNano())
		switch s {
		case dgTagYes:
			e.cfg.Log("dg: the kharej server routes each user port to its own target (per-port routing on)")
		case dgTagNo:
			if !e.filtered.Load() {
				e.cfg.Log("dg: the kharej server does not route by user port (an older hs2) — every user port reaches its default panel")
			}
		}
	}
}

func isRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// dgDialFn / dgProbeAnswer: variables so tests can stand in for a filtered
// port and shorten the wait for a probe's answer.
var (
	dgDialFn      = dgDial
	dgProbeAnswer = infoTimeout
)

// dgDial dials addr over the tun: rcvbuf as forwardDialer, timeout 0 = its
// default.
func dgDial(ctx context.Context, addr string, rcvbuf int, timeout time.Duration) (net.Conn, error) {
	d := forwardDialer(rcvbuf)
	if timeout > 0 {
		d.Timeout = timeout
	}
	return d.DialContext(ctx, "tcp", addr)
}

func (e *dgEdge) listen(ctx context.Context, p string, port int) error {
	addr := forwardTarget(e.cfg.UserListenIP, p)
	ln, err := listenReuseRcvBuf(addr, 0)
	if err != nil {
		return err
	}
	var pc net.PacketConn
	if e.cfg.UDP {
		if pc, err = net.ListenPacket("udp", addr); err != nil {
			ln.Close()
			return err
		}
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		if pc != nil {
			pc.Close()
		}
	}()
	go func() {
		var bo acceptBackoff // a transient EMFILE/ENOBUFS must not close the port for good
		for {
			c, err := ln.Accept()
			if err != nil {
				if !bo.wait(ctx, err, e.cfg.Log, "user port "+p) {
					return
				}
				continue
			}
			bo.ok()
			go e.serveTCP(ctx, c, port)
		}
	}()
	if pc != nil {
		go e.serveUDP(ctx, pc, port)
	}
	return nil
}

// tagTCP reports whether a TCP connection is tagged: always, once the probe
// found an exit that routes tags — the exit then decides with the table it has
// NOW (a port mapped there a moment ago included), at the cost of 3 bytes once
// per connection. tagUDP: the same, but only when the exit has a table at all
// (udpTag), since there the 3 bytes ride on every datagram; an exit without
// one gets UDP exactly as before. Unknown and "no" tag nothing.
func (e *dgEdge) tagTCP(port int) bool {
	return port > 0 && e.state.Load() == dgTagYes
}

func (e *dgEdge) tagUDP(port int) bool {
	return port > 0 && e.state.Load() == dgTagYes && e.udpTag.Load()
}

// serveTCP carries one user connection. Before the first probe has answered
// (the first seconds, while the tun comes up) it waits for it — an untagged
// dial would wait for the tun just the same — but not when the tun answers
// without a verdict. A refused tagged port (the exit was replaced by an older
// one) falls back to the untagged one before any user byte has been read.
func (e *dgEdge) serveTCP(ctx context.Context, user net.Conn, port int) {
	if port > 0 && e.state.Load() == dgTagUnknown {
		e.poke()
		e.waitKnown(ctx, dgUnknownWait)
	}
	if e.tagTCP(port) {
		up, err := e.dial(ctx, e.tagAddr(), dgTunRcvBuf, 0)
		if err == nil {
			if _, err = up.Write([]byte{dgTagVer, byte(port >> 8), byte(port)}); err == nil {
				relay(user, up)
				return
			}
			up.Close()
			user.Close()
			return
		}
		if !isRefused(err) {
			user.Close() // the tun is not carrying: the untagged port would not either
			e.poke()     // ...or the tagged port went silent: the probe finds out which
			return
		}
		e.older()
		e.poke()
	}
	proxyTCP(ctx, user, e.dfltAddr(), dgTunRcvBuf)
}

// serveUDP forwards one user port's datagrams: one upstream socket per client,
// tagged or not as the exit is known at the flow's first datagram.
func (e *dgEdge) serveUDP(ctx context.Context, pc net.PacketConn, port int) {
	type flow struct {
		up     *net.UDPConn
		tagged bool
		last   time.Time
	}
	var mu sync.Mutex
	flows := map[string]*flow{}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			for k, f := range flows {
				if time.Since(f.last) > udpProxyIdle {
					f.up.Close()
					delete(flows, k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65535)
	hdr := []byte{dgTagVer, byte(port >> 8), byte(port)}
	for {
		n, caddr, err := pc.ReadFrom(buf[3:])
		if err != nil {
			return
		}
		key := caddr.String()
		want := e.tagUDP(port)
		mu.Lock()
		f := flows[key]
		if f != nil && f.tagged != want {
			// What the exit does changed (found older, filtered, or upgraded)
			// since this flow began: redo it the right way from this datagram.
			f.up.Close()
			delete(flows, key)
			f = nil
		}
		mu.Unlock()
		if f == nil {
			tagged := want
			to := e.dfltAddr()
			if tagged {
				to = e.tagAddr()
			}
			ua, err := net.ResolveUDPAddr("udp", to)
			if err != nil {
				continue
			}
			up, err := net.DialUDP("udp", nil, ua)
			if err != nil {
				continue
			}
			f = &flow{up: up, tagged: tagged, last: time.Now()}
			mu.Lock()
			flows[key] = f
			mu.Unlock()
			go func(f *flow, caddr net.Addr) {
				rb := make([]byte, 65535)
				for {
					f.up.SetReadDeadline(time.Now().Add(udpProxyIdle))
					m, err := f.up.Read(rb)
					if err != nil {
						if f.tagged && isRefused(err) {
							// The tagged port refused this flow (an exit replaced
							// by an older one, or one without UDP): ask the probe,
							// which decides — a UDP error alone never turns
							// per-port routing off for the whole edge.
							e.poke()
						}
						break
					}
					pc.WriteTo(rb[:m], caddr)
					mu.Lock()
					f.last = time.Now()
					mu.Unlock()
				}
				f.up.Close()
				mu.Lock()
				if flows[key] == f {
					delete(flows, key)
				}
				mu.Unlock()
			}(f, caddr)
		}
		if f.tagged {
			copy(buf[:3], hdr)
			f.up.Write(buf[:3+n])
		} else {
			f.up.Write(buf[3 : 3+n])
		}
		mu.Lock()
		f.last = time.Now()
		mu.Unlock()
	}
}
