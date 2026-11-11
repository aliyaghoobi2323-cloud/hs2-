package main

// Interactive traffic shapes beyond the 1-byte echo:
//
//   - call: a constant two-way stream like a video call (fixed-size frames at a
//     fixed interval, echoed back by the server, so both directions carry the
//     same rate);
//   - game: small frames (60-200 bytes) every 20-50 ms, echoed back;
//   - crowd: many light connections, each doing a small request/response now
//     and then, the way thousands of chat/browsing users look to the tunnel.
//     They count as "flowing" for the link pool (a few hundred bytes/s that
//     never pause for long), so the pool sizes itself to tens of links.
//
// call and game frames carry their scheduled send time, so a frame held back
// by a blocked write is charged for the wait too, as an app would feel it.

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// streamResult is the JSON summary of one call or game stream.
type streamResult struct {
	Sent    int     `json:"sent"`           // frames scheduled after warm-up
	N       int     `json:"n"`              // of them, echoed back before the end
	Lost    int     `json:"lost"`           // never came back (counted as late too)
	P50     float64 `json:"p50_ms"`         // round-trip time
	P95     float64 `json:"p95_ms"`         //
	P99     float64 `json:"p99_ms"`         //
	Max     float64 `json:"max_ms"`         //
	Jitter  float64 `json:"jitter_ms"`      // mean |rtt[i] - rtt[i-1]|, in arrival order
	LatePct float64 `json:"late_pct"`       // share of Sent with rtt > -latems, or lost
	Mbps    float64 `json:"mbps,omitempty"` // offered rate, one direction (call)
	// SecP99 / SecMax: per 1 s of send time from the probe start, the rtt p99 / max (-1: no sample)
	SecP99 []float64 `json:"sec_p99,omitempty"`
	SecMax []float64 `json:"sec_max,omitempty"`
	Error  string    `json:"error,omitempty"`
	rtts   []float64 // the samples, for callers that aggregate several streams
}

// crowdResult is the JSON summary of the crowd load.
type crowdResult struct {
	Conns int     `json:"conns"` // connections asked for
	Open  int     `json:"open"`  // connected at the end
	N     int     `json:"n"`     // requests answered after warm-up
	Fail  int     `json:"fail"`  // dial or request errors
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
	// Win: per -crowdwin window (from the start, seconds)
	Win []crowdWinResult `json:"win,omitempty"`
}

type crowdWinResult struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
	N    int     `json:"n"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
}

// crowdWin / probeStart: -crowdwin and the probe's start (crowd windows).
var (
	crowdWin   string
	probeStart time.Time
)

// crowdWindows splits the crowd's latencies (at seconds ts from the start)
// at the -crowdwin boundaries.
func crowdWindows(ts, lat []float64) []crowdWinResult {
	if crowdWin == "" {
		return nil
	}
	b := []float64{0}
	for _, f := range strings.Split(crowdWin, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(f), 64); err == nil {
			b = append(b, v)
		}
	}
	b = append(b, 1e9)
	var out []crowdWinResult
	for i := 0; i+1 < len(b); i++ {
		var w []float64
		for k, t := range ts {
			if t >= b[i] && t < b[i+1] {
				w = append(w, lat[k])
			}
		}
		out = append(out, crowdWinResult{From: b[i], To: b[i+1], N: len(w), P95: pct(w, .95), P99: pct(w, .99)})
	}
	return out
}

// parseRate reads "2mbit", "500kbit", "1.5m", "64k" or plain bits/s.
func parseRate(s string) (float64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "bps"), "bit")
	mul := 1.0
	switch {
	case strings.HasSuffix(s, "g"):
		mul, s = 1e9, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mul, s = 1e6, s[:len(s)-1]
	case strings.HasSuffix(s, "k"):
		mul, s = 1e3, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("bad rate %q", s)
	}
	return v * mul, nil
}

// crowdMinReply: the smallest crowd reply, bytes.
const crowdMinReply = 600

// frameHdr: u16 total frame length, u32 seq, i64 scheduled send time
// (nanoseconds since the probe started).
const frameHdr = 14

type streamSpec struct {
	addr   string
	delay  time.Duration // start this long after the probe
	gap    func() time.Duration
	size   func() int    // whole frame, >= frameHdr
	start  time.Time     // probe start: time base of the frames
	warm   time.Duration // frames scheduled before start+warm are not counted
	lateMs float64
	mbps   float64
}

// runStream runs one framed echo stream until stop, then waits up to 3 s for
// the frames still in flight.
func runStream(sp streamSpec, stop <-chan struct{}) *streamResult {
	res := &streamResult{Mbps: sp.mbps}
	select {
	case <-stop:
		res.Error = "stopped before start"
		return res
	case <-time.After(sp.delay):
	}
	c, err := net.DialTimeout("tcp", sp.addr, 10*time.Second)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	c.Write([]byte{'E'})
	var b1 [1]byte
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, b1[:]); err != nil {
		res.Error = "handshake: " + err.Error()
		return res
	}
	c.SetReadDeadline(time.Time{})

	var sent, recv atomic.Int64 // counted frames only
	var writerDone atomic.Bool
	var rtts []float64
	var secs []int
	var jsum float64
	var jn int
	readerDone := make(chan struct{})
	go func() { // reader
		defer close(readerDone)
		buf := make([]byte, 65536)
		prev := -1.0
		for {
			if _, err := io.ReadFull(c, buf[:2]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(buf[:2]))
			if n < frameHdr {
				return
			}
			if _, err := io.ReadFull(c, buf[2:n]); err != nil {
				return
			}
			ts := time.Duration(int64(binary.BigEndian.Uint64(buf[6:14])))
			if ts < sp.warm {
				continue
			}
			rtt := ms(time.Since(sp.start) - ts)
			rtts = append(rtts, rtt)
			secs = append(secs, int(ts/time.Second))
			if prev >= 0 {
				jsum += math.Abs(rtt - prev)
				jn++
			}
			prev = rtt
			if recv.Add(1) >= sent.Load() && writerDone.Load() {
				return
			}
		}
	}()
	// writer
	buf := make([]byte, 65536)
	next := time.Now()
	tm := time.NewTimer(0)
	defer tm.Stop()
	var seq uint32
loop:
	for {
		if d := time.Until(next); d > 0 {
			tm.Reset(d)
			select {
			case <-stop:
				break loop
			case <-tm.C:
			}
		} else {
			select {
			case <-stop:
				break loop
			default:
			}
		}
		n := min(max(sp.size(), frameHdr), len(buf))
		ts := next.Sub(sp.start)
		binary.BigEndian.PutUint16(buf[0:2], uint16(n))
		binary.BigEndian.PutUint32(buf[2:6], seq)
		binary.BigEndian.PutUint64(buf[6:14], uint64(ts))
		seq++
		if ts >= sp.warm {
			sent.Add(1)
		}
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(buf[:n]); err != nil {
			res.Error = "write: " + err.Error()
			break
		}
		next = next.Add(sp.gap())
	}
	writerDone.Store(true)
	if recv.Load() >= sent.Load() {
		c.Close()
	} else {
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
	}
	<-readerDone
	res.Sent = int(sent.Load())
	res.N = len(rtts)
	res.rtts = rtts
	res.SecP99, res.SecMax = perSecond(secs, rtts)
	res.Lost = max(res.Sent-res.N, 0)
	res.P50, res.P95, res.P99, res.Max = pct(rtts, .5), pct(rtts, .95), pct(rtts, .99), pct(rtts, 1)
	if jn > 0 {
		res.Jitter = jsum / float64(jn)
	}
	late := res.Lost
	for _, v := range rtts {
		if v > sp.lateMs {
			late++
		}
	}
	if res.Sent > 0 {
		res.LatePct = 100 * float64(late) / float64(res.Sent)
	}
	return res
}

// callSpec: size-byte frames at a fixed interval that makes bitsPerSec.
func callSpec(bitsPerSec float64, size int) (gap func() time.Duration, sz func() int) {
	size = max(size, frameHdr)
	g := time.Duration(float64(size*8) / bitsPerSec * float64(time.Second))
	return func() time.Duration { return g }, func() int { return size }
}

// gameSpec: 60-200 byte frames every 20-50 ms.
func gameSpec() (gap func() time.Duration, sz func() int) {
	return func() time.Duration { return time.Duration(20+rand.IntN(31)) * time.Millisecond },
		func() int { return 60 + rand.IntN(141) }
}

// serveRequests is the server side of a crowd connection: each request is
// u16 padding length, u32 reply length, then the padding; the reply is that
// many bytes.
func serveRequests(c net.Conn, blob []byte) {
	var h [6]byte
	for {
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		pad := int(binary.BigEndian.Uint16(h[0:2]))
		n := int(binary.BigEndian.Uint32(h[2:6]))
		if _, err := io.CopyN(io.Discard, c, int64(pad)); err != nil {
			return
		}
		for n > 0 {
			k := min(n, len(blob))
			if _, err := c.Write(blob[:k]); err != nil {
				return
			}
			n -= k
		}
	}
}

// runCrowd keeps n light connections, each sending a small request every
// ~every (uniform 0.5x-1.5x) and reading a reply of crowdMinReply..maxReply
// bytes. The defaults (0.7 s, up to 4 KiB: ~3 KB/s per connection) keep every
// 2 s health sample above the pool's flowSteadyRate (256 B/s), so each
// connection counts as flowing and the pool sizes for them (per_link each).
// Connections open over the first 2 s; a failed one redials after 1 s.
func runCrowd(addr string, n int, every time.Duration, maxReply int, measuring func() bool, stop <-chan struct{}) *crowdResult {
	res := &crowdResult{Conns: n}
	var mu sync.Mutex
	var lat, lts []float64
	var open atomic.Int32
	var wg sync.WaitGroup
	maxReply = max(maxReply, crowdMinReply)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait := func(d time.Duration) bool {
				select {
				case <-stop:
					return false
				case <-time.After(d):
					return true
				}
			}
			if !wait(time.Duration(rand.Int64N(int64(2 * time.Second)))) {
				return
			}
			fail := func() {
				mu.Lock()
				res.Fail++
				mu.Unlock()
			}
			for {
				c, err := net.DialTimeout("tcp", addr, 10*time.Second)
				if err != nil {
					fail()
					if !wait(time.Second) {
						return
					}
					continue
				}
				open.Add(1)
				done := make(chan struct{})
				go func() {
					select {
					case <-stop:
					case <-done:
					}
					c.Close()
				}()
				req := make([]byte, 6+200)
				reply := make([]byte, 32<<10)
				ok := true
				if _, err := c.Write([]byte{'R'}); err != nil {
					ok = false
				}
				for ok {
					if !wait(time.Duration(float64(every) * (0.5 + rand.Float64()))) {
						break
					}
					pad := 20 + rand.IntN(181)
					want := crowdMinReply + rand.IntN(maxReply-crowdMinReply+1)
					binary.BigEndian.PutUint16(req[0:2], uint16(pad))
					binary.BigEndian.PutUint32(req[2:6], uint32(want))
					s := time.Now()
					c.SetDeadline(s.Add(10 * time.Second))
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
					if ok && measuring() {
						mu.Lock()
						lat = append(lat, ms(time.Since(s)))
						lts = append(lts, s.Sub(probeStart).Seconds())
						mu.Unlock()
					}
				}
				close(done)
				open.Add(-1)
				select {
				case <-stop:
					return
				default:
				}
				fail()
				if !wait(time.Second) {
					return
				}
			}
		}()
	}
	// the open count just before the end (closing at stop would hide it)
	tk := time.NewTicker(200 * time.Millisecond)
	for running := true; running; {
		select {
		case <-stop:
			running = false
		case <-tk.C:
			res.Open = int(open.Load())
		}
	}
	tk.Stop()
	wg.Wait()
	res.N = len(lat)
	res.P50, res.P95, res.P99, res.Max = pct(lat, .5), pct(lat, .95), pct(lat, .99), pct(lat, 1)
	res.Win = crowdWindows(lts, append([]float64(nil), lat...))
	return res
}

// perSecond: the p99 and max of lat per whole second sec (-1 where empty).
func perSecond(sec []int, lat []float64) (p99, mx []float64) {
	n := 0
	for _, s := range sec {
		n = max(n, s+1)
	}
	by := make([][]float64, n)
	for i, s := range sec {
		if s >= 0 {
			by[s] = append(by[s], lat[i])
		}
	}
	for _, v := range by {
		if len(v) == 0 {
			p99, mx = append(p99, -1), append(mx, -1)
			continue
		}
		p99, mx = append(p99, pct(v, .99)), append(mx, pct(v, 1))
	}
	return
}
