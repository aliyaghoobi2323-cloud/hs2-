package engine

// xs261 (HS2_XS261, default on; "0" = the b254 behaviour): the cross-side
// switch. A one-direction cut on a call's link is seen by one side only:
// lab1 b258 NN2 2u6, Iran's sends on the up-cut link froze and darkcall moved
// Iran's primary off it in 1.3 s, but the exit kept sending its direction on
// that link (its TCP saw no trouble until the 2 MB smux window filled, 8.4 s
// in, because Iran's window updates rode the cut direction), so Iran's
// receive stalled ~5 s.
//
//   - XS record (kind 0x15 + u64 seq, on a sub): "make the sub this record
//     arrived on your primary now". It names no sub id (the two sides number
//     their subs apart): the sub it rides is the one to use.
//   - Sent by a side that (a) moved a call off a dark/txDark sub (the
//     record rides the new primary), or (b) whose call receive is silent for
//     >= xsRxAfter while another sub it can send on shows a fresh receive on
//     its link (the record rides that sub). At most one per xsSendGap per
//     conn (a new target may go sooner on the switch path).
//   - The receiver acts for that conn only: a live sub that is not already
//     its primary and that it does not itself see dark/txDark becomes the
//     primary through darkcall's switch (resend of what the peer may lack).
//     A seq not above the last one seen is stale and ignored; at most one
//     act per xsActGap. Never answered (no echo), so it cannot ping-pong.
//   - Compatibility: an older peer ends the conn on an unknown record
//     (errMConnProto), so a side sends XS only on a link whose peer announced
//     capExtMConnXS over kindInfo (ext bit 14, only with the key on). The
//     record is parsed with the key off too.
//   - b258's cs224 fix: a sub a darkcall switch chooses loses its "left"
//     mark (mconn_darkcall.go), under the same key.

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sync/atomic"
	"time"
)

const (
	mRecXS = 0x15 // either side: kind + u64 seq

	// capExtMConnXS: ext bit 14 of kindInfo: this server reads XS records.
	capExtMConnXS = 1 << 14

	xsRxAfter  = 800 * time.Millisecond
	xsSendGap  = time.Second
	xsSwGap    = 300 * time.Millisecond // a switch to a new target
	xsActGap   = 500 * time.Millisecond
	xsRxResend = 1500 * time.Millisecond // a still-silent receive asks again
	// xsRxMax: only the onset of a silence asks (a call whose other
	// direction simply carries nothing must not keep asking)
	xsRxMax = 4 * time.Second
)

var xs261On atomic.Bool

func init() { xs261On.Store(envBoolDefault("HS2_XS261", true)) }

// xsConn: per conn, under m.mu.
type xsConn struct {
	seqOut uint64
	seqIn  uint64
	sentAt time.Time
	sentTo *msub
	actAt  time.Time
}

var xsStat struct {
	sentSw   atomic.Uint64 // sent after our own switch
	sentRx   atomic.Uint64 // sent on a silent receive
	noCap    atomic.Uint64 // not sent: the peer did not announce the cap
	rx       atomic.Uint64 // received
	acted    atomic.Uint64 // the primary moved
	same     atomic.Uint64 // already the primary
	stale    atomic.Uint64 // seq not above the last
	rate     atomic.Uint64 // within xsActGap of the last act
	ign      atomic.Uint64 // key off / conn not light / sub not live / sub dark here
	noCandRx atomic.Uint64 // silent receive without a candidate
}

func xs261LogPart() string {
	s := &xsStat
	return fmt.Sprintf(" · xs261 on %v sent sw %d rx %d nocap %d nocand %d · got %d acted %d same %d stale %d rate %d ign %d",
		xs261On.Load(), s.sentSw.Load(), s.sentRx.Load(), s.noCap.Load(), s.noCandRx.Load(),
		s.rx.Load(), s.acted.Load(), s.same.Load(), s.stale.Load(), s.rate.Load(), s.ign.Load())
}

type xsPeerFn func(*msub) bool

var xsPeerP atomic.Pointer[xsPeerFn]

func init() { xsPeerSet(xsPeerOS) }

func xsPeerSet(f xsPeerFn) { xsPeerP.Store(&f) }

// xsPeerOS: the peer on s's link announced capExtMConnXS.
func xsPeerOS(s *msub) bool {
	mtr := dcSubMeter(s)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.Ext&capExtMConnXS != 0
}

func xsPeer(s *msub) bool { return (*xsPeerP.Load())(s) }

// xsSendLocked (under m.mu): ask the peer to make to its primary. rx: the
// silent-receive trigger (else a switch of ours).
func (m *mConn) xsSendLocked(to *msub, now time.Time, rx bool) bool {
	if !xs261On.Load() || to == nil || to.dead || to.retired || m.ended || m.closing ||
		to == m.raw1 && m.rawW.Load() { // a raw sub carries no records
		return false
	}
	x := &m.dc.xs
	gap := xsSendGap
	if !rx && to != x.sentTo {
		gap = xsSwGap
	}
	if !x.sentAt.IsZero() && now.Sub(x.sentAt) < gap {
		return false
	}
	if !xsPeer(to) {
		xsStat.noCap.Add(1)
		return false
	}
	x.seqOut++
	x.sentAt, x.sentTo = now, to
	c := getChunk(mRecXS)
	binary.BigEndian.PutUint64(c.buf[1:9], x.seqOut)
	// ahead of any resend the switch queued: the peer should leave the cut
	// link before it reads the copies
	c.refs.Add(1)
	to.q = slices.Insert(to.q, 0, c)
	select {
	case to.kick <- struct{}{}:
	default:
	}
	if rx {
		xsStat.sentRx.Add(1)
	} else {
		xsStat.sentSw.Add(1)
	}
	m.dcLogf("%x xs261 send iran=%v sub %d(%s) seq %d rx=%v", m.id, m.iran, to.idx, to.dc.path, x.seqOut, rx)
	return true
}

// xsRxSilentLocked (darkcall visit of a call, under m.mu): the call's
// receive is silent >= xsRxAfter: the peer's primary is likely on a link
// whose peer->us direction is cut. Ask it to use a sub whose link shows a
// fresh receive and that our sends do not see dark.
func (m *mConn) xsRxSilentLocked(live []*msub, now time.Time, nowc int64) {
	d := &m.dc
	if sil := now.Sub(d.rxAt); !xs261On.Load() || d.rxAt.IsZero() || sil < xsRxAfter || sil > xsRxMax {
		return
	}
	if !d.xs.sentAt.IsZero() && now.Sub(d.xs.sentAt) < xsRxResend {
		return
	}
	// the peer's primary: the sub we last read a record on
	var a *msub
	for _, s := range live {
		if a == nil || s.lastRecv.Load() > a.lastRecv.Load() {
			a = s
		}
	}
	var to *msub
	for _, s := range live {
		if s == a || !s.dc.ok || s.dc.isDark || s.dc.txDark {
			continue
		}
		if a != nil && s.dc.path != "" && s.dc.path == a.dc.path {
			continue // same path: likely as cut
		}
		if s.dc.lastRd == 0 || time.Duration(nowc-s.dc.lastRd) >= darkcallAfter() {
			continue // no proof its link receives
		}
		if to == nil || s.dc.lastRd > to.dc.lastRd {
			to = s
		}
	}
	if to == nil {
		xsStat.noCandRx.Add(1)
		d.xs.sentAt = now // do not scan every tick
		return
	}
	m.xsSendLocked(to, now, true)
}

// onXS (reader of s): the peer asks us to make s our primary.
func (m *mConn) onXS(s *msub, seq uint64) {
	xsStat.rx.Add(1)
	if !xs261On.Load() {
		xsStat.ign.Add(1)
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	x := &m.dc.xs
	if seq <= x.seqIn {
		xsStat.stale.Add(1)
		return
	}
	x.seqIn = seq
	if m.ended || m.closing || m.mode != planeLight || m.stN > 0 || m.rawW.Load() || s.dead || s.retired || s.bulk.Load() ||
		!slices.Contains(m.rrLiveSubsLocked(), s) || s.dc.isDark || s.dc.txDark {
		xsStat.ign.Add(1)
		return
	}
	p := m.primary.Load()
	if p == s {
		xsStat.same.Add(1)
		return
	}
	if !x.actAt.IsZero() && now.Sub(x.actAt) < xsActGap {
		xsStat.rate.Add(1)
		return
	}
	x.actAt = now
	xsStat.acted.Add(1)
	if p == nil || p.dead || p.retired {
		m.prefPrimary = s
		m.setPrimaryLocked()
		m.sendCond.Broadcast()
	} else {
		r := &m.r
		r.mu.Lock()
		next := r.next
		r.mu.Unlock()
		m.dcSwitchLocked(p, s, now, next)
	}
	m.dcLogf("%x xs261 act iran=%v -> sub %d(%s) seq %d", m.id, m.iran, s.idx, s.dc.path, seq)
}
