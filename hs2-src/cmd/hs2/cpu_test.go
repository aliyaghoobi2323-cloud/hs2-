package main

import (
	"runtime"
	"testing"
	"time"
)

// The CPU meter reports this process's use between samples (% of one core)
// and says so when the daemon uses nearly all its cores for several samples.
func TestCPUMeter(t *testing.T) {
	var logs []string
	m := &cpuMeter{logf: func(f string, a ...any) { logs = append(logs, f) }}
	if m.sample() != 0 {
		t.Fatal("the first sample has no interval and must read 0")
	}
	busy := func(d time.Duration) {
		done := make(chan struct{})
		for i := 0; i < runtime.NumCPU(); i++ {
			go func() {
				for {
					select {
					case <-done:
						return
					default:
					}
				}
			}()
		}
		time.Sleep(d)
		close(done)
	}
	var hot float64
	for i := 0; i < 4; i++ {
		busy(400 * time.Millisecond)
		hot = m.sample()
	}
	if hot < 50*float64(runtime.NumCPU()) {
		t.Fatalf("all cores busy measured as %.0f%% of one core (%d cores)", hot, runtime.NumCPU())
	}
	if len(logs) == 0 {
		t.Fatalf("a daemon using all its cores for several samples was not reported as the bottleneck (%.0f%%)", hot)
	}
	time.Sleep(400 * time.Millisecond)
	if idle := m.sample(); idle > 30 {
		t.Fatalf("an idle interval measured %.0f%%", idle)
	}
	if len(logs) != 2 {
		t.Fatalf("leaving the bottleneck was not logged once: %v", logs)
	}
}
