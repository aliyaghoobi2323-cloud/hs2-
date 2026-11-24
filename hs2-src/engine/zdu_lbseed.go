package engine

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// b438: the LB judge's verdicts ride the ZDU hand-off (HS2_ZDU_LBSEED, on by
// default). A new process used to start with an empty fast judge: for its
// first 10–16 s every path read "no reading yet", factor 1, and the edge
// followed that report (lab b437: the cut path's steer target 3 -> 27, ~25
// links dialled onto it, then re-marked). Now the old process sends its
// per-path state (handoff FeatLBState, LBSTATE after FDS and again right
// before GO) and the new one seeds its judge from it:
//
//   - only paths the old judge had a reading for, only when the snapshot is
//     at most lbSeedMaxAge old (else today's behaviour);
//   - the seeded state is the old judge's, so fresh readings fold into it
//     exactly as they would have in the old process (cuts, recoveries, the
//     smoothing go on, nothing waits for the seed);
//   - a seeded path the new judge has not read within lbSeedTTL: a cut one
//     stays this judge's own cut (it comes back by the judge's no-reading
//     probe rule and the ramp, as in the old process; lab b438 run 2: a
//     reset to "no reading yet" put the cut path from 0.10 to 1.00 in 10 s),
//     any other falls back to "no reading yet" (today's start state);
//   - the GO refresh only touches paths still on inherited state: a path the
//     new judge has read keeps its own verdict.

var (
	lbSeedOn     = envBoolDefault("HS2_ZDU_LBSEED", true)
	lbSeedMaxAge = 30 * time.Second
	lbSeedTTL    = 30 * time.Second
	lbSeedMaxLen = 32 << 10
)

// ZDULBSeedOn reports HS2_ZDU_LBSEED (the hand-off FeatLBState bit).
func ZDULBSeedOn() bool { return lbSeedOn }

type lbSeedPath struct {
	N       string  `json:"n"`
	M       float64 `json:"m"` // applied factor (b.mult)
	T       float64 `json:"t"` // target
	R       string  `json:"r,omitempty"`
	E       float64 `json:"e"` // smoothed loss
	Q       float64 `json:"q,omitempty"`
	F       float64 `json:"f,omitempty"`
	Raw     float64 `json:"raw,omitempty"`
	Bad     int     `json:"bad,omitempty"`
	Cut     bool    `json:"cut,omitempty"`
	Why     string  `json:"why,omitempty"`
	CutAgo  int64   `json:"ca,omitempty"` // ms before At
	OkAgo   int64   `json:"oa"`           // ms before At, -1 none
	ReadAgo int64   `json:"ra"`
	UncAgo  int64   `json:"ua"`          // -1 none
	Hold    int64   `json:"h,omitempty"` // ms
	// b443: a path in its dial cooldown (the old judge had dropped it): the
	// cooldown left (ms after At) and its failed dials. The new LB cools it
	// for the rest, at the old factor M; its judge state rides only for the
	// stash (Cut/Why/E): the judge starts reading it again after the cooldown,
	// as the old process would have.
	Cool  int64 `json:"cl,omitempty"`
	Fails int   `json:"fl,omitempty"`
}

type lbSeedLB struct {
	K string       `json:"k"` // the LB's path names, sorted, joined
	P []lbSeedPath `json:"p"`
}

type lbSeedDoc struct {
	At int64      `json:"at"` // unix ns
	L  []lbSeedLB `json:"l"`
}

var lbSeedReg struct {
	mu      sync.Mutex
	lbs     []*LB
	pending *lbSeedDoc
	// counters (status / tests)
	seeded, refreshed, expired, stale int
}

func (b *LB) seedKey() string {
	var ns []string
	for _, p := range b.paths {
		ns = append(ns, p.Name())
	}
	sort.Strings(ns)
	return strings.Join(ns, ",")
}

// ZDULBRegister records a dialing-side LB (its state rides a later hand-off)
// and seeds it from a hand-off LBSTATE already received.
func ZDULBRegister(b *LB) {
	if b == nil {
		return
	}
	lbSeedReg.mu.Lock()
	lbSeedReg.lbs = append(lbSeedReg.lbs, b)
	doc := lbSeedReg.pending
	lbSeedReg.mu.Unlock()
	if doc != nil && lbSeedOn {
		b.applySeed(doc, time.Now(), false) // too old by now: today's behaviour
	}
}

func msAgo(now, t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return max(now.Sub(t).Milliseconds(), 0)
}

func agoT(now time.Time, ms int64) time.Time {
	if ms < 0 {
		return time.Time{}
	}
	return now.Add(-time.Duration(ms) * time.Millisecond)
}

// ZDULBStateExport is this process's LB verdicts for the hand-off (nil: none).
func ZDULBStateExport() []byte {
	lbSeedReg.mu.Lock()
	lbs := append([]*LB(nil), lbSeedReg.lbs...)
	lbSeedReg.mu.Unlock()
	now := time.Now()
	doc := lbSeedDoc{At: now.UnixNano()}
	for _, b := range lbs {
		if s, ok := b.exportSeed(now); ok {
			doc.L = append(doc.L, s)
		}
	}
	if len(doc.L) == 0 {
		return nil
	}
	out, err := json.Marshal(doc)
	if err != nil || len(out) > lbSeedMaxLen {
		return nil
	}
	return out
}

func (b *LB) exportSeed(now time.Time) (lbSeedLB, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.auto2On() || b.judge == nil {
		return lbSeedLB{}, false
	}
	out := lbSeedLB{K: b.seedKey()}
	now = b.now() // the judge's clock (ages are what travels)
	for j, p := range b.paths {
		q := b.judge.p[p.Name()]
		if cl := b.coolExportLocked(j, now); cl != nil { // b443: a cooled path (dropped by the judge)
			out.P = append(out.P, *cl)
			continue
		}
		if q == nil || !q.have || q.seeded {
			continue // only the old judge's own readings (a seed is not re-handed)
		}
		out.P = append(out.P, lbSeedPath{N: p.Name(), M: b.mult[j], T: q.target, R: q.reason, E: q.ewma, Q: q.queueMs,
			F: q.fail, Raw: q.raw, Bad: q.bad, Cut: q.cut, Why: q.cutWhy, CutAgo: max(msAgo(now, q.cutAt), 0),
			OkAgo: msAgo(now, q.okSince), ReadAgo: max(msAgo(now, q.lastRead), 0), UncAgo: msAgo(now, q.uncutAt),
			Hold: q.hold.Milliseconds()})
	}
	return out, len(out.P) > 0
}

// ZDULBStateSeed takes the old process's LBSTATE (refresh: the one sent at
// GO). It seeds every registered LB and keeps the state for LBs built later.
// Returns how many paths were seeded.
func ZDULBStateSeed(blob []byte, refresh bool) int {
	if !lbSeedOn || len(blob) == 0 || len(blob) > lbSeedMaxLen {
		return 0
	}
	var doc lbSeedDoc
	if json.Unmarshal(blob, &doc) != nil || doc.At == 0 {
		return 0
	}
	now := time.Now()
	lbSeedReg.mu.Lock()
	if age := now.Sub(time.Unix(0, doc.At)); age > lbSeedMaxAge || age < -lbSeedMaxAge {
		lbSeedReg.stale++
		lbSeedReg.mu.Unlock()
		return 0
	}
	lbSeedReg.pending = &doc
	lbs := append([]*LB(nil), lbSeedReg.lbs...)
	lbSeedReg.mu.Unlock()
	n := 0
	for _, b := range lbs {
		n += b.applySeed(&doc, now, refresh)
	}
	return n
}

// applySeed seeds b's judge from doc (taken at doc.At). refresh: only paths
// still on inherited state or never read.
func (b *LB) applySeed(doc *lbSeedDoc, now time.Time, refresh bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.auto2On() {
		return 0
	}
	age := now.Sub(time.Unix(0, doc.At)) // wall clock, both processes on this host
	if age > lbSeedMaxAge || age < -lbSeedMaxAge {
		return 0
	}
	now = b.now()               // the judge's clock from here on
	at := now.Add(-max(age, 0)) // the snapshot, on that clock
	key := b.seedKey()
	var s *lbSeedLB
	for i := range doc.L {
		if doc.L[i].K == key {
			s = &doc.L[i]
		}
	}
	if s == nil {
		return 0
	}
	if b.judge == nil {
		ln, rn := b.localN, b.remN
		if ln == "" {
			ln, rn = "local", "remote"
		}
		b.judge = newQ2Judge(lb2Floor, ln, rn, b.log)
		b.judge.cutOut = b.l3Cut
	}
	idx := map[string]int{}
	for j, p := range b.paths {
		if p.Weight > 0 {
			idx[p.Name()] = j
		}
	}
	n := 0
	var line []string
	for _, sp := range s.P {
		j, ok := idx[sp.N]
		if !ok || math.IsNaN(sp.M) || math.IsNaN(sp.E) {
			continue
		}
		if sp.Cool > 0 { // b443: a cooled path
			if b.coolSeedLocked(j, sp, now, at) {
				n++
				line = append(line, sp.N+" "+ftoa2(b.mult[j])+" (cooled "+fmtDur(b.until[j].Sub(now))+" more)")
			}
			continue
		}
		if q := b.judge.p[sp.N]; q != nil && q.have && !q.seeded {
			continue // the new judge's own reading wins
		}
		q := &q2Path{
			ewma: sp.E, queueMs: sp.Q, fail: sp.F, raw: sp.Raw, have: true, bad: sp.Bad,
			cut: sp.Cut, cutWhy: sp.Why, cutAt: agoT(at, sp.CutAgo), okSince: agoT(at, sp.OkAgo),
			lastRead: agoT(at, sp.ReadAgo), uncutAt: agoT(at, sp.UncAgo), hold: time.Duration(sp.Hold) * time.Millisecond,
			target: math.Min(math.Max(sp.T, lb2Floor), 1), reason: sp.R, factor: 1,
			seeded: true, seedUntil: now.Add(lbSeedTTL),
		}
		if !sp.Cut {
			q.cutAt = time.Time{}
		}
		b.judge.p[sp.N] = q
		b.mult[j] = math.Min(math.Max(sp.M, lb2Floor), lbMultMax)
		b.qual[j].loss, b.qual[j].queueMs, b.qual[j].have = q.ewma, q.queueMs, true
		n++
		if sp.M < 0.98 || sp.Cut {
			line = append(line, sp.N+" "+ftoa2(b.mult[j])+" ("+sp.R+")")
		}
	}
	if n > 0 {
		b.judgeAt = now
		lbSeedReg.mu.Lock()
		if refresh {
			lbSeedReg.refreshed += n
		} else {
			lbSeedReg.seeded += n
		}
		lbSeedReg.mu.Unlock()
		what := "seeded"
		if refresh {
			what = "refreshed at GO"
		}
		b.log("lb auto2: zdu: judge %s from the old process (%d path(s), state %s old): %s", what, n,
			fmtDur(now.Sub(at)), strings.Join(append(line, ""), " ")+"others ok")
	}
	return n
}

// seedAgeLocked runs after each judge update: a seeded path the new judge
// read is its own from now on; one still unread at its TTL stays as it is
// when cut, or when the quiet-probe rule lifted its inherited cut in the TTL
// (it is on the step-by-step way back; lab b442 run 13: a silent cut lifted
// at +12 s was reset to "no reading yet" at the TTL, 0.10 -> 1.00 in 15 s,
// re-cut and 8 links re-marked), else falls back to "no reading yet" (today's
// start state).
func (b *LB) seedAgeLocked(now time.Time) {
	if b.judge == nil {
		return
	}
	for k, q := range b.judge.p {
		if !q.seeded {
			continue
		}
		if q.lastRead.After(q.seedUntil.Add(-lbSeedTTL)) {
			q.seeded = false // a fresh reading folded in
			continue
		}
		if now.After(q.seedUntil) {
			lbSeedReg.mu.Lock()
			lbSeedReg.expired++
			lbSeedReg.mu.Unlock()
			if q.cut || q.uncutAt.After(q.seedUntil.Add(-lbSeedTTL)) {
				q.seeded = false
				b.log("lb auto2: zdu: inherited cut of %s unread for %s: kept (it comes back by the no-reading probe rule)", k, fmtDur(lbSeedTTL))
				continue
			}
			b.judge.p[k] = &q2Path{factor: 1, target: 1, reason: "no reading yet"}
			b.log("lb auto2: zdu: inherited state of %s expired unread after %s: judged afresh", k, fmtDur(lbSeedTTL))
		}
	}
}

// ZDULBSeedStats: paths seeded at start, refreshed at GO, expired unread,
// stale snapshots dropped.
func ZDULBSeedStats() (seeded, refreshed, expired, stale int) {
	lbSeedReg.mu.Lock()
	defer lbSeedReg.mu.Unlock()
	return lbSeedReg.seeded, lbSeedReg.refreshed, lbSeedReg.expired, lbSeedReg.stale
}

// ---- b443: paths in their dial cooldown ----------------------------------------
//
// Lab r443 op5: the lossy path was in its 2 m dial cooldown at a Kharej
// hand-off. adapt2 drops an ineligible path from the judge (forget(keep)), so
// the old process's LBSTATE left it out; the new LB started it uncooled at
// factor 1 ("no reading yet"), dialled it at once and the edge followed that
// report (steer target 3 -> 27 for ~12 s). Now the old process stashes the
// judge state of a path it drops while cooled, LBSTATE carries the path with
// its factor, cut state and the cooldown left, and the new LB cools it for
// the rest of that cooldown at the old factor: when it ends, the path comes
// back exactly as it would have in the old process (one probe dial, then the
// judge reads it afresh from the inherited factor).

// coolStashLocked keeps the judge state of every path adapt2 is about to drop
// while it is cooled, and forgets the stash of a path that is eligible again
// (its judge entry is then the live one). Caller holds b.mu.
//
// A path cooled from LBSTATE that comes back before the inherited cooldown
// ends (a dial the new process had in flight before the seed arrived
// succeeded: lab b443 op8, the cut path ramped 0.10 -> 1.00 in 15 s) gets the
// inherited judge state back as an unread seed (the b438 rules: a fresh
// reading folds in, an unread cut is kept at the TTL).
func (b *LB) coolStashLocked(keep map[string]bool, now time.Time) {
	if !lbSeedOn || b.judge == nil {
		return
	}
	for j, p := range b.paths {
		k := p.Name()
		if keep[k] || j >= len(b.until) || b.until[j].IsZero() {
			if st := b.judgeCool[k]; st != nil && keep[k] {
				if q := b.judge.p[k]; st.seeded && now.Before(st.seedUntil) && (q == nil || !q.have) {
					r := *st
					r.seedUntil = now.Add(lbSeedTTL)
					b.judge.p[k] = &r
					b.log("lb auto2: zdu: %s back before the inherited cooldown ended: its inherited state (%s) is restored", k, r.reason)
				}
				delete(b.judgeCool, k)
			}
			continue
		}
		if q := b.judge.p[k]; q != nil && q.have {
			if b.judgeCool == nil {
				b.judgeCool = map[string]*q2Path{}
			}
			c := *q
			b.judgeCool[k] = &c
		}
	}
}

// coolExportLocked is path j's LBSTATE entry while it is cooled (nil: not
// cooled). now is the judge's clock. Caller holds b.mu.
func (b *LB) coolExportLocked(j int, now time.Time) *lbSeedPath {
	if j >= len(b.until) || b.until[j].IsZero() || b.paths[j].Gone {
		return nil
	}
	left := b.until[j].Sub(now).Milliseconds()
	if left < 1 {
		left = 1 // over: the next dial is its probe (the new LB probes it first too)
	}
	sp := &lbSeedPath{N: b.paths[j].Name(), M: b.mult[j], T: b.mult[j], R: "cooled", OkAgo: -1, UncAgo: -1, Cool: left, Fails: b.fails[j]}
	q := b.judgeCool[sp.N]
	if q == nil && b.judge != nil {
		// b448 op4: cooled less than one adapt2 round before the hand-off, so
		// coolStashLocked has not stashed it yet and its live judge entry still
		// holds the verdict (cut). Without this the entry went out as a bare
		// "cooled" and the early-back restore ramped the cut path 0.10 -> 1.00.
		if p := b.judge.p[sp.N]; p != nil && p.have {
			q = p
		}
	}
	if q != nil {
		sp.T, sp.R, sp.E, sp.Q, sp.F, sp.Raw, sp.Bad = q.target, q.reason, q.ewma, q.queueMs, q.fail, q.raw, q.bad
		sp.Cut, sp.Why = q.cut, q.cutWhy
		if q.cut {
			sp.CutAgo = max(msAgo(now, q.cutAt), 0)
		}
		// b449: the recovery clocks too (as a judged entry carries them). Without
		// them the state restored on an early back had no last reading and no
		// hold, so the judge's no-reading probe rule lifted the inherited cut
		// at its first round and the path ramped 0.10 -> 1.00 (lab b448 op4).
		sp.OkAgo, sp.ReadAgo, sp.UncAgo = msAgo(now, q.okSince), max(msAgo(now, q.lastRead), 0), msAgo(now, q.uncutAt)
		sp.Hold = q.hold.Milliseconds()
	}
	return sp
}

// coolSeedLocked cools path j for what is left of the old process's cooldown
// (taken at "at", on the judge's clock) at the old factor, and stashes the
// old judge state so a later hand-off passes it on. A path the new LB already
// holds links on, or whose judge has read it, keeps its own state. Caller
// holds b.mu.
func (b *LB) coolSeedLocked(j int, sp lbSeedPath, now, at time.Time) bool {
	k := sp.N
	if q := b.judge.p[k]; q != nil && q.have && !q.seeded {
		return false // the new judge's own reading wins
	}
	if j >= len(b.until) || b.links[j] > 0 && b.until[j].IsZero() {
		return false // dialled and up here already: its own state
	}
	until := at.Add(time.Duration(sp.Cool) * time.Millisecond)
	if !until.After(now) {
		until = now // the cooldown is over: its next dial is the probe
	}
	b.until[j] = until
	b.fails[j] = max(b.fails[j], sp.Fails)
	b.mult[j] = math.Min(math.Max(sp.M, lb2Floor), lbMultMax)
	delete(b.judge.p, k) // cooled: not judged until it is back (adapt2 forgets it anyway)
	if b.judgeCool == nil {
		b.judgeCool = map[string]*q2Path{}
	}
	st := &q2Path{ewma: sp.E, queueMs: sp.Q, fail: sp.F, raw: sp.Raw, have: true, bad: sp.Bad,
		cut: sp.Cut, cutWhy: sp.Why, cutAt: agoT(at, sp.CutAgo), target: math.Min(math.Max(sp.T, lb2Floor), 1),
		reason: sp.R, factor: 1, okSince: agoT(at, sp.OkAgo), uncutAt: agoT(at, sp.UncAgo),
		lastRead: agoT(at, sp.ReadAgo), hold: time.Duration(sp.Hold) * time.Millisecond,
		seeded: true, seedUntil: until} // seeded: from LBSTATE (restored if it is back before until)
	if sp.Cut && st.hold < lb2Hold {
		// b449: an older sender's cooled entry has no hold (and "ra" 0 reads as
		// last read at the snapshot): the least a cut is ever held.
		st.hold = lb2Hold
	}
	b.judgeCool[k] = st
	return true
}
