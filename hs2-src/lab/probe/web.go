package main

// web: page loads like a user browsing. Every ~-webevery (uniform 0.8x-1.2x,
// 2-3 s by default) a "page" opens -webconns short connections at once, each
// fetching a uniform -webmin..-webmax byte reply (the crowd 'R' request, then
// close). A page's time is from its start until its last fetch is done,
// connects included. Pages run independently, so a slow one does not delay
// the next.

import (
	"context"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// webResult is the JSON summary of the web load.
type webResult struct {
	Pages      int     `json:"pages"`      // pages started after warm-up
	N          int     `json:"n"`          // of them, fully loaded
	Fail       int     `json:"fail"`       // a fetch failed (dial, read, 30 s timeout)
	Unfinished int     `json:"unfinished"` // still loading 3 s after the end
	P50        float64 `json:"p50_ms"`     // page load time
	P95        float64 `json:"p95_ms"`     //
	P99        float64 `json:"p99_ms"`     //
	Max        float64 `json:"max_ms"`     //
	KBAvg      float64 `json:"kb_avg"`     // mean page size, KB
	Conns      int     `json:"conns"`      // connections per page
}

type webSpec struct {
	addr       string
	delay      time.Duration // first page this long after the probe start
	warm       time.Duration // pages started before start+warm are not counted
	start      time.Time
	every      time.Duration
	conns      int
	minB, maxB int
}

// webFetch: one short connection that asks for n bytes and closes.
func webFetch(ctx context.Context, addr string, n int) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	pad := 200 + rand.IntN(400) // a request with headers
	req := make([]byte, 7+pad)
	req[0] = 'R'
	binary.BigEndian.PutUint16(req[1:3], uint16(pad))
	binary.BigEndian.PutUint32(req[3:7], uint32(n))
	if _, err := c.Write(req); err != nil {
		return err
	}
	_, err = io.CopyN(io.Discard, c, int64(n))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func runWeb(sp webSpec, stop <-chan struct{}) *webResult {
	res := &webResult{Conns: sp.conns}
	// in-flight pages get 3 s after stop, then are cut off
	cutCtx, cut := context.WithCancel(context.Background())
	defer cut()
	go func() {
		<-stop
		time.Sleep(3 * time.Second)
		cut()
	}()
	select {
	case <-stop:
		return res
	case <-time.After(sp.delay):
	}
	var mu sync.Mutex
	var lat []float64
	var size int64
	var wg sync.WaitGroup
	page := func() {
		defer wg.Done()
		s := time.Now()
		counted := s.Sub(sp.start) >= sp.warm
		ctx, cancel := context.WithTimeout(cutCtx, 30*time.Second)
		defer cancel()
		sizes := make([]int, sp.conns)
		total := 0
		for i := range sizes {
			sizes[i] = sp.minB + rand.IntN(sp.maxB-sp.minB+1)
			total += sizes[i]
		}
		errs := make(chan error, sp.conns)
		for _, n := range sizes {
			go func() { errs <- webFetch(ctx, sp.addr, n) }()
		}
		var err error
		for range sizes {
			if e := <-errs; e != nil && err == nil {
				err = e
				cancel() // the page failed; stop its other fetches
			}
		}
		el := ms(time.Since(s))
		if !counted {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		res.Pages++
		switch {
		case err == nil:
			lat = append(lat, el)
			size += int64(total)
		case cutCtx.Err() != nil:
			res.Unfinished++
		default:
			res.Fail++
		}
	}
	for {
		wg.Add(1)
		go page()
		select {
		case <-stop:
		case <-time.After(time.Duration(float64(sp.every) * (0.8 + 0.4*rand.Float64()))):
			continue
		}
		break
	}
	wg.Wait()
	res.N = len(lat)
	res.P50, res.P95, res.P99, res.Max = pct(lat, .5), pct(lat, .95), pct(lat, .99), pct(lat, 1)
	if res.N > 0 {
		res.KBAvg = float64(size) / float64(res.N) / 1000
	}
	return res
}
