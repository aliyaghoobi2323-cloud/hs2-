package engine

import "sync/atomic"

// rc15 (HS2_LITE_HEDGE8 default on): the hedge is Iran's decision, so Iran
// hedges only toward an exit that reads it. The exit announces
// capExtLiteHedge (ext bit 18) when its HEDGE3 machinery is on (HEDGE8 implies
// it). Toward an exit without the bit (rc14/rc12, or HS2_LITE_HEDGE8=0):
//   - the first sub stays RAW when the link allows it (rc14 behaviour; the
//     hedge wants records only toward a hedge exit),
//   - no hedge sub is opened (hedgeStartLocked),
//   - a hedge open that lands on such a link anyway (another link of a
//     mixed pool) goes out as a plain join, never as role 7.
//
// An rc15 exit with an old Iran never sees role 7: the exit side only adds
// the early response window (hedgeExitInit), a bounded buffer.
var capExtLiteHedge uint32 = 1 << 18

var hedgeCapStat struct {
	noCap atomic.Uint64 // conns not hedged: the first sub's exit has no capExtLiteHedge
}

// hedgePeerOK: the exit behind link l reads role 7 (mconnRoleHedge).
func hedgePeerOK(l Link) bool {
	if !liteHedge3 {
		return false
	}
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.Ext&capExtLiteHedge != 0
}

// hedgeLinkOK: a hedged conn may ride link l (HEDGE2 alone keeps its old,
// ungated behaviour; HEDGE3+ needs the exit's bit).
func hedgeLinkOK(l Link) bool {
	return !liteHedge3 || hedgePeerOK(l)
}

// hedgeKeyOK: firstKey (the first sub's open key) is a link whose exit
// reads the hedge. Keys that are not links (tests' fake openers) pass.
func hedgeKeyOK(firstKey any) bool {
	if !liteHedge3 {
		return true
	}
	if l, ok := firstKey.(Link); ok {
		return hedgeLinkOK(l)
	}
	return true
}
