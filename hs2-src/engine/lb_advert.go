package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

// Multipath LB, stage 6: each side announces its OWN listening addresses.
//
// The listening side of a tunnel (iran in reverse, kharej in direct) owns the
// addresses the other side dials, so it tells the dialing side which they are
// and their weights; the dialing side builds its paths from that list (its
// own source addresses still come from its local config). Changing the
// listening side's lb section and sending it SIGHUP is then enough: it listens
// on the new set and re-announces it, and the dialing side moves its paths
// without a restart and without closing a link.
//
// Wire (kindLBAdvert, opened by the edge — the smux client in both directions
// — only on a link whose exit announced capLBAdvert over kindInfo):
//
//	edge -> exit  [kindLBAdvert][advert]
//	exit -> edge  [advert]
//	advert: [ver=1][n u8] n × ([iplen u8 (4|16)][ip][weight u8])
//
// Each side sends its own list (n = 0: nothing to announce, e.g. the dialing
// side, or a listener on a wildcard address). The edge repeats the exchange
// every lbAdvertEvery on one live link, and at once after a local reload, so
// an exit-side (direct) change reaches the dialing edge within that period.
//
// Safety: an advert arrives only over an authenticated link (the shared-key
// carrier), is validated (valid unicast IPs, no duplicates, at most
// lbAdvertMax entries, weights 0..100 summing to 100) and only ever replaces
// the REMOTE addresses: the primary path (addr) is kept by the dialing side
// whatever the advert says. A peer without capLBAdvert never sends one, and the
// dialing side keeps its local lists (stage 5 behaviour).

// capLBAdvert (kindInfo caps): this side reads and answers kindLBAdvert.
const capLBAdvert = 1 << 5

const kindLBAdvert byte = 11

const (
	lbAdvertVer = 1
	lbAdvertMax = 32
)

// lbAdvertEvery: how often the edge repeats its advert exchange (var for tests).
var lbAdvertEvery = 15 * time.Second

// LBAdvertIP is one announced address.
type LBAdvertIP struct {
	IP     string `json:"ip"`
	Weight int    `json:"weight"`
}

// ValidateLBAdvert checks an announced list (see the safety note above).
// An empty list is valid (nothing announced).
func ValidateLBAdvert(l []LBAdvertIP) error {
	if len(l) == 0 {
		return nil
	}
	if len(l) > lbAdvertMax {
		return fmt.Errorf("lb advert: %d addresses (max %d)", len(l), lbAdvertMax)
	}
	seen := map[string]bool{}
	sum := 0
	for _, e := range l {
		ip := net.ParseIP(e.IP)
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("lb advert: %q is not a unicast IP", e.IP)
		}
		k := ip.String()
		if seen[k] {
			return fmt.Errorf("lb advert: %s listed twice", k)
		}
		seen[k] = true
		if e.Weight < 0 || e.Weight > 100 {
			return fmt.Errorf("lb advert: weight %d of %s out of 0..100", e.Weight, k)
		}
		sum += e.Weight
	}
	if sum != 100 {
		return fmt.Errorf("lb advert: weights sum to %d, not 100", sum)
	}
	return nil
}

func encodeAdvert(l []LBAdvertIP) []byte {
	b := []byte{lbAdvertVer, 0}
	n := 0
	for _, e := range l {
		ip := net.ParseIP(e.IP)
		if ip == nil || n >= lbAdvertMax {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		w := e.Weight
		if w < 0 {
			w = 0
		} else if w > 255 {
			w = 255
		}
		b = append(b, byte(len(ip)))
		b = append(b, ip...)
		b = append(b, byte(w))
		n++
	}
	b[1] = byte(n)
	return b
}

var errAdvert = errors.New("lb advert: malformed message")

func readAdvert(r io.Reader) ([]LBAdvertIP, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if h[0] < 1 || int(h[1]) > lbAdvertMax {
		return nil, errAdvert
	}
	out := make([]LBAdvertIP, 0, h[1])
	for i := 0; i < int(h[1]); i++ {
		var ln [1]byte
		if _, err := io.ReadFull(r, ln[:]); err != nil {
			return nil, err
		}
		if ln[0] != 4 && ln[0] != 16 {
			return nil, errAdvert
		}
		buf := make([]byte, int(ln[0])+1)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		out = append(out, LBAdvertIP{IP: net.IP(buf[:ln[0]]).String(), Weight: int(buf[ln[0]])})
	}
	return out, nil
}

func advertEqual(a, b []LBAdvertIP) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]LBAdvertIP(nil), a...)
	y := append([]LBAdvertIP(nil), b...)
	s := func(l []LBAdvertIP) { sort.Slice(l, func(i, j int) bool { return l[i].IP < l[j].IP }) }
	s(x)
	s(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// LBAdvert is one tunnel's advert state on either side.
type LBAdvert struct {
	mu       sync.Mutex
	mine     []LBAdvertIP
	peer     []LBAdvertIP
	peerAt   time.Time
	peerHas  bool // the peer runs stage 6 (answered / sent kindLBAdvert)
	onPeer   func([]LBAdvertIP)
	kick     chan struct{}
	links    map[Link]struct{} // edge: live links whose exit has capLBAdvert
	rejected string
	log      func(string, ...any)
}

// NewLBAdvert: mine is this side's own list (nil: nothing to announce);
// onPeer (dialing side, may be nil) gets each new valid list from the peer.
func NewLBAdvert(mine []LBAdvertIP, onPeer func([]LBAdvertIP), logf func(string, ...any)) *LBAdvert {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &LBAdvert{mine: append([]LBAdvertIP(nil), mine...), onPeer: onPeer, kick: make(chan struct{}, 1),
		links: map[Link]struct{}{}, log: logf}
}

// SetMine replaces this side's list (after a reload) and re-announces it.
func (a *LBAdvert) SetMine(l []LBAdvertIP) {
	a.mu.Lock()
	a.mine = append([]LBAdvertIP(nil), l...)
	a.mu.Unlock()
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Mine is this side's list as announced now.
func (a *LBAdvert) Mine() []LBAdvertIP { return a.getMine() }

func (a *LBAdvert) getMine() []LBAdvertIP {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]LBAdvertIP(nil), a.mine...)
}

// LBAdvertState is what status shows.
type LBAdvertState struct {
	PeerHas  bool         `json:"peer_has"`          // the peer announces / reads adverts
	Peer     []LBAdvertIP `json:"peer,omitempty"`    // the peer's latest valid list
	PeerAt   time.Time    `json:"peer_at,omitempty"` // when it changed last
	Rejected string       `json:"rejected,omitempty"`
}

// State reports the advert state for status.
func (a *LBAdvert) State() LBAdvertState {
	if a == nil {
		return LBAdvertState{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return LBAdvertState{PeerHas: a.peerHas, Peer: append([]LBAdvertIP(nil), a.peer...), PeerAt: a.peerAt, Rejected: a.rejected}
}

// received handles the peer's list (either side).
func (a *LBAdvert) received(l []LBAdvertIP) {
	a.mu.Lock()
	a.peerHas = true
	if len(l) == 0 {
		a.mu.Unlock()
		return
	}
	if err := ValidateLBAdvert(l); err != nil {
		if a.rejected != err.Error() {
			a.log("lb: rejected the other server's address list: %v — keeping the current paths", err)
		}
		a.rejected = err.Error()
		a.mu.Unlock()
		return
	}
	a.rejected = ""
	if advertEqual(l, a.peer) {
		a.mu.Unlock()
		return
	}
	a.peer = append([]LBAdvertIP(nil), l...)
	a.peerAt = time.Now()
	f := a.onPeer
	a.mu.Unlock()
	if f != nil {
		f(l)
	}
}

// serve runs the exit side of one kindLBAdvert stream.
func (a *LBAdvert) serve(st io.ReadWriteCloser) {
	defer st.Close()
	if d, ok := st.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Now().Add(infoTimeout))
	}
	l, err := readAdvert(st)
	if err != nil {
		return
	}
	a.received(l)
	st.Write(encodeAdvert(a.getMine()))
}

// exchange runs the edge side over one link.
func (a *LBAdvert) exchange(ro rawStreamOpener) error {
	st, err := ro.OpenRawStream()
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(infoTimeout))
	if _, err := st.Write(append([]byte{kindLBAdvert}, encodeAdvert(a.getMine())...)); err != nil {
		return err
	}
	l, err := readAdvert(st)
	if err != nil {
		return err
	}
	a.received(l)
	return nil
}

// addLink registers an edge link whose exit announced capLBAdvert and kicks
// an exchange.
func (a *LBAdvert) addLink(l Link) {
	a.mu.Lock()
	a.links[l] = struct{}{}
	a.mu.Unlock()
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// runEdge exchanges adverts on one live link now and then, and at once after
// a kick (a new link, a local reload).
func (a *LBAdvert) runEdge(ctx context.Context) {
	t := time.NewTicker(lbAdvertEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
		a.mu.Lock()
		var ls []Link
		for l := range a.links {
			if !l.Alive() {
				delete(a.links, l)
				continue
			}
			ls = append(ls, l)
		}
		a.mu.Unlock()
		for _, l := range ls {
			ro, ok := l.(rawStreamOpener)
			if ok && a.exchange(ro) == nil {
				break
			}
		}
	}
}

// --- runtime path changes ------------------------------------------------

// SetPaths replaces the balancer's paths without touching a live link. A path
// kept by name keeps its state; a new one starts empty; a removed one with
// links stays, draining: it takes no new link (the rebalance retires its idle
// links, busy ones end with their users) and is dropped once empty. Exactly
// one path must be Primary.
func (b *LB) SetPaths(paths []LBPath) error {
	np := 0
	seen := map[string]bool{}
	for _, p := range paths {
		if p.Primary {
			np++
		}
		if seen[p.Name()] {
			return fmt.Errorf("lb: path %s listed twice", p.Name())
		}
		seen[p.Name()] = true
	}
	if np != 1 {
		return fmt.Errorf("lb: %d primary paths (want 1)", np)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	next := append([]LBPath(nil), paths...)
	for i, p := range b.paths {
		if !seen[p.Name()] && b.links[i] > 0 {
			p.Weight, p.Primary, p.Gone = 0, false, true
			next = append(next, p)
		}
	}
	b.remapLocked(next)
	// A cooled path that is (re)announced gets its probe dial now instead of
	// at the end of its cooldown: the listening side may have just started
	// listening on it (a reload, or it came up after this side).
	now := b.now()
	for j, p := range b.paths {
		if !p.Gone && !b.until[j].IsZero() && b.until[j].After(now) {
			b.until[j] = now
		}
	}
	return nil
}

// remapLocked moves every per-path state onto next (matched by name).
func (b *LB) remapLocked(next []LBPath) {
	old := map[string]int{}
	for i, p := range b.paths {
		old[p.Name()] = i
	}
	n := len(next)
	links, fails, total := make([]int, n), make([]int, n), make([]uint64, n)
	until, probing := make([]time.Time, n), make([]bool, n)
	mult, qual := ones(n), make([]lbQual, n)
	dials, dfails, rebal := make([]int, n), make([]int, n), make([]uint64, n)
	to := map[int]int{}
	for j, p := range next {
		i, ok := old[p.Name()]
		if !ok {
			continue
		}
		to[i] = j
		links[j], fails[j], total[j] = b.links[i], b.fails[i], b.total[i]
		until[j], probing[j] = b.until[i], b.probing[i]
		mult[j], qual[j] = b.mult[i], b.qual[i]
		dials[j], dfails[j], rebal[j] = b.dials[i], b.dfails[i], b.rebal[i]
		if p.Gone {
			until[j] = time.Time{} // a draining path is never probed
		}
	}
	for k, i := range b.held {
		if j, ok := to[i]; ok {
			b.held[k] = j
		} else { // cannot happen: a path with a held link is kept
			delete(b.held, k)
			delete(b.hooks, k)
		}
	}
	b.paths, b.links, b.fails, b.total = next, links, fails, total
	b.until, b.probing, b.mult, b.qual = until, probing, mult, qual
	b.dials, b.dfails, b.rebal = dials, dfails, rebal
}

// dropDrainedLocked removes draining paths that have no link left.
func (b *LB) dropDrainedLocked() {
	var next []LBPath
	drop := false
	for i, p := range b.paths {
		if p.Gone && b.links[i] <= 0 {
			drop = true
			continue
		}
		next = append(next, p)
	}
	if drop {
		b.remapLocked(next)
	}
}

func (b *LB) indexLocked(name string) int {
	for i, p := range b.paths {
		if p.Name() == name {
			return i
		}
	}
	return -1
}

// --- runtime listener changes --------------------------------------------

// ListenerSet is a merged listener whose members can change while it runs
// (MergeListeners' result).
type ListenerSet interface {
	net.Listener
	// AddListener starts accepting from ln too.
	AddListener(ln net.Listener)
	// RemoveListener closes the member listening on addr's IP (established
	// connections are not touched). It reports whether one was found.
	RemoveListener(ip string) bool
	// ListenIPs is the IP of every member.
	ListenIPs() []string
}
