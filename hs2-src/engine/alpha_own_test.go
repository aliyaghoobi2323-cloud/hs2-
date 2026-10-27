package engine

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// γ1 (R1 round 3, decision T19): α as the lesser of the shared (QUIET)
// queue's cap and the link's own queue's cap, its probe, and the BULK
// links' own one-way delay loops. The models below are fluid: each link's
// sender fills its cap (a worst case), a bottleneck serves C, and each 100
// ms reading feeds the link's sendCap what it delivered and the one-way
// queues the link's messages would read (the least of the last two steps,
// as owdLook's least of 200 ms).

// fqModel: active busy links on an fq_codel bottleneck of c bytes/s. Each
// link has a flow queue of its own; the bottleneck serves the backlogged
// links an equal share (DRR) and the others what they send; a link's own
// one-way queue is its backlog at its service rate, plus a segment's DRR
// round while it has one; the QUIET link's (shared) queue is none. Each
// flow queue runs CoDel (5 ms target, 100 ms interval: a drop once the
// sojourn stayed past the target an interval, then at interval/√count
// while it stays; the fluid senders do not answer drops, so these count
// what a cap that keeps the queue does not prevent). It returns the mean
// delivered sum over the last 10 s of 30 (of c) and CoDel's drops over the
// last 25 s, as a share of the segments delivered.
func fqModel(c float64, active int, version string) (sum, drops float64) {
	const ms = time.Millisecond
	const dt = 10 * ms
	type link struct {
		sc        *sendCap
		cap, back float64
		q1        time.Duration
		acked     float64
		// CoDel: the time (ms steps) the sojourn has stayed past the target
		// since, dropping, the drops so far and when the next is due
		above, next int
		dropping    bool
		count       int
	}
	ls := make([]*link, active)
	for i := range ls {
		ls[i] = &link{sc: &sendCap{linked: true, alpha: true}, cap: c / float64(active), above: -1}
	}
	at := time.Hour
	var tot, segs, nDrops float64
	for step := 0; step < 300; step++ {
		served := make([]float64, active)
		for k := 0; k < 10; k++ { // 10 ms sub-steps
			now := step*100 + k*10 // ms
			// DRR: the backlogged links share what the sparse ones leave
			// (water-filling over demands: a sparse link asks its cap)
			dem := make([]float64, active)
			for i, l := range ls {
				dem[i] = l.cap
				if l.back > 0 {
					dem[i] = math.Inf(1)
				}
			}
			share := fairFill(c, dem)
			for i, l := range ls {
				in := l.cap * dt.Seconds()
				out := math.Min(share[i]*dt.Seconds(), l.back+in)
				l.back = math.Max(l.back+in-out, 0)
				served[i] += out
				// CoDel on the flow queue
				soj := 0.0
				if l.back > 0 && share[i] > 0 {
					soj = (l.back + ownMSS) / share[i] * 1000
				}
				switch {
				case soj < 5:
					l.above, l.dropping = -1, false
				case l.above < 0:
					l.above = now
				case !l.dropping && now-l.above >= 100:
					l.dropping, l.count = true, l.count/2+1
					l.next = now + int(100/math.Sqrt(float64(l.count)))
					if step >= 50 {
						nDrops++
					}
				case l.dropping && now >= l.next:
					l.count++
					l.next = now + int(100/math.Sqrt(float64(l.count)))
					if step >= 50 {
						nDrops++
					}
				}
			}
		}
		at += 100 * ms
		for i, l := range ls {
			d := served[i] / 0.1
			l.acked += served[i]
			q := time.Duration(0)
			if l.back > 0 && d > 0 {
				q = time.Duration((l.back + ownMSS) / d * float64(time.Second))
			}
			read := min(q, l.q1) // the least of the last two
			if step == 0 {
				read = q
			}
			l.q1 = q
			s := capSample{Now: at, Delivery: d, Acked: uint64(l.acked), Written: uint64(l.acked),
				Segs: uint32(l.acked / 1000), SRTT: 72*ms + q, MinRTT: 72 * ms}
			switch version {
			case "v4": // round 2's version 4: its own queue as the one signal
				s.OWD, s.OWDok = read, true
			case "v5": // version 5: the QUIET link's, none on fq_codel
				s.OWDok = true
			default: // γ1: both
				s.OWDok = true
				s.OWN, s.OWNok = read, true
			}
			l.sc.Update(s)
			if l.sc.own > 0 {
				l.cap = float64(l.sc.own)
			}
			if step >= 50 {
				segs += served[i] / ownMSS
			}
			if step >= 200 {
				tot += d
			}
		}
	}
	if segs > 0 {
		drops = nDrops / segs
	}
	return tot / 100 / c, drops
}

// fairFill is the max-min fair share of c over demands.
func fairFill(c float64, dem []float64) []float64 {
	out := make([]float64, len(dem))
	rest := make([]int, 0, len(dem))
	for i := range dem {
		rest = append(rest, i)
	}
	left := c
	for len(rest) > 0 {
		lvl := left / float64(len(rest))
		var keep []int
		lim := false
		for _, i := range rest {
			if dem[i] <= lvl {
				out[i] = dem[i]
				left -= dem[i]
				lim = true
			} else {
				keep = append(keep, i)
			}
		}
		if !lim {
			for _, i := range keep {
				out[i] = lvl
			}
			break
		}
		rest = keep
	}
	return out
}

// On fq_codel the own-queue cap keeps CoDel from dropping where a link's
// rate allows (one link alone: version 5 let BBR's queue stand in its flow
// queue, and CoDel dropped 7% of its segments on the rig), and the sum at
// the path with any number of busy links: version 4, its own queue at half
// the target with the plateau's slow growth, fell from 89 to 50 Mbit/s with
// 17 flows on 64 links on the rig. (Links at a few Mbit/s each are past
// CoDel's reach: a segment's DRR round alone is past 5 ms there, for any
// sender.)
func TestAlphaOwnFQ(t *testing.T) {
	type res struct{ g, gd, v4, v4d, v5, v5d float64 }
	cases := []struct {
		mbit   float64
		active int
	}{{100, 1}, {25, 1}, {100, 2}, {100, 17}, {100, 64}, {100, 300}, {25, 17}, {10, 64}}
	var out []res
	for _, c0 := range cases {
		c := c0.mbit * 1e6 / 8
		var r res
		r.g, r.gd = fqModel(c, c0.active, "g1")
		r.v4, r.v4d = fqModel(c, c0.active, "v4")
		r.v5, r.v5d = fqModel(c, c0.active, "v5")
		out = append(out, r)
		t.Logf("%3.0f Mbit/s, %3d busy links: sum γ1 %.3f (CoDel drops %.3f%%), version 4 %.3f (%.3f%%), version 5 %.3f (%.3f%%)",
			c0.mbit, c0.active, r.g, 100*r.gd, r.v4, 100*r.v4d, r.v5, 100*r.v5d)
	}
	for k, c0 := range cases {
		r := out[k]
		if r.g < 0.95 {
			t.Errorf("%.0f Mbit/s, %d links: γ1's sum %.3f of the path, want >= 0.95", c0.mbit, c0.active, r.g)
		}
		// where a link's share holds 2 segments within 5 ms, its drops a
		// fifth of version 5's at most
		if c0.mbit*1e6/8/float64(c0.active)*0.005 >= 2*ownMSS && r.gd > 0.2*r.v5d {
			t.Errorf("%.0f Mbit/s, %d links: CoDel drops %.3f%% of the segments, want a fifth of version 5's %.3f%% at most",
				c0.mbit, c0.active, 100*r.gd, 100*r.v5d)
		}
	}
}

// fifoModel: m busy links on one shared FIFO of c bytes/s: the queue is
// everyone's, the shared (QUIET) reading and each link's own see the same.
// It returns the mean sum over the last 10 s, the worst queue after 10 s,
// and the own caps' cuts.
func fifoModel(c float64, m int, own bool) (sum float64, worst time.Duration, ownCuts uint64) {
	const ms = time.Millisecond
	type link struct {
		sc    *sendCap
		cap   float64
		acked float64
	}
	ls := make([]*link, m)
	for i := range ls {
		ls[i] = &link{sc: &sendCap{linked: true, alpha: true}, cap: c / float64(m)}
	}
	old := AlphaOwn
	defer func() { AlphaOwn = old }()
	if !own {
		AlphaOwn = 0
	}
	var queue float64 // bytes
	at := time.Hour
	q1 := time.Duration(0)
	for step := 0; step < 300; step++ {
		in := 0.0
		for _, l := range ls {
			in += l.cap
		}
		served := math.Min(c, in+queue/0.1) // backlogged: c; else what came
		queue = math.Max(queue+(in-c)*0.1, 0)
		q := time.Duration(queue / c * float64(time.Second))
		read := min(q, q1)
		q1 = q
		at += 100 * ms
		for _, l := range ls {
			d := served * l.cap / in
			l.acked += d / 10
			l.sc.Update(capSample{Now: at, Delivery: d, Acked: uint64(l.acked), Written: uint64(l.acked),
				Segs: uint32(l.acked / 1000), SRTT: 72*ms + q, MinRTT: 72 * ms,
				OWD: read, OWDok: true, OWN: read, OWNok: own})
			if l.sc.own > 0 {
				l.cap = float64(l.sc.own)
			}
			if step >= 200 {
				sum += d
			}
		}
		if step >= 100 {
			worst = max(worst, q)
		}
	}
	for _, l := range ls {
		ownCuts += l.sc.bCuts
	}
	return sum / 100 / c, worst, ownCuts
}

// On a shared FIFO the own queue past the shared one is about none: the
// own cap never cuts, and α is version 5's (the shared cap's half target).
func TestAlphaOwnFIFO(t *testing.T) {
	for _, m := range []int{1, 4, 64} {
		c := 25e6 / 8
		g, gw, cuts := fifoModel(c, m, true)
		v, vw, _ := fifoModel(c, m, false)
		t.Logf("FIFO 25 Mbit/s, %d links: γ1 %.3f (queue up to %v, own cuts %d), version 5 %.3f (up to %v)", m, g, gw, cuts, v, vw)
		if cuts != 0 || math.Abs(g-v) > 0.01 || gw > vw+time.Millisecond {
			t.Fatalf("FIFO, %d links: γ1 %.3f (own cuts %d, queue %v) against version 5 %.3f (%v): want the same", m, g, cuts, gw, v, vw)
		}
	}
}

// The own cap: past its target and rising it cuts from the delivery (at
// most 10%), standing past it by at most 2%, not while the delivery still
// grows (start-up), and under it grows by 3% with no plateau; its target
// is at least ownSegs segments at the delivery rate; the lesser of the two
// caps is in force; none without a reading of its own.
func TestAlphaOwnSteps(t *testing.T) {
	const ms = time.Millisecond
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-6*b+1 }
	c := &sendCap{linked: true, alpha: true}
	at, acked := time.Hour, 0.0
	feed := func(dlv float64, own time.Duration, ok bool) {
		at += 100 * ms
		acked += dlv / 10
		c.Update(capSample{Now: at, Delivery: dlv, Acked: uint64(acked), Written: uint64(acked), Segs: uint32(acked / 1000),
			SRTT: 72 * ms, MinRTT: 72 * ms, OWDok: true, OWN: own, OWNok: ok})
	}
	const r = 50e6 / 8
	feed(r, 0, true)
	feed(r, 0, true) // armed
	if c.bRate <= 0 || !near(c.bRate, wcNetKOf()*r) {
		t.Fatalf("armed: own cap %.0f, want C4-lite's %.0f", c.bRate, wcNetKOf()*r)
	}
	feed(r, 0, true)
	feed(r, 0, true)
	feed(r, 9*ms, true) // past 5 ms and rising: 4% under what was delivered
	if !near(c.bRate, r*0.96) || c.bCuts != 1 {
		t.Fatalf("rising to 9 ms: own cap %.0f (cuts %d), want %.0f", c.bRate, c.bCuts, r*0.96)
	}
	if c.own != uint64(c.bRate) {
		t.Fatalf("the lesser cap in force: %d, want the own %.0f (shared %.0f)", c.own, c.bRate, c.aRate)
	}
	x := c.bRate
	feed(r*0.96, 9*ms, true) // standing at 9 ms: at most 2%
	if !near(c.bRate, x*0.98) {
		t.Fatalf("standing at 9 ms: own cap %.0f, want %.0f", c.bRate, x*0.98)
	}
	x = c.bRate
	feed(x, 0, true) // no queue: +3%
	if !near(c.bRate, x*1.03) {
		t.Fatalf("no queue: own cap %.0f, want %.0f", c.bRate, x*1.03)
	}
	x = c.bRate
	feed(x, 2*ms, true) // 2 ms of the 5: +3% x 0.6
	if !near(c.bRate, x*(1+0.03*0.6)) {
		t.Fatalf("2 ms under the target: own cap %.0f, want %.0f", c.bRate, x*(1+0.03*0.6))
	}
	// the target at a low rate: 2 segments at 1 Mbit/s is 23.2 ms
	lo := &sendCap{linked: true, alpha: true}
	at2, ack2 := time.Hour, 0.0
	feedLo := func(own time.Duration) {
		at2 += 100 * ms
		ack2 += 1e6 / 8 / 10
		lo.Update(capSample{Now: at2, Delivery: 1e6 / 8, Acked: uint64(ack2), Written: uint64(ack2), Segs: uint32(ack2 / 1000),
			SRTT: 72 * ms, MinRTT: 72 * ms, OWDok: true, OWN: own, OWNok: true})
	}
	for i := 0; i < 4; i++ {
		feedLo(0)
	}
	feedLo(20 * ms)
	if lo.bCuts != 0 || lo.bTgt < 23*ms {
		t.Fatalf("1 Mbit/s, 20 ms: own cuts %d, target %v; want none, target ~23.2 ms", lo.bCuts, lo.bTgt)
	}
	// start-up: a queue while the delivery grows by 60% a reading holds
	su := &sendCap{linked: true, alpha: true}
	at3, ack3, d := time.Hour, 0.0, 2e6/8
	for i := 0; i < 6; i++ {
		at3 += 100 * ms
		ack3 += d / 10
		q := time.Duration(0)
		if i >= 2 {
			q = 20 * ms
		}
		su.Update(capSample{Now: at3, Delivery: d, Acked: uint64(ack3), Written: uint64(ack3), Segs: uint32(ack3 / 1000),
			SRTT: 72 * ms, MinRTT: 72 * ms, OWDok: true, OWN: q, OWNok: true})
		d *= 1.6
	}
	if su.bCuts != 0 {
		t.Fatalf("start-up's queue while the delivery grows: %d own cuts, want none", su.bCuts)
	}
	// no reading of its own: no own cap
	feed(r, 0, false)
	if c.bRate != 0 || c.own != uint64(c.aRate) {
		t.Fatalf("no own reading: own cap %.0f, cap %d; want none and the shared %.0f", c.bRate, c.own, c.aRate)
	}
	// the switch off: the shared cap alone, whatever the own reading
	AlphaOwn = 0
	defer func() { AlphaOwn = 1 }()
	feed(r, 30*ms, true)
	if c.bRate != 0 {
		t.Fatalf("HS2_TUNE_ALPHA_OWN=0: own cap %.0f, want none", c.bRate)
	}
}

// The own cap's status: on the link's sendcap line.
func TestAlphaOwnStatusLine(t *testing.T) {
	c := &sendCap{linked: true, alpha: true, bRate: 1e6, bQ: 3 * time.Millisecond, bTgt: 5 * time.Millisecond, bCuts: 2}
	l := &sendCapLink{core: *c}
	if s := l.line(1); !strings.Contains(s, "own 8.0 Mbit/s, queue 3.0 ms past the shared, target 5.0 ms, cuts 2") {
		t.Fatalf("status line %q lacks the own cap", s)
	}
}

// The probe (HS2_TUNE_ALPHA_PROBE, default on): a path ten times faster
// after the shared cap's start ended (10 Mbit/s, then 100) is filled in
// seconds, past the plateau; without it the cap grows by a few % a reading
// (0.5% near the rate it was last cut from).
func TestAlphaProbe(t *testing.T) {
	const ms = time.Millisecond
	grow := func() (x2, x5 float64) {
		c := &sendCap{linked: true, alpha: true}
		at, acked := time.Hour, 0.0
		path, cap := 10e6/8, 10e6/8
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
		for i := 0; i < 5; i++ {
			feed(0)
		}
		feed(20 * ms) // at the 10 Mbit/s path: the start ends with a cut
		path = 100e6 / 8
		from := c.aRate
		for i := 0; i < 20; i++ {
			feed(0)
		}
		x2 = c.aRate / from
		for i := 0; i < 30; i++ {
			feed(0)
		}
		return x2, c.aRate / from
	}
	on2, on5 := grow()
	AlphaProbe = 0
	off2, off5 := grow()
	AlphaProbe = 1
	t.Logf("a path ten times faster: the cap x%.2f in 2 s and x%.2f in 5 s with the probe, x%.2f and x%.2f without", on2, on5, off2, off5)
	if on5 < 8 || off5 > 4 {
		t.Fatalf("with the probe x%.2f in 5 s (want >= 8), without x%.2f (want < 4)", on5, off5)
	}
}

// A BULK link's own loop sends nothing while the link is idle (γ1): none
// noted, or under owdIdleRate.
func TestOWDIdle(t *testing.T) {
	var o owdLink
	if !o.idle() {
		t.Fatal("no rate noted: want idle")
	}
	o.noteRate(10e6 / 8)
	if o.idle() {
		t.Fatal("10 Mbit/s: want busy")
	}
	var p owdLink
	p.noteRate(0.5e6 / 8)
	if !p.idle() {
		t.Fatal("0.5 Mbit/s: want idle")
	}
}

// FuzzOWDDecode: the 16-byte kindOWD message is the peer's data; its
// decoder takes any bytes without panicking, refuses a short one or an
// unknown version, and what it accepts encodes back to the same 16 bytes
// (the 3 reserved bytes aside) when it carries a queue.
func FuzzOWDDecode(f *testing.F) {
	var b [owdMsgLen]byte
	owdEncode(b[:], 987654321, int64(4321*time.Microsecond), true)
	f.Add(b[:])
	owdEncode(b[:], -5, 0, false)
	f.Add(b[:])
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0})
	f.Add(bytes.Repeat([]byte{0xff}, 40))
	f.Fuzz(func(t *testing.T, in []byte) {
		stamp, q, have, ok := owdDecode(in)
		if len(in) < owdMsgLen || (len(in) > 0 && in[0] != 1) {
			if ok {
				t.Fatalf("accepted %x", in)
			}
			return
		}
		if !ok {
			t.Fatalf("refused a version 1 message %x", in)
		}
		if q < 0 {
			t.Fatalf("negative queue %v from %x", q, in)
		}
		var out [owdMsgLen]byte
		owdEncode(out[:], stamp, int64(q), have)
		if !bytes.Equal(out[4:], in[4:owdMsgLen]) {
			t.Fatalf("re-encoded %x, from %x", out, in[:owdMsgLen])
		}
		// a queue read from the peer feeds α: the receiver keeps it
		var o owdLink
		if !o.recv(ctrlNow(), in) {
			t.Fatalf("recv refused %x", in)
		}
		if got, ok := o.queue(); have && (!ok || got != q) {
			t.Fatalf("queue %v %v, want %v", got, ok, q)
		}
	})
}

// owdRestamp: the stamp as the message leaves the session, moved on by the
// socket's unsent bytes at its pacing rate (20 KiB at 9 Mbit/s: 18.2 ms).
func TestOWDRestamp(t *testing.T) {
	var b [owdMsgLen]byte
	owdEncode(b[:], 5, int64(3*time.Millisecond), true)
	before := ctrlNow()
	owdRestamp(b[:], &fakeCapSys{st: tcpStat{notsent: 20 << 10, pacingRate: 9e6 / 8}, ok: true})
	st, q, have, ok := owdDecode(b[:])
	if !ok || !have || q != 3*time.Millisecond {
		t.Fatalf("restamped message: ok %v have %v queue %v", ok, have, q)
	}
	if w := time.Duration(st - before); w < 18*time.Millisecond || w > 50*time.Millisecond {
		t.Fatalf("stamp %v past the call, want ~18.2 ms (the unsent bytes' wait)", w)
	}
	owdRestamp(b[:3], nil) // short: untouched, no panic
}

// A reading that does not fall after the own cap's cuts (here it grows as
// the cap falls, as a host's wait at a lower cap would) is none of the
// link's doing: after ownNoHelpN cuts the cap goes back to where they
// began and that queue is the base of its readings; a queue that drains
// after each cut is its own (no such reset); HS2_TUNE_ALPHA_OWN_CHECK=0:
// it cuts on.
func TestAlphaOwnCheck(t *testing.T) {
	const ms = time.Millisecond
	run := func(phantom bool) *sendCap {
		c := &sendCap{linked: true, alpha: true}
		at, acked := time.Hour, 0.0
		capNow := 8e6 / 8
		for i := 0; i < 40; i++ {
			at += 100 * ms
			dlv := capNow
			acked += dlv / 10
			q := time.Duration(0)
			if i >= 5 {
				if phantom {
					q = time.Duration(10e6 / 8 / capNow * float64(8*ms)) // 10 ms at 8 Mbit/s, longer as the cap falls
				} else if i == 5 {
					q = 8 * ms // its own: drains once cut
				} else if i == 6 {
					q = 6 * ms
				}
			}
			c.Update(capSample{Now: at, Delivery: dlv, Acked: uint64(acked), Written: uint64(acked), Segs: uint32(acked / 1000),
				SRTT: 72 * ms, MinRTT: 72 * ms, OWDok: true, OWN: q, OWNok: true})
			if c.own > 0 {
				capNow = math.Min(float64(c.own), 8e6/8)
			}
		}
		return c
	}
	p := run(true)
	t.Logf("a queue that grows as the cap falls: own cap %.1f Mbit/s, cuts %d, not its own %d (base %v)", p.bRate*8/1e6, p.bCuts, p.bPhantoms, p.bOff)
	if p.bPhantoms == 0 || p.bRate < 7.5e6/8 {
		t.Fatalf("phantom queue: %d resets, own cap %.1f Mbit/s; want one at least and the cap back near 8", p.bPhantoms, p.bRate*8/1e6)
	}
	o := run(false)
	if o.bPhantoms != 0 || o.bCuts == 0 {
		t.Fatalf("its own queue: %d resets, %d cuts; want cuts and no reset", o.bPhantoms, o.bCuts)
	}
	AlphaOwnCheck = 0
	defer func() { AlphaOwnCheck = 1 }()
	n := run(true)
	t.Logf("check off: own cap %.1f Mbit/s after %d cuts", n.bRate*8/1e6, n.bCuts)
	if n.bPhantoms != 0 || n.bRate > 7e6/8 {
		t.Fatalf("check off: %d resets, own cap %.1f Mbit/s; want the ratchet down", n.bPhantoms, n.bRate*8/1e6)
	}
}
