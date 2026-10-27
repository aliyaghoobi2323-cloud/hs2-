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

// Its own queue past the target for 300 ms (three readings: a standing
// one) cuts the cap from the lesser of itself and the delivery rate, less
// the headroom, by the excess's share of the round trip (at most half, and
// never under half the largest delivery of the base's span), once a round
// trip at most; a reading past the target that does not stand holds it;
// under half the target it grows 3% a reading, under the target 1%. Its
// own: srtt − min_rtt less the least of that over the last 12 s, the base.
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
	if !near(c.ar, dlv*1.03) {
		t.Fatalf("on with no queue: %.0f, want %.0f", c.ar, dlv*1.03)
	}
	// 40 ms of queue, two readings: not standing yet, held
	prev := c.ar
	k.read(&c, dlv, mrtt+40*time.Millisecond, mrtt, 0, 0)
	k.read(&c, dlv, mrtt+40*time.Millisecond, mrtt, 0, 0)
	if c.ar != prev {
		t.Fatalf("a queue of 200 ms moved the cap: %.0f, was %.0f", c.ar, prev)
	}
	// the third: standing, 39 ms over the base of 1 ms at 112 ms srtt:
	// down by (39-5)/112 from the delivery rate (the lesser) less QUIET's
	// 1 MB/s
	k.read(&c, dlv, mrtt+40*time.Millisecond, mrtt, 1e6, 0)
	if want := (dlv - 1e6) * (1 - 34.0/112); !near(c.ar, want) {
		t.Fatalf("40 ms standing: %.0f, want %.0f", c.ar, want)
	}
	// within the round trip after it, no second cut
	prev = c.ar
	k.read(&c, dlv, mrtt+200*time.Millisecond, mrtt, 0, 0)
	if c.ar != prev {
		t.Fatalf("a second cut within a round trip: %.0f, was %.0f", c.ar, prev)
	}
	// a round trip on: cut by what has stood (the least of the last three
	// readings: 39 ms) at 272 ms srtt
	k.now += 200 * time.Millisecond
	k.read(&c, dlv, mrtt+200*time.Millisecond, mrtt, 0, 0)
	if want := prev * (1 - 34.0/272); !near(c.ar, want) {
		t.Fatalf("200 ms of queue, 39 standing: %.0f, want %.0f", c.ar, want)
	}
	// 199 ms standing: by half at most, and never under half the largest
	// delivery of the span
	k.now += 300 * time.Millisecond
	k.read(&c, dlv, mrtt+300*time.Millisecond, mrtt, 0, 0)
	if !near(c.ar, alphaKeep*dlv) {
		t.Fatalf("a third cut: %.0f, want no less than %.0f", c.ar, alphaKeep*dlv)
	}
	// 4 ms over the base of 1: under the target, over half of it
	prev = c.ar
	k.read(&c, dlv, mrtt+4*time.Millisecond, mrtt, 0, 0)
	if !near(c.ar, prev*1.01) {
		t.Fatalf("3 ms of own queue: %.0f, want %.0f", c.ar, prev*1.01)
	}
	// the status line
	l := &sendCapLink{core: c}
	if s := l.lineOf("3 bulk"); !strings.HasPrefix(s, "sendcap link 3 bulk: alpha cap ") || !strings.Contains(s, "own queue 3.0 ms over a base of 1.0, target 5.0 ms") {
		t.Fatalf("line %q", s)
	}
}

// A queue that stands whatever this end sends (what the other end's bulk
// keeps queued, which this end's ACKs wait behind) is the base, not its own:
// no cut, the cap grows. It starved the download on the rig.
func TestAlphaStandingQueueIsBase(t *testing.T) {
	c := sendCap{alpha: true}
	var k alphaSock
	const dlv = 10e6
	for range 30 {
		k.read(&c, dlv, 80*time.Millisecond, 72*time.Millisecond, 0, 0) // 8 ms, always
	}
	if c.ar < dlv || c.aQ != 0 || c.aBase != 8*time.Millisecond {
		t.Fatalf("under a standing 8 ms: cap %.0f, own queue %v over %v", c.ar, c.aQ, c.aBase)
	}
	// the base is the least of the last 12 s: an old low reading goes
	for range 130 {
		k.read(&c, dlv, 84*time.Millisecond, 72*time.Millisecond, 0, 0)
	}
	if c.aBase != 12*time.Millisecond {
		t.Fatalf("base %v after 13 s at 12 ms", c.aBase)
	}
	// and it is three targets at most: a larger standing queue (BBR's
	// start-up one) is this end's to drain
	for range 130 {
		k.read(&c, dlv, 102*time.Millisecond, 72*time.Millisecond, 0, 0)
	}
	if c.aBase != 3*alphaTarget || c.aQ != 15*time.Millisecond {
		t.Fatalf("base %v, own queue %v after 13 s at 30 ms", c.aBase, c.aQ)
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
	// at 1 Mbit/s 20 ms of own queue (under 2 packets) is no cause to cut
	c := sendCap{alpha: true}
	var k alphaSock
	k.read(&c, 125000, 72*time.Millisecond, 72*time.Millisecond, 0, 0)
	k.read(&c, 125000, 72*time.Millisecond, 72*time.Millisecond, 0, 0)
	k.read(&c, 125000, 92*time.Millisecond, 72*time.Millisecond, 0, 0)
	if c.ar < 125000 {
		t.Fatalf("20 ms of queue at 1 Mbit/s cut the cap to %.0f", c.ar)
	}
}

// Never past 1.25 times the largest delivery rate of 2 s (the headroom
// deepens a cut, it does not hold the cap down: below its delivery, less
// what the other direction's ACKs take, a starved socket never grew back),
// never under the floor.
func TestAlphaCeilAndFloor(t *testing.T) {
	c := sendCap{alpha: true}
	var k alphaSock
	const dlv = 1e6
	for range 40 {
		k.read(&c, dlv, 72*time.Millisecond, 72*time.Millisecond, 50e3, 2e6)
	}
	head := 50e3 + alphaAckShare*2e6
	if want := alphaCeil * dlv; !near(c.ar, want) || !near(c.aHead, head) {
		t.Fatalf("held at %.0f (headroom %.0f), want the ceiling %.0f (%.0f)", c.ar, c.aHead, want, head)
	}
	c2 := sendCap{alpha: true}
	var k2 alphaSock
	k2.read(&c2, 20e3, 72*time.Millisecond, 72*time.Millisecond, 0, 0)
	k2.read(&c2, 20e3, 72*time.Millisecond, 72*time.Millisecond, 0, 0)
	for range 3 {
		k2.read(&c2, 20e3, 572*time.Millisecond, 72*time.Millisecond, 0, 0)
	}
	if c2.ar != alphaFloor {
		t.Fatalf("own queue of 500 ms standing at 20 KB/s: %.0f, want the floor %d", c2.ar, alphaFloor)
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
	sys := &fakeCapSys{ok: true, st: tcpStat{dlvOK: true, sndMSS: 1448, rtt: 72000, minRTT: 72000, deliveryRate: 10e6}}
	l := newSendCapLink(sys, nil)
	l.core.linked, l.core.alpha = true, true
	var side uint64
	l.side = func() uint64 { return side }
	plain := newSendCapLink(&fakeCapSys{ok: true, st: sys.st}, nil)
	plain.core.linked = true
	written := uint64(0)
	for i := 1; i <= 6; i++ {
		// no queue at first, then 40 ms of its own, standing
		if i == 4 {
			sys.st.rtt = 112000
		}
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
	return alphaFluidX(alphaPath{c: c, rtt: rtt, side: side, secs: secs})
}

// alphaPath is the fluid model's path: rate c, round trip rtt, side
// bytes/s of QUIET through the same bottleneck, rcvd bytes/s the socket
// receives (the other direction's bulk: ACKs to send, α's headroom), bias
// added to every round trip the socket reads (the other direction's queue
// and its host's pacing, which the ACKs wait behind), for secs.
type alphaPath struct {
	c          float64
	rtt, bias  time.Duration
	side, rcvd float64
	secs       float64
}

func alphaFluidX(p alphaPath) (q50, q90 time.Duration, share float64) {
	c, rtt, side, secs := p.c, p.rtt, p.side, p.secs
	cp := sendCap{alpha: true, linked: true}
	var rcvdTot uint64
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
		rcvdTot += uint64(p.rcvd * dt.Seconds())
		segs += uint32(mine/1448) + 1
		target := rtt.Seconds() + p.bias.Seconds() + queue/c
		srtt += (target - srtt) * math.Min(1, dt.Seconds()/(rtt.Seconds()/4))
		if i > steps/3 {
			qs = append(qs, queue/c)
			servedTail += mine
		}
		if i%100 == 0 {
			now := time.Duration(i) * dt
			r, apply, lift := cp.Update(capSample{Now: now, Delivery: servedRead / 0.1, Acked: acked, Written: wrote, Segs: segs,
				SRTT: time.Duration(srtt * float64(time.Second)), MinRTT: rtt, Side: sideTot, Rcvd: rcvdTot})
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
// α holds its queue near its target at every rate, and the socket keeps the
// path but for what QUIET takes (1% here); also beside the other
// direction's bulk (85% of the path received: ACKs to send) whose standing
// queue adds 0 to 10 ms to every round trip it reads.
func TestAlphaFluidHoldsQueue(t *testing.T) {
	for _, bias := range []time.Duration{0, 4 * time.Millisecond, 10 * time.Millisecond} {
		for _, mb := range []float64{1, 5, 25, 100} {
			c := mb * mbit
			side, rcvd := 0.01*c, 0.0
			if bias > 0 {
				rcvd = 0.85 * c
			}
			q50, q90, share := alphaFluidX(alphaPath{c: c, rtt: 72 * time.Millisecond, bias: bias, side: side, rcvd: rcvd, secs: 30})
			tgt := alphaTargetAt(c)
			t.Logf("%5.0f Mbit/s, other direction's queue %v: own queue p50 %v p90 %v (target %v); its share %.3f of the path (QUIET %.3f)",
				mb, bias, q50.Round(100*time.Microsecond), q90.Round(100*time.Microsecond), tgt.Round(100*time.Microsecond), share, side/c)
			if q50 > 2*tgt || q90 > 4*tgt {
				t.Fatalf("%.0f Mbit/s, %v: queue p50 %v p90 %v, against a target of %v", mb, bias, q50, q90, tgt)
			}
			if share < 0.9*(1-side/c) {
				t.Fatalf("%.0f Mbit/s, %v: the socket had %.3f of the path, want >= 0.9 of what QUIET leaves", mb, bias, share)
			}
		}
	}
}

// alphaDuplex runs α at both ends of one BULK connection over a fluid model
// of a path of c bytes/s each way, round trip rtt: each end's bulk queues at
// its direction's bottleneck, and each end's round trips hold the other
// direction's queue too (its ACKs ride the other end's bulk, the same
// flow). Each end offers twice the path (BBR uncapped); up starts upAt
// before down. It returns each direction's share of the path over the last
// two thirds, and its queue's median.
func alphaDuplex(c float64, rtt time.Duration, secs float64, upAt time.Duration) (down, up float64, qd, qu time.Duration) {
	type end struct {
		cp                 sendCap
		cap, queue         float64
		srtt               float64
		served, tail       float64
		acked, wrote, rcvd uint64
		segs               uint32
		qs                 []float64
		start              time.Duration
	}
	e := [2]*end{{start: upAt}, {}} // 0: down, 1: up
	for _, x := range e {
		x.cp = sendCap{alpha: true, linked: true}
		x.srtt = rtt.Seconds()
	}
	const dt = time.Millisecond
	steps := int(secs * 1000)
	for i := 1; i <= steps; i++ {
		now := time.Duration(i) * dt
		for k, x := range e {
			send := 0.0
			if now >= x.start {
				send = 2 * c
				if x.cap > 0 {
					send = x.cap
				}
			}
			in := send * dt.Seconds()
			out := math.Min(x.queue+in, c*dt.Seconds())
			x.queue += in - out
			x.served += out
			x.acked += uint64(out)
			x.wrote += uint64(in)
			x.segs += uint32(out/1448) + 1
			e[1-k].rcvd += uint64(out)
			if i > steps/3 {
				x.tail += out
				x.qs = append(x.qs, x.queue/c)
			}
		}
		for k, x := range e {
			target := rtt.Seconds() + x.queue/c + e[1-k].queue/c
			x.srtt += (target - x.srtt) * math.Min(1, dt.Seconds()/(rtt.Seconds()/4))
			if i%100 == 0 && now >= x.start {
				r, apply, lift := x.cp.Update(capSample{Now: now, Delivery: x.served / 0.1, Acked: x.acked, Written: x.wrote, Segs: x.segs,
					SRTT: time.Duration(x.srtt * float64(time.Second)), MinRTT: rtt, Rcvd: x.rcvd})
				switch {
				case apply:
					x.cap = float64(r)
				case lift:
					x.cap = 0
				}
				x.served = 0
			}
		}
	}
	med := func(v []float64) time.Duration {
		sort.Float64s(v)
		return time.Duration(v[len(v)/2] * float64(time.Second))
	}
	tail := c * secs * 2 / 3
	return e[0].tail / tail, e[1].tail / tail, med(e[0].qs), med(e[1].qs)
}

// α at both ends of one BULK connection, each end's round trips holding
// the other direction's queue too (its ACKs wait behind it): neither
// direction starves, whichever starts first (the first build of α left the
// download at 3.8 of 25 Mbit/s beside the upload on the rig; this model
// had it at 0.01 to 0.13 of the path).
func TestAlphaDuplexNoStarvation(t *testing.T) {
	for _, mb := range []float64{5, 25, 100} {
		for _, at := range []time.Duration{0, 500 * time.Millisecond, 2 * time.Second} {
			d, u, qd, qu := alphaDuplex(mb*mbit, 72*time.Millisecond, 30, at)
			t.Logf("%5.0f Mbit/s, up %v ahead: down %.3f up %.3f of the path; queues %v / %v", mb, at, d, u, qd.Round(100*time.Microsecond), qu.Round(100*time.Microsecond))
			if d < 0.9 || u < 0.9 {
				t.Fatalf("%.0f Mbit/s, up %v ahead: down %.3f up %.3f of the path, want >= 0.9 each", mb, at, d, u)
			}
		}
	}
}
