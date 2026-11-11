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
	stripeFastReserve.Store(stripeFastReserveFromEnv(os.Getenv("HS2_MCONN_STRIPE_FAST_RESERVE")))
}

// stripeFastReserve: HS2_MCONN_STRIPE_FAST_RESERVE (up56, with
// HS2_MCONN_STRIPE_FAST; default 3, 0 turns it off, at most 6): the pool
// keeps this many idle links set aside for the stripe at all times, so an
// upload's first subs find links the moment it is hot instead of waiting
// ~1.5-2 s for the pool to build them (up55's lab: seconds 1-5 of a single
// upload 318 -> 336 Mbit/s only). The reserve comes ON TOP of the
// autopilot's target (stripeExtra, capped by max_links) and a set-aside link
// is heavy for every light pick (stripeAdoptLocked) - the light users keep
// exactly the pool they had; a reserved link a user or download reached
// anyway is not counted (and lapses as before). A sub that takes a reserved
// link asks the pool for its replacement right away (pickStripe).
var stripeFastReserve atomic.Int64

func stripeFastReserveFromEnv(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 3
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 3
	}
	return int64(min(n, 6))
}

// stripeReserveN: the standing reserve now (0: off).
func stripeReserveN() int {
	if !stripeFast.Load() {
		return 0
	}
	return int(stripeFastReserve.Load())
}

// stripeReserveIdle: ml (set aside) still counts toward the reserve.
func stripeReserveIdle(ml *managedLink) bool {
	return !ml.stripeRes.IsZero() && ml.link.Alive() && ml.heavy == 0 && ml.flowing == 0 && ml.users.Load() == 0
}

// stripeReserveLocked (stripeExtra, under m.mu, each health tick): keep the
// standing reserve - renew up to r idle set-aside links (they do not lapse)
// and ask for the missing ones.
func (m *LinkManager) stripeReserveLocked(now time.Time) {
	r := stripeReserveN()
	if r <= 0 {
		return
	}
	kept := 0
	for _, ml := range m.links {
		if kept < r && stripeReserveIdle(ml) {
			ml.stripeRes = now
			kept++
		}
	}
	stripeFastStat.reserved.Store(int64(kept))
	if kept < r {
		m.stripeWantRaiseLocked(now, r-kept)
	}
}

// stripeReserveRefillLocked (pickStripe, under m.mu, a sub took a set-aside
// link): ask the pool for its replacement now, not at the next health tick.
// One more per link taken - the subs the fast growth still wants beyond it
// are asked when a pick finds nothing (stripeAskLocked).
func (m *LinkManager) stripeReserveRefillLocked(now time.Time) {
	if stripeReserveN() <= 0 {
		return
	}
	m.stripeWantRaiseLocked(now, m.stripeWant+1)
	stripeFastStat.refills.Add(1)
}

// stripeWantRaiseLocked (under m.mu): the stripe wants w links (at least).
// With HS2_MCONN_STRIPE_FAST the pool's target rises by the new ones at
// once (up56): the target otherwise moves only at the next health tick
// (healthTick, 2 s) - the up56 lab log showed the reserve's replacements
// and the doubling chain's links set aside in batches exactly 2 s apart.
// The next tick's decideTarget recomputes the same sum (stripeExtra counts
// stripeWant), so nothing drifts.
func (m *LinkManager) stripeWantRaiseLocked(now time.Time, w int) {
	w = min(w, stripeWantCap())
	m.stripeWantUntil = now.Add(stripeWantFor)
	if w <= m.stripeWant {
		return
	}
	d := w - m.stripeWant
	m.stripeWant = w
	if !stripeFast.Load() || m.pin.Load() > 0 {
		return
	}
	if t := int(m.target.Load()); t < m.max {
		m.setTarget(min(t+d, m.max))
		stripeFastStat.bumps.Add(1)
	}
}

var stripeFastStat struct {
	retries atomic.Uint64 // fast opens tried again after no link
	chained atomic.Uint64 // subs opened by the doubling chain
	early   atomic.Uint64 // uploads that striped after one hot sample

	reserved atomic.Int64  // up56: idle links in the standing reserve (last health tick)
	refills  atomic.Uint64 // up56: reserved links taken and asked again at once
	hsKicks  atomic.Uint64 // up56: chains resumed the moment the upgrade's handshake ended
	bumps    atomic.Uint64 // up56: pool targets raised at once for the stripe
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
