package engine

import (
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// α's tests (round 2, decision T18; sendcap.go): its arithmetic reading by
// reading, its runner on a synthetic socket, and a fluid model of a
// bottleneck it holds at CoDel's target.

// alphaFeed is one reading of a busy socket for α: delivery dlv, srtt and
// min_rtt, side bytes/s on QUIET, rcvd bytes/s received, over 100 ms.
type alphaSock struct {
	now                time.Duration
	acked, wrote, side uint64
	rcvd               uint64
	segs               uint32
}

func (k *alphaSock) read(c *sendCap, dlv float64, srtt, minRTT time.Duration, side, rcvd float64) (uint64, bool, bool) {
	k.now += 100 * time.Millisecond
	b := uint64(dlv / 10)
	k.acked += b
	k.wrote += b
	k.segs += uint32(b/1448) + 1
	k.side += uint64(side / 10)
	k.rcvd += uint64(rcvd / 10)
	return c.Update(capSample{Now: k.now, Delivery: dlv, Acked: k.acked, Written: k.wrote, Segs: k.segs,
		SRTT: srtt, MinRTT: minRTT, Side: k.side, Rcvd: k.rcvd})
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(math.Abs(a), math.Abs(b))+1 }

// A queue past the target cuts the cap from the lesser of itself and the
// delivery rate by the excess's share of the round trip (at most half);
// under half the target it grows 4% a reading, under the target 1%.
func TestAlphaCutAndGrow(t *testing.T) {
	c := sendCap{alpha: true}
	var k alphaSock
	const dlv = 10e6
	mrtt := 72 * time.Millisecond
	// the first fresh reading (the second: acks moved) turns it on, from
	// the delivery rate, and moves it
	k.read(&c, dlv, mrtt+time.Millisecond, mrtt, 0, 0)
	if c.on {
		t.Fatal("on at the first reading")
	}
	k.read(&c, dlv, mrtt+time.Millisecond, mrtt, 0, 0)
	if !near(c.ar, dlv*1.04) {
		t.Fatalf("on with no queue: %.0f, want %.0f", c.ar, dlv*1.04)
	}
	// 40 ms of queue at 112 ms srtt: down by (40-5)/112 from the delivery
	// rate, the lesser
	k.read(&c, dlv, mrtt+40*time.Millisecond, mrtt, 0, 0)
	if want := dlv * (1 - 35.0/112); !near(c.ar, want) {
		t.Fatalf("40 ms of queue: %.0f, want %.0f", c.ar, want)
	}
	// a queue past a round trip cuts by half at most
	prev := c.ar
	k.read(&c, dlv, mrtt+200*time.Millisecond, mrtt, 0, 0)
	if !near(c.ar, prev*alphaCutMax) {
		t.Fatalf("200 ms of queue: %.0f, want %.0f", c.ar, prev*alphaCutMax)
	}
	// 4 ms: under the target, over half of it
	prev = c.ar
	k.read(&c, dlv, mrtt+4*time.Millisecond, mrtt, 0, 0)
	if !near(c.ar, prev*1.01) {
		t.Fatalf("4 ms of queue: %.0f, want %.0f", c.ar, prev*1.01)
	}
	// the status line
	l := &sendCapLink{core: c}
	if s := l.lineOf("3 bulk"); !strings.HasPrefix(s, "sendcap link 3 bulk: alpha cap ") || !strings.Contains(s, "queue 4.0 ms, target 5.0 ms") {
		t.Fatalf("line %q", s)
	}
}

// The target is CoDel's 5 ms, or two full packets' time where that is
// longer (a slow path: 24 ms at 1 Mbit/s).
func TestAlphaTargetSlowPath(t *testing.T) {
	for _, x := range []struct {
		mbit float64
		want time.Duration
	}{{1, 24224 * time.Microsecond}, {2.5, 9689600 * time.Nanosecond}, {5, alphaTarget}, {100, alphaTarget}} {
		got := alphaTargetAt(x.mbit * mbit)
		if math.Abs(float64(got-x.want)) > float64(time.Microsecond) {
			t.Fatalf("%.1f Mbit/s: target %v, want %v", x.mbit, got, x.want)
		}
	}
	// at 1 Mbit/s 20 ms of queue (under 2 packets) is no cause to cut
	c := sendCap{alpha: true}
	var k alphaSock
	k.read(&c, 125000, 92*time.Millisecond, 72*time.Millisecond, 0, 0)
	k.read(&c, 125000, 92*time.Millisecond, 72*time.Millisecond, 0, 0)
	if c.ar < 125000 {
		t.Fatalf("20 ms of queue at 1 Mbit/s cut the cap to %.0f", c.ar)
	}
}

// Never past 1.25 times the largest delivery rate of 2 s less the
// headroom (QUIET's bytes and the ACKs of what the socket receives), never
// under the floor.
func TestAlphaCeilAndFloor(t *testing.T) {
	c := sendCap{alpha: true}
	var k alphaSock
	const dlv = 1e6
	for range 40 {
		k.read(&c, dlv, 72*time.Millisecond, 72*time.Millisecond, 50e3, 2e6)
	}
	head := 50e3 + alphaAckShare*2e6
	if want := alphaCeil*dlv - head; !near(c.ar, want) || !near(c.aHead, head) {
		t.Fatalf("held at %.0f (headroom %.0f), want the ceiling %.0f (%.0f)", c.ar, c.aHead, want, head)
	}
	for range 40 {
		k.read(&c, 20e3, 572*time.Millisecond, 72*time.Millisecond, 0, 0)
	}
	if c.ar != alphaFloor {
		t.Fatalf("under a standing queue at 20 KB/s: %.0f, want the floor %d", c.ar, alphaFloor)
	}
}

// Lifted when the socket goes idle (C4-lite's rule), and on again from the
// delivery rate, not from where it was.
func TestAlphaLiftAndRestart(t *testing.T) {
	c := sendCap{alpha: true}
	k := capSock{srtt: 72 * time.Millisecond}
	k.feed(&c, 10, 10e6, false)
	if c.set == 0 {
		t.Fatal("no cap on a busy socket")
	}
	if k.trickle(&c, 30, 100, 0); !k.anyLft || c.set != 0 {
		t.Fatalf("an idle socket kept its cap %d", c.set)
	}
	if c.ar != 0 {
		t.Fatalf("α's rate %f kept over the idle span", c.ar)
	}
	k.feed(&c, 1, 2e6, false)
	if c.ar <= 0 || c.ar > 2e6*1.25 {
		t.Fatalf("restarted at %.0f, want from 2e6", c.ar)
	}
}

// The runner sets α's cap on the socket it reads (TCP_INFO's srtt and
// min_rtt), with QUIET's bytes in the headroom; a C4-lite socket beside it
// keeps 1.25 times its delivery rate.
func TestAlphaRunner(t *testing.T) {
	sys := &fakeCapSys{ok: true, st: tcpStat{dlvOK: true, sndMSS: 1448, rtt: 112000, minRTT: 72000, deliveryRate: 10e6}}
	l := newSendCapLink(sys, nil)
	l.core.linked, l.core.alpha = true, true
	var side uint64
	l.side = func() uint64 { return side }
	plain := newSendCapLink(&fakeCapSys{ok: true, st: sys.st}, nil)
	plain.core.linked = true
	written := uint64(0)
	for i := 1; i <= 3; i++ {
		sys.st.bytesAcked += 1e6
		plain.sys.(*fakeCapSys).st.bytesAcked += 1e6
		written += 1e6
		side += 1e4
		now := time.Duration(i) * 100 * time.Millisecond
		l.step(now, written)
		plain.step(now, written)
	}
	if len(sys.sets) == 0 || sys.sets[len(sys.sets)-1] >= 10e6 {
		t.Fatalf("α's caps on the socket %v, want under the delivery rate (40 ms of queue)", sys.sets)
	}
	if ps := plain.sys.(*fakeCapSys).sets; len(ps) == 0 || ps[len(ps)-1] != uint64(1.25*10e6) {
		t.Fatalf("C4-lite's %v, want 1.25 x 10 MB/s", ps)
	}
}

// alphaFluid runs α over a fluid model of a FIFO bottleneck of c bytes/s
// and round trip rtt, for secs: the sender (a BBR held by the cap) sends at
// the cap, or twice the path without one; side bytes/s of the link's QUIET
// share the bottleneck; srtt follows the queue's delay (TCP's smoothing,
// about a quarter round trip); the delivery rate is what the bottleneck
// served this socket over the last reading. It returns the queue's
// median and 90th percentile over the last two thirds, and the socket's
// share of the path then.
func alphaFluid(c float64, rtt time.Duration, side float64, secs float64) (q50, q90 time.Duration, share float64) {
	cp := sendCap{alpha: true, linked: true}
	const dt = time.Millisecond
	var queue float64 // bytes waiting at the bottleneck
	srtt := rtt.Seconds()
	var cap float64
	var acked, wrote, sideTot uint64
	var segs uint32
	var servedRead, servedTail float64
	var qs []float64
	steps := int(secs * 1000)
	for i := 1; i <= steps; i++ {
		send := 2 * c
		if cap > 0 {
			send = cap
		}
		in := (send + side) * dt.Seconds()
		out := math.Min(queue+in, c*dt.Seconds())
		// what left: shared in proportion to what came in
		mine := out * send / (send + side)
		queue += in - out
		servedRead += mine
		acked += uint64(mine)
		wrote += uint64(send * dt.Seconds())
		sideTot += uint64(side * dt.Seconds())
		segs += uint32(mine/1448) + 1
		target := rtt.Seconds() + queue/c
		srtt += (target - srtt) * math.Min(1, dt.Seconds()/(rtt.Seconds()/4))
		if i > steps/3 {
			qs = append(qs, queue/c)
			servedTail += mine
		}
		if i%100 == 0 {
			now := time.Duration(i) * dt
			r, apply, lift := cp.Update(capSample{Now: now, Delivery: servedRead / 0.1, Acked: acked, Written: wrote, Segs: segs,
				SRTT: time.Duration(srtt * float64(time.Second)), MinRTT: rtt, Side: sideTot})
			switch {
			case apply:
				cap = float64(r)
			case lift:
				cap = 0
			}
			servedRead = 0
		}
	}
	sort.Float64s(qs)
	q50 = time.Duration(qs[len(qs)/2] * float64(time.Second))
	q90 = time.Duration(qs[len(qs)*9/10] * float64(time.Second))
	share = servedTail / (c * secs * 2 / 3)
	return q50, q90, share
}

// On a FIFO bottleneck that BBR alone would fill (twice its rate offered),
// α holds the queue near its target at every rate, and the socket keeps the
// path but for what QUIET takes (1% here).
func TestAlphaFluidHoldsQueue(t *testing.T) {
	for _, mb := range []float64{1, 5, 25, 100} {
		c := mb * mbit
		side := 0.01 * c
		q50, q90, share := alphaFluid(c, 72*time.Millisecond, side, 30)
		tgt := alphaTargetAt(c)
		t.Logf("%5.0f Mbit/s: queue p50 %v p90 %v (target %v); its share %.3f of the path (QUIET %.3f)",
			mb, q50.Round(100*time.Microsecond), q90.Round(100*time.Microsecond), tgt.Round(100*time.Microsecond), share, side/c)
		if q50 > 2*tgt || q90 > 4*tgt {
			t.Fatalf("%.0f Mbit/s: queue p50 %v p90 %v, against a target of %v", mb, q50, q90, tgt)
		}
		if share < 0.9*(1-side/c) {
			t.Fatalf("%.0f Mbit/s: the socket had %.3f of the path, want >= 0.9 of what QUIET leaves", mb, share)
		}
	}
}
