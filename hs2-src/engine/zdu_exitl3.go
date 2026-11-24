package engine

import "sync/atomic"

// ZDU review issue 6 (proposals/zdu350.md): the exit's (Kharej's) L3 picker
// keeps TUN packets off an older Iran generation's links.
//
// During an Iran handover the exit holds links to the old Iran (draining) and
// to the new one. The old Iran exits when its users are gone, and every TUN
// download the exit queued on one of its links then is lost. So the exit marks
// its links of an older Iran instance (linkMeter.zduOld, the same mark the Iran
// side sets from its LinkManager) once the newest instance has at least half
// as many live links as the older ones (the stepped shift of zdu_phase.go: a
// single new link never takes every TUN flow), and the L3 picker (zdu_l3.go)
// then picks only among current links while one is alive. Any process can
// write the shared TUN, so a download on a new-Iran link reaches the user
// whether or not the new Iran has its GO yet.
//
// HS2_ZDU off: zduExitMark returns at once; no meter is ever marked.

// zduExitMarked: the exit links marked old (zduLinks' fast test).
var zduExitMarked atomic.Int64

// zduExitMark re-marks the exit's older-generation links. Called when a
// link's kindInfo arrives and when a link leaves.
func (p *linkPeers) zduExitMark() {
	if p == nil || !zduOn.Load() {
		return
	}
	p.mu.Lock()
	var cur zduInst
	for m := range p.m {
		if pi := m.peerInfo.Load(); pi.zdu() && pi.Inst.newer(cur) {
			cur = pi.Inst
		}
	}
	n, o := 0, 0
	for m := range p.m {
		if pi := m.peerInfo.Load(); pi.zdu() {
			if pi.Inst == cur {
				n++
			} else {
				o++
			}
		}
	}
	censorPeerInst(cur) // b437 Q2: a newer peer instance is a generation change (own lock only)
	mark := cur.known() && o > 0 && 2*n >= o
	marked := int64(0)
	for m := range p.m {
		pi := m.peerInfo.Load()
		x := mark && pi.zdu() && pi.Inst != cur
		if m.zduOld.Load() != x {
			m.zduOld.Store(x)
			zduL3Gen.Add(1)
		}
		if x {
			marked++
		}
	}
	p.mu.Unlock()
	zduExitMarked.Store(marked)
}
