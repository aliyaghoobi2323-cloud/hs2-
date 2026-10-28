package engine

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// R1 red team, first pass (method step 4; docs/architecture/wip/r1/redteam/
// REPORT.fa.md): attacks on the R1 candidate j3g that need no rig.

// An edge that met an older exit (a link without CapGroup: level 1, or
// HS2_TUNE_QUIET=0 there) turns its QUIET links off. The other server is
// then upgraded and restarted — servers are upgraded one at a time — and its
// new links have CapGroup: the edge must take its QUIET links back, not keep
// every connection on BULK links until it restarts itself. One line says so,
// once.
func TestRedQuietBackAfterPeerUpgrade(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	var mu sync.Mutex
	var lines []string
	lm := NewLinkManager(nil, 1, 1, 8, func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	})
	q := lm.enableQuiet()
	full := tlscarrier.LocalAuthHdr().Caps
	if full&tlscarrier.CapGroup == 0 {
		t.Skip("this binary's caps lack CapGroup (a kill switch)")
	}
	q.noteCaps(full | tlscarrier.CapBulk) // the first link: on
	if !q.active() {
		t.Fatalf("a level 2 exit: QUIET links off (%s)", q.line())
	}
	// the exit is rolled back to an older build: its links lack the level 2 caps
	q.noteCaps(full &^ (tlscarrier.CapGroup | tlscarrier.CapHint | tlscarrier.CapMoveBack))
	if q.active() || lm.quietWant() != 0 {
		t.Fatalf("an older exit: QUIET links still on (%s)", q.line())
	}
	// ... and upgraded again: its first link after the restart has CapGroup
	q.noteCaps(full | tlscarrier.CapBulk)
	if !q.active() || lm.quietWant() != q.want {
		t.Fatalf("the exit upgraded and restarted: QUIET links still off (%s)", q.line())
	}
	q.noteCaps(full | tlscarrier.CapBulk) // its next links: nothing more
	mu.Lock()
	defer mu.Unlock()
	off, on := 0, 0
	for _, l := range lines {
		switch {
		case strings.Contains(l, "no group streams"):
			off++
		case strings.Contains(l, "group streams again"):
			on++
		}
	}
	if off != 1 || on != 1 {
		t.Fatalf("log lines: %d off, %d back on, want one each: %q", off, on, lines)
	}
}

// stalledHint is a kindHint stream whose writer is stalled (the QUIET
// link's socket waits on an RTO under loss, the case the hint was made
// for): each write waits out its deadline.
type stalledHint struct {
	dl     atomic.Value
	writes atomic.Int64
}

func (s *stalledHint) Write(p []byte) (int, error) {
	s.writes.Add(1)
	d, _ := s.dl.Load().(time.Time)
	if w := time.Until(d); d.IsZero() || w > time.Second {
		time.Sleep(time.Second)
	} else {
		time.Sleep(w)
	}
	return 0, os.ErrDeadlineExceeded
}

func (s *stalledHint) Close() error                       { return nil }
func (s *stalledHint) SetWriteDeadline(t time.Time) error { s.dl.Store(t); return nil }

// countHint is a kindHint stream that takes every write at once.
type countHint struct{ ids sync.Map }

func (c *countHint) Write(p []byte) (int, error) {
	if id, ok := hintDecode(p); ok {
		c.ids.Store(id, true)
	}
	return len(p), nil
}

func (c *countHint) Close() error { return nil }

// redExitGroups makes n group connections at the exit, as serveStream does
// (exitGroups.add), over one emulated QUIET link; their meter is the link's.
func redExitGroups(t *testing.T, n int, base uint32) (*classPair, []*exitGroup) {
	t.Helper()
	p := newClassPair(t, newAngleWire(4*angleRateBps, time.Millisecond), newAngleWire(4*angleRateBps, time.Millisecond),
		classCaps(tlscarrier.CapQuiet))
	var egs []*exitGroup
	for i := 0; i < n; i++ {
		if _, err := p.edge.sess.OpenGroup(7, 0, base+uint32(i)); err != nil {
			t.Fatal(err)
		}
		st, err := p.exit.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		eg := exitGroups.add(st, p.xmtr)
		if eg == nil {
			t.Fatalf("group %d refused", base+uint32(i))
		}
		egs = append(egs, eg)
	}
	t.Cleanup(func() {
		for _, eg := range egs {
			exitGroups.remove(eg)
			eg.Close()
		}
	})
	return p, egs
}

// The exit judges its group connections once a moveTick in one goroutine
// (runExitHints) and writes each hint on the stream of the connection's own
// QUIET link first, waiting up to 200 ms for it, then on the next. With two
// QUIET links (max_links past 5) and one of them stalled, each hint for a
// connection on it holds the whole loop 200 ms: with 20 downloads due at
// once the tick takes 4 s, every other connection's hint waits that long,
// and the late ticks (steps > 1) start every rule's run again. The loop
// must not wait on a stream: a tick takes no more than a few ms, and the
// hints still reach the edge on the healthy link.
func TestRedHintTickNeverWaits(t *testing.T) {
	angleCores(t)
	if MoveHintOff() {
		t.Skip("HS2_TUNE_MOVE_HINT=0")
	}
	const n = 20
	stalledLink, egs := redExitGroups(t, n, 0x5ED00000)
	stalled, good := &stalledHint{}, &countHint{}
	cs, cg := &hintChan{st: stalled, mtr: stalledLink.xmtr}, &hintChan{st: good, mtr: &linkMeter{}}
	exitHints.add(cs)
	exitHints.add(cg)
	t.Cleanup(func() { exitHints.remove(cs); exitHints.remove(cg) })
	// every connection's writer has been inside a big write for a while: a
	// send backlog (hintRule)
	now := ctrlNow()
	for _, eg := range egs {
		eg.big.at.Store(now - int64(time.Second))
	}
	rules := map[*exitGroup]*hintRule{}
	var took time.Duration
	var due time.Time
	for k := 0; k <= hintRun; k++ {
		t0 := time.Now()
		exitHintTick(rules, now+int64(k)*int64(moveTick))
		took = max(took, time.Since(t0))
		due = t0
	}
	waitQuiet(t, "every hint on the healthy stream", func() bool {
		got := 0
		good.ids.Range(func(any, any) bool { got++; return true })
		return got == n
	})
	all := time.Since(due)
	t.Logf("longest tick %v; every hint on the healthy stream %v after it was due; writes on the stalled stream %d",
		took, all, stalled.writes.Load())
	if took > 50*time.Millisecond {
		t.Fatalf("a tick took %v with one hint stream stalled (%d hints due)", took, n)
	}
	if all > time.Second {
		t.Fatalf("the hints reached the healthy stream %v after they were due (one stream stalled)", all)
	}
}

// A move stream that reaches the exit before its group's main stream — the
// main stream's SYN waits on the QUIET link (an RTO, or the exit's reader
// of that link parked on a full buffer) while the BULK link is quick — is
// refused at once today, and the connection is wedged: the exit reads the
// main stream up to the move and waits for the rest for good, the edge's
// frames on the refused move stream are dropped. §4.5 gives a sub-stream 2 s
// for its group to be made. Here the edge moves while its main stream's SYN
// is held 600 ms on the QUIET link, then uploads: every byte must arrive.
func TestRedMoveBeforeMainSyn(t *testing.T) {
	angleCores(t)
	r := newQuietRig(t, 4*angleRateBps, 10*time.Millisecond, false)
	w := r.wires[0] // the QUIET link's way to the exit
	w.mu.Lock()
	w.holdTill = time.Now().Add(600 * time.Millisecond)
	w.mu.Unlock()
	st := r.open(9, qpUp)
	gs := st.(*groupStream)
	refused0 := exitGroups.refused.Load()
	if !r.q.move(gs, "up") || gs.g.Moved() == nil {
		t.Fatalf("no move (%s)", r.q.line())
	}
	const total = 1 << 20
	b := make([]byte, 16<<10)
	done := make(chan error, 1)
	go func() {
		for off := 0; off < total; off += len(b) {
			if _, err := st.Write(b); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	up := r.upCount(qpUp)
	deadline := time.Now().Add(10 * time.Second)
	for up.Load() < total && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("read at the exit %d of %d; move streams refused %d", up.Load(), total, exitGroups.refused.Load()-refused0)
	if up.Load() != total {
		t.Fatalf("the exit read %d of %d bytes: the connection is wedged (move streams refused %d)",
			up.Load(), total, exitGroups.refused.Load()-refused0)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer is stuck")
	}
	st.Close()
}

// The one-way delays (owd.go) are the receiver's monotonic clock less the
// sender's stamp: the two clocks' offset rides in every reading, and the
// base (the least of 30 s) takes it out only while it stays put. NTP moves
// CLOCK_MONOTONIC's rate while it slews an offset (systemd-timesyncd, the
// Kharej server's: up to 500 ppm, 0.5 ms each second; chrony up to 8.3 %),
// a VM that is paused or migrated comes back with its clock behind, and a
// server that lost its NTP servers for hours (Iran's shutdowns) slews what
// it drifted once they are back. Each makes the sending end's stamps fall
// behind the path: the receiver reads the shift as a queue of that
// direction until the base ages past it (30 s).
//
// TestRedOWDClockSlew measures the estimator (a 50 ms shift slewed within a
// second: the reading and how long it lasts; a 500 ppm slew: the queue it
// reads) and what α (sendcap.go), which takes the QUIET link's one-way
// queue of its direction as its own (version 5) with no cross-check, makes
// of it on a 100 Mbit/s path with no real queue: the cap must keep the path
// (>= 0.9 of it) through the false queue, as srtt − min_rtt (no clock in it)
// says there is none.
func TestRedOWDClockSlew(t *testing.T) {
	const ms = time.Millisecond
	// the estimator: a message each 50 ms, a path of 36 ms
	for _, c := range []struct {
		name  string
		shift func(t time.Duration) time.Duration // how far the sender's clock fell behind at t
	}{
		{"50 ms within a second (chrony, a paused VM)", func(t time.Duration) time.Duration {
			return min(max(t-10*time.Second, 0), 600*ms) * 50 / 600
		}},
		{"500 ppm from 10 s (timesyncd)", func(t time.Duration) time.Duration {
			return max(t-10*time.Second, 0) / 2000
		}},
	} {
		var o owdLink
		var worst time.Duration
		var falseFor time.Duration
		for at := time.Duration(0); at < 60*time.Second; at += 50 * ms {
			now := int64(time.Hour + at)
			stamp := now - int64(36*ms) - int64(c.shift(at))
			var b [owdMsgLen]byte
			owdEncode(b[:], stamp, 0, false)
			o.recv(now, b[:])
			q := time.Duration(o.qIn.Load())
			worst = max(worst, q)
			if q > alphaLow {
				falseFor += 50 * ms
			}
		}
		t.Logf("estimator, %s: false queue up to %v, past alphaLow (%v) for %v of 60 s", c.name, worst, alphaLow, falseFor)
	}
	// α on a 100 Mbit/s path with no queue of its own, reading the false one
	// (the shape the estimator gave: 50 ms for 30 s from 10 s)
	path := 100e6 / 8
	c := &sendCap{linked: true, alpha: true}
	at, acked := time.Hour, uint64(0)
	capNow := path * 1.25
	qOwn := time.Duration(0)
	var sum, minCap float64
	minCap = math.Inf(1)
	n := 0
	for i := 0; i < 600; i++ { // 60 s
		dlv := math.Min(capNow, path)
		qOwn += time.Duration((capNow - path) / path * float64(100*ms))
		qOwn = max(qOwn, 0)
		at += 100 * ms
		acked += uint64(dlv / 10)
		fake := time.Duration(0)
		if i >= 100 && i < 400 {
			fake = 50 * ms
		}
		c.Update(capSample{Now: at, Delivery: dlv, Acked: acked, Written: acked, Segs: uint32(acked / 1000),
			SRTT: 72*ms + qOwn, MinRTT: 72 * ms, OWD: qOwn + fake, OWDok: true})
		if c.own > 0 {
			capNow = float64(c.own)
		}
		if i >= 100 && i < 400 {
			sum += dlv
			n++
			minCap = math.Min(minCap, capNow)
		}
	}
	mean := sum / float64(n) / path
	t.Logf("alpha through 30 s of a false 50 ms queue: delivered %.3f of the path, cap down to %.1f Mbit/s (%d cuts)",
		mean, minCap*8/1e6, c.aCuts)
	if mean < 0.9 {
		t.Fatalf("a clock slew cut a 100 Mbit/s path with no queue to %.3f of it for 30 s (cap down to %.1f Mbit/s)",
			mean, minCap*8/1e6)
	}
}
