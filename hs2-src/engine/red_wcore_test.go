package engine

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// Red team (Z5 window core, part A): what the edge's runner hands the core
// from the exit's record and from smux's arm, at its most hostile -- a
// record with srtt 0 or 2^32-1 µs, a least round trip above srtt, resends
// that go backwards, wrap or outnumber the segments, records that repeat or
// come out of order; turnarounds of 0, of an hour, of an idle -- never
// panics the core and never gives a ceiling past MaxStreamBuffer, a
// negative or absurd reference, or a step that never comes back.
func TestRedWcoreHostileInputs(t *testing.T) {
	states := map[wcState]int{}
	ceils := 0
	for seed := int64(1); seed <= 40; seed++ {
		redWcoreRun(t, seed, states, &ceils)
	}
	t.Logf("states %v, %d ceilings set", states, ceils)
	if len(states) < 3 || ceils == 0 {
		t.Fatalf("the core hardly ran: states %v, %d ceilings", states, ceils)
	}
}

func redWcoreRun(t *testing.T, seed int64, states map[wcState]int, nCeil *int) {
	r := rand.New(rand.NewSource(seed))
	c := newWcore(wcConfig{side: wcEdge, stage: 1 + r.Intn(2), need: wtNeed, maxWin: wtMax, minWin: wtMin}, r.Float64)
	var b wcBuilder
	now := wtT0
	var snap wcLinkSnap
	snap.minRTT, snap.srtt = 100*time.Millisecond, 105*time.Millisecond
	type st struct {
		id   uint32
		read uint64
		upd  uint32
		hit  uint32
		win  uint32
	}
	streams := []*st{{id: 1, win: wtNeed}, {id: 3, win: wtNeed}}
	var sndRet, sndSegs uint64
	sndAt := now
	hostile := func() uint32 {
		switch r.Intn(6) {
		case 0:
			return 0
		case 1:
			return math.MaxUint32
		case 2:
			return 1
		default:
			return uint32(50_000 + r.Intn(500_000))
		}
	}
	turn := func() time.Duration {
		switch r.Intn(8) {
		case 0:
			return 0
		case 1:
			return time.Hour
		case 2:
			return time.Duration(1+r.Intn(30)) * time.Second // an idle stream's
		default:
			return time.Duration(100+r.Intn(400)) * time.Millisecond
		}
	}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("seed %d: the core panicked: %v", seed, p)
		}
	}()
	for step := 0; step < 600; step++ {
		now = now.Add(time.Duration(50+r.Intn(400)) * time.Millisecond)
		snap.now = now
		snap.rcvd += uint64(r.Intn(40 << 20))
		snap.segsIn += uint64(r.Intn(30000))
		snap.ooo += uint64(r.Intn(100))
		snap.oooOK = r.Intn(5) > 0
		if r.Intn(10) == 0 {
			snap.minRTT = time.Duration(hostile()) * time.Microsecond
			snap.srtt = time.Duration(hostile()) * time.Microsecond
		}
		snap.poolFree = int64(r.Intn(128 << 20))
		if r.Intn(4) == 0 { // a record from the exit
			switch r.Intn(5) {
			case 0: // backwards (a reset counter, a wrap)
				sndRet, sndSegs = uint64(r.Intn(100)), uint64(r.Intn(100))
			case 1: // resends past the segments
				sndRet += uint64(r.Intn(1 << 20))
				sndSegs += uint64(r.Intn(2))
			default:
				sndSegs += uint64(r.Intn(100000))
				sndRet += uint64(r.Intn(2000))
			}
			switch r.Intn(4) {
			case 0: // the same record again
			case 1: // an older one
				sndAt = sndAt.Add(-time.Second)
			default:
				sndAt = now
			}
			snap.sndAt = sndAt
			snap.sndSrtt = time.Duration(hostile()) * time.Microsecond
			snap.sndMinRTT = time.Duration(hostile()) * time.Microsecond
			snap.sndRetrans, snap.sndSegsOut = sndRet, sndSegs
		}
		snap.pongs = snap.pongs[:0]
		for k := r.Intn(3); k > 0; k-- {
			snap.pongs = append(snap.pongs, turn())
		}
		snap.ctrlMin = time.Duration(hostile()) * time.Microsecond
		if r.Intn(30) == 0 && len(streams) > 0 { // a stream goes, another comes
			streams = streams[1:]
			streams = append(streams, &st{id: uint32(5 + 2*step), win: wtNeed})
		}
		snap.streams = snap.streams[:0]
		for _, s := range streams {
			s.read += uint64(r.Intn(20 << 20))
			s.upd += uint32(r.Intn(20))
			s.hit += uint32(r.Intn(20))
			if c.ceils[s.id] > 0 && r.Intn(2) == 0 {
				s.win = c.ceils[s.id]
			}
			snap.streams = append(snap.streams, wcStreamSnap{id: s.id, readTot: s.read, updN: s.upd, hitN: s.hit,
				turnMin: turn(), turnLast: turn(), win: s.win, braked: r.Intn(20) == 0})
		}
		snap.parks += uint32(r.Intn(2))
		snap.brakes += uint32(r.Intn(2))
		in := b.build(snap)
		out := c.step(in)
		states[out.state]++
		for _, x := range out.ceils {
			if x.ceil > 0 {
				*nCeil++
			}
			if x.ceil > wtMax {
				t.Fatalf("seed %d step %d: ceiling %d past MaxStreamBuffer (state %v)", seed, step, x.ceil, out.state)
			}
		}
		if out.turnRef < 0 || out.turnRef > time.Hour || out.qBrake < 0 || out.qBrake > time.Hour {
			t.Fatalf("seed %d step %d: reference %v, brake %v", seed, step, out.turnRef, out.qBrake)
		}
		if out.B > uint64(len(in.streams)+1)*wtMax {
			t.Fatalf("seed %d step %d: budget %d for %d streams", seed, step, out.B, len(in.streams))
		}
		if !out.next.After(now) || out.next.Sub(now) > time.Minute {
			t.Fatalf("seed %d step %d: next step %v after this one", seed, step, out.next.Sub(now))
		}
	}
}
