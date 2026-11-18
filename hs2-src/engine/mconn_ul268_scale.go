package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// ul268 at scale (review-rel69k findings 1, 2, 7).
//
//  1. A data-dark-only tick no longer sweeps every Iran conn: an index
//     (link key -> Iran subs on it, kept by addSubLocked / removeSubLocked /
//     msub.close) gives the conns that have a sub on a dark link, and only
//     those are visited. A link must be judged dark on 2 consecutive watch
//     ticks (ul268DarkTicks), and a link whose visit started a rescue is not
//     visited again for ul268Hold (it stays in the join avoid set). The full
//     sweep still runs when an er92-silent link is in the set (rare), and
//     rescueStat.trigger counts er92 episodes only.
//  2. The cover check runs at the decision against the raw sub link's real
//     outstanding bytes (SIOCOUTQ: unacked + unsent of the link socket, an
//     upper bound for this conn's share) plus ul268CovMargin (bytes the peer
//     acked but has not read yet); the rate guess is used only when the
//     queue cannot be read. A budget refusal is cleared by a later growth or
//     when the ring reaches its cap, and by release().
//  3. ul268's dark-only rescue and darkcall's lite switch exclude each other
//     per conn for ul268Excl: whichever acted first owns the episode.

const (
	ul268DarkTicks = 2
	ul268CovMargin = 64 << 10
)

var (
	ul268Hold  = time.Duration(envIntDef("HS2_UL268_HOLD_MS", 2000)) * time.Millisecond
	ul268Excl  = 1500 * time.Millisecond
	ul268OutqF = func(s *msub) (uint64, bool) { // tests replace it
		if tc := subTCP(s); tc != nil {
			return tcpOutq(tc)
		}
		return 0, false
	}
)

var ul268Scale struct {
	visits  atomic.Uint64 // targeted dark-only passes (ticks)
	conns   atomic.Uint64 // conns visited by them
	held    atomic.Uint64 // dark links skipped: hold-off after a rescue
	sweepUs atomic.Int64  // longest targeted pass, µs
	sumUs   atomic.Uint64 // all targeted passes, µs
	pre     atomic.Uint64 // indexed conns skipped without the lock (no recent bytes)
	full    atomic.Uint64 // full sweeps (an er92-silent link in the set)
	outqNo  atomic.Uint64 // cover decisions without a readable outq
	dcSkip  atomic.Uint64 // ul268 rescues skipped: darkcall switched the conn
	dcYield atomic.Uint64 // darkcall lite visits skipped: ul268 rescued the conn
}

func ul268ScaleLogLine() string {
	s := &ul268Scale
	return "ul268 scale: visits " + u64s(s.visits.Load()) + " conns " + u64s(s.conns.Load()) + " held " + u64s(s.held.Load()) +
		" pre " + u64s(s.pre.Load()) + " maxus " + u64s(uint64(max(0, s.sweepUs.Load()))) + " sumus " + u64s(s.sumUs.Load()) + " full " + u64s(s.full.Load()) + " outqno " + u64s(s.outqNo.Load()) +
		" dcskip " + u64s(s.dcSkip.Load()) + " dcyield " + u64s(s.dcYield.Load())
}

// ---- the index ---------------------------------------------------------------

var ul268Idx struct {
	mu sync.Mutex
	m  map[any]map[*msub]struct{}
}

// ul268IdxAdd (under m.mu): s of an Iran conn joins its link's entry.
func ul268IdxAdd(s *msub) {
	if s.key == nil || s.u268Idx.Swap(true) {
		return
	}
	ul268Idx.mu.Lock()
	if ul268Idx.m == nil {
		ul268Idx.m = map[any]map[*msub]struct{}{}
	}
	e := ul268Idx.m[s.key]
	if e == nil {
		e = map[*msub]struct{}{}
		ul268Idx.m[s.key] = e
	}
	e[s] = struct{}{}
	ul268Idx.mu.Unlock()
}

// ul268IdxDel: s leaves the index (idempotent).
func ul268IdxDel(s *msub) {
	if !s.u268Idx.Swap(false) {
		return
	}
	ul268Idx.mu.Lock()
	if e := ul268Idx.m[s.key]; e != nil {
		delete(e, s)
		if len(e) == 0 {
			delete(ul268Idx.m, s.key)
		}
	}
	ul268Idx.mu.Unlock()
}

func ul268IdxLen() (links, subs int) {
	ul268Idx.mu.Lock()
	defer ul268Idx.mu.Unlock()
	for _, e := range ul268Idx.m {
		links++
		subs += len(e)
	}
	return
}

// ul268ConnsOn: the conns with a sub on one of keys (each once), and per
// conn the dark keys it has a sub on.
func ul268ConnsOn(keys []any) ([]*mConn, map[*mConn][]any) {
	ul268Idx.mu.Lock()
	defer ul268Idx.mu.Unlock()
	var out []*mConn
	by := map[*mConn][]any{}
	for _, k := range keys {
		for s := range ul268Idx.m[k] {
			if s.m == nil {
				continue
			}
			if _, ok := by[s.m]; !ok {
				out = append(out, s.m)
			}
			by[s.m] = append(by[s.m], k)
		}
	}
	return out, by
}

// ul268HoldUntil: per dark link, no visit before (monoNano). rescueWatch
// goroutine only (rescueSilentLinks).
var ul268HoldUntil = map[any]int64{}

// ---- the cover check ---------------------------------------------------------

// ul268CoverNowLocked (Iran, under mu): may a data-dark rescue of this raw
// conn start now? The ring must hold every byte ever written, or at least
// the raw link socket's outstanding bytes plus ul268CovMargin.
func (m *mConn) ul268CoverNowLocked() bool {
	r := &m.ring
	held := r.uHeld.Load()
	if held <= 0 {
		return false
	}
	if held >= r.uTotA.Load() {
		return true
	}
	if q, ok := ul268OutqF(m.raw1); ok {
		return held >= int64(q)+ul268CovMargin
	}
	ul268Scale.outqNo.Add(1)
	return r.uCov.Load()
}

// ---- the targeted pass -------------------------------------------------------

// ul268VisitDark (rescueWatch): a tick whose rescue set holds data-dark-only
// links and no er92-silent one: visit only the conns with a sub on them.
func ul268VisitDark(set, callSet, only map[any]bool, now time.Time) {
	t0 := time.Now()
	nowN := monoNano(now)
	for k, u := range ul268HoldUntil {
		if u <= nowN || !only[k] {
			delete(ul268HoldUntil, k)
		}
	}
	keys := make([]any, 0, len(only))
	for k := range only {
		if ul268HoldUntil[k] > nowN {
			ul268Scale.held.Add(1)
			continue
		}
		keys = append(keys, k)
	}
	ul268Scale.visits.Add(1)
	if len(keys) == 0 {
		return
	}
	conns, by := ul268ConnsOn(keys)
	ul268Scale.conns.Add(uint64(len(conns)))
	for _, m := range conns {
		if !m.iran {
			continue
		}
		if !m.ul268Recent(nowN) {
			ul268Scale.pre.Add(1) // neither ring wrote within rescueKeep: no rescue is eligible
			continue
		}
		started, stop := false, false
		if _, ok := rescueConns.Load(m); ok {
			started, stop = rescueVisitRaw(m, now, set, callSet)
		}
		if !started && !stop {
			if _, ok := rrConns.Load(m); ok {
				started, stop = rescueVisitRec(m, now, set, callSet)
			}
		}
		if started {
			for _, k := range by[m] {
				ul268HoldUntil[k] = nowN + int64(ul268Hold)
			}
		}
		if stop {
			break
		}
	}
	us := time.Since(t0).Microseconds()
	ul268Scale.sumUs.Add(uint64(max(0, us)))
	if us > ul268Scale.sweepUs.Load() {
		ul268Scale.sweepUs.Store(us)
	}
}

// ul268DcRecentLocked (under mu): darkcall switched this conn within
// ul268Excl (ul268 then leaves it: no double resend).
func (m *mConn) ul268DcRecentLocked(now time.Time) bool {
	return !m.dcSwAt.IsZero() && now.Sub(m.dcSwAt) < ul268Excl
}

// ul268RecentLocked (under mu): ul268 rescued this conn within ul268Excl
// (darkcall's lite visit then leaves it).
func (m *mConn) ul268RecentLocked(now time.Time) bool {
	return !m.u268At.IsZero() && now.Sub(m.u268At) < ul268Excl
}

// ul268Recent (no lock): a raw or records ring of m took bytes within
// rescueKeep, the precondition of rescueEligibleLocked / rrEligibleLocked.
func (m *mConn) ul268Recent(nowN int64) bool {
	for _, l := range [2]int64{m.ring.last.Load(), m.rk.last.Load()} {
		if l != 0 && nowN-l <= int64(rescueKeep) {
			return true
		}
	}
	return false
}
