package engine

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"time"
)

// b331: HS2_B331_SPAREPATH (both sides, reverse, HS2_LB_AUTO2; default off).
//
// After a cut path recovers it stays under-linked for about an hour in
// production (rebal329): the edge holds ~30 % of its links retiring
// (HS2_RETIRE_KEEP_IDLE keeps their idle users up to 60 min), and those sit on
// OTHER paths. The exit never redials for the recovered path: its room check
// (edgeRoomLocked) counts the retiring links, it groups per edge address
// across both of its own IPs, and addSpares stops at fillLimit, which the
// retiring links fill. HS2_LB_SWAP cannot help: the recovered path has no
// retiring link of its own to bring back.
//
// With the key on:
//   - the edge appends its per-path SERVING (and retiring) link counts to the
//     advert it sends every lbAdvertEvery (an optional block after the advert,
//     advert version 3; see below for both directions of compatibility);
//   - the exit, when the counts are fresh, dials ONE spare for a specific PATH
//     (pinned: acquireLocked takes the pin first) when that path is at least
//     sparePathUnder serving links under its share of the edge's serving
//     links, even past the slot cap (fillLimit) — at most one per path per
//     sparePathGap and sparePathPerMin in all, never past max_links or
//     2·target+8 slots, never during an outage;
//   - the exit's room check (spareForGap) counts the edge's serving links of
//     the path instead of every link to its address;
//   - the edge takes an arriving link as serving when its PATH is under its
//     share of the serving links (the exit's report shares), before the
//     per-address rule (arrivalWantedLocked). Reconcile then retires a serving
//     link of the path most over its share once empty, so the serving count
//     stays at the target.
//
// Wire (edge -> exit, inside kindLBAdvert):
//
//	[advert ver=3 ...][front ext, if any][serving block]
//	serving block: [0xB3][ver][len u16][n u8] n × ([iplen][edge ip][iplen][exit ip][serving u16][retiring u16])
//	               ver 2 appends (inside len): [m u8] m × ([iplen][edge ip][target u16])
//
// b336 (ver 2): the edge's own per-IP serving target (its reconcile shares,
// shrinkShares). The exit computes shares per PATH, the edge balances per
// Iran IP: a spare for a path whose Iran IP was already at its target got
// "not replaced" again in the same tick (rb331 v6 dial loop). sparePathTick
// now dials only when the path's Iran IP is ALSO >= 1 serving link under its
// target (the edge's, or the exit's per-IP share sum from an old edge). An
// old exit reads exactly len bytes and the n entries: the targets are skipped
// unread.
//
// An old exit reads ver >= 2 as "a front ext follows": it reads the front ext
// if there is one and drops the rest unread, or (no front ext) reads the
// block's 4-byte header as a front ext header, rejects it (magic) and goes on
// with no fronts — exactly what a ver 2 advert without fronts gave it. An old
// edge sends no block: the new exit keeps the counts unknown and behaves as
// before.
var b331SparePath bool // loadEnvKeys: HS2_B331_SPAREPATH

const (
	lbAdvertVerServ = 3
	servBlockMagic  = 0xB3
	servBlockVer    = 2 // b336: per edge IP targets appended
	servBlockMax    = 64
	sparePathUnder  = 2.0              // serving links under the path's share
	sparePathGap    = 30 * time.Second // per path
	// sparePathSteady: no spare for a path whose weight factor was under
	// 0.95 in the last minute. rb331 v4 (lab3): a path flapping 0.33 <-> 1.00
	// on loss spikes got a spare each time the exit saw 1.00, the edge judged
	// the arrival on the lowered share, the link came up born-retiring and the
	// exit closed it 10 s later: 6 wasted dials in 5 min (factor 0.4-0.56, up to
	// 1.00 for 10-25 s, each dial 0-5 s after the jump).
	sparePathSteady  = time.Minute
	sparePathPerMin  = 6                // in all
	sparePathFresh   = 45 * time.Second // the edge's counts (3 advert rounds)
	sparePathPinLife = 30 * time.Second
)

// edgePathCount is one path's link counts at the edge.
type edgePathCount struct {
	Edge, Exit        string // IPs
	Serving, Retiring int
	// b336 (block ver 2): the edge's serving target for the Edge IP as a
	// whole (the same on each of its paths); HasTarget false: not sent.
	EdgeTarget int
	HasTarget  bool
}

var errServBlock = errors.New("lb advert: malformed serving block")

func encodeServBlock(cs []edgePathCount) []byte {
	if len(cs) == 0 {
		return nil
	}
	body := []byte{0}
	n := 0
	for _, c := range cs {
		e, x := net.ParseIP(c.Edge), net.ParseIP(c.Exit)
		if e == nil || x == nil || n >= servBlockMax {
			continue
		}
		if v := e.To4(); v != nil {
			e = v
		}
		if v := x.To4(); v != nil {
			x = v
		}
		body = append(body, byte(len(e)))
		body = append(body, e...)
		body = append(body, byte(len(x)))
		body = append(body, x...)
		body = binary.BigEndian.AppendUint16(body, uint16(min(max(c.Serving, 0), 0xFFFF)))
		body = binary.BigEndian.AppendUint16(body, uint16(min(max(c.Retiring, 0), 0xFFFF)))
		n++
	}
	if n == 0 {
		return nil
	}
	body[0] = byte(n)
	// b336: per edge IP targets (each IP once), kept within the length an
	// old exit accepts.
	var tb []byte
	m, seen := 0, map[string]bool{}
	for _, c := range cs {
		e := net.ParseIP(c.Edge)
		if !c.HasTarget || e == nil || seen[c.Edge] || m >= servBlockMax {
			continue
		}
		seen[c.Edge] = true
		if v := e.To4(); v != nil {
			e = v
		}
		tb = append(tb, byte(len(e)))
		tb = append(tb, e...)
		tb = binary.BigEndian.AppendUint16(tb, uint16(min(max(c.EdgeTarget, 0), 0xFFFF)))
		m++
	}
	if m > 0 && len(body)+1+len(tb) <= servBlockMax*(2+32+4)+1 {
		body = append(body, byte(m))
		body = append(body, tb...)
	}
	h := []byte{servBlockMagic, servBlockVer, 0, 0}
	binary.BigEndian.PutUint16(h[2:], uint16(len(body)))
	return append(h, body...)
}

func readServBlock(r io.Reader) ([]edgePathCount, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if h[0] != servBlockMagic || h[1] < 1 {
		return nil, errServBlock
	}
	ln := int(binary.BigEndian.Uint16(h[2:]))
	if ln < 1 || ln > servBlockMax*(2+32+4)+1 {
		return nil, errServBlock
	}
	buf := make([]byte, ln)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	n := int(buf[0])
	b := buf[1:]
	ip := func() (string, bool) {
		if len(b) < 1 || (b[0] != 4 && b[0] != 16) || len(b) < 1+int(b[0]) {
			return "", false
		}
		s := net.IP(b[1 : 1+b[0]]).String()
		b = b[1+b[0]:]
		return s, true
	}
	out := make([]edgePathCount, 0, n)
	for i := 0; i < n; i++ {
		e, ok1 := ip()
		x, ok2 := ip()
		if !ok1 || !ok2 || len(b) < 4 {
			return nil, errServBlock
		}
		out = append(out, edgePathCount{Edge: e, Exit: x, Serving: int(binary.BigEndian.Uint16(b)), Retiring: int(binary.BigEndian.Uint16(b[2:]))})
		b = b[4:]
	}
	if h[1] >= 2 && len(b) >= 1 { // b336: per edge IP targets
		m := int(b[0])
		b = b[1:]
		tg := map[string]int{}
		for i := 0; i < m; i++ {
			e, ok := ip()
			if !ok || len(b) < 2 {
				return out, nil // counts stand, targets dropped
			}
			tg[e] = int(binary.BigEndian.Uint16(b))
			b = b[2:]
		}
		for i := range out {
			if t, ok := tg[out[i].Edge]; ok {
				out[i].EdgeTarget, out[i].HasTarget = t, true
			}
		}
	}
	return out, nil
}

// SetServing installs the edge's per-path count builder (nil: none sent).
func (a *LBAdvert) SetServing(f func() []edgePathCount) {
	a.mu.Lock()
	a.serving = f
	a.mu.Unlock()
}

// OnServing sets the exit's listener for the edge's per-path counts.
func (a *LBAdvert) OnServing(f func([]edgePathCount)) {
	a.mu.Lock()
	a.onServing = f
	a.mu.Unlock()
}

// servBlockOut is the edge's block for this exchange (nil: none).
func (a *LBAdvert) servBlockOut() []byte {
	a.mu.Lock()
	f := a.serving
	a.mu.Unlock()
	if f == nil {
		return nil
	}
	return encodeServBlock(f())
}

// readAdvertTail (exit) reads what follows a ver >= 2 advert: the front ext
// and, from ver 3, the serving block (either may be absent).
func (a *LBAdvert) readAdvertTail(br *bufio.Reader, l []LBAdvertIP, ver byte) {
	if ver < lbAdvertVerServ {
		if m, err := readFrontExt(br); err == nil {
			attachFronts(l, m)
		}
		return
	}
	if peekFrontExt(br) {
		if m, err := readFrontExt(br); err == nil {
			attachFronts(l, m)
		}
	}
	if b, err := br.Peek(1); err != nil || b[0] != servBlockMagic {
		return
	}
	cs, err := readServBlock(br)
	if err != nil {
		return
	}
	a.mu.Lock()
	f := a.onServing
	a.mu.Unlock()
	if f != nil {
		f(cs)
	}
}

// --- edge -------------------------------------------------------------------

// edgePathCounts is the edge's serving and retiring links per path.
func (m *LinkManager) edgePathCounts() []edgePathCount {
	m.mu.RLock()
	idx := map[string]int{}
	var out []edgePathCount
	for _, ml := range m.links {
		p := mlPath(ml) // edge view: "edgeIP>exitIP"
		if p == "" || !ml.link.Alive() {
			continue
		}
		i, ok := idx[p]
		if !ok {
			j := strings.IndexByte(p, '>')
			if j < 0 {
				continue
			}
			i = len(out)
			idx[p] = i
			out = append(out, edgePathCount{Edge: p[:j], Exit: p[j+1:]})
		}
		if ml.serving() {
			out[i].Serving++
		} else {
			out[i].Retiring++
		}
	}
	tg := m.edgeIPTargetsLocked()
	m.mu.RUnlock()
	for i := range out {
		if t, ok := tg[out[i].Edge]; ok {
			out[i].EdgeTarget, out[i].HasTarget = t, true
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Edge+out[a].Exit < out[b].Edge+out[b].Exit })
	return out
}

// edgeIPTargetsLocked (b336) is the edge's serving target per own IP: its
// serving links split by the shares reconcile keeps to (shrinkShares; the
// exit's report under HS2_LB_AUTO2). nil: no shares. Caller holds m.mu (read).
func (m *LinkManager) edgeIPTargetsLocked() map[string]int {
	if m.shrinkShares == nil {
		return nil
	}
	var ms []*managedLink
	for _, x := range m.links {
		if x.link.Alive() && x.serving() {
			ms = append(ms, x)
		}
	}
	if len(ms) == 0 {
		return nil
	}
	_, w := m.shrinkShares(ms)
	W := 0
	for _, x := range w {
		if x > 0 {
			W += x
		}
	}
	if W == 0 {
		return nil
	}
	out := map[string]int{}
	for ip, x := range w {
		if x > 0 {
			out[ip] = int(float64(len(ms))*float64(x)/float64(W) + 0.5)
		}
	}
	return out
}

// arrivalPathLocked (key on): an arriving link serves when its path is under
// its share (the exit's report) of the serving links plus it. known is false
// without a fresh report. Caller holds m.mu.
func (m *LinkManager) arrivalPathLocked(path string, serving []*managedLink) (ok, known bool) {
	st := m.steer
	if !b331SparePath || st == nil || !st.active {
		return false, false
	}
	w, in := st.repShare[path]
	if !in || st.repCut[path] || w <= 0 {
		return false, false
	}
	W := 0.0
	for p, x := range st.repShare {
		if !st.repCut[p] && x > 0 {
			W += x
		}
	}
	if W <= 0 {
		return false, false
	}
	np := 0
	for _, x := range serving {
		if mlPath(x) == path {
			np++
		}
	}
	return float64(np+1) <= float64(len(serving)+1)*w/W+0.5, true
}

// --- exit -------------------------------------------------------------------

// sparePathState is the exit's view of the edge's counts and its own asks.
type sparePathState struct {
	serv   map[string]int // exit path name ("exitIP>edgeIP") -> serving links at the edge
	total  int            // serving links at the edge
	at     time.Time
	last   map[string]time.Time // the last spare per path
	recent []time.Time          // spares in the last minute
	pins   []sparePin
	lowAt  map[string]time.Time // the last tick the path's factor was < 0.95 (sparePathSteady)
	dials  int                  // spares started in all (status/log)
	// b336: serving links and the edge's own target per edge IP (ipTgt
	// absent for an IP: an old edge, the exit's per-IP share decides).
	ipServ map[string]int
	ipTgt  map[string]int
	ipSkip int // spares skipped: their edge IP at/over its target (status/log)
}

type sparePin struct {
	path  string
	until time.Time
}

// setEdgeCounts stores the edge's counts (exit; from the advert).
func (b *LB) setEdgeCounts(cs []edgePathCount) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sp331 == nil {
		b.sp331 = &sparePathState{last: map[string]time.Time{}}
	}
	s := b.sp331
	s.serv, s.total, s.at = map[string]int{}, 0, b.now()
	s.ipServ, s.ipTgt = map[string]int{}, map[string]int{}
	for _, c := range cs {
		s.serv[c.Exit+">"+c.Edge] += c.Serving
		s.total += c.Serving
		s.ipServ[c.Edge] += c.Serving
		if c.HasTarget {
			s.ipTgt[c.Edge] = c.EdgeTarget
		}
	}
}

// setSparePast installs the pool's past-the-cap spare dial.
func (b *LB) setSparePast(f func() int) {
	b.mu.Lock()
	b.sparePast = f
	b.mu.Unlock()
}

// edgeCountsLocked returns the edge's fresh counts (nil: none / stale).
// Caller holds b.mu.
func (b *LB) edgeCountsLocked(now time.Time) *sparePathState {
	s := b.sp331
	if !b331SparePath || s == nil || s.serv == nil || now.Sub(s.at) > sparePathFresh || s.total == 0 {
		return nil
	}
	return s
}

// servShareLocked is path j's share of the edge's serving links (0: not
// eligible). Caller holds b.mu.
func (b *LB) servShareLocked(j int, eff []int, s *sparePathState, now time.Time) float64 {
	W := 0
	for i := range b.paths {
		if b.eligibleLocked(i, now) && eff[i] > 0 {
			W += eff[i]
		}
	}
	if W == 0 || !b.eligibleLocked(j, now) || eff[j] <= 0 {
		return 0
	}
	return float64(s.total) * float64(eff[j]) / float64(W)
}

// edgeRoomServLocked: edgeRoomLocked in the edge's serving frame, per path
// (key on, fresh counts). Caller holds b.mu.
func (b *LB) edgeRoomServLocked(u int, eff []int, now time.Time) (int, bool) {
	s := b.edgeCountsLocked(now)
	if s == nil || b.paths[u].Local == "" {
		return 0, false
	}
	c := s.serv[b.paths[u].Name()]
	if c < lb2Keep {
		return lb2Keep - c, true
	}
	W := 0
	for i := range b.paths {
		if b.eligibleLocked(i, now) && eff[i] > 0 {
			W += eff[i]
		}
	}
	if W == 0 || eff[u] <= 0 {
		return 0, true
	}
	k := 0
	for k < 8 && float64(c+k+1) <= float64(s.total+k+1)*float64(eff[u])/float64(W)+0.5 {
		k++
	}
	return k, true
}

// edgeIPUnderLocked (b336): the edge IP as a whole is at least 1 serving
// link under its target — the edge's own (block ver 2) or, from an old edge,
// the sum of its paths' shares here. Caller holds b.mu.
func (b *LB) edgeIPUnderLocked(ip string, eff []int, s *sparePathState, now time.Time) bool {
	if t, ok := s.ipTgt[ip]; ok {
		return t-s.ipServ[ip] >= 1
	}
	sh := 0.0
	for i, p := range b.paths {
		if p.Remote == ip && p.Local != "" {
			sh += b.servShareLocked(i, eff, s, now)
		}
	}
	return sh-float64(s.ipServ[ip]) >= 1
}

// pinLocked takes a pending pin for acquireLocked (-1: none). Caller holds
// b.mu.
func (b *LB) pinLocked(now time.Time) int {
	s := b.sp331
	if s == nil || len(s.pins) == 0 {
		return -1
	}
	for len(s.pins) > 0 {
		p := s.pins[0]
		s.pins = s.pins[1:]
		if now.After(p.until) {
			continue
		}
		if j := b.indexLocked(p.path); j >= 0 && b.eligibleLocked(j, now) {
			return j
		}
	}
	return -1
}

// sparePathTick (exit, each rebalance round): one spare for the path furthest
// under its share of the edge's serving links, by sparePathUnder or more.
func (b *LB) sparePathTick(now time.Time) bool {
	b.mu.Lock()
	s := b.edgeCountsLocked(now)
	f := b.sparePast
	if s == nil || f == nil {
		b.mu.Unlock()
		return false
	}
	keep := s.recent[:0]
	for _, t := range s.recent {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	s.recent = keep
	pins := s.pins[:0]
	for _, q := range s.pins {
		if !now.After(q.until) {
			pins = append(pins, q)
		}
	}
	s.pins = pins
	if len(s.recent) >= sparePathPerMin {
		b.mu.Unlock()
		return false
	}
	eff := b.effWeightsLocked()
	u, uBy, uShare := -1, 0.0, 0.0
	for j, p := range b.paths {
		if p.Local == "" {
			continue
		}
		name := p.Name()
		if t, ok := s.last[name]; ok && now.Sub(t) < sparePathGap {
			continue
		}
		pinned := false
		for _, q := range s.pins {
			if q.path == name && !now.After(q.until) {
				pinned = true
			}
		}
		if pinned {
			continue
		}
		if j < len(b.mult) && b.mult[j] < 0.95 {
			if s.lowAt == nil {
				s.lowAt = map[string]time.Time{}
			}
			s.lowAt[name] = now
			continue
		}
		if t, ok := s.lowAt[name]; ok && now.Sub(t) < sparePathSteady {
			continue
		}
		sh := b.servShareLocked(j, eff, s, now)
		if d := sh - float64(s.serv[name]); d >= sparePathUnder && d > uBy {
			if !b.edgeIPUnderLocked(p.Remote, eff, s, now) { // b336
				s.ipSkip++
				continue
			}
			u, uBy, uShare = j, d, sh
		}
	}
	if u < 0 {
		b.mu.Unlock()
		return false
	}
	name := b.paths[u].Name()
	have, total := s.serv[name], s.total
	s.pins = append(s.pins, sparePin{path: name, until: now.Add(sparePathPinLife)})
	b.mu.Unlock()
	got := f() // the pool's lock, not under b.mu
	b.mu.Lock()
	if got == 0 { // drop the pin again
		for i, q := range s.pins {
			if q.path == name {
				s.pins = append(s.pins[:i], s.pins[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		return false
	}
	s.last[name] = now
	s.recent = append(s.recent, now)
	s.dials++
	n, all := len(s.recent), s.dials
	b.mu.Unlock()
	b.log("lb sparepath: %s has %d serving link(s) at the edge, %.1f under its share %.1f of %d: spare dial started for it (past the slot cap if need be) [%d in the last min, %d in all]",
		name, have, uBy, uShare, total, n, all)
	return true
}

// addSparePast (exit pool, key on): one spare slot even at or past the slot
// cap (fillLimit), for a path the balancer pinned. Never past max_links or
// 2·target+8 slots, never with 2 spares already pending or in an outage.
func (p *exitPool) addSparePast() int {
	if zduDraining.Load() { // ZDU B6: no spares while draining (zdu_drainmode.go)
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pending := 0
	for _, s := range p.slots {
		if s.spare {
			pending++
		}
	}
	cur := p.curSlotsLocked() // HS2_ZDU legacy slots do not count
	if p.outage || p.want <= 0 || pending >= 2 || cur >= p.max || cur >= 2*p.want+8 {
		return 0
	}
	p.startSlotLocked()
	p.slots[len(p.slots)-1].spare = true
	return 1
}
