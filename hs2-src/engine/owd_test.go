package engine

import (
	"context"
	"math"
	"net"
	"sync/atomic"
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

// The beat costs at most 1% of the BULK rate, within 100 ms to 1 s.
func TestOWDTick(t *testing.T) {
	for _, c := range []struct {
		mbit float64
		want time.Duration
	}{{0, time.Second}, {0.5, time.Second}, {1, 800 * time.Millisecond}, {4, 200 * time.Millisecond}, {8, 100 * time.Millisecond}, {1000, 100 * time.Millisecond}} {
		if got := owdTickOf(c.mbit * 1e6 / 8); got != c.want {
			t.Fatalf("%.1f Mbit/s: beat %v, want %v", c.mbit, got, c.want)
		}
	}
}

func owdReset() {
	for _, a := range []*atomic.Int64{&owd.qIn, &owd.qInAt, &owd.qOut, &owd.qOutAt, &owd.rate, &owd.rateAt, &owd.msgs} {
		a.Store(0)
	}
}

// Two ends over a pipe: each learns the queue of its own sending
// direction from the other's measurement.
func TestOWDLoopPipe(t *testing.T) {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owdReset()
	defer owdReset()
	go owdLoop(ctx, a)
	go owdLoop(ctx, b)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := owdQueue(); ok {
			if n := owd.msgs.Load(); n < 2 {
				t.Fatalf("a queue heard after %d messages", n)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
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
