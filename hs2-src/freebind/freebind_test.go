package freebind

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpecific(t *testing.T) {
	for addr, want := range map[string]bool{
		":443": false, "0.0.0.0:443": false, "[::]:443": false, "": false,
		"1.2.3.4:443": true, "[2001:db8:5::1]:2082": true, "[fe80::1%eth0]:80": true,
		"2001:db8:5::1": true, "example.com:80": false,
	} {
		if got := Specific(addr); got != want {
			t.Errorf("Specific(%q) = %v, want %v", addr, got, want)
		}
	}
}

// captureLogs swaps logf/Usable/pollEvery for the test.
func captureLogs(t *testing.T, usable func(net.IP) bool) func() []string {
	var mu sync.Mutex
	var lines []string
	oldLog, oldU, oldP := logf, Usable, pollEvery
	logf = func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	Usable, pollEvery = usable, 5*time.Millisecond
	t.Cleanup(func() { logf, Usable, pollEvery = oldLog, oldU, oldP })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// A late address logs once (even with several listeners on it) and once more
// when it becomes usable.
func TestWatchLogsOnceThenUp(t *testing.T) {
	var up atomic.Bool
	logs := captureLogs(t, func(net.IP) bool { return up.Load() })
	Watch("[2001:db8::425]:2082")
	Watch("[2001:db8::425]:443") // same IP: no second line
	Watch("0.0.0.0:80")          // wildcard: never watched
	if l := logs(); len(l) != 1 || Pending.Load() != 1 {
		t.Fatalf("logs %q pending %d", l, Pending.Load())
	}
	up.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for Pending.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l := logs(); len(l) != 2 || Pending.Load() != 0 {
		t.Fatalf("logs %q pending %d", l, Pending.Load())
	}
}

// A usable address logs nothing.
func TestWatchUsableSilent(t *testing.T) {
	logs := captureLogs(t, func(net.IP) bool { return true })
	Watch("127.0.0.1:1")
	if l := logs(); len(l) != 0 {
		t.Fatalf("logs %q", l)
	}
}
