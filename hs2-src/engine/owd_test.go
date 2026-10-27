package engine

import (
	"context"
	"math"
	"net"
	"testing"
	"time"
)

// The base of the one-way delay: the least raw delay of the span, whatever
// the clocks' offset; a sample past it is queue; the base forgets a
// second once owdWin seconds passed.
func TestOWDBase(t *testing.T) {
	const off = int64(-123456789012) // the other end's clock: any offset
	var b owdBase
	ms := int64(time.Millisecond)
	if q := b.add(100, off+72*ms); q != 0 {
		t.Fatalf("first sample: queue %d, want 0", q)
	}
	if q := b.add(100, off+80*ms); q != 8*ms {
		t.Fatalf("8 ms over the base: %d", q)
	}
	if q := b.add(101, off+75*ms); q != 3*ms {
		t.Fatalf("next second, 3 ms over: %d", q)
	}
	// the base second (100) leaves the span at 100 + owdWin: 75 is the base then
	if q := b.add(100+owdWin, off+76*ms); q != 1*ms {
		t.Fatalf("after the span: queue %d, want 1 ms (base 75)", q)
	}
	// a gap longer than the span starts over
	if q := b.add(200+2*owdWin, off+90*ms); q != 0 {
		t.Fatalf("after a long gap: %d, want 0", q)
	}
}

func TestOWDCodec(t *testing.T) {
	var b [owdMsgLen]byte
	owdEncode(b[:], 987654321, int64(4321*time.Microsecond), true)
	st, q, have, ok := owdDecode(b[:])
	if !ok || !have || st != 987654321 || q != 4321*time.Microsecond {
		t.Fatalf("round trip: %d %v %v %v", st, q, have, ok)
	}
	owdEncode(b[:], 5, 0, false)
	if _, _, have, ok := owdDecode(b[:]); !ok || have {
		t.Fatalf("no queue: have %v ok %v", have, ok)
	}
	b[0] = 2
	if _, _, _, ok := owdDecode(b[:]); ok {
		t.Fatal("a version this build does not know is refused")
	}
}

// The beat costs at most 1% of the link's rate, within 25 ms to 1 s.
func TestOWDTick(t *testing.T) {
	for _, c := range []struct {
		mbit float64
		want time.Duration
	}{{0, time.Second}, {0.5, time.Second}, {1, 800 * time.Millisecond}, {4, 200 * time.Millisecond},
		{8, 100 * time.Millisecond}, {32, 25 * time.Millisecond}, {1000, 25 * time.Millisecond}} {
		if got := owdTickOf(c.mbit * 1e6 / 8); got != c.want {
			t.Fatalf("%.1f Mbit/s: beat %v, want %v", c.mbit, got, c.want)
		}
	}
	var o owdLink
	if o.tick() != owdTickMax {
		t.Fatal("no rate yet: the slow beat")
	}
	o.noteRate(100e6 / 8)
	if o.tick() != owdTickMin {
		t.Fatalf("100 Mbit/s: beat %v", o.tick())
	}
}

// The queue is the least reading of the last owdLook: one message held
// behind a lost segment is no queue, a queue that stands is.
func TestOWDRecent(t *testing.T) {
	var r owdRecent
	ms := int64(time.Millisecond)
	at := int64(time.Hour)
	for i := 0; i < 8; i++ {
		at += 25 * ms
		r.add(at, 1*ms)
	}
	at += 25 * ms
	if q := r.add(at, 80*ms); q != 1*ms {
		t.Fatalf("one held message: queue %v, want 1 ms", time.Duration(q))
	}
	var q int64
	for i := 0; i < 9; i++ {
		at += 25 * ms
		q = r.add(at, 12*ms)
	}
	if q != 12*ms {
		t.Fatalf("a standing 12 ms for 225 ms: queue %v", time.Duration(q))
	}
}

// Two ends over a pipe: each learns the queue of its own sending
// direction from the other's measurement.
func TestOWDLoopPipe(t *testing.T) {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var oa, ob owdLink
	oa.noteRate(100e6 / 8)
	ob.noteRate(100e6 / 8)
	go owdLoop(ctx, a, &oa)
	go owdLoop(ctx, b, &ob)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, okA := oa.queue()
		_, okB := ob.queue()
		if okA && okB {
			if oa.msgs.Load() < 2 || ob.msgs.Load() < 2 {
				t.Fatalf("a queue heard after %d and %d messages", oa.msgs.Load(), ob.msgs.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no one-way queue heard in 5 s")
}

// The judge's two fixes in β2's fluid loop (TestAlphaTwoWays): with the
// one-way delay as its queue, α holds each direction of a duplex link at
// its own path, the 100 down / 20 up path too (TestAlphaTwoWays' open
// item: the download held at the upload's rate on srtt); from a low start
// it fills the path in about a second (fix 1; β2's 3% a reading took
// seconds).
func TestAlphaOWDTwoWays(t *testing.T) {
	const ms = time.Millisecond
	type end struct {
		c         *sendCap
		path, cap float64
		q         time.Duration
		acked     float64
		sum       float64
		fill      int
	}
	run := func(dnPath, upPath, dnFrom float64, useOWD bool) (dn, up *end, worst time.Duration) {
		dn = &end{c: &sendCap{linked: true, alpha: true}, path: dnPath, cap: dnFrom, fill: -1}
		up = &end{c: &sendCap{linked: true, alpha: true}, path: upPath, cap: upPath, fill: -1}
		at := time.Hour
		for i := 0; i < 300; i++ {
			at += 100 * ms
			for _, e := range []*end{dn, up} {
				e.q += time.Duration((e.cap - e.path) / e.path * float64(100*ms))
				e.q = max(e.q, 0)
				e.acked += math.Min(e.cap, e.path) / 10
			}
			for k, e := range []*end{dn, up} {
				o := up
				if k == 1 {
					o = dn
				}
				dlv := math.Min(e.cap, e.path)
				e.c.Update(capSample{Now: at, Delivery: dlv, Acked: uint64(e.acked), Written: uint64(e.acked),
					Segs: uint32(e.acked / 1000), Received: uint64(o.acked),
					SRTT: 72*ms + e.q + o.q, MinRTT: 72 * ms, OWD: e.q, OWDok: useOWD})
				if e.c.own > 0 {
					e.cap = float64(e.c.own)
				}
				if e.fill < 0 && dlv >= 0.9*e.path {
					e.fill = i
				}
				if i >= 200 {
					e.sum += dlv
				}
				if i >= 100 {
					worst = max(worst, e.q)
				}
			}
		}
		return dn, up, worst
	}
	for _, c0 := range []struct{ dn, up, dnFrom float64 }{
		{25e6 / 8, 25e6 / 8, 2e6 / 8}, {5e6 / 8, 5e6 / 8, 1e6 / 8}, {25e6 / 8, 25e6 / 8, 25e6 / 8},
		{100e6 / 8, 20e6 / 8, 10e6 / 8}, {100e6 / 8, 20e6 / 8, 100e6 / 8}, {20e6 / 8, 100e6 / 8, 2e6 / 8},
	} {
		dn, up, worst := run(c0.dn, c0.up, c0.dnFrom, true)
		sdn, sup, _ := run(c0.dn, c0.up, c0.dnFrom, false)
		t.Logf("down %.0f / up %.0f Mbit/s (down from %.0f): one-way: down %.3f, up %.3f of their paths, down filled 0.9 at reading %d, queue at most %v; srtt (β2): down %.3f, up %.3f",
			c0.dn*8/1e6, c0.up*8/1e6, c0.dnFrom*8/1e6, dn.sum/100/dn.path, up.sum/100/up.path, dn.fill, worst,
			sdn.sum/100/sdn.path, sup.sum/100/sup.path)
		if dn.sum/100 < 0.9*dn.path || up.sum/100 < 0.9*up.path || worst > 15*ms {
			t.Fatalf("one-way: down %.3f, up %.3f of their paths, a queue up to %v: want >= 0.9 each and <= 15 ms",
				dn.sum/100/dn.path, up.sum/100/up.path, worst)
		}
		if dn.fill < 0 || dn.fill > 15 {
			t.Fatalf("the download filled 0.9 of its path at reading %d: want within 15 (1.5 s)", dn.fill)
		}
	}
}

// Fix 1 off (HS2_TUNE_ALPHA_START=0): β2's start, slow; on: within 1.5 s.
func TestAlphaStartFix(t *testing.T) {
	const ms = time.Millisecond
	fill := func() int {
		c := &sendCap{linked: true, alpha: true}
		path, cap, acked := 100e6/8, 8e6/8, 0.0
		at := time.Hour
		for i := 0; i < 200; i++ {
			at += 100 * ms
			dlv := math.Min(cap, path)
			acked += dlv / 10
			c.Update(capSample{Now: at, Delivery: dlv, Acked: uint64(acked), Written: uint64(acked),
				Segs: uint32(acked / 1000), SRTT: 72 * ms, MinRTT: 72 * ms, OWDok: true})
			if c.own > 0 {
				cap = float64(c.own)
			}
			if dlv >= 0.9*path {
				return i
			}
		}
		return -1
	}
	on := fill()
	TuneAlphaStart = 0
	off := fill()
	TuneAlphaStart = 1
	t.Logf("0.9 of a 100 Mbit/s path from 8 Mbit/s: reading %d with fix 1, %d without", on, off)
	if on < 0 || on > 15 {
		t.Fatalf("with fix 1 the cap filled the path at reading %d: want within 15", on)
	}
	if off >= 0 && off <= on {
		t.Fatalf("without fix 1 (%d) no slower than with it (%d): the test does not test it", off, on)
	}
}

// A message's stamp moves on by the socket's unsent bytes at its pacing
// rate: 20 KiB unsent at a 9 Mbit/s cap is 18.2 ms (version 2 read it as
// queue).
func TestOWDLocalWait(t *testing.T) {
	if w := owdLocalWait(tcpStat{notsent: 20 << 10, pacingRate: 9e6 / 8}); math.Abs(float64(w)-18.2e6) > 0.1e6 {
		t.Fatalf("20 KiB at 9 Mbit/s: %v", time.Duration(w))
	}
	if w := owdLocalWait(tcpStat{notsent: 1250, deliveryRate: 1.25e6}); w != int64(time.Millisecond) {
		t.Fatalf("no pacing rate: the delivery rate's, 1 ms; got %v", time.Duration(w))
	}
	if owdLocalWait(tcpStat{notsent: 1000}) != 0 || owdLocalWait(tcpStat{pacingRate: 1e6}) != 0 {
		t.Fatal("no rate or nothing unsent: no wait")
	}
}

// α's start holds while the link's one-way delay is not heard yet: an srtt
// holding the other direction's queue (30 ms here) does not end it.
func TestAlphaStartWaitsForOWD(t *testing.T) {
	const ms = time.Millisecond
	c := &sendCap{linked: true, alpha: true}
	at, acked := time.Hour, 0.0
	for i := 0; i < 5; i++ {
		at += 100 * ms
		acked += 1e6
		c.Update(capSample{Now: at, Delivery: 1e7, Acked: uint64(acked), Written: uint64(acked),
			Segs: uint32(acked / 1000), SRTT: 102 * ms, MinRTT: 72 * ms, OWDwait: true})
	}
	if !c.aStart || c.aRate < wcNetKOf()*1e7*0.999 {
		t.Fatalf("no one-way delay yet: start %v, cap %.0f, want the start and C4-lite's %.0f", c.aStart, c.aRate, wcNetKOf()*1e7)
	}
}

// Fix 1's start is judged before the app-limited hold: an app-limited
// reading with a queue of its own past 5 ms ends it, the cap cut to what
// was delivered (the first version held the start, and C4-lite's 1.25 x,
// for good on app-limited readings).
func TestAlphaStartEndsWhenAppLimited(t *testing.T) {
	const ms = time.Millisecond
	c := &sendCap{linked: true, alpha: true}
	at, acked := time.Hour, 0.0
	feed := func(q time.Duration, app bool) {
		at += 100 * ms
		acked += 1e6
		c.Update(capSample{Now: at, Delivery: 1e7, AppLimited: app, Acked: uint64(acked), Written: uint64(acked),
			Segs: uint32(acked / 1000), SRTT: 72 * ms, MinRTT: 72 * ms, OWD: q, OWDok: true})
	}
	feed(0, false)
	feed(0, false)
	if !c.aStart {
		t.Fatal("armed: want the start")
	}
	feed(30*ms, true)
	if c.aStart || c.aRate >= 1e7 {
		t.Fatalf("app-limited with a 30 ms queue of its own: start %v, cap %.0f; want it over and the cap under %.0f", c.aStart, c.aRate, 1e7)
	}
}

// Free growth: when the start ended at a queue BBR's start-up made while it
// delivered a tenth of the path, a one-way queue under a quarter of the
// target for alphaFreeN readings lets the cap grow by alphaGrowFree a
// reading (HS2_TUNE_ALPHA_FREE=0: by a few % from the cut).
func TestAlphaFreeGrowth(t *testing.T) {
	const ms = time.Millisecond
	grow := func() float64 {
		c := &sendCap{linked: true, alpha: true}
		at, acked := time.Hour, 0.0
		path, cap := 100e6/8, 8e6/8
		feed := func(q time.Duration) {
			at += 100 * ms
			dlv := math.Min(cap, path)
			acked += dlv / 10
			c.Update(capSample{Now: at, Delivery: dlv, Acked: uint64(acked), Written: uint64(acked),
				Segs: uint32(acked / 1000), SRTT: 72 * ms, MinRTT: 72 * ms, OWD: q, OWDok: true})
			if c.own > 0 {
				cap = float64(c.own)
			}
		}
		feed(0)
		feed(0)
		feed(20 * ms) // start-up's queue at a tenth of the path: the start ends with a cut
		if c.aStart {
			t.Fatal("a 20 ms queue of its own: want the start over")
		}
		from := c.aRate
		for i := 0; i < 20; i++ {
			feed(0)
		}
		return c.aRate / from
	}
	on := grow()
	AlphaFree = 0
	off := grow()
	AlphaFree = 1
	t.Logf("2 s without a queue after the start's cut: the cap x%.2f with free growth, x%.2f without", on, off)
	if on < 2.5 || off >= 2 {
		t.Fatalf("free growth x%.2f (want >= 2.5), without x%.2f (want < 2)", on, off)
	}
}
