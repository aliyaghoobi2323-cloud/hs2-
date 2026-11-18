//go:build linux

package engine

import (
	"net"
	"testing"
	"time"
)

// BenchmarkUL268Poll400: one ul268 watch tick at 4x scale (400 links): a
// TCP_INFO read per link (tcpStats, one getsockopt each) plus the dark-set
// pass. Loopback TCP pairs with a little unacked data stand in for links.
func BenchmarkUL268Poll400(b *testing.B) {
	const n = 400
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, n)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			acc <- c
		}
	}()
	tcs := make([]*net.TCPConn, n)
	for i := range tcs {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		s := <-acc
		defer s.Close()
		tcs[i] = c.(*net.TCPConn)
		c.Write(make([]byte, 1000))
	}
	smp := make([]ul268Sample, n)
	for i := range smp {
		smp[i] = ul268Sample{key: i, path: []string{"a", "b", "c", "d"}[i%4], st: &ul268Link{}}
	}
	t0 := time.Now().UnixNano()
	b.ReportAllocs()
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		for i := range smp {
			smp[i].ts, smp[i].ok = tcpStats(tcs[i])
		}
		ul268DarkSet(smp, t0+int64(it)*int64(250*time.Millisecond))
	}
	b.ReportMetric(float64(n)*4, "getsockopt/s@250ms")
}
