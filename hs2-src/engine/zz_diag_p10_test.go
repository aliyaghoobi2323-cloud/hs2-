package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// temporary diagnostic (not committed): the newheavy first second, sampled
func TestZZDiagNewheavy(t *testing.T) {
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
	a0, b0 := h1.Load(), h2.Load()
	l.download(ctx, 2, 16, &h2)
	dOpen := time.Since(t0)
	var sb strings.Builder
	pa, pb := h1.Load(), h2.Load()
	sa, sbb := pa, pb
	for i := 0; i < 15; i++ {
		time.Sleep(100 * time.Millisecond)
		x, y := h1.Load(), h2.Load()
		fmt.Fprintf(&sb, " %d/%d", (x-pa)>>10, (y-pb)>>10)
		pa, pb = x, y
		if i == 9 {
			fmt.Fprintf(&sb, " | first s: h1 %.3f of floor;", float64(x-sa)/(c/2))
		}
	}
	t.Logf("open %v (h1 %d KiB during the opens); KiB per 100 ms h1/h2:%s; h2 from before the opens %d", dOpen, (sa-a0)>>10, sb.String(), b0)
	_ = sbb
}
