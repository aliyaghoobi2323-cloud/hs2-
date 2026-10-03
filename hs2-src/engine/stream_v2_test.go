package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// Integration rig for the v2 link pool over real TLS on the loopback: an edge
// LinkManager built by hand (so a test can pin its target, shorten its clocks
// and read its internals) and an exit that is either the real one (RunKharej)
// or a faithful copy of the previous release's (no kindStats). Reverse or
// direct.

// logSink collects one side's log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) logf(f string, a ...any) {
	line := fmt.Sprintf(f, a...)
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
}

func (s *logSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *logSink) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines)
}

// count returns how many lines from index from on contain sub.
func (s *logSink) count(sub string, from int) int {
	n := 0
	for i, l := range s.snapshot() {
		if i >= from && strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// first returns the index of the first line at or after from containing every
// one of subs, or -1.
func (s *logSink) first(from int, subs ...string) int {
	for i, l := range s.snapshot() {
		if i < from {
			continue
		}
		ok := true
		for _, sub := range subs {
			if !strings.Contains(l, sub) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func (s *logSink) String() string { return strings.Join(s.snapshot(), "\n") }

type v2Opts struct {
	direct    bool // edge dials (NewMTCPDialer) and the exit listens; else reverse
	min, max  int  // edge envelope
	perLink   int
	pin       int // > 0: the edge's serving target is pinned (test hook)
	target    int // > 0: the target the edge publishes before its first tick
	exitLinks int // reverse exit: initial count (RevLinks), min and max
	exitMin   int
	exitMax   int
	exitCfg   int               // direct exit: its MaxLinks (reported over kindInfo, not applied)
	drainIdle time.Duration     // > 0: idle reclaim on retiring links
	panel     string            // "" = an echo panel
	oldExit   bool              // exit of the previous release: no kindStats case, no meter
	oldEdge   bool              // edge speaks only kindPool: no kindCtrl, no kindStats
	tune      func(*apTunables) // shorten the controller's clocks

	// Per-port routing: the edge opens one user listener per port (TCP, and
	// UDP when udp) and tags its connections; the exit routes by routes, then
	// panel (noPanel: no default — an unmapped port is refused).
	userPorts []int
	udp       bool
	routes    map[int]string
	noPanel   bool
}

type v2Rig struct {
	t        *testing.T
	lm       *LinkManager
	userAddr string
	portAddr map[int]string // user port -> its listener (TCP and UDP)
	dials    atomic.Int64   // links dialled: by the exit (reverse) or the edge (direct)
	edge     *logSink
	exit     *logSink
	exitFn   atomic.Pointer[StatsFn]
}

// exitView is the exit's own view of the pool (live links; target in reverse).
func (r *v2Rig) exitView() PoolStats {
	if f := r.exitFn.Load(); f != nil {
		return (*f)()
	}
	return PoolStats{Links: -1}
}

func startV2(t *testing.T, o v2Opts) *v2Rig {
	t.Helper()
	// Surplus links an exit dials are kept 30 s in production (they may be
	// replacing a link that died unnoticed); these tests watch many resizes.
	old := bornSpareGrace
	bornSpareGrace = 3 * time.Second
	t.Cleanup(func() { bornSpareGrace = old })
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	r := &v2Rig{t: t, edge: &logSink{}, exit: &logSink{}}
	t.Cleanup(cancel)
	t.Cleanup(func() { // runs first (LIFO): dump the logs of a failed run
		if t.Failed() || os.Getenv("HS2_TEST_LOGS") != "" {
			if r.lm != nil {
				t.Logf("edge stats at the end: %+v\nlinks: %+v", r.lm.Stats(), r.rows())
			}
			t.Logf("edge log:\n%s", r.edge)
			t.Logf("exit log:\n%s", r.exit)
		}
	})
	panel := o.panel
	if panel == "" && !o.noPanel {
		panel = echoPanel(t)
	}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	carrierAddr := ln.Addr().String()
	setExit := func(f StatsFn) { r.exitFn.Store(&f) }

	var lm *LinkManager
	if o.direct {
		if o.oldExit {
			var live atomic.Int32
			setExit(func() PoolStats { return PoolStats{Links: int(live.Load()), Phase: "listening"} })
			go runOldExitListener(ctx, ln, srv, panel, &live, r.exit.logf)
		} else {
			go RunKharej(ctx, KharejConfig{Listener: ln, Server: srv, Panel: panel, Routes: o.routes, Log: r.exit.logf, OnStart: setExit, MaxLinks: o.exitCfg})
		}
		d := &countingDialer{d: NewMTCPDialer(carrierAddr, "lab.example.com", key, ""), n: &r.dials}
		lm = NewLinkManager(d, o.min, o.max, o.perLink, r.edge.logf)
	} else {
		lm = NewLinkManager(nil, o.min, o.max, o.perLink, r.edge.logf)
		lm.accept = true
	}
	r.lm = lm
	if o.drainIdle > 0 {
		lm.SetDrainIdle(o.drainIdle)
	}
	if o.pin > 0 {
		lm.pin.Store(int32(o.pin))
	}
	if o.target > 0 {
		lm.target.Store(int32(o.target))
	}
	if o.tune != nil {
		o.tune(&lm.ap.tun)
	}
	// As RunIran: a link takes user connections once its kindInfo exchange is
	// over (an older edge has no exchange to wait for).
	lm.gateInfo = !o.oldEdge
	mine := peerInfo{MaxLinks: lm.max, Caps: capPortTags, Ports: o.userPorts}
	if o.udp {
		mine.Flags |= flagUDP
	}
	lm.OnLink = func(l Link) {
		if !o.oldEdge {
			go openControl(ctx, l, r.edge.logf)
			go openStats(ctx, l, r.edge.logf)
			go openInfo(ctx, l, mine)
		}
		if !o.direct {
			go openPoolCtl(ctx, l, lm, r.edge.logf, func() { lm.markPoolRefused(l) })
		}
	}
	go lm.Run(ctx)

	if !o.direct {
		go acceptReverseLinks(ctx, ln, srv, lm, r.edge.logf)
		dial := func() (*tlscarrier.Carrier, error) {
			r.dials.Add(1)
			return tlscarrier.DialFrom(carrierAddr, "lab.example.com", key, "")
		}
		if o.oldExit {
			pool := runOldExitReverse(ctx, panel, o.exitLinks, o.exitMin, o.exitMax, dial, r.exit.logf)
			setExit(pool.stats)
		} else {
			go RunKharej(ctx, KharejConfig{
				Panel: panel, Routes: o.routes, RevLinks: o.exitLinks, RevMin: o.exitMin, RevMax: o.exitMax,
				RevDial: dial, Log: r.exit.logf, OnStart: setExit,
			})
		}
	}

	userLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.userAddr = userLn.Addr().String()
	go func() { <-ctx.Done(); userLn.Close() }()
	go func() {
		for {
			c, err := userLn.Accept()
			if err != nil {
				return
			}
			go serveUserTCP(ctx, c, lm, 0)
		}
	}()
	r.portAddr = map[int]string{}
	for _, port := range o.userPorts {
		pl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		r.portAddr[port] = pl.Addr().String()
		go func() { <-ctx.Done(); pl.Close() }()
		go func() {
			for {
				c, err := pl.Accept()
				if err != nil {
					return
				}
				go serveUserTCP(ctx, c, lm, port)
			}
		}()
		if o.udp {
			pc, err := net.ListenPacket("udp", pl.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			go func() { <-ctx.Done(); pc.Close() }()
			go serveUserUDP(ctx, pc, lm, port, r.edge.logf)
		}
	}
	return r
}

type countingDialer struct {
	d LinkDialer
	n *atomic.Int64
}

func (c *countingDialer) DialLink(ctx context.Context) (Link, error) {
	c.n.Add(1)
	return c.d.DialLink(ctx)
}

// waitFor polls ok every 20 ms until it holds or d has passed, then fails the
// test with what.
func (r *v2Rig) waitFor(what string, d time.Duration, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out after %s: %s (edge %+v; exit %+v)", d, what, r.lm.Stats(), r.exitView())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// linkRow is one live edge link as the pool sees it (sampler fields are from
// the last health tick).
type linkRow struct {
	id                    int
	serving, retiring     bool
	open, flowing, recent int
	users                 int32
}

func (r *v2Rig) rows() []linkRow {
	if r.lm == nil {
		return nil
	}
	r.lm.mu.RLock()
	defer r.lm.mu.RUnlock()
	var out []linkRow
	for _, ml := range r.lm.links {
		if !ml.link.Alive() {
			continue
		}
		out = append(out, linkRow{id: ml.id, serving: ml.serving(), retiring: ml.retiring,
			open: ml.open, flowing: ml.flowing, recent: ml.recent, users: ml.users.Load()})
	}
	return out
}

// hotLinks returns the ids of links with at least one flowing stream.
func (r *v2Rig) hotLinks() []int {
	var ids []int
	for _, row := range r.rows() {
		if row.flowing > 0 {
			ids = append(ids, row.id)
		}
	}
	sort.Ints(ids)
	return ids
}

func (r *v2Rig) row(id int) (linkRow, bool) {
	for _, row := range r.rows() {
		if row.id == id {
			return row, true
		}
	}
	return linkRow{}, false
}

// statsStates returns the kindStats state of every live edge link.
func (r *v2Rig) statsStates() []int32 {
	r.lm.mu.RLock()
	defer r.lm.mu.RUnlock()
	var out []int32
	for _, ml := range r.lm.links {
		if ml.link.Alive() && ml.mtr != nil {
			out = append(out, ml.mtr.statsState.Load())
		}
	}
	return out
}

// ---- user connections -----------------------------------------------------

// pinger is a user connection that round-trips size random bytes through the
// echo panel every `every`, recording the first error.
type pinger struct {
	c    net.Conn
	n    atomic.Int64 // successful round trips
	mu   sync.Mutex
	err  error
	stop chan struct{}
	done chan struct{}
}

func startPinger(addr string, size int, every time.Duration) *pinger {
	p := &pinger{stop: make(chan struct{}), done: make(chan struct{})}
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		p.fail(err)
		close(p.done)
		return p
	}
	p.c = c
	go p.loop(size, every)
	return p
}

func (p *pinger) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *pinger) error() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *pinger) loop(size int, every time.Duration) {
	defer close(p.done)
	msg, got := make([]byte, size), make([]byte, size)
	for {
		rand.Read(msg)
		p.c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := p.c.Write(msg); err != nil {
			p.fail(fmt.Errorf("write after %d round trips: %w", p.n.Load(), err))
			return
		}
		if _, err := io.ReadFull(p.c, got); err != nil {
			p.fail(fmt.Errorf("read after %d round trips: %w", p.n.Load(), err))
			return
		}
		if !bytes.Equal(msg, got) {
			p.fail(errors.New("echo corrupted"))
			return
		}
		p.n.Add(1)
		select {
		case <-p.stop:
			return
		case <-time.After(every):
		}
	}
}

// close stops the pinger, closes its connection and returns its first error.
func (p *pinger) close() error {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
	if p.c != nil {
		p.c.Close()
	}
	return p.error()
}

// idleConn is a user connection that (optionally) echoes once and then stays
// silent; a reader records how and when the tunnel ends it.
type idleConn struct {
	c      net.Conn
	opened time.Time
	echo   chan []byte
	done   chan struct{}
	endErr error // valid once done is closed
	endAt  time.Time
}

func dialIdle(addr string, hello bool) (*idleConn, error) {
	opened := time.Now()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	ic := &idleConn{c: c, opened: opened, echo: make(chan []byte, 16), done: make(chan struct{})}
	go ic.read()
	if hello {
		if err := ic.probe(); err != nil {
			c.Close()
			return nil, err
		}
	}
	return ic, nil
}

func (ic *idleConn) read() {
	buf := make([]byte, 256)
	for {
		n, err := ic.c.Read(buf)
		if n > 0 {
			select {
			case ic.echo <- append([]byte(nil), buf[:n]...):
			default:
			}
		}
		if err != nil {
			ic.endErr, ic.endAt = err, time.Now()
			close(ic.done)
			return
		}
	}
}

// probe round-trips a few bytes (echo panel).
func (ic *idleConn) probe() error {
	msg := []byte("idle-probe")
	ic.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := ic.c.Write(msg); err != nil {
		return err
	}
	var got []byte
	timeout := time.After(3 * time.Second)
	for len(got) < len(msg) {
		select {
		case b := <-ic.echo:
			got = append(got, b...)
		case <-ic.done:
			return fmt.Errorf("closed: %v", ic.endErr)
		case <-timeout:
			return errors.New("no echo within 3s")
		}
	}
	if !bytes.Equal(got, msg) {
		return fmt.Errorf("echo corrupted: %q", got)
	}
	return nil
}

func (ic *idleConn) ended() bool {
	select {
	case <-ic.done:
		return true
	default:
		return false
	}
}

// ---- the held-connection shrink scenario ------------------------------------

// heldScenario is the load test 28 (and its mixed-version variants) shrinks
// under: nIdle connections that echoed once and went silent, two bulk flows on
// two different links, and one light "held" connection — a 32-byte round trip
// every 200 ms, far below the flowing rate but never idle — on a third link.
// The bulk links are the busiest, so a shrink to 2 keeps them serving and
// retires the held connection's link: it must keep working while retiring.
type heldScenario struct {
	idle []*idleConn
	bulk [2]*pinger
	held *pinger
	hot  []int // ids of the bulk flows' links
}

func (r *v2Rig) startHeldScenario(nIdle int) *heldScenario {
	r.t.Helper()
	sc := &heldScenario{}
	r.t.Cleanup(func() {
		for _, p := range append(sc.bulk[:], sc.held) {
			if p != nil {
				p.close()
			}
		}
		for _, ic := range sc.idle {
			ic.c.Close()
		}
	})
	for i := 0; i < nIdle; i++ {
		ic, err := dialIdle(r.userAddr, true)
		if err != nil {
			r.t.Fatalf("idle connection %d: %v", i, err)
		}
		sc.idle = append(sc.idle, ic)
	}
	// One bulk flow at a time: once the first counts as flowing on its link,
	// Pick sends the second elsewhere (it prefers links with fewer flows).
	for i := range sc.bulk {
		sc.bulk[i] = startPinger(r.userAddr, 4096, 10*time.Millisecond)
		want := i + 1
		r.waitFor(fmt.Sprintf("bulk flow %d flowing on its own link", want), 8*time.Second,
			func() bool { return len(r.hotLinks()) == want })
	}
	sc.hot = r.hotLinks()
	sc.held = startPinger(r.userAddr, 32, 200*time.Millisecond)
	r.waitFor("held connection round-trips", 5*time.Second, func() bool { return sc.held.n.Load() >= 3 })
	for _, p := range append(sc.bulk[:], sc.held) {
		if err := p.error(); err != nil {
			r.t.Fatalf("user flow failed before the shrink: %v", err)
		}
	}
	return sc
}

// retiringHeldID returns the id of the one retiring link (the held one).
func (r *v2Rig) retiringHeldID() int {
	r.t.Helper()
	var ids []int
	for _, row := range r.rows() {
		if row.retiring {
			ids = append(ids, row.id)
		}
	}
	if len(ids) != 1 {
		r.t.Fatalf("want exactly one retiring link (the held one), have %v: %+v", ids, r.rows())
	}
	return ids[0]
}

// checkIdleEnds asserts that every idle connection the tunnel ended saw a clean
// EOF (the edge closes with FIN; an RST would read as a reset), and that those
// still open are exactly the ones on serving links (open users minus the
// scenario's three flows).
func (r *v2Rig) checkIdleEnds(sc *heldScenario, active int) {
	r.t.Helper()
	var ended int
	r.waitFor("every idle connection off a serving link closed", 5*time.Second, func() bool {
		ended = 0
		for _, ic := range sc.idle {
			if ic.ended() {
				ended++
			}
		}
		return ended+(r.lm.Stats().Users-active) == len(sc.idle)
	})
	if ended == 0 {
		r.t.Fatalf("no idle connection on a retiring link was reclaimed")
	}
	for i, ic := range sc.idle {
		if ic.ended() && !errors.Is(ic.endErr, io.EOF) {
			r.t.Fatalf("idle connection %d ended with %v, want a clean EOF (FIN, not RST)", i, ic.endErr)
		}
	}
}

// checkNoRedial asserts (reverse) that the exit never redialled a link the
// edge closed and the edge's churn guard never tripped.
func (r *v2Rig) checkNoRedial() {
	r.t.Helper()
	if n := r.exit.count("; redial", 0); n != 0 {
		r.t.Fatalf("the exit redialled %d links the edge closed", n)
	}
	if n := r.edge.count("redials links this server retires", 0); n != 0 {
		r.t.Fatalf("the edge's churn guard tripped %d times", n)
	}
}

// checkFlows asserts the scenario's live flows never failed.
func (sc *heldScenario) checkFlows(t *testing.T, when string) {
	t.Helper()
	if err := sc.held.error(); err != nil {
		t.Fatalf("%s: the held connection broke: %v", when, err)
	}
	for i, b := range sc.bulk {
		if err := b.error(); err != nil {
			t.Fatalf("%s: bulk flow %d broke: %v", when, i+1, err)
		}
	}
}

// probeSurvivors checks that every idle connection still open carries data.
func (sc *heldScenario) probeSurvivors(t *testing.T) int {
	t.Helper()
	n := 0
	for i, ic := range sc.idle {
		if ic.ended() {
			continue
		}
		if err := ic.probe(); err != nil {
			t.Fatalf("idle connection %d on a serving link does not work: %v", i, err)
		}
		n++
	}
	return n
}

// shrinkWithHeld runs the core of test 28 on a rig pinned at `from` serving
// links: pin 2; the pool serves 2 within 5 s; every idle connection on a
// retiring link is closed with FIN and the empty links are closed, so within
// 15 s at most 3 links remain (2 serving + the held one); the held connection
// and the bulk flows never fail; nothing is dialled. Returns the held link id.
func (r *v2Rig) shrinkWithHeld(sc *heldScenario) int {
	r.t.Helper()
	dials := r.dials.Load()
	pinAt := time.Now()
	r.lm.pin.Store(2)
	r.waitFor("2 serving links within 5s of the shrink", 5*time.Second, func() bool { return r.lm.Stats().Serving == 2 })
	for _, id := range sc.hot {
		if row, ok := r.row(id); !ok || !row.serving {
			r.t.Fatalf("bulk flow link %d was retired instead of an idle one: %+v", id, r.rows())
		}
	}
	r.waitFor("at most 3 links (2 serving + the held one) within 15s", 15*time.Second-time.Since(pinAt), func() bool {
		s, e := r.lm.Stats(), r.exitView()
		return s.Links <= 3 && s.Retiring == 1 && s.HeldBy == 1 && e.Links >= 0 && e.Links <= 3
	})
	r.t.Logf("shrunk to %d links (%d serving, held by %d) %.1fs after the pin", r.lm.Stats().Links, r.lm.Stats().Serving,
		r.lm.Stats().HeldBy, time.Since(pinAt).Seconds())
	r.checkIdleEnds(sc, 3)
	sc.checkFlows(r.t, "during the shrink")
	if d := r.dials.Load() - dials; d != 0 {
		r.t.Fatalf("%d link(s) dialled while shrinking; want none", d)
	}
	return r.retiringHeldID()
}

// closeHeld ends the held connection and waits for its retiring link to close:
// exactly 2 links remain, at both ends, with nothing dialled.
func (r *v2Rig) closeHeld(sc *heldScenario) {
	r.t.Helper()
	dials := r.dials.Load()
	if err := sc.held.close(); err != nil {
		r.t.Fatalf("the held connection broke while its link was retiring: %v", err)
	}
	r.waitFor("the held link closes once its connection ended: 2 links", 12*time.Second, func() bool {
		s, e := r.lm.Stats(), r.exitView()
		return s.Links == 2 && s.Serving == 2 && s.Retiring == 0 && e.Links == 2
	})
	if d := r.dials.Load() - dials; d != 0 {
		r.t.Fatalf("%d link(s) dialled after the held link closed; want none", d)
	}
}

// ---- the previous release's exit ------------------------------------------

// runOldExitReverse is the previous release's reverse exit: the same dial pool
// (setTarget / retireIfOver are unchanged), but its sessions carry no meter and
// its serveStream has no kindStats case, so the edge's stats stream is closed.
func runOldExitReverse(ctx context.Context, panel string, links, min, max int,
	dial func() (*tlscarrier.Carrier, error), logf func(string, ...any)) *exitPool {
	if links < min {
		links = min
	}
	pool := newExitPool(ctx, min, max, func() (dialedLink, error) { return dial() }, logf)
	pool.serve = func(ctx context.Context, car dialedLink) string {
		return serveOldExitLink(ctx, car.(*tlscarrier.Carrier), panel, pool)
	}
	pool.setTarget(links)
	return pool
}

// runOldExitListener is the previous release's direct exit (listening).
func runOldExitListener(ctx context.Context, ln net.Listener, srv *tlscarrier.Server, panel string,
	live *atomic.Int32, logf func(string, ...any)) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go srv.Handle(ctx, conn, func(car *tlscarrier.Carrier) {
			logf("link up from %s (now %d)", conn.RemoteAddr(), live.Add(1))
			serveOldExitLink(ctx, car, panel, nil)
			car.Close()
			logf("link down from %s (now %d)", conn.RemoteAddr(), live.Add(-1))
		})
	}
}

// serveOldExitLink is the previous release's serveReverseLink / direct link
// loop: smux server without a meter.
func serveOldExitLink(ctx context.Context, car *tlscarrier.Carrier, panel string, pool *exitPool) string {
	sess, why, err := newSession(car.RawConn(), true, nil, nil)
	if err != nil {
		return "session setup failed: " + describeNetErr(err)
	}
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-closed:
		}
	}()
	var downErr error
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			downErr = err
			break
		}
		go serveOldStream(ctx, st, panel, car, pool)
	}
	close(closed)
	sess.Close()
	return sessionEndReason(why, downErr)
}

// serveOldStream is the previous release's serveStream: kindCtrl, kindPool and
// user TCP; any other kind (kindStats) is closed at once.
func serveOldStream(ctx context.Context, st *smux.Stream, panel string, car *tlscarrier.Carrier, pool *exitPool) {
	var kind [1]byte
	st.SetReadDeadline(time.Now().Add(kindTimeout))
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		st.Close()
		return
	}
	st.SetReadDeadline(time.Time{})
	switch kind[0] {
	case kindCtrl:
		serveControl(ctx, st, car)
	case kindPool:
		servePoolCtl(ctx, st, pool)
	case kindTCP:
		up, err := net.DialTimeout("tcp", panel, 5*time.Second)
		if err != nil {
			st.Close()
			return
		}
		relay(st, up)
	default:
		st.Close()
	}
}

// ---- 30. mixed versions ----------------------------------------------------

// childEnv names the test a re-executed test binary should run for real.
const childEnv = "HS2_ENGINE_TEST_CHILD"

// inChild reports whether this is the fresh process running t; otherwise it
// re-runs t alone in a fresh process, fails t if that run fails, and returns
// false. Used where a process-wide once-log (statsOldLogged) must be unused.
func inChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv(childEnv) == t.Name() {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("--- PASS: "+t.Name())) {
		t.Fatalf("fresh-process run failed (%v):\n%s", err, out)
	}
	t.Logf("fresh-process run:\n%s", out)
	return false
}

// 30(a) A new edge against the previous release's exit (no kindStats): every
// link falls back ("does not report link stats", logged once per process), and
// shrinking still works through the exit's unchanged retireIfOver: 7 → 2
// with the exit dialling nothing, while a connection held on a retiring link
// keeps working.
func TestMixedOldExitShrinksViaRetireIfOver(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time pool resizing (~25s)")
	}
	if !inChild(t) {
		return
	}
	r := startV2(t, v2Opts{min: 2, max: 8, perLink: 8, pin: 7, target: 7,
		exitLinks: 7, exitMin: 2, exitMax: 8, drainIdle: 2 * time.Second, oldExit: true})
	r.waitFor("7 serving links, stats unsupported on each", 10*time.Second, func() bool {
		s := r.lm.Stats()
		if s.Serving != 7 || s.Links != 7 || r.exitView().Links != 7 {
			return false
		}
		for _, st := range r.statsStates() {
			if st != statsUnsupported {
				return false
			}
		}
		return s.ExitStats == "older exit: download pressure unknown"
	})
	sc := r.startHeldScenario(20)
	exitMark := r.exit.mark()
	r.shrinkWithHeld(sc)
	r.closeHeld(sc)
	sc.checkFlows(t, "after the shrink")
	if n := r.dials.Load(); n != 7 {
		t.Fatalf("the old exit dialled %d links; want the initial 7 only", n)
	}
	if n := r.exit.count("retired — closed by the edge while above its target (pattern shrinking)", exitMark); n != 5 {
		t.Fatalf("the old exit retired %d slots via retireIfOver; want 5", n)
	}
	r.checkNoRedial()
	if n := r.edge.count("does not report link stats", 0); n != 1 {
		t.Fatalf("the older-exit fallback was logged %d times; want exactly once", n)
	}
	sc.probeSurvivors(t)
}

// 30(b) An old-style edge that speaks only kindPool (no kindCtrl, no kindStats)
// against the new exit: the exit's pool still follows the edge's target down
// (retiring the slots of links the edge closes, never redialling them) and up.
func TestMixedOldEdgeResizesNewExit(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time pool resizing (~20s)")
	}
	r := startV2(t, v2Opts{min: 2, max: 8, perLink: 8, pin: 3, target: 3,
		exitLinks: 6, exitMin: 2, exitMax: 8, oldEdge: true})
	// The exit comes up with its own 6 slots, dialing through the gate; the
	// edge wants 3. Slots still waiting for their turn when the target arrives
	// retire without dialing; any that were already dialed arrive retiring and
	// are closed once old enough, and the exit retires their slots.
	r.waitFor("the exit follows the edge down to 3", 16*time.Second, func() bool {
		e, s := r.exitView(), r.lm.Stats()
		return e.Target == 3 && e.Links == 3 && s.Links == 3 && s.Serving == 3
	})
	d0 := r.dials.Load()
	if d0 < 3 || d0 > 6 {
		t.Fatalf("exit dialled %d links; want 3 to its initial 6", d0)
	}
	if !echoOnce(r.userAddr, 4096) {
		t.Fatal("no echo through the tunnel")
	}
	r.lm.pin.Store(6)
	r.waitFor("the exit grows to 6", 10*time.Second, func() bool {
		e, s := r.exitView(), r.lm.Stats()
		return e.Target == 6 && e.Links == 6 && s.Serving == 6 && s.Links == 6
	})
	if n := r.dials.Load(); n != d0+3 {
		t.Fatalf("exit dialled %d links in all; want %d + 3", n, d0)
	}
	r.lm.pin.Store(2)
	r.waitFor("the exit shrinks to 2", 16*time.Second, func() bool {
		e, s := r.exitView(), r.lm.Stats()
		return e.Target == 2 && e.Links == 2 && s.Links == 2
	})
	if n := r.dials.Load(); n != d0+3 {
		t.Fatalf("exit redialled while shrinking: %d dials in all, want %d", n, d0+3)
	}
	if !echoOnce(r.userAddr, 4096) {
		t.Fatal("no echo through the tunnel after resizing")
	}
	for _, st := range r.statsStates() {
		if st != statsPending {
			t.Fatalf("an old-style edge opened kindStats (state %d)", st)
		}
	}
	if s := r.lm.Stats(); s.ExitStats != "" {
		t.Fatalf("ExitStats = %q for an edge that never asked", s.ExitStats)
	}
	r.checkNoRedial()
}

// 30(c) Direct mode (the edge dials with NewMTCPDialer, RunKharej listens):
// a pinned shrink retires links, reclaims the idle connections on them, closes
// them once empty and dials nothing, while a connection held on a retiring
// link keeps working; once it ends, its link closes too.
func TestMixedDirectShrinkKeepsHeldConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time pool resizing (~20s)")
	}
	testDirectShrink(t, false)
}

// 30(c) Direct mode against the previous release's exit (no kindStats):
// stats fall back and the shrink, which is edge-local, works the same.
func TestMixedDirectOldExitShrinks(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time pool resizing (~20s)")
	}
	testDirectShrink(t, true)
}

func testDirectShrink(t *testing.T, oldExit bool) {
	r := startV2(t, v2Opts{direct: true, min: 2, max: 7, perLink: 8, pin: 7, drainIdle: 2 * time.Second, oldExit: oldExit})
	want := "ok"
	if oldExit {
		want = "older exit: download pressure unknown"
	}
	r.waitFor("7 serving links", 10*time.Second, func() bool {
		s := r.lm.Stats()
		return s.Serving == 7 && s.Links == 7 && r.exitView().Links == 7 && s.ExitStats == want
	})
	if n := r.dials.Load(); n != 7 {
		t.Fatalf("edge dialled %d links to fill a pool of 7", n)
	}
	sc := r.startHeldScenario(20)
	r.shrinkWithHeld(sc)
	r.closeHeld(sc)
	sc.checkFlows(t, "after the shrink")
	if n := r.dials.Load(); n != 7 {
		t.Fatalf("edge dialled %d links in all; want the initial 7 only", n)
	}
	sc.probeSurvivors(t)
}
