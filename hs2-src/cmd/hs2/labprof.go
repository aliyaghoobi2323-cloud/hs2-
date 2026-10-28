package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"
)

// labProfile (HS2_PROFILE=DIR, lab only; the R1 round-3 judge's diagnosis
// of a link held at a tenth of the path): the process records blocking and
// lock contention from its start, a CPU profile of the HS2_PROFILE_SECS (5)
// seconds before HS2_PROFILE_AT (20) seconds after it, then writes its CPU,
// block, mutex and goroutine profiles to DIR/<pid>-<kind>.pprof. Off (unset)
// nothing changes.
func labProfile() {
	dir := os.Getenv("HS2_PROFILE")
	if dir == "" {
		return
	}
	at, secs := 20, 5
	if v, err := strconv.Atoi(os.Getenv("HS2_PROFILE_AT")); err == nil && v > 0 {
		at = v
	}
	if v, err := strconv.Atoi(os.Getenv("HS2_PROFILE_SECS")); err == nil && v > 0 && v < at {
		secs = v
	}
	runtime.SetBlockProfileRate(100000) // a sample per 100 µs blocked
	runtime.SetMutexProfileFraction(10)
	name := func(kind string) string { return filepath.Join(dir, fmt.Sprintf("%d-%s.pprof", os.Getpid(), kind)) }
	log.Printf("lab profile: %s, CPU from %ds to %ds", dir, at-secs, at)
	go func() {
		time.Sleep(time.Duration(at-secs) * time.Second)
		f, err := os.Create(name("cpu"))
		if err != nil {
			return
		}
		if pprof.StartCPUProfile(f) == nil {
			time.Sleep(time.Duration(secs) * time.Second)
			pprof.StopCPUProfile()
		}
		f.Close()
		for _, k := range []string{"block", "mutex", "goroutine"} {
			if g, err := os.Create(name(k)); err == nil {
				pprof.Lookup(k).WriteTo(g, 0)
				g.Close()
			}
		}
	}()
}
