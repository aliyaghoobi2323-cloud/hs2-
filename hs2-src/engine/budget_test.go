package engine

import (
	"errors"
	"math"
	"testing"
	"time"
)

// budgetDir is one direction of a model link for the budget's tests: a
// window-limited sender (in flight = B, or its demand's when that is less)
// over a path of rate c and base round trip r, whose own queue is what it
// holds past c × (r + the way back's queue).
type budgetDir struct {
	core   budgetCore
	c      float64 // its share of the bottleneck, bytes/s
	demand float64 // bytes/s it would send (0: all it can)
	b      float64
	rate   float64 // delivered at the latest step
	q      time.Duration
	now    time.Duration // the readings' clock
	ownOWD bool          // the link's own one-way delays (both directions' queues) are read
}

// inflight is what the direction holds in flight: its budget when that
// holds it (blocked), else what it would send (qBack: the way back).
func (d *budgetDir) inflight(r, qBack time.Duration) (w float64, blocked bool) {
	need := math.Inf(1)
	if d.demand > 0 {
		need = d.demand * (r + qBack + d.q).Seconds()
	}
	if d.b <= 0 || need <= d.b {
		if math.IsInf(need, 1) {
			return 4 * d.c * (r + qBack).Seconds(), false // no budget: BBR's own window (2 BDP and more)
		}
		return need, false
	}
	return d.b, true
}

// settle is the queue and the rate a window w makes over r and qBack.
func (d *budgetDir) settle(w float64, r, qBack time.Duration) {
	bdp := d.c * (r + qBack).Seconds()
	if w > bdp {
		d.q = time.Duration((w - bdp) / d.c * float64(time.Second))
		d.rate = d.c
	} else {
		d.q = 0
		d.rate = w / (r + qBack).Seconds()
	}
}

// read is the budget's reading of the direction.
func (d *budgetDir) read(r, qBack time.Duration, blocked bool, back *budgetDir, qOut, qIn time.Duration, owd bool) {
	d.now += coreTick
	in := budgetIn{Now: d.now, On: true, Delivery: d.rate, SRTT: r + d.q + qBack, MinRTT: r, TxRate: d.rate,
		Blocked: blocked, MSS: 1448, Frame: BulkFrameSize(d.rate), DlvMax: d.rate}
	if back != nil {
		in.RxRate = back.rate
	}
	if owd {
		in.QOut, in.QOutOK, in.QIn, in.QInOK = qOut, true, qIn, true
	}
	if d.ownOWD && back != nil {
		in.LOut, in.LOutOK, in.LIn, in.LInOK = d.q, true, qBack, true
	}
	d.b = d.core.step(in)
}

const bMbit = 1e6 / 8

// One direction on its own (fq_codel or FIFO: its queue is its own) from
// the start: the queue settles at the target and the rate at the path's,
// at 1, 25, 100 and 1000 Mbit/s and 20, 72 and 150 ms.
func TestBudgetSettles(t *testing.T) {
	T := budgetTarget()
	for _, rate := range []float64{1, 25, 100, 1000} {
		for _, rtt := range []time.Duration{20 * time.Millisecond, 72 * time.Millisecond, 150 * time.Millisecond} {
			d := &budgetDir{c: rate * bMbit}
			d.rate = 0.05 * d.c // the first sample after a move: a little
			var qs []time.Duration
			minRate := math.Inf(1)
			for i := 0; i < 300; i++ {
				w, bl := d.inflight(rtt, 0)
				d.settle(w, rtt, 0)
				d.read(rtt, 0, bl, nil, 0, 0, false)
				if i >= 100 {
					qs = append(qs, d.q)
					minRate = math.Min(minRate, d.rate)
				}
			}
			mx := time.Duration(0)
			for _, q := range qs {
				mx = max(mx, q)
			}
			if minRate < 0.97*d.c || mx > 2*T+time.Millisecond {
				t.Fatalf("%g Mbit/s, %v: rate %.1f%% of the path at least, queue %v at most (target %v)",
					rate, rtt, 100*minRate/d.c, mx, T)
			}
		}
	}
}

// After a start in BBR's start-up (the delivery doubling a reading), the
// budget leaves the start only once the delivery stops growing, and then
// holds the queue near T: never a start that ends at a tenth of the path
// (the round-2 judge's F2).
func TestBudgetStartWaitsForGrowth(t *testing.T) {
	var c budgetCore
	r := 72 * time.Millisecond
	T := budgetTarget()
	path := 100 * bMbit
	dlv := 2 * bMbit
	for i := 0; i < 40; i++ {
		q := 10 * time.Millisecond // a start-up queue all along
		if dlv >= path {
			dlv = path
		}
		c.step(budgetIn{On: true, Delivery: dlv, SRTT: r + q, MinRTT: r, TxRate: dlv, MSS: 1448, DlvMax: dlv})
		if dlv < path && c.mode != budgetStart {
			t.Fatalf("reading %d: left the start at %.1f Mbit/s of 100, growing", i, dlv/bMbit)
		}
		dlv *= 2
	}
	if c.mode != budgetRun {
		t.Fatalf("still %v with the delivery flat and a queue", c.mode)
	}
	if want := path * (r + T).Seconds(); math.Abs(c.b-want) > 0.15*want {
		t.Fatalf("B %.0f KiB after the start, want about %.0f", c.b/1024, want/1024)
	}
}

// A path ten times faster: the budget, held by it with no queue, doubles
// its step each reading and reaches 0.9 of the new rate within 2.5 s.
func TestBudgetGrowsAfterCapacityRise(t *testing.T) {
	r := 72 * time.Millisecond
	d := &budgetDir{c: 10 * bMbit, rate: 10 * bMbit}
	for i := 0; i < 100; i++ {
		w, bl := d.inflight(r, 0)
		d.settle(w, r, 0)
		d.read(r, 0, bl, nil, 0, 0, false)
	}
	d.c = 100 * bMbit
	for i := 1; i <= 60; i++ {
		w, bl := d.inflight(r, 0)
		d.settle(w, r, 0)
		d.read(r, 0, bl, nil, 0, 0, false)
		if d.rate >= 0.9*d.c {
			if i > 25 {
				t.Fatalf("0.9 of the new rate after %d readings", i)
			}
			return
		}
	}
	t.Fatalf("at %.1f Mbit/s of 100 after 6 s", d.rate/bMbit)
}

// A path that halves: the queue comes back under 2T within five readings.
func TestBudgetCutsAfterCapacityDrop(t *testing.T) {
	r := 72 * time.Millisecond
	T := budgetTarget()
	d := &budgetDir{c: 100 * bMbit, rate: 100 * bMbit}
	for i := 0; i < 100; i++ {
		w, bl := d.inflight(r, 0)
		d.settle(w, r, 0)
		d.read(r, 0, bl, nil, 0, 0, false)
	}
	d.c = 50 * bMbit
	for i := 1; i <= 8; i++ {
		w, bl := d.inflight(r, 0)
		d.settle(w, r, 0)
		d.read(r, 0, bl, nil, 0, 0, false)
		if i >= 5 && d.q > 2*T {
			t.Fatalf("reading %d after the drop: queue %v", i, d.q)
		}
	}
}

// settleTwoWay is a two-way link's queues and rates for windows wd and wu
// (the acks of each direction wait behind the other's data): the direction
// whose window passes its path's by more holds the queue, S, and the other
// direction's rate is its window over r + S.
func settleTwoWay(dn, up *budgetDir, wd, wu float64, r, extra time.Duration) {
	x := wd/dn.c - r.Seconds()
	y := wu/up.c - r.Seconds()
	S := math.Max(math.Max(x, y), 0)
	dn.q, up.q = 0, 0
	if x >= y {
		dn.q = time.Duration(S * float64(time.Second))
	} else {
		up.q = time.Duration(S * float64(time.Second))
	}
	up.q += extra
	S += extra.Seconds()
	dn.rate = math.Min(dn.c, wd/(r.Seconds()+S))
	up.rate = math.Min(up.c, wu/(r.Seconds()+S))
}

// The asymmetric two-way link of round 2 (a 100 Mbit/s download beside a 20
// Mbit/s upload on one BULK link, fq_codel: srtt holds both queues, and the
// QUIET link sees neither): both directions keep their path's rate (α on
// srtt held the download at 17), and the queue stays under CoDel's 5 ms.
// After a start-up spike of the upload of 15 ms, both are back at 0.97 of
// their paths within 3 s (a split of the shared srtt that left one under
// its path does not stay: budgetRoom).
func TestBudgetTwoWayAsym(t *testing.T) {
	r := 72 * time.Millisecond
	for _, ownOWD := range []bool{false, true} {
		for _, spike := range []bool{false, true} {
			dn := &budgetDir{c: 100 * bMbit, rate: 100 * bMbit, ownOWD: ownOWD}
			up := &budgetDir{c: 20 * bMbit, rate: 20 * bMbit, ownOWD: ownOWD}
			minDn, minUp := math.Inf(1), math.Inf(1)
			maxQ := time.Duration(0)
			for i := 0; i < 600; i++ {
				wd, bd := dn.inflight(r, up.q)
				wu, bu := up.inflight(r, dn.q)
				extra := time.Duration(0)
				if spike && i >= 200 && i < 203 {
					extra = 15 * time.Millisecond // a start-up burst of the upload
				}
				settleTwoWay(dn, up, wd, wu, r, extra)
				dn.read(r, up.q, bd, up, 0, 0, true)
				up.read(r, dn.q, bu, dn, 0, 0, true)
				if i >= 100 && (i < 200 || i > 230) {
					minDn, minUp = math.Min(minDn, dn.rate), math.Min(minUp, up.rate)
					maxQ = max(maxQ, dn.q+up.q)
				}
			}
			if minDn < 0.97*dn.c || minUp < 0.97*up.c || maxQ > 5*time.Millisecond {
				t.Fatalf("own delays %v, spike %v: download at least %.1f, upload %.1f Mbit/s, queue at most %v",
					ownOWD, spike, minDn/bMbit, minUp/bMbit, maxQ)
			}
		}
	}
}

// The start of round 3's M1 on the rig (100 down beside 20 up): the
// upload's start-up held a queue of 20 ms for a second while the download
// read it in its srtt. With the link's own one-way delays the download
// takes it for the way back's and keeps its budget; with the guess alone
// it cuts, but never under half of its 10 s most acked × min_rtt.
func TestBudgetTwoWayStartSpike(t *testing.T) {
	r := 72 * time.Millisecond
	T := budgetTarget()
	for _, ownOWD := range []bool{true, false} {
		var c budgetCore
		in := budgetIn{On: true, Delivery: 90 * bMbit, MinRTT: r, SRTT: r + T, TxRate: 90 * bMbit, RxRate: 18 * bMbit,
			Blocked: true, MSS: 1448, DlvMax: 90 * bMbit}
		for i := 0; i < 20; i++ {
			in.Now += coreTick
			in.SRTT = r + T + time.Duration(i%2)*10*time.Millisecond // a start-up queue to leave the start
			c.step(in)
		}
		in.SRTT = r + T
		for i := 0; i < 20; i++ {
			in.Now += coreTick
			c.step(in)
		}
		b0 := c.b
		in.SRTT = r + 20*time.Millisecond
		if ownOWD {
			in.LIn, in.LInOK, in.LOut, in.LOutOK = 20*time.Millisecond, true, 0, true
		}
		for i := 0; i < 10; i++ {
			in.Now += coreTick
			in.TxRate = 90 * bMbit * math.Min(1, c.b/b0)
			c.step(in)
		}
		floor := budgetKMin * 90 * bMbit * r.Seconds()
		if ownOWD && c.b < 0.95*b0 || !ownOWD && c.b < floor*0.999 {
			t.Fatalf("own delays %v: B %.0f KiB after the way back's 20 ms (was %.0f, floor %.0f)", ownOWD, c.b/1024, b0/1024, floor/1024)
		}
	}
}

// On a shared FIFO the QUIET link's one-way delays say which direction
// queues: a link held by the way back's queue (the other direction's,
// 20 ms) keeps its rate (base takes it), and one whose own direction
// queues past T cuts.
func TestBudgetQuietOWD(t *testing.T) {
	r := 72 * time.Millisecond
	T := budgetTarget()
	var c budgetCore
	in := budgetIn{On: true, Delivery: 25 * bMbit, MinRTT: r, TxRate: 25 * bMbit, Blocked: true, MSS: 1448, DlvMax: 25 * bMbit,
		QOutOK: true, QInOK: true}
	in.QIn = 20 * time.Millisecond
	in.SRTT = r + in.QIn + T
	for i := 0; i < 30; i++ {
		c.step(in)
	}
	if want := 25 * bMbit * (r + in.QIn + T).Seconds(); c.b < 0.95*want {
		t.Fatalf("B %.0f KiB with the way back's queue of 20 ms, want at least %.0f (the rate kept)", c.b/1024, 0.95*want/1024)
	}
	b0 := c.b
	in.QIn, in.QOut = 0, 10*time.Millisecond
	in.SRTT = r + in.QOut
	c.step(in)
	if c.b >= b0 {
		t.Fatalf("B %.0f KiB with this direction's queue at 10 ms on the QUIET link: no cut from %.0f", c.b/1024, b0/1024)
	}
}

// 64 links of one flow each share a fq_codel bottleneck of 100 Mbit/s (each
// flow its fair share, its queue its own): the sum keeps the path, no link
// starves (version 4 of round 2 fell from 89 to 50 Mbit/s and held two
// uploads at 65 and 9), and none queues past 2T.
func TestBudgetManyLinksFQ(t *testing.T) {
	r := 72 * time.Millisecond
	T := budgetTarget()
	for _, n := range []int{1, 2, 17, 64, 300} {
		C := 100 * bMbit
		links := make([]*budgetDir, n)
		for i := range links {
			links[i] = &budgetDir{c: C / float64(n), rate: C / float64(n) * 0.1}
		}
		var sum, minShare float64
		maxQ := time.Duration(0)
		for step := 0; step < 400; step++ {
			// water-filling: each flow's share of the bottleneck is the fair
			// one, past what the flows that want less take
			ws := make([]float64, n)
			bl := make([]bool, n)
			for i, l := range links {
				ws[i], bl[i] = l.inflight(r, 0)
			}
			left, k := C, n
			share := make([]float64, n)
			done := make([]bool, n)
			for k > 0 {
				fair := left / float64(k)
				moved := false
				for i := range links {
					if !done[i] && ws[i]/r.Seconds() <= fair {
						share[i], done[i], moved = ws[i]/r.Seconds(), true, true
						left -= share[i]
						k--
					}
				}
				if !moved {
					for i := range links {
						if !done[i] {
							share[i], done[i] = fair, true
						}
					}
					k = 0
				}
			}
			sum, minShare = 0, math.Inf(1)
			for i, l := range links {
				l.c = math.Max(share[i], 1)
				l.settle(ws[i], r, 0)
				l.read(r, 0, bl[i], nil, 0, 0, false)
				if step >= 200 {
					maxQ = max(maxQ, l.q)
				}
				sum += l.rate
				minShare = math.Min(minShare, l.rate)
			}
		}
		if sum < 0.97*C || minShare < 0.5*C/float64(n) || maxQ > 2*T {
			t.Fatalf("%d links: sum %.1f Mbit/s, least link %.2f (fair %.2f), queue at most %v",
				n, sum/bMbit, minShare/bMbit, C/float64(n)/bMbit, maxQ)
		}
	}
}

// A sender with less to send than its budget (app-limited) keeps it, within
// twice the target at its 2 s maximum; a standing queue it did not make by
// the budget (BBR's own window) brings B down to the target.
func TestBudgetNotLimited(t *testing.T) {
	r := 72 * time.Millisecond
	T := budgetTarget()
	var c budgetCore
	in := budgetIn{On: true, Delivery: 50 * bMbit, SRTT: r, MinRTT: r, TxRate: 50 * bMbit, MSS: 1448, DlvMax: 50 * bMbit}
	c.step(in)
	in.SRTT = r + 20*time.Millisecond
	c.step(in) // the start ends: a queue, no growth
	in.SRTT = r
	for i := 0; i < 20; i++ {
		c.step(in)
	}
	capB := budgetCapK * 50 * bMbit * (r + T).Seconds()
	if c.b > capB*1.001 {
		t.Fatalf("B %.0f KiB unused, past %.0f", c.b/1024, capB/1024)
	}
	in.SRTT = r + 30*time.Millisecond // BBR's standing queue, B not binding
	c.step(in)
	if want := 50 * bMbit * (r + T).Seconds(); c.b > want*1.001 {
		t.Fatalf("B %.0f KiB with a queue of 30 ms it did not hold, want the target's %.0f", c.b/1024, want/1024)
	}
}

// Idle (C4-lite's lift) lifts the budget, and the next busy spell starts
// again; a reading with nothing to judge by holds B.
func TestBudgetIdleAndHold(t *testing.T) {
	r := 72 * time.Millisecond
	var c budgetCore
	in := budgetIn{On: true, Delivery: 10 * bMbit, SRTT: r, MinRTT: r, TxRate: 10 * bMbit, MSS: 1448, DlvMax: 10 * bMbit}
	if b := c.step(in); b <= 0 || c.mode != budgetStart {
		t.Fatalf("armed: B %.0f, mode %v", b, c.mode)
	}
	b0 := c.b
	c.step(budgetIn{On: true, SRTT: r, MinRTT: r}) // no delivery sample, the ring keeps one
	if c.b != b0 {
		t.Fatalf("B moved from %.0f to %.0f with no sample", b0, c.b)
	}
	cuts := c.cuts
	if b := c.step(budgetIn{On: false}); b != 0 || c.mode != budgetIdle || c.cuts != cuts {
		t.Fatalf("idle: B %.0f, mode %v, cuts %d (were %d)", b, c.mode, c.cuts, cuts)
	}
	if b := c.step(in); b <= 0 || c.mode != budgetStart {
		t.Fatalf("busy again: B %.0f, mode %v", b, c.mode)
	}
}

// The gate: open with no budget, with room, and with nothing in flight (a
// frame larger than B still goes); else a wait of the deficit at the rate,
// within budgetWaitMin and budgetWaitMax, the link marked held; an outq
// error opens it.
func TestBudgetGate(t *testing.T) {
	outq, oerr := 0, error(nil)
	lb := newLinkBudget(func() (int, error) { return outq, oerr })
	if lb.gate(1000) != 0 {
		t.Fatal("a gate with no budget held a frame")
	}
	lb.b.Store(100 << 10)
	lb.rate.Store(int64(10 * bMbit))
	outq = 50 << 10
	if lb.gate(16<<10) != 0 || lb.blocked.Load() {
		t.Fatal("held with room")
	}
	outq = 0
	if lb.gate(200<<10) != 0 {
		t.Fatal("held a large frame with nothing in flight")
	}
	outq = 95 << 10
	w := lb.gate(16 << 10)
	want := time.Duration((float64(11<<10) + float64(100<<10)/budgetWaitBeyond) / (10 * bMbit) * float64(time.Second))
	if w != min(max(want, budgetWaitMin), budgetWaitMax) || !lb.blocked.Load() || lb.held.Load() != 1 {
		t.Fatalf("wait %v (want %v), blocked %v, held %d", w, want, lb.blocked.Load(), lb.held.Load())
	}
	lb.rate.Store(int64(10000 * bMbit))
	if w := lb.gate(16 << 10); w != budgetWaitMin {
		t.Fatalf("wait %v at 10 Gbit/s, want the least, %v", w, budgetWaitMin)
	}
	lb.rate.Store(0)
	if w := lb.gate(16 << 10); w != budgetWaitMax {
		t.Fatalf("wait %v at an unknown rate, want the most, %v", w, budgetWaitMax)
	}
	oerr = errors.New("gone")
	if lb.gate(16<<10) != 0 {
		t.Fatal("held on an outq error")
	}
	if newLinkBudget(nil) != nil {
		t.Fatal("a budget without SIOCOUTQ")
	}
}

// The runner's step: the rates from the socket's counters, the QUIET link's
// one-way delays, B and the gate's rate set, the held mark taken; the
// status line.
func TestLinkBudgetStep(t *testing.T) {
	defer func(p *owdLink) { quietOWD.Store(p) }(quietOWD.Load())
	o := &owdLink{}
	now := ctrlNow()
	o.qOut.Store(int64(time.Millisecond))
	o.qOutAt.Store(now)
	o.qIn.Store(int64(3 * time.Millisecond))
	o.qInAt.Store(now)
	quietOWD.Store(o)
	lb := newLinkBudget(func() (int, error) { return 0, nil })
	lb.blocked.Store(true)
	t0 := 10 * time.Second
	ts := tcpStat{deliveryRate: uint64(20 * bMbit), rtt: 80000, minRTT: 72000, bytesAcked: 1000, bytesReceived: 500, sndMSS: 1448}
	lb.step(t0, true, ts, 20*bMbit, nil)
	ts.bytesAcked += uint64(2 * bMbit / 10)
	ts.bytesReceived += uint64(1 * bMbit / 10)
	lb.blocked.Store(true)
	pace := lb.step(t0+100*time.Millisecond, true, ts, 20*bMbit, nil)
	if lb.blocked.Load() {
		t.Fatal("the held mark was not taken")
	}
	if lb.b.Load() <= 0 || lb.rate.Load() <= 0 {
		t.Fatalf("B %d, rate %d", lb.b.Load(), lb.rate.Load())
	}
	// srtt − min_rtt 8 ms; the way back: two-way (1 of 2 Mbit/s) so
	// min(4, T), and the QUIET link's 3 ms
	if lb.core.qRev != max(min(4*time.Millisecond, budgetTarget()), 3*time.Millisecond) {
		t.Fatalf("way back %v", lb.core.qRev)
	}
	// the pacing cap: BudgetPaceK of its rate at the target
	if want := float64(BudgetPaceK) / 100 * float64(lb.b.Load()) / (lb.core.base + budgetTarget()).Seconds(); math.Abs(float64(pace)-want) > 0.01*want {
		t.Fatalf("pace %d, want %.0f", pace, want)
	}
	if s := lb.line(3); s == "" {
		t.Fatal("no line")
	}
	// the link's own one-way delays, both fresh: the way back is theirs
	own := &owdLink{}
	own.qOut.Store(int64(500 * time.Microsecond))
	own.qOutAt.Store(ctrlNow())
	own.qIn.Store(int64(7 * time.Millisecond))
	own.qInAt.Store(ctrlNow())
	ts.bytesAcked += uint64(2 * bMbit / 10)
	ts.bytesReceived += uint64(1 * bMbit / 10)
	lb.step(t0+200*time.Millisecond, true, ts, 20*bMbit, own)
	// S 8 ms split 7 : 0.5 by them: the way back 7.47 ms, this direction's
	// 0.53, under the QUIET link's 1 ms
	s := 8 * time.Millisecond
	wantRev := time.Duration(float64(s) * float64(7*time.Millisecond) / float64(7500*time.Microsecond))
	if !lb.core.own || lb.core.qRev != wantRev || lb.core.q != time.Millisecond {
		t.Fatalf("own delays: own %v, way back %v, queue %v; want %v and 1 ms (the QUIET link's)", lb.core.own, lb.core.qRev, lb.core.q, wantRev)
	}
	if p := lb.step(t0+300*time.Millisecond, false, ts, 20*bMbit, nil); p != 0 {
		t.Fatalf("pace %d idle", p)
	}
	if lb.b.Load() != 0 {
		t.Fatalf("B %d idle", lb.b.Load())
	}
}

// A BULK link's own beat (γ2): 50 ms at least, and a message only while
// this end's data is acknowledged at owdOwnBusy.
func TestOWDOwnBeat(t *testing.T) {
	o := &owdLink{own: true}
	o.noteRate(1e9 / 8) // 1 Gbit/s: the 1% rule's beat is its 25 ms floor
	if tk := o.tick(); tk != owdOwnTickMin {
		t.Fatalf("own beat %v, want %v", tk, owdOwnTickMin)
	}
	if o.own = false; o.tick() != owdTickMin {
		t.Fatalf("QUIET beat %v", o.tick())
	}
	o.own = true
	ts := tcpStat{bytesAcked: 1 << 20}
	if o.busy(ts, true) {
		t.Fatal("busy at the first look")
	}
	o.at0 -= int64(100 * time.Millisecond)
	if o.busy(ts, true) {
		t.Fatal("busy with nothing acknowledged")
	}
	o.at0 -= int64(100 * time.Millisecond)
	ts.bytesAcked += 64 << 10 // 640 KiB/s over 100 ms
	if !o.busy(ts, true) {
		t.Fatal("not busy at 640 KiB/s")
	}
	if o.busy(ts, false) {
		t.Fatal("busy with no reading")
	}
}

// A BULK link's own reading (γ2) is the middle of the window's readings,
// not its trough (a QUIET link keeps the least): a queue swinging between
// 1 and 30 ms reads about 15, where the least says 1.
func TestOWDOwnMedian(t *testing.T) {
	var r owdRecent
	if r.median() != 0 {
		t.Fatal("a median with no readings")
	}
	for _, own := range []bool{false, true} {
		o := &owdLink{own: own}
		now := int64(time.Hour)
		var m [owdMsgLen]byte
		// the base first: a message with no queue
		owdEncode(m[:], now-int64(70*time.Millisecond), 0, false)
		o.recv(now, m[:])
		for i, q := range []time.Duration{1, 30, 15, 22, 8, 12, 27} {
			at := now + int64(i+1)*int64(20*time.Millisecond)
			owdEncode(m[:], at-int64(70*time.Millisecond)-int64(q*time.Millisecond), 0, false)
			o.recv(at, m[:])
		}
		got := time.Duration(o.qIn.Load())
		want := time.Duration(0) // the least of the window (the base reading is in it)
		if own {
			want = 12 * time.Millisecond // 0 1 8 12 15 22 27 30: the lower middle
		}
		if got != want {
			t.Fatalf("own %v: read %v, want %v", own, got, want)
		}
	}
}
