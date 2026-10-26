package engine

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// C4-lite's tests (REDESIGN §3.4): the pure cap, its runner over a
// synthetic socket, and the Linux BBR sender of the link model (Z5,
// wcore_model_test.go) both ways.

// capFeed reads a socket every 100 ms for n readings from *now on: busy at
// dlv (that much acknowledged and written, and its segments), its delivery
// rate dlv and flagged appLim. It returns the last decision, and whether
// any reading applied or lifted the cap.
type capSock struct {
	now    time.Duration
	acked  uint64
	wrote  uint64
	segs   uint32
	srtt   time.Duration
	anyApp bool
	anyLft bool
}

func (k *capSock) feed(c *sendCap, n int, dlv float64, appLim bool) (rate uint64, apply, lift bool) {
	for range n {
		k.now += 100 * time.Millisecond
		b := uint64(dlv / 10)
		k.acked += b
		k.wrote += b
		k.segs += uint32(b/1448) + 1
		rate, apply, lift = c.Update(capSample{Now: k.now, Delivery: dlv, AppLimited: appLim, Acked: k.acked, Written: k.wrote, Segs: k.segs, SRTT: k.srtt})
		k.anyApp = k.anyApp || apply
		k.anyLft = k.anyLft || lift
	}
	return rate, apply, lift
}

// trickle reads it n times more with only b bytes and segs segments a
// reading.
func (k *capSock) trickle(c *sendCap, n int, b uint64, segs uint32) (rate uint64, apply, lift bool) {
	for range n {
		k.now += 100 * time.Millisecond
		k.acked += b
		k.wrote += b
		k.segs += segs
		rate, apply, lift = c.Update(capSample{Now: k.now, Delivery: 1e6, Acked: k.acked, Written: k.wrote, Segs: k.segs, SRTT: k.srtt})
		k.anyApp = k.anyApp || apply
		k.anyLft = k.anyLft || lift
	}
	return rate, apply, lift
}

// TestSendCapFloor: 1.25 times a delivery rate of 100 KiB/s is under the
// floor: the cap is 1 MiB/s.
func TestSendCapFloor(t *testing.T) {
	var c sendCap
	var k capSock
	rate, _, _ := k.feed(&c, 5, 100<<10, false)
	if rate != 1<<20 || !k.anyApp {
		t.Fatalf("rate %d applied %v, want %d", rate, k.anyApp, 1<<20)
	}
	// and past it, 1.25 times the largest of the last 2 s
	k.feed(&c, 5, 40e6, false)
	rate, _, _ = k.feed(&c, 5, 20e6, false)
	if rate != 50e6 {
		t.Fatalf("after 40 then 20 MB/s within 2 s: %d, want 1.25 × 40 MB/s", rate)
	}
	rate, _, _ = k.feed(&c, 21, 20e6, false)
	if rate != 25e6 {
		t.Fatalf("2 s on at 20 MB/s: %d, want 1.25 × 20 MB/s", rate)
	}
}

// TestSendCapAfterNonAppLimited: no cap while every delivery-rate sample is
// app-limited; the first that is not applies it (over the 2 s of samples,
// those included: an app-limited one that Linux kept is past the ones
// before it, a rate the path did carry); with C4-lite off, never.
func TestSendCapAfterNonAppLimited(t *testing.T) {
	var c sendCap
	var k capSock
	if _, _, _ = k.feed(&c, 10, 50e6, true); k.anyApp {
		t.Fatal("applied on app-limited samples")
	}
	rate, apply, _ := k.feed(&c, 1, 40e6, false)
	if !apply || rate != 62.5e6 {
		t.Fatalf("first sample not app-limited: rate %d apply %v, want 1.25 × 50 MB/s", rate, apply)
	}
	// it stays on while app-limited samples come (until idle)
	if rate, _, lift := k.feed(&c, 10, 30e6, true); rate == 0 || lift {
		t.Fatalf("lifted on app-limited samples: rate %d lift %v", rate, lift)
	}
	// a stale sample (nothing acknowledged since) does not apply it
	var c2 sendCap
	c2.Update(capSample{Now: 100 * time.Millisecond, Delivery: 1e6, AppLimited: true, Acked: 1 << 20, Written: 1 << 20, Segs: 800})
	if _, apply, _ := c2.Update(capSample{Now: 200 * time.Millisecond, Delivery: 1e6, Acked: 1 << 20, Written: 2 << 20, Segs: 1600}); apply {
		t.Fatal("applied on a sample with nothing acknowledged since the reading before")
	}
	defer func(v int) { TuneSendCap = v }(TuneSendCap)
	TuneSendCap = 0
	if !SendCapOff() {
		t.Fatal("TuneSendCap 0 and SendCapOff false")
	}
	var c3 sendCap
	var k3 capSock
	if k3.feed(&c3, 10, 50e6, false); k3.anyApp {
		t.Fatal("applied with C4-lite off")
	}
}

// TestSendCapLiftOnIdle: less than 8 KiB acknowledged and written and less
// than 8 segments over the idle span (1 s, or 2·srtt up to 5 s) lifts the
// cap, once; 8 segments or 8 KiB do not; a quiet socket is read once a
// second, or at once when written to.
func TestSendCapLiftOnIdle(t *testing.T) {
	for _, c := range []struct {
		name  string
		b     uint64
		segs  uint32
		srtt  time.Duration
		lift  bool
		after int // readings to the lift
	}{
		{"idle", 700, 0, 0, true, 10},
		{"a segment every other reading", 700, 1, 0, true, 10},
		{"8 segments a second", 100, 1, 0, false, 0},
		{"8 KiB a second", 900, 0, 0, false, 0},
		{"srtt 1.5 s: 3 s span", 0, 0, 1500 * time.Millisecond, true, 30},
	} {
		var sc sendCap
		k := capSock{srtt: c.srtt}
		k.feed(&sc, 10, 20e6, false)
		lifts, at := 0, 0
		for i := 1; i <= 40; i++ {
			segs := c.segs
			if c.name == "a segment every other reading" && i%2 == 0 {
				segs = 0
			}
			if _, _, lift := k.trickle(&sc, 1, c.b, segs); lift {
				lifts++
				at = i
			}
		}
		switch {
		case c.lift && (lifts != 1 || at != c.after):
			t.Fatalf("%s: %d lifts, at reading %d (want 1 at %d)", c.name, lifts, at, c.after)
		case !c.lift && lifts != 0:
			t.Fatalf("%s: lifted", c.name)
		}
	}
	// a quiet socket is due once a second, or once 8 KiB were written
	var sc sendCap
	var k capSock
	k.feed(&sc, 10, 20e6, false)
	k.trickle(&sc, 12, 0, 0)
	if sc.due(k.now+500*time.Millisecond, k.wrote+100) {
		t.Fatal("a quiet socket due after 0.5 s")
	}
	if !sc.due(k.now+500*time.Millisecond, k.wrote+8<<10) || !sc.due(k.now+time.Second, k.wrote) {
		t.Fatal("a quiet socket not due once written to, or after 1 s")
	}
}

// TestSendCapHostRate: C3's rate from C4-lite's readings (decision T2) is
// the least of the largest delivery rate of 2 s and the latest sample that
// was not app-limited: a drop shows at once (where the pacing cap keeps the
// largest of 2 s), an app-limited sample past the rest raises neither; none
// before a sample that is not app-limited.
func TestSendCapHostRate(t *testing.T) {
	var c sendCap
	var k capSock
	k.feed(&c, 5, 50e6, true)
	if _, ok := c.hostRate(); ok {
		t.Fatal("a rate for C3 before any sample not app-limited")
	}
	k.feed(&c, 5, 50e6, false)
	if r, ok := c.hostRate(); !ok || r != 50e6 {
		t.Fatalf("busy at 50 MB/s: %v %v", r, ok)
	}
	k.feed(&c, 3, 80e6, true) // a higher app-limited sample Linux kept
	if r, _ := c.hostRate(); r != 50e6 {
		t.Fatalf("after app-limited samples at 80 MB/s: %v, want the latest not app-limited, 50 MB/s", r)
	}
	rate, _, _ := k.feed(&c, 1, 5e6, false) // the path dropped
	if r, _ := c.hostRate(); r != 5e6 || rate != 100e6 {
		t.Fatalf("a drop to 5 MB/s: C3's rate %v (want 5e6), the cap %d (want 1.25 × 80 MB/s, the largest of 2 s)", r, rate)
	}
}

// fakeCapSys is a link socket for C4-lite's runner: its TCP_INFO, and the
// caps set on it.
type fakeCapSys struct {
	st   tcpStat
	ok   bool
	sets []uint64
}

func (f *fakeCapSys) info() (tcpStat, bool) { return f.st, f.ok }

func (f *fakeCapSys) setPacing(rate uint64) bool {
	f.sets = append(f.sets, rate)
	return true
}

// TestSendCapMinWithPacenet: the pacing net hands its cap to C4-lite, and
// the lesser of the two is set; the net's alone while C4-lite has none; and
// the net's hand-off (wcLink.netSet) goes to the link's C4-lite when it has
// one, to the socket as before when not.
func TestSendCapMinWithPacenet(t *testing.T) {
	var c sendCap
	var k capSock
	if rate, _, _ := k.feed(&c, 5, 80e6, false); rate != 100e6 {
		t.Fatalf("own cap %d", rate)
	}
	c.SetPacenetCap(50e6)
	if rate, apply, _ := k.feed(&c, 1, 80e6, false); rate != 50e6 || !apply {
		t.Fatalf("net's 50 MB/s under the own 100: rate %d apply %v", rate, apply)
	}
	c.SetPacenetCap(200e6)
	if rate, apply, _ := k.feed(&c, 1, 80e6, false); rate != 100e6 || !apply {
		t.Fatalf("net's 200 MB/s over the own 100: rate %d apply %v", rate, apply)
	}
	c.SetPacenetCap(0)
	// the net's alone: C4-lite not applied yet (app-limited)
	var c2 sendCap
	var k2 capSock
	k2.feed(&c2, 5, 80e6, true)
	c2.SetPacenetCap(30e6)
	if rate, apply, _ := k2.feed(&c2, 1, 80e6, true); rate != 30e6 || !apply {
		t.Fatalf("the net's alone: rate %d apply %v", rate, apply)
	}
	c2.SetPacenetCap(0)
	if rate, _, lift := k2.feed(&c2, 1, 80e6, true); rate != 0 || !lift {
		t.Fatalf("the net's lifted: rate %d lift %v", rate, lift)
	}
	// the runner: the net's hand-off sets the lesser at once
	sys := &fakeCapSys{ok: true, st: tcpStat{dlvOK: true, deliveryRate: 80e6, bytesAcked: 1 << 30, segsOut: 1 << 20, rtt: 50000}}
	l := newSendCapLink(sys, nil)
	for i := 1; i <= 5; i++ {
		sys.st.bytesAcked += 8e6
		sys.st.segsOut += 6000
		l.step(time.Duration(i)*100*time.Millisecond, uint64(i)*8e6)
	}
	wl := &wcLink{}
	wl.sc.Store(l)
	direct := 0
	set := func(*net.TCPConn, uint64) bool { direct++; return true }
	if !wl.netSet(set, 40e6) || direct != 0 || sys.sets[len(sys.sets)-1] != 40e6 {
		t.Fatalf("hand-off: direct %d, sets %v", direct, sys.sets)
	}
	if !wl.netSet(set, 0) || sys.sets[len(sys.sets)-1] != 100e6 {
		t.Fatalf("the net's lifted: sets %v (want back to the own 100 MB/s)", sys.sets)
	}
	var bare wcLink
	if !bare.netSet(set, 40e6) || direct != 1 {
		t.Fatal("no C4-lite: the net sets the socket itself")
	}
}

// TestSendCapNoDeliveryRate: a kernel without tcpi_delivery_rate gets no
// cap of C4-lite's, and one log line for the process.
func TestSendCapNoDeliveryRate(t *testing.T) {
	var lines []string
	f := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	coreLog.Store(&f)
	defer coreLog.Store(nil)
	sendcapNoDlv.Store(false)
	for range 2 {
		sys := &fakeCapSys{ok: true, st: tcpStat{bytesAcked: 1 << 30, segsOut: 1 << 20}}
		l := newSendCapLink(sys, nil)
		for i := 1; i <= 30; i++ {
			sys.st.bytesAcked += 8e6
			sys.st.segsOut += 6000
			l.step(time.Duration(i)*100*time.Millisecond, uint64(i)*8e6)
		}
		if len(sys.sets) != 0 {
			t.Fatalf("caps set: %v", sys.sets)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "tcpi_delivery_rate") {
		t.Fatalf("log lines: %q", lines)
	}
	t.Log(lines[0])
}

// TestSendCapStatusLine: the document's line (§3.4).
func TestSendCapStatusLine(t *testing.T) {
	sys := &fakeCapSys{ok: true, st: tcpStat{dlvOK: true, deliveryRate: 10e6, bytesAcked: 1 << 30, segsOut: 1 << 20}}
	l := newSendCapLink(sys, nil)
	for i := 1; i <= 10; i++ {
		sys.st.appLimited = i <= 5
		sys.st.bytesAcked += 1e6
		sys.st.segsOut += 700
		l.step(time.Duration(i)*100*time.Millisecond, uint64(i)*1e6)
	}
	got := l.line(2)
	want := "sendcap link 2: cap 100 Mbit/s (1.25 x max delivery 2s), app-limited 50%, idle-lifted 0"
	if got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}

// capOvershoot is the most the sender sent over any round trip from one
// round trip past its first sample that was not app-limited, against what
// the bottleneck carries in it.
func capOvershoot(m *wmModel) float64 {
	rtt := m.rtt
	from := m.main.fullAt + rtt
	if m.main.fullAt < 0 || from+rtt > m.end {
		return 0
	}
	sum, worst := int64(0), int64(0)
	for t := from; t < m.end; t++ {
		sum += int64(m.txWire[t])
		if t-from >= rtt {
			sum -= int64(m.txWire[t-rtt])
		}
		if t-from >= rtt-1 {
			worst = max(worst, sum)
		}
	}
	return float64(worst) / (m.rate * float64(rtt))
}

// TestSendCapModelNoOvershoot: the link model's Linux BBR sender (Z5) both
// ways (the exit's for 4 downloads with the edge's window core and the
// exit's pacing net; the edge's for 2 uploads), the probe on the same link:
// with C4-lite no round trip after the first that was not app-limited sends
// more than 1.3 times the bottleneck, on paths where 1.25 times the
// delivery rate is over C4-lite's floor (1 MiB/s); and with nothing left to
// send the cap is lifted. Without C4-lite for reference, and a 5 Mbit/s
// path where the floor is the cap (logged).
func TestSendCapModelNoOvershoot(t *testing.T) {
	paths := []struct {
		mbit   float64
		rtt, q int
		loss   float64
		assert bool
	}{
		{10, 72, 50, 0, true}, {25, 72, 50, 0, true}, {100, 72, 50, 0, true}, {100, 20, 20, 0, true},
		{300, 150, 50, 0, true}, {100, 72, 50, 0.001, true}, {5, 72, 100, 0, false},
	}
	for _, p := range paths {
		for _, up := range []bool{false, true} {
			var res [2]struct {
				ratio, mbit float64
				resend      float64
			}
			for i, on := range []bool{false, true} {
				c := wmPath(p.mbit, wmMS(p.rtt), wmMS(p.q))
				c.loss, c.dur, c.sendCap, c.txLog = p.loss, 20*time.Second, on, true
				c.mode = wmCore
				c.streams = make([]wmStreamCfg, 4)
				if up {
					c.exit, c.streams = true, make([]wmStreamCfg, 2)
				}
				m := newModel(c)
				r := m.run()
				res[i].ratio, res[i].mbit, res[i].resend = capOvershoot(m), r.mbit, r.resend
			}
			dir := "down"
			if up {
				dir = "up"
			}
			name := fmt.Sprintf("%s %4.0f/%d q%d l%g", dir, p.mbit, p.rtt, p.q, 100*p.loss)
			t.Logf("%-24s worst RTT %.2f× the bottleneck (without C4-lite %.2f×); %.1f Mbit/s resent %.2f%% (without %.1f, %.2f%%)",
				name, res[1].ratio, res[0].ratio, res[1].mbit, 100*res[1].resend, res[0].mbit, 100*res[0].resend)
			if p.assert && res[1].ratio > 1.3 {
				t.Errorf("%s: a round trip sent %.2f times the bottleneck with C4-lite", name, res[1].ratio)
			}
		}
	}
	// lifted once the link has nothing left to send
	c := wmPath(100, wmMS(72), wmMS(50))
	c.dur, c.sendCap, c.mode, c.quietAt = 16*time.Second, true, wmCore, 12*time.Second
	c.streams = make([]wmStreamCfg, 4)
	m := newModel(c)
	m.run()
	t.Logf("quiet from 12 s: C4-lite's cap lifted at %.1f s", float64(m.capOff)/1000)
	if m.capOff == 0 || m.capOff > 12000+2500 {
		t.Fatalf("quiet from 12 s: the cap lifted at %d ms (0: never)", m.capOff)
	}
}
