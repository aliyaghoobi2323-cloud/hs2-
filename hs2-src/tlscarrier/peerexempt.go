package tlscarrier

import (
	"container/list"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
)

// b388 (sec353 1a): peer IP exemption from the Reality pre-hello cap.
//
// preAcquire runs before any byte of a connection is read, so the per-/24
// (/64) pre-hello cap (preMaxNet) counts our own dialer like any stranger:
// another host in the peer's /24 holding 64 idle connections pushed our link
// refill off reality (rq387: prehello_cap +231, 0 reality links, the dialer's
// "both" fell back to tls for 10 min; a pure "reality" IP would have refused
// the links). Authentication only exists after the hello, so the exemption is
// by source IP: the configured peer IPs (the other side's lb list, re-read on
// every reload) plus IPs that authenticated a link (reality or tls) in the
// last 24 h. An exempt IP skips the listener's per-/24 and global pre-hello
// counts and has its own bound per IP (2*max_links+8, the reverse accept cap),
// so a spoofed or stolen peer IP cannot open an unlimited number of
// pre-hello connections either. Probe responses do not change: an exempt
// connection runs exactly the same code after the slot is taken.
//
// HS2_B388_PEEREXEMPT (default on, "0" off).
var PeerExemptOn atomic.Bool

func loadPeerExemptKey() { PeerExemptOn.Store(envdef.Get("HS2_B388_PEEREXEMPT") != "0") }

const (
	peerLearnTTL   = 24 * time.Hour
	peerLearnMax   = 1024 // learned (authenticated) IPs kept, LRU
	peerBoundDef   = 2*300 + 8
	peerBoundFloor = 2*32 + 8
)

// PeerAllow is the process-wide allowlist of peer IPs (all reality listeners
// of the process share it: a peer authenticated on one IP is exempt on all).
type PeerAllow struct {
	mu      sync.Mutex
	static  map[string]bool
	learned map[string]*list.Element // value: *peerSeen
	lru     *list.List               // front = most recent
	pre     map[string]int           // concurrent exempt pre-hello conns per IP
	bound   int
	now     func() time.Time
	// counters
	exempted atomic.Int64 // exempt pre-hello slots taken
	capped   atomic.Int64 // exempt IP over its own bound
}

type peerSeen struct {
	ip string
	t  time.Time
}

// DefaultPeers is the process's allowlist (cmd/hs2 fills the static part).
var DefaultPeers = NewPeerAllow()

func NewPeerAllow() *PeerAllow {
	return &PeerAllow{static: map[string]bool{}, learned: map[string]*list.Element{},
		lru: list.New(), pre: map[string]int{}, bound: peerBoundDef, now: time.Now}
}

func peerKey(a net.Addr) string {
	var ip net.IP
	switch t := a.(type) {
	case *net.TCPAddr:
		ip = t.IP
	case *net.UDPAddr:
		ip = t.IP
	default:
		return ""
	}
	return normIP(ip)
}

func normIP(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// SetStatic replaces the configured peer IPs (bad entries are skipped).
func (p *PeerAllow) SetStatic(ips []string) {
	m := map[string]bool{}
	for _, s := range ips {
		if k := normIP(net.ParseIP(s)); k != "" {
			m[k] = true
		}
	}
	p.mu.Lock()
	p.static = m
	p.mu.Unlock()
}

// SetBound sets the concurrent exempt pre-hello connections allowed per IP
// from the link ceiling: 2*maxLinks+8 (floor 72).
func (p *PeerAllow) SetBound(maxLinks int) {
	b := 2*maxLinks + 8
	if b < peerBoundFloor {
		b = peerBoundFloor
	}
	p.mu.Lock()
	p.bound = b
	p.mu.Unlock()
}

// Learn records an IP that authenticated a link.
func (p *PeerAllow) Learn(a net.Addr) {
	if p == nil {
		return
	}
	k := peerKey(a)
	if k == "" {
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.learned[k]; e != nil {
		e.Value.(*peerSeen).t = now
		p.lru.MoveToFront(e)
		return
	}
	p.learned[k] = p.lru.PushFront(&peerSeen{ip: k, t: now})
	for p.lru.Len() > peerLearnMax {
		b := p.lru.Back()
		delete(p.learned, b.Value.(*peerSeen).ip)
		p.lru.Remove(b)
	}
}

// exemptLocked: k is a configured peer or authenticated within the TTL
// (expired entries are dropped from the tail).
func (p *PeerAllow) exemptLocked(k string) bool {
	if p.static[k] {
		return true
	}
	now := p.now()
	for b := p.lru.Back(); b != nil; b = p.lru.Back() {
		s := b.Value.(*peerSeen)
		if now.Sub(s.t) < peerLearnTTL {
			break
		}
		delete(p.learned, s.ip)
		p.lru.Remove(b)
	}
	return p.learned[k] != nil
}

// Exempt reports whether a's IP is exempt.
func (p *PeerAllow) Exempt(a net.Addr) bool {
	k := peerKey(a)
	if p == nil || k == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exemptLocked(k)
}

// acquire: exempt = a's IP is a peer; ok = it got an exempt slot (false: over
// its own bound).
func (p *PeerAllow) acquire(a net.Addr) (release func(), exempt, ok bool) {
	k := peerKey(a)
	if p == nil || k == "" {
		return nil, false, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exemptLocked(k) {
		return nil, false, false
	}
	if p.pre[k] >= p.bound {
		p.capped.Add(1)
		return nil, true, false
	}
	p.pre[k]++
	p.exempted.Add(1)
	return func() {
		p.mu.Lock()
		if p.pre[k]--; p.pre[k] <= 0 {
			delete(p.pre, k)
		}
		p.mu.Unlock()
	}, true, true
}

// PeerAllowSnap is the allowlist's status.
type PeerAllowSnap struct {
	Static   int   `json:"static"`
	Learned  int   `json:"learned"`
	Bound    int   `json:"bound"`
	Exempted int64 `json:"exempted"`
	Capped   int64 `json:"capped"`
}

func (p *PeerAllow) Snap() PeerAllowSnap {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PeerAllowSnap{Static: len(p.static), Learned: p.lru.Len(), Bound: p.bound,
		Exempted: p.exempted.Load(), Capped: p.capped.Load()}
}
