package engine

import (
	"testing"
	"time"
)

func TestCtrlMtuWire135(t *testing.T) {
	v := ctrlJoinRetrans(123456, 1500, 1448)
	rt, pm, ms := ctrlSplitRetrans(v)
	if rt != 123456 || pm != 1500 || ms != 1448 {
		t.Fatalf("split %d %d %d", rt, pm, ms)
	}
	// an older exit: the plain counter, no MTU
	if rt, pm, _ := ctrlSplitRetrans(77); rt != 77 || pm != 0 {
		t.Fatalf("old pong %d %d", rt, pm)
	}
	// the ping: seq survives the mask with the idle and MTU bits set
	wire := uint64(42) | ctrlIdleBit | ctrlMtuBit | uint64(1320)<<ctrlMtuShift
	if wire&ctrlSeqMask != 42 || uint32(wire>>ctrlMtuShift)&0xFFFF != 1320 || wire&ctrlIdleBit == 0 {
		t.Fatalf("ping wire %x", wire)
	}
}

func TestPmtuTrackDrop135(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := newPmtuTrack()
	tr.now = func() time.Time { return now }
	rs := func(p ...uint32) []pmtuReading {
		out := make([]pmtuReading, len(p))
		for i, x := range p {
			out[i] = pmtuReading{key: i, pmtu: x, mss: x - 52}
		}
		return out
	}
	v, drop := tr.observe("a", rs(1500, 1500, 1500, 1500))
	if drop || v.Med != 1500 || v.Usual != 1500 || v.Low != 0 || v.MssMed != 1448 {
		t.Fatalf("first %+v %v", v, drop)
	}
	// one link lowers: no drop event (below pmtuDropMin), but it is low
	now = now.Add(5 * time.Second)
	v, drop = tr.observe("a", rs(1500, 1500, 1500, 1300))
	if drop || v.Low != 1 || v.Min != 1300 {
		t.Fatalf("one %+v %v", v, drop)
	}
	// three of four lower: a drop
	now = now.Add(5 * time.Second)
	v, drop = tr.observe("a", rs(1300, 1300, 1500, 1300))
	if !drop || v.Drops != 1 || v.Med != 1300 || v.Usual != 1500 || v.Low != 3 || !v.RecentDrop() {
		t.Fatalf("drop %+v %v", v, drop)
	}
	// stable at the new value: no new drop; after the window it is the usual
	now = now.Add(5 * time.Second)
	if _, drop = tr.observe("a", rs(1300, 1300, 1500, 1300)); drop {
		t.Fatal("stable read as a drop")
	}
	now = now.Add(pmtuUsualWin + time.Minute)
	v, _ = tr.observe("a", rs(1300, 1300, 1300, 1300))
	if v.Usual != 1300 || v.Low != 0 || v.RecentDrop() {
		t.Fatalf("settled %+v", v)
	}
	// a second fall within pmtuDropGap is the same event
	tr.observe("c", rs(1500, 1500, 1500, 1500))
	now = now.Add(time.Second)
	if _, drop = tr.observe("c", rs(1300, 1300, 1300, 1300)); !drop {
		t.Fatal("no first drop")
	}
	now = now.Add(time.Second)
	if _, drop = tr.observe("c", rs(1200, 1200, 1200, 1200)); drop {
		t.Fatal("drop inside the gap")
	}
	now = now.Add(pmtuDropGap)
	if v, drop = tr.observe("c", rs(1100, 1100, 1100, 1100)); !drop || v.Drops != 2 {
		t.Fatalf("second drop %+v %v", v, drop)
	}
	// no readings (or a socket's 68 placeholder): nil
	if v, _ := tr.observe("b", rs(0, 68)); v != nil {
		t.Fatalf("no reading %+v", v)
	}
}

// The view puts this side's sends and the peer's in the right direction.
func TestPathViewPmtu135(t *testing.T) {
	for _, exit := range []bool{false, true} {
		v := newPathView(exit)
		l := pathLink{key: 1, path: "x", tsOK: true, ts: tcpStat{pmtu: 1500, sndMss: 1448}, peerPmtu: 1280, peerMss: 1228}
		rows, _ := v.build([]LBPathView{{key: "x", State: "ok"}}, []pathLink{l})
		r := rows[0]
		if r.PmtuDown == nil || r.PmtuUp == nil || r.PmtuMin != 1280 {
			t.Fatalf("exit=%v %+v", exit, r)
		}
		own, peer := r.PmtuUp, r.PmtuDown
		if exit {
			own, peer = r.PmtuDown, r.PmtuUp
		}
		if own.Med != 1500 || own.MssMed != 1448 || peer.Med != 1280 {
			t.Fatalf("exit=%v own %+v peer %+v", exit, own, peer)
		}
	}
}

func TestEffMtu135(t *testing.T) {
	for _, c := range []struct {
		pmtu, mss uint32
		v6        bool
		want      uint32
	}{{1500, 1448, false, 1500}, {1500, 1428, true, 1500}, {1300, 1248, false, 1300}, {1500, 1024, true, 1096}, {1500, 1460, false, 1500}, {1500, 0, false, 1500}} {
		if got := effMtu(c.pmtu, c.mss, c.v6); got != c.want {
			t.Fatalf("effMtu(%d,%d,%v)=%d want %d", c.pmtu, c.mss, c.v6, got, c.want)
		}
	}
}
