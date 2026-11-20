package main

import (
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"
)

func TestZDUDrainMemLimit(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	debug.SetMemoryLimit(1 << 62)
	lim, use := zduDrainMemLimit()
	if want := use + max(use/4, 64<<20); lim != want || debug.SetMemoryLimit(-1) != want {
		t.Fatalf("limit %d (in force %d), want %d (use %d)", lim, debug.SetMemoryLimit(-1), want, use)
	}
	// a lower limit already set is kept
	debug.SetMemoryLimit(32 << 20)
	if lim, _ := zduDrainMemLimit(); lim != 32<<20 || debug.SetMemoryLimit(-1) != 32<<20 {
		t.Fatalf("lower limit replaced: %d", lim)
	}
}

// Review F4: with most runtime memory outside the heap (goroutine stacks,
// as in a drain with thousands of user goroutines) the drain limit must
// stay above the memory in use, or the GC thrashes at its CPU cap.
func TestZDUDrainMemLimitAboveUse(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	debug.SetMemoryLimit(1 << 62)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4000; i++ { // ~4000 x 32 KiB stacks = 125 MiB outside the heap
		wg.Add(1)
		go func() {
			defer wg.Done()
			var pad [24 << 10]byte
			pad[0] = 1
			<-stop
			_ = pad
		}()
	}
	time.Sleep(200 * time.Millisecond)
	defer func() { close(stop); wg.Wait() }()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	inUse := int64(ms.Sys - ms.HeapReleased)
	lim, _ := zduDrainMemLimit()
	if lim < inUse {
		t.Fatalf("drain GOMEMLIMIT %d MiB below the runtime memory in use %d MiB (heap %d MiB, stacks %d MiB)",
			lim>>20, inUse>>20, ms.HeapInuse>>20, ms.StackInuse>>20)
	}
	if lim < inUse+(64<<20)*9/10 {
		t.Fatalf("drain GOMEMLIMIT %d MiB leaves no margin over use %d MiB", lim>>20, inUse>>20)
	}
}
