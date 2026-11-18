package engine

import (
	"errors"
	"net"
	"testing"
	"time"
)

// bh100: a path cooled for dials that time out before TCP connects (a
// blackhole, either direction) is probed every lbSilentProbe with
// HS2_LB_SILENT; it stays out while each probe fails and is back on the first
// probe that connects. A TLS-stage failure, or the key off, keeps the usual
// lbProbeEvery.
func TestSilentProbeDarkPathFast(t *testing.T) {
	dark := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}
	tlsFail := errors.New("tlscarrier: reading server proof: EOF")
	for _, c := range []struct {
		name string
		on   bool
		err  error
		want time.Duration
	}{
		{"on dark", true, dark, lbSilentProbe},
		{"off dark", false, dark, lbProbeEvery},
		{"on tls", true, tlsFail, lbProbeEvery},
	} {
		t.Run(c.name, func(t *testing.T) {
			withSilent(t, c.on)
			lb, _ := NewLB(twoPaths(50, 50), nil)
			lb.SetAuto(true)
			clock := time.Unix(1e9, 0)
			lb.now = func() time.Time { return clock }
			for k := 0; k < 4; k++ { // both paths up, the peer reachable
				lb.dialVia(func(p LBPath) (any, error) { return new(int), nil })
			}
			clock = clock.Add(lbStartGrace + time.Second)
			lb.mu.Lock()
			defer lb.mu.Unlock()
			for k := 0; k < lbFailMax; k++ {
				lb.links[1]++
				lb.resultLocked(1, false, nil, c.err)
			}
			if got := lb.until[1].Sub(clock); got != c.want {
				t.Fatalf("cooled for %v, want %v", got, c.want)
			}
			if lb.eligibleLocked(1, clock) {
				t.Fatal("cooled path eligible")
			}
			if !c.on || c.err != dark {
				return
			}
			// dark for 3 more probes: each fails, the path stays out
			for k := 0; k < 3; k++ {
				clock = lb.until[1]
				i, probe := lb.acquireLocked()
				if i != 1 || !probe {
					t.Fatalf("probe %d: acquire = %d,%v, want the cooled path's probe", k, i, probe)
				}
				lb.resultLocked(1, true, nil, dark)
				if lb.eligibleLocked(1, clock) || lb.until[1].Sub(clock) != lbSilentProbe {
					t.Fatalf("probe %d failed but path eligible / cooled %v", k, lb.until[1].Sub(clock))
				}
			}
			// the blackhole clears: the next probe (<= lbSilentProbe later) connects
			clock = lb.until[1]
			i, probe := lb.acquireLocked()
			if i != 1 || !probe {
				t.Fatalf("acquire after clear = %d,%v", i, probe)
			}
			lb.resultLocked(1, true, new(int), nil)
			if !lb.eligibleLocked(1, clock) {
				t.Fatal("path not back after its probe connected")
			}
		})
	}
}

// bh100: a cut path that was cooled (the judge forgot it) and is back from
// its probe gets its share back step by step with HS2_LB_SILENT, though its
// probe-floor links give no reading; without the key it stays at the cut's
// factor (the stuck share seen in the lab).
func TestSilentBackFromCooldownRegainsShare(t *testing.T) {
	for _, on := range []bool{true, false} {
		withAuto2(t)
		withSilent(t, on)
		b, _ := NewLB(twoPaths(50, 50), nil)
		b.SetAuto(true)
		now := time.Unix(1000, 0)
		b.now = func() time.Time { return now }
		step := func(rs map[int]q2Reading) {
			now = now.Add(5 * time.Second)
			b.adapt2(rs)
		}
		for i := 0; i < 3; i++ {
			step(map[int]q2Reading{0: {loss: 0.002}, 1: {loss: 0.08}})
		}
		if b.mult[1] > 0.1+1e-9 {
			t.Fatalf("on=%v: not cut: %.2f", on, b.mult[1])
		}
		b.mu.Lock()
		b.until[1] = now.Add(time.Hour) // cooled: dials time out
		b.mu.Unlock()
		for i := 0; i < 3; i++ {
			step(map[int]q2Reading{0: {loss: 0.002}})
		}
		if b.judge.p[b.paths[1].Name()] != nil {
			t.Fatalf("on=%v: judge kept a cooled path", on)
		}
		if b.mult[1] > 0.1+1e-9 {
			t.Fatalf("on=%v: cooled path factor rose to %.2f", on, b.mult[1])
		}
		b.mu.Lock()
		b.until[1] = time.Time{} // its probe link is up
		b.mu.Unlock()
		prev := lbShares(b.paths, b.mult)[1]
		for i := 0; i < 40; i++ {
			step(map[int]q2Reading{0: {loss: 0.002}}) // the path's own links give no reading
			sh := lbShares(b.paths, b.mult)[1]
			if sh-prev > lbMaxStepPct+1e-6 {
				t.Fatalf("on=%v: share rose %.1f points in one step", on, sh-prev)
			}
			prev = sh
		}
		if on && b.mult[1] < 0.99 {
			t.Fatalf("key on: back path factor %.2f after 200 s, want 1", b.mult[1])
		}
		if !on && b.mult[1] > 0.1+1e-9 {
			t.Fatalf("key off: factor changed to %.2f (want the old behaviour)", b.mult[1])
		}
	}
}

// bh100: with the key on, a cooled path whose probe is due gets a spare dial
// from fillMin even when every live path has its two links (a steady pool
// makes no dial of its own); not while the probe is in flight, not before the
// cooldown ends, never with the key off.
func TestSilentProbeDueAsksSpare(t *testing.T) {
	for _, on := range []bool{true, false} {
		withAuto2(t)
		withSilent(t, on)
		b, _ := NewLB(twoPaths(50, 50), nil)
		b.SetAuto(true)
		now := time.Unix(1000, 0)
		b.now = func() time.Time { return now }
		asked := 0
		b.setSpare(func(n int) int { asked += n; return n })
		for k := 0; k < 6; k++ {
			b.dialVia(func(p LBPath) (any, error) { return new(int), nil })
		}
		b.mu.Lock()
		b.until[1] = now.Add(lbSilentProbe)
		b.mu.Unlock()
		if b.fillMin(); asked != 0 {
			t.Fatalf("on=%v: spare asked before the cooldown ended", on)
		}
		now = now.Add(lbSilentProbe)
		b.fillMin()
		if want := map[bool]int{true: 1, false: 0}[on]; asked != want {
			t.Fatalf("on=%v: spare asked %d, want %d", on, asked, want)
		}
		b.mu.Lock()
		b.probing[1] = true
		b.mu.Unlock()
		asked = 0
		if b.fillMin(); asked != 0 {
			t.Fatalf("on=%v: spare asked while the probe is in flight", on)
		}
	}
}
