package engine

import (
	"os"
	"sort"
	"strconv"
)

// lbv81: HS2_LB_SELFQ=K (default 0 = off, production unchanged). The judge's
// queue reading is the median rtt-minrtt of a path's busy links. When one of
// our own heavy downloads lands on one path (the stripes spread a few seconds
// later), that path carries 30-40x the bytes of the others and its own BBR
// startup fills the path queue: the lbv81 soak read 37-45 ms on the clean
// path for ~10 s (ss ground truth: 338-424 MB/5 s there vs 11 MB elsewhere)
// and the judge lowered the clean path to ~0.5 for 20 s (both labs, every
// heavy download that started on a single path). With K > 0 a path whose
// acked bytes this tick exceed K x the median path's (and 8 MiB) is
// self-loaded: its queue reading may lower the smoothed queue but not raise
// it. Loss and dial failures are judged as before; an outside queue (a path
// slower for everyone) carries fewer of our bytes, not more, so it is still
// seen.
var lbSelfQ = func() float64 {
	v, err := strconv.ParseFloat(os.Getenv("HS2_LB_SELFQ"), 64)
	if err != nil || v < 2 || v > 100 {
		return 0
	}
	return v
}()

const selfQMinBytes = 8 << 20

// selfLoaded marks the paths carrying an outsized share of this tick's acked
// bytes (nil when HS2_LB_SELFQ is off or fewer than 3 paths carried bytes).
func selfLoaded(acked []uint64) []bool {
	if lbSelfQ <= 0 {
		return nil
	}
	var v []uint64
	for _, a := range acked {
		if a > 0 {
			v = append(v, a)
		}
	}
	if len(v) < 3 {
		return nil
	}
	sort.Slice(v, func(i, k int) bool { return v[i] < v[k] })
	med := float64(v[len(v)/2])
	out := make([]bool, len(acked))
	for j, a := range acked {
		out[j] = a >= selfQMinBytes && float64(a) > lbSelfQ*med
	}
	return out
}
