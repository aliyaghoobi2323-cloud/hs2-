package engine

import (
	"errors"
	"log"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_EVAC=1 (evac56, Iran; off by default): a striped
// connection moves off a path that went bad under it.
//
// Production (tunnel 1, rel54 with HS2_MCONN_STRIPE_FAST=1): a single upload
// at ~1.3 Gbit/s fell to ~150-200 Mbit/s and stayed there when one of the
// paths it was striped over went lossy (27 % loss, then cut). The cut drain
// keeps flowing connections where they are and the rescue moves only light
// copies, so the heavy connection's stripe sub on the bad path stayed: the
// receiver reassembles by offset, every chunk the scheduler put on that sub
// arrives late, and the whole in-order stream runs at the bad sub's pace
// (head-of-line). The scheduler's "least queued" rule does not help: the
// sub's own queue drains into the link's send buffer, so it looks free.
//
// With the key on, Iran's manager tick (mConn.tick, stK > 0) looks at each
// live sub of a striped connection; one whose link is on a path the
// steering marked lowered or cut (rescueLowKey, lb_rescue.go: the dialing
// side's report, factor <= rescueFactor or cut; nothing is marked while every
// path is bad) is closed — exactly what a link kill does to it — when at
// least one other live sub of the connection is on a good path. The stripe
// RESEND machinery (mconn_stripe_resend.go, default on both sides) then puts
// everything that sub held, wrote in the last stripeKeepAge or was writing at
// the front of a live sub on BOTH sides (Iran's upload chunks; the exit's
// download chunks when it sees the sub end), so no byte is lost; the receiver
// drops what it already has. The growth rule then opens a replacement sub,
// and pickStripe skips links on lowered paths while the key is on, so the
// replacement lands on a good path.
//
// Uploads only (the last rate sample's faster direction is Iran's send,
// stUpNow): Iran is then the sender and its own resend carries everything
// the closed sub held. A download's sender is the exit, which learns of the
// close only when its writes fail: chunks it wrote into the closed stream
// meanwhile were lost and the download ended (lab d1 with a mid-run degrade,
// evac56 first build: 3 of 4 downloads at 0 Mbit/s from ~14 s; rel54 without
// the move recovers there, ratio 1.6). A download would need the close
// handshake (HS2_MCONN_CLOSE_HS: the exit stops sending on the sub first) —
// not done yet.
//
// Bounds (light users and calls are not touched): only striped connections
// (heavy by definition; a call is never striped); at most one sub per
// connection per stripeEvacGap; at most stripeEvacPerSec subs per second over
// the process (HS2_MCONN_STRIPE_EVAC_PER_SEC, default 8); never while the
// connection is handshaking, closing or not in the light plane; never the
// last sub on a good path.
var stripeEvacOn atomic.Bool // loadEnvKeys: HS2_MCONN_STRIPE_EVAC

var errStripeEvac = errors.New("mconn: stripe sub moved off a lowered path")

// capExtMConnResend: ext bit3 of kindInfo (peerinfo.go): this server survives
// a striping sub's death by a resend (HS2_MCONN_STRIPE_RESEND on). Iran
// closes a sub on purpose only toward an exit that announces it: an older
// exit (r31: no resend) would reset the connection when the sub ends.
const capExtMConnResend = 1 << 3

// stripeEvacPeerOK: the exit behind s's link announced the resend (tests
// replace it).
var stripeEvacPeerOK = func(s *msub) bool {
	l, ok := s.key.(Link)
	if !ok {
		return false
	}
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi.mconnStripe() && pi.Ext&capExtMConnResend != 0
}

const stripeEvacGap = time.Second

var (
	stripeEvacPerSec = func() int64 {
		if n, err := strconv.Atoi(os.Getenv("HS2_MCONN_STRIPE_EVAC_PER_SEC")); err == nil && n > 0 {
			return int64(n)
		}
		return 8
	}()
	stripeEvacSec     atomic.Int64
	stripeEvacSecN    atomic.Int64
	stripeEvacN       atomic.Uint64 // subs moved off (diagnostics)
	stripeEvacRefused atomic.Uint64 // refused by the rate bound
	// stripeEvacLow: the lowered-path test (rescueLowKey; tests replace it)
	stripeEvacLow = rescueLowKey
)

// stripeEvacAllow takes one move from the per-second bound.
func stripeEvacAllow(now time.Time) bool {
	sec := now.Unix()
	if old := stripeEvacSec.Load(); old != sec && stripeEvacSec.CompareAndSwap(old, sec) {
		stripeEvacSecN.Store(0)
	}
	if stripeEvacSecN.Add(1) > stripeEvacPerSec {
		stripeEvacRefused.Add(1)
		return false
	}
	return true
}

// stripeEvacLocked (Iran, manager tick, under m.mu): the sub to close now,
// or nil. The caller closes it outside the lock (subDied).
func (m *mConn) stripeEvacLocked(now time.Time) *msub {
	if !stripeEvacOn.Load() || !m.iran || m.stN == 0 || !stripeResend.Load() || m.ended || m.closing || m.handshaking ||
		m.mode != planeLight || m.rawW.Load() || now.Before(m.stEvacAt) || (!m.stUpNow && !stripeEvacDown.Load()) {
		return nil
	}
	p := m.primary.Load()
	var bad *msub
	good := 0
	for _, s := range m.subs {
		if s.retired || s.dead || s.closeHS || (!s.stripe && s != p) {
			continue
		}
		if stripeEvacLow(s.key) {
			if !m.stUpNow && (s == p || !subEvacHS(s.rw)) {
				// a download moves only a stripe sub whose exit reads
				// CLOSE-REQ (stripeEvacDownLocked)
				continue
			}
			if bad == nil || bad == p { // a stripe sub before the primary
				bad = s
			}
			continue
		}
		good++
	}
	if bad == nil || good == 0 || !stripeEvacPeerOK(bad) || !stripeEvacAllow(now) {
		return nil
	}
	m.stEvacAt = now.Add(stripeEvacGap)
	stripeEvacN.Add(1)
	if !m.stUpNow {
		m.stripeEvacDownLocked(bad, good)
		return nil
	}
	m.tr.ev("stripe: evac sub %d (lowered path, primary=%v)", bad.idx, bad == p)
	// a rare event (striped connections only, rate-bounded): always logged,
	// so production shows that the move engages
	log.Printf("stripe %x: evac sub %d off a lowered path (primary=%v good=%d, moved so far %d, rate-refused %d)", m.id&0xffffff, bad.idx, bad == p, good, stripeEvacN.Load(), stripeEvacRefused.Load())
	return bad
}

// HS2_MCONN_STRIPE_EVAC_RAW=1 (with HS2_MCONN_STRIPE_EVAC; off by default,
// not yet measured in the lab): a raw connection (one sub, not striped yet)
// whose sub is on a lowered path and still moves at least stripeEvacRawRate
// grows a stripe sub on a good path even below the stripe growth rate (on a
// lossy path its rate fell under it, so it would never stripe and stay at
// the bad path's pace). Once the new sub is live the primary on the lowered
// path is moved off by stripeEvacLocked. The raw upgrade's handshake runs
// over the lowered path (as HS2_LB_RESCUE_RAW's): a sub dying in it ends
// the connection — only connections that move bulk data take that risk, a
// light user never does (the rate floor), and the per-second bound applies.
var stripeEvacRaw atomic.Bool // loadEnvKeys: HS2_MCONN_STRIPE_EVAC_RAW

// stripeEvacRawRate: bytes/s a raw connection must still move (1 MB/s).
const stripeEvacRawRate = 1 << 20

// stripeEvacRawGrowLocked (stripeTickLocked, under m.mu): a raw connection
// on a lowered path grows despite a rate under the growth rate.
func (m *mConn) stripeEvacRawGrowLocked(n int, rate float64, now time.Time) bool {
	if !stripeEvacOn.Load() || !stripeEvacRaw.Load() || !m.iran || n != 1 || !m.rawW.Load() || m.raw1 == nil || !m.stUpNow ||
		rate < stripeEvacRawRate || !stripeEvacLow(m.raw1.key) || now.Before(m.stEvacAt) || !stripeEvacAllow(now) {
		return false
	}
	m.stEvacAt = now.Add(stripeEvacGap)
	log.Printf("stripe %x: raw sub on a lowered path at %.1f Mbit/s — growing a stripe sub on a good path", m.id&0xffffff, rate*8/1e6)
	return true
}

// stripeEvacSkip (pickStripe and its shared / cross-direction picks): with
// the key on, a new stripe sub never goes on a link of a lowered path.
func stripeEvacSkip(l any) bool {
	return stripeEvacOn.Load() && stripeEvacLow(l)
}

// ---- downloads (evac58, HS2_MCONN_STRIPE_EVAC_DOWN) --------------------------
//
// HS2_MCONN_STRIPE_EVAC_DOWN=1 (with HS2_MCONN_STRIPE_EVAC; off by default):
// a download's stripe sub on a lowered path is moved off too, through the
// close handshake (mconn_closehs.go) instead of a close. The sender is the
// exit: closing the sub under it lost what it wrote meanwhile (evac56's
// first build: downloads at 0). Now:
//
//  1. Iran: CLOSE-REQ on the bad stripe sub (retired: nothing more of
//     Iran's goes there; the primary is never moved this way).
//  2. Exit (onCloseReq): retires the sub (the stripe puts nothing more on
//     it) and queues again, at the front of its live sub with the shortest
//     queue, every sequence chunk the bad sub holds or wrote within
//     stripeKeepAge (stripeCloseResendLocked); the bad sub keeps its own
//     copies and drains them, then CLOSE-ACK(end). Iran's reassembly drops
//     the duplicates, so the in-order stream no longer waits for the lossy
//     path.
//  3. Iran: CLOSE-ACK once every byte up to end was read on the sub, then
//     CLOSE-DONE; both sides close it cleanly (closedCleanLocked).
//
// The growth rule sees one stripe sub fewer and opens a replacement, which
// pickStripe keeps off lowered paths. Peer gate: the exit announces
// flagMConnCloseHS (production default on both sides) and the resend (ext
// bit 3). An exit without this build's step 2 still answers CLOSE-REQ
// correctly; its queued chunks then drain over the bad path (slower, no loss).
var stripeEvacDown atomic.Bool // loadEnvKeys: HS2_MCONN_STRIPE_EVAC_DOWN

var stripeEvacDownN atomic.Uint64 // CLOSE-REQs sent by the download move

// subEvacHS: s is a stripe sub whose exit reads CLOSE-REQ (the edge's
// openStripe sets it from flagMConnCloseHS; kept apart from the lite subs'
// closeHS so the lite rules never close a stripe sub).
func subEvacHS(rw mSub) bool {
	c, ok := rw.(interface{ mconnEvacHS() bool })
	return ok && c.mconnEvacHS()
}

// stripeEvacDownLocked (Iran, under m.mu): step 1 on bad.
func (m *mConn) stripeEvacDownLocked(bad *msub, good int) {
	bad.retired = true
	bad.closeHS = true
	bad.push(getChunk(mRecCloseReq))
	stripeEvacDownN.Add(1)
	m.sendCond.Broadcast()
	m.tr.ev("stripe: evac (download) CLOSE-REQ on sub %d", bad.idx)
	log.Printf("stripe %x: evac sub %d off a lowered path (download: close handshake, good=%d, moved so far %d, downloads %d, rate-refused %d)", m.id&0xffffff, bad.idx, good, stripeEvacN.Load(), stripeEvacDownN.Load(), stripeEvacRefused.Load())
}

// stripeCloseResendLocked (exit, onCloseReq, under m.mu): step 2's copy.
// s stays as it is (its queue and kept chunks drain or die with it); the
// chunks are queued once more at the front of the live sub with the
// shortest queue. 0: nothing queued (not striping, no resend, no live sub).
func (m *mConn) stripeCloseResendLocked(s *msub) int {
	if m.iran || !s.stripe || !stripeResend.Load() || m.stN == 0 || m.ended {
		return 0
	}
	b := m.resendTargetLocked()
	if b == nil || b == s {
		return 0
	}
	var cs []*mchunk
	for _, r := range s.sent {
		cs = append(cs, r.c)
	}
	for _, c := range s.q {
		if c.seq() {
			cs = append(cs, c)
		}
	}
	if s.cur != nil && s.cur.seq() {
		cs = append(cs, s.cur)
	}
	for _, c := range s.curMore {
		if c.seq() {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		return 0
	}
	slices.SortFunc(cs, func(a, b *mchunk) int {
		switch {
		case a.off < b.off:
			return -1
		case a.off > b.off:
			return 1
		}
		return 0
	})
	cs = slices.CompactFunc(cs, func(a, b *mchunk) bool { return a == b })
	front := make([]*mchunk, 0, len(cs)+len(b.q))
	for _, c := range cs {
		c.refs.Add(1)
		c.copies++
		if e := c.off + uint64(c.n); e > b.txEnd {
			b.txEnd = e
		}
		b.qBytes += c.qsize()
		front = append(front, c)
	}
	b.q = append(front, b.q...)
	b.lastPush = time.Now()
	select {
	case b.kick <- struct{}{}:
	default:
	}
	stripeResendStat.chunks.Add(uint64(len(cs)))
	log.Printf("stripe %x: CLOSE-REQ on stripe sub %d: %d chunks queued again on sub %d", m.id&0xffffff, s.idx, len(cs), b.idx)
	return len(cs)
}
