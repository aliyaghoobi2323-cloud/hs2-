package engine

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// Unit tests of the pure core (wcore.go): its helpers, estimators, loss
// classification and the state machine's transitions, one decision at a
// time. The link model (wcore_model_test.go) runs it whole.

const (
	wtNeed = 2 << 20
	wtMax  = 16 << 20
	wtMin  = 64 << 10
	wtMiB  = 1 << 20
)

var wtT0 = time.Unix(1_700_000_000, 0)

func wtCore(stage int) *wcore {
	c := newWcore(wcConfig{side: wcEdge, stage: stage, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
	c.tBase = 100 * time.Millisecond
	return c
}

// wtIn is an interval with n fast-looking streams of window w (no pool).
func wtIn(now time.Time, n int, w uint32) wcIn {
	in := wcIn{now: now, poolFree: -1}
	for i := 0; i < n; i++ {
		in.streams = append(in.streams, wcStream{id: uint32(2*i + 1), win: w, upd: 4, hit: 4, read: 1})
	}
	return in
}

// wtMeas is an interval in which the n streams of wtIn are F, read rF
// bytes/s together and hold wF of window (as at the previous snapshot).
func wtMeas(n int, rF, wF float64) wcMeas {
	m := wcMeas{dt: 1, sameF: true, rF: rF, rLink: rF, wF: wF, wPrev: wF, hit: 1}
	for i := 0; i < n; i++ {
		m.f = append(m.f, i)
		m.hStep += float64(wcStep(wtNeed, uint32(wF)/uint32(n))) / 2
	}
	return m
}

// wtInF marks the streams of in as F.
func wtInF(c *wcore, in wcIn) {
	for _, s := range in.streams {
		c.inF[s.id] = true
	}
}

func TestWcoreStepOf(t *testing.T) {
	for _, tc := range []struct {
		need, w, want uint32
	}{
		{wtNeed, 0, 0},
		{wtNeed, 1 * wtMiB, wtMiB / 2},
		{wtNeed, 2 * wtMiB, wtMiB},      // at need: half
		{wtNeed, 3 * wtMiB, wtMiB},      // past it: need/2 ...
		{wtNeed, 8 * wtMiB, wtMiB},      // ... or w/8, whichever is more
		{wtNeed, 16 * wtMiB, 2 * wtMiB}, // w/8
		{wtNeed, 2*wtMiB + 2, wtMiB},    // never more than half
		{0, 4 * wtMiB, 2 * wtMiB},
	} {
		if got := wcStep(tc.need, tc.w); got != tc.want {
			t.Errorf("wcStep(%d, %d) = %d, want %d", tc.need, tc.w, got, tc.want)
		}
	}
	// the model's copy of smux.StepOf is the same function
	for w := uint32(0); w <= wtMax; w += 37 << 10 {
		if a, b := wcStep(wtNeed, w), modelStepOf(wtNeed, w); a != b {
			t.Fatalf("w %d: wcStep %d, modelStepOf %d", w, a, b)
		}
	}
}

func TestWcoreSplit(t *testing.T) {
	inf := math.Inf(1)
	for _, tc := range []struct {
		name   string
		budget uint64
		demand []float64
		lo, hi uint32
		want   []uint32
	}{
		{"equal", 8 * wtMiB, []float64{inf, inf, inf, inf}, wtNeed, wtMax, []uint32{2 * wtMiB, 2 * wtMiB, 2 * wtMiB, 2 * wtMiB}},
		{"a met demand frees the rest", 12 * wtMiB, []float64{inf, 2.5 * wtMiB, inf}, wtNeed, wtMax,
			[]uint32{19 * wtMiB / 4, 5 * wtMiB / 2, 19 * wtMiB / 4}},
		{"demand below the floor", 12 * wtMiB, []float64{1, inf}, wtNeed, wtMax, []uint32{2 * wtMiB, 10 * wtMiB}},
		{"capped at MaxStreamBuffer", 100 * wtMiB, []float64{inf, inf}, wtNeed, wtMax, []uint32{wtMax, wtMax}},
		{"budget under the floors", 3 * wtMiB, []float64{inf, inf}, wtNeed, wtMax, []uint32{3 * wtMiB / 2, 3 * wtMiB / 2}},
		{"none", wtMiB, nil, wtNeed, wtMax, []uint32{}},
	} {
		got := wcSplit(tc.budget, tc.demand, tc.lo, tc.hi)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %v", tc.name, got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
	// properties, on random inputs: the shares add up to at most the
	// budget; within [lo, hi] when the budget covers the floors; max-min
	// (a share below another one is its whole demand)
	r := rand.New(rand.NewPCG(7, 7))
	for k := 0; k < 2000; k++ {
		n := 1 + r.IntN(8)
		d := make([]float64, n)
		for i := range d {
			d[i] = inf
			if r.IntN(2) == 0 {
				d[i] = float64(r.IntN(20 * wtMiB))
			}
		}
		b := uint64(r.IntN(40 * wtMiB))
		s := wcSplit(b, d, wtNeed, wtMax)
		sum := uint64(0)
		for _, v := range s {
			sum += uint64(v)
		}
		if sum > b {
			t.Fatalf("budget %d, demands %v: shares %v add up to %d", b, d, s, sum)
		}
		for i, v := range s {
			want := math.Min(math.Max(d[i], wtNeed), wtMax)
			if v > wtMax || (b >= uint64(n)*wtNeed && v < wtNeed) {
				t.Fatalf("budget %d, demands %v: share %d out of [lo, hi]", b, d, v)
			}
			for j, u := range s {
				if v+1 < u && float64(v)+1 < want {
					t.Fatalf("budget %d, demands %v: share %d (%d) below %d (%d) with its demand unmet", b, d, v, i, u, j)
				}
			}
		}
	}
}

func TestWcoreLossRef(t *testing.T) {
	var r wcLossRef
	if r.past(1, 0) {
		t.Fatal("an empty level is past")
	}
	r.note(0.01, 4*wtMiB, 0.3)
	if !r.ok || r.v != 0.01 || r.b != 4*wtMiB {
		t.Fatalf("first note: %+v", r)
	}
	r.note(0.02, 4*wtMiB, 0.3) // the same budget: EWMA
	if math.Abs(r.v-0.013) > 1e-9 {
		t.Fatalf("same budget: %v, want 0.013", r.v)
	}
	r.note(0.043, 8*wtMiB, 0.3) // as clean at a larger budget: a third of the weight
	if math.Abs(r.v-0.016) > 1e-9 || r.b != 8*wtMiB {
		t.Fatalf("larger budget: %+v, want 0.016 at 8 MiB", r)
	}
	for _, tc := range []struct {
		v, x, abs float64
		want      bool
	}{
		{0.01, 0.019, 0.003, false}, // ×2 is the bar here
		{0.01, 0.02, 0.003, true},
		{0, 0.0029, 0.003, false}, // +abs is the bar here
		{0, 0.003, 0.003, true},
		{0.0005, 0.0034, 0.003, false},
		{0.0005, 0.0035, 0.003, true},
	} {
		ref := wcLossRef{v: tc.v, b: 1, ok: true}
		if got := ref.past(tc.x, tc.abs); got != tc.want {
			t.Errorf("level %v: past(%v, %v) = %v", tc.v, tc.x, tc.abs, got)
		}
	}
}

func TestWcoreLossVerdict(t *testing.T) {
	ref := func(v float64, b uint64) wcLossRef { return wcLossRef{v: v, b: b, ok: true} }
	for _, tc := range []struct {
		name       string
		snd, ooo   wcLossRef
		B          uint64
		m          wcMeas
		growth     bool
		cong, jump bool
		src        wcLossSrc
		histB      uint64 // a budget in force earlier in the record's span (0: none)
	}{
		{name: "random loss at its level", snd: ref(0.01, 4*wtMiB), B: 4 * wtMiB, m: wcMeas{snd: 0.012, sndOK: true}},
		{name: "past the level of a smaller budget", snd: ref(0.001, 4*wtMiB), B: 6 * wtMiB, m: wcMeas{snd: 0.01, sndOK: true},
			cong: true, src: wcSrcSnd},
		{name: "a larger budget earlier in the record", snd: ref(0.001, 4*wtMiB), B: 4 * wtMiB, histB: 6 * wtMiB,
			m: wcMeas{snd: 0.01, sndOK: true}, cong: true, src: wcSrcSnd},
		{name: "past it at the same budget", snd: ref(0.001, 4*wtMiB), B: 4 * wtMiB, m: wcMeas{snd: 0.01, sndOK: true},
			jump: true, src: wcSrcSnd},
		{name: "... with the queue growing", snd: ref(0.001, 4*wtMiB), B: 4 * wtMiB, m: wcMeas{snd: 0.01, sndOK: true},
			growth: true, cong: true, src: wcSrcSnd},
		{name: "no record this interval", snd: ref(0.001, 4*wtMiB), B: 6 * wtMiB, m: wcMeas{snd: 0.5}},
		{name: "ooo on a clean path", ooo: ref(0.001, 4*wtMiB), B: 6 * wtMiB, m: wcMeas{ooo: 0.05, oooOK: true},
			cong: true, src: wcSrcOoo},
		{name: "ooo on a lossy path says nothing", ooo: ref(0.2, 4*wtMiB), B: 6 * wtMiB, m: wcMeas{ooo: 0.6, oooOK: true}},
		{name: "ooo at the same budget", ooo: ref(0.001, 4*wtMiB), B: 4 * wtMiB, m: wcMeas{ooo: 0.05, oooOK: true},
			jump: true, src: wcSrcOoo},
		{name: "... with the queue growing", ooo: ref(0.001, 4*wtMiB), B: 4 * wtMiB, m: wcMeas{ooo: 0.05, oooOK: true},
			growth: true, cong: true, src: wcSrcOoo},
	} {
		c := wtCore(1)
		c.sndRef, c.oooRef, c.B = tc.snd, tc.ooo, tc.B
		if tc.histB > 0 {
			c.bHist = []wcRate{{wtT0, float64(tc.histB)}}
		}
		cong, jump, src := c.lossVerdict(tc.m, tc.growth)
		if cong != tc.cong || jump != tc.jump || src != tc.src {
			t.Errorf("%s: cong %v jump %v src %v, want %v %v %v", tc.name, cong, jump, src, tc.cong, tc.jump, tc.src)
		}
	}
}

func TestWcoreKneeAndHold(t *testing.T) {
	const r = 100e6 / 8 * 8 // 100 MB/s
	m := wtMeas(2, r, 0)
	m.hStep = wtMiB
	for _, tc := range []struct {
		name  string
		setup func(c *wcore)
		want  float64
	}{
		{"knee + h·R̂·T, h = qTarget/T", func(c *wcore) { c.bGood = 1 << 40 }, r*0.12 + wtMiB + 0.15*r*0.1},
		{"h at least wcHMin", func(c *wcore) { c.bGood = 1 << 40; c.tBase = time.Second }, r*1.02 + wtMiB + 0.03*r*1},
		{"h at most wcHMax", func(c *wcore) { c.bGood = 1 << 40; c.tBase = 40 * time.Millisecond }, r*0.06 + wtMiB + 0.25*r*0.04},
		{"Q_cap known: qTarget = Q_cap/3", func(c *wcore) {
			c.bGood = 1 << 40
			c.qcap, c.qcapAt, c.qcapOK = 30*time.Millisecond, wtT0, true
		}, r*0.12 + wtMiB + 0.1*r*0.1},
		{"capped where losses of ours came", func(c *wcore) { c.bGood = 1 << 40; c.bCap, c.bCapAt = 9e6, wtT0 }, 9e6},
		{"a stale cap is gone", func(c *wcore) { c.bGood = 1 << 40; c.bCap, c.bCapAt = 9e6, wtT0.Add(-wcQcapTTL) }, r*0.12 + wtMiB + 0.15*r*0.1},
		{"past what paid only by the margin", func(c *wcore) { c.bGood = 8e6 }, 8e6 * 1.15},
		{"never below the floor", func(c *wcore) { c.bGood = 0 }, 2 * wtNeed},
	} {
		c := wtCore(1)
		c.tau, c.tauOK = 20*time.Millisecond, true
		c.noteRate(wtT0, r)
		tc.setup(c)
		if got := float64(c.bHold(wtT0, m)); math.Abs(got-tc.want) > 2 {
			t.Errorf("%s: bHold %.0f, want %.0f", tc.name, got, tc.want)
		}
	}
	// the probe adds probeQ of queue at R̂ (Q_cap/2 if less, within
	// [wcGpMin, wcGpMax] of a round trip)
	c := wtCore(1)
	c.noteRate(wtT0, r)
	if got, want := c.bProbe(wtT0, m, 10e6), 10e6+0.2*r*0.1; math.Abs(float64(got)-want) > 2 {
		t.Errorf("bProbe %d, want %.0f", got, want)
	}
	c.qcap, c.qcapAt, c.qcapOK = 10*time.Millisecond, wtT0, true
	if got, want := c.bProbe(wtT0, m, 10e6), 10e6+0.05*r*0.1; math.Abs(float64(got)-want) > 2 {
		t.Errorf("bProbe with Q_cap 10 ms: %d, want %.0f", got, want)
	}
	c.qcap = 0
	if got, want := c.bProbe(wtT0, m, 10e6), 10e6+0.04*r*0.1; math.Abs(float64(got)-want) > 2 {
		t.Errorf("bProbe with Q_cap 0: %d, want %.0f", got, want)
	}
	if q := c.qTarget(wtT0.Add(wcQcapTTL)); q != wcQTarget {
		t.Errorf("Q_cap past its TTL still sets qTarget: %v", q)
	}
	x := wtCore(1)
	x.cfg.side = wcExit
	if x.qTarget(wtT0) != wcQTargetExit || x.probeQ(wtT0) != wcProbeQExit {
		t.Errorf("exit side: qTarget %v probeQ %v", x.qTarget(wtT0), x.probeQ(wtT0))
	}
}

func TestWcoreLearnQcap(t *testing.T) {
	c := wtCore(1)
	c.qRef = 10 * time.Millisecond
	c.learnQcap(wtT0, wcMeas{qTurn: 70 * time.Millisecond})
	if c.qcap != 60*time.Millisecond || c.qTarget(wtT0) != 15*time.Millisecond || c.probeQ(wtT0) != 20*time.Millisecond {
		t.Fatalf("Q_cap %v: qTarget %v probeQ %v", c.qcap, c.qTarget(wtT0), c.probeQ(wtT0))
	}
	c.learnQcap(wtT0.Add(time.Second), wcMeas{qTurn: 40 * time.Millisecond}) // the least one counts
	if c.qcap != 30*time.Millisecond || c.qTarget(wtT0) != 10*time.Millisecond || c.probeQ(wtT0) != 15*time.Millisecond {
		t.Fatalf("Q_cap %v: qTarget %v probeQ %v", c.qcap, c.qTarget(wtT0), c.probeQ(wtT0))
	}
	c.learnQcap(wtT0.Add(2*time.Second), wcMeas{qTurn: 90 * time.Millisecond})
	if c.qcap != 30*time.Millisecond {
		t.Fatalf("a larger queue raised Q_cap: %v", c.qcap)
	}
	c.learnQcap(wtT0.Add(wcQcapTTL+3*time.Second), wcMeas{qTurn: 90 * time.Millisecond}) // a stale one is replaced
	if c.qcap != 80*time.Millisecond {
		t.Fatalf("stale Q_cap kept: %v", c.qcap)
	}
}

func TestWcoreTau(t *testing.T) {
	const r = 100e6
	for _, tc := range []struct {
		name          string
		wF, wPrev, rF float64
		prev          time.Duration // τ before (−1: none yet)
		want          time.Duration
	}{
		{"window-limited on a clean path", 11e6 + 1e6, 12e6, r, -1, 10 * time.Millisecond},
		{"held behind holes", 31e6, 31e6, r, -1, 200 * time.Millisecond},
		{"at most 2·T_base", 101e6, 101e6, r, -1, 200 * time.Millisecond},
		{"never below 0", 5e6, 5e6, r, -1, 0},
		{"moved windows say nothing", 24e6, 12e6, r, 30 * time.Millisecond, 30 * time.Millisecond},
		{"EWMA", 21e6, 21e6, r, 40 * time.Millisecond, 40*time.Millisecond + time.Duration(wcTauAlpha*float64(60*time.Millisecond))},
	} {
		c := wtCore(1)
		if tc.prev >= 0 {
			c.tau, c.tauOK = tc.prev, true
		}
		m := wcMeas{rF: tc.rF, wF: tc.wF, wPrev: tc.wPrev, hStep: 1e6}
		if tc.name == "window-limited on a clean path" {
			m.wF, m.wPrev = 12e6, 12e6
		}
		c.noteTau(m)
		if d := c.tau - tc.want; d < -time.Microsecond || d > time.Microsecond {
			t.Errorf("%s: τ %v, want %v", tc.name, c.tau, tc.want)
		}
	}
}

func TestWcoreRhat(t *testing.T) {
	c := wtCore(1)
	for i := 0; i < 30; i++ {
		r := 50e6
		if i == 2 {
			r = 90e6 // an old peak
		}
		if i == 25 {
			r = 70e6
		}
		c.noteRate(wtT0.Add(time.Duration(i)*time.Second), r)
	}
	now := wtT0.Add(29 * time.Second)
	if got := c.rhatF(now, wcMeas{}); got != 70e6 {
		t.Errorf("R̂ %v, want the 10 s max 70e6", got)
	}
	if got := c.rhatF(now, wcMeas{rOther: 30e6, rF: 10e6}); got != 40e6 {
		t.Errorf("R̂_F %v, want 70e6 − 30e6", got)
	}
	if got := c.rhatF(now, wcMeas{rOther: 30e6, rF: 60e6}); got != 60e6 {
		t.Errorf("R̂_F %v, want at least R_F", got)
	}
	// the last wcRhatMinN samples are kept however old
	d := wtCore(1)
	for i := 0; i < 3; i++ {
		d.noteRate(wtT0.Add(time.Duration(i)*time.Second), 10e6*float64(i+1))
	}
	if got := d.rhatF(wtT0.Add(time.Hour), wcMeas{}); got != 30e6 {
		t.Errorf("R̂ from old samples %v", got)
	}
}

// off: steady fast streams for two intervals start STARTUP at 1.5× their
// windows (bPub starts at the windows in force); anything unsteady waits.
func TestWcoreOff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage int
		setup func(c *wcore, in *wcIn, m *wcMeas)
		calls int
		want  wcState
	}{
		{"one steady interval", 1, nil, 1, wcOff},
		{"two", 1, nil, 2, wcStartup},
		{"the autopilot probes the pool (freeze)", 1, func(c *wcore, in *wcIn, m *wcMeas) { in.apProbing = true }, 3, wcOff},
		{"windows still moving", 1, func(c *wcore, in *wcIn, m *wcMeas) { m.wPrev = 0.7 * m.wF }, 3, wcOff},
		{"no fast stream", 1, func(c *wcore, in *wcIn, m *wcMeas) { m.f = nil }, 3, wcOff},
		{"waits for the exit's first loss level", 1, func(c *wcore, in *wcIn, m *wcMeas) { c.sndSeen = true }, 3, wcOff},
		{"... not for ever", 1, func(c *wcore, in *wcIn, m *wcMeas) { c.sndSeen = true }, wcOffWait, wcStartup},
		{"stage 1: the windows without the core already queue at the exit", 1, func(c *wcore, in *wcIn, m *wcMeas) {
			m.qSnd, m.qSndOK = 40*time.Millisecond, true
		}, 3, wcOff},
		{"stage 1: ... or lose with a queue behind them", 1, func(c *wcore, in *wcIn, m *wcMeas) {
			m.qSnd, m.qSndOK = 12*time.Millisecond, true
			m.snd, m.sndOK = 0.05, true
		}, 3, wcOff},
		{"stage 1: random loss leaves no queue", 1, func(c *wcore, in *wcIn, m *wcMeas) {
			m.qSnd, m.qSndOK = 1*time.Millisecond, true
			m.snd, m.sndOK = 0.05, true
		}, 2, wcStartup},
		{"stage 2 may go below them", 2, func(c *wcore, in *wcIn, m *wcMeas) { m.qSnd, m.qSndOK = 40*time.Millisecond, true }, 2, wcStartup},
	} {
		c := wtCore(tc.stage)
		in := wtIn(wtT0, 1, wtNeed)
		m := wtMeas(1, 20e6, wtNeed)
		if tc.setup != nil {
			tc.setup(c, &in, &m)
		}
		for i := 0; i < tc.calls; i++ {
			c.off(in, m, nil)
		}
		if c.state != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, c.state, tc.want)
			continue
		}
		if c.state == wcStartup && (c.B != 3*wtMiB || c.bPub != wtNeed) {
			t.Errorf("%s: B %d bPub %d, want 1.5·2 MiB and 2 MiB", tc.name, c.B, c.bPub)
		}
	}
}

// wtStartup is a core in STARTUP at B 4 MiB (from 2 MiB, read 20 MB/s
// there) on one stream, with clean loss levels at 2 MiB.
func wtStartup() (*wcore, wcIn) {
	c := wtCore(1)
	in := wtIn(wtT0, 1, 4*wtMiB)
	wtInF(c, in)
	c.state, c.B, c.bPub, c.prevB, c.prevR, c.fullR, c.gear = wcStartup, 4*wtMiB, 4*wtMiB, 2*wtMiB, 20e6, 20e6, 1
	c.sndRef = wcLossRef{v: 0, b: 2 * wtMiB, ok: true}
	c.oooRef = wcLossRef{v: 0, b: 2 * wtMiB, ok: true}
	c.qRef = 2 * time.Millisecond
	return c, in
}

func TestWcoreStartup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(c *wcore, in *wcIn, m *wcMeas)
		want   wcState
		why    wcDrainWhy
		bCheck func(c *wcore) bool
	}{
		{"the rate grew with B: up a gear", func(c *wcore, in *wcIn, m *wcMeas) { m.rF = 36e6 }, wcStartup, 0,
			func(c *wcore) bool { return c.gear == 2 && c.B > 4*wtMiB && c.bGood == 4*wtMiB }},
		{"the rate stopped growing for wcFullRounds", func(c *wcore, in *wcIn, m *wcMeas) { c.flat = wcFullRounds - 1 }, wcHold, 0, nil},
		{"the queue grew after a flat interval", func(c *wcore, in *wcIn, m *wcMeas) {
			c.flat = 1
			m.qTurn, m.qOK = 40*time.Millisecond, true
		}, wcDrain, wcWhyQueue, nil},
		{"loss came with B", func(c *wcore, in *wcIn, m *wcMeas) { m.snd, m.sndOK = 0.02, true }, wcDrain, wcWhyLoss,
			func(c *wcore) bool { return c.bCap == uint64(wcCapBack*4*wtMiB) && c.qcapOK && c.B <= 2*wtMiB }},
		{"holes, no record yet: keep B, wait", func(c *wcore, in *wcIn, m *wcMeas) {
			c.flat = 1
			m.qTurn, m.qOK = 90*time.Millisecond, true
			m.ooo, m.oooOK = 0.5, true
			c.oooRef = wcLossRef{v: 0.5, b: 2 * wtMiB, ok: true}
			m.qSnd, m.qSndOK = 0, true
		}, wcStartup, 0, func(c *wcore) bool { return c.B == 4*wtMiB && c.flat == 1 }},
		{"holes, the exit's record shows no queue: no queue", func(c *wcore, in *wcIn, m *wcMeas) {
			m.rF = 36e6
			m.qTurn, m.qOK = 90*time.Millisecond, true
			m.ooo, m.oooOK = 0.5, true
			c.oooRef = wcLossRef{v: 0.5, b: 2 * wtMiB, ok: true}
			m.qSnd, m.qSndOK = 0, true
			m.snd, m.sndOK = 0, true
		}, wcStartup, 0, func(c *wcore) bool { return c.B > 4*wtMiB }},
		{"the autopilot probes the pool (freeze)", func(c *wcore, in *wcIn, m *wcMeas) { in.apProbing = true; m.rF = 36e6 }, wcHold, 0, nil},
	} {
		c, in := wtStartup()
		m := wtMeas(1, 22e6, 4*wtMiB)
		tc.setup(c, &in, &m)
		var ev []wcEvent
		c.startup(in, m, &ev)
		if c.state != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, c.state, tc.want)
			continue
		}
		if tc.want == wcDrain {
			if len(ev) == 0 || ev[len(ev)-1].kind != wcEvDrain || ev[len(ev)-1].why != tc.why {
				t.Errorf("%s: events %+v, want a drain for %v", tc.name, ev, tc.why)
			}
		}
		if tc.bCheck != nil && !tc.bCheck(c) {
			t.Errorf("%s: B %d gear %d bGood %d bCap %d flat %d", tc.name, c.B, c.gear, c.bGood, c.bCap, c.flat)
		}
	}
}

// wtHold is a core in HOLD at B 8 MiB on one stream, its references set, no
// probe or refresh due.
func wtHold() (*wcore, wcIn) {
	c := wtCore(1)
	in := wtIn(wtT0, 1, 8*wtMiB)
	wtInF(c, in)
	c.state, c.B, c.bPub, c.bGood = wcHold, 8*wtMiB, 8*wtMiB, 8*wtMiB
	c.noteRate(wtT0, 60e6)
	c.tau, c.tauOK = 20*time.Millisecond, true
	c.sndRef = wcLossRef{v: 0.0001, b: 8 * wtMiB, ok: true}
	c.oooRef = wcLossRef{v: 0.0001, b: 8 * wtMiB, ok: true}
	c.qRef, c.qSndRef, c.qSndRefOK = 5*time.Millisecond, 5*time.Millisecond, true
	c.rLinkHold, c.holdR, c.holdN = 60e6, [2]float64{60e6, 60e6}, 2
	c.probeAt, c.refreshAt = wtT0.Add(time.Minute), wtT0.Add(time.Minute)
	return c, in
}

func TestWcoreHold(t *testing.T) {
	q := func(m *wcMeas, d time.Duration) { m.qTurn, m.qOK = d, true }
	for _, tc := range []struct {
		name  string
		setup func(c *wcore, in *wcIn, m *wcMeas)
		calls int
		want  wcState
		why   wcDrainWhy
	}{
		{"steady", nil, 3, wcHold, 0},
		{"the queue grew once", func(c *wcore, in *wcIn, m *wcMeas) { q(m, 20*time.Millisecond) }, 1, wcHold, 0},
		{"... twice", func(c *wcore, in *wcIn, m *wcMeas) { q(m, 20*time.Millisecond) }, 2, wcDrain, wcWhyQueue},
		{"a strong rise at once", func(c *wcore, in *wcIn, m *wcMeas) { q(m, 40*time.Millisecond) }, 1, wcDrain, wcWhyQueue},
		{"the exit sees no queue: data behind holes", func(c *wcore, in *wcIn, m *wcMeas) {
			q(m, 40*time.Millisecond)
			m.qSnd, m.qSndOK = 5*time.Millisecond, true
		}, 3, wcHold, 0},
		{"random loss at its level", func(c *wcore, in *wcIn, m *wcMeas) {
			c.sndRef.v = 0.01
			m.snd, m.sndOK = 0.012, true
		}, 3, wcHold, 0},
		{"loss past its level that came with a larger B", func(c *wcore, in *wcIn, m *wcMeas) {
			c.bHist = []wcRate{{wtT0, 10 * wtMiB}}
			m.snd, m.sndOK = 0.02, true
		}, 1, wcDrain, wcWhyLoss},
		{"the queue grew and the link slowed", func(c *wcore, in *wcIn, m *wcMeas) {
			q(m, 40*time.Millisecond)
			m.rLink = 30e6
		}, 1, wcDrain, wcWhyRate},
		{"a probe is due", func(c *wcore, in *wcIn, m *wcMeas) { c.probeAt = wtT0 }, 1, wcProbe, 0},
		{"... but the autopilot probes the pool (freeze)", func(c *wcore, in *wcIn, m *wcMeas) {
			c.probeAt = wtT0
			in.apProbing = true
		}, 3, wcHold, 0},
		{"a refresh is due", func(c *wcore, in *wcIn, m *wcMeas) { c.refreshAt = wtT0 }, 1, wcDrain, 0},
	} {
		c, in := wtHold()
		m := wtMeas(1, 60e6, 8*wtMiB)
		m.hit = 0
		if tc.setup != nil {
			tc.setup(c, &in, &m)
		}
		var ev []wcEvent
		for i := 0; i < tc.calls && c.state == wcHold; i++ {
			c.hold(in, m, &ev)
		}
		if c.state != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, c.state, tc.want)
			continue
		}
		switch {
		case tc.want == wcDrain && tc.name == "a refresh is due":
			if !c.refreshing || len(ev) != 0 || c.B >= 8*wtMiB {
				t.Errorf("%s: refreshing %v events %+v B %d", tc.name, c.refreshing, ev, c.B)
			}
		case tc.want == wcDrain:
			if len(ev) == 0 || ev[len(ev)-1].kind != wcEvDrain || ev[len(ev)-1].why != tc.why || c.B >= 8*wtMiB {
				t.Errorf("%s: events %+v B %d, want a drain for %v", tc.name, ev, c.B, tc.why)
			}
		case tc.want == wcProbe:
			if c.probeFrom == 0 || c.B != c.bProbe(wtT0, m, c.probeFrom) {
				t.Errorf("%s: B %d from %d", tc.name, c.B, c.probeFrom)
			}
		}
	}
}

// wtProbe is a core probing 9 MiB from 8 MiB, at 60 MB/s before it.
func wtProbe() (*wcore, wcIn) {
	c, in := wtHold()
	c.state, c.probeFrom, c.probeBase, c.B = wcProbe, 8*wtMiB, 60e6, 9*wtMiB
	return c, in
}

func TestWcoreProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rF      float64
		setup   func(c *wcore, in *wcIn, m *wcMeas)
		want    wcState
		ok, bad int
	}{
		{"paid", 63.6e6, nil, wcHold, 1, 0},
		{"paid as much as it could: grow on", 66.6e6, nil, wcStartup, 1, 0},
		{"did not pay", 60.5e6, nil, wcHold, 0, 1},
		{"loss came with it", 63.6e6, func(c *wcore, in *wcIn, m *wcMeas) { m.snd, m.sndOK = 0.02, true }, wcDrain, 0, 1},
		{"F changed: inconclusive", 63.6e6, func(c *wcore, in *wcIn, m *wcMeas) { m.sameF = false }, wcHold, 0, 0},
	} {
		c, in := wtProbe()
		m := wtMeas(1, tc.rF, 9*wtMiB)
		if tc.setup != nil {
			tc.setup(c, &in, &m)
		}
		var ev []wcEvent
		c.probe(in, m, &ev)
		if c.state != tc.want || c.cnt.probeOK != tc.ok || c.cnt.probeFail != tc.bad {
			t.Errorf("%s: %v ok %d failed %d, want %v %d %d", tc.name, c.state, c.cnt.probeOK, c.cnt.probeFail, tc.want, tc.ok, tc.bad)
			continue
		}
		switch {
		case tc.ok > 0 && c.bGood != 9*wtMiB:
			t.Errorf("%s: bGood %d", tc.name, c.bGood)
		case tc.bad > 0 && (c.B > c.probeFrom || c.backoff != 1):
			t.Errorf("%s: B %d (from %d) backoff %d", tc.name, c.B, c.probeFrom, c.backoff)
		case tc.bad > 0 && c.probeAt.Sub(wtT0) < 2*wcProbeMin:
			t.Errorf("%s: next probe in %v: no backoff", tc.name, c.probeAt.Sub(wtT0))
		}
	}
	// the backoff doubles the interval up to wcProbeBackMax
	c := wtCore(1)
	for b, want := range []time.Duration{6 * time.Second, 12 * time.Second, 24 * time.Second, wcProbeBackMax, wcProbeBackMax} {
		c.backoff = b
		c.schedProbe(wtT0)
		if got := c.probeAt.Sub(wtT0); got != want {
			t.Errorf("backoff %d: next probe in %v, want %v", b, got, want)
		}
	}
}

func TestWcoreOut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  int
		state  wcState
		B      uint64
		full   bool
		ap     bool
		want   []uint32
		growOK bool
		brake  bool
	}{
		{"OFF publishes nothing", 1, wcOff, 0, false, false, []uint32{0, 0, 0}, true, false},
		{"B over F, none for the slow stream", 1, wcHold, 12 * wtMiB, false, false, []uint32{6 * wtMiB, 6 * wtMiB, 0}, true, true},
		{"stage 1: at need or below is no ceiling", 1, wcHold, 4 * wtMiB, false, false, []uint32{0, 0, 0}, true, false},
		{"stage 2, path full: below need too", 2, wcHold, 3 * wtMiB, true, false, []uint32{3 * wtMiB / 2, 3 * wtMiB / 2, 0}, true, true},
		{"DRAIN: no growth", 1, wcDrain, 12 * wtMiB, false, false, []uint32{6 * wtMiB, 6 * wtMiB, 0}, false, true},
		{"the autopilot probes the pool: no growth", 1, wcHold, 12 * wtMiB, false, true, []uint32{6 * wtMiB, 6 * wtMiB, 0}, false, true},
	} {
		c := wtCore(tc.stage)
		in := wtIn(wtT0, 3, 4*wtMiB)
		in.apProbing = tc.ap
		c.inF[1], c.inF[3] = true, true
		c.state, c.B, c.bPub, c.pathFull = tc.state, tc.B, tc.B, tc.full
		c.turnMins.add(wtT0, 100*time.Millisecond, wcTurnWin)
		c.qRef = 5 * time.Millisecond
		o := c.out(in, wtT0, nil)
		for i, x := range o.ceils {
			if x.id != in.streams[i].id || x.ceil != tc.want[i] {
				t.Errorf("%s: ceilings %+v, want %v", tc.name, o.ceils, tc.want)
				break
			}
		}
		if o.growOK != tc.growOK || (o.qBrake > 0) != tc.brake || o.B != tc.B {
			t.Errorf("%s: growOK %v qBrake %v B %d", tc.name, o.growOK, o.qBrake, o.B)
		}
		if tc.brake && o.turnRef != 105*time.Millisecond {
			t.Errorf("%s: turnRef %v, want turnBase + q_ref", tc.name, o.turnRef)
		}
		if tc.state != wcOff && o.next.Sub(wtT0) > wcRampTick {
			t.Errorf("%s: next wake in %v", tc.name, o.next.Sub(wtT0))
		}
	}
}

func TestWcoreBuilder(t *testing.T) {
	var b wcBuilder
	s := wcLinkSnap{now: wtT0, segsIn: 100, ooo: 5, oooOK: true, rcvd: 1000, parks: 1, borrows: 1, poolFree: -1,
		streams: []wcStreamSnap{{id: 1, readTot: 500, updN: 3, hitN: 2, win: wtNeed, turnMin: 90 * time.Millisecond}}}
	in := b.build(s)
	if in.segsIn != 0 || in.linkBytes != 0 || in.streams[0].read != 0 || in.streams[0].upd != 0 || in.snd.fresh {
		t.Fatalf("first snapshot counts: %+v", in)
	}
	if in.streams[0].turnMin != 90*time.Millisecond || in.streams[0].win != wtNeed {
		t.Fatalf("first snapshot's samples: %+v", in.streams[0])
	}
	s.now = wtT0.Add(100 * time.Millisecond)
	s.segsIn, s.ooo, s.rcvd, s.parks, s.borrows, s.poolShort = 300, 6, 5000, 4, 2, 1 // a borrow is a park too
	s.streams = []wcStreamSnap{
		{id: 1, readTot: 1500, updN: 5, hitN: 3, lagN: 1, win: 3 * wtMiB},
		{id: 3, readTot: 700, updN: 2, win: wtNeed}, // new: counts from here
	}
	s.sndAt, s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = wtT0, 120*time.Millisecond, 100*time.Millisecond, 10, 1000
	in = b.build(s)
	if in.segsIn != 200 || in.ooo != 1 || in.linkBytes != 4000 || in.parks != 2 || in.poolShort != 1 {
		t.Fatalf("link deltas: %+v", in)
	}
	if x := in.streams[0]; x.read != 1000 || x.upd != 2 || x.hit != 1 || x.lag != 1 || x.win != 3*wtMiB || x.turnMin != 0 {
		t.Fatalf("stream deltas: %+v", x)
	}
	if x := in.streams[1]; x.read != 0 || x.upd != 0 {
		t.Fatalf("a new stream counted from before: %+v", x)
	}
	if !in.snd.fresh || in.snd.retrans != 0 || in.snd.segsOut != 0 || in.snd.srtt != 120*time.Millisecond {
		t.Fatalf("the first record only sets the base: %+v", in.snd)
	}
	s.now = wtT0.Add(200 * time.Millisecond)
	in = b.build(s) // the same record again
	if in.snd.fresh {
		t.Fatal("an old record is fresh again")
	}
	s.now = wtT0.Add(300 * time.Millisecond)
	s.sndAt, s.sndRetrans, s.sndSegsOut = wtT0.Add(2*time.Second), 25, 3000
	s.segsIn = 100            // a counter that went back (a new socket): no delta
	s.parks, s.borrows = 5, 4 // only borrows: no park
	in = b.build(s)
	if !in.snd.fresh || in.snd.retrans != 15 || in.snd.segsOut != 2000 || in.segsIn != 0 || in.parks != 0 {
		t.Fatalf("second record: %+v segsIn %d parks %d", in.snd, in.segsIn, in.parks)
	}
	// a stream the snapshot left out counts from its next one on
	s.now = wtT0.Add(400 * time.Millisecond)
	s.streams = s.streams[1:]
	in = b.build(s)
	s.now = wtT0.Add(500 * time.Millisecond)
	s.streams = append(s.streams, wcStreamSnap{id: 1, readTot: 9000, updN: 9, win: wtNeed})
	in = b.build(s)
	if x := in.streams[1]; x.id != 1 || x.read != 0 || x.upd != 0 {
		t.Fatalf("a stream back in the snapshot: %+v", x)
	}
}

func TestWcoreMerge(t *testing.T) {
	a := wcIn{now: wtT0, segsIn: 10, ooo: 1, linkBytes: 100, parks: 1, pongs: []time.Duration{1},
		snd:     wcSnd{fresh: true, retrans: 2, segsOut: 100},
		streams: []wcStream{{id: 1, read: 10, upd: 1, hit: 1, turnMin: 50, turnLast: 60, win: 1}, {id: 3, read: 5}}}
	a = wcCopy(a)
	b := wcIn{now: wtT0.Add(time.Second), segsIn: 5, ooo: 2, linkBytes: 50, apProbing: true, poolShort: 1, pongs: []time.Duration{2},
		snd:     wcSnd{fresh: true, retrans: 3, segsOut: 50, srtt: 7},
		streams: []wcStream{{id: 1, read: 20, upd: 2, hit: 1, lag: 1, turnMin: 40, turnLast: 0, win: 2}, {id: 5, read: 7}}}
	wcMerge(&a, b)
	if a.now != b.now || a.segsIn != 15 || a.ooo != 3 || a.linkBytes != 150 || a.parks != 1 || a.poolShort != 1 || !a.apProbing || len(a.pongs) != 2 {
		t.Fatalf("link: %+v", a)
	}
	if a.snd.retrans != 5 || a.snd.segsOut != 150 || a.snd.srtt != 7 {
		t.Fatalf("records: %+v", a.snd)
	}
	if len(a.streams) != 2 || a.streams[0].id != 1 || a.streams[1].id != 5 {
		t.Fatalf("streams (3 is gone, 5 came): %+v", a.streams)
	}
	if x := a.streams[0]; x.read != 30 || x.upd != 3 || x.hit != 2 || x.lag != 1 || x.turnMin != 40 || x.turnLast != 60 || x.win != 2 {
		t.Fatalf("stream 1: %+v", x)
	}
}

// step: between evaluations the input accumulates and only the ramp moves;
// the published budget rises by at most a ramp step per wake.
func TestWcoreStepRamp(t *testing.T) {
	c, in := wtHold()
	c.last = wtMeas(1, 60e6, 8*wtMiB)
	c.B, c.bPub, c.evalAt, c.lastAt = 12*wtMiB, 8*wtMiB, wtT0.Add(time.Second), wtT0.Add(-time.Second)
	prev := c.bPub
	for i := 1; i <= 5; i++ {
		in.now = wtT0.Add(time.Duration(i) * wcRampTick)
		in.segsIn = 10
		o := c.step(in)
		if o.B < prev || float64(o.B-prev) > math.Max(wcRampQ.Seconds()*60e6, wcRampShare*float64(prev))+1 {
			t.Fatalf("wake %d: published %d after %d", i, o.B, prev)
		}
		if o.next.Sub(in.now) > wcRampTick {
			t.Fatalf("wake %d: next in %v", i, o.next.Sub(in.now))
		}
		prev = o.B
	}
	if c.acc.segsIn != 50 || c.state != wcHold {
		t.Fatalf("accumulated %d, state %v", c.acc.segsIn, c.state)
	}
	if c.rampLeft() <= 0 && c.bPub < c.B {
		t.Fatal("rampLeft says done before the ramp is")
	}
}

// An old exit's record (no TCP_INFO in it: srtt 0) is no loss level to
// wait for, and gives no q_snd.
func TestWcoreOldExitRecord(t *testing.T) {
	c := wtCore(1)
	in := wtIn(wtT0, 1, wtNeed)
	in.snd = wcSnd{fresh: true}
	c.step(in)
	if c.sndSeen || !c.qSndAt.IsZero() {
		t.Fatalf("old record: sndSeen %v q_snd at %v", c.sndSeen, c.qSndAt)
	}
	in.now = wtT0.Add(time.Second)
	in.snd = wcSnd{fresh: true, srtt: 130 * time.Millisecond, minRTT: 100 * time.Millisecond, segsOut: 100}
	c.step(in)
	if !c.sndSeen || c.qSnd != 30*time.Millisecond {
		t.Fatalf("new record: sndSeen %v q_snd %v", c.sndSeen, c.qSnd)
	}
}
