package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"testing"
	"time"
)

// C2's model tests (REDESIGN §3.2): the scheduler over a synthetic socket
// that takes R bytes/s, in virtual time. The socket's writer takes the next
// item (Next) when the one before is out; a bulk source keeps one item
// queued (smux: one frame pending per stream, the next one there as soon as
// the scheduler takes it), a paced one pushes at its rate, a light one at
// random (exponential gaps); Tick each 100 ms with the link's rate R.

type simKind uint8

const (
	simBacklog simKind = iota
	simPaced
	simRandom
)

type simFlow struct {
	user        uint32
	id          uint64
	kind        simKind
	lane        Lane
	size        int
	rate        float64       // simPaced: bytes/s
	gap         time.Duration // simRandom: the mean gap
	start, stop time.Duration // stop 0: never
	next        time.Duration
	seq, got    int // items pushed; the next one due out (order)
}

type simItem struct {
	f      *simFlow
	seq    int
	at     time.Duration // pushed
	size   int
	inProg int // the frame being written when it was pushed (0: none)
}

type simServe struct {
	f    *simFlow
	at   time.Duration // written from
	size int
	wait time.Duration
	// bound: what the item may wait (simRandom latency and control items:
	// a bulk frame and the frame in progress when it came, at R)
	bound time.Duration
}

type schedSim struct {
	t      *testing.T
	s      *Sched[simItem]
	r      float64 // bytes/s
	rnd    *rand.Rand
	flows  []*simFlow
	served []simServe
	// ctrlNext: each Next while a control item waits must take it
	ctrlQ int
}

func newSchedSim(t *testing.T, rateMbit float64, seed uint64) *schedSim {
	return &schedSim{t: t, s: NewSched[simItem](), r: rateMbit * 1e6 / 8, rnd: rand.New(rand.NewPCG(seed, seed^0xc2))}
}

func (m *schedSim) add(f simFlow) *simFlow {
	if f.id == 0 {
		f.id = uint64(len(m.flows) + 1)
	}
	if f.size == 0 {
		f.size = BulkFrameSize(m.r)
	}
	f.next = f.start
	m.flows = append(m.flows, &f)
	return m.flows[len(m.flows)-1]
}

func (m *schedSim) dur(n int) time.Duration {
	return time.Duration(float64(n) / m.r * float64(time.Second))
}

func (m *schedSim) push(f *simFlow, now time.Duration, inProg int) {
	f.seq++
	m.s.Push(simItem{f: f, seq: f.seq, at: now, size: f.size, inProg: inProg}, f.user, f.id, f.lane, f.size)
	if f.lane == LaneCtrl {
		m.ctrlQ++
	}
}

// run runs the link until end.
func (m *schedSim) run(end time.Duration) {
	var (
		now       time.Duration
		busyUntil time.Duration
		busy      bool
		inProg    int
		nextTick  = schedTick
	)
	for {
		te := nextTick
		if busy && busyUntil < te {
			te = busyUntil
		}
		for _, f := range m.flows {
			if f.kind != simBacklog && f.next < te && (f.stop == 0 || f.next < f.stop) {
				te = f.next
			}
			if f.kind == simBacklog && f.seq == 0 && f.start < te {
				te = f.start
			}
		}
		if te >= end {
			return
		}
		now = te
		if busy && now >= busyUntil {
			busy, inProg = false, 0
		}
		for _, f := range m.flows {
			switch {
			case f.stop != 0 && now >= f.stop:
			case f.kind == simBacklog:
				if f.seq == 0 && now >= f.start {
					m.push(f, now, inProg)
				}
			default:
				for f.next <= now && (f.stop == 0 || f.next < f.stop) {
					m.push(f, now, inProg)
					if f.kind == simPaced {
						f.next += time.Duration(float64(f.size) / f.rate * float64(time.Second))
					} else {
						f.next += time.Duration(m.rnd.ExpFloat64() * float64(f.gap))
					}
				}
			}
		}
		if now >= nextTick {
			m.s.Tick(now, m.r)
			nextTick += schedTick
		}
		if busy {
			continue
		}
		it, ok := m.s.Next()
		if !ok {
			if m.ctrlQ > 0 {
				m.t.Fatalf("%v: a control item waits and Next has nothing", now)
			}
			continue
		}
		f := it.f
		if m.ctrlQ > 0 {
			if f.lane != LaneCtrl {
				m.t.Fatalf("%v: a control item waits and Next took %v of flow %d", now, f.lane, f.id)
			}
			m.ctrlQ--
		}
		if it.seq != f.got+1 {
			m.t.Fatalf("%v: flow %d out of order: item %d after %d", now, f.id, it.seq, f.got)
		}
		f.got = it.seq
		sv := simServe{f: f, at: now, size: it.size, wait: now - it.at}
		if f.kind == simRandom {
			sv.bound = m.dur(BulkFrameSize(m.r) + it.inProg)
		}
		m.served = append(m.served, sv)
		busy, inProg, busyUntil = true, it.size, now+m.dur(it.size)
		if f.kind == simBacklog && (f.stop == 0 || now < f.stop) {
			m.push(f, now, inProg)
		}
	}
}

// rate is what the flows for which pick says true got over [a, b), bytes/s.
func (m *schedSim) rate(a, b time.Duration, pick func(*simFlow) bool) float64 {
	n := 0
	for _, s := range m.served {
		if s.at >= a && s.at < b && pick(s.f) {
			n += s.size
		}
	}
	return float64(n) / (b - a).Seconds()
}

func ofUser(u uint32) func(*simFlow) bool { return func(f *simFlow) bool { return f.user == u } }

// waits checks every bounded item's wait and returns its p99 and max.
func (m *schedSim) waits(name string) (p99, worst time.Duration) {
	var w []time.Duration
	for _, s := range m.served {
		if s.bound == 0 {
			continue
		}
		if s.wait > s.bound {
			m.t.Fatalf("%s: a light item waited %v at %v, more than %v (a bulk frame and the frame in progress)", name, s.wait, s.at, s.bound)
		}
		w = append(w, s.wait)
	}
	if len(w) == 0 {
		m.t.Fatalf("%s: no light item went out", name)
	}
	sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
	return w[len(w)*99/100], w[len(w)-1]
}

// TestSchedLightWait: behind two bulk users of four flows each (frames of
// BulkFrameSize(R)), every light frame of five light users waits no more
// than a bulk frame and the frame in progress when it came, at R.
func TestSchedLightWait(t *testing.T) {
	for _, mbit := range []float64{1, 10, 100, 1000} {
		m := newSchedSim(t, mbit, 1)
		for u := uint32(1); u <= 2; u++ {
			for range 4 {
				m.add(simFlow{user: u, kind: simBacklog, lane: LaneBulk})
			}
		}
		for u := uint32(10); u < 15; u++ {
			m.add(simFlow{user: u, kind: simRandom, lane: LaneLatency, size: 200, gap: 50 * time.Millisecond})
		}
		m.run(10 * time.Second)
		p99, worst := m.waits(fmt.Sprintf("%.0f Mbit/s", mbit))
		t.Logf("%.0f Mbit/s: light wait p99 %v max %v (bulk frame %d B = %v)", mbit, p99, worst, BulkFrameSize(m.r), m.dur(BulkFrameSize(m.r)))
	}
}

// TestSchedLoneUser: a user alone takes the link, with 1 flow and with 16.
func TestSchedLoneUser(t *testing.T) {
	for _, n := range []int{1, 16} {
		m := newSchedSim(t, 100, 1)
		for range n {
			m.add(simFlow{user: 1, kind: simBacklog, lane: LaneBulk})
		}
		m.run(5 * time.Second)
		if r := m.rate(500*time.Millisecond, 5*time.Second, ofUser(1)); r < 0.95*m.r {
			t.Fatalf("%d flows: %.1f%% of the link", n, 100*r/m.r)
		}
	}
}

// TestSchedOneVsSixteen: a user of one flow next to one of sixteen keeps
// 0.9 of its floor min(D_u, R/2), the other takes the rest, and the two
// fill the link; with the one-flow user's demand at 0.3 R too.
func TestSchedOneVsSixteen(t *testing.T) {
	for _, demand := range []float64{0, 0.3} { // 0: as much as it can
		m := newSchedSim(t, 100, 1)
		a := simFlow{user: 1, kind: simBacklog, lane: LaneBulk}
		floor := m.r / 2
		if demand > 0 {
			a.kind, a.rate = simPaced, demand*m.r
			floor = demand * m.r
		}
		m.add(a)
		for range 16 {
			m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
		}
		m.run(10 * time.Second)
		from, to := time.Second, 10*time.Second
		ra, rb := m.rate(from, to, ofUser(1)), m.rate(from, to, ofUser(2))
		t.Logf("demand %.1f: one flow %.1f%%, sixteen %.1f%% of the link", demand, 100*ra/m.r, 100*rb/m.r)
		if ra < 0.9*floor {
			t.Fatalf("demand %.1f: the one-flow user got %.1f%% of the link, its floor is %.1f%%", demand, 100*ra/m.r, 100*floor/m.r)
		}
		if ra+rb < 0.97*m.r {
			t.Fatalf("demand %.1f: the two got %.1f%% of the link", demand, 100*(ra+rb)/m.r)
		}
	}
}

// TestSchedLowDemandFreesShare: a user that asks for 0.1 R gets that and
// no more; the rest goes to the others, two of 1 and 4 flows: each keeps
// its floor R/3, and what is past the floors goes by flows (4 to 1).
func TestSchedLowDemandFreesShare(t *testing.T) {
	m := newSchedSim(t, 100, 1)
	m.add(simFlow{user: 1, kind: simPaced, lane: LaneBulk, rate: 0.1 * m.r})
	m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
	for range 4 {
		m.add(simFlow{user: 3, kind: simBacklog, lane: LaneBulk})
	}
	m.run(10 * time.Second)
	from, to := time.Second, 10*time.Second
	ra, rb, rc := m.rate(from, to, ofUser(1)), m.rate(from, to, ofUser(2)), m.rate(from, to, ofUser(3))
	t.Logf("low demand %.1f%%, 1 flow %.1f%%, 4 flows %.1f%% of the link", 100*ra/m.r, 100*rb/m.r, 100*rc/m.r)
	if ra < 0.09*m.r || ra > 0.11*m.r {
		t.Fatalf("the user asking for 10%% got %.1f%%", 100*ra/m.r)
	}
	if rb < 0.9*m.r/3 || rc < 0.9*m.r/3 {
		t.Fatalf("a floor (R/3) not kept: %.1f%% and %.1f%%", 100*rb/m.r, 100*rc/m.r)
	}
	if ra+rb+rc < 0.97*m.r {
		t.Fatalf("the link %.1f%% full", 100*(ra+rb+rc)/m.r)
	}
	// the surplus over the floors, by flows: 4 to 1
	if x := (rc - m.r/3) / (rb - m.r/3); x < 2.5 || x > 6 {
		t.Fatalf("the surplus went %.1f to 1 between 4 flows and 1", x)
	}
}

// TestSchedNewHeavyUser: a user of 16 flows that comes 3 s in takes
// neither the light users' bound nor the floor of the bulk user already
// there (R/2 from then, in every 200 ms).
func TestSchedNewHeavyUser(t *testing.T) {
	for _, mbit := range []float64{10, 100} {
		m := newSchedSim(t, mbit, 2)
		m.add(simFlow{user: 1, kind: simBacklog, lane: LaneBulk})
		for u := uint32(10); u < 15; u++ {
			m.add(simFlow{user: u, kind: simRandom, lane: LaneLatency, size: 300, gap: 40 * time.Millisecond})
		}
		for range 16 {
			m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk, start: 3 * time.Second})
		}
		m.run(10 * time.Second)
		p99, worst := m.waits(fmt.Sprintf("%.0f Mbit/s", mbit))
		least := math.Inf(1)
		for a := 3 * time.Second; a+2*schedTick <= 10*time.Second; a += 2 * schedTick {
			least = math.Min(least, m.rate(a, a+2*schedTick, ofUser(1)))
		}
		t.Logf("%.0f Mbit/s: light p99 %v max %v; the bulk user's least 200 ms after: %.1f%% of the link", mbit, p99, worst, 100*least/m.r)
		if least < 0.9*m.r/2 {
			t.Fatalf("%.0f Mbit/s: the bulk user had %.1f%% of the link in a 200 ms, its floor is 50%%", mbit, 100*least/m.r)
		}
	}
}

// TestSchedCtrlNeverBehindData: a control item is always next, whatever is
// queued in the lanes, in the order they came.
func TestSchedCtrlNeverBehindData(t *testing.T) {
	s := NewSched[int]()
	rnd := rand.New(rand.NewPCG(3, 4))
	ctrl, nextCtrl := 0, 0 // pushed, next due
	s.Tick(0, 1e6)
	for i := range 20000 {
		switch rnd.IntN(4) {
		case 0:
			s.Push(-1-ctrl, uint32(rnd.IntN(5)), uint64(rnd.IntN(20)), LaneCtrl, 64)
			ctrl++
		case 1:
			s.Push(i, uint32(rnd.IntN(5)), uint64(rnd.IntN(20)), LaneLatency, 300)
		default:
			s.Push(i, uint32(rnd.IntN(5)), uint64(rnd.IntN(20)), LaneBulk, 1+rnd.IntN(16<<10))
		}
		if i%100 == 0 {
			s.Tick(time.Duration(i)*time.Millisecond, 1e6)
		}
		for range rnd.IntN(3) {
			it, ok := s.Next()
			if !ok {
				break
			}
			if nextCtrl < ctrl {
				if it != -1-nextCtrl {
					t.Fatalf("control item %d waits and Next took %d", nextCtrl, it)
				}
				nextCtrl++
			} else if it < 0 {
				t.Fatalf("control item %d again", -1-it)
			}
		}
	}
}

// TestSchedLaneCredit: a user that sends light frames at 400 KiB/s gets
// 160 KiB/s and a burst of 32 KiB of them in the latency lane; the rest
// goes in its bulk ring, in order, and none is lost.
func TestSchedLaneCredit(t *testing.T) {
	m := newSchedSim(t, 100, 1)
	l := m.add(simFlow{user: 1, kind: simPaced, lane: LaneLatency, size: 1 << 10, rate: 400 << 10})
	m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
	const end = 3 * time.Second
	// the latency lane's bytes, as Stats counts them, at each item of l
	var lane uint64
	before := m.s.Stats().Served[LaneLatency]
	m.run(end)
	lane = m.s.Stats().Served[LaneLatency] - before
	maxLane := float64(schedLaneBurst) + schedLaneRate*end.Seconds()
	got := 0
	for _, s := range m.served {
		if s.f == l {
			got++
		}
	}
	t.Logf("latency lane %d B (at most %.0f), %d of %d light frames out, %d queued as bulk", lane, maxLane, got, l.seq, m.s.Stats().Demoted)
	if float64(lane) > maxLane {
		t.Fatalf("the latency lane took %d B in %v, its credit is %.0f", lane, end, maxLane)
	}
	if m.s.Stats().Demoted == 0 {
		t.Fatal("nothing past the credit went in the bulk ring")
	}
	if got < l.seq-8 { // (the last few may still be queued at the end)
		t.Fatalf("%d of %d light frames went out", got, l.seq)
	}
}

// TestSchedThousandStreamsUser: a user of 1000 flows next to one of a
// single flow: the single flow keeps its floor (R/2), and control items
// wait for nothing but the frame in progress.
func TestSchedThousandStreamsUser(t *testing.T) {
	m := newSchedSim(t, 100, 1)
	for range 1000 {
		m.add(simFlow{user: 1, kind: simBacklog, lane: LaneBulk})
	}
	m.add(simFlow{user: 2, kind: simBacklog, lane: LaneBulk})
	c := m.add(simFlow{user: 3, kind: simRandom, lane: LaneCtrl, size: 64, gap: 10 * time.Millisecond})
	m.run(10 * time.Second)
	rb := m.rate(time.Second, 10*time.Second, ofUser(2))
	worst := time.Duration(0)
	for _, s := range m.served {
		if s.f == c {
			if lim := m.dur(s.size + BulkFrameSize(m.r)); s.wait > lim {
				t.Fatalf("a control item waited %v, more than the frame in progress (%v)", s.wait, lim)
			}
			worst = max(worst, s.wait)
		}
	}
	t.Logf("the single flow: %.1f%% of the link; control wait max %v", 100*rb/m.r, worst)
	if rb < 0.9*m.r/2 {
		t.Fatalf("the single flow next to 1000 got %.1f%% of the link, its floor is 50%%", 100*rb/m.r)
	}
}

// TestBulkFrameSize: 5 ms of the link's rate within 1 and 16 KiB.
func TestBulkFrameSize(t *testing.T) {
	for _, c := range []struct {
		mbit float64
		want int
	}{{0, 1024}, {1, 1024}, {10, 6250}, {26, 16250}, {27, 16384}, {100, 16384}, {1000, 16384}} {
		if got := BulkFrameSize(c.mbit * 1e6 / 8); got != c.want {
			t.Fatalf("%.0f Mbit/s: %d, want %d", c.mbit, got, c.want)
		}
	}
	if got := BulkFrameSize(math.NaN()); got != 1024 {
		t.Fatalf("an unknown rate: %d", got)
	}
}

// TestSchedOffSwitch: HS2_TUNE_SCHED=0 (TuneSched 0) says off.
func TestSchedOffSwitch(t *testing.T) {
	defer func(v int) { TuneSched = v }(TuneSched)
	if SchedOff() && os.Getenv("HS2_TUNE_SCHED") == "" { // (TestMain applies it)
		t.Fatal("off by default")
	}
	TuneSched = 0
	if !SchedOff() {
		t.Fatal("TuneSched 0 and SchedOff false")
	}
}

// BenchmarkSchedNext: one Next and the push that keeps its flow queued
// (smux's next frame), at 10 and 10000 flows (10 flows a user); a Tick each
// 4096 ops. The cost must not grow with the flows.
func BenchmarkSchedNext(b *testing.B) {
	for _, n := range []int{10, 10000} {
		b.Run(fmt.Sprintf("flows=%d", n), func(b *testing.B) {
			s := NewSched[uint64]()
			for i := range n {
				s.Push(uint64(i), uint32(i/10), uint64(i), LaneBulk, 16<<10)
			}
			now := time.Duration(0)
			s.Tick(now, 125e6)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				it, ok := s.Next()
				if !ok {
					b.Fatal("empty")
				}
				s.Push(it, uint32(it/10), it, LaneBulk, 16<<10)
				if i&4095 == 4095 {
					now += schedTick
					s.Tick(now, 125e6)
				}
			}
		})
	}
}
