package engine

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-path live view for the status file (multipath LB only).
//
// Each status write hands the view the live links of the tunnel — which path
// each is on, its byte counters (linkMeter), its TCP_INFO and the edge's own
// verdicts (degraded, draining, suspect) — and gets one row per path plus a
// totals row. Rates and loss come from cumulative per-path counters sampled
// on every write and compared over lbViewWindow, so they need no timer of
// their own; a link that ends keeps its bytes in its path's total.
//
// Directions are the USERS' directions on both servers: down is Kharej → Iran
// (what the Iran server receives, the Kharej server sends), up the reverse.
// Loss of a direction is the share of data segments its sender resent: the
// sender's own TCP_INFO where this side sends, the peer's retransmits from
// the control pong where the edge receives (the exit has no such report: its
// up loss stays absent).
//
// Rows are paths as THIS side knows them: on the dialing side each
// (local > remote) path of the balancer, on the listening side each address
// it is reached on (the peer's source IPs shown as its remote).

// lbViewWindow is the window rates and loss are measured over.
const lbViewWindow = 10 * time.Second

// lbViewMinSegs: fewer data segments in the window give no loss reading.
const lbViewMinSegs = 50

// LBPathView is one path's row in the status file ("lb_paths"); the totals
// row ("lb_total") has the same shape with Local/Remote empty.
type LBPathView struct {
	Local    string   `json:"local"`  // this side's IP ("*" = the default route)
	Remote   string   `json:"remote"` // the other side's IP(s), comma-separated
	Primary  bool     `json:"primary,omitempty"`
	Source   string   `json:"source,omitempty"`        // advert | local (dialing side)
	SharePct float64  `json:"share_pct"`               // configured share of the links
	EffPct   float64  `json:"eff_pct,omitempty"`       // mode auto: the effective share
	Target   int      `json:"target"`                  // links its share asks for now
	Links    int      `json:"links"`                   // live links on it (dials in flight excluded)
	Serving  int      `json:"serving"`                 // ... taking new connections
	Users    int      `json:"users"`                   // user connections on it
	DownMbit float64  `json:"down_mbit"`               // Kharej → Iran, last lbViewWindow
	UpMbit   float64  `json:"up_mbit"`                 // Iran → Kharej
	DownMB   float64  `json:"down_mb"`                 // since start (this process), MB
	UpMB     float64  `json:"up_mb"`                   //
	LossDown *float64 `json:"loss_down_pct,omitempty"` // resent share, % (absent: no reading)
	LossUp   *float64 `json:"loss_up_pct,omitempty"`
	RTTp50   float64  `json:"rtt_p50_ms,omitempty"` // smoothed RTT over its links, ms
	RTTp99   float64  `json:"rtt_p99_ms,omitempty"`
	MinRTT   float64  `json:"min_rtt_ms,omitempty"` // lowest RTT any of its links saw
	Degraded int      `json:"degraded,omitempty"`   // edge: lossy or stuck, being replaced
	Draining int      `json:"draining,omitempty"`
	Suspect  int      `json:"suspect,omitempty"`  // edge: nothing received lately
	Retiring int      `json:"retiring,omitempty"` // edge: closing once empty
	Fails    int      `json:"fails,omitempty"`    // dialing side: dials failed in a row
	FailsAll uint64   `json:"fails_total,omitempty"`
	State    string   `json:"state"`              // ok | cooling | probing | no-peer | draining
	CooledS  int      `json:"cooled_s,omitempty"` // cooling: seconds until its probe dial

	key string // the view's path key (not in JSON)
}

// pathLink is one live link as the view reads it.
type pathLink struct {
	key           any    // identity across writes
	path          string // its path's key
	local, remote string // its socket's IPs (remote shown on listening rows)
	rx, tx        uint64 // payload bytes this side received / sent
	users         int
	ts            tcpStat
	tsOK          bool
	peerRt        uint64 // edge: the exit's cumulative retransmits (this side's rx loss)
	peerOK        bool
	serving       bool
	degraded      bool
	draining      bool
	suspect       bool
	retiring      bool
}

type pathCounters struct{ rx, tx, rt, so, prt, si uint64 }

func (a *pathCounters) add(b pathCounters) {
	a.rx += b.rx
	a.tx += b.tx
	a.rt += b.rt
	a.so += b.so
	a.prt += b.prt
	a.si += b.si
}

type pathSample struct {
	at time.Time
	c  pathCounters
}

type pathAcc struct {
	cum  pathCounters
	hist []pathSample
}

// pathView keeps the per-path counters between status writes.
type pathView struct {
	mu   sync.Mutex
	exit bool // this side is the exit: what it sends is the users' download
	prev map[any]linkPrev
	acc  map[string]*pathAcc
	tot  pathAcc
	now  func() time.Time
}

type linkPrev struct {
	path string
	c    pathCounters
}

func newPathView(exit bool) *pathView {
	return &pathView{exit: exit, prev: map[any]linkPrev{}, acc: map[string]*pathAcc{}, now: time.Now}
}

func linkCounters(l pathLink) pathCounters {
	c := pathCounters{rx: l.rx, tx: l.tx}
	if l.tsOK {
		c.rt, c.so, c.si = l.ts.retrans, uint64(l.ts.segsOut), uint64(l.ts.segsIn)
	}
	if l.peerOK {
		c.prt = l.peerRt
	}
	return c
}

// delta is cur − prev per counter; a counter that went back (a 32-bit kernel
// counter wrapped, a link reused) counts from zero.
func delta(cur, prev pathCounters) pathCounters {
	d := func(a, b uint64) uint64 {
		if a >= b {
			return a - b
		}
		return a
	}
	return pathCounters{d(cur.rx, prev.rx), d(cur.tx, prev.tx), d(cur.rt, prev.rt), d(cur.so, prev.so), d(cur.prt, prev.prt), d(cur.si, prev.si)}
}

// push records a sample and returns the window's change and its length.
func (a *pathAcc) push(now time.Time) (pathCounters, time.Duration) {
	a.hist = append(a.hist, pathSample{now, a.cum})
	for len(a.hist) >= 2 && now.Sub(a.hist[1].at) >= lbViewWindow {
		a.hist = a.hist[1:]
	}
	if len(a.hist) > 64 { // writes far more often than expected: bound it
		a.hist = a.hist[len(a.hist)-64:]
	}
	old := a.hist[0]
	return delta(a.cum, old.c), now.Sub(old.at)
}

// build turns one set of live links into the rows. defs are the known paths
// in display order (their static fields filled); a link on a path not in defs
// gets a row of its own.
func (v *pathView) build(defs []LBPathView, links []pathLink) ([]LBPathView, *LBPathView) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	rows := append([]LBPathView(nil), defs...)
	idx := map[string]int{}
	for i := range rows {
		idx[rows[i].key] = i
	}
	type agg struct {
		rtts    []float64
		minRTT  float64
		remotes []string
		peer    bool
	}
	ag := make([]agg, len(rows))
	seen := make(map[any]bool, len(links))
	var allRTT []float64
	allMin := 0.0
	peerAny := false
	for _, l := range links {
		i, ok := idx[l.path]
		if !ok {
			lo, re := l.local, l.remote
			if k := strings.IndexByte(l.path, '>'); k >= 0 {
				lo, re = l.path[:k], l.path[k+1:]
			}
			rows = append(rows, LBPathView{Local: lo, Remote: re, State: "ok", key: l.path})
			ag = append(ag, agg{})
			i = len(rows) - 1
			idx[l.path] = i
		}
		r := &rows[i]
		seen[l.key] = true
		cur := linkCounters(l)
		p, had := v.prev[l.key]
		d := cur
		if had && p.path == l.path {
			d = delta(cur, p.c)
		}
		v.prev[l.key] = linkPrev{l.path, cur}
		a := v.acc[l.path]
		if a == nil {
			a = &pathAcc{}
			v.acc[l.path] = a
		}
		a.cum.add(d)
		v.tot.cum.add(d)

		r.Links++
		r.Users += l.users
		if l.serving {
			r.Serving++
		}
		if l.degraded {
			r.Degraded++
		}
		if l.draining {
			r.Draining++
		}
		if l.suspect {
			r.Suspect++
		}
		if l.retiring {
			r.Retiring++
		}
		if l.tsOK && l.ts.rttUs > 0 {
			ms := float64(l.ts.rttUs) / 1000
			ag[i].rtts = append(ag[i].rtts, ms)
			allRTT = append(allRTT, ms)
		}
		if l.tsOK && l.ts.minRttUs > 0 {
			ms := float64(l.ts.minRttUs) / 1000
			if ag[i].minRTT == 0 || ms < ag[i].minRTT {
				ag[i].minRTT = ms
			}
			if allMin == 0 || ms < allMin {
				allMin = ms
			}
		}
		if l.peerOK {
			ag[i].peer = true
		}
		if l.remote != "" && !hasStr(ag[i].remotes, l.remote) {
			ag[i].remotes = append(ag[i].remotes, l.remote)
		}
	}
	for k := range v.prev {
		if !seen[k] {
			delete(v.prev, k)
		}
	}
	tot := &LBPathView{State: "ok"}
	for i := range rows {
		r := &rows[i]
		a := v.acc[r.key]
		if a == nil {
			a = &pathAcc{}
			v.acc[r.key] = a
		}
		d, dt := a.push(now)
		v.fill(r, a.cum, d, dt, ag[i].peer)
		peerAny = peerAny || ag[i].peer
		r.RTTp50, r.RTTp99 = pctl(ag[i].rtts, 50), pctl(ag[i].rtts, 99)
		r.MinRTT = round1pct(ag[i].minRTT)
		if r.Remote == "" && len(ag[i].remotes) > 0 {
			sort.Strings(ag[i].remotes)
			r.Remote = strings.Join(ag[i].remotes, ",")
		}
		tot.SharePct += r.SharePct
		tot.Target += r.Target
		tot.Links += r.Links
		tot.Serving += r.Serving
		tot.Users += r.Users
		tot.Degraded += r.Degraded
		tot.Draining += r.Draining
		tot.Suspect += r.Suspect
		tot.Retiring += r.Retiring
		tot.FailsAll += r.FailsAll
	}
	d, dt := v.tot.push(now)
	v.fill(tot, v.tot.cum, d, dt, peerAny)
	tot.SharePct = round1pct(tot.SharePct)
	tot.RTTp50, tot.RTTp99, tot.MinRTT = pctl(allRTT, 50), pctl(allRTT, 99), round1pct(allMin)
	return rows, tot
}

// fill sets a row's rates, totals and loss from its counters.
// peerKnown: some link of the row has the exit's retransmit report.
func (v *pathView) fill(r *LBPathView, cum, d pathCounters, dt time.Duration, peerKnown bool) {
	rx, tx := 0.0, 0.0
	if s := dt.Seconds(); s >= 1 {
		rx, tx = float64(d.rx)*8/1e6/s, float64(d.tx)*8/1e6/s
	}
	var own, peer *float64
	if d.so >= lbViewMinSegs {
		f := round2(100 * float64(d.rt) / float64(d.so))
		own = &f
	}
	if peerKnown && !v.exit && d.si >= lbViewMinSegs {
		f := round2(100 * float64(d.prt) / float64(d.si))
		peer = &f
	}
	if v.exit { // the exit sends the download
		r.DownMbit, r.UpMbit = round2(tx), round2(rx)
		r.DownMB, r.UpMB = mb(cum.tx), mb(cum.rx)
		r.LossDown = own
	} else {
		r.DownMbit, r.UpMbit = round2(rx), round2(tx)
		r.DownMB, r.UpMB = mb(cum.rx), mb(cum.tx)
		r.LossUp, r.LossDown = own, peer
	}
}

func mb(b uint64) float64 { return round1pct(float64(b) / 1e6) }

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// pctl is the nearest-rank percentile of xs (sorted in place), rounded to
// 0.1 ms; 0 for none.
func pctl(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	k := (p*len(xs) + 99) / 100
	if k < 1 {
		k = 1
	}
	return round1pct(xs[k-1])
}

func hasStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// pathIPs splits a "local>remote" key into its IPs.
func pathIPs(k string) (string, string) {
	if i := strings.IndexByte(k, '>'); i >= 0 {
		return k[:i], k[i+1:]
	}
	return k, ""
}

// listenDefs is the listening side's rows: each announced address with its
// share; targets split total links by weight (largest remainder).
func listenDefs(mine []LBAdvertIP, total int) []LBPathView {
	W := 0
	for _, e := range mine {
		W += max(e.Weight, 0)
	}
	out := make([]LBPathView, len(mine))
	w := make([]int, len(mine))
	for i, e := range mine {
		out[i] = LBPathView{Local: e.IP, State: "ok", Primary: i == 0, key: e.IP}
		w[i] = max(e.Weight, 0)
		if W > 0 {
			out[i].SharePct = round1pct(100 * float64(w[i]) / float64(W))
		}
	}
	if W > 0 {
		cnt := make([]int, len(mine))
		for k := 0; k < total; k++ {
			j := pickLargestRemainder(w, cnt, func(int) bool { return true }, 0)
			cnt[j]++
		}
		for i := range out {
			out[i].Target = cnt[i]
		}
	}
	return out
}

// listenKey is a listening-side link's row key: the local address it came in on.
func listenKey(path string) string {
	l, _ := pathIPs(path)
	return l
}

// meterLinks reads exit-side links from their meters (users, bytes, the
// socket's TCP_INFO); keyOf gives each one's row key ("" skips it).
func meterLinks(ms []*linkMeter, keyOf func(*linkMeter) string) []pathLink {
	out := make([]pathLink, 0, len(ms))
	for _, m := range ms {
		k := keyOf(m)
		if k == "" {
			continue
		}
		lo, re := pathIPs(m.path)
		pl := pathLink{key: m, path: k, local: lo, remote: re, rx: m.rdBytes.Load(), tx: m.wrBytes.Load(),
			users: int(m.users.Load()), serving: true}
		if m.tcp != nil {
			pl.ts, pl.tsOK = tcpStats(m.tcp)
		}
		out = append(out, pl)
	}
	return out
}

// meters lists the live links' meters.
func (p *linkPeers) meters() []*linkMeter {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*linkMeter, 0, len(p.m))
	for m := range p.m {
		out = append(out, m)
	}
	return out
}

// viewDefs is the dialing side's rows: each balancer path with its share,
// target, failures and state (Stats), in configured order.
func (b *LB) viewDefs() []LBPathView {
	st := b.Stats()
	b.mu.Lock()
	now := b.now()
	until := map[string]time.Time{}
	paths := map[string]LBPath{}
	for j, p := range b.paths {
		until[p.Name()] = b.until[j]
		paths[p.Name()] = p
	}
	b.mu.Unlock()
	out := make([]LBPathView, 0, len(st))
	for _, s := range st {
		p := paths[s.Path]
		lo := p.Local
		if lo == "" {
			lo = "*"
		}
		r := LBPathView{Local: lo, Remote: p.Remote, Primary: s.Primary, Source: s.Source, SharePct: s.SharePct,
			EffPct: s.EffPct, Target: s.Target, Fails: s.Fails, FailsAll: s.FailsAll, State: s.State, key: s.Path}
		if u := until[s.Path]; !u.IsZero() && u.After(now) {
			r.CooledS = int(u.Sub(now).Seconds() + 0.5)
		}
		out = append(out, r)
	}
	return out
}

// viewLinks is the dialing exit's live links (reverse): each held link with
// a meter, keyed by its balancer path. TCP_INFO is read outside the lock.
func (b *LB) viewLinks() []pathLink {
	type held struct {
		key  any
		path string
		h    lbHooks
	}
	b.mu.Lock()
	hs := make([]held, 0, len(b.hooks))
	for k, h := range b.hooks {
		if i, ok := b.held[k]; ok && h.mtr != nil {
			hs = append(hs, held{k, b.paths[i].Name(), h})
		}
	}
	b.mu.Unlock()
	out := make([]pathLink, 0, len(hs))
	for _, x := range hs {
		m := x.h.mtr
		lo, re := pathIPs(m.path)
		pl := pathLink{key: x.key, path: x.path, local: lo, remote: re, rx: m.rdBytes.Load(), tx: m.wrBytes.Load(),
			users: int(m.users.Load()), serving: true}
		if x.h.stats != nil {
			pl.ts, pl.tsOK = x.h.stats()
		}
		out = append(out, pl)
	}
	return out
}

// pathOf is the balancer path name a live link is held on ("" = none).
func (b *LB) pathOf(link any) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i, ok := b.held[link]; ok {
		return b.paths[i].Name()
	}
	return ""
}

// viewLinks is the edge's live links for the per-path view; keyOf gives each
// one's row key ("" = its own "local>remote" path). TCP_INFO is read outside
// the lock.
func (m *LinkManager) viewLinks(keyOf func(ml *managedLink) string) []pathLink {
	type snap struct {
		ml *managedLink
		pl pathLink
	}
	m.mu.RLock()
	ss := make([]snap, 0, len(m.links))
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		pl := pathLink{key: ml, users: int(ml.users.Load()), serving: ml.serving(), degraded: ml.degraded,
			draining: ml.draining, suspect: ml.suspect, retiring: ml.retiring && !ml.degraded && !ml.draining}
		if ml.mtr != nil {
			pl.local, pl.remote = pathIPs(ml.mtr.path)
			pl.path = ml.mtr.path
			pl.rx, pl.tx = ml.mtr.rdBytes.Load(), ml.mtr.wrBytes.Load()
			pl.peerOK = ml.mtr.peerSeen.Load()
			pl.peerRt = ml.mtr.peerRetrans.Load()
		}
		ss = append(ss, snap{ml, pl})
	}
	m.mu.RUnlock()
	out := make([]pathLink, 0, len(ss))
	for _, s := range ss {
		if keyOf != nil {
			if k := keyOf(s.ml); k != "" {
				s.pl.path = k
			}
		}
		if s.pl.path == "" {
			s.pl.path = "?"
		}
		s.pl.ts, s.pl.tsOK = linkTCPStatsOf(s.ml.link)
		out = append(out, s.pl)
	}
	return out
}
