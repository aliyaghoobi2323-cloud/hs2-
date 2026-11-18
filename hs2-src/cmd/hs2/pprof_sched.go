package main

import (
	"fmt"
	"net/http"
	"runtime/metrics"
)

// b322 (st322 storm diag): /debug/hs2sched prints a few runtime/metrics
// counters as "name value" lines, cheap enough to poll at 10 Hz (no STW,
// unlike goroutine?debug=1): goroutines created/live/runnable, heap
// allocations, cumulative CPU, and the scheduler-latency histogram as
// "lat <upper-bound-ns> <count>" rows (cumulative since start; diff two
// polls for a window).
var schedNames = []string{
	"/sched/goroutines-created:goroutines",
	"/sched/goroutines:goroutines",
	"/sched/goroutines/runnable:goroutines",
	"/sched/goroutines/running:goroutines",
	"/gc/heap/allocs:objects",
	"/gc/heap/allocs:bytes",
	"/cpu/classes/total:cpu-seconds",
	"/cpu/classes/user:cpu-seconds",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/latencies:seconds",
}

func serveSched(w http.ResponseWriter, _ *http.Request) {
	s := make([]metrics.Sample, len(schedNames))
	for i, n := range schedNames {
		s[i].Name = n
	}
	metrics.Read(s)
	for _, x := range s {
		switch x.Value.Kind() {
		case metrics.KindUint64:
			fmt.Fprintf(w, "%s %d\n", x.Name, x.Value.Uint64())
		case metrics.KindFloat64:
			fmt.Fprintf(w, "%s %.6f\n", x.Name, x.Value.Float64())
		case metrics.KindFloat64Histogram:
			h := x.Value.Float64Histogram()
			for i, c := range h.Counts {
				if c != 0 {
					fmt.Fprintf(w, "lat %.0f %d\n", h.Buckets[i+1]*1e9, c)
				}
			}
		}
	}
}
