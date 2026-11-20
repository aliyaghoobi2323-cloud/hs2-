package engine

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HS2_B327_PICKLIST (b327; default ON since b334 / rel69p, "=0" off; st327
// lab2 5-round A/B k16/slack 0: pick CPU -65..-75 %, no latency metric
// significantly worse; full -race green on b325 2ed2f9e8): a cheaper
// user-connection fast pick (pickHeldFast) for reconnect storms.
//
// Why (st325, lab2): the storm's cost is the pick scan itself, ~130 us per
// reopen over ~300 links; 36 % of it the smux closed flag (false sharing, see
// smux Session.dead padding) and ~25 % steerRankLocked's per-link string-map
// lookup of the path's steering factor.
//
// What:
//
//	(a) the steering factor of each link's path is cached on the link at
//	    every steer tick (write lock), so steerRankLocked reads a field
//	    instead of looking the path up in repFactor. A link that joined
//	    after the tick (b327FacOK false) still looks it up.
//	(b) a short candidate list: per steering group (a path's links not
//	    turned off; the off links of all paths) the best b327K serving
//	    (tier 0) links by the pick key without the path rank, plus the
//	    group's next-best link and its key at build time (the group's
//	    "threshold"). A pick scans the list only (~8 links a group instead of
//	    ~300), computing each candidate's CURRENT key exactly as
//	    pickLockedFP does, and takes the best (ties at random). It is valid
//	    because between two builds a non-candidate's key can only get worse
//	    (placements raise load and the burst cap's count; everything else in
//	    the key — flags, flowing, heavy, pressure, steering, the pool itself
//	    — changes only under the write lock, which bumps the epoch and so
//	    invalidates the list): if no group's threshold, ranked for this
//	    connection's drawn path, is in a better class than the chosen
//	    candidate, and no threshold of the same class is lighter by more
//	    than b327Slack, the full scan would have chosen a link of the same
//	    class and at most b327Slack lighter. Otherwise (stale, empty, every
//	    candidate dead, a link still waiting for kindInfo, too many groups
//	    or stripe-held links) the list is rebuilt by one picker (one full pass, as
//	    one old pick) and others take the full scan meanwhile.
//
// Guarantees kept: tier 0 only when a tier-0 link is chosen (no live
// candidate: full scan, which does the tiers); class order cut path ->
// burst cap / pressure -> path rank -> light before heavy exactly as
// pickKey.less; stripe-held links (whose stripe can end lock-free, which
// makes them better) are always candidates, ranked live as pickLockedFP
// ranks them; the fpPicks compare-and-swap of lk162 unchanged. No
// allocation on the pick (seqlock of atomics; scratch reused on rebuild).
var b327PickList atomic.Bool

// b327PickListDefault: the key's default (b334: on, see above).
const b327PickListDefault = true

func init() { b327PickList.Store(envBoolDefault("HS2_B327_PICKLIST", b327PickListDefault)) }

const (
	b327KMax = 16 // max candidates per steering group (HS2_B327_K)
	b327Grp  = 12 // max steering groups (paths + off); more: no list
	b327Str  = 32 // max stripe-held candidates; more: no list
	b327Max  = b327KMax*b327Grp + b327Str
)

// b327K is the number of candidates per steering group (HS2_B327_K, 1..16,
// default 16; st327 ran 8). More candidates spread a burst of picks between
// two builds over more links (st327b: storm open tails with 8).
var b327K = 16

// b327Slack is the load a list pick may exceed a group's threshold by
// (HS2_B327_SLACK, default 0 since st327b; st327 ran 1). 0: a list pick is
// always at its class's lightest load, as the full scan's (st327: with
// slack 1 about half the picks of a burst land one placement above the
// lightest link, two placements per candidate per build).
var b327Slack = 0

func init() {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_B327_SLACK"))); err == nil && v >= 0 {
		b327Slack = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_B327_K"))); err == nil && v >= 1 && v <= b327KMax {
		b327K = v
	}
	b327Cfg = fmt.Sprintf(" k=%d slack=%d", b327K, b327Slack)
}

// b327Cfg: k and slack as configured at start, for b327Line. The memlog
// goroutine must not read b327K/b327Slack themselves: tests change them
// while it runs (data race under -race; never changed in production).
var b327Cfg string

var b327Stat struct {
	list    atomic.Uint64 // picks decided from the list
	build   atomic.Uint64 // list (re)builds
	stale   atomic.Uint64 // list found stale / invalid -> rebuild or full scan
	full    atomic.Uint64 // picks that took the full scan (list unusable, rebuild busy)
	scanned atomic.Uint64 // candidates examined by list picks
}

// b327Thr is a group's next-best link and its key parts at build time.
type b327Thr struct {
	ml                     atomic.Pointer[managedLink]
	cutNew, pressed, heavy atomic.Bool
	load                   atomic.Int64
}

// b327State is the candidate list, a seqlock of atomics (ver odd while
// a build writes it). build/scratch are guarded by mu (TryLock).
type b327State struct {
	epoch atomic.Uint64 // bumped under the pool's write lock on every change

	ver     atomic.Uint64
	built   atomic.Uint64 // epoch the list was built at (0: never)
	failed  atomic.Uint64 // epoch+1 a build found the list unusable at
	n       atomic.Int32
	nThr    atomic.Int32
	cand    [b327Max]atomic.Pointer[managedLink]
	thr     [b327Grp]b327Thr
	mu      sync.Mutex
	groups  [b327Grp]b327Group
	striped [b327Str]*managedLink
}

type b327Ent struct {
	ml   *managedLink
	key  pickKey
	load int
}

type b327Group struct {
	path string
	off  bool
	n    int
	e    [b327KMax + 1]b327Ent
}

// b327Bump invalidates the list. Caller holds m.mu for writing.
func (m *LinkManager) b327Bump() { m.b327.epoch.Add(1) }

// b327FacTickLocked caches every link's steering factor (a). Caller holds
// m.mu for writing (steerTickLocked).
func (m *LinkManager) b327FacTickLocked() {
	m.b327Bump()
	for _, ml := range m.links {
		ml.b327Fac, ml.b327FacOK = m.steerFactor(mlPath(ml)), true
	}
}

// steerFactorOf is m.steerFactor(mlPath(ml)), from the per-tick cache when
// HS2_B327_PICKLIST is on and the link has one.
func (m *LinkManager) steerFactorOf(ml *managedLink, p string) float64 {
	if ml.b327FacOK && b327PickList.Load() {
		return ml.b327Fac
	}
	return m.steerFactor(p)
}

// b327Key is ml's pick key as pickLockedFP computes it (limit 0, stripes
// off), and its fpPicks as read. ok=false: not a tier-0 candidate now.
func (m *LinkManager) b327Key(ml *managedLink, drawn string) (pickKey, int32, bool) {
	if !ml.aliveCheap() || ml.retiring || ml.degraded || ml.draining || ml.suspect || ml.zduOld || !m.infoReadyLocked(ml) {
		return pickKey{}, 0, false
	}
	fp := ml.fpPicks.Load()
	picks := ml.picks + int(fp)
	k := newPickKey(ml.pressed, ml.flowing, picks, picks+ml.histPicks(), int(ml.users.Load()), m.perLink)
	k.heavy = pickClass && (ml.heavy > 0 || stripeOn(ml))
	k.pressed = k.pressed || stripeGuardPressed(ml)
	k.path = m.steerRankLocked(ml, drawn)
	k.cutNew = m.steerCutNewLocked(ml)
	if stripeOn(ml) {
		k.path = stripeSteerRank
	}
	return k, fp, true
}

// pickListFP is pickLockedFP(0) from the candidate list; ok=false: the
// caller runs the full scan. Caller holds m.mu for reading.
func (m *LinkManager) pickListFP() (ml *managedLink, fp int32, ok bool) {
	if m.b327.failed.Load() == m.b327.epoch.Load()+1 {
		b327Stat.full.Add(1)
		return nil, 0, false // unusable until the pool changes
	}
	drawn := m.steerDrawLocked(false, func(ml *managedLink) bool {
		return ml.aliveCheap() && m.infoReadyLocked(ml) && ml.serving() && !stripeOn(ml)
	})
	for try := 0; try < 2; try++ {
		if ml, fp, ok = m.pickListTry(drawn); ok {
			b327Stat.list.Add(1)
			return
		}
		b327Stat.stale.Add(1)
		if try == 0 {
			if !m.b327.mu.TryLock() {
				break
			}
			m.b327BuildLocked()
			m.b327.mu.Unlock()
		}
	}
	b327Stat.full.Add(1)
	return nil, 0, false
}

// pickListTry reads the list once (seqlock); ok=false: stale, torn, empty,
// all dead or a threshold beats the choice.
func (m *LinkManager) pickListTry(drawn string) (*managedLink, int32, bool) {
	s := &m.b327
	v := s.ver.Load()
	if v&1 != 0 || s.built.Load() != s.epoch.Load() {
		return nil, 0, false
	}
	n := int(s.n.Load())
	var chosen *managedLink
	var chosenFP int32
	var best pickKey
	ties := 0
	for i := 0; i < n && i < b327Max; i++ {
		ml := s.cand[i].Load()
		if ml == nil {
			continue
		}
		k, fp, ok := m.b327Key(ml, drawn)
		if !ok {
			continue
		}
		switch {
		case chosen == nil || k.less(best):
			chosen, chosenFP, best, ties = ml, fp, k, 1
		case !best.less(k):
			ties++
			if pickTie(ties) {
				chosen, chosenFP = ml, fp
			}
		}
	}
	b327Stat.scanned.Add(uint64(n))
	if chosen == nil {
		return nil, 0, false
	}
	for i, nt := 0, int(s.nThr.Load()); i < nt && i < b327Grp; i++ {
		t := &s.thr[i]
		tm := t.ml.Load()
		if tm == nil {
			continue
		}
		tk := pickKey{cutNew: t.cutNew.Load(), pressed: t.pressed.Load(), heavy: t.heavy.Load(), load: int(t.load.Load())}
		tk.path = m.steerRankLocked(tm, drawn)
		if tk.classLess(best) {
			return nil, 0, false
		}
		if !best.classLess(tk) && best.load > tk.load+b327Slack {
			return nil, 0, false
		}
	}
	if s.ver.Load() != v {
		return nil, 0, false
	}
	return chosen, chosenFP, true
}

// b327Sub orders two entries of one group: pickKey.less without the path
// rank (constant inside a group).
func b327Sub(a, b pickKey) bool {
	if a.cutNew != b.cutNew {
		return !a.cutNew
	}
	if a.pressed != b.pressed {
		return !a.pressed
	}
	if a.heavy != b.heavy {
		return !a.heavy
	}
	if a.load != b.load {
		return a.load < b.load
	}
	return a.users < b.users
}

// b327BuildLocked rebuilds the list. Caller holds m.mu (read or write) and
// m.b327.mu.
func (m *LinkManager) b327BuildLocked() {
	s := &m.b327
	b327Stat.build.Add(1)
	ep := s.epoch.Load()
	ng := 0
	usable := true
	ns := 0 // stripe-held links: always candidates (a stripe ending lock-free makes a link better)
	for _, ml := range m.links {
		if !usable {
			break
		}
		if !ml.aliveCheap() {
			continue
		}
		if !m.infoReadyLocked(ml) {
			usable = false // may become ready lock-free: a better link the list would not know
			break
		}
		k, _, ok := m.b327Key(ml, "")
		if !ok {
			continue
		}
		if stripeOn(ml) {
			if ns == len(s.striped) {
				usable = false
				break
			}
			s.striped[ns] = ml
			ns++
			continue
		}
		p, off := mlPath(ml), ml.steer.off
		g := -1
		for i := 0; i < ng; i++ {
			if s.groups[i].off == off && (off || s.groups[i].path == p) {
				g = i
				break
			}
		}
		if g < 0 {
			if ng == b327Grp {
				usable = false
				break
			}
			g = ng
			ng++
			s.groups[g].path, s.groups[g].off, s.groups[g].n = p, off, 0
		}
		gr := &s.groups[g]
		e := b327Ent{ml: ml, key: k, load: k.load}
		// insertion into the sorted top b327K+1
		j := gr.n
		if j == b327K+1 {
			if !b327Sub(k, gr.e[b327K].key) {
				continue
			}
			j = b327K
		} else {
			gr.n++
		}
		for j > 0 && b327Sub(k, gr.e[j-1].key) {
			gr.e[j] = gr.e[j-1]
			j--
		}
		gr.e[j] = e
	}
	s.ver.Add(1) // odd: readers back off
	n, nt := 0, 0
	if usable {
		for g := 0; g < ng; g++ {
			gr := &s.groups[g]
			for j := 0; j < gr.n && j < b327K; j++ {
				s.cand[n].Store(gr.e[j].ml)
				n++
			}
			if gr.n > b327K {
				t := &s.thr[nt]
				x := gr.e[b327K]
				t.ml.Store(x.ml)
				t.cutNew.Store(x.key.cutNew)
				t.pressed.Store(x.key.pressed)
				t.heavy.Store(x.key.heavy)
				t.load.Store(int64(x.load))
				nt++
			}
		}
	}
	if usable && n+ns > b327Max {
		usable = false
	}
	if usable {
		for i := 0; i < ns; i++ {
			s.cand[n].Store(s.striped[i])
			n++
		}
	}
	for i := 0; i < ns; i++ {
		s.striped[i] = nil
	}
	for i := n; i < b327Max; i++ {
		if s.cand[i].Load() == nil {
			break
		}
		s.cand[i].Store(nil)
	}
	for i := nt; i < b327Grp; i++ {
		s.thr[i].ml.Store(nil)
	}
	for g := 0; g < ng; g++ {
		gr := &s.groups[g]
		for j := range gr.e {
			gr.e[j] = b327Ent{}
		}
		gr.path = ""
	}
	s.n.Store(int32(n))
	s.nThr.Store(int32(nt))
	if usable && n > 0 {
		s.built.Store(ep)
	} else {
		s.built.Store(ep - 1) // never equal to this epoch: unusable until the next change
		s.failed.Store(ep + 1)
	}
	s.ver.Add(1)
}

// b327Line: " b327: list=N build=N stale=N full=N scan=N" once it ran,
// plus the pile counters once a busy second was seen.
func b327Line() string {
	l, b := b327Stat.list.Load(), b327Stat.build.Load()
	s := ""
	if l+b != 0 {
		s = fmt.Sprintf(" b327: list=%d build=%d stale=%d full=%d scan=%d%s", l, b, b327Stat.stale.Load(), b327Stat.full.Load(), b327Stat.scanned.Load(), b327Cfg)
	}
	if n := b327Pile.busy.Load(); n != 0 {
		s += fmt.Sprintf(" b327pile: busysec=%d maxmax=%d summax=%d max>=4=%d max>=8=%d", n, b327Pile.maxMax.Load(), b327Pile.sumMax.Load(), b327Pile.ge4.Load(), b327Pile.ge8.Load())
	}
	return s
}

// b327Pile (st327b, both arms, debug): the most user-connection placements
// (pickHeldFast) any single link took in one wall-clock second. A second
// with >= b327PileBusy placements logs one line
// "b327pile: t=UNIX picks=N links=N max=N list=BOOL" (storm seconds only).
const b327PileBusy = 40

var b327Pile struct {
	t                              atomic.Int64
	picks, links, max              atomic.Int32
	busy, maxMax, sumMax, ge4, ge8 atomic.Uint64
}

// b327PileNote counts one placement on ml in this second. No allocation;
// the per-second roll is racy at the edge (debug counter).
func b327PileNote(ml *managedLink) {
	now := time.Now().Unix()
	g := &b327Pile
	if t := g.t.Load(); t != now && g.t.CompareAndSwap(t, now) {
		p, l, mx := g.picks.Swap(0), g.links.Swap(0), g.max.Swap(0)
		if p >= b327PileBusy && t != 0 {
			g.busy.Add(1)
			g.sumMax.Add(uint64(mx))
			if mx >= 4 {
				g.ge4.Add(1)
			}
			if mx >= 8 {
				g.ge8.Add(1)
			}
			for {
				o := g.maxMax.Load()
				if uint64(mx) <= o || g.maxMax.CompareAndSwap(o, uint64(mx)) {
					break
				}
			}
			log.Printf("b327pile: t=%d picks=%d links=%d max=%d list=%v", t, p, l, mx, b327PickList.Load())
		}
	}
	if ml.b327SecT.Load() != now {
		ml.b327SecT.Store(now)
		ml.b327SecN.Store(0)
		g.links.Add(1)
	}
	c := ml.b327SecN.Add(1)
	g.picks.Add(1)
	for {
		o := g.max.Load()
		if c <= o || g.max.CompareAndSwap(o, c) {
			break
		}
	}
}
