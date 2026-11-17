// sc161 load generator: crowd connections ('R' probe protocol) with open latency,
// and connection storms ('E' echo). Prints aggregated lines every -win.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	mu       sync.Mutex
	openLat  []float64
	reqLat   []float64
	openFail int64
	reqFail  int64
	openNow  atomic.Int64
	t0       = time.Now()
)

func q(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -1
	}
	return v[min(len(v)-1, int(float64(len(v))*p))]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func crowdConn(addr string, every, life time.Duration, stopAt time.Time) {
	time.Sleep(time.Duration(rand.Int64N(int64(3 * time.Second))))
	req := make([]byte, 6+200)
	reply := make([]byte, 8192)
	for time.Now().Before(stopAt) {
		s := time.Now()
		c, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			mu.Lock()
			openFail++
			mu.Unlock()
			time.Sleep(time.Second)
			continue
		}
		openNow.Add(1)
		first := true
		ok := true
		if _, err := c.Write([]byte{'R'}); err != nil {
			ok = false
		}
		dieAt := time.Now().Add(time.Duration(float64(life) * (0.5 + rand.Float64())))
		for ok && time.Now().Before(dieAt) && time.Now().Before(stopAt) {
			if !first {
				time.Sleep(time.Duration(float64(every) * (0.5 + rand.Float64())))
			}
			pad := 20 + rand.IntN(181)
			want := 600 + rand.IntN(4096-600+1)
			binary.BigEndian.PutUint16(req[0:2], uint16(pad))
			binary.BigEndian.PutUint32(req[2:6], uint32(want))
			rs := time.Now()
			c.SetDeadline(rs.Add(10 * time.Second))
			if _, err := c.Write(req[:6+pad]); err != nil {
				ok = false
				break
			}
			for got := 0; got < want; {
				k, err := c.Read(reply[:min(len(reply), want-got)])
				got += k
				if err != nil {
					ok = false
					break
				}
			}
			mu.Lock()
			if !ok {
				if first {
					openFail++
				} else {
					reqFail++
				}
			} else if first {
				openLat = append(openLat, ms(time.Since(s)))
			} else {
				reqLat = append(reqLat, ms(time.Since(rs)))
			}
			mu.Unlock()
			first = false
		}
		c.Close()
		openNow.Add(-1)
	}
}

func stormConn(addr string, hold time.Duration) {
	s := time.Now()
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		mu.Lock()
		openFail++
		mu.Unlock()
		return
	}
	defer c.Close()
	openNow.Add(1)
	defer openNow.Add(-1)
	c.SetDeadline(time.Now().Add(15 * time.Second))
	b := make([]byte, 17)
	b[0] = 'E'
	if _, err := c.Write(b); err == nil {
		got := 0
		for got < 17 {
			k, err := c.Read(b[got:])
			got += k
			if err != nil {
				break
			}
		}
		mu.Lock()
		if got == 17 {
			openLat = append(openLat, ms(time.Since(s)))
		} else {
			openFail++
		}
		mu.Unlock()
	} else {
		mu.Lock()
		openFail++
		mu.Unlock()
	}
	time.Sleep(hold)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8443", "")
	mode := flag.String("mode", "crowd", "crowd|storm")
	n := flag.Int("n", 1000, "")
	dur := flag.Duration("t", 360*time.Second, "")
	every := flag.Duration("every", 700*time.Millisecond, "")
	life := flag.Duration("life", 40*time.Second, "")
	burst := flag.Duration("burst", 5*time.Second, "storm: open n within this")
	period := flag.Duration("period", 30*time.Second, "storm: repeat")
	hold := flag.Duration("hold", 10*time.Second, "storm: keep each open")
	win := flag.Duration("win", 10*time.Second, "")
	tag := flag.String("tag", "g", "")
	dropAt := flag.String("dropat", "", "drop: times since start, comma list")
	frac := flag.Float64("frac", 0.5, "drop: fraction closed")
	reburst := flag.Duration("reburst", 2*time.Second, "drop: reconnect spread")
	flag.Parse()
	stopAt := time.Now().Add(*dur)
	go func() {
		for range time.Tick(*win) {
			mu.Lock()
			o, r := openLat, reqLat
			of, rf := openFail, reqFail
			openLat, reqLat, openFail, reqFail = nil, nil, 0, 0
			mu.Unlock()
			sort.Float64s(o)
			sort.Float64s(r)
			fmt.Printf("%s t=%.0f unix=%d open_n=%d open_p50=%.1f open_p99=%.1f open_max=%.1f open_fail=%d req_n=%d req_p50=%.1f req_p99=%.1f req_fail=%d live=%d\n",
				*tag, time.Since(t0).Seconds(), time.Now().Unix(), len(o), q(o, .5), q(o, .99), q(o, 1), of, len(r), q(r, .5), q(r, .99), rf, openNow.Load(), dropped.Load())
			os.Stdout.Sync()
		}
	}()
	var wg sync.WaitGroup
	if *mode == "drop" {
		go dropDriver(*dropAt)
		for i := 0; i < *n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); dropConn(*addr, *every, *frac, *reburst, stopAt) }()
		}
		wg.Wait()
	} else if *mode == "crowd" {
		for i := 0; i < *n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); crowdConn(*addr, *every, *life, stopAt) }()
		}
		wg.Wait()
	} else {
		for time.Now().Before(stopAt) {
			bs := time.Now()
			gap := *burst / time.Duration(*n)
			for i := 0; i < *n; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); stormConn(*addr, *hold) }()
				time.Sleep(gap)
			}
			if d := *period - time.Since(bs); d > 0 {
				time.Sleep(d)
			}
		}
		wg.Wait()
	}
	time.Sleep(*win + time.Second)
}
