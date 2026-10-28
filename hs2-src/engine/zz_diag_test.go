package engine

// TEMPORARY diagnostic for TestAngleP10/newheavy (r1-prep); never committed.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestZZDiagNewHeavy(t *testing.T) {
	if s := os.Getenv("ZZ_SEGS"); s != "" {
		n, _ := strconv.Atoi(s)
		defer func(v int) { HQGSegs = v }(HQGSegs)
		HQGSegs = n
	}
	const c = angleRateBps
	l := newAngleLink(t, c, angleDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var h1, h2 atomic.Uint64
	p := l.probe(ctx, 3)
	l.download(ctx, 1, 4, &h1)
	time.Sleep(3 * time.Second)
	p.on()
	t0 := time.Now()
	l.download(ctx, 2, 16, &h2)
	time.Sleep(9 * time.Second)
	p.mu.Lock()
	r := append([]time.Duration(nil), p.rtts...)
	p.mu.Unlock()
	var b strings.Builder
	for i, d := range r {
		fmt.Fprintf(&b, " %d:%.1f", i, float64(d)/1e6)
	}
	_ = t0
	p99, n := p.pct(0.99)
	p50, _ := p.pct(0.5)
	cl := coreLinks.of(l.exit)
	hq := ""
	if cl != nil && cl.hq != nil {
		hq = cl.hq.line(cl.id, 9*time.Second)
	}
	st, _ := l.exit.SchedStats()
	t.Logf("segs %d: probe p50 %.1f p99 %.1f (%d); exit lane p50/p99 %v/%v; %s; most unsent down %d\n samples(ms):%s",
		HQGSegs, float64(p50)/1e6, float64(p99)/1e6, n, st.LaneP50, st.LaneP99, hq, l.down.most(), b.String())
}
