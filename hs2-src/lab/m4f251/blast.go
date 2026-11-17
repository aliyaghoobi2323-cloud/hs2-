// blast: m4f251 bench sender. N goroutines send small UDP packets to dst for
// T, then print the total packets/s (the per-packet egress cost shows here).
package main

import (
	"flag"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	dst := flag.String("dst", "10.77.0.2:9", "")
	n := flag.Int("n", 4, "senders")
	d := flag.Duration("t", 10*time.Second, "")
	sz := flag.Int("s", 64, "payload")
	flag.Parse()
	var tot atomic.Uint64
	var wg sync.WaitGroup
	end := time.Now().Add(*d)
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("udp", *dst)
			if err != nil {
				panic(err)
			}
			b := make([]byte, *sz)
			var k uint64
			for time.Now().Before(end) {
				for j := 0; j < 256; j++ {
					if _, err := c.Write(b); err == nil {
						k++
					}
				}
			}
			tot.Add(k)
		}()
	}
	wg.Wait()
	fmt.Printf("pps %.0f\n", float64(tot.Load())/d.Seconds())
}
