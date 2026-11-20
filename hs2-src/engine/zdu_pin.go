package engine

import (
	"fmt"
	"sync/atomic"
)

// ZDU building block B4 (proposals/zdu350.md §7 S3): the Iran generation pin.
//
// During a Kharej handover the Iran pool holds links of two peer processes
// (instances, zdu.go). A mconn connection lives in ONE of them: its exit-side
// state (the conn id, the reorder ring, the target socket) exists only in the
// process that took its first sub. So every sub of a connection must land on
// a link of that same peer instance:
//
//   - a new connection's first sub goes to the CURRENT instance (the newest
//     announced by any live link), so new users move to the new process as
//     soon as one of its links is up;
//   - once a sub sits on a link of a known instance the connection is pinned
//     to it, and every later sub (join, rescue join, RST join, call subs,
//     stripes, the role-6 end notice) is placed only on links of that
//     instance, until the connection ends (the old process drains it).
//
// The pin travels as a mark in the avoid list, the existing way an opener
// passes a placement rule down to the pickers, and is applied by avoided(),
// the per-link test every picker under pickPlaneCall / pickStripeWhy already
// runs. pickFirstLite's pickWait has no avoid list: a first sub it puts on
// another generation goes through pickPlaneCall with the mark instead. The
// three pick functions also check their result once more (zduPickOK): a link
// of another instance there means a picker missed the mark; it is counted
// (zdu_crossgen_reject, must stay 0) and refused, so a cross-generation sub
// never opens.
//
// No link of the pinned instance (the old process's links are gone): the pick
// fails exactly as an empty pool does today (open retries until its wait, then
// errMConnNoLink; a stripe or end notice gives up). A sub on another instance
// would reach a process that never saw the connection: the exit answers an
// unknown join with a refusal / RST, and a stripe would carry bytes no reorder
// ring expects. "No link" keeps the connection on the subs it has, which is
// what the drain needs; crossing would only turn a clean drain into a reset.
//
// HS2_ZDU off (or no link that announced capExtZDU): no mark is added, the
// avoid lists, the picks and the wire are exactly as before; the only cost on
// the pick hot path is one atomic bool load in avoided().

// zduPinMark in an avoid list: place the sub only on a link of inst.
type zduPinMark struct{ inst zduInst }

// zduStat: the pin counters (" zdu: ..." on the mem line once ZDU acted).
var zduStat struct {
	pinned    atomic.Int64 // connections pinned to an instance (first known sub)
	toCurrent atomic.Int64 // first-sub picks aimed at the current instance
	liteMoved atomic.Int64 // lite first subs moved off pickWait's other-instance link
	noLink    atomic.Int64 // pinned picks that found no link of their instance
	crossgen  atomic.Int64 // zdu_crossgen_reject: a picker missed the mark (must stay 0)
	held      atomic.Int64 // scans that held a new generation below its need (zdu_phase.go)
	mixNew    atomic.Int64 // mix-phase first subs sent to the new generation
	mixOld    atomic.Int64 // ... and to the older one
}

// zduLM is the LinkManager's view of the peer instances.
type zduLM struct {
	gen   atomic.Uint64 // bumped when a link's kindInfo carries an instance
	cache atomic.Pointer[zduCurCache]
	// zdu_counts.go, under LinkManager.mu: this pool's share of the gauges.
	peerOld, oldConns int64
	// zdu_exit.go: the draining old process's link count (foreign-drain term).
	foreign atomic.Pointer[zduForeign]
	// zdu_gate.go: the old process's handover boot term of ctlTarget.
	boost atomic.Int32
	// zdu_phase.go: when the mix phase began (monoNano, clock-step safe; 0: not mixing).
	mixAt atomic.Int64
	// zdu_phase.go (zdu389): the generation that reached full in this pool;
	// it stays current while it is the newest (no fall back to mix/hold).
	full atomic.Pointer[zduInst]
}

type zduCurCache struct {
	epoch, gen uint64
	inst       zduInst
	mix        *zduMix // non-nil: both generations take new conns (zdu_phase.go)
	until      int64   // mix: rescan after this (nanotime)
}

// linkZDUInst is the peer instance of a link: its own kindInfo answer's, or
// none (no answer yet, refused, or a peer without ZDU). The pool's answer
// (exitInfo) is never used: a link without its own answer has no known
// generation.
func linkZDUInst(l Link) zduInst {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return zduInst{}
	}
	if pi := mtr.peerInfo.Load(); pi.zdu() {
		return pi.Inst
	}
	return zduInst{}
}

// zduNoteInfo: a link's kindInfo answer arrived (stream_iran.go).
func (m *LinkManager) zduNoteInfo(pi *peerInfo) {
	if pi.zdu() {
		m.zdu.gen.Add(1)
		if zduOn.Load() { // the current instance may have changed: re-mark peerOld (zdu_counts.go)
			m.mu.Lock()
			m.zduRefreshLocked()
			m.mu.Unlock()
		}
	}
}

// zduCurrent is the newest instance among the live links (none while no link
// announced one). Cached until the pool changes (b327 epoch) or an answer
// arrives.
func (m *LinkManager) zduCurrent() zduInst {
	g := m.zdu.gen.Load()
	if g == 0 {
		return zduInst{}
	}
	ep := m.b327.epoch.Load()
	if c := m.zdu.cache.Load(); c != nil && c.epoch == ep && c.gen == g && c.fresh() {
		return c.inst
	}
	m.mu.RLock()
	cur := m.zduScanLocked(ep, g)
	m.mu.RUnlock()
	return cur
}

// zduCurrentLocked is zduCurrent for a caller holding m.mu (read or write).
func (m *LinkManager) zduCurrentLocked() zduInst {
	g := m.zdu.gen.Load()
	if g == 0 {
		return zduInst{}
	}
	ep := m.b327.epoch.Load()
	if c := m.zdu.cache.Load(); c != nil && c.epoch == ep && c.gen == g && c.fresh() {
		return c.inst
	}
	return m.zduScanLocked(ep, g)
}

// zduScanLocked finds the newest instance among the live links and caches it.
func (m *LinkManager) zduScanLocked(ep, g uint64) zduInst {
	c := m.zduPhaseLocked()
	c.epoch, c.gen = ep, g
	m.zdu.cache.Store(c)
	return c.inst
}

// zduTarget is the instance this connection's next sub must land on: its pin,
// or for a connection not pinned yet the current instance; none = no rule.
func (e *mconnEdge) zduTarget() zduInst {
	if p := e.zduPin.Load(); p != nil {
		return *p
	}
	if t := e.lm.zduCurrent(); t.known() {
		zduStat.toCurrent.Add(1)
		if mx := e.lm.zduMixNow(); mx != nil { // review issue 3: gradual shift (zdu_phase.go)
			return mx.pick()
		}
		return t
	}
	return zduInst{}
}

// zduAvoid adds the pin mark to an avoid list (a copy: the caller's backing
// array is never written). Key off: avoid itself, no allocation.
func (e *mconnEdge) zduAvoid(avoid []any) []any {
	if !zduOn.Load() {
		return avoid
	}
	t := e.zduTarget()
	if !t.known() {
		return avoid
	}
	return append(avoid[:len(avoid):len(avoid)], zduPinMark{inst: t})
}

// zduPinTo pins the connection to the instance of l, its sub's link, if the
// connection is not pinned yet and l's instance is known.
func (e *mconnEdge) zduPinTo(l Link) {
	if !zduOn.Load() || e.zduPin.Load() != nil {
		return
	}
	if in := linkZDUInst(l); in.known() {
		if e.zduPin.CompareAndSwap(nil, &in) {
			zduStat.pinned.Add(1)
		}
	}
}

// zduPinOf returns the pin mark of an avoid list.
func zduPinOf(avoid []any) (zduInst, bool) {
	for _, a := range avoid {
		if mk, ok := a.(zduPinMark); ok {
			return mk.inst, true
		}
	}
	return zduInst{}, false
}

// zduPinAvoids: avoid carries a pin and l is of another instance.
func zduPinAvoids(l Link, avoid []any) bool {
	in, ok := zduPinOf(avoid)
	return ok && linkZDUInst(l) != in
}

// zduPickOK is the pick functions' last check: false (counted, rel run) when
// a picker returned a link of another instance than the pin.
func zduPickOK(l Link, rel func(), avoid []any) bool {
	if !zduOn.Load() {
		return true
	}
	in, ok := zduPinOf(avoid)
	if !ok || linkZDUInst(l) == in {
		return true
	}
	zduStat.crossgen.Add(1)
	if rel != nil {
		rel()
	}
	return false
}

// zduNoLink counts a pinned pick that found nothing.
func zduNoLink(avoid []any) {
	if !zduOn.Load() {
		return
	}
	if _, ok := zduPinOf(avoid); ok {
		zduStat.noLink.Add(1)
	}
}

// zduPinLine: " zdu: ..." on the mem line once the pin acted.
func zduPinLine() string {
	p, c, x := zduStat.pinned.Load(), zduStat.toCurrent.Load(), zduStat.crossgen.Load()
	if p == 0 && c == 0 && x == 0 {
		return ""
	}
	return fmt.Sprintf(" zdu: pinned=%d to_current=%d lite_moved=%d pin_nolink=%d zdu_crossgen_reject=%d held=%d mix_new=%d mix_old=%d", p, c, zduStat.liteMoved.Load(), zduStat.noLink.Load(), x, zduStat.held.Load(), zduStat.mixNew.Load(), zduStat.mixOld.Load())
}
