package engine

import (
	"io"
	"net"
	"testing"
	"time"
)

// The exit counts its own user connections, the active ones and the
// throughput (it used to report 0 users and 0 Mbit/s while relaying
// thousands of connections), in both directions.
func TestExitCountsItsOwnTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time sampling")
	}
	for _, direct := range []bool{true, false} {
		name := "reverse"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			r := startV2(t, v2Opts{direct: direct, min: 2, max: 4, perLink: 8, pin: 2, target: 2, exitLinks: 2, exitMin: 2, exitMax: 4})
			r.waitFor("links up", 10*time.Second, func() bool { return r.lm.Stats().Links >= 2 && r.exitView().Links >= 2 })
			var ps []*pinger
			for i := 0; i < 3; i++ { // flowing: 4 KiB every 50 ms each
				ps = append(ps, startPinger(r.userAddr, 4096, 50*time.Millisecond))
			}
			idle, err := net.Dial("tcp", r.userAddr) // open but silent
			if err != nil {
				t.Fatal(err)
			}
			defer idle.Close()
			idle.Write([]byte{1})
			io.ReadFull(idle, make([]byte, 1))
			defer func() {
				for _, p := range ps {
					p.close()
				}
			}()
			r.waitFor("the exit counts 4 open, 3 active, real throughput", 20*time.Second, func() bool {
				e := r.exitView()
				return e.Counted && e.Users == 4 && e.Flowing == 3 && e.MbitPerS > 0.5 && e.PeakMbit >= e.MbitPerS
			})
			e, s := r.exitView(), r.lm.Stats()
			t.Logf("%s: exit %d open / %d active / %.1f Mbit/s (peak %.1f); edge %d open / %d active / %.1f Mbit/s",
				name, e.Users, e.Flowing, e.MbitPerS, e.PeakMbit, s.Users, s.Flowing, s.MbitPerS)
		})
	}
}

// peakOf takes the highest two-tick average: one spike is not a peak.
func TestPeakOfSmoothsSpikes(t *testing.T) {
	if p := peakOf([]float64{0, 100, 0}); p != 50 {
		t.Fatalf("spike: %v, want 50", p)
	}
	if p := peakOf([]float64{80, 100, 90}); p != 95 {
		t.Fatalf("plateau: %v, want 95", p)
	}
}

// dgtun reports its own flows and throughput on every side, with the last
// minute's peak, and says so.
func TestDgStatsCountedWithPeak(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 1, 8, 8, func(string, ...any) {})
	for _, g := range []float64{0, 125000, 250000, 0} { // bytes/s per tick
		p.publishStats(apSample{G: g, open: 5, flowing: 2})
	}
	st := p.Stats()
	if !st.Counted || st.Users != 5 || st.Flowing != 2 {
		t.Fatalf("stats %+v", st)
	}
	if st.MbitPerS != 0 || st.PeakMbit < 1.4 || st.PeakMbit > 1.6 { // peak = (1+2)/2 Mbit/s
		t.Fatalf("now %.2f, peak %.2f Mbit/s", st.MbitPerS, st.PeakMbit)
	}
}
