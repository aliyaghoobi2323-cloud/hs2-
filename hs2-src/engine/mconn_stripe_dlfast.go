package engine

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_DL_FAST=1 (dr116, Iran = the edge, the side that decides
// every stripe growth; on in the production table (envdef, Edge) since
// dr116, HS2_MCONN_STRIPE_DL_FAST=0 turns it off; legacy defaults: off): a single download stripes early and
// widens by doubling, as HS2_MCONN_STRIPE_FAST does for an upload.
//
// Why: ab114 (d1, prod-like dl79 lab) — a single download upgraded raw →
// stripe only ~7.5 s after it started (the first sample at 1 s, then
// stripeHotTicks=4 hot 1 s samples, then the raw handshake drained a raw
// queue grown for seconds) and grew ~0.9 subs/s; the stripe phase ran ~2× the
// raw phase, so most of a 12 s download ran on one sub.
//
// With the key:
//   - a raw connection's first stripeDlWindow (3 s) is sampled every
//     stripeDlStep (200 ms) by a download sampler of its own (the growth
//     sample stSample keeps its 1 s windows); stripeDlFastHot (2) samples in
//     a row where it received at stripeDlFastRate (HS2_MCONN_STRIPE_DL_FAST_MBIT,
//     default 80 Mbit/s) or more upgrade it at once — 400 ms of 80 Mbit/s is
//     not a page's or a reel's burst, and one that is gets one stripe sub;
//   - past that a download needs stripeDlFastHot hot 1 s ticks at
//     stripeDlFastRate instead of stripeHotTicks (any rate over 5 Mbit/s);
//   - while every sub carries stripeFastRate (30 Mbit/s) a growth doubles the
//     width (stFastMore chain, capped by K): each extra sub is picked by the
//     same picker (share / keep-free rules; a light user's link never taken),
//     and every 1 s tick still passes the queue gate (a sub unused → no growth);
//   - solo: only stripeDlFastMax (HS2_MCONN_STRIPE_DL_FAST_MAX, 1) connection(s)
//     at a time use the fast rule for a download (stripeDlActive); with
//     eight parallel downloads the others grow the usual way (the upload
//     lesson of rel54b, u8 crowd p99).
//
// No wire change. Off: nothing changes.
var stripeDlFast atomic.Bool

// stripeDlFastRate: bytes/s a download must carry for the fast rule.
var stripeDlFastRate atomic.Int64

// stripeDlFastMax: downloads that may use the fast rule at the same time.
var stripeDlFastMax atomic.Int32

// stripeDlActive (process-wide): connections whose last growth was a fast
// download one (mConn.stDlCnt).
var stripeDlActive atomic.Int32

const stripeDlFastHot = 2

// stripeDlStat: diagnostics.
var stripeDlStat struct {
	early, ticks, chained, soloHeld atomic.Uint64
}

func init() {
	stripeDlFast.Store(os.Getenv("HS2_MCONN_STRIPE_DL_FAST") == "1")
	stripeDlFastRate.Store(80e6 / 8)
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_DL_FAST_MBIT")), 64); err == nil && v > 0 {
		stripeDlFastRate.Store(int64(v * 1e6 / 8))
	}
	stripeDlFastMax.Store(1)
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_DL_FAST_MAX"))); err == nil && n > 0 {
		stripeDlFastMax.Store(int32(min(n, 16)))
	}
}

// stripeDlSoloLocked: this connection may use the download fast rule now.
func (m *mConn) stripeDlSoloLocked() bool {
	if !stripeDlFast.Load() || stripeK.Load() == 0 {
		return false
	}
	n := stripeDlActive.Load()
	if m.stDlCnt {
		n--
	}
	if n >= stripeDlFastMax.Load() {
		stripeDlStat.soloHeld.Add(1)
		return false
	}
	return true
}

// stripeDlCountLocked: keep stripeDlActive in step with this connection.
func (m *mConn) stripeDlCountLocked(on bool) {
	if on == m.stDlCnt {
		return
	}
	m.stDlCnt = on
	if on {
		stripeDlActive.Add(1)
	} else {
		stripeDlActive.Add(-1)
	}
}

// stripeDlWindow: a raw connection's age up to which the download sampler
// runs (past it the 1 s ticks and stripeDlHotOKLocked decide).
const stripeDlWindow = 3 * time.Second

// stripeDlStep: the download sampler's period.
const stripeDlStep = 200 * time.Millisecond

// stripeDlOnLocked: the download sampler applies to this connection now.
func (m *mConn) stripeDlOnLocked(now time.Time) bool {
	return stripeDlFast.Load() && m.stK >= 2 && m.stN == 0 && m.rawW.Load() && !m.rawBorn.IsZero() && now.Sub(m.rawBorn) < stripeDlWindow
}

// stripeDlSampleLocked (every raw tick, under m.mu): the download sampler,
// apart from the growth sample (stSample). true with the rate: the
// stripeDlFastHot-th stripeDlStep sample in a row where the connection
// received at stripeDlFastRate or more (and more than it sent).
func (m *mConn) stripeDlSampleLocked(now time.Time, sent, recvd uint64) (bool, float64) {
	if !m.stripeDlOnLocked(now) {
		m.stDlHot = 0
		return false, 0
	}
	if m.stDlT.IsZero() {
		m.stDlT, m.stDlS, m.stDlR = now, sent, recvd
		return false, 0
	}
	w := now.Sub(m.stDlT)
	if w < stripeDlStep*9/10 {
		return false, 0
	}
	got, put := recvd-m.stDlR, sent-m.stDlS
	m.stDlT, m.stDlS, m.stDlR = now, sent, recvd
	rate := float64(got) / w.Seconds()
	if put >= got || !m.stripeDlEarlyHotLocked(rate) {
		if put >= got {
			m.stDlHot = 0
		}
		return false, 0
	}
	return true, rate
}

// stripeDlNextLocked (rawTick): the next raw tick — every stripeDlStep while
// the download sampler runs.
func (m *mConn) stripeDlNextLocked(now time.Time, d time.Duration) time.Duration {
	if m.stripeDlOnLocked(now) && !m.stripeDlBusyLocked() {
		return min(d, stripeDlStep)
	}
	return d
}

// stripeDlBusyLocked: another download holds the fast rule (no point
// sampling fast).
func (m *mConn) stripeDlBusyLocked() bool {
	n := stripeDlActive.Load()
	if m.stDlCnt {
		n--
	}
	return n >= stripeDlFastMax.Load()
}

// stripeDlEarlyHotLocked (a download sample): true when the sample is the
// stripeDlFastHot-th in a row at stripeDlFastRate.
func (m *mConn) stripeDlEarlyHotLocked(rate float64) bool {
	if rate < float64(stripeDlFastRate.Load()) || !m.stripeDlSoloLocked() {
		if rate < float64(stripeDlFastRate.Load()) {
			m.trLocked(trDlCold)
		} else {
			m.trLocked(trDlSolo)
		}
		m.stDlHot = 0
		return false
	}
	m.stDlHot++
	if m.stDlHot >= stripeDlFastHot {
		stripeDlStat.early.Add(1)
		return true
	}
	return false
}

// stripeDlHotOKLocked (a raw download's 1 s tick, stHot already counted):
// the hot-tick gate is passed with the fast rule.
func (m *mConn) stripeDlHotOKLocked(up bool, rate float64) bool {
	if up || m.stHot < stripeDlFastHot || rate < float64(stripeDlFastRate.Load()) || !m.stripeDlSoloLocked() {
		return false
	}
	stripeDlStat.ticks.Add(1)
	return true
}

// stripeDlPlanLocked (a growth n → n+1 of a download decided): the extra
// subs of the doubling chain (0: none); marks the connection fast.
func (m *mConn) stripeDlPlanLocked(n int, rate float64, up bool) int {
	if up || m.stripeDlFairSkipLocked(n) || !m.stripeDlSoloLocked() {
		return 0
	}
	if !m.stDlCnt {
		m.stDlAt = time.Now() // HS2_MCONN_STRIPE_DL_FAIR (mconn_stripe_dlfair.go)
	}
	m.stripeDlCountLocked(true)
	if rate < float64(n)*float64(stripeFastRate.Load()) {
		return 0
	}
	more := max(n-1, 1)
	for more > 0 && !m.stripeKCapLocked(n+1+more-1, rate) {
		more--
	}
	return more
}

// stripeEarlyDl: the early window a download uses when the upload's is off.
func stripeEarlyDl() time.Duration {
	if stripeDlFast.Load() && stripeK.Load() > 0 {
		return 200 * time.Millisecond
	}
	return 0
}

// stripeEarlyHotAnyLocked (an early sample): an upload by the up57 rule
// (only with its own early window on); a download never (its own sampler).
func (m *mConn) stripeEarlyHotAnyLocked(up bool, rate float64) bool {
	if up {
		return stripeEarlyOn() > 0 && stripeEarlyHotLocked(up, rate)
	}
	return false // a download: the dr116 sampler (stripeDlSampleLocked)
}

// stripeChainHintLocked: the chain's want hint goes to the pool.
func (m *mConn) stripeChainHintLocked() bool {
	if m.stGrowUp {
		return m.stripeFastOnLocked(true)
	}
	return m.stDlCnt && stripeDlFast.Load()
}

// stripeChainSoloLocked (the chain's next sub): the solo rule of the
// chain's direction.
func (m *mConn) stripeChainSoloLocked() bool {
	if !m.stGrowUp && m.stDlCnt {
		return m.stripeDlSoloLocked()
	}
	return m.stripeFastSoloLocked() // as before dr116
}

// stripeFastRetryOnLocked: a failed stripe open retries within
// stripeFastBackoff (the pool's set-aside link, asked by the failed pick,
// comes in a few hundred ms) — an upload with the fast rule, or (dr116b) a
// download that took the download fast rule: lab d1L1 decided at 1.6 s, the
// first pick found no set-aside link and the 2 s mReopenBackoff put the
// first stripe sub at 4.4 s.
func (m *mConn) stripeFastRetryOnLocked() bool {
	if m.stGrowUp {
		return m.stripeFastOnLocked(true)
	}
	return m.stDlCnt && stripeDlFast.Load()
}
