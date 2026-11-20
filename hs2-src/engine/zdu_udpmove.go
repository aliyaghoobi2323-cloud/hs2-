package engine

import (
	"sync/atomic"
	"time"
)

// ZDU (proposals/zdu350.md, g400): quiet UDP flows leave an old exit
// generation's links.
//
// After a Kharej handover the old Kharej process exits at "drain end", once
// no user connection rides its links. A TCP user connection cannot move, so
// it is drained: the hourly xray restart (HH:01) ends them all. A UDP flow on
// the Iran user port is different: its stream stays open while the client
// sends, and the xray restart ends it only if a datagram hits the Kharej's
// connected backend socket while xray is down (the refused read closes the
// stream). A flow that was quiet in those seconds keeps the old process
// alive to the next storm, or to the drain deadline (zdu392: 4 UDP flows held
// the old Kharej for 11 min). It does not need to: a UDP flow carries no
// byte stream, so ending it only means the client's next datagram opens a
// new flow, on a current link (the picker skips peerOld links). The backend
// sees a new source port (a NAT rebind: QUIC and WireGuard follow it).
//
// Rule (key on only): a flow whose link is peerOld (zdu_counts.go) and that
// carried no datagram either way for zduUDPMoveIdle is ended. An active flow
// is never cut here: datagrams in flight are never dropped by this rule.
// Key off: the sweep runs every 30 s and only ends flows idle for udpIdle,
// exactly as before.

// zduUDPMoveIdle: how quiet a flow on an old generation's link must be.
const zduUDPMoveIdle = 10 * time.Second

// zduUDPMoved counts flows ended by the rule (zdu_udp_moved= on the mem line).
var zduUDPMoved atomic.Int64

// udpSweepEvery is the UDP port's sweep period: 30 s, 5 s with the key on
// (so a quiet flow leaves an old link within zduUDPMoveIdle + 5 s).
func udpSweepEvery() time.Duration {
	if zduOn.Load() {
		return 5 * time.Second
	}
	return 30 * time.Second
}

// sweepUDPFlows ends idle flows (udpIdle) and, key on, quiet flows on an old
// generation's link. Caller holds the port's mu.
func sweepUDPFlows(flows map[string]*udpFlow, now time.Time) {
	zdu := zduOn.Load()
	for k, f := range flows {
		idle := now.Sub(f.last)
		switch {
		case idle > udpIdle:
		case zdu && idle > zduUDPMoveIdle && f.mtr != nil && f.mtr.zduOld.Load():
			zduUDPMoved.Add(1)
		default:
			continue
		}
		f.end()
		delete(flows, k)
	}
}
