package engine

import (
	"slices"
	"sync/atomic"
)

// l480d CALLPEND (HS2_L480D_CALLPEND, default off since lab3 z+w; no wire change).
// Lab l480d (lab3, l475/l476 fault: 1/4 of the links blackholed 5 s per
// window): call99 is set by 1-3 calls per run stuck ~8.5 s (the whole
// blackhole plus the dead link's own RTO), in rc15 and in l480 alike. The
// stuck call's subs went dark and every sub opened for it after that
// (callMoreSubsLocked's pair sub, the lite upgrade, the reopen below two
// subs, the call rescue and the records rescue join) was picked by load: a
// blackholed link carries nothing and looks the least loaded, so h5B's call
// fea1df3a opened ~35 fresh subs in 0.6 s and each was dark at once. These
// opens now carry NEWPICK's l480PendMark: pickPlaneCallSkip leaves out the
// pool's links silent while pending (a write unanswered and nothing read
// for l478SilMin; an idle link is never left out, and with no other link
// the pick is unchanged).
var l480dCallPendOn atomic.Bool

func init() {
	l480dCallPendOn.Store(envBoolDefault("HS2_L480", true) && envBoolDefault("HS2_L480D_CALLPEND", false)) // l480d: off (lab3 z+w: call99 +2888 n.s., st3 +2.0 worse)
}

// l480dCallPend: avoid with the l480PendMark (once) when the key is on.
func l480dCallPend(avoid []any) []any {
	if !l480dCallPendOn.Load() || slices.ContainsFunc(avoid, func(a any) bool { _, ok := a.(l480PendMark); return ok }) {
		return avoid
	}
	return append(slices.Clip(avoid), l480PendMark{})
}

// l480d PAIRSK (HS2_L480D_PAIRSK, on with HS2_L480; no wire change). WAN b484
// (28cfe8ac vs rc15, 8 ABBA pairs): calls stuck > 1 s +1.0 and > 3 s +1.6 per
// run, proven worse, with l480's nopair ~6300-8800 per run: while a STICK
// holds (the old dark primary, until its link reads again + l480Hold 3 s),
// callPairLocked returned at once and a call fell to the duplicating path
// (no callLate filter, no cs221 path split, no b262 dark-pair check). The
// pair now goes on with the stuck old sub left out of the candidates (two
// other subs suffice: both get the copy).
var l480dPairSk atomic.Bool

var l480dStat struct {
	pairSk atomic.Uint64 // call pairs placed while a stick held (old sub left out)
}

func init() {
	l480dPairSk.Store(envBoolDefault("HS2_L480", true) && envBoolDefault("HS2_L480D_PAIRSK", true))
}

// l480d CALLLIVE (HS2_L480D_CALLLIVE, default off since lab3 o; no wire
// change). lab3 p2D (PAIRSK on): call 22dacde117ee4cd stuck 8.4 s; after its
// subs 0/1 went dark it opened ~35 fresh subs in 1.7 s and every one was dark
// at once with rxidle ~2.8 s, i.e. on blackholed links: a blackholed link
// with nothing pending is idle, not "silent while pending", so CALLPEND's
// mark kept it, and the load-based pick likes idle links best. The lab's
// links all share one path, so callStuckMark{path} leaves no link out
// either. Call opens (pair sub, upgrade, reopen, call rescue, records rescue
// join, call stuck, call late) now carry LIVEPICK's l480LiveMark: the pick
// keeps only links that read within l480LiveFor (pool-wide; with none, the
// pick is unchanged).
var l480dCallLiveOn atomic.Bool

func init() {
	l480dCallLiveOn.Store(envBoolDefault("HS2_L480", true) && envBoolDefault("HS2_L480D_CALLLIVE", false)) // l480d: off (lab3 o: call99 +1878 n.s., no help)
}

// l480dCallLive: avoid with the l480LiveMark (once) when the key is on.
func l480dCallLive(avoid []any) []any {
	if l485CallOpen.Load() { // l485 CALLOPEN: every call open, whatever the l480 keys
		return l480WithLiveAlways(avoid)
	}
	if !l480dCallLiveOn.Load() || slices.ContainsFunc(avoid, func(a any) bool { _, ok := a.(l480LiveMark); return ok }) {
		return avoid
	}
	return l480WithLive(avoid)
}
