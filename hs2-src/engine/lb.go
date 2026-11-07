package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Multipath load balancing, stage 1 (fixed weights). See lb-spec.md.
//
// The DIALING side of a tunnel (the kharej exit in reverse, the iran edge in
// direct) spreads its links over several (local IP, remote IP) paths by weight.
// Every new link — initial fill, autopilot growth, replacement after a death —
// is placed on the path furthest below its share (largest remainder on the
// current link counts, dials in flight included). A path whose dials fail
// lbFailMax times in a row is cooled down (weight 0) for lbCooldown, then gets
// one probe dial; the primary path (addr, bind_local_ip) is never cooled, so
// with every extra path dead all links go to the primary and the tunnel never
// depends on an extra.
//
// Every path is dialed from the start, by weight, whether or not the peer has
// advertised capLB yet. Waiting for it put every initial link on the primary
// (the first links are dialed before any kindInfo answer), and as the
// rebalance moves only idle links an extra path could stay empty for good
// (reverse lab: 8/0 instead of 4/4). Dialing an extra remote IP sends an
// older peer nothing new: nothing listens there, the dials fail, and after
// lbFailMax failures the path is cooled like any dead path, its share going
// to the others (one probe per cooldown after that). capLB stays in kindInfo
// for status ("no-peer": a cooled path to a peer known to lack it).

// capLB: this side runs multipath LB (it listens on, or dials, extra IPs).
// Advertised only when lb is enabled, so with lb absent the kindInfo bytes are
// exactly as before.
const capLB = 1 << 4

// LBPath is one path of a multipath tunnel, as seen from the dialing side.
type LBPath struct {
	Local   string // source IP to bind ("" = the default route)
	Remote  string // the listening side's IP
	Addr    string // Remote joined with the tunnel port: the dial target
	Weight  int    // share weight (local% × remote%); 0 = fallback only
	Primary bool   // the addr path: never cooled, needs no peer capability
}

// Name is the path's display name, "local>remote".
func (p LBPath) Name() string {
	l := p.Local
	if l == "" {
		l = "*"
	}
	return l + ">" + p.Remote
}

// LBPathStat is one path's state for the status file.
type LBPathStat struct {
	Path     string  `json:"path"`
	Weight   int     `json:"weight"`
	SharePct float64 `json:"share_pct"` // configured share among all paths
	Links    int     `json:"links"`     // links up (and dials in flight) on it
	Target   int     `json:"target"`    // its share of the current link count
	Fails    int     `json:"fails"`     // dials failed in a row
	FailsAll uint64  `json:"fails_total"`
	State    string  `json:"state"` // ok | cooling | probing | no-peer
	Primary  bool    `json:"primary,omitempty"`
	// Mode auto (lb_auto.go): the effective weight (configured × a quality
	// factor in [0.5, 1.5]) and the quality the factor came from.
	EffWeight float64 `json:"eff_weight,omitempty"`
	EffPct    float64 `json:"eff_pct,omitempty"`
	LossPct   float64 `json:"loss_pct,omitempty"` // retransmitted share of segments sent
	QueueMs   float64 `json:"queue_ms,omitempty"` // median srtt − minrtt of busy links
	Rebal     uint64  `json:"rebalanced,omitempty"`
}

// Tunables (vars for tests).
var (
	lbFailMax  = 3
	lbCooldown = 60 * time.Second
)

// LB places links on paths. It is safe for concurrent use.
type LB struct {
	mu      sync.Mutex
	paths   []LBPath
	links   []int // live links + dials in flight
	fails   []int
	total   []uint64
	until   []time.Time // cooling until (zero = not cooled)
	probing []bool      // the one probe dial after a cooldown is in flight
	held    map[any]int // live link -> path index
	hooks   map[any]lbHooks
	auto    bool      // mode auto: weights follow path quality (lb_auto.go)
	mult    []float64 // per path quality factor in [lbMultMin, lbMultMax] (1 in fixed)
	qual    []lbQual
	qprev   map[any]qPrev // a link's TCP_INFO counters at the last quality tick
	dials   []int         // dials started on each path since the last quality tick
	dfails  []int         // ... of which failed
	rebal   []uint64      // idle links retired from each path to rebalance
	peerLB  bool          // the peer's latest kindInfo answer has capLB (status)
	peerKnw bool          // a kindInfo answer has been seen (peerLB is real)
	// peerState reports what the peer's live links say about capLB: known is
	// false while no live link carries a kindInfo answer (nil = always ok).
	peerState func() (known, has bool)
	now       func() time.Time
	log       func(string, ...any)
}

// NewLB builds a balancer over paths; exactly one must be Primary.
func NewLB(paths []LBPath, logf func(string, ...any)) (*LB, error) {
	np := 0
	for _, p := range paths {
		if p.Primary {
			np++
		}
	}
	if np != 1 {
		return nil, fmt.Errorf("lb: %d primary paths (want 1)", np)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	n := len(paths)
	return &LB{paths: append([]LBPath(nil), paths...), links: make([]int, n), fails: make([]int, n),
		total: make([]uint64, n), until: make([]time.Time, n), probing: make([]bool, n),
		held: map[any]int{}, hooks: map[any]lbHooks{}, mult: ones(n), qual: make([]lbQual, n),
		qprev: map[any]qPrev{}, dials: make([]int, n), dfails: make([]int, n), rebal: make([]uint64, n),
		now: time.Now, log: logf}, nil
}

// Paths returns the configured paths.
func (b *LB) Paths() []LBPath { return append([]LBPath(nil), b.paths...) }

func (b *LB) primary() int {
	for i, p := range b.paths {
		if p.Primary {
			return i
		}
	}
	return 0
}

// refreshPeerLocked reads the peer's capLB from live links (status only).
func (b *LB) refreshPeerLocked() {
	if b.peerState == nil {
		return
	}
	if known, has := b.peerState(); known {
		b.peerKnw, b.peerLB = true, has
	}
}

// eligibleLocked reports whether path i may take a normal (non-probe) link.
func (b *LB) eligibleLocked(i int, now time.Time) bool {
	if b.paths[i].Primary {
		return true
	}
	return b.until[i].IsZero()
}

// acquire picks the path for the next dial and reserves a link on it.
// probe is true when the dial is a cooled path's one probe.
func (b *LB) acquire() (i int, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.refreshPeerLocked()
	prim := b.primary()
	// A cooled path whose cooldown is over gets one probe dial first.
	for j := range b.paths {
		if !b.until[j].IsZero() && !now.Before(b.until[j]) && !b.probing[j] {
			b.probing[j] = true
			b.links[j]++
			b.dials[j]++
			return j, true
		}
	}
	ok := func(j int) bool { return b.eligibleLocked(j, now) }
	i = -1
	if b.auto {
		i = b.minTwoLocked(ok)
	}
	if i < 0 {
		i = pickLargestRemainder(b.effWeightsLocked(), b.links, ok, prim)
	}
	b.links[i]++
	b.dials[i]++
	return i, false
}

// effWeightsLocked is each path's effective weight: configured × quality
// factor, scaled ×100 so a factor of 1 keeps the configured proportions.
func (b *LB) effWeightsLocked() []int {
	w := make([]int, len(b.paths))
	for j, p := range b.paths {
		w[j] = int(float64(p.Weight)*100*b.mult[j] + 0.5)
	}
	return w
}

func ones(n int) []float64 {
	m := make([]float64, n)
	for i := range m {
		m[i] = 1
	}
	return m
}

// pickLargestRemainder returns the eligible path furthest below its share of
// (current eligible links + 1); ties go to the larger weight, then the lower
// index. With no eligible weight it returns fallback.
func pickLargestRemainder(weights []int, links []int, ok func(int) bool, fallback int) int {
	n, w := 1, 0
	for j := range weights {
		if ok(j) {
			n += links[j]
			w += weights[j]
		}
	}
	if w == 0 {
		return fallback
	}
	best, bestDef := -1, 0
	for j, pw := range weights {
		if !ok(j) || pw == 0 {
			continue
		}
		def := pw*n - links[j]*w // (share − links) × w, exact in integers
		if best < 0 || def > bestDef || (def == bestDef && pw > weights[best]) {
			best, bestDef = j, def
		}
	}
	return best
}

// result records the outcome of a dial on path i; on success link (comparable)
// is held on the path until release(link).
func (b *LB) result(i int, probe bool, link any, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.paths[i]
	if probe {
		b.probing[i] = false
	}
	if err != nil {
		b.links[i]--
		b.fails[i]++
		b.total[i]++
		b.dfails[i]++
		if !p.Primary && (probe || b.fails[i] >= lbFailMax) {
			wasCool := !b.until[i].IsZero()
			cd := b.cooldownLocked()
			b.until[i] = b.now().Add(cd)
			if !wasCool {
				b.log("lb: path %s cooled for %s after %d failed dial(s): %v — its share goes to the other paths", p.Name(), fmtDur(cd), b.fails[i], err)
			}
		}
		return
	}
	if !b.until[i].IsZero() {
		b.log("lb: path %s is back (probe link up after %d failed dial(s))", p.Name(), b.fails[i])
	}
	b.fails[i] = 0
	b.until[i] = time.Time{}
	if link != nil {
		b.held[link] = i
	}
}

// release frees the path slot of a link that ended. Safe to call twice.
func (b *LB) release(link any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i, ok := b.held[link]; ok {
		delete(b.held, link)
		delete(b.hooks, link)
		delete(b.qprev, link)
		b.links[i]--
	}
}

// Stats is each path's state, in configured order.
func (b *LB) Stats() []LBPathStat {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	noPeer := b.peerKnw && !b.peerLB
	prim := b.primary()
	eff := b.effWeightsLocked()
	total, wAll, wOK, effAll := 0, 0, 0, 0
	for j, p := range b.paths {
		total += b.links[j]
		wAll += p.Weight
		effAll += eff[j]
		if b.eligibleLocked(j, now) {
			wOK += eff[j]
		}
	}
	out := make([]LBPathStat, len(b.paths))
	for j, p := range b.paths {
		s := LBPathStat{Path: p.Name(), Weight: p.Weight, Links: b.links[j], Fails: b.fails[j], FailsAll: b.total[j], Primary: p.Primary, State: "ok"}
		if wAll > 0 {
			s.SharePct = round1pct(100 * float64(p.Weight) / float64(wAll))
		}
		switch {
		case noPeer && !b.until[j].IsZero() && p.Remote != b.paths[prim].Remote:
			s.State = "no-peer" // cooled, and the peer does not run lb
		case b.probing[j]:
			s.State = "probing"
		case !b.until[j].IsZero():
			s.State = "cooling"
		}
		if b.eligibleLocked(j, now) && wOK > 0 {
			s.Target = (eff[j]*total + wOK/2) / wOK
		}
		s.Rebal = b.rebal[j]
		if b.auto {
			s.EffWeight = round1pct(float64(p.Weight) * b.mult[j])
			if effAll > 0 {
				s.EffPct = round1pct(100 * float64(eff[j]) / float64(effAll))
			}
			s.LossPct = round1pct(100 * b.qual[j].loss)
			s.QueueMs = round1pct(b.qual[j].queueMs)
		}
		out[j] = s
	}
	return out
}

func round1pct(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// LBLine is a one-line text summary of path stats ("a>b 7/7 ok, ...").
func LBLine(st []LBPathStat) string {
	s := ""
	for i, p := range st {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s %d/%d", p.Path, p.Links, p.Target)
		if p.EffPct > 0 {
			s += fmt.Sprintf(" w%.0f%% loss %.1f%% q %.0fms", p.EffPct, p.LossPct, p.QueueMs)
		}
		if p.State != "ok" {
			s += " " + p.State
		}
		if p.Fails > 0 {
			s += fmt.Sprintf(" (%d fail)", p.Fails)
		}
	}
	return s
}

// dialVia dials one link through the balancer: dial gets the chosen path.
func (b *LB) dialVia(dial func(LBPath) (any, error)) (any, error) {
	i, probe := b.acquire()
	link, err := dial(b.paths[i])
	if err == nil && link == nil {
		err = errors.New("lb: dial returned no link")
	}
	b.result(i, probe, link, err)
	if err != nil {
		return nil, fmt.Errorf("path %s: %w", b.paths[i].Name(), err)
	}
	return link, nil
}

// setPeerState lets the balancer read the peer's capLB from live links.
func (b *LB) setPeerState(f func() (known, has bool)) {
	b.mu.Lock()
	b.peerState = f
	b.mu.Unlock()
}

// capState reports, over an exit's live links, whether any carries the edge's
// kindInfo answer (known) and whether the latest such answer has cap c.
func (p *linkPeers) capState(c uint8) (known, has bool) {
	if p == nil {
		return false, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for m := range p.m {
		if pi := m.peerInfo.Load(); pi != nil && pi.V2 {
			known = true
			if pi.Caps&c != 0 {
				return true, true
			}
		}
	}
	return known, false
}

// lbLinkDialer is the direct-mode edge's LinkDialer over an LB: one inner
// dialer per path; a link is released from its path when it closes.
type lbLinkDialer struct {
	lb    *LB
	inner map[string]LinkDialer // by LBPath.Name()
}

// NewLBLinkDialer builds a LinkDialer that places each link on a path of lb;
// mk builds the inner dialer for one path.
func NewLBLinkDialer(lb *LB, mk func(LBPath) LinkDialer) LinkDialer {
	d := &lbLinkDialer{lb: lb, inner: map[string]LinkDialer{}}
	for _, p := range lb.paths {
		d.inner[p.Name()] = mk(p)
	}
	return d
}

func (d *lbLinkDialer) DialLink(ctx context.Context) (Link, error) {
	v, err := d.lb.dialVia(func(p LBPath) (any, error) {
		l, err := d.inner[p.Name()].DialLink(ctx)
		if err != nil {
			return nil, err
		}
		return l, nil
	})
	if err != nil {
		return nil, err
	}
	l := v.(Link)
	d.lb.attach(l, lbHooks{
		stats: func() (tcpStat, bool) { return linkTCPStatsOf(l) },
		retire: func() bool {
			if l.Active() != 0 {
				return false
			}
			l.Close()
			return true
		},
	})
	if c, ok := l.(interface{ closed() <-chan struct{} }); ok {
		go func() { <-c.closed(); d.lb.release(l) }()
	}
	return l, nil
}

// lbOf returns the LB behind a LinkDialer built by NewLBLinkDialer (else nil).
func lbOf(d LinkDialer) *LB {
	if x, ok := d.(*lbLinkDialer); ok {
		return x.lb
	}
	return nil
}

// MergeListeners accepts from every listener as one. Close closes them all.
// Used to listen on extra LB IPs without touching the accept loops.
func MergeListeners(lns ...net.Listener) net.Listener {
	if len(lns) == 1 {
		return lns[0]
	}
	m := &mergedListener{lns: lns, ch: make(chan acceptRes), done: make(chan struct{})}
	for _, ln := range lns {
		go func(ln net.Listener) {
			for {
				c, err := ln.Accept()
				select {
				case m.ch <- acceptRes{c, err}:
				case <-m.done:
					if c != nil {
						c.Close()
					}
					return
				}
				if err != nil && errors.Is(err, net.ErrClosed) {
					return
				}
			}
		}(ln)
	}
	return m
}

type acceptRes struct {
	c   net.Conn
	err error
}

type mergedListener struct {
	lns  []net.Listener
	ch   chan acceptRes
	once sync.Once
	done chan struct{}
}

func (m *mergedListener) Accept() (net.Conn, error) {
	select {
	case r := <-m.ch:
		if r.err != nil && errors.Is(r.err, net.ErrClosed) {
			select {
			case <-m.done:
				return nil, net.ErrClosed
			default:
				// one listener died alone: keep serving the others
				return nil, fmt.Errorf("lb listener: %v", r.err)
			}
		}
		return r.c, r.err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *mergedListener) Close() error {
	m.once.Do(func() {
		close(m.done)
		for _, ln := range m.lns {
			ln.Close()
		}
	})
	return nil
}

func (m *mergedListener) Addr() net.Addr { return m.lns[0].Addr() }
