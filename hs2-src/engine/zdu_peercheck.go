package engine

import "fmt"

// ZDU review F2: a handover is safe only when the peer side runs ZDU too.
// Without capExtZDU + an instance on the far end there is no generation pin
// there (a Kharej handover with a rel69p Iran, or one with HS2_ZDU off,
// lets mconn subs cross instances; an Iran handover with such a Kharej has
// no legacy slots or poolctl filter). So both sides refuse unless every live
// link's own kindInfo answer announced capExtZDU and an instance. A link
// whose answer has not arrived yet counts as not ZDU (refuse, retry later).

// ZDUPeerCheck reports whether every live link of this process (Iran: every
// link pool's links; Kharej: the exit's link set) has a ZDU peer. links is
// the live link count, zdu the ones with a ZDU peer. No live link = ok (no
// peer to protect; the warm wait still needs a link before READY).
func ZDUPeerCheck() (ok bool, links, zdu int) {
	zduLMs.mu.Lock()
	ls := append([]*LinkManager(nil), zduLMs.l...)
	zduLMs.mu.Unlock()
	for _, m := range ls {
		if m.closing.Load() {
			continue
		}
		l, z := m.zduPeerCount()
		links += l
		zdu += z
	}
	l, z := zduExitPeers.Load().zduPeerCount()
	links += l
	zdu += z
	return zdu == links, links, zdu
}

// ZDUPeerLine is the check as one status line: "ZDU-OK links=N zdu=N" or
// "ZDU-NO links=N zdu=M" (M < N).
func ZDUPeerLine() string {
	ok, l, z := ZDUPeerCheck()
	w := "ZDU-NO"
	if ok {
		w = "ZDU-OK"
	}
	return fmt.Sprintf("%s links=%d zdu=%d", w, l, z)
}

func (m *LinkManager) zduPeerCount() (links, zdu int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		links++
		if linkZDUInst(ml.link).known() {
			zdu++
		}
	}
	return
}

func (p *linkPeers) zduPeerCount() (links, zdu int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for m := range p.m {
		links++
		if pi := m.peerInfo.Load(); pi.zdu() {
			zdu++
		}
	}
	return
}
