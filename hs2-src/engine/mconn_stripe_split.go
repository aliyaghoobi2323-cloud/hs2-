package engine

import (
	"slices"
	"time"
)

// gate74: Iran's upload gate with the grow and cut decisions split
// (HS2_MCONN_STRIPE_UP_SPLIT, Iran default on; with HS2_MCONN_STRIPE_UP and _REL).
//
// prod73 showed the production gate cutting ~150 times a run and never
// growing: the healthy path queue (3-7 ms) sat over the 2 ms base cap. A
// 10 ms cap with a median of 3 samples for BOTH decisions doubled one upload
// (343 -> 626 Mbit/s) but delayed the cut too, and the light users paid
// (echo p99 +31 ms). Split:
//   - GROW reads the median of the last stripeUpSplitMed excesses over the
//     learned base (the path's lower decile) capped at stripeUpGrowBaseMax
//     (10 ms): the path's own standing noise does not hold growth;
//   - CUT reads the latest sample alone, over the base capped at
//     stripeUpCutBaseMax (10 ms), against stripeUpCutPct (67) % of the
//     target — stricter than the grow side's band, so a queue spike still
//     cuts at once.
//
// Lab (prod73's production-like lab: 4 paths, netem normal jitter 4 ms,
// 1-2 % loss on two, crowd 3000 on 40 links, calls, game, one upload; both
// labs, interleaved): the cut on the strict 2 ms base (CUT_BASE_MAX=2)
// changed nothing (cuts 107/run, upload = rel58); with 10 ms at 67 % the
// upload rose and the light users did not lose (CHANGELOG gate74).
var (
	stripeUpSplit       bool // loadEnvKeys: HS2_MCONN_STRIPE_UP_SPLIT
	stripeUpGrowBaseMax = envMsDefault("HS2_MCONN_STRIPE_UP_GROW_BASE_MAX", 10*time.Millisecond)
	stripeUpCutBaseMax  = envMsDefault("HS2_MCONN_STRIPE_UP_CUT_BASE_MAX", 10*time.Millisecond)
	stripeUpCutPct      = min(100, envIntDefault("HS2_MCONN_STRIPE_UP_CUT_PCT", 67))
)

// stripeUpSplitMed: samples (stripeQEvery each) the grow decision's median takes.
const stripeUpSplitMed = 3

// stripeUpSplitLocked (sender, under m.mu): from the raw path queue, the
// grow sample (median excess over the grow base), the cut sample (latest
// excess over the cut base) and the grow base (diagnostics).
func (m *mConn) stripeUpSplitLocked(pq *stripeQ, qRaw time.Duration) (qGrow, qCut, growBase time.Duration) {
	p10 := pq.p10Base()
	growBase = min(p10, stripeUpGrowBaseMax)
	qCut = max(0, qRaw-min(p10, stripeUpCutBaseMax))
	qGrow = m.stripeUpMedNLocked(max(0, qRaw-growBase), stripeUpSplitMed)
	return qGrow, qCut, growBase
}

// stripeUpMedNLocked (under m.mu): the median of q and the n-1 before it.
func (m *mConn) stripeUpMedNLocked(q time.Duration, n int) time.Duration {
	n = min(n, stripeUpMedMax)
	if n <= 1 {
		return q
	}
	m.stQh[m.stQn%n] = q
	m.stQn++
	k := min(m.stQn, n)
	var s [stripeUpMedMax]time.Duration
	copy(s[:k], m.stQh[:k])
	slices.Sort(s[:k])
	return s[k/2]
}

// p10Base: the path queue's uncapped lower decile (0 until learned).
func (p *stripeQ) p10Base() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.p10
}
