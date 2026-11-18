package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// startPprof serves Go's CPU and memory profiles when HS2_PPROF names a
// loopback address (e.g. HS2_PPROF=127.0.0.1:6060), for measuring where a
// busy server spends its CPU:
//
//	curl -o cpu.prof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
//
// Off unless set. Anything but a loopback address is refused (the profiles
// show the process's internals; they are never exposed to the network).
func startPprof() {
	addr := strings.TrimSpace(os.Getenv("HS2_PPROF"))
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Printf("pprof: HS2_PPROF=%q refused — it must be a loopback address such as 127.0.0.1:6060", addr)
		return
	}
	// HS2_PPROF_LOCKS=N also samples lock contention (1 in N mutex events)
	// and blocking (events of N µs or more): /debug/pprof/mutex and /block.
	if n, err := strconv.Atoi(os.Getenv("HS2_PPROF_LOCKS")); err == nil && n > 0 {
		runtime.SetMutexProfileFraction(n)
		runtime.SetBlockProfileRate(n * 1000)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// mem69 (HS2_MEM_OBJS diagnosis): heap dump with object labels, written
	// to a fixed place (/tmp/hs2heap-<pid>.dump and .labels)
	mux.HandleFunc("/debug/hs2heapdump", func(w http.ResponseWriter, _ *http.Request) {
		p := fmt.Sprintf("/tmp/hs2heap-%d", os.Getpid())
		if err := engine.MemHeapDump(p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, p)
	})
	// hc183: half-close counters (halfCloses, halfCloseIdles, halfOpen now)
	mux.HandleFunc("/debug/hs2halfclose", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, engine.HalfCloseStats())
	})
	mux.HandleFunc("/debug/hs2sched", serveSched) // b322: runtime/metrics for storm diag
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("pprof: %v", err)
		return
	}
	log.Printf("pprof: serving profiles on http://%s/debug/pprof/ (HS2_PPROF)", ln.Addr())
	go http.Serve(ln, mux)
}
