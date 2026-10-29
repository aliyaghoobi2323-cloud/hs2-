package engine

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// R1 independent review, part B (engine logic and concurrency; method step
// 6; docs/architecture/wip/r1/review/B-engine.fa.md).

// owdQuietPair runs one QUIET link's one-way delay loops, the edge's end
// and the exit's, over a pipe until ctx ends.
func owdQuietPair(ctx context.Context, wg *sync.WaitGroup) (edge, exit *owdLink) {
	c1, c2 := net.Pipe()
	edge, exit = &owdLink{quiet: true}, &owdLink{quiet: true}
	wg.Add(2)
	go func() { defer wg.Done(); owdLoop(ctx, c1, edge) }()
	go func() { defer wg.Done(); owdLoop(ctx, c2, exit) }()
	return edge, exit
}

// B1: with more than one QUIET link (four past five links), each end's BULK
// links note their traffic on the one QUIET link quietOWD points to at that
// end (the latest whose kindOWD loop started there: owdShared, sendCapLink.
// step), and with OWDIdle (the red team's second pass, R2-10) every other
// QUIET link's loop parks at that end for good. The two ends need not take
// the same link as their latest: the reverse exit's slots bring QUIET links
// up within one handshake of each other, and each end stores its latest as
// its own goroutine gets there. Then each end's latest is parked at the
// other end: no message comes back on it, and neither end's BULK links read
// a QUIET one-way queue again (the budget's qFwd and way back, α's queue)
// until a QUIET link is replaced. With OWDIdle 0 (fin2) the others beat at
// 1 s and both ends had their readings.
func TestReviewBOWDTwoQuietLatest(t *testing.T) {
	run := func(idle int) (got [4]bool) {
		withTune(t, OWDIdle, idle)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		e0, x0 := owdQuietPair(ctx, &wg)
		e1, x1 := owdQuietPair(ctx, &wg)
		_, _ = e0, x1
		// the edge's latest is q1, the exit's q0; each end's BULK links
		// carry a download (1 MB/s) and note it as sendCapLink.step does
		edgeQ, exitQ := e1, x0
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); {
			for _, o := range []*owdLink{edgeQ, exitQ} {
				o.noteRate(1e6)
				o.noteTraffic(10 << 10)
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, got[0] = edgeQ.queue()
		_, got[1] = edgeQ.queueIn()
		_, got[2] = exitQ.queue()
		_, got[3] = exitQ.queueIn()
		cancel()
		wg.Wait()
		return got
	}
	if got := run(0); got != [4]bool{true, true, true, true} {
		t.Fatalf("OWDIdle 0: readings (edge out, in; exit out, in) %v, want all (the case is not shown)", got)
	}
	got := run(1)
	t.Logf("OWDIdle 1, the ends' latest QUIET links differ: readings (edge out, in; exit out, in) %v", got)
	if got != [4]bool{true, true, true, true} {
		t.Fatalf("a busy pool lost its QUIET one-way queues: (edge out, in; exit out, in) %v, want all", got)
	}
}

// B1, the other half: the QUIET link quietOWD points to ends (a DPI reset,
// or QuietFollow closing a retiring link: the newest first, the very one
// quietOWD points to) while other QUIET links live. quietOWD went on
// pointing at the dead link's delays (the red team's first pass, Z12), for
// good when no QUIET link is dialed again (QuietFollow's smaller count).
func TestReviewBOWDQuietHandOver(t *testing.T) {
	withTune(t, OWDIdle, 1)
	old := quietOWD.Load()
	defer quietOWD.Store(old)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	e0, _ := owdQuietPair(ctx, &wg)
	// q1: its own context, so it alone ends
	ctx1, cancel1 := context.WithCancel(ctx)
	e1, _ := owdQuietPair(ctx1, &wg)
	quietOWD.Store(e1)
	time.Sleep(50 * time.Millisecond)
	cancel1()
	deadline := time.Now().Add(2 * time.Second)
	for quietOWD.Load() == e1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if q := quietOWD.Load(); q != e0 {
		t.Fatalf("q1's loop ended with q0 alive: quietOWD %p, want q0's %p (q1's %p)", q, e0, e1)
	}
	cancel()
	wg.Wait()
}
