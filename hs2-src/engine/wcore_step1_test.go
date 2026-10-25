package engine

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Step 1's sighted sides only (the decision of 19 Mehr, WINDOW_CORE §2.2,
// §8.1, §11). The red team's sweeps found the "blind" sides of the core, the
// exit's core for uploads and the edge's for downloads with an old exit (no
// sender's fields in its record), still draining links behind a
// per-connection policer (8 of 96 model runs at 60% of the path, most at
// ~300 ms round trips): growing the windows lifts the sender's BBR pacing
// past the policer for good (app-limited samples never lower its max-bw
// filter), and a receiver with no record of the sender cannot see the
// policer's signature. So the edge's core leaves OFF only once the exit sent
// an extended record (srtt != 0), and the exit's sessions get no core
// unless the lab knob WcoreUpload is set (until step 3 sends the exit a
// record of the edge's socket).

// The exit's sessions are not registered with the runner by default; with
// the lab knob they are, for their uploads; with the pacing cap's lab
// experiment alone, for the cap only (no core).
func TestWcoreStep1ExitNotRegistered(t *testing.T) {
	up, pc := WcoreUpload, WcorePaceCap
	t.Cleanup(func() { WcoreUpload, WcorePaceCap = up, pc })
	pair := func() (cli, srv *smux.Session, ma, mb *linkMeter) {
		t.Helper()
		a, b := net.Pipe()
		ma, mb = &linkMeter{}, &linkMeter{}
		cli, _, err := newSession(a, false, nil, ma)
		if err != nil {
			t.Fatal(err)
		}
		srv, _, err = newSession(b, true, nil, mb)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cli.Close(); srv.Close() })
		return cli, srv, ma, mb
	}

	WcoreUpload, WcorePaceCap = 0, 0
	cli, srv, ma, mb := pair()
	if ma.wc == nil || ma.wc.side != wcEdge || ma.wc.off || !wcRegistered(cli) {
		t.Fatalf("the edge's session: core %+v registered %v, want the edge's core", ma.wc, wcRegistered(cli))
	}
	if mb.wc != nil || wcRegistered(srv) {
		t.Fatalf("the exit's session got a core by default: %+v (registered %v)", mb.wc, wcRegistered(srv))
	}
	if newWcLink(nil, wcTestConfig(), nil, nil, true, nil) != nil {
		t.Fatal("newWcLink made the exit's core by default")
	}

	WcorePaceCap = 1.25
	_, srv, _, mb = pair()
	if mb.wc == nil || !mb.wc.off || !wcRegistered(srv) {
		t.Fatalf("the exit's session with the pacing cap's experiment: %+v, want a registration for the cap only (off)", mb.wc)
	}
	WcorePaceCap = 0

	WcoreUpload = 1
	_, srv, _, mb = pair()
	if mb.wc == nil || mb.wc.side != wcExit || mb.wc.off || !wcRegistered(srv) {
		t.Fatalf("the exit's session with WcoreUpload: core %+v registered %v, want the exit's core", mb.wc, wcRegistered(srv))
	}
}

// queued is how long what the pipe holds waits before it leaves the
// bottleneck: the queue a sender's srtt sees on top of the round trip.
func (p *wcPipe) queued() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(0, time.Until(p.lastDep))
}

// wcExitRecords stores the exit's stats record in mtr every 500 ms until the
// test ends, as openStats does from the exit's replies: an old exit's (64
// bytes: no sender's fields, srtt 0) while ext is false, a new exit's
// (96 bytes: its socket's srtt, the round trip rtt and the download pipe's
// queue, its min_rtt and its segments) once ext is set.
func wcExitRecords(t *testing.T, mtr *linkMeter, p *wcPath, rtt time.Duration, ext *atomic.Bool) {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for seq := uint32(1); ; seq++ {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			tx := mtr.rdBytes.Load()
			r := &statsRec{seq: seq, mono: uint64(seq) * uint64(500*time.Millisecond), tx: tx, at: time.Now()}
			if ext.Load() {
				r.srtt = uint32((rtt + p.down.queued()) / time.Microsecond)
				r.minRTT = uint32(rtt / time.Microsecond)
				r.segsOut = tx / 1448
			}
			mtr.peer.Store(r)
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// wcOldExitRig is an edge's core on the path where TestWcoreRunnerGrowsAndDrains
// grows a window past Need (5 MB/s, a 100 ms round trip, Need 256 KiB),
// with the exit's records as wcExitRecords stores them.
func wcOldExitRig(t *testing.T) (l *wcLink, ws *wcSet, st *smux.Stream, ext *atomic.Bool, need uint32) {
	p := newWcPath(5<<20, 50*time.Millisecond)
	cfg := wcTestConfig()
	mtr := &linkMeter{}
	mtr.rttMin.Store(100_000)
	edge, exit := wcSessions(t, p.edge(), p.exit(), cfg, mtr)
	ws = &wcSet{tracePath: filepath.Join(t.TempDir(), "trace")}
	l = newWcLink(edge, cfg, nil, mtr, false, nil)
	ws.put(l)
	st = wcDownload(t, edge, exit)
	ext = &atomic.Bool{}
	wcExitRecords(t, mtr, p, 100*time.Millisecond, ext)
	runWcSet(t, ws)
	return l, ws, st, ext, uint32(cfg.NeedStreamBuffer)
}

// wcStayedOff checks every trace line of the rig so far: OFF, no budget, no
// ceiling and no window past what it has without the core (Need, or its
// cover: smux Cover); it returns how many saw a fast stream.
func wcStayedOff(t *testing.T, tr []wcTraceLine, need uint32) (fast int) {
	t.Helper()
	base := smux.Cover(need, ^uint32(0))
	for _, x := range tr {
		if x.State != "off" || x.B != 0 || x.BPub != 0 {
			t.Fatalf("an old exit's records only: %s, budget %d/%d (%+v)", x.State, x.B, x.BPub, x)
		}
		for _, s := range x.Streams {
			if s.Ceil != 0 || s.Win > base {
				t.Fatalf("an old exit's records only: ceiling %d, window %d (Need %d, its cover %d): %+v", s.Ceil, s.Win, need, base, x)
			}
		}
		if x.NF > 0 {
			fast++
		}
	}
	return fast
}

// An edge whose exit sends only 64-byte records (no sender's fields, srtt
// 0) never leaves OFF and never sets a ceiling, on a path where the core
// grows the window past Need with a new exit (TestWcoreRunnerGrowsAndDrains).
// Before step 1's decision the core waited for the exit's first record of
// any kind and started "blind" on it within a second or two.
func TestWcoreStep1OldExitStaysOff(t *testing.T) {
	l, ws, st, _, need := wcOldExitRig(t)
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if s := wcState(l.inStats.state.Load()); s != wcOff || l.inStats.active.Load() || st.CoreCeil() != 0 {
			t.Fatalf("an old exit's records only: core %v (active %v), ceiling %d", s, l.inStats.active.Load(), st.CoreCeil())
		}
	}
	tr := readTrace(t, ws.tracePath)
	if fast := wcStayedOff(t, tr, need); fast < 5 {
		t.Fatalf("the stream counted as fast in %d of %d evaluations: the path tested nothing", fast, len(tr))
	}
	if n := wcoreNote(&linkMeter{wc: l}); n != "" {
		t.Fatalf("link-end note %q for a core that never left OFF", n)
	}
}

// An edge with a new exit leaves OFF only after the exit's first extended
// record: OFF for as long as only 64-byte records come, then out of OFF
// once one with the sender's fields (srtt != 0) does.
func TestWcoreStep1NewExitLeavesOffOnRecord(t *testing.T) {
	l, ws, st, ext, need := wcOldExitRig(t)
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if s := wcState(l.inStats.state.Load()); s != wcOff || st.CoreCeil() != 0 {
			t.Fatalf("before the exit's first extended record: core %v, ceiling %d", s, st.CoreCeil())
		}
	}
	before := readTrace(t, ws.tracePath)
	if fast := wcStayedOff(t, before, need); fast < 3 {
		t.Fatalf("the stream counted as fast in %d of %d evaluations: the path tested nothing", fast, len(before))
	}
	recAt := time.Now()
	ext.Store(true)
	wcWaitFor(t, 20*time.Second, "the core never left OFF after the exit's extended records began", func() bool {
		return l.inStats.active.Load()
	})
	tr := readTrace(t, ws.tracePath)
	i := slices.IndexFunc(tr, func(x wcTraceLine) bool { return x.State != "off" })
	if i < 0 {
		t.Fatalf("no trace line out of OFF in %d", len(tr))
	}
	at, _ := time.Parse(time.RFC3339Nano, tr[i].T)
	if at.Before(recAt) {
		t.Fatalf("left OFF at %v, before the exit's first extended record (%v)", at.Format(time.StampMilli), recAt.Format(time.StampMilli))
	}
	t.Logf("left OFF %v after the exit's extended records began (%s)", at.Sub(recAt).Round(time.Millisecond), tr[i].State)
}

// The sender's fields are what count, srtt != 0: a record with segments in
// it and no srtt (an exit with no TCP_INFO reads 0 in all of them) is no
// extended record, and leaves the edge's core OFF.
func TestWcoreStep1SeenNeedsSrtt(t *testing.T) {
	c := wtCore(1)
	in := wtIn(wtT0, 1, wtNeed)
	in.snd = wcSnd{fresh: true, segsOut: 100, retrans: 1}
	c.step(in)
	if c.sndSeen || !c.sndAny {
		t.Fatalf("a record with srtt 0: sndSeen %v sndAny %v, want only a record seen", c.sndSeen, c.sndAny)
	}
	in.now = wtT0.Add(time.Second)
	in.snd = wcSnd{fresh: true, srtt: 101 * time.Millisecond, minRTT: 100 * time.Millisecond, segsOut: 200}
	c.step(in)
	if !c.sndSeen {
		t.Fatal("an extended record (srtt != 0): sndSeen false")
	}
}

// An exit whose kernel's TCP_INFO ends before tcpi_min_rtt (Linux before
// 4.6) still fills srtt, and nothing past it: no q_snd and no resend share
// of the sender ever comes. Its records are no extended record either (the
// blind regime step 1 leaves OFF), whatever its srtt and segments say.
func TestWcoreStep1SeenNeedsMinRTT(t *testing.T) {
	for _, tc := range []struct {
		name string
		snd  wcSnd
	}{
		{"srtt only", wcSnd{fresh: true, srtt: 101 * time.Millisecond}},
		{"srtt and segments, no min_rtt", wcSnd{fresh: true, srtt: 101 * time.Millisecond, segsOut: 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := wtCore(1)
			for i := range 4 {
				in := wtIn(wtT0.Add(time.Duration(i)*time.Second), 1, wtNeed)
				in.snd = tc.snd
				c.step(in)
			}
			if c.sndSeen || !c.sndAny {
				t.Fatalf("records with no min_rtt: sndSeen %v sndAny %v, want only records seen", c.sndSeen, c.sndAny)
			}
		})
	}
}

// In the model the exit's side (uploads) and an edge with an old exit run
// as the windows without the core under the default config: on the red
// team's policer grid at ~300 ms, where those blind sides drained links, and
// on clean and lossy paths where they started. The exit's side is base
// exactly (no core, and no cover on the exit's end: SmuxCover); an edge with
// an old exit has its downloads' covers only until the exit's first record
// shows it has no TCP_INFO to see a policer by (coverBlind; red team, H2:
// behind a policer at 60% of 300/72 its covers resent 10.4% against base's
// 0.3%), so its runs are held to no budget, OFF throughout and base's
// resends. Not to base's rate: the covered start takes the sender out of
// BBR's STARTUP for good, and its PROBE_BW paces each step where STARTUP
// bursts it (3–17% under base over six seeds, with the model's max_bw or
// Linux's), and the model's max_bw, which expires after 10 rounds whatever
// came, takes a PROBE_RTT sample for the path in one seed of 300/150 behind
// 20 ms (32.6 against 54.7 Mbit/s). The lab knob (WcoreUpload) still runs
// the exit's core.
func TestWcoreStep1ModelBlindSidesAreBase(t *testing.T) {
	type run struct {
		rate      float64
		rtt, q    int
		loss, pol float64 // pol: the policer's share of the path
		seed      uint64
	}
	// (behind the policer the queue never fills: its size changes nothing;
	// the grid's blind runs that drained were at 250 to 350 ms, most at 300)
	var runs []run
	for _, rtt := range []int{250, 300, 350} {
		for seed := uint64(1); seed <= 2; seed++ {
			runs = append(runs, run{300, rtt, 50, 0, 0.6, seed})
		}
	}
	runs = append(runs, run{1000, 300, 50, 0, 0.6, 2})
	runs = append(runs, run{300, 150, 50, 0, 0, 1}, run{1000, 100, 50, 0.001, 0, 1}, run{300, 150, 20, 0.001, 0, 3})
	for _, x := range runs {
		for _, side := range []string{"up", "dn-old"} {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.loss, c.policer, c.seed, c.dur = x.loss, x.pol*x.rate*wmMbit, x.seed, 40*time.Second
			c.exit, c.oldExit = side == "up", side == "dn-old"
			b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
			cv := b
			if side == "dn-old" {
				cv = wmRun(c.with(wmCover))
			}
			msg := fmt.Sprintf("%s %s l%g pol%g s%d: base %.1f Mbit resent %.3f%% | cover %.1f Mbit resent %.3f%% | core %.1f Mbit resent %.3f%% peakB %.1f MiB last %v",
				side, c.name, 100*x.loss, x.pol, x.seed, b.mbit, 100*b.resend, cv.mbit, 100*cv.resend, k.mbit, 100*k.resend, float64(k.peakB)/(1<<20), k.lastState)
			switch {
			case k.peakB != 0 || k.lastState != wcOff:
				t.Errorf("the default config ran the core: %s", msg)
			case side == "up" && (k.mbit != b.mbit || k.resend != b.resend || !slices.Equal(k.rtxSec, b.rtxSec)):
				t.Errorf("the default config is not base: %s", msg)
			case side == "dn-old" && (k.mbit < 0.55*b.mbit || k.resend > b.resend+0.003):
				t.Errorf("the default config is not base's resends: %s", msg)
			default:
				t.Log(msg)
			}
		}
	}
	// the lab knob: the exit's core runs (it grows the windows past Need and
	// the uploads past base on a clean 300/150: 90.5 to 115.4 Mbit/s)
	up := WcoreUpload
	WcoreUpload = 1
	t.Cleanup(func() { WcoreUpload = up })
	c := wmPath(300, wmMS(150), wmMS(50))
	c.exit, c.seed, c.dur = true, 1, 40*time.Second
	b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
	msg := fmt.Sprintf("up %s with WcoreUpload: base %.1f Mbit | core %.1f Mbit resent %.3f%% peakB %.1f MiB", c.name, b.mbit, k.mbit, 100*k.resend, float64(k.peakB)/(1<<20))
	if k.peakB <= wmNeed || k.mbit < 1.1*b.mbit {
		t.Errorf("the lab knob's exit core did not run: %s", msg)
	} else {
		t.Log(msg)
	}
}
