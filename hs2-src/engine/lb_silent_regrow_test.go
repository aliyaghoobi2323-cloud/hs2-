package engine

import (
	"fmt"
	"testing"
)

// TestSilentRegrowRetiresMore (bh101): a path far under its share (back from
// a blackhole) makes a rebalance round retire up to lb2RegrowBatch idle links
// (one per donor path) with HS2_LB_SILENT_REGROW, lb2DrainBatch without it,
// and never without HS2_LB_SILENT.
func TestSilentRegrowRetiresMore(t *testing.T) {
	withAuto2(t)
	run := func(silent, regrow bool) int {
		so, sr := lbSilent, lbSilentRegrow
		lbSilent, lbSilentRegrow = silent, regrow
		t.Cleanup(func() { lbSilent, lbSilentRegrow = so, sr })
		var ps []LBPath
		for i := 0; i < 5; i++ {
			ps = append(ps, LBPath{Local: "10.0.0.1", Remote: fmt.Sprintf("10.1.0.%d", i+1), Addr: fmt.Sprintf("10.1.0.%d:443", i+1), Weight: 20, Primary: i == 0})
		}
		b, _ := NewLB(ps, nil)
		b.SetAuto(true)
		b.mu.Lock()
		b.mult[0] = 0.1
		b.mu.Unlock()
		for i := 0; i < 40; i++ {
			j, _ := b.acquire()
			k := new(int)
			b.result(j, false, k, nil)
			b.attach(k, lbHooks{retire: func() bool { b.release(k); return true }})
		}
		b.mu.Lock()
		if b.links[0] > 3 {
			b.mu.Unlock()
			t.Fatalf("setup: path 0 has %d links", b.links[0])
		}
		b.mult[0] = 1
		b.mu.Unlock()
		return b.rebalance2()
	}
	if n := run(true, true); n != lb2RegrowBatch {
		t.Fatalf("regrow on: retired %d, want %d", n, lb2RegrowBatch)
	}
	if n := run(true, false); n != lb2DrainBatch {
		t.Fatalf("regrow off: retired %d, want %d", n, lb2DrainBatch)
	}
	if n := run(false, true); n != lb2DrainBatch {
		t.Fatalf("silent off: retired %d, want %d", n, lb2DrainBatch)
	}
}
