package engine

import (
	"sync/atomic"
	"testing"
)

// d421 bench adapter: l3Rx keyed by the link's peer instance; two links of
// two processes (ZDU on: the bench's two streams come from them).
func benchRxLinks() []*l3Link {
	return []*l3Link{instLink(1, zduInst{Start: 1, Rand: 1}), instLink(2, zduInst{Start: 2, Rand: 2})}
}
func benchRx() *l3Rx                             { return &l3Rx{} }
func benchRxAccept(r *l3Rx, l *l3Link, p []byte) { r.accept(l.peerInst(), p) }

// instLink is a dup-capable link whose peer announced ZDU instance in (zero:
// no instance, an old peer).
func instLink(id uint32, in zduInst) *l3Link {
	l := dupLink(id, 0, true)
	l.peer = new(atomic.Pointer[peerInfo])
	pi := &peerInfo{V2: true, Inst: in}
	if in.known() {
		pi.Ext = capExtZDU
	}
	l.peer.Store(pi)
	return l
}

func benchSecondLinks(n int) []*l3Link {
	var ls []*l3Link
	for i := 0; i < n; i++ {
		ls = append(ls, instLink(uint32(i)*40503+7, zduInst{Start: uint64(1 + i%2), Rand: 5}))
	}
	return ls
}

// BenchmarkL3DupSecondNoInst is BenchmarkL3DupSecond over links without a
// peer instance (an old peer, HS2_ZDU off): the before-fix links.
func BenchmarkL3DupSecondNoInst(b *testing.B) {
	var links []*l3Link
	for i := 0; i < 96; i++ {
		links = append(links, dupLink(uint32(i)*40503+7, 0, true))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = dupSecond(links, uint32(i)*2654435761, links[i%96])
	}
}

// BenchmarkL3DupSecondOneInst: every link of one peer process (ZDU on, no
// handover: the normal production case).
func BenchmarkL3DupSecondOneInst(b *testing.B) {
	var links []*l3Link
	for i := 0; i < 96; i++ {
		links = append(links, instLink(uint32(i)*40503+7, zduInst{Start: 1, Rand: 5}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = dupSecond(links, uint32(i)*2654435761, links[i%96])
	}
}
