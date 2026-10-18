package engine

import (
	"math"
	"math/rand"
	"sync/atomic"
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

// A user whose app downloads fast under a ceiling of SmuxStreamBuffer (its
// window grows past SmuxNeedStreamBuffer, its claim on the window pool with
// it) and then freezes holds that claim, and the link keeps moving; once
// frozen apps fill the link's grown buffer too, the wedge guard closes
// every stuck relay, the frozen fast one with them, and the whole pool
// comes back.
func TestRedFrozenFastAppThenGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time readers")
	}
	w := Wcore
	Wcore = 0 // this test is the core: no runner to reset its ceilings
	defer func() { Wcore = w }()
	free := windowPool().Free()
	r := newWedgeRig(t, true)
	var stop atomic.Bool
	fast := r.open(func(u *wUser) {
		buf := make([]byte, 64<<10)
		for !stop.Load() {
			n, err := u.app.Read(buf)
			u.got.Add(int64(n))
			if err != nil {
				return
			}
		}
		<-u.ended // frozen: never reads again
	})
	r.edge.SetCoreRef(0, 0, true)
	var win uint32
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end) && win < 8<<20; time.Sleep(20 * time.Millisecond) {
		s, _ := r.edge.CoreSnapshot(nil)
		for _, x := range s {
			x.Stream.SetCoreCeil(uint32(SmuxStreamBuffer))
			win = max(win, x.Win)
		}
	}
	if win < 8<<20 {
		t.Fatalf("the fast download's window grew to %d only", win)
	}
	stop.Store(true)
	time.Sleep(time.Second) // what its window let in arrives
	held := r.edge.Extra()
	if held <= 0 {
		t.Fatalf("a frozen app past its fast start holds no claim (extra %d)", held)
	}
	healthy := r.open(readAt(0))
	time.Sleep(500 * time.Millisecond)
	before := healthy.got.Load()
	time.Sleep(time.Second)
	if healthy.got.Load()-before < 4<<20 {
		t.Fatalf("healthy user moved %d KiB in 1 s next to the frozen fast app", (healthy.got.Load()-before)>>10)
	}
	for i := 0; i < 4; i++ {
		if k := r.lookFast(); k != 0 {
			t.Fatalf("guard closed %d relays while the link flowed", k)
		}
	}
	t.Logf("frozen fast app: window %d MiB, link extra %d MiB, pool %d of %d MiB", win>>20, held>>20, windowPool().Free()>>20, free>>20)
	stuck := r.openFrozen(150)
	killed := 0
	for i := 0; i < 60 && !ended(fast); i++ {
		killed += r.lookFast()
	}
	if !ended(fast) {
		t.Fatalf("the frozen fast app's relay is still open (guard closed %d)", killed)
	}
	for i := 0; i < 40 && killed < len(stuck)+1; i++ {
		killed += r.lookFast()
	}
	if ended(healthy) {
		t.Fatal("the healthy relay was closed")
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.edge.Extra() != 0 || windowPool().Free() != free {
		if time.Now().After(deadline) {
			t.Fatalf("after the release: buffer %d past SmuxSessionBuffer, pool %d of %d", r.edge.Extra(), windowPool().Free(), free)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("guard closed %d relays", killed)
}
