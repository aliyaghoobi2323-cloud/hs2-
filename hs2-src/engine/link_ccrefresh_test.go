package engine

import (
	"net"
	"runtime"
	"slices"
	"testing"
	"time"
)

// us200: the watch refreshes only a light, BBR-past-STARTUP, low-estimate link.
func TestCCRefreshTickRules(t *testing.T) {
	type step struct {
		acked   uint64
		notsent uint32
		ci      ccInfo
		want    bool
	}
	probe := ccInfo{bbr: true, gain: 320, bwBytes: 50_000}
	cases := []struct {
		name  string
		steps []step
	}{
		{"light link past startup: refreshed on the 2nd light tick", []step{
			{1000, 0, probe, false}, {2000, 0, probe, false}, {3000, 0, probe, true},
		}},
		{"bulk link never", []step{
			{1000, 0, probe, false}, {1000 + ccLightBytes, 0, probe, false}, {1000 + 2*ccLightBytes, 0, probe, false}, {1000 + 3*ccLightBytes, 0, probe, false},
		}},
		{"queued data resets the light count", []step{
			{1000, 0, probe, false}, {2000, 0, probe, false}, {3000, 5, probe, false}, {4000, 0, probe, false}, {5000, 0, probe, true},
		}},
		{"still in startup: left alone", []step{
			{1000, 0, ccInfo{bbr: true, gain: ccBBRHighGain, bwBytes: 50_000}, false}, {2000, 0, ccInfo{bbr: true, gain: ccBBRHighGain}, false}, {3000, 0, ccInfo{bbr: true, gain: ccBBRHighGain}, false},
		}},
		{"high estimate: left alone", []step{
			{1000, 0, probe, false}, {2000, 0, ccInfo{bbr: true, gain: 256, bwBytes: ccRefreshBwMax}, false}, {3000, 0, ccInfo{bbr: true, gain: 256, bwBytes: ccRefreshBwMax}, false},
		}},
		{"not bbr: left alone", []step{
			{1000, 0, ccInfo{}, false}, {2000, 0, ccInfo{}, false}, {3000, 0, ccInfo{}, false},
		}},
	}
	for _, c := range cases {
		var st ccRefreshState
		for i, s := range c.steps {
			did := false
			got := ccRefreshTick(&st, tcpStat{bytesAcked: s.acked, notsent: s.notsent},
				func() (ccInfo, bool) { return s.ci, true }, func() bool { did = true; return true })
			if got != s.want || did != s.want {
				t.Fatalf("%s: step %d: got %v (reinit %v), want %v", c.name, i, got, did, s.want)
			}
		}
	}
}

// us200: on Linux a real socket in BBR is re-initialised by the toggle and
// keeps its algorithm.
func TestCCReinitRealSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			buf := make([]byte, 64<<10)
			for {
				if _, err := c.Read(buf); err != nil {
					break
				}
			}
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := c.(*net.TCPConn)
	ci, ok := tcpCCInfo(tc)
	if !ok {
		t.Fatal("no cc info")
	}
	if !ci.bbr {
		t.Skip("default congestion control is not bbr here")
	}
	// bulk on loopback: BBR finds its bandwidth and leaves STARTUP
	buf := make([]byte, 1<<20)
	for i := 0; i < 200; i++ {
		c.Write(buf)
	}
	time.Sleep(50 * time.Millisecond)
	before, _ := tcpCCInfo(tc)
	if !tcpCCReinit(tc) {
		t.Fatal("reinit failed")
	}
	after, ok := tcpCCInfo(tc)
	if !ok || !after.bbr || (after.gain != 0 && after.gain != ccBBRHighGain) || after.bwBytes >= before.bwBytes && before.bwBytes > 0 {
		t.Fatalf("before %+v, after reinit %+v ok=%v: want bbr in startup with its estimate reset", before, after, ok)
	}
	c.Write(buf[:64<<10]) // a few ACKs: the gains are set per ACK
	time.Sleep(20 * time.Millisecond)
	g, _ := tcpCCInfo(tc)
	t.Logf("before %+v, after %+v, after traffic %+v", before, after, g)
}

// cc203: the exit's watch runs the same rules per tracked socket and forgets
// a socket once its link is gone.
func TestCCExitWatch(t *testing.T) {
	was := linkCCRefresh.Load()
	defer linkCCRefresh.Store(was)
	w := &ccExitWatch{links: map[*net.TCPConn]*ccRefreshState{}}
	a, b := &net.TCPConn{}, &net.TCPConn{}
	linkCCRefresh.Store(false)
	w.track(a)()
	if len(w.links) != 0 {
		t.Fatal("key off: tracked")
	}
	linkCCRefresh.Store(true)
	ua := w.track(a)
	ub := w.track(b)
	acked := map[*net.TCPConn]uint64{a: 1000, b: 1000}
	stats := func(tc *net.TCPConn) (tcpStat, bool) { return tcpStat{bytesAcked: acked[tc]}, true }
	probe := func(*net.TCPConn) (ccInfo, bool) { return ccInfo{bbr: true, gain: 320, bwBytes: 50_000}, true }
	var done []*net.TCPConn
	reinit := func(tc *net.TCPConn) bool { done = append(done, tc); return true }
	step := func(da, db uint64) int {
		acked[a] += da
		acked[b] += db
		return w.tick(stats, probe, reinit)
	}
	// a stays light, b carries a download (>= ccLightBytes a tick)
	if n := step(0, 0); n != 0 { // first look
		t.Fatalf("tick 1: %d", n)
	}
	if n := step(100, ccLightBytes); n != 0 {
		t.Fatalf("tick 2: %d", n)
	}
	if n := step(100, ccLightBytes); n != 1 || len(done) != 1 || done[0] != a {
		t.Fatalf("tick 3: %d %v", n, done)
	}
	ua()
	ub()
	if len(w.links) != 0 {
		t.Fatal("untrack left sockets")
	}
}

// hl235: a quiet link with a cold estimate is re-initialised every
// ccIdleTicks quiet ticks, and at once in PROBE_RTT; data acknowledged,
// queued bytes, a retransmission, not BBR, a warm estimate (>=
// ccRefreshBwMax) or HS2_LINK_CC_IDLE=0: left alone.
func TestCCIdleTickRules(t *testing.T) {
	old := ccIdleOff
	t.Cleanup(func() { ccIdleOff = old })
	startup := ccInfo{bbr: true, gain: ccBBRHighGain, cwndGain: ccBBRHighGain, bwBytes: 1000}
	probeRTT := ccInfo{bbr: true, gain: ccBBRUnit, cwndGain: ccBBRUnit, bwBytes: 1000}
	type step struct {
		acked, retrans uint64
		notsent        uint32
		ci             ccInfo
	}
	quiet := func(n int, ci ccInfo) []step {
		var ss []step
		for i := 0; i < n; i++ {
			ss = append(ss, step{1000 + uint64(i)*100, 0, 0, ci})
		}
		return ss
	}
	cases := []struct {
		name string
		off  bool
		st   []step
		want []int // steps (indexes) that re-init
	}{
		{"quiet cold link: every ccIdleTicks", false, quiet(1+2*ccIdleTicks, startup), []int{ccIdleTicks, 2 * ccIdleTicks}},
		{"PROBE_RTT on a quiet link: at once", false, quiet(3, probeRTT), []int{1, 2}},
		{"data acknowledged: never", false, []step{{1000, 0, 0, probeRTT}, {1000 + ccIdleBytes, 0, 0, probeRTT}, {1000 + 2*ccIdleBytes, 0, 0, startup}, {1000 + 3*ccIdleBytes, 0, 0, startup}, {1000 + 4*ccIdleBytes, 0, 0, startup}}, nil},
		{"queued: never", false, []step{{1000, 0, 1, probeRTT}, {1000, 0, 1, probeRTT}, {1000, 0, 1, startup}, {1000, 0, 1, startup}}, nil},
		{"retransmitting (stuck): never", false, []step{{1000, 1, 0, probeRTT}, {1000, 2, 0, probeRTT}, {1000, 3, 0, startup}, {1000, 4, 0, startup}}, nil},
		{"not bbr: never", false, quiet(1+ccIdleTicks, ccInfo{}), nil},
		{"warm estimate: never", false, quiet(1+ccIdleTicks, ccInfo{bbr: true, gain: ccBBRUnit, cwndGain: ccBBRUnit, bwBytes: ccRefreshBwMax}), nil},
		{"key off: never", true, quiet(1+2*ccIdleTicks, startup), nil},
	}
	for _, c := range cases {
		ccIdleOff = c.off
		var st ccRefreshState
		wantAt := map[int]bool{}
		for _, i := range c.want {
			wantAt[i] = true
		}
		for i, s := range c.st {
			did := false
			got := ccRefreshTick(&st, tcpStat{bytesAcked: s.acked, retrans: s.retrans, notsent: s.notsent},
				func() (ccInfo, bool) { return s.ci, true }, func() bool { did = true; return true })
			if got != wantAt[i] || did != wantAt[i] {
				t.Fatalf("%s: step %d: got %v (reinit %v), want %v", c.name, i, got, did, wantAt[i])
			}
		}
	}
}

// pk236: a light link whose BBR leaves STARTUP again right after each refresh
// (prod rel69c: every ~5 s) is re-inited with a doubling gap (8 s .. 60 s),
// not every 2 ticks; a transfer (>= ccBulkBytes in a tick) clears the
// backoff so the next light spell refreshes as promptly as before.
func TestCCRefreshBackoff(t *testing.T) {
	probe := ccInfo{bbr: true, gain: 320, bwBytes: 50_000}
	var st ccRefreshState
	acked := uint64(1000) // 8 KB a tick: light, not hl235-quiet
	var at []int
	tick := func(i int, add uint64) {
		acked += add
		if ccRefreshTick(&st, tcpStat{bytesAcked: acked}, func() (ccInfo, bool) { return probe, true }, func() bool { return true }) {
			at = append(at, i)
		}
	}
	for i := 1; i <= 200; i++ { // 400 s light, always out of STARTUP
		tick(i, 8000)
	}
	want := []int{3, 7, 15, 31, 61, 91, 121, 151, 181}
	if !slices.Equal(at, want) {
		t.Fatalf("refresh ticks %v, want %v", at, want)
	}
	// a medium tick (light < x < bulk) leaves the backoff alone
	at = nil
	tick(201, 1<<20)
	for i := 202; i <= 205; i++ {
		tick(i, 8000)
	}
	if len(at) != 0 {
		t.Fatalf("medium tick cleared the backoff: %v", at)
	}
	// a transfer clears it: refreshed on the 2nd light tick after it
	tick(206, ccBulkBytes)
	for i := 207; i <= 211; i++ { // 212 would be the next (back 4)
		tick(i, 8000)
	}
	if !slices.Equal(at, []int{208}) {
		t.Fatalf("after a transfer: refresh ticks %v, want [208]", at)
	}
	// hl235 quiet re-init is not backed off: a quiet cold link right after a
	// light refresh still gets the re-init every ccIdleTicks (212 is the
	// light refresh: back 4 waited out, then back 8)
	if ccIdleOff {
		return
	}
	at = nil
	for i := 212; i <= 220; i++ {
		tick(i, 100)
	}
	if !slices.Equal(at, []int{212, 214, 217, 220}) {
		t.Fatalf("quiet re-init under backoff: ticks %v, want [212 214 217 220]", at)
	}
}
