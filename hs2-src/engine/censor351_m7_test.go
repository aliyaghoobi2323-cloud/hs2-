package engine

import (
	"testing"
	"time"
)

func TestCensorAccPeerLossAndBusy(t *testing.T) {
	a := newCensorAcc(2)
	prev := qPrev{acked: 0}
	prev.cen = censorPrev{peerOK: true, peerRt: 10, segsIn: 1000, chrono: true, busyUs: 0}
	cur := qPrev{acked: 1 << 20}
	cur.cen = censorPrev{peerOK: true, peerRt: 40, segsIn: 2000, chrono: true, busyUs: 4_500_000, rwndUs: 500_000, rcvd: 0}
	a.add(1, &cur, &prev, 5*time.Second)
	p := a.p[1]
	if p.peerRt != 30 || p.segsIn != 1000 || p.busy != 1 || p.chronoN != 1 {
		t.Fatalf("acc %+v", p)
	}
	if p.limited < 0.79 || p.limited > 0.81 {
		t.Fatalf("limited %v, want 0.8", p.limited)
	}
	s := censorLBSample(nil, "10.0.0.1", "10.0.0.2", q2Reading{loss: 0.01}, 1e6, a, 1)
	if len(s) != 1 || s[0].PeerLoss < 0.029 || s[0].PeerLoss > 0.031 || s[0].BusyLinks != 1 || s[0].PathLimited < 0.79 {
		t.Fatalf("sample %+v", s)
	}
	// Idle link: not busy, no limited share; a path without enough
	// received segments keeps PeerLoss unknown.
	b := newCensorAcc(1)
	b.add(0, &qPrev{cen: censorPrev{peerOK: true, peerRt: 1, segsIn: 10}}, &qPrev{cen: censorPrev{peerOK: true}}, 5*time.Second)
	s = censorLBSample(nil, "10.0.0.1", "10.0.0.2", q2Reading{}, 0, b, 0)
	if s[0].PeerLoss != -1 || s[0].BusyLinks != 0 || s[0].PathLimited != -1 {
		t.Fatalf("idle sample %+v", s[0])
	}
	// Detector off: no accumulator, the old shape.
	s = censorLBSample(nil, "10.0.0.1", "10.0.0.2", q2Reading{}, 0, nil, 0)
	if s[0].PeerLoss != -1 || s[0].PathLimited != -1 {
		t.Fatalf("off sample %+v", s[0])
	}
}
