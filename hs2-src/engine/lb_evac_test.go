package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// --- judge: a front's handshake failure is not path quality ---

func TestJudgeFailFrontNotLoss(t *testing.T) {
	old := lbJudgeData
	t.Cleanup(func() { lbJudgeData = old })

	front := fmt.Errorf("tlscarrier: reading server proof: %w",
		&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})
	conn := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	if dialConnectStage(front) || dialConnectStage(io.EOF) || dialConnectStage(errors.New("reality: auth failed")) {
		t.Fatal("a failure after the connect counted as the path's")
	}
	if !dialConnectStage(conn) || !dialConnectStage(fmt.Errorf("lb: %w", conn)) {
		t.Fatal("a connect failure not seen as the path's")
	}

	lbJudgeData = true
	for _, c := range []struct {
		d, f, cf int
		want     float64
	}{
		{2, 1, 0, 0},   // the lab case: one handshake timeout out of two dials
		{2, 2, 2, 0},   // too few dials to judge at all
		{8, 8, 0, 0},   // fronts failing: the front's fallback / cool-down, not the judge
		{8, 4, 4, 0.5}, // the path refuses connects: counted
		{0, 0, 0, 0},
	} {
		if g := judgeFail(c.d, c.f, c.cf); g != c.want {
			t.Fatalf("judgeFail(%d,%d,%d) = %v, want %v", c.d, c.f, c.cf, g, c.want)
		}
	}
	lbJudgeData = false
	if g := judgeFail(2, 1, 0); g != 0.5 {
		t.Fatalf("key off: old rule wanted 0.5, got %v", g)
	}
}

// The lab case through the judge: three paths at the same 1.1% loss, B's tick
// with one front-handshake failure out of two dials. Old rule: B lowered to
// ~0.19 ("lower: loss 1.1%"). New rule: B stays at 1.
func TestJudgeEqualPathsFrontFailure(t *testing.T) {
	old := lbJudgeData
	t.Cleanup(func() { lbJudgeData = old })
	run := func(on bool) (float64, string) {
		lbJudgeData = on
		j := newQ2Judge(lb2Floor, "k", "i", func(string, ...any) {})
		now := time.Unix(1000, 0)
		A, B, C := "10.0.0.2>10.0.1.1", "10.0.0.2>10.0.2.1", "10.0.0.2>10.0.3.1"
		for i := 0; i < 3; i++ {
			now = now.Add(lb2QualEvery)
			rs := map[string]q2Reading{A: {loss: 0.011}, B: {loss: 0.011}, C: {loss: 0.010}}
			if i == 1 {
				r := rs[B]
				r.fail = judgeFail(2, 1, 0)
				rs[B] = r
			}
			j.update(now, lb2QualEvery, rs)
			if i == 1 {
				return j.p[B].target, j.p[B].reason
			}
		}
		return 0, ""
	}
	if tgt, why := run(false); tgt > 0.3 {
		t.Fatalf("old rule: want B lowered (the bug), got %.2f (%s)", tgt, why)
	}
	if tgt, why := run(true); tgt != 1 {
		t.Fatalf("data-plane judge: B lowered to %.2f (%s) for a front failure", tgt, why)
	}
}

// The LB's result keeps the two counts apart.
func TestLBConnectFailCounted(t *testing.T) {
	b, err := NewLB([]LBPath{{Remote: "10.0.1.1", Weight: 1, Primary: true}, {Remote: "10.0.2.1", Weight: 1}}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	i, probe := b.acquire()
	b.result(i, probe, nil, fmt.Errorf("tlscarrier: reading server proof: %w", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}))
	i2, probe2 := b.acquire()
	b.result(i2, probe2, nil, &net.OpError{Op: "dial", Err: errors.New("refused")})
	d, f, c := 0, 0, 0
	for j := range b.paths {
		d, f, c = d+b.dials[j], f+b.dfails[j], c+b.cfails[j]
	}
	if d != 2 || f != 2 || c != 1 {
		t.Fatalf("dials %d fails %d connect fails %d; want 2 2 1", d, f, c)
	}
}

// --- evacuation of light users off cut paths ---

// evacLink is a live mtcp link over a pipe with n user streams (the peer
// side accepts and discards).
func evacLink(t *testing.T, n int) (*mtcpLink, []*countedStream) {
	t.Helper()
	c1, c2 := net.Pipe()
	cli, err := smux.Client(c1, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smux.Server(c2, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			s, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, s)
		}
	}()
	t.Cleanup(func() { cli.Close(); srv.Close() })
	l := &mtcpLink{sess: cli, flows: map[*countedStream]struct{}{}}
	var out []*countedStream
	for i := 0; i < n; i++ {
		st, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		cs := &countedStream{Stream: st, link: l}
		cs.ewma = 3 << 10 // a crowd user: a small request every ~0.7 s
		cs.prevBytes = 1  // not idle for reclaimIdle (bytes 0 != 1)
		l.active.Add(1)
		l.flows[cs] = struct{}{}
		out = append(out, cs)
	}
	return l, out
}

func withEvac(t *testing.T) {
	t.Helper()
	old, oldCut := lbEvac, lbCutDrain
	lbEvac, lbCutDrain = true, true
	t.Cleanup(func() { lbEvac, lbCutDrain = old, oldCut })
}

// Heavy users and a stream mid-request are never closed; light ones that
// stay quiet are; at most n.
func TestEvacLightSparesHeavyAndActive(t *testing.T) {
	withEvac(t)
	l, cs := evacLink(t, 7)
	now := time.Now()
	heavy, call, mid, cp := cs[0], cs[1], cs[2], cs[6]
	cp.copyOnly.Store(true)                                             // an mconn second copy: the connection already rides another link too
	heavy.ewma, heavy.backlogEwma, heavy.lastActive = 1<<30, 1<<30, now // a download
	call.ewma = 250 << 10                                               // a 2 Mbit/s call: over evacLightRate
	ml := &managedLink{link: l, cutDrain: true, retiring: true, retireSince: now.Add(-10 * time.Second)}
	m := &LinkManager{}
	ml.reclaiming.Store(true)
	sleeps := 0
	got := m.evacLight(ml, now, 16, func(time.Duration) {
		sleeps++
		if sleeps == 1 {
			mid.moved(100) // the reply of a request in flight arrives during the quiet window
		}
	})
	if got != 3 {
		t.Fatalf("closed %d, want the 3 quiet light streams", got)
	}
	for _, s := range []*countedStream{heavy, call, mid, cp} {
		if s.done.Load() {
			t.Fatal("a heavy / call / mid-request / copy-only stream was closed")
		}
	}
	if ml.reclaiming.Load() {
		t.Fatal("reclaiming not released")
	}
	if m.evacTotal.Load() != 3 {
		t.Fatalf("evacTotal %d", m.evacTotal.Load())
	}
	// the per-call bound
	l2, _ := evacLink(t, 10)
	ml2 := &managedLink{link: l2, cutDrain: true, retiring: true, retireSince: now.Add(-10 * time.Second)}
	if got := m.evacLight(ml2, now, 4, func(time.Duration) {}); got != 4 {
		t.Fatalf("closed %d, want the bound 4", got)
	}
}

func TestEvacDue(t *testing.T) {
	withEvac(t)
	now := time.Now()
	ml := &managedLink{cutDrain: true, retiring: true, retireSince: now.Add(-evacAfter / 2)}
	if evacDue(ml, now) {
		t.Fatal("due before evacAfter")
	}
	ml.retireSince = now.Add(-evacAfter)
	if !evacDue(ml, now) {
		t.Fatal("not due after evacAfter")
	}
	ml.cutDrain = false // an ordinary retiring link (pool shrink): untouched
	if evacDue(ml, now) {
		t.Fatal("ordinary retiring link evacuated")
	}
	lbEvac = false
	ml.cutDrain = true
	if evacDue(ml, now) {
		t.Fatal("key off: evacuated")
	}
}

// drainTick's pool-wide bound: two cut-drained links of 16 light users each,
// evacPerTick 20 -> 20 closed in the tick, not 32; ordinary retiring links
// and serving links are untouched.
func TestEvacPerTickBound(t *testing.T) {
	withEvac(t)
	oldTick, oldLink := evacPerTick, evacPerLink
	evacPerTick, evacPerLink = 20, 12
	oldQuiet := evacQuiet
	evacQuiet = 10 * time.Millisecond
	t.Cleanup(func() { evacPerTick, evacPerLink, evacQuiet = oldTick, oldLink, oldQuiet })
	m, clk, _ := newV2Manager(nil, 1, 64, true)
	var all [][]*countedStream
	add := func(cut, retiring bool) {
		l, cs := evacLink(t, 16)
		all = append(all, cs)
		ml := addManaged(m, l)
		m.mu.Lock()
		ml.open = 16
		ml.retiring, ml.cutDrain = retiring, cut
		if retiring {
			ml.retireSince = m.now()
		}
		m.mu.Unlock()
	}
	add(true, true)
	add(true, true)
	add(false, true)  // a pool shrink's retiring link
	add(false, false) // serving
	clk.Advance(evacAfter + time.Second)
	m.drainTick()
	closed := func(cs []*countedStream) int {
		n := 0
		for _, s := range cs {
			if s.done.Load() {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for closed(all[0])+closed(all[1]) < 20 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := closed(all[0]) + closed(all[1]); n != 20 {
		t.Fatalf("closed %d on cut links in one tick, want evacPerTick 20", n)
	}
	if closed(all[2]) != 0 || closed(all[3]) != 0 {
		t.Fatal("a non-cut link's users were closed")
	}
}
