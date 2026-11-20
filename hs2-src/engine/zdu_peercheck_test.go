package engine

import (
	"strings"
	"testing"
)

// Review F2: the handover peer check. Kharej (exit link set): every live
// link's Iran must have announced capExtZDU + an instance. Iran (link
// pools): every live link's Kharej likewise.
func TestZDUPeerCheck(t *testing.T) {
	setZDU(t, true)
	was := zduExitPeers.Load()
	t.Cleanup(func() { zduExitPeers.Store(was) })
	zduLMs.mu.Lock()
	lms := zduLMs.l
	zduLMs.l = nil
	zduLMs.mu.Unlock()
	t.Cleanup(func() { zduLMs.mu.Lock(); zduLMs.l = lms; zduLMs.mu.Unlock() })

	zduExitPeers.Store(nil)
	if ok, l, _ := ZDUPeerCheck(); !ok || l != 0 {
		t.Fatalf("no links: ok %v links %d", ok, l)
	}
	// Kharej: two ZDU Iran links + one pre-ZDU (rel69p or key off) link.
	p := &linkPeers{}
	zduExitPeers.Store(p)
	a, b, c := zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInst{})
	p.add(a.mtr)
	p.add(b.mtr)
	if ok, l, z := ZDUPeerCheck(); !ok || l != 2 || z != 2 {
		t.Fatalf("all ZDU: ok %v %d/%d", ok, z, l)
	}
	p.add(c.mtr)
	if ok, l, z := ZDUPeerCheck(); ok || l != 3 || z != 2 {
		t.Fatalf("one pre-ZDU Iran link: ok %v %d/%d", ok, z, l)
	}
	if s := ZDUPeerLine(); !strings.HasPrefix(s, "ZDU-NO links=3 zdu=2") {
		t.Fatalf("line %q", s)
	}
	// a link whose kindInfo answer has not arrived (no peerInfo) is not ZDU
	p.remove(c.mtr)
	d := newMCLink(capPortTags | capMConn)
	d.mtr.peerInfo.Store(nil)
	p.add(d.mtr)
	if ok, _, _ := ZDUPeerCheck(); ok {
		t.Fatal("link with no answer counted as ZDU")
	}
	zduExitPeers.Store(nil)

	// Iran: a link pool whose Kharej links lack ZDU.
	m, _ := zduPool(0, 2)
	zduLMs.mu.Lock()
	zduLMs.l = []*LinkManager{m}
	zduLMs.mu.Unlock()
	if ok, l, z := ZDUPeerCheck(); !ok || l != 2 || z != 2 {
		t.Fatalf("iran all ZDU: ok %v %d/%d", ok, z, l)
	}
	if s := ZDUPeerLine(); s != "ZDU-OK links=2 zdu=2" {
		t.Fatalf("line %q", s)
	}
	zduAddLinks(m, zduLink(zduInst{}))
	if ok, l, z := ZDUPeerCheck(); ok || l != 3 || z != 2 {
		t.Fatalf("iran one pre-ZDU Kharej link: ok %v %d/%d", ok, z, l)
	}
	m.zduForget()
}
