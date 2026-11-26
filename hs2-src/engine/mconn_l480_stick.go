package engine

import (
	"fmt"
	"reflect"
	"slices"
	"sync/atomic"
	"time"
)

// l480 (default on since l480b; no wire change; HS2_L480=0 turns all off). HS2_L480 turns on all of
// HS2_L480_STICK, HS2_L480_LIVEPICK, HS2_L478_FASTOPEN and HS2_L477_TAILRES.
//
// Trace l478 x1B (lab3): after l478 moved a lite conn's dark primary s0 to the
// live sub s2, s0's queue still grew 200 -> 1100 -> 2000 B: a lite conn that
// is not in the one-copy path queues every chunk on EVERY sub that fits
// (sendLocked's duplicating path), so the dead link kept getting copies (and
// the bytes the live sub missed while full or cold rode only the dead one),
// and any later setPrimaryLocked whose prefPrimary was cleared fell back to
// m.subs[0], the dead sub again.
//
// STICK: after the move, while the old sub's link has read nothing since,
// (a) setPrimaryLocked never makes the old sub primary again (the live sub
// keeps it), and (b) the duplicating path skips the old sub (a miss, as for a
// full queue). The stick ends when the old link reads again or the new sub
// goes away.
//
// Trace l480 v1B (receive/send trace): with the fresh third sub a lite conn
// has 3 live subs and sendLocked takes callPairLocked (2 copies on a picked
// pair), which put the reply written after the move on the two dark subs
// s0 + s1 and never on the live s2: the peer had every byte up to the move
// (the tails were all duplicates) yet waited the whole blackhole for the
// next one. While a stick holds, the pair path is skipped (the duplicating
// path copies to every sub but the old dark one).
//
// LIVEPICK: b449/l478's dark open (Iran) picks its link only among links of
// the whole pool that read within l480LiveFor (pool-wide silence, not just
// this conn's subs: x1B's first fresh sub often landed on another
// blackholed link and a second open came 1.25 s later). When no link of the
// pool outside the avoid list is that fresh, the pick is unchanged.
var (
	l480Stick    atomic.Bool
	l480LivePick atomic.Bool
)

const l480LiveFor = 400 * 1e6 // ns

// l480Hold: the stick holds this long after the old link reads again.
var l480Hold = time.Duration(envIntDef("HS2_L480_HOLD_MS", 3000)) * time.Millisecond

func init() {
	all := envBoolDefault("HS2_L480", true) // l480b: default on (lab3 series c, 6 ABBA pairs)
	l480Stick.Store(all || envBoolDefault("HS2_L480_STICK", false))
	l480LivePick.Store(all || envBoolDefault("HS2_L480_LIVEPICK", false))
	if all {
		l478FastOpen.Store(true)
		l477TailRes.Store(true)
	}
}

var l480Stat struct {
	sticks  atomic.Uint64 // moves armed
	held    atomic.Uint64 // setPrimaryLocked kept the live sub (old one chosen again)
	ends    atomic.Uint64 // sticks ended (old link read again, or new sub gone)
	skipped atomic.Uint64 // copies not queued on the old sub
	picks   atomic.Uint64 // dark opens with the pool-wide live filter
	avoided atomic.Uint64 // silent links added to their avoid lists
	nolive  atomic.Uint64 // ... no live link left: the filter was dropped
	joins   atomic.Uint64 // early dark verdicts at a fresh sub's join (l480JoinSilent)
	nopair  atomic.Uint64 // sends kept off callPairLocked while a stick holds
}

func l480LogPart() string {
	return fmt.Sprintf(" · l480 stick %v livepick %v sticks %d held %d ends %d skipped %d picks %d avoided %d nolive %d joins %d nopair %d",
		l480Stick.Load(), l480LivePick.Load(), l480Stat.sticks.Load(), l480Stat.held.Load(), l480Stat.ends.Load(),
		l480Stat.skipped.Load(), l480Stat.picks.Load(), l480Stat.avoided.Load(), l480Stat.nolive.Load(), l480Stat.joins.Load(), l480Stat.nopair.Load()) +
		fmt.Sprintf(" · l480d pairsk %v %d callpend %v", l480dPairSk.Load(), l480dStat.pairSk.Load(), l480dCallPendOn.Load()) + l485LogPart()
}

// l480ArmLocked (l478MovePrimaryLocked, after the move to b): stick to b.
func (m *mConn) l480ArmLocked(s, b *msub) {
	if !l480Stick.Load() {
		return
	}
	var rd int64
	if mtr := dcMeterOf(s); mtr != nil {
		rd = mtr.lastRd.Load()
	}
	m.l480Old, m.l480New, m.l480Rd, m.l480ReAt = s, b, rd, 0
	m.l480TrN = 20
	l480Stat.sticks.Add(1)
}

// l480SkipSubLocked: the old dark primary while its stick holds, else nil.
func (m *mConn) l480SkipSubLocked() *msub {
	s := m.l480Old
	if s == nil {
		return nil
	}
	nw := m.l480New
	ok := l480Stick.Load() && !s.retired && !s.dead && nw != nil && !nw.retired && !nw.dead &&
		slices.Contains(m.subs, nw)
	if ok {
		if mtr := dcMeterOf(s); mtr == nil {
			ok = false // no meter: no evidence it is dead
		} else if mtr.lastRd.Load() != m.l480Rd {
			// its link reads again: hold l480Hold more (u1B: a link back from
			// a blackhole still drains ~35 KB of backlog at cwnd 1; a reply
			// paired onto it right away waited ~4 s)
			now := ctrlNow()
			if m.l480ReAt == 0 {
				m.l480ReAt = now
			}
			ok = time.Duration(now-m.l480ReAt) < l480Hold
		}
	}
	if !ok {
		m.l480Old, m.l480New, m.l480ReAt = nil, nil, 0
		l480Stat.ends.Add(1)
		m.tr.ev("l480: stick ended")
		return nil
	}
	l480Stat.skipped.Add(1)
	return s
}

// l480StickPrimaryLocked (end of setPrimaryLocked): never back to the old
// dark primary while the stick holds.
func (m *mConn) l480StickPrimaryLocked() {
	if m.l480Old == nil || m.primary.Load() != m.l480Old {
		return
	}
	if m.l480SkipSubLocked() == nil {
		return
	}
	nw := m.l480New
	if nw.bulk.Load() {
		return
	}
	m.prefPrimary = nw
	m.primary.Store(nw)
	l480Stat.held.Add(1)
	m.tr.ev("l480: kept live sub %d as primary (not dark sub %d)", nw.idx, m.l480Old.idx)
}

// l480LiveMark in an avoid list: pickPlaneCallSkip adds the pool's silent
// links to it (l480AvoidSilentLocked).
type l480LiveMark struct{}

// l480WithLive (b449DarkOpenLocked, fast opens): a copy of avoid with the mark.
func l480WithLive(avoid []any) []any {
	if !l480LivePick.Load() {
		return avoid
	}
	return append(slices.Clip(avoid), l480LiveMark{})
}

// l480TakeLive removes the mark; ok reports it was there.
func l480TakeLive(avoid []any) ([]any, bool) {
	i := slices.IndexFunc(avoid, func(a any) bool { _, ok := a.(l480LiveMark); return ok })
	if i < 0 {
		return avoid, false
	}
	out := make([]any, 0, len(avoid))
	out = append(out, avoid[:i]...)
	return append(out, avoid[i+1:]...), true
}

// l480LiveAvoid: avoid plus every alive pool link that read nothing within
// l480LiveFor, unless that leaves no live link outside avoid.
func (m *LinkManager) l480LiveAvoid(avoid []any) []any {
	avoid, ok := l480TakeLive(avoid)
	if !ok {
		return avoid
	}
	l480Stat.picks.Add(1)
	now := ctrlNow()
	skip := l480SkipSet(avoid)
	var add []any
	live := 0
	m.mu.RLock()
	for _, ml := range m.links {
		if ml.mtr == nil || !ml.aliveCheap() {
			continue
		}
		k := avoidKey(ml.link)
		if skip[k] {
			continue
		}
		rd := ml.mtr.lastRd.Load()
		if rd != 0 && now-rd < l480LiveFor {
			live++
		} else {
			add = append(add, k)
		}
	}
	m.mu.RUnlock()
	if live == 0 {
		l480Stat.nolive.Add(1)
		return avoid
	}
	l480Stat.avoided.Add(uint64(len(add)))
	return append(avoid, add...)
}

// l480JoinSilent (l477TailResLocked when a fresh sub joins): with STICK on,
// a sub whose link has been silent while pending for l478's bar
// (max(600 ms, 3 srtt)) counts as dark at the join, without waiting for
// l477's 1 s. Trace l480 x1B: the 9 s mode's conns got their fresh sub at
// 0.67..0.95 s of silence (b449 / l478), l477 skipped them at the join
// (< 1 s) and no later visit resent: the tail stayed on the dead link until
// its own RTO after the blackhole.
func l480JoinSilent(s *msub, nowc int64) (time.Duration, int64, bool) {
	if !l480Stick.Load() || !l478Silent(s, nowc) {
		return 0, 0, false
	}
	mtr := dcMeterOf(s)
	if mtr == nil {
		return 0, 0, false
	}
	rd, ps := mtr.lastRd.Load(), mtr.pendSince.Load()
	d := time.Duration(nowc - max(rd, ps))
	if d <= 0 {
		return 0, 0, false
	}
	l480Stat.joins.Add(1)
	return d, rd, true
}

// l480RxTraceLocked (deliver, under m.r.mu, trace on only): the receive side
// of a dark episode — a reassembly gap opening (a chunk past next), the
// chunk that fills it, and the first chunks of a fresh sub (idx >= 2).
func (m *mConn) l480RxTraceLocked(s *msub, off, end, next uint64, gap, inOrder bool) {
	switch {
	case !inOrder && !gap:
		m.tr.ev("l480 rx: gap opens at %d: sub %d brought [%d,%d)", next, s.idx, off, end)
	case inOrder && gap:
		m.tr.ev("l480 rx: sub %d [%d,%d) at next %d (gap was open; next now %d, ooo %d)", s.idx, off, end, next, m.r.next, len(m.r.ooo))
	case s.idx >= 2 && s.l480RxN < 30:
		s.l480RxN++
		m.tr.ev("l480 rx: sub %d [%d,%d) next %d in-order %v", s.idx, off, end, next, inOrder)
	}
}

// l480TxTraceLocked (sendLocked, trace on, the first sends after a move):
// where the chunk went (subs whose queue now ends with it) and the primary.
func (m *mConn) l480TxTraceLocked(c *mchunk, path string) {
	m.l480TrN--
	var on []int
	for _, s := range m.subs {
		if n := len(s.q); n > 0 && s.q[n-1] == c {
			on = append(on, s.idx)
		}
	}
	pi := -1
	if p := m.primary.Load(); p != nil {
		pi = p.idx
	}
	m.tr.ev("l480 tx: [%d,%d) %s on subs %v (primary %d, dupOff %v)", c.off, c.off+uint64(c.n), path, on, pi, m.dupOff)
}

// l480b NEWPICK (HS2_L480_NEWPICK, on with HS2_L480). Trace l480b p1B (lab3,
// probe 5/s on port 2090): the probes that fail during a blackhole are new
// conns whose first sub (pickWait) and hedge sub both landed on blackholed
// links: their mConn starts only when that link's backlog drains (16 probe
// conns started in the same second, ~4 s after the blackhole), or Kharej
// first hears of the conn after the probe's 5 s. A new conn now leaves out
// links that are silent while pending (a write unanswered and nothing read
// for l478SilMin): the first sub's pick takes another link (pickSkip) and
// the hedge sub's pick gets the l480PendMark. An idle link (nothing
// pending) is never left out; with no other link the pick is unchanged.
var l480NewPick atomic.Bool

func init() {
	l480NewPick.Store(envBoolDefault("HS2_L480", true) || envBoolDefault("HS2_L480_NEWPICK", false))
}

var l480NewStat struct {
	first   atomic.Uint64 // first subs moved off a silent link
	firstNo atomic.Uint64 // ... no other link found: kept
	hedge   atomic.Uint64 // hedge picks with the pending-silence filter
	avoided atomic.Uint64 // silent links added to hedge avoid lists
}

func l480NewLogPart() string {
	return fmt.Sprintf(" · l480b newpick %v first %d kept %d hedge %d avoided %d", l480NewPick.Load(),
		l480NewStat.first.Load(), l480NewStat.firstNo.Load(), l480NewStat.hedge.Load(), l480NewStat.avoided.Load())
}

// l480LinkSilent: the link has a write unanswered and read nothing for
// l478SilMin (pending silence, as l478Silent; an idle link is not silent).
func l480LinkSilent(mtr *linkMeter, nowc int64) bool {
	if mtr == nil {
		return false
	}
	rd, wr, ps := mtr.lastRd.Load(), mtr.lastWr.Load(), mtr.pendSince.Load()
	return rd != 0 && wr > rd && time.Duration(nowc-rd) >= l478SilMin && time.Duration(nowc-ps) >= l478SilMin
}

// l480PendMark in an avoid list: pickPlaneCallSkip adds the pool's links
// silent while pending (l480PendAvoid).
type l480PendMark struct{}

// l480WithPend (hedge open): a copy of avoid with the mark.
func l480WithPend(avoid []any) []any {
	if !l480NewPick.Load() {
		return avoid
	}
	return append(slices.Clip(avoid), l480PendMark{})
}

// l480PendAvoid: avoid plus every alive pool link silent while pending,
// unless that leaves no other alive link outside avoid.
func (m *LinkManager) l480PendAvoid(avoid []any) []any {
	i := slices.IndexFunc(avoid, func(a any) bool { _, ok := a.(l480PendMark); return ok })
	if i < 0 {
		return avoid
	}
	avoid = slices.Delete(slices.Clone(avoid), i, i+1)
	l480NewStat.hedge.Add(1)
	nowc := ctrlNow()
	skip := l480SkipSet(avoid)
	var add []any
	other := 0
	m.mu.RLock()
	for _, ml := range m.links {
		if !ml.aliveCheap() {
			continue
		}
		k := avoidKey(ml.link)
		if skip[k] {
			continue
		}
		if l480LinkSilent(ml.mtr, nowc) {
			add = append(add, k)
		} else {
			other++
		}
	}
	m.mu.RUnlock()
	if other == 0 || len(add) == 0 {
		return avoid
	}
	l480NewStat.avoided.Add(uint64(len(add)))
	return append(avoid, add...)
}

// l480FirstPick (pickFirstLite, after pickWaitSkip): a first sub picked on
// a link silent while pending moves to another link. l480c: one pick that
// leaves out every alive link silent while pending at once (l480b's 3
// pickSkip tries kept the silent link 65 times in c2B: the load-based pick
// kept returning another silent, lightly loaded link); with no other link
// (or the pick still silent), the first pick is kept.
func (e *mconnEdge) l480FirstPick(l Link, rel func()) (Link, func()) {
	if !l480NewPick.Load() || !l480LinkSilent(linkMeterOf(l), ctrlNow()) {
		return l, rel
	}
	skip := append(slices.Clip(e.failed), l)
	skip = append(skip, e.lm.l480SilentLinks(skip)...)
	if ol, orel, ok := e.lm.pickSkip(skip); ok {
		if !l480LinkSilent(linkMeterOf(ol), ctrlNow()) {
			rel()
			l480NewStat.first.Add(1)
			return ol, orel
		}
		orel()
	}
	l480NewStat.firstNo.Add(1)
	return l, rel
}

// l480SilentLinks: the alive pool links silent while pending, not in skip.
func (m *LinkManager) l480SilentLinks(skip []Link) []Link {
	nowc := ctrlNow()
	var out []Link
	m.mu.RLock()
	for _, ml := range m.links {
		if ml.aliveCheap() && !slices.Contains(skip, ml.link) && l480LinkSilent(ml.mtr, nowc) {
			out = append(out, ml.link)
		}
	}
	m.mu.RUnlock()
	return out
}

// l480SkipSet: the hashable entries of an avoid list as a set. l480d: an
// avoid list may hold marks with a map inside (rescueJoinMark from rrStart):
// hashing one panics (lab3 y2D, CALLPEND on the records rescue join); those
// entries are marks, never link keys, so they are left out.
func l480SkipSet(avoid []any) map[any]bool {
	skip := make(map[any]bool, len(avoid))
	for _, a := range avoid {
		if a != nil && reflect.ValueOf(a).Comparable() {
			skip[a] = true
		}
	}
	return skip
}
