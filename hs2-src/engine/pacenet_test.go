package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// wnFeed is a socket the net reads every 100 ms: it sends segs segments a
// second of which share are resends, delivers dlv bytes/s and paces at
// pace bytes/s, with q of queue over its 100 ms min_rtt.
type wnFeed struct {
	n                *wcNet
	now              time.Time
	rtx, segs, acked float64
	q                time.Duration
}

func newWnFeed() *wnFeed { return &wnFeed{n: &wcNet{}, now: time.Unix(1e9, 0)} }

// run reads for d at those rates and returns every decision that was not
// "nothing".
func (f *wnFeed) run(d time.Duration, segs, share, dlv, pace float64) []wcNetOut {
	var outs []wcNetOut
	for t := time.Duration(0); t < d; t += wcRampTick {
		f.now = f.now.Add(wcRampTick)
		f.segs += segs / 10
		f.rtx += share * segs / 10
		f.acked += dlv / 10
		o := f.n.step(wcNetIn{now: f.now, retrans: uint64(f.rtx), segsOut: uint64(f.segs), acked: uint64(f.acked), dlv: dlv, pace: pace,
			srtt: 100*time.Millisecond + f.q, minRTT: 100 * time.Millisecond})
		if o.act != wcNetNone {
			outs = append(outs, o)
		}
	}
	return outs
}

const wnMB = 1e6 / 8 // bytes/s of a Mbit/s

// The net engages on a socket that resends past wcNetLoss while it paces
// far past what it delivers, at wcNetK times the delivery rate; follows the
// delivery rate down only while the resends go on with a queue at the
// socket (the path slowed: not the cap), and up whenever; is lifted after
// wcNetClean clean, and held twice as long when the trouble comes back soon
// after a lift.
func TestPaceNetEngageRelease(t *testing.T) {
	f := newWnFeed()
	if o := f.run(3*time.Second, 20000, 0, 240*wnMB, 940*wnMB); len(o) != 0 {
		t.Fatalf("a clean socket at 3.9× its delivery rate: %+v", o)
	}
	o := f.run(time.Second, 20000, 0.1, 240*wnMB, 940*wnMB)
	if len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("10%% resent at 3.9× the delivery rate: %+v, want the cap", o)
	}
	if want := uint64(wcNetK * 240 * wnMB); o[0].cap != want || math.Abs(o[0].ratio-940.0/240) > 0.01 || o[0].share < wcNetLoss {
		t.Fatalf("engaged at %+v, want a cap of %d (%.4g× the delivery rate)", o[0], want, wcNetK)
	}
	// resending less (the cap pays) with no queue at the socket, delivering
	// less (held by its cap): the cap stays
	f.run(wcNetSpan+time.Second, 10000, 0.03, 150*wnMB, 940*wnMB)
	if want := uint64(wcNetK * 240 * wnMB); f.n.cap != want {
		t.Fatalf("resending with no queue, delivering 150 Mbit/s: cap %d, want it held at %d", f.n.cap, want)
	}
	// the path slows down, the resends go on with a queue: the cap follows
	// (the largest delivery rate of wcNetSpan)
	f.q = 30 * time.Millisecond
	o = f.run(wcNetSpan+time.Second, 10000, 0.03, 100*wnMB, 940*wnMB)
	if want := uint64(wcNetK * 100 * wnMB); f.n.cap != want || len(o) == 0 || o[len(o)-1].act != wcNetMoveCap {
		t.Fatalf("slowed to 100 Mbit/s, still resending: cap %.0f Mbit/s (%+v), want %.0f", float64(f.n.cap)/wnMB, o, float64(want)/wnMB)
	}
	// clean, delivering less (held by its cap and its windows): the cap stays
	f.q = 0
	held := f.n.cap
	if o = f.run(3*time.Second, 5000, 0, 50*wnMB, float64(held)); len(o) != 0 || f.n.cap != held {
		t.Fatalf("clean at 50 Mbit/s: %+v cap %d, want it held at %d", o, f.n.cap, held)
	}
	// ... and delivering more: it rises
	f.run(time.Second, 15000, 0, 160*wnMB, float64(held))
	if want := uint64(wcNetK * 160 * wnMB); f.n.cap != want {
		t.Fatalf("clean at 160 Mbit/s: cap %d, want %d", f.n.cap, want)
	}
	// clean for wcNetClean since the last resend: lifted
	o = f.run(wcNetClean, 15000, 0, 160*wnMB, float64(f.n.cap))
	if len(o) == 0 || o[len(o)-1].act != wcNetRelease || f.n.cap != 0 || o[len(o)-1].idle {
		t.Fatalf("clean for %v: %+v cap %d, want the cap lifted", wcNetClean, o, f.n.cap)
	}
	// the trouble comes back within wcNetRecur: held twice as long
	f.run(5*time.Second, 15000, 0, 160*wnMB, 620*wnMB)
	if o = f.run(time.Second, 15000, 0.2, 160*wnMB, 620*wnMB); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("20%% resent again 5 s after the lift: %+v", o)
	}
	f.run(wcNetClean+time.Second, 15000, 0, 160*wnMB, 240*wnMB)
	if f.n.cap == 0 {
		t.Fatalf("back within %v of a lift: lifted after %v clean, want twice that", wcNetRecur, wcNetClean)
	}
	f.run(wcNetClean, 15000, 0, 160*wnMB, 240*wnMB)
	if f.n.cap != 0 {
		t.Fatalf("held past twice %v clean", wcNetClean)
	}
	if f.n.n != 2 || f.n.maxShare < 0.19 {
		t.Fatalf("counted %d engagements, largest share %.3f", f.n.n, f.n.maxShare)
	}
}

// A clean socket is never touched, whatever it paces at; nor one that
// resends with its pacing near what it delivers (BBR's ProBE_BW, CUBIC: no
// burst to tame), nor one under the random loss of its path, nor one that
// sends too little to judge.
func TestPaceNetNeverOnClean(t *testing.T) {
	for _, tc := range []struct {
		name             string
		segs, share, dlv float64
		pace             float64
	}{
		{"clean at 3.9× its delivery rate", 20000, 0, 240 * wnMB, 940 * wnMB},
		{"clean at 10×", 20000, 0, 24 * wnMB, 240 * wnMB},
		{"5% resent, pacing 1.25× (PROBE_BW)", 20000, 0.05, 240 * wnMB, 300 * wnMB},
		{"5% resent, pacing 2×", 20000, 0.05, 240 * wnMB, 480 * wnMB},
		{"0.3% resent (random loss) at 3.9×", 20000, 0.003, 240 * wnMB, 940 * wnMB},
		{"just under wcNetLoss resent at 3.9×", 20000, 0.95 * wcNetLoss, 240 * wnMB, 940 * wnMB},
		{"50 segments a second, 10% resent", 50, 0.1, 0.5 * wnMB, 5 * wnMB},
		{"no delivery rate", 20000, 0.1, 0, 940 * wnMB},
		{"no pacing rate (unknown)", 20000, 0.1, 240 * wnMB, 0},
	} {
		f := newWnFeed()
		if o := f.run(time.Minute, tc.segs, tc.share, tc.dlv, tc.pace); len(o) != 0 || f.n.cap != 0 || f.n.n != 0 {
			t.Errorf("%s: %+v", tc.name, o)
		}
	}
}

// The cap is never under wcPaceFloor, and an idle socket's is lifted at
// once (the next transfer starts free); a socket that just began sending
// (its first STARTUP: the delivery rate doubles each round trip) engages
// only past wcNetWarmLoss for wcNetWarm, and so does one that sends again
// after idling.
func TestPaceNetFloorAndIdle(t *testing.T) {
	f := newWnFeed()
	if o := f.run(wcNetWarm-wcRampTick, 20000, 0.9*wcNetWarmLoss, 240*wnMB, 940*wnMB); len(o) != 0 {
		t.Fatalf("%.0f%% resent at 3.9× within %v of the socket's first data: %+v", 90*wcNetWarmLoss, wcNetWarm, o)
	}
	if o := f.run(time.Second, 20000, 0.9*wcNetWarmLoss, 240*wnMB, 940*wnMB); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("the same past the warm-up: %+v", o)
	}
	f = newWnFeed()
	if o := f.run(2*time.Second, 20000, 2*wcNetWarmLoss, 240*wnMB, 940*wnMB); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("%.0f%% resent within the warm-up (a policer the first STARTUP runs into): %+v", 200*wcNetWarmLoss, o)
	}
	f = newWnFeed()
	f.run(wcNetWarm, 400, 0, 100e3, 1e6)
	o := f.run(2*time.Second, 400, 0.1, 100e3, 1e6)
	if len(o) == 0 || o[0].act != wcNetEngage || o[0].cap != wcPaceFloor {
		t.Fatalf("a slow socket (100 kB/s) resending 10%% at 10×: %+v, want the cap at the floor %d", o, wcPaceFloor)
	}
	f = newWnFeed()
	f.run(wcNetWarm, 20000, 0, 240*wnMB, 940*wnMB)
	f.run(time.Second, 20000, 0.1, 240*wnMB, 940*wnMB)
	if f.n.cap == 0 {
		t.Fatal("not engaged")
	}
	o = f.run(2*time.Second, 0, 0, 0, 0)
	if len(o) != 1 || o[0].act != wcNetRelease || !o[0].idle || f.n.cap != 0 {
		t.Fatalf("idle: %+v, want the cap lifted for idleness", o)
	}
	if o = f.run(10*time.Second, 0, 0, 0, 0); len(o) != 0 {
		t.Fatalf("idle and lifted: %+v", o)
	}
	if o = f.run(wcNetWarm-wcRampTick, 20000, 0.9*wcNetWarmLoss, 240*wnMB, 940*wnMB); len(o) != 0 {
		t.Fatalf("sending again after idling, %.0f%% resent within %v: %+v", 90*wcNetWarmLoss, wcNetWarm, o)
	}
}

// wnLogLink is an exit's link for the log's tests.
func wnLogLink(name string) *wcLink {
	l := &wcLink{side: wcExit, pnet: true}
	l.setName(name)
	return l
}

// The net's line: once per link within wcLogNetEvery, past wcAggLinks
// links a minute one summary for the rest (a path all the links share
// turning bad writes a few lines, not one per link).
func TestPaceNetLogLine(t *testing.T) {
	ws := &wcSet{}
	var logs wcLogs
	logs.set(ws)
	now := time.Unix(1e9, 0)
	o := wcNetOut{act: wcNetEngage, cap: uint64(1.5 * 160 * wnMB), share: 0.061, ratio: 3.9, k: 1.5, dlv: 160 * wnMB}
	l := wnLogLink("link from 1.2.3.4:5678")
	ws.netLine(l, now, o)
	const want = "mtcp: link from 1.2.3.4:5678: resending 6.1% at a pacing rate 3.9× what the path delivers — pacing held at 1.5× (~240 Mbit/s) until the resends stop"
	if got := logs.all(); len(got) != 1 || got[0] != want {
		t.Fatalf("line %q, want %q", got, want)
	}
	ws.netLine(l, now.Add(wcLogNetEvery/2), o)
	if n := logs.count("resending"); n != 1 {
		t.Fatalf("the same link again within %v: %d lines", wcLogNetEvery, n)
	}
	ws.netLine(l, now.Add(wcLogNetEvery), o)
	if n := logs.count("resending"); n != 2 {
		t.Fatalf("the same link after %v: %d lines", wcLogNetEvery, n)
	}
	// twelve links within a minute (a fresh minute): eight lines, then one
	// line for the other four
	ws = &wcSet{}
	logs = wcLogs{}
	logs.set(ws)
	for i := 0; i < 12; i++ {
		ws.netLine(wnLogLink(fmt.Sprintf("link from 10.0.0.%d:443", i)), now.Add(time.Duration(i)*time.Second), o)
	}
	if n := logs.count("resending"); n != wcAggLinks {
		t.Fatalf("12 links in a minute: %d lines, want %d", n, wcAggLinks)
	}
	ws.flush(now.Add(wcAggEvery + time.Second))
	if n := logs.count(fmt.Sprintf("mtcp: pacing net: 4 more link(s) resent past %.4g%% and had their pacing held in the last minute", 100*wcNetLoss)); n != 1 {
		t.Fatalf("no summary: %q", logs.all())
	}
}

// The exit's link-end note: what the net did, after the window core's (if
// it ran), and cut back off the reason as before.
func TestPaceNetEndNote(t *testing.T) {
	mtr := &linkMeter{wc: wnLogLink("link from 1.2.3.4:5678")}
	if n := wcoreEndNote(mtr); n != "" {
		t.Fatalf("a net that never engaged: %q", n)
	}
	st := &mtr.wc.inStats
	st.netHeld.Store(2)
	st.netShare.Store(math.Float64bits(0.061))
	if n := wcoreEndNote(mtr); n != " (while up: pacing held 2 time(s) for resends (up to 6.1%))" {
		t.Fatalf("note %q", n)
	}
	st.active.Store(true)
	st.peakB.Store(4 << 20)
	n := wcoreEndNote(mtr)
	if !strings.HasPrefix(n, " (while up: window core: up to 4.0 MiB") || !strings.HasSuffix(n, "; pacing held 2 time(s) for resends (up to 6.1%))") {
		t.Fatalf("with the core's note %q", n)
	}
	why, note := cutEndNote("read: " + reasonPeerClosed + n)
	if why != "read: "+reasonPeerClosed || note != n {
		t.Fatalf("cut %q / %q", why, note)
	}
}

// A cap must pay: one that held for wcNetJudge while the socket resent as
// much as before (random loss of the path) is lifted, and that share is the
// path's level: the net engages again only past wcNetLvlRel times it (for
// wcNetLvlTTL), not at every reading of the same random loss.
func TestPaceNetMustPay(t *testing.T) {
	f := newWnFeed()
	f.run(wcNetWarm, 20000, 0, 100*wnMB, 390*wnMB)
	o := f.run(time.Second, 20000, 0.03, 100*wnMB, 390*wnMB)
	if len(o) != 1 || o[0].act != wcNetEngage {
		t.Fatalf("3%% resent at 3.9×: %+v", o)
	}
	o = f.run(wcNetJudge+time.Second, 20000, 0.03, 100*wnMB, 150*wnMB)
	if len(o) != 1 || o[0].act != wcNetRelease || o[0].idle || f.n.cap != 0 {
		t.Fatalf("3%% resent for %v under the cap: %+v, want it lifted", wcNetJudge, o)
	}
	if o = f.run(time.Minute, 20000, 0.03, 100*wnMB, 390*wnMB); len(o) != 0 {
		t.Fatalf("the same random loss for a minute after: %+v", o)
	}
	if o = f.run(time.Second, 20000, 0.12, 100*wnMB, 390*wnMB); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("12%% resent past the path's 3%%: %+v, want the cap", o)
	}
	// a cap that pays is kept
	f = newWnFeed()
	f.run(wcNetWarm, 20000, 0, 100*wnMB, 390*wnMB)
	f.run(time.Second, 20000, 0.2, 100*wnMB, 390*wnMB)
	if o = f.run(2*wcNetJudge, 20000, 0.01, 100*wnMB, 150*wnMB); len(o) != 0 || f.n.cap == 0 {
		t.Fatalf("20%% resent, then 1%% under the cap: %+v cap %d, want it kept", o, f.n.cap)
	}
}

// A cap that does not bind (the socket paces under it: its own sender's
// estimate came down below it) cannot be what holds the delivery rate, so
// it follows that rate down while the resends go on, queue or not: a
// policer that comes mid-transfer builds no queue, and the cap set from the
// rate before it (z17 rig, a 180 Mbit/s policer at 15 s on 300/150 under
// the core: held at ~370 Mbit/s while the sender paced at 290 over 45
// delivered, lifted at the judge, 4–5% resent). It is judged from then on.
func TestPaceNetUnboundCapFollows(t *testing.T) {
	f := newWnFeed()
	f.run(wcNetWarm, 30000, 0, 300*wnMB, 1170*wnMB)
	o := f.run(time.Second, 30000, 0.1, 300*wnMB, 1170*wnMB)
	if len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("not engaged: %+v", o)
	}
	// the policer: 45 Mbit/s delivered, the sender paces at 290 (under the
	// cap of 375), 8% resent, no queue
	f.run(wcNetSpan+time.Second, 4000, 0.08, 45*wnMB, 290*wnMB)
	if want := uint64(wcNetK * 45 * wnMB); math.Abs(float64(f.n.cap)-float64(want)) > 0.06*float64(want) {
		t.Fatalf("paced at 290 under a cap it does not reach, 45 delivered, 8%% resent: cap %.0f Mbit/s, want ~%.0f",
			float64(f.n.cap)/wnMB, float64(want)/wnMB)
	}
	// now it binds and pays: the judge keeps it
	held := f.n.cap
	if o = f.run(2*wcNetJudge, 4000, 0.005, 45*wnMB, float64(held)); f.n.cap != held {
		t.Fatalf("binding and paying: %+v cap %d, want it kept at %d", o, f.n.cap, held)
	}
	// a cap that binds (the socket paces at it) does not follow the rate
	// down with no queue (the ratchet): TestPaceNetEngageRelease
}

// A socket whose delivery rate still grows past wcNetGrow a second (its
// STARTUP, longer than the warm-up on a long round trip) is capped only
// past wcNetWarmLoss: a cap at k times a rate that doubles each round trip
// ends that STARTUP at a fraction of the path; once the rate stops growing
// it is.
func TestPaceNetNotWhileGrowing(t *testing.T) {
	f := newWnFeed()
	f.run(wcNetWarm, 20000, 0, 10*wnMB, 39*wnMB)
	dlv := 10 * wnMB
	for i := 0; i < 30; i++ {
		dlv *= 1.07 // ×2 a second
		if o := f.run(wcRampTick, 20000, 0.05, dlv, 3.9*dlv); len(o) != 0 {
			t.Fatalf("5%% resent while the delivery rate doubles each second (now %.0f Mbit/s): %+v", dlv/wnMB, o)
		}
	}
	if o := f.run(2*time.Second, 20000, 0.05, dlv, 3.9*dlv); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("5%% resent once the rate stopped growing: %+v", o)
	}
}

// ... and one that loses past wcNetWarmLoss as its rate grows (its STARTUP
// runs into a policer) is capped at once.
func TestPaceNetGrowingPastWarmLoss(t *testing.T) {
	f := newWnFeed()
	f.run(wcNetWarm, 20000, 0, 10*wnMB, 39*wnMB)
	dlv := 10 * wnMB
	var o []wcNetOut
	for i := 0; i < 10 && len(o) == 0; i++ {
		dlv *= 1.07
		o = f.run(wcRampTick, 20000, 2*wcNetWarmLoss, dlv, 3.9*dlv)
	}
	if len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("%.0f%% resent as the rate grows: %+v", 200*wcNetWarmLoss, o)
	}
}

// Linux's delivery rate is the latest sample that was not app-limited: a
// sender held by its windows can read the old, faster path's for seconds
// after it slowed down. The net takes what was acknowledged meanwhile when
// that is less, and the cap follows the path down.
func TestPaceNetStaleDeliveryRate(t *testing.T) {
	f := newWnFeed()
	f.run(wcNetWarm, 20000, 0, 240*wnMB, 940*wnMB)
	if o := f.run(time.Second, 20000, 0.1, 240*wnMB, 940*wnMB); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("not engaged: %+v", o)
	}
	// the path slows to 80 Mbit/s: 80 acknowledged a second, tcpi still
	// reads 240, the queue stands and the resends go on (fewer: the cap
	// pays)
	f.q = 30 * time.Millisecond
	for end := f.now.Add(wcNetSpan + time.Second); f.now.Before(end); {
		f.now = f.now.Add(wcRampTick)
		f.segs += 1000
		f.rtx += 40
		f.acked += 80 * wnMB / 10
		f.n.step(wcNetIn{now: f.now, retrans: uint64(f.rtx), segsOut: uint64(f.segs), acked: uint64(f.acked), dlv: 240 * wnMB, pace: 300 * wnMB,
			srtt: 130 * time.Millisecond, minRTT: 100 * time.Millisecond})
	}
	if want := uint64(wcNetK * 80 * wnMB); math.Abs(float64(f.n.cap)-float64(want)) > 0.06*float64(want) {
		t.Fatalf("tcpi's stale 240 Mbit/s, 80 acknowledged: cap %.0f Mbit/s, want ~%.0f", float64(f.n.cap)/wnMB, float64(want)/wnMB)
	}
}

// On a 2 s path a flow's ACKs can pause for more than a second while it
// still sends: idle is judged over max(wcNetIdle, 2·srtt), and its cap
// stays.
func TestPaceNetIdleLongRTT(t *testing.T) {
	n := &wcNet{}
	now := time.Unix(1e9, 0)
	var rtx, segs, acked float64
	read := func(d time.Duration, segsPS, share, ackPS float64) (outs []wcNetOut) {
		for t := time.Duration(0); t < d; t += wcRampTick {
			now = now.Add(wcRampTick)
			segs += segsPS / 10
			rtx += share * segsPS / 10
			acked += ackPS / 10
			if o := n.step(wcNetIn{now: now, retrans: uint64(rtx), segsOut: uint64(segs), acked: uint64(acked), dlv: 4 * wnMB, pace: 16 * wnMB,
				srtt: 2 * time.Second, minRTT: 2 * time.Second}); o.act != wcNetNone {
				outs = append(outs, o)
			}
		}
		return
	}
	read(wcNetWarmMax, 3000, 0, 4*wnMB)
	if o := read(time.Second, 3000, 0.1, 4*wnMB); len(o) == 0 || o[0].act != wcNetEngage {
		t.Fatalf("not engaged: %+v", o)
	}
	read(time.Second, 3000, 0.01, 4*wnMB)
	if o := read(1500*time.Millisecond, 3000, 0, 0); len(o) != 0 || n.cap == 0 {
		t.Fatalf("1.5 s with no ACK on a 2 s path: %+v, want the cap kept", o)
	}
	if o := read(3*time.Second, 0, 0, 0); len(o) != 1 || !o[0].idle {
		t.Fatalf("4.5 s with no ACK on a 2 s path: %+v, want it lifted for idleness", o)
	}
}
