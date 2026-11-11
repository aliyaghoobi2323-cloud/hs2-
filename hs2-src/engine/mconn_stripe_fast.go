package engine

import (
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_FAST=1 (Iran, the edge that opens stripe subs; with
// HS2_MCONN_STRIPE): a heavy UPLOAD widens its stripe fast.
//
// Without it a single upload sat on its first sub for stripeHotTicks (4)
// samples, then grew by one sub a second, with mReopenBackoff (2 s) after
// every failed open: the up54 lab's median per second over 55 single
// uploads was 213 140 182 178 164 191 261 284 310 286 Mbit/s — the stripe
// reached 4-5 subs only when a 10 s upload was nearly over. With the key:
//
//   - the first stripe sub of an upload comes after one hot sample (a
//     download keeps stripeHotTicks: a page's burst must not take links);
//   - while every sub already carries stripeFastRate (HS2_MCONN_STRIPE_FAST_MBIT,
//     default 30 Mbit/s each) the growth doubles the width (n → 2n, capped
//     by the stripe's K and HS2_MCONN_STRIPE_UP's cap): the extra subs are
//     opened one after the other right after the first succeeds
//     (stFastMore), each again through the same picker — the share and
//     keep-free rules decide every link as before, so a light user's link is
//     never taken; when the picker finds nothing the chain stops;
//   - HS2_MCONN_STRIPE_UP's queue gate still decides: no growth while the
//     upload's path queue says no (stripeUpRoomLocked), as before;
//   - a failed stripe open waits stripeFastBackoff (250 ms + up to 250 ms
//     jitter) instead of 2 s.
//
// Off (the default before up55): nothing changes.
var stripeFast atomic.Bool

// stripeFastRate: bytes/s per sub for the doubling growth.
var stripeFastRate atomic.Int64

func init() {
	stripeFast.Store(os.Getenv("HS2_MCONN_STRIPE_FAST") == "1") // loadEnvKeys reads it again with the envdef table
	stripeFastRate.Store(30e6 / 8)
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_FAST_MBIT")), 64); err == nil && v > 0 {
		stripeFastRate.Store(int64(v * 1e6 / 8))
	}
}

const (
	stripeFastBackoff    = 250 * time.Millisecond
	stripeFastBackoffJit = 250 * time.Millisecond
)

// stripeFastOn: the fast rule applies to this growth (an upload).
func stripeFastOn(up bool) bool { return up && stripeFast.Load() }

// stripeRetryLocked: when the next stripe open may be tried after a failed one.
func (m *mConn) stripeRetryLocked(now time.Time) time.Time {
	if m.stGrowUp && stripeFast.Load() {
		m.stFastMore = 0
		return now.Add(stripeFastBackoff + rand.N(stripeFastBackoffJit))
	}
	return now.Add(mReopenBackoff)
}

// stripeFastPlanLocked (in stripeTickLocked, a growth n → n+1 decided): how
// many more subs to open right after this one (0: none).
func (m *mConn) stripeFastPlanLocked(n int, rate float64, up bool) int {
	if !stripeFastOn(up) || rate < float64(n)*float64(stripeFastRate.Load()) {
		return 0
	}
	more := max(n-1, 1) // n → 2n: this open plus n-1 more (the first growth: 1 → 3)
	for more > 0 && !m.stripeKCapLocked(n+1+more-1, rate) {
		more--
	}
	return more
}

// stripeFastNextLocked (openStripe, a sub just joined): open the next one of
// the chain now. true: stOpening was set, the caller starts openStripe.
func (m *mConn) stripeFastNextLocked() bool {
	if m.stFastMore <= 0 {
		return false
	}
	m.stFastMore--
	if m.ended || m.closing || m.peerFin || m.wclosed || m.stOpening || m.callOn || m.callUp {
		m.stFastMore = 0
		return false
	}
	n := 1 + m.liveStripeLocked()
	if !m.stripeKCapLocked(n, m.stFastRate) {
		m.stFastMore = 0
		return false
	}
	if m.stUp && n >= 2 && !m.stripeUpRoomLocked() {
		m.stFastMore = 0
		return false
	}
	m.stOpening = true
	stripeFastStat.chained.Add(1)
	return true
}

// stripeWantHint (in a stripe open's avoid list, never equal to a link): the
// subs the fast growth still opens — the pool is asked for that many links at
// once (pickStripe → stripeAskLocked) instead of one a pick.
type stripeWantHint struct{ n int }

func stripeWantOf(avoid []any) int {
	for _, a := range avoid {
		if h, ok := a.(stripeWantHint); ok {
			return h.n
		}
	}
	return 0
}

// stripeFastTries: a fast open that found no link retries this many times
// (stripeFastBackoff + jitter apart, ~2 s in all) in its own goroutine, with
// stOpening held: the pool's link set aside for it is taken within a few
// hundred ms instead of at the next growth tick.
const stripeFastTries = 6

// stripeFastRetry (openStripe, no mu): wait and try again (true).
func (m *mConn) stripeFastRetry(try int) bool {
	if try >= stripeFastTries {
		return false
	}
	m.mu.Lock()
	ok := m.stGrowUp && stripeFast.Load() && !m.ended && !m.closing && !m.peerFin && !m.wclosed && !m.callOn && !m.callUp
	m.mu.Unlock()
	if !ok {
		return false
	}
	time.Sleep(stripeFastBackoff + rand.N(stripeFastBackoffJit))
	stripeFastStat.retries.Add(1)
	return true
}

// stripeFastRawRetry (openStripe, a raw first sub, no mu): the upgrade
// failed (still raw, nothing in flight); wait and try again (true) with
// lightOpening set as rawTick sets it.
func (m *mConn) stripeFastRawRetry(try int) bool {
	m.mu.Lock()
	still := m.rawW.Load() && !m.lightOpening
	m.mu.Unlock()
	if !still || !m.stripeFastRetry(try) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended || m.closing || !m.rawW.Load() || m.lightOpening {
		return false
	}
	m.lightOpening = true
	return true
}

var stripeFastStat struct {
	retries atomic.Uint64 // fast opens tried again after no link
	chained atomic.Uint64 // subs opened by the doubling chain
	early   atomic.Uint64 // uploads that striped after one hot sample
}

// stripeUpSubWin: HS2_STRIPE_UP_SUB_WIN_MB (the exit, 0/off by default, 3-8):
// the fixed smux window the exit gives an upload's stripe sub (the exit
// receives the upload). up55's lab (rel53 production keys, 74 ms, crowd
// 3000): every upload stripe sub's link sat app-limited (ss -ti: rwnd_limited
// ~1%, ~1300-1500 segments ≈ 2 MiB unacked) — bound by the exit's smux
// window, which the adaptive rule hardly grew (aw_kh: peak 2 MiB, 1-2 grows
// a run: the upload's own queue on the link fails the no-queue probe), so a
// sub carried at most ~220 Mbit/s (2 MiB / 75 ms) and less as the RTT rose.
// A stripe sub rides a link no user rides (stripeAlone) and Iran's upload
// gate (HS2_MCONN_STRIPE_UP) still cuts the width when the path queues, so a
// larger window there does not sit ahead of a light user. Mirrors Iran's
// HS2_STRIPE_SUB_WIN_MB for the download.
var stripeUpSubWin = stripeSubWinFromEnv(os.Getenv("HS2_STRIPE_UP_SUB_WIN_MB"))

// stripeUpSubWinN: exit stripe subs given HS2_STRIPE_UP_SUB_WIN_MB.
var stripeUpSubWinN atomic.Int64

// stripeUpSubWinLocked (the exit, under m.mu): s just joined the stripe.
func (m *mConn) stripeUpSubWinLocked(s *msub) {
	if stripeUpSubWin == 0 || m.iran || smuxAutoWin == nil {
		return
	}
	var st any = s.rw
	if ms, ok := s.rw.(*mconnSub); ok {
		st = ms.stream
	}
	o, ok := st.(interface {
		SetAutoWindowOff()
		SetOffWindow(int, int)
	})
	if !ok {
		return
	}
	o.SetAutoWindowOff()              // the budget's growth back, then
	o.SetOffWindow(stripeUpSubWin, 0) // the fixed window (its UPD goes out with the next read)
	stripeUpSubWinN.Add(1)
}
