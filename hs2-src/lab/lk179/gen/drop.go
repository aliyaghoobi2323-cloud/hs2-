package main

// lk179 drop mode: N long-lived crowd conns; at each -dropat (since start) a
// fraction -frac of the live conns close at once and reconnect within
// -reburst (the hourly kharej xray restart: 50-70% of conns drop, ~all
// reconnect within seconds). Drop-closes are not failures; the re-opens are
// timed as opens.

import (
	"encoding/binary"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

var dropCh atomic.Pointer[chan struct{}]
var dropped atomic.Int64

func init() { c := make(chan struct{}); dropCh.Store(&c) }

func dropDriver(at string) {
	for _, s := range strings.Split(at, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		if w := d - time.Since(t0); w > 0 {
			time.Sleep(w)
		}
		nc := make(chan struct{})
		old := dropCh.Swap(&nc)
		close(*old)
	}
}

func dropConn(addr string, every time.Duration, frac float64, reburst time.Duration, stopAt time.Time) {
	time.Sleep(time.Duration(rand.Int64N(int64(5 * time.Second))))
	req := make([]byte, 6+200)
	reply := make([]byte, 8192)
	for time.Now().Before(stopAt) {
		ch := *dropCh.Load()
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
		var killed atomic.Bool
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-ch:
					if rand.Float64() < frac {
						killed.Store(true)
						c.Close()
						return
					}
					ch = *dropCh.Load()
				case <-done:
					return
				}
			}
		}()
		first, ok := true, true
		if _, err := c.Write([]byte{'R'}); err != nil {
			ok = false
		}
		for ok && time.Now().Before(stopAt) {
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
				if !killed.Load() {
					if first {
						openFail++
					} else {
						reqFail++
					}
				}
			} else if first {
				openLat = append(openLat, ms(time.Since(s)))
			} else {
				reqLat = append(reqLat, ms(time.Since(rs)))
			}
			mu.Unlock()
			first = false
		}
		close(done)
		c.Close()
		openNow.Add(-1)
		if killed.Load() {
			dropped.Add(1)
			time.Sleep(time.Duration(rand.Int64N(int64(reburst))))
		}
	}
}
