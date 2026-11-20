package engine

import (
	"testing"
	"time"
)

// slot 4: the reverse edge's read-only path view (censor351_edge.go).
func TestCensorEdgeTickAccumulates(t *testing.T) {
	m := &LinkManager{}
	ml := &managedLink{mtr: &linkMeter{path: "10.0.0.1>10.1.0.1"}}
	ml.mtr.peerSeen.Store(true)
	t0 := time.Unix(1_800_000_000, 0)
	st := tcpStat{segsOut: 1000, retrans: 10, bytesAcked: 1 << 20, segsIn: 5000, bytesRcvd: 1 << 22}
	m.censorEdgeTick(t0, []censorEdgeObs{{ml, st}})
	a := m.cen.acc[censorPair{"10.0.0.1", "10.1.0.1"}]
	if a == nil || a.segs != 0 {
		t.Fatalf("first tick only stores prev: %+v", a)
	}
	ml.mtr.peerRetrans.Store(40)
	st2 := st
	st2.segsOut, st2.retrans, st2.bytesAcked, st2.segsIn, st2.bytesRcvd = 1400, 30, 3<<20, 7000, 1<<23
	m.censorEdgeTick(t0.Add(2*time.Second), []censorEdgeObs{{ml, st2}})
	if a.segs != 400 || a.retr != 20 || a.cp.busy != 1 {
		t.Fatalf("acc %+v", a)
	}
	p, ok := censorEdgeSample(censorPair{"10.0.0.1", "10.1.0.1"}, a, t0.Add(5*time.Second))
	if !ok || p.Loss != 0.05 || p.Silent || p.RateBps <= 0 || p.Local.String() != "10.0.0.1" || p.Remote.String() != "10.1.0.1" {
		t.Fatalf("sample %+v ok=%v", p, ok)
	}
	// peerRetrans started unseen in prev (0 → 40 is the first diff with
	// peerOK on both): 40 / 2000 received.
	if p.PeerLoss < 0.019 || p.PeerLoss > 0.021 {
		t.Fatalf("peer loss %v", p.PeerLoss)
	}
	// After the period the sample resets the pair; a vanished link is dropped.
	m.censorEdgeTick(t0.Add(6*time.Second), []censorEdgeObs{{ml, st2}})
	if a2 := m.cen.acc[censorPair{"10.0.0.1", "10.1.0.1"}]; a2 == nil || a2.segs != 0 {
		t.Fatalf("not reset after the sample: %+v", a2)
	}
	m.censorEdgeTick(t0.Add(12*time.Second), nil)
	if len(m.cen.prev) != 0 {
		t.Fatalf("prev of a dead link kept: %d", len(m.cen.prev))
	}
}

func TestCensorEdgeSampleSilentAndFloor(t *testing.T) {
	k := censorPair{"10.0.0.1", "10.1.0.1"}
	t0 := time.Unix(1_800_000_000, 0)
	// Data waiting, retransmits going out, nothing acked: silent.
	a := &censorEdgeAcc{since: t0, segs: 12, retr: 12, busyWait: true}
	p, ok := censorEdgeSample(k, a, t0.Add(5*time.Second))
	if !ok || !p.Silent || p.Loss != 1 {
		t.Fatalf("silent %+v ok=%v", p, ok)
	}
	// Too few segments, acked: no sample yet; reset after censorEdgeMax.
	a = &censorEdgeAcc{since: t0, segs: 10, acked: 5000}
	if _, ok := censorEdgeSample(k, a, t0.Add(5*time.Second)); ok || a.segs != 10 {
		t.Fatalf("floor: ok=%v %+v", ok, a)
	}
	if _, ok := censorEdgeSample(k, a, t0.Add(censorEdgeMax)); ok || a.segs != 0 {
		t.Fatalf("max: ok=%v %+v", ok, a)
	}
}

// Key off: the health tick's edge pass never runs, nothing is allocated.
func TestCensorEdgeOffNoState(t *testing.T) {
	m := NewLinkManager(nil, 1, 4, 0, func(string, ...any) {})
	m.accept = true
	m.sampleHealth()
	if m.cen != nil {
		t.Fatal("edge state allocated with the detector off")
	}
}
