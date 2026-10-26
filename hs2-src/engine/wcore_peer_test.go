package engine

import (
	"math"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// TestWcorePeerQueue: a peer keeps every link's records for wcPeerKeep and
// reads their quantiles over a span; a core counts as on while it is
// evaluated, and a link that ends is forgotten with its records.
func TestWcorePeerQueue(t *testing.T) {
	p := newWcPeer()
	a, b := wtCore(1), wtCore(1)
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	for i, q := range []int{40, 10, 12, 30, 14} {
		c := a
		if i%2 == 1 {
			c = b
		}
		p.noteRec(c, wtT0.Add(time.Duration(i)*time.Second), ms(q))
	}
	now := wtT0.Add(4 * time.Second)
	if q, n := p.queue(now, wcPeerSpan, 0.5); n != 5 || q != ms(14) {
		t.Errorf("median of 5 records: %v of %d, want 14ms of 5", q, n)
	}
	if q, n := p.queue(now, 2*time.Second, 0.5); n != 3 || q != ms(14) {
		t.Errorf("median of the last 2 s: %v of %d, want 14ms of 3", q, n)
	}
	if q, _ := p.queue(now, wcPeerSpan, 0.25); q != ms(12) {
		t.Errorf("lower quartile: %v, want 12ms", q)
	}
	// older than wcPeerKeep: gone as the next comes (the records of 0 and
	// 1 s, at 8.5 s)
	p.noteRec(a, wtT0.Add(wcPeerKeep+time.Second+time.Millisecond), ms(20))
	if len(p.recs) != 4 || p.recs[0].q != ms(12) {
		t.Errorf("records past wcPeerKeep kept: %d, first %v", len(p.recs), p.recs[0].q)
	}
	p.noteCore(a, now, true, 50e6)
	p.noteCore(b, now, false, 10e6)
	if n, r := p.others(b, now); n != 1 || r != 50e6 {
		t.Errorf("others of b: %d (%.0f), want 1 (50e6)", n, r)
	}
	if n, _ := p.others(a, now); n != 0 {
		t.Errorf("others of a: %d, want 0 (b is off)", n)
	}
	if n, _ := p.others(b, now.Add(wcPeerStale+time.Millisecond)); n != 0 {
		t.Errorf("a core not evaluated for wcPeerStale still counts")
	}
	p.drop(a)
	for _, r := range p.recs {
		if r.c == a {
			t.Fatalf("a dropped link's record kept")
		}
	}
	if _, ok := p.cores[a]; ok || p.empty() {
		t.Errorf("drop: a kept %v, empty %v (b is left)", ok, p.empty())
	}
	p.drop(b)
	if !p.empty() {
		t.Errorf("both dropped, not empty: %d records, %d cores", len(p.recs), len(p.cores))
	}
}

// wtPeer gives c a peer with another core on (when on) carrying 60e6 bytes/s,
// and the exit's records qs (ms, oldest first, a second apart, the last at
// now) for both links.
func wtPeer(c *wcore, now time.Time, on bool, qs ...int) *wcore {
	p := newWcPeer()
	o := wtCore(1)
	p.noteCore(o, now, on, 60e6)
	c.peer = p
	for i, q := range qs {
		at := now.Add(-time.Duration(len(qs)-1-i) * time.Second)
		d := time.Duration(q) * time.Millisecond
		w := c
		if i%2 == 0 {
			w = o
		}
		p.noteRec(w, at, d)
		copy(c.qSnds[1:], c.qSnds[:])
		c.qSnds[0] = wcRate{at, d.Seconds()}
		c.qSnd, c.qSndAt = d, at
	}
	return o
}

// TestWcorePeerLateCore (red team 3, finding 1): a core in OFF judges its
// link's windows past the knee when the exit's records show a queue past Δq
// (alone, in every one of the link's last three: judge, p1). Where other
// cores of the peer are on, the queue they hold (qTarget) is theirs: a late
// download's core stayed OFF for good behind it (P6X, P6L: 78 against
// 245–255 Mbit/s, and two of four links OFF from 360 and 480 s to the end).
// With another core on, the records of every link judge (their median: one
// is a burst as often as a queue), past Δq + qTarget.
func TestWcorePeerLateCore(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		qs   []int
		harm bool
	}{
		{"alone: the windows hold a queue past Δq", false, []int{14, 16, 15, 13}, true},
		{"alone: one record past Δq is a burst (judge, p1)", false, []int{4, 16, 3, 5}, false},
		{"another core on holds qTarget", true, []int{14, 16, 15, 13}, false},
		{"... a burst among the records", true, []int{14, 45, 15, 13}, false},
		{"... and the path queues past it", true, []int{28, 32, 30, 29}, true},
		{"no peer at all", false, nil, false},
	} {
		c := wtCore(1)
		c.sndSeen = true
		now := wtT0
		if tc.qs != nil {
			wtPeer(c, now, tc.on, tc.qs...)
		}
		in := wtIn(now, 1, wtNeed)
		m := c.measure(in, 1)
		if got := c.harm(m); got != tc.harm {
			t.Errorf("%s: harm %v (grp %v, q %v), want %v", tc.name, got, m.grp, m.qSnd3, tc.harm)
		}
	}
}

// TestWcorePeerRefs: with another core of the peer on, q_snd's HOLD
// reference judges from at most qTarget, and q_ref follows the queue only to
// qTarget + Δq/2 on a path with no holes; alone both stay where HOLD found
// them (the link's own queue).
func TestWcorePeerRefs(t *testing.T) {
	for _, grp := range []bool{false, true} {
		c, in := wtHold()
		ms := time.Millisecond
		c.qSndRef, c.qRef = 30*ms, 30*ms
		m := wtMeas(1, 60e6, 8*wtMiB)
		m.grp, m.at = grp, wtT0
		ref := c.qSndRefOf(wtT0, m)
		c.capRef(wtT0, m)
		want, wantRef := 30*ms, 30*ms
		if grp {
			want, wantRef = wcQTarget, wcQTarget+c.dq()/2
		}
		if ref != want || c.qRef != wantRef {
			t.Errorf("grp %v: q_snd reference %v, q_ref %v; want %v, %v", grp, ref, c.qRef, want, wantRef)
		}
		// where data waits behind holes q_ref holds that wait: not capped
		c.qRef = 30 * ms
		m.holey = true
		c.capRef(wtT0, m)
		if c.qRef != 30*ms {
			t.Errorf("grp %v: q_ref capped behind holes: %v", grp, c.qRef)
		}
		// the path's queue past the capped reference by Δq: growth, twice a drain
		m.holey = false
		m.qPath, m.qPathOK, m.qSnd, m.qSndOK = 36*ms, true, 36*ms, true
		var ev []wcEvent
		c.hold(in, m, &ev)
		c.hold(in, m, &ev)
		if drained := c.state == wcDrain; drained != grp {
			t.Errorf("grp %v: a path queue of 36 ms against a reference of 30: drained %v", grp, drained)
		}
	}
}

// TestWcorePeerTau: τ_hold learns the wait behind holes, not the queue the
// path holds: with another core on the peer, a window-limited interval's
// turn past T_base less what stands of the queue (the records' lower
// quartile); alone as before (the link's own queue is what its knee needs).
func TestWcorePeerTau(t *testing.T) {
	ms := time.Millisecond
	for _, grp := range []bool{false, true} {
		c := wtCore(1)
		// a window of 8 MiB less H_step at 60e6 bytes/s turns in 132 ms:
		// T_base 100 ms + 32 ms
		m := wtMeas(1, 60e6, 8*wtMiB)
		m.at = wtT0
		m.grp, m.qStand, m.qStandOK = grp, 20*ms, grp
		c.noteTau(m)
		turn := time.Duration(((m.wF - m.hStep) / m.rF) * float64(time.Second))
		want := turn - c.tBase
		if grp {
			want -= 20 * ms
		}
		if d := c.tau - want; d < -ms || d > ms {
			t.Errorf("grp %v: τ_hold %v, want %v", grp, c.tau, want)
		}
	}
}

// TestWcorePeerShare: with other cores on and a queue that stands, the HOLD
// budget is the knee at the link's smoothed rate plus the same margin for
// every core (qTarget at the fair share), so two links at 2:1 move toward
// even; with no queue standing each core keeps its own rule (R̂). A core
// past 1.2 times the fair share does not probe.
func TestWcorePeerShare(t *testing.T) {
	ms := time.Millisecond
	p := newWcPeer()
	mk := func(rAvg float64, stand time.Duration) (*wcore, wcMeas) {
		c := wtCore(1)
		c.noteRate(wtT0, rAvg)
		c.rAvg, c.bGood, c.peer = rAvg, 64*wtMiB, p
		p.noteCore(c, wtT0, true, rAvg)
		m := wtMeas(1, rAvg, 8*wtMiB)
		m.at, m.grp, m.qStand, m.qStandOK = wtT0, true, stand, true
		return c, m
	}
	big, mb := mk(60e6, 15*ms)
	small, msm := mk(30e6, 15*ms)
	f := (60e6 + 30e6) / 2.0
	h := wcQTarget.Seconds() / big.tBase.Seconds()
	for _, x := range []struct {
		name string
		c    *wcore
		m    wcMeas
	}{{"big", big, mb}, {"small", small, msm}} {
		b := x.c.bHold(wtT0, x.m)
		want := x.c.knee(x.c.rAvg, x.m) + h*f*x.c.tBase.Seconds()
		if math.Abs(float64(b)-want) > 2 {
			t.Errorf("%s: B_hold %d, want the knee at its rate and the shared margin %.0f", x.name, b, want)
		}
	}
	// (each holds the same margin in the shared queue: its rate is that margin
	// over the queue, so the shares even out)
	if !big.aboveShare(wtT0, mb) || small.aboveShare(wtT0, msm) {
		t.Errorf("aboveShare: big %v (60e6 against 45e6), small %v", big.aboveShare(wtT0, mb), small.aboveShare(wtT0, msm))
	}
	// no queue standing: R̂'s rule, as alone
	mb.qStand = 2 * ms
	r := big.rhatF(wtT0, mb)
	if b, want := big.bHold(wtT0, mb), big.knee(r, mb)+h*r*big.tBase.Seconds(); math.Abs(float64(b)-want) > 2 {
		t.Errorf("no queue standing: B_hold %d, want R̂'s %.0f", b, want)
	}
}

// TestWcorePeerShareQcap: where Q_cap lowers qTarget (a shallow queue,
// qTarget a third of it) the shared rule holds only a core past the fair
// share; one at
// or below it keeps its own rule (R̂'s knee and margin) and may probe: the
// rule's equilibrium at a qTarget of a few ms sat under the exit's records of
// the senders' bursts and held two downloads on 300/150 behind 50 ms at 203
// against 230 Mbit/s (z24 rig).
func TestWcorePeerShareQcap(t *testing.T) {
	ms := time.Millisecond
	p := newWcPeer()
	mk := func(rAvg float64) (*wcore, wcMeas) {
		c := wtCore(1)
		c.noteRate(wtT0, rAvg)
		c.rAvg, c.bGood, c.peer = rAvg, 64*wtMiB, p
		c.qcap, c.qcapAt, c.qcapOK = 12*ms, wtT0, true
		p.noteCore(c, wtT0, true, rAvg)
		m := wtMeas(1, rAvg, 8*wtMiB)
		m.at, m.grp, m.qStand, m.qStandOK = wtT0, true, 8*ms, true
		return c, m
	}
	big, mb := mk(60e6)
	small, msm := mk(30e6)
	if _, ok := big.fairShare(wtT0, mb); !ok {
		t.Errorf("a core past the fair share with qTarget lowered by Q_cap: no shared rule")
	}
	if f, ok := small.fairShare(wtT0, msm); ok || f != 45e6 {
		t.Errorf("a core under the fair share with qTarget lowered by Q_cap: shared %v (%.0f), want its own rule", ok, f)
	}
	h := math.Min(math.Max(small.qTarget(wtT0).Seconds()/small.tBase.Seconds(), wcHMin), wcHMax)
	r := small.rhatF(wtT0, msm)
	if b, want := small.bHold(wtT0, msm), small.knee(r, msm)+h*r*small.tBase.Seconds(); math.Abs(float64(b)-want) > 2 {
		t.Errorf("under the share: B_hold %d, want R̂'s %.0f", b, want)
	}
	// no Q_cap, or one deep enough to leave qTarget as it is: both under the
	// shared rule, as before
	for _, q := range []time.Duration{0, 60 * ms} {
		small.qcap, small.qcapOK = q, q > 0
		if _, ok := small.fairShare(wtT0, msm); !ok {
			t.Errorf("Q_cap %v: a core under the fair share left out of the shared rule", q)
		}
	}
}

// TestWcorePeerRegistry: the runner's registry keeps a peer while any link to
// it is registered, idle ones included (an idle link is not stepped, so its
// core is in neither the records nor the cores): when the one busy link to
// a host ended beside an idle one, the registry forgot the peer the idle
// link still held, and the next link to that host got another, so the two
// never shared.
func TestWcorePeerRegistry(t *testing.T) {
	cfg := wcTestConfig()
	ws := &wcSet{}
	var sess []*smux.Session
	link := func() *wcLink {
		a, b := net.Pipe()
		edge, exit := wcSessions(t, a, b, cfg, nil)
		t.Cleanup(func() { edge.Close(); exit.Close() })
		sess = append(sess, edge)
		l := newWcLink(edge, cfg, nil, nil, false, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 40000 + len(sess)})
		if l == nil {
			t.Fatal("no window core link")
		}
		ws.put(l)
		return l
	}
	busy, idle := link(), link()
	p := busy.core.peer
	if p == nil || idle.core.peer != p || len(ws.peers) != 1 {
		t.Fatalf("two links to one host: peers %p and %p, %d in the registry", p, idle.core.peer, len(ws.peers))
	}
	// the busy one was evaluated, the idle one never; the busy one ends
	p.noteCore(busy.core, time.Now(), true, 50e6)
	sess[0].Close()
	ws.wakeAll()
	if ws.peers[busy.peerKey] != p {
		t.Fatalf("the busy link ended: the registry forgot the peer its idle link holds")
	}
	if l := link(); l.core.peer != p {
		t.Errorf("a new link to the host after the busy one ended: another peer, not the idle link's")
	}
	// the last links end: the peer goes
	sess[1].Close()
	sess[2].Close()
	ws.wakeAll()
	if len(ws.peers) != 0 {
		t.Errorf("every link ended: %d peers left in the registry", len(ws.peers))
	}
}
