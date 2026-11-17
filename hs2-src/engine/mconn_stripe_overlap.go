package engine

import "sync/atomic"

// HS2_MCONN_STRIPE_OVERLAP=1 (Iran, with HS2_MCONN_STRIPE_FAST; off by
// default): the stripe's doubling chain (stFastMore) after a raw upgrade does
// not wait for the upgrade's handshake to end (upov57).
//
// Why: production (tunnel 1, rel54, FAST + EARLY_MS 200, 75 ms RTT) showed a
// single upload at ~370 Mbit/s for ~4 s and ~1000 after: up56 logged `tick
// held hs=true` — the chain waits for UPGRADE(Bk), which the exit sends only
// once its raw writer in flight on the first sub is done (mconn_raw.go step
// 2), and the first sub's link is a busy shared one. Meanwhile only the
// upgrade's own sub took new data.
//
// What changes: nothing on the wire. Iran already writes records on the
// upgrade's sub from step 1, tagged by offset, and the exit already keeps
// records past the raw boundary in reassembly (out of order) until its raw
// reader reaches it (mconn_raw.go: "records from the second sub wait in
// reassembly"). With the key the chain's extra stripe subs open at once
// after the upgrade's sub, each with STRIPE first (onStripe raises the exit's
// reorder bound: stripeReorder, more on demand with HS2_MCONN_STRIPE_UP), so
// the bytes the raw sub's queue still holds drain while the new data goes
// over every stripe sub. The raw sub becomes just another sub once the
// handshake ends (UPACK), as today.
//
// Limits kept: the reorder bound and the process's reorder budget hold the
// stripe subs when the gap is too wide (deliver waits, it never drops), so
// memory stays bounded as before. RESEND/EVAC stay off while handshaking
// (stripeGraceOnLocked / stripeEvacLocked check it), and a sub dying while
// handshaking ends the connection (subDied) — with the key more subs live in
// that window, so the chain overlaps only toward an exit that announced
// capExtMConnOverlap (ext bit4): an exit of this version, tested with joins
// that come before UPGRADE and with records far past its raw position. An
// older exit (r31, rel54, evac56) keeps today's order.
var stripeOverlapOn atomic.Bool // loadEnvKeys: HS2_MCONN_STRIPE_OVERLAP

// capExtMConnOverlap: ext bit4 of kindInfo (peerinfo.go): this server takes
// stripe joins and records past the raw boundary while a raw upgrade's
// handshake is still on (announced with the stripe on; the key is Iran's).
const capExtMConnOverlap = 1 << 4

// stripeOverlapPeerOK: the exit behind s's link announced capExtMConnOverlap
// (tests replace it).
var stripeOverlapPeerOK = func(s *msub) bool {
	l, ok := s.key.(Link)
	if !ok {
		return false
	}
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi.mconnStripe() && pi.Ext&capExtMConnOverlap != 0
}

// stripeOverlapStat: chain opens that did not wait for the handshake.
var stripeOverlapStat atomic.Uint64

// stripeOverlapLocked (Iran, under m.mu): the chain may go on now although
// the raw upgrade's handshake has not ended.
func (m *mConn) stripeOverlapLocked() bool {
	if !m.handshaking {
		return true
	}
	if !stripeOverlapOn.Load() || !m.iran || m.raw1 == nil || m.rawW.Load() {
		return false
	}
	for _, s := range m.subs {
		if s != m.raw1 && s.stripe && !s.dead {
			return stripeOverlapPeerOK(s)
		}
	}
	return false
}
