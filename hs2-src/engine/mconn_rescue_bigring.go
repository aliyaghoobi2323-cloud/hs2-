package engine

import "time"

// ---- mb174: the records rings' big budget (rrBigMax) -----------------------
//
// cd152 grew a records ring to rrRateSecs seconds of its conn's write rate
// (up to rrRingMax) from rrBigMax, first come first served, and a ring never
// gave its growth back while the conn wrote anything within rescueKeep. In
// production (rel66b, ~4k conns) the budget sat at its cap: rings grown by a
// burst (a page, an image, a video segment) kept 0.5-1 MB on keep-alive
// conns that trickled, and a call starting later stayed at 32 KB (a records
// rescue of the call then NAKs and the call ends, the cd152 failure).
//
// Now:
//   - shrink: a ring whose rate (the last full window, or the running one
//     when longer) wants at most half its size for rrRateSecs seconds in a
//     row (the sweeper, every second) shrinks to what the rate wants (at
//     least rrRingCap), keeping its newest bytes; the bytes above go back to
//     rrBigMax. What the conn wrote in the last rrRateSecs seconds still fits
//     — the coverage cd152 sized the ring for;
//   - tiers: growth past rrRingCap is taken from rrBigMax only while the
//     budget's use stays under the conn's tier — a call (callOn, or
//     call-like within callFreeTTL) all of it, a
//     sustained writer (rrSusWins full windows in a row needing more than
//     rrRingCap) rrBigSusPct %, any other (a burst) rrBigBurstPct %. So the
//     budget's last part is kept for calls and steady streams, and bursts
//     give theirs back within seconds.

const (
	rrSusWins     = 3
	rrBigBurstPct = 40
	rrBigSusPct   = 60
)

// noteSustained (a window just rolled): count the windows in a row that need
// a big ring.
func (r *rescueRing) noteSustained() {
	if r.rate > 0 && rrCapFor(r.rate) > rrRingCap {
		r.susN++
	} else {
		r.susN = 0
	}
}

// bigTake: n more big bytes for r within its tier (rrBigTake's rule).
func (r *rescueRing) bigTake(n int) bool {
	lim := rrBigMax
	switch {
	case r.call:
	case r.susN >= rrSusWins:
		lim = rrBigMax * rrBigSusPct / 100
	default:
		lim = rrBigMax * rrBigBurstPct / 100
	}
	if rrStat.big.Add(int64(n)) > lim {
		rrStat.big.Add(-int64(n))
		rrStat.bigNo.Add(1)
		return false
	}
	if r.call {
		r.bigC += n
		rrStat.bigCall.Add(int64(n))
	}
	return true
}

// rateNow: the ring's write rate as of now (bytes/s): the last full window
// while it is recent, the running window when it is already longer.
func (r *rescueRing) rateNow(now int64) int {
	if r.winT0 == 0 {
		return 0
	}
	d := now - r.winT0
	rate := 0
	if d < 2*int64(time.Second) {
		rate = r.rate
	}
	if d > 0 {
		rate = max(rate, int(int64(r.winN)*int64(time.Second)/max(d, int64(time.Second))))
	}
	return rate
}

// shrinkIdle (sweeper, under rkMu, no rescue waiting): give big capacity
// back after rrRateSecs seconds of a rate that wants at most half the ring.
func (r *rescueRing) shrinkIdle(now int64) {
	if !r.rec || r.big == 0 || r.buf == nil {
		r.lowT0 = 0
		return
	}
	want := max(rrCapFor(r.rateNow(now)), len(r.buf)-r.big)
	if want > len(r.buf)/2 {
		r.lowT0 = 0
		return
	}
	if r.lowT0 == 0 {
		r.lowT0 = now
		return
	}
	if now-r.lowT0 < int64(max(rrRateSecs, 1))*int64(time.Second) {
		return
	}
	size := (want + 4<<10 - 1) &^ (4<<10 - 1)
	size = max(size, len(r.buf)-r.big)
	if size >= len(r.buf) {
		return
	}
	nb := make([]byte, size)
	if r.n > size {
		// keep the newest size bytes
		all := make([]byte, r.n)
		r.copyOut(all)
		copy(nb, all[r.n-size:])
		r.n = size
	} else {
		r.copyOut(nb)
	}
	gone := len(r.buf) - size
	rpPut(r.buf) // gc296
	r.buf, r.head = nb, 0
	r.big -= gone
	rrStat.big.Add(-int64(gone))
	if c := min(r.bigC, gone); c > 0 {
		r.bigC -= c
		rrStat.bigCall.Add(-int64(c))
	}
	r.lowT0 = 0
	r.susN = 0
	rrStat.shrunk.Add(1)
	rrStat.shrunkKB.Add(uint64(gone >> 10))
}
