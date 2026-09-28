// probe measures what a user feels through the tunnel: bulk throughput and,
// at the same time, the latency of interactive traffic.
//
//	probe -server -listen 127.0.0.1:5201        (plays the panel, kharej side)
//	probe -addr 127.0.0.1:8443 -bulk 8 -t 20s   (user side, through the tunnel)
//
// The client opens -bulk download connections and, on a separate connection,
// sends a 1-byte echo every -every and times the reply. It also opens a fresh
// connection every second and times connect+first-byte (what a page load or a
// new app request feels like). With -udp it also runs a UDP echo probe.
// Output is one JSON line so sweeps can aggregate it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func server(listen string) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if pc, err := net.ListenPacket("udp", listen); err == nil {
		go func() {
			b := make([]byte, 65536)
			for {
				n, a, err := pc.ReadFrom(b)
				if err != nil {
					return
				}
				pc.WriteTo(b[:n], a)
			}
		}()
	}
	blob := make([]byte, 64<<10)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			var m [1]byte
			if _, err := io.ReadFull(c, m[:]); err != nil {
				return
			}
			switch m[0] {
			case 'B': // bulk download
				for {
					if _, err := c.Write(blob); err != nil {
						return
					}
				}
			case 'U': // bulk upload sink
				io.Copy(io.Discard, c)
			case 'E': // echo
				c.Write(m[:])
				io.Copy(c, c)
			}
		}(c)
	}
}

type result struct {
	Mbps       float64 `json:"mbps"`
	UpMbps     float64 `json:"up_mbps,omitempty"`
	EchoN      int     `json:"echo_n"`
	EchoLost   int     `json:"echo_lost"`
	EchoP50    float64 `json:"echo_p50_ms"`
	EchoP95    float64 `json:"echo_p95_ms"`
	EchoP99    float64 `json:"echo_p99_ms"`
	EchoMax    float64 `json:"echo_max_ms"`
	ConnN      int     `json:"conn_n"`
	ConnFail   int     `json:"conn_fail"`
	ConnP50    float64 `json:"conn_p50_ms"`
	ConnP95    float64 `json:"conn_p95_ms"`
	UDPN       int     `json:"udp_n,omitempty"`
	UDPLost    int     `json:"udp_lost,omitempty"`
	UDPP50     float64 `json:"udp_p50_ms,omitempty"`
	UDPP95     float64 `json:"udp_p95_ms,omitempty"`
	BulkErrors int     `json:"bulk_errors"`
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -1
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(p * float64(len(s)-1))
	return s[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func main() {
	srv := flag.Bool("server", false, "run the panel side")
	listen := flag.String("listen", "127.0.0.1:5201", "server listen address")
	addr := flag.String("addr", "127.0.0.1:8443", "client target")
	bulk := flag.Int("bulk", 8, "parallel download connections")
	up := flag.Int("up", 0, "parallel upload connections")
	dur := flag.Duration("t", 20*time.Second, "test duration")
	every := flag.Duration("every", 100*time.Millisecond, "echo interval")
	warm := flag.Duration("warm", 3*time.Second, "warm-up excluded from stats")
	udp := flag.Bool("udp", false, "also run a UDP echo probe")
	flag.Parse()
	if *srv {
		server(*listen)
		return
	}
	var r result
	var down, upb atomic.Int64
	var bulkErr atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()
	measuring := func() bool { return time.Since(start) > *warm }
	for i := 0; i < *bulk+*up; i++ {
		isUp := i >= *bulk
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", *addr, 10*time.Second)
			if err != nil {
				bulkErr.Add(1)
				return
			}
			defer c.Close()
			go func() { <-stop; c.Close() }()
			buf := make([]byte, 64<<10)
			if isUp {
				c.Write([]byte{'U'})
				for {
					n, err := c.Write(buf)
					if measuring() {
						upb.Add(int64(n))
					}
					if err != nil {
						return
					}
				}
			}
			c.Write([]byte{'B'})
			for {
				n, err := c.Read(buf)
				if measuring() {
					down.Add(int64(n))
				}
				if err != nil {
					select {
					case <-stop:
					default:
						bulkErr.Add(1)
					}
					return
				}
			}
		}()
	}
	var mu sync.Mutex
	var echo, conn, udpRTT []float64
	// echo probe on one long-lived connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := net.DialTimeout("tcp", *addr, 10*time.Second)
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte{'E'})
		var b [1]byte
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			s := time.Now()
			c.SetDeadline(s.Add(5 * time.Second))
			if _, err := c.Write(b[:]); err != nil {
				return
			}
			if _, err := io.ReadFull(c, b[:]); err != nil {
				mu.Lock()
				r.EchoLost++
				mu.Unlock()
				return
			}
			if measuring() {
				mu.Lock()
				echo = append(echo, ms(time.Since(s)))
				mu.Unlock()
			}
		}
	}()
	// new-connection probe
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			go func() {
				s := time.Now()
				c, err := net.DialTimeout("tcp", *addr, 5*time.Second)
				if err == nil {
					defer c.Close()
					c.SetDeadline(time.Now().Add(5 * time.Second))
					var b [1]byte
					if _, err = c.Write([]byte{'E'}); err == nil {
						_, err = io.ReadFull(c, b[:])
					}
				}
				if !measuring() {
					return
				}
				mu.Lock()
				if err != nil {
					r.ConnFail++
				} else {
					conn = append(conn, ms(time.Since(s)))
				}
				mu.Unlock()
			}()
		}
	}()
	if *udp {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("udp", *addr)
			if err != nil {
				return
			}
			defer c.Close()
			t := time.NewTicker(*every)
			defer t.Stop()
			b := make([]byte, 64)
			for seq := 0; ; seq++ {
				select {
				case <-stop:
					return
				case <-t.C:
				}
				s := time.Now()
				c.SetDeadline(s.Add(2 * time.Second))
				c.Write([]byte(fmt.Sprintf("%08d", seq)))
				n, err := c.Read(b)
				if !measuring() {
					continue
				}
				mu.Lock()
				if err != nil || n != 8 {
					r.UDPLost++
				} else {
					udpRTT = append(udpRTT, ms(time.Since(s)))
				}
				mu.Unlock()
			}
		}()
	}
	time.Sleep(*dur)
	close(stop)
	elapsed := (time.Since(start) - *warm).Seconds()
	wg.Wait()
	r.Mbps = float64(down.Load()*8) / elapsed / 1e6
	r.UpMbps = float64(upb.Load()*8) / elapsed / 1e6
	r.BulkErrors = int(bulkErr.Load())
	r.EchoN, r.EchoP50, r.EchoP95, r.EchoP99 = len(echo), pct(echo, .5), pct(echo, .95), pct(echo, .99)
	r.EchoMax = pct(echo, 1)
	r.ConnN, r.ConnP50, r.ConnP95 = len(conn), pct(conn, .5), pct(conn, .95)
	if *udp {
		r.UDPN, r.UDPP50, r.UDPP95 = len(udpRTT), pct(udpRTT, .5), pct(udpRTT, .95)
	}
	json.NewEncoder(os.Stdout).Encode(r)
}
