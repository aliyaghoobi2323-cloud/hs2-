//go:build linux

package engine

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// wnIdleLinks is n idle exit link sockets on the loopback, each registered
// for the pacing net (no core, as the exit's sessions run) with its link
// meter, and a count of the TCP_INFO reads the process makes from now on.
func wnIdleLinks(tb testing.TB, n int) ([]*wcLink, *atomic.Int64) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer ln.Close()
	cfg := wcTestConfig()
	cfg.WindowPool = nil
	links := make([]*wcLink, 0, n)
	for range n {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			tb.Fatal(err)
		}
		s, err := ln.Accept()
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { c.Close(); s.Close() })
		l := newWcLink(nil, cfg, s.(*net.TCPConn), &linkMeter{}, true, nil)
		if l == nil || !l.pnet {
			tb.Fatal("an exit's link socket not registered for the pacing net")
		}
		links = append(links, l)
	}
	calls := &atomic.Int64{}
	old := tcpInfoRead.Load()
	f := func(fd int) (*unix.TCPInfo, int, error) {
		calls.Add(1)
		return (*old)(fd)
	}
	tcpInfoRead.Store(&f)
	tb.Cleanup(func() { tcpInfoRead.Store(old) })
	return links, calls
}

// The exit's idle links (the independent judge of step 1, row پ۵): 300 idle
// link sockets woken every 100 ms for 10 s are read once a second each once
// their first idle second is read (before: on every wake, 3000 TCP_INFO a
// second).
func TestPaceNetIdleLinksReads(t *testing.T) {
	links, calls := wnIdleLinks(t, 300)
	now := time.Now()
	wake := func(d time.Duration) int64 {
		c0 := calls.Load()
		for t := time.Duration(0); t < d; t += wcRampTick {
			now = now.Add(wcRampTick)
			for _, l := range links {
				l.wake(now)
			}
		}
		return calls.Load() - c0
	}
	first := wake(2 * time.Second)
	rest := wake(8 * time.Second)
	perSec := float64(rest) / 8
	t.Logf("300 idle links: %d TCP_INFO reads in the first 2 s, then %.0f a second (every wake: 3000)", first, perSec)
	if perSec > 1.1*300 {
		t.Errorf("300 idle links: %.0f TCP_INFO reads a second, want about 300", perSec)
	}
}

// BenchmarkPaceNetIdleLinks: one wake of the runner's pacing net over 300
// idle exit link sockets (ns/op), and the TCP_INFO reads a second it makes
// at a wake every 100 ms (tcpinfo/s).
func BenchmarkPaceNetIdleLinks(b *testing.B) {
	links, calls := wnIdleLinks(b, 300)
	now := time.Now()
	for range 20 { // the first idle span: read on every wake
		now = now.Add(wcRampTick)
		for _, l := range links {
			l.wake(now)
		}
	}
	c0 := calls.Load()
	b.ResetTimer()
	for range b.N {
		now = now.Add(wcRampTick)
		for _, l := range links {
			l.wake(now)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(calls.Load()-c0)/(float64(b.N)*wcRampTick.Seconds()), "tcpinfo/s")
}
