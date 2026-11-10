package engine

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_UP=1 (with HS2_MCONN_STRIPE=K on both servers): the stripe
// for an upload — one heavy connection's Iran → exit direction spread over
// several links — with the same rules as the download's stripe, in the other
// direction. Everything of mconn_stripe.go is reused as it is: the same subs
// (plane planeStripe, opened by Iran), the same STRIPE record, the same send
// rule (stripeSendLocked: each chunk once, on the stripe sub with the least
// queued), the same links (pickStripe: only links no user rides) and the same
// growth rule (stripeTickLocked already samples both directions). What the
// upload lacked, and this key adds:
//
//   - the scavenger on Iran's side (mconn_stripe_scav.go): Iran is the
//     upload's sender, so it gates its own width by the path's queue as the
//     OTHER users' links see it in the upload direction — the median
//     srtt − minrtt (TCP_INFO of Iran's link sockets) of Iran's busy links
//     that carry none of this connection's subs (stripeUpPathQ; the exit
//     reads its own sockets for the download). Without it Iran put every
//     chunk on every stripe sub at once (width -1): K links' worth of upload
//     queue in front of every game and call packet going up.
//   - growth by the upload's own use: the rule "every stripe sub carried data
//     in the last sample" (stripeAllUsedLocked) looked only at what Iran
//     received; for an upload it looks at what Iran queued on the sub, so an
//     upload grows past its first stripe sub, and stops where the gate leaves
//     a sub unused.
//   - the exit's reorder bound: a striping exit receiver had only mRecvBuf
//     (4 MiB) of out-of-order room. With the key on the exit takes a stripe
//     slot the first time an upload's reorder reaches that bound (lazily: a
//     download-only stripe never takes one), raising it to stripeReorder.
//   - capability: capExtMConnStripeUp, ext bit 2 of kindInfo (bit 0 is the
//     stripe, bit 1 is taken by the UDP fast path's capExtUDPFast on another
//     branch), announced only with both keys on. Iran gates its upload only
//     when the exit announced it; against an older exit (or the key off on
//     either side) the bytes on the wire and the behaviour are those of
//     HS2_MCONN_STRIPE alone.
//
// Two limits sized for one 2 Gbit/s user (both directions, key on only):
//
//   - links: a configured K (HS2_MCONN_STRIPE, at most stripeKMax) stays the
//     cap while the subs carry little; past it the stripe grows on, up to
//     stripeKMaxUp, only while each sub already carries stripeSubRateHi on
//     average (the regime where one link gives 50-150 Mbit/s under 1% loss
//     and 80 ms and 2 Gbit/s needs 14-40 of them). The scavenger and the
//     pool's free-link rules apply to every one of them as before.
//   - reorder: a receiver's out-of-order bound grows on demand, in
//     stripeReorderStep steps, from stripeReorder up to stripeReorderConnMax
//     for one connection, while the process's extra (above stripeReorder per
//     slot) stays under stripeReorderExtraMax. The bound is only room: memory
//     is used only while one link lags the others (2 Gbit/s × 80 ms is
//     ~20 MB; a retransmission timeout on one sub more). Worst case per
//     process: stripeMaxConns × stripeReorder per side plus the extra.

const (
	capExtMConnStripeUp = 1 << 2 // ext bit2: stripes uploads (HS2_MCONN_STRIPE_UP)

	// stripeKMaxUp: the most subs one connection stripes over (key on).
	stripeKMaxUp = 32
	// stripeReorderStep / stripeReorderConnMax / stripeReorderExtraMax: the
	// on-demand growth of a striping receiver's reorder bound (key on).
	stripeReorderStep     = 8 << 20
	stripeReorderConnMax  = 64 << 20
	stripeReorderExtraMax = 128 << 20
)

// stripeUp: HS2_MCONN_STRIPE_UP=1. Atomic so tests can switch it.
var stripeUp atomic.Bool

// stripeSubRateHi: bytes/s each sub must carry on average before a stripe
// grows past its configured K (key on; default 40 Mbit/s). A variable for
// tests.
var stripeSubRateHi atomic.Int64

// stripeUpQTarget: Iran's upload gate target, HS2_MCONN_STRIPE_UP_QMS (ms,
// default HS2_MCONN_STRIPE_QMS's 3). The light users' links sit 1-2 ms
// over their lowest with no queue (delayed ACKs, a stale srtt), so their
// median rarely falls under half of 3 ms: a higher target lets the upload
// grow, a lower one holds it near one link. 0: HS2_MCONN_STRIPE_QMS's.
// A variable for tests.
var stripeUpQTarget atomic.Int64

func init() {
	stripeUpLightPct.Store(50)
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_UP_LQ"))); err == nil && v >= 1 && v <= 50 {
		stripeUpLightPct.Store(int32(v))
	}
	stripeSubRateHi.Store(40e6 / 8)
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_UP_QMS")), 64); err == nil && v > 0 {
		stripeUpQTarget.Store(int64(v * float64(time.Millisecond)))
	}
}

// mconnStripeUp reports whether the other server takes part in an upload
// stripe (it raises its reorder bound for one).
func (pi *peerInfo) mconnStripeUp() bool {
	return pi.mconnStripe() && pi.Ext&capExtMConnStripeUp != 0
}

// setStripeUp (Iran, right after setStripe): the exit announced the upload
// stripe; this connection gates its upload like the exit gates a download.
func (m *mConn) setStripeUp() {
	m.mu.Lock()
	m.stUp = true
	m.mu.Unlock()
}

// stripeKCapLocked: whether the stripe may grow past n subs now (n = 1 +
// live stripe subs; rate: the faster direction's bytes/s).
func (m *mConn) stripeKCapLocked(n int, rate float64) bool {
	if n < m.stK {
		return true
	}
	return stripeUp.Load() && n < stripeKMaxUp && rate >= float64(n)*float64(stripeSubRateHi.Load())
}

// stripeWantCap: links the stripe may ask the pool for at once (process).
func stripeWantCap() int {
	if stripeUp.Load() {
		return stripeKMaxUp
	}
	return stripeWantMax
}

// ---- Iran: the upload's path queue -------------------------------------------

// stripeUpPathQ: Iran's links, measured for the upload's gate (tracked only
// with both keys on).
var stripeUpPathQ = &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, light: true}

// lightQ: the queue the light users' links show (sorted lt, at least one):
// their stripeUpLightPct percentile — the median by default, the lower
// quartile with HS2_MCONN_STRIPE_UP_LQ=25. A light user rarely moves stripeBusyBytes upward, so
// in the upload direction the busy links alone left the gate blind — the
// lab's first version (busy links only) put the game's p95 85 → 108 ms. A
// queue at the bottleneck raises every link's srtt, the lowest ones too;
// a few sparse links (a stale srtt, a loss recovery) sit 5-15 ms over their
// lowest with no queue at all and pulled the median to 1.5-3 ms (the kernel
// lab at 1% loss: most links 0.3-0.5 ms), which held the gate shut. The
// quartile opens it (kernel lab, one upload: 0% 202 → 1020 Mbit/s, 1%
// 146 → 664, 3% 107 → 364) but costs light users at 3% (the game's p95
// 81.9 → 85.6 ms) and in the userspace lab (the game's p95 +2 ms at 0-1%),
// so the median stays the default.
func lightQ(lt []time.Duration) time.Duration {
	return lt[len(lt)*int(stripeUpLightPct.Load())/100]
}

// stripeUpLightPct: HS2_MCONN_STRIPE_UP_LQ (1..50, default 50).
var stripeUpLightPct atomic.Int32

// stripeUpTrack counts an Iran link (its meter and its TCP_INFO reader)
// while it is up; the returned func forgets it. Nothing with a key off.
func stripeUpTrack(mtr *linkMeter, stat func() (tcpStat, bool)) func() {
	if !stripeUp.Load() || stripeK.Load() == 0 || mtr == nil || stat == nil {
		return func() {}
	}
	p := stripeUpPathQ
	p.mu.Lock()
	p.links[mtr] = &stripeLinkQ{stat: stat}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.links, mtr)
		p.mu.Unlock()
	}
}

// trackEdgeLinkUp: newEdgeLink's hook (nothing with a key off).
func trackEdgeLinkUp(l *mtcpLink) {
	if !stripeUp.Load() || stripeK.Load() == 0 || l.sess == nil {
		return
	}
	forget := stripeUpTrack(l.mtr, l.tcpStats)
	go func() {
		<-l.closed()
		forget()
	}()
}

// subMeterOf: the link meter under one of m's subs (Iran: the link it was
// opened on; exit: the counted stream's).
func (m *mConn) subMeterOf(s *msub) *linkMeter {
	if m.iran {
		if l, ok := s.key.(Link); ok {
			return linkMeterOf(l)
		}
		return nil
	}
	return subMeter(s)
}

// ---- receivers: the reorder bound on demand ------------------------------------

// growOOOLocked (under r.mu, the bound reached): more out-of-order room if
// the key allows it. true: the bound grew (the caller looks again).
func (r *mrecv) growOOOLocked() bool {
	if !r.oooGrow {
		return false
	}
	if r.oooMax < stripeReorder {
		// the exit's first time over mRecvBuf: a slot (the same bound as
		// Iran's stripeActive slots)
		if stripeStat.rxActive.Add(1) > stripeMaxConns {
			stripeStat.rxActive.Add(-1)
			return false
		}
		r.oooSlot = true
		r.oooMax = stripeReorder
		stripeStat.reorderGrow.Add(1)
		return true
	}
	if r.oooMax >= stripeReorderConnMax {
		return false
	}
	step := min(stripeReorderStep, stripeReorderConnMax-r.oooMax)
	if stripeReorderExtra.Add(int64(step)) > stripeReorderExtraMax {
		stripeReorderExtra.Add(-int64(step))
		return false
	}
	r.oooMax += step
	r.oooExtra += step
	stripeStat.reorderGrow.Add(1)
	return true
}

// stripeReorderExtra: the process's reorder room above stripeReorder per
// slot (key on).
var stripeReorderExtra atomic.Int64

// releaseOOOLocked (under r.mu): give the grown room (and the exit's slot)
// back; the bound returns to base (0: mRecvBuf).
func (r *mrecv) releaseOOOLocked(base int) {
	if r.oooExtra > 0 {
		stripeReorderExtra.Add(-int64(r.oooExtra))
		r.oooExtra = 0
	}
	if r.oooSlot {
		r.oooSlot = false
		stripeStat.rxActive.Add(-1)
	}
	r.oooMax = base
}

// stripeRxJoin (exit, a stripe sub joined): with the key on, this receiver
// may grow its reorder bound once an upload's reorder needs it.
func (m *mConn) stripeRxJoin() {
	if !stripeUp.Load() {
		return
	}
	r := &m.r
	r.mu.Lock()
	if !r.dead {
		r.oooGrow = true
	}
	r.mu.Unlock()
}

// stripeTargetLocked: the gate's queue target for m's sending direction
// (0: no gate).
func (m *mConn) stripeTargetLocked() time.Duration {
	target := time.Duration(stripeQTarget.Load())
	if up := stripeUpQTarget.Load(); m.iran && target > 0 && up > 0 {
		target = time.Duration(up) // HS2_MCONN_STRIPE_UP_QMS
	}
	return target
}

// stripeUpRoomLocked (Iran, an upload past its first stripe sub): the last
// gate measurement would widen (under half the target), or there is nothing
// to measure. Without it a held width still opened subs that only took
// links (3% loss in the lab: the pool at its ceiling, the game's p95 +11 ms);
// the width's floor of 1 (stripeSendLocked) keeps the pair the upload had.
func (m *mConn) stripeUpRoomLocked() bool {
	return !m.stWInit || m.stNoGate || m.stQ < m.stripeTargetLocked()/2
}

// stripeUpAllUsedLocked (Iran, an upload): every live stripe sub took a
// chunk from the stripe rule in the last mLitePoll (the gate uses them all;
// lastPush is set at the join too, which read as used for a second).
func (m *mConn) stripeUpAllUsedLocked(now time.Time) bool {
	t := now.Add(-mLitePoll)
	for _, s := range m.subs {
		if s.stripe && !s.retired && !s.dead && s.stPushed.Before(t) {
			return false
		}
	}
	return true
}
