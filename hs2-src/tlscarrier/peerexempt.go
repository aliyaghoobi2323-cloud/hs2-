package tlscarrier

import (
	"bufio"
	"container/list"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	// b411: authenticated peer IPs are saved next to the config and loaded
	// into the static part at the next start (a restart or a first start
	// after an upgrade must not wait for the first link to authenticate).
	peerSavedTTL = 7 * 24 * time.Hour
	peerSavedMax = 256
	peerSaveGap  = time.Hour // re-save a known IP's time at most this often
)

// PeerAllow is the process-wide allowlist of peer IPs (all reality listeners
// of the process share it: a peer authenticated on one IP is exempt on all).
type PeerAllow struct {
	mu      sync.Mutex
	static  map[string]bool // conf + saved (IPs)
	conf    map[string]bool // the configured peer IPs (SetStatic)
	saved   map[string]time.Time
	net64   map[string]bool // b411: /64 of every static IPv6
	learn64 map[string]int  // b411: learned IPv6 per /64
	store   string          // b411: save file ("" = none)
	saving  bool
	dirty   bool
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
	return &PeerAllow{static: map[string]bool{}, conf: map[string]bool{}, saved: map[string]time.Time{},
		net64: map[string]bool{}, learn64: map[string]int{}, learned: map[string]*list.Element{},
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

// prefix64 is the /64 of an IPv6 key ("" for IPv4 or bad input). b411: an
// IPv6 peer is exempt by its /64 (the same unit the pre-hello cap counts by),
// so a peer whose "auto6" address moves inside its /64 stays exempt.
func prefix64(k string) string {
	ip := net.ParseIP(k)
	if ip == nil || ip.To4() != nil {
		return ""
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// SetStatic replaces the configured peer IPs (bad entries, e.g. an "auto6"
// only the other side resolves, are skipped). IPs loaded by SetStore stay.
func (p *PeerAllow) SetStatic(ips []string) {
	m := map[string]bool{}
	for _, s := range ips {
		ip := net.ParseIP(strings.TrimSpace(s))
		if k := normIP(ip); k != "" && !ip.IsUnspecified() {
			m[k] = true
		}
	}
	p.mu.Lock()
	p.conf = m
	p.rebuildStaticLocked()
	p.mu.Unlock()
}

func (p *PeerAllow) rebuildStaticLocked() {
	st, n64 := map[string]bool{}, map[string]bool{}
	add := func(k string) {
		st[k] = true
		if q := prefix64(k); q != "" {
			n64[q] = true
		}
	}
	for k := range p.conf {
		add(k)
	}
	for k := range p.saved {
		add(k)
	}
	p.static, p.net64 = st, n64
}

// SetStore (b411) names the file authenticated peer IPs are saved to and
// loads the IPs saved there in the last 7 days into the static part. Returns
// how many were loaded. path "" = no saving.
func (p *PeerAllow) SetStore(path string) (int, error) {
	if p == nil {
		return 0, nil
	}
	now := p.now()
	loaded := map[string]time.Time{}
	var rerr error
	if path != "" {
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				fs := strings.Fields(sc.Text())
				if len(fs) != 2 || strings.HasPrefix(fs[0], "#") {
					continue
				}
				ip := net.ParseIP(fs[0])
				k := normIP(ip)
				sec, err := strconv.ParseInt(fs[1], 10, 64)
				if k == "" || err != nil || ip.IsUnspecified() {
					continue
				}
				t := time.Unix(sec, 0)
				if now.Sub(t) < peerSavedTTL && t.Before(now.Add(time.Hour)) {
					loaded[k] = t
				}
			}
			f.Close()
		} else if !os.IsNotExist(err) {
			rerr = err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = path
	for k, t := range loaded {
		if o, ok := p.saved[k]; !ok || t.After(o) {
			p.saved[k] = t
		}
	}
	p.trimSavedLocked(now)
	p.rebuildStaticLocked()
	return len(loaded), rerr
}

func (p *PeerAllow) trimSavedLocked(now time.Time) {
	for k, t := range p.saved {
		if now.Sub(t) >= peerSavedTTL {
			delete(p.saved, k)
		}
	}
	if len(p.saved) <= peerSavedMax {
		return
	}
	ks := make([]string, 0, len(p.saved))
	for k := range p.saved {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return p.saved[ks[i]].After(p.saved[ks[j]]) })
	for _, k := range ks[peerSavedMax:] {
		delete(p.saved, k)
	}
}

// noteSaveLocked records an authenticated IP for the save file; a new IP (or
// one last saved over an hour ago) writes the file in the background.
func (p *PeerAllow) noteSaveLocked(k string, now time.Time) {
	if p.store == "" {
		return
	}
	if t, ok := p.saved[k]; ok && now.Sub(t) < peerSaveGap {
		return
	}
	_, had := p.saved[k]
	p.saved[k] = now
	p.trimSavedLocked(now)
	if !had {
		p.rebuildStaticLocked()
	}
	p.dirty = true
	if !p.saving {
		p.saving = true
		go p.saveLoop()
	}
}

func (p *PeerAllow) saveLoop() {
	for {
		p.mu.Lock()
		if !p.dirty || p.store == "" {
			p.saving = false
			p.mu.Unlock()
			return
		}
		p.dirty = false
		path := p.store
		var b strings.Builder
		b.WriteString("# hs2: peer IPs that authenticated a link (ip unix_time), loaded at start for 7 days\n")
		ks := make([]string, 0, len(p.saved))
		for k := range p.saved {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Fprintf(&b, "%s %d\n", k, p.saved[k].Unix())
		}
		p.mu.Unlock()
		writeFileAtomic(path, []byte(b.String()))
	}
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".peer-ips-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Flush waits (up to 1 s) for a pending background save.
func (p *PeerAllow) Flush() {
	for i := 0; i < 500; i++ {
		p.mu.Lock()
		busy := p.saving
		p.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
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
	p.noteSaveLocked(k, now)
	if e := p.learned[k]; e != nil {
		e.Value.(*peerSeen).t = now
		p.lru.MoveToFront(e)
		return
	}
	p.learned[k] = p.lru.PushFront(&peerSeen{ip: k, t: now})
	if q := prefix64(k); q != "" {
		p.learn64[q]++
	}
	for p.lru.Len() > peerLearnMax {
		p.dropLearnedLocked(p.lru.Back())
	}
}

func (p *PeerAllow) dropLearnedLocked(b *list.Element) {
	k := b.Value.(*peerSeen).ip
	delete(p.learned, k)
	p.lru.Remove(b)
	if q := prefix64(k); q != "" {
		if p.learn64[q]--; p.learn64[q] <= 0 {
			delete(p.learn64, q)
		}
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
		p.dropLearnedLocked(b)
	}
	if p.learned[k] != nil {
		return true
	}
	if q := prefix64(k); q != "" {
		return p.net64[q] || p.learn64[q] > 0
	}
	return false
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
	if q := prefix64(k); q != "" {
		k = q // b411: an IPv6 peer is exempt by /64, so its bound is per /64 too
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
	Saved    int   `json:"saved"` // b411: of static, from the save file / authenticated
	Learned  int   `json:"learned"`
	Bound    int   `json:"bound"`
	Exempted int64 `json:"exempted"`
	Capped   int64 `json:"capped"`
}

func (p *PeerAllow) Snap() PeerAllowSnap {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PeerAllowSnap{Static: len(p.static), Saved: len(p.saved), Learned: p.lru.Len(), Bound: p.bound,
		Exempted: p.exempted.Load(), Capped: p.capped.Load()}
}
