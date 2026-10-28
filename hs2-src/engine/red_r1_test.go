package engine

import (
	"fmt"
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
	for k := 0; k <= hintRun; k++ {
		t0 := time.Now()
		exitHintTick(rules, now+int64(k)*int64(moveTick))
		took = max(took, time.Since(t0))
	}
	waitQuiet(t, "every hint on the healthy stream", func() bool {
		got := 0
		good.ids.Range(func(any, any) bool { got++; return true })
		return got == n
	})
	t.Logf("longest tick %v; writes on the stalled stream %d", took, stalled.writes.Load())
	if took > 50*time.Millisecond {
		t.Fatalf("a tick took %v with one hint stream stalled (%d hints due)", took, n)
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
