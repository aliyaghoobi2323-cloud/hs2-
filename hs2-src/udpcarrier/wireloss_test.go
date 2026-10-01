package udpcarrier

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

// The reorder-tolerant wire-loss estimator (carrier.go trackWire/wireLossPPM).
// The path carries a per-datagram wire sequence; the receiver must report the
// GENUINE loss and treat reordering/jitter as delivered, not lost. The old
// estimator (high-water minus receive count per window) booked a reordered
// early arrival as up to 100% phantom loss, which pinned FEC parity near its
// ceiling and tripped the pool governor — the field's "99% loss, 0 kernel
// drops" and the collapse that never reproduced on the clean lab path.

// inOrder returns seqs 0..n-1 in order.
func inOrder(n int) []uint32 {
	a := make([]uint32, n)
	for i := range a {
		a[i] = uint32(i)
	}
	return a
}

// reordered returns 0..n-1 with every everyK-th seq delayed by depth slots
// (bounded reorder, no loss).
func reordered(n, everyK, depth int) []uint32 {
	type a struct{ seq, at int }
	as := make([]a, n)
	for i := 0; i < n; i++ {
		at := i
		if everyK > 0 && i%everyK == 0 {
			at += depth
		}
		as[i] = a{i, at}
	}
	sort.SliceStable(as, func(i, j int) bool {
		if as[i].at != as[j].at {
			return as[i].at < as[j].at
		}
		return as[i].seq < as[j].seq
	})
	out := make([]uint32, n)
	for i, x := range as {
		out[i] = uint32(x.seq)
	}
	return out
}

// jittered returns 0..n-1 reordered by a random bounded forward delay in
// [0,depth], no loss — closer to a real jittery path than a fixed pattern.
func jittered(n, depth int, seed uint64) []uint32 {
	r := rand.New(rand.NewPCG(seed, seed*2+1))
	type a struct {
		seq int
		at  float64
	}
	as := make([]a, n)
	for i := 0; i < n; i++ {
		as[i] = a{i, float64(i) + r.Float64()*float64(depth)}
	}
	sort.SliceStable(as, func(i, j int) bool { return as[i].at < as[j].at })
	out := make([]uint32, n)
	for i, x := range as {
		out[i] = uint32(x.seq)
	}
	return out
}

// feed drives trackWire with an arrival stream and returns the resolved loss
// fraction from the cumulative counters.
func feed(arrivals []uint32) (recv, lost uint64, frac float64) {
	c := &Conn{}
	for _, s := range arrivals {
		c.trackWire(s)
	}
	recv, lost = c.wireRecv.Load(), c.wireLost.Load()
	if recv+lost > 0 {
		frac = float64(lost) / float64(recv+lost)
	}
	return
}

// A clean in-order path reports exactly zero loss (no clean-path regression).
func TestWireLossInOrderIsZero(t *testing.T) {
	recv, lost, frac := feed(inOrder(5000))
	if lost != 0 || frac != 0 {
		t.Fatalf("in-order path: lost=%d frac=%.4f, want 0", lost, frac)
	}
	if recv != 5000 {
		t.Fatalf("in-order path received %d, want 5000", recv)
	}
}

// Reordering with no real loss reports ~zero — the whole point of the fix. The
// old estimator reported several percent here (and far more on a real path).
func TestWireLossReorderIsNotLoss(t *testing.T) {
	// Depths stay within the confirmation window (what the estimator tolerates),
	// so they must read as zero loss however the window is tuned.
	cases := []struct {
		name     string
		arrivals []uint32
	}{
		{"every-3rd-by-9", reordered(5000, 3, 9)},
		{"every-2nd-half-window", reordered(5000, 2, wireConfirmWin/2)},
		{"jitter-quarter-window", jittered(5000, wireConfirmWin/4, 1)},
		{"jitter-near-window", jittered(5000, wireConfirmWin*3/4, 7)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, lost, frac := feed(tc.arrivals)
			// Reorder within the window must never be counted lost.
			if lost != 0 {
				t.Fatalf("%s: reorder booked %d lost (frac %.4f), want 0", tc.name, lost, frac)
			}
		})
	}
}

// Genuine loss is still reported, close to the true rate, with and without
// reordering layered on top.
func TestWireLossReportsRealLoss(t *testing.T) {
	// Drop every D-th emitted seq (in order otherwise): true loss = 1/D.
	for _, D := range []int{50, 20, 10} {
		arr := make([]uint32, 0, 6000)
		for i := 0; i < 6000; i++ {
			if i%D == 0 {
				continue // dropped: never arrives
			}
			arr = append(arr, uint32(i))
		}
		_, _, frac := feed(arr)
		want := 1.0 / float64(D)
		if math.Abs(frac-want) > want*0.1 {
			t.Fatalf("drop 1/%d: reported %.4f, want ~%.4f", D, frac, want)
		}
	}

	// Real loss AND reordering together: still ~the true loss, no phantom.
	r := rand.New(rand.NewPCG(99, 100))
	type a struct {
		seq int
		at  float64
	}
	var as []a
	dropped := 0
	for i := 0; i < 6000; i++ {
		if i%20 == 0 {
			dropped++
			continue
		}
		as = append(as, a{i, float64(i) + r.Float64()*48}) // jitter depth 48
	}
	sort.SliceStable(as, func(i, j int) bool { return as[i].at < as[j].at })
	arr := make([]uint32, len(as))
	for i, x := range as {
		arr[i] = uint32(x.seq)
	}
	_, _, frac := feed(arr)
	if math.Abs(frac-0.05) > 0.01 {
		t.Fatalf("5%% loss with reorder: reported %.4f, want ~0.05", frac)
	}
}

// Reordering DEEPER than the confirmation window reads as bounded loss (one loss
// plus one late duplicate that is ignored), never a runaway — the pathological
// multipath case degrades gracefully rather than catastrophically.
func TestWireLossDeepReorderIsBounded(t *testing.T) {
	// Delay every 10th seq by 4x the window: each is booked lost when the window
	// passes it, then ignored when it finally arrives. ~1/10 reads as loss.
	arr := reordered(5000, 10, wireConfirmWin*4)
	_, _, frac := feed(arr)
	if frac > 0.12 {
		t.Fatalf("deep reorder (1/10 beyond the window) read as %.3f loss, want <= ~0.10", frac)
	}
}

// The feedback loop's per-window PPM (what the encoder/governor actually read)
// stays near zero on a reordering, loss-free path and tracks real loss.
func TestWireLossPPMWindowed(t *testing.T) {
	windowed := func(arrivals []uint32, win int) (maxFrac, meanFrac float64) {
		c := &Conn{}
		var lostW, nW float64
		for i, s := range arrivals {
			c.trackWire(s)
			if (i+1)%win == 0 {
				f := float64(c.wireLossPPM()) / 1e6
				if f > maxFrac {
					maxFrac = f
				}
				lostW += f
				nW++
			}
		}
		if nW > 0 {
			meanFrac = lostW / nW
		}
		return
	}
	// Reorder within the window, no loss: every window reports ~0.
	if mx, mn := windowed(jittered(8000, wireConfirmWin/2, 3), 64); mx > 0.01 || mn > 0.002 {
		t.Fatalf("reorder path windowed loss too high: max=%.3f mean=%.3f", mx, mn)
	}
	// Real 5% loss (in order): the windowed mean is near 5%.
	arr := make([]uint32, 0, 8000)
	for i := 0; i < 8000; i++ {
		if i%20 == 0 {
			continue
		}
		arr = append(arr, uint32(i))
	}
	if _, mn := windowed(arr, 64); math.Abs(mn-0.05) > 0.015 {
		t.Fatalf("real 5%% loss windowed mean=%.3f, want ~0.05", mn)
	}
}
