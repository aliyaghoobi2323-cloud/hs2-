package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func noJitter(d time.Duration) time.Duration { return d }

// refusalErr is the i-th refusal a fake dial returns: v2 and level, in turn,
// wrapped as a real dial would.
func refusalErr(i int) error {
	if i%2 == 0 {
		return tlscarrier.ErrPeerV2
	}
	return fmt.Errorf("%w (this server speaks levels 1-1, the other server none of them): upgrade the older server", tlscarrier.ErrLevel)
}

// gaps is the time between successive instants.
func gaps(at []time.Time) []time.Duration {
	var out []time.Duration
	for i := 1; i < len(at); i++ {
		out = append(out, at[i].Sub(at[i-1]))
	}
	return out
}

// timedLog records log lines with the fake time they were written at.
type timedLog struct {
	mu    sync.Mutex
	clk   *v2Clock
	at    []time.Time
	lines []string
}

func (l *timedLog) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at = append(l.at, l.clk.Now())
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

// refusalLines returns when each refusal line was logged, failing on any
// "dials failing" line (a refusal is not a dead path).
func (l *timedLog) refusalLines(t *testing.T) []time.Time {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var at []time.Time
	for i, s := range l.lines {
		if strings.Contains(s, "dials failing") {
			t.Errorf("a refusal logged as a failing path: %q", s)
		}
		if strings.Contains(s, "dialing it again in") {
			at = append(at, l.at[i])
		}
	}
	return at
}

// The wait doubles from 8 s to 60 s, a refusal while the dials wait changes
// nothing, a link resets it, and the line comes at most once a minute.
func TestRefusalBackoff(t *testing.T) {
	b := refusalBackoff{jitter: noJitter}
	now := time.Unix(1000, 0)
	var logged []time.Time
	refuse := func() time.Duration {
		w, logIt := b.refused(now)
		if logIt {
			logged = append(logged, now)
		}
		return w
	}
	for i, want := range []time.Duration{8, 16, 32, 60, 60, 60} {
		if w := refuse(); w != want*time.Second {
			t.Fatalf("refusal %d: wait %s, want %ds", i+1, w, want)
		}
		if w := refuse(); w != want*time.Second { // a dial under way meanwhile
			t.Fatalf("refusal %d again: wait %s, want the same %ds", i+1, w, want)
		}
		if h := b.holding(now.Add(time.Second)); h != want*time.Second-time.Second {
			t.Fatalf("holding %s", h)
		}
		now = now.Add(want * time.Second)
		if b.holding(now) != 0 {
			t.Fatal("still holding when the wait is over")
		}
	}
	b.ok()
	if w := refuse(); w != 8*time.Second {
		t.Fatalf("after a link: wait %s, want 8s", w)
	}
	for i := 1; i < len(logged); i++ {
		if logged[i].Sub(logged[i-1]) < time.Minute {
			t.Fatalf("logged %s apart", logged[i].Sub(logged[i-1]))
		}
	}
	if len(logged) < 3 {
		t.Fatalf("logged %d times in %s of refusals", len(logged), now.Sub(time.Unix(1000, 0)))
	}
	for i := 0; i < 1000; i++ {
		if j := jitterTenth(8 * time.Second); j < 7200*time.Millisecond || j > 8800*time.Millisecond {
			t.Fatalf("jitter %s outside 8s ±10%%", j)
		}
	}
	if !tlscarrier.PeerRefused(refusalErr(0)) || !tlscarrier.PeerRefused(refusalErr(1)) || !tlscarrier.PeerRefused(tlscarrier.ErrOldServer) ||
		tlscarrier.PeerRefused(context.DeadlineExceeded) || tlscarrier.PeerRefused(tlscarrier.ErrServerProof) {
		t.Fatal("PeerRefused tells the wrong errors")
	}
}

// refusingDialer refuses every dial but the ones whose index is in good.
type refusingDialer struct {
	mu   sync.Mutex
	clk  *v2Clock
	at   []time.Time
	good map[int]bool
	last *meteredFakeLink
}

func (d *refusingDialer) DialLink(context.Context) (Link, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := len(d.at)
	d.at = append(d.at, d.clk.Now())
	if d.good[i] {
		d.last = newMeteredFake()
		return d.last, nil
	}
	return nil, refusalErr(i)
}

func (d *refusingDialer) dials() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.at...)
}

func wantGaps(t *testing.T, who string, at []time.Time, want ...time.Duration) {
	t.Helper()
	g := gaps(at)
	if len(g) < len(want) {
		t.Fatalf("%s: dials %s apart, want %s", who, g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("%s: dials %s apart, want %s", who, g, want)
		}
	}
}

// Direct: the edge's pool dials an exit that refuses it 8, 16, 32, 60, 60 s
// apart, starts over at 8 s after a link, and says so once a minute.
func TestEdgeDialBackoffOnRefusal(t *testing.T) {
	m, clk, _ := newV2Manager(nil, 1, 1, false)
	d := &refusingDialer{clk: clk, good: map[int]bool{6: true}}
	m.dialer = d
	m.refuse.jitter = noJitter
	lg := &timedLog{clk: clk}
	m.log = lg.logf
	m.target.Store(1)
	ctx := context.Background()
	step := func() {
		m.reap()
		m.reconcile(ctx, 1)
		m.dialWG.Wait()
		clk.Advance(time.Second)
	}
	for i := 0; i < 400 && len(d.dials()) < 7; i++ {
		step()
	}
	at := d.dials()
	wantGaps(t, "refused", at, 8*time.Second, 16*time.Second, 32*time.Second, 60*time.Second, 60*time.Second, 60*time.Second)
	for i := 0; i < 5; i++ { // the link serves: no dial
		step()
	}
	if n := len(d.dials()); n != 7 {
		t.Fatalf("%d dials with the link up, want 7", n)
	}
	d.last.alive.Store(false) // it dies; the exit refuses again
	for i := 0; i < 100 && len(d.dials()) < 9; i++ {
		step()
	}
	wantGaps(t, "after a link", d.dials()[7:], 8*time.Second)
	logged := lg.refusalLines(t)
	if len(logged) < 3 {
		t.Fatalf("refusal logged %d times", len(logged))
	}
	for i := 1; i < len(logged); i++ {
		if logged[i].Sub(logged[i-1]) < time.Minute {
			t.Fatalf("refusal lines %s apart", logged[i].Sub(logged[i-1]))
		}
	}
}

type nopDialed struct{}

func (nopDialed) Close() error { return nil }

// Reverse: the exit's slots dial an edge that refuses them 8, 16, 32, 60, 60 s
// apart, start over at 8 s after a link, and say so once a minute.
func TestExitDialBackoffOnRefusal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := newV2Clock()
	lg := &timedLog{clk: clk}
	d := &refusingDialer{clk: clk, good: map[int]bool{6: true}}
	p := newExitPool(ctx, 1, 1, func() (dialedLink, error) {
		l, err := d.DialLink(ctx)
		if err != nil {
			return nil, err
		}
		l.Close()
		return nopDialed{}, nil
	}, lg.logf)
	p.gate = instantGate()
	p.clock = clk.Now
	p.refuse.jitter = noJitter
	var smu sync.Mutex // one slot sleeps at a time; the clock moves with it
	p.sleep = func(ctx context.Context, d time.Duration) bool {
		smu.Lock()
		clk.Advance(d)
		smu.Unlock()
		return ctx.Err() == nil
	}
	p.serve = func(context.Context, dialedLink) string { return "read: " + reasonPeerClosed } // the link ends at once
	p.setTarget(1)
	deadline := time.Now().Add(10 * time.Second)
	for len(d.dials()) < 9 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	at := d.dials()
	if len(at) < 9 {
		t.Fatalf("%d dials", len(at))
	}
	wantGaps(t, "refused", at[:7], 8*time.Second, 16*time.Second, 32*time.Second, 60*time.Second, 60*time.Second, 60*time.Second)
	wantGaps(t, "after a link", at[7:], 8*time.Second)
	logged := lg.refusalLines(t)
	if len(logged) < 3 {
		t.Fatalf("refusal logged %d times", len(logged))
	}
	for i := 1; i < len(logged); i++ {
		if logged[i].Sub(logged[i-1]) < time.Minute {
			t.Fatalf("refusal lines %s apart", logged[i].Sub(logged[i-1]))
		}
	}
}
