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
//
// Extra loads (streams.go), each reported as its own JSON object:
//
//	-call 2mbit   a two-way constant-rate stream like a video call ("call")
//	-game         small frames every 20-50 ms, echoed back ("game")
//	-late 5s      start the echo, call and game this long after the downloads
//	-crowd 300    light connections, a small request every ~-crowdevery ("crowd")
//	-web          page loads: 4 parallel 50-500 KB fetches every 2-3 s ("web", web.go)
//	-journey 20   N virtual users: reels, video call, big download, app ping,
//	              game, in turn ("journey", journey.go; -journeyscale shrinks it)
//
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
			case 'R': // crowd: small requests, replies of the asked size
				serveRequests(c, blob)
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
	UDPP99     float64 `json:"udp_p99_ms,omitempty"`
	UDPMax     float64 `json:"udp_max_ms,omitempty"`
	BulkErrors int     `json:"bulk_errors"`
	// optional loads; absent unless asked for
	Call  *streamResult `json:"call,omitempty"`
	Game  *streamResult `json:"game,omitempty"`
	Crowd *crowdResult  `json:"crowd,omitempty"`
	Web   *webResult    `json:"web,omitempty"`
	// the user-journey scenario (journey.go)
	Journey *journeyResult `json:"journey,omitempty"`
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
	echoDelay := flag.Duration("echodelay", 0, "open the echo connection this long after the downloads (a game started while they run)")
	seg := flag.Int64("seg", 0, "bytes per download connection before it reconnects (0 = one connection for the whole run)")
	call := flag.String("call", "", "also run a two-way video-call-like stream at this rate, e.g. 2mbit")
	callSize := flag.Int("callsize", 1000, "call frame size, bytes")
	game := flag.Bool("game", false, "also run a game-like stream: 60-200 byte frames every 20-50 ms")
	late := flag.Duration("late", 0, "start the echo, call and game this long after the downloads (sets -echodelay if that is 0)")
	lateMs := flag.Float64("latems", 150, "call/game: a round trip above this many ms counts as late")
	crowd := flag.Int("crowd", 0, "light connections, each sending a small request every ~-crowdevery")
	crowdEvery := flag.Duration("crowdevery", 700*time.Millisecond, "crowd: mean interval between requests per connection (uniform 0.5x-1.5x)")
	crowdMax := flag.Int("crowdmax", 4096, "crowd: largest reply, bytes (uniform from 600)")
	web := flag.Bool("web", false, "also load \"pages\": every ~-webevery, -webconns parallel short connections of -webmin..-webmax bytes")
	webEvery := flag.Duration("webevery", 2500*time.Millisecond, "web: mean interval between page starts (uniform 0.8x-1.2x)")
	webConns := flag.Int("webconns", 4, "web: connections per page")
	webMin := flag.Int("webmin", 50000, "web: smallest fetch, bytes")
	webMax := flag.Int("webmax", 500000, "web: largest fetch, bytes")
	journey := flag.Int("journey", 0, "virtual users going through reels, call, download, ping, game in turn (journey.go)")
	jScale := flag.Float64("journeyscale", 1, "journey: multiply sizes and rates by this (0.5 for a slow link)")
	jSpread := flag.Duration("jspread", 10*time.Second, "journey: users start spread over this long")
	jReels := flag.Int("jreels", 4, "journey: reels per round")
	jReelMin := flag.Int("jreelmin", 2000000, "journey: smallest reel, bytes")
	jReelMax := flag.Int("jreelmax", 8000000, "journey: largest reel, bytes")
	jTTF := flag.Int("jttf", 1000000, "journey: bytes whose arrival is the reel's TTFMB")
	jGapMin := flag.Duration("jreelgapmin", 2*time.Second, "journey: shortest pause between reels")
	jGapMax := flag.Duration("jreelgapmax", 6*time.Second, "journey: longest pause between reels")
	jCallMin := flag.String("jcallmin", "1.5mbit", "journey: lowest call rate")
	jCallMax := flag.String("jcallmax", "2.5mbit", "journey: highest call rate")
	jCallSize := flag.Int("jcallsize", 1000, "journey: call frame size, bytes")
	jCallTMin := flag.Duration("jcalltmin", 20*time.Second, "journey: shortest call")
	jCallTMax := flag.Duration("jcalltmax", 40*time.Second, "journey: longest call")
	jBulkTMin := flag.Duration("jbulktmin", 20*time.Second, "journey: shortest download")
	jBulkTMax := flag.Duration("jbulktmax", 60*time.Second, "journey: longest download")
	jPings := flag.Int("jpings", 5, "journey: app pings after the download")
	jGameTMin := flag.Duration("jgametmin", 20*time.Second, "journey: shortest game")
	jGameTMax := flag.Duration("jgametmax", 40*time.Second, "journey: longest game")
	jRotate := flag.Bool("jrotate", true, "journey: user i starts its first round at phase i%4 so a short run samples every phase")
	flag.Parse()
	if *late > 0 && *echoDelay == 0 {
		*echoDelay = *late
	}
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
		if !isUp && *seg > 0 {
			// A user who downloads file after file (or segment after segment),
			// each on a new connection.
			go func() {
				defer wg.Done()
				buf := make([]byte, 64<<10)
				for {
					select {
					case <-stop:
						return
					default:
					}
					c, err := net.DialTimeout("tcp", *addr, 10*time.Second)
					if err != nil {
						bulkErr.Add(1)
						time.Sleep(time.Second)
						continue
					}
					done := make(chan struct{})
					go func() {
						select {
						case <-stop:
						case <-done:
						}
						c.Close()
					}()
					c.Write([]byte{'B'})
					var got int64
					for got < *seg {
						n, err := c.Read(buf)
						got += int64(n)
						if measuring() {
							down.Add(int64(n))
						}
						if err != nil {
							break
						}
					}
					close(done)
				}
			}()
			continue
		}
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
		select {
		case <-stop:
			return
		case <-time.After(*echoDelay):
		}
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
	var callBps float64
	if *call != "" {
		var err error
		if callBps, err = parseRate(*call); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	stream := func(dst **streamResult, gap func() time.Duration, size func() int, mbps float64) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := runStream(streamSpec{addr: *addr, delay: *late, gap: gap, size: size,
				start: start, warm: max(*warm, *late), lateMs: *lateMs, mbps: mbps}, stop)
			mu.Lock()
			*dst = res
			mu.Unlock()
		}()
	}
	if callBps > 0 {
		gap, size := callSpec(callBps, *callSize)
		stream(&r.Call, gap, size, callBps/1e6)
	}
	if *game {
		gap, size := gameSpec()
		stream(&r.Game, gap, size, 0)
	}
	if *crowd > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := runCrowd(*addr, *crowd, *crowdEvery, *crowdMax, measuring, stop)
			mu.Lock()
			r.Crowd = res
			mu.Unlock()
		}()
	}
	if *web {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := runWeb(webSpec{addr: *addr, delay: *late, warm: max(*warm, *late), start: start,
				every: *webEvery, conns: max(*webConns, 1), minB: max(*webMin, 1),
				maxB: max(*webMax, *webMin, 1)}, stop)
			mu.Lock()
			r.Web = res
			mu.Unlock()
		}()
	}
	if *journey > 0 {
		cmin, err := parseRate(*jCallMin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		cmax, err := parseRate(*jCallMax)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		jsp := journeySpec{addr: *addr, users: *journey, scale: *jScale, spread: *jSpread,
			reels: *jReels, reelMin: *jReelMin, reelMax: max(*jReelMax, *jReelMin), ttf: *jTTF,
			reelGapMin: *jGapMin, gapMax: *jGapMax, callMin: cmin, callMax: max(cmax, cmin),
			callSize: *jCallSize, callTMin: *jCallTMin, callTMax: *jCallTMax,
			bulkTMin: *jBulkTMin, bulkTMax: *jBulkTMax, pings: *jPings,
			gameTMin: *jGameTMin, gameTMax: *jGameTMax, lateMs: *lateMs, rotate: *jRotate}
		if jsp.scale <= 0 {
			jsp.scale = 1
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := runJourney(jsp, stop)
			mu.Lock()
			r.Journey = res
			mu.Unlock()
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
		r.UDPP99, r.UDPMax = pct(udpRTT, .99), pct(udpRTT, 1)
	}
	json.NewEncoder(os.Stdout).Encode(r)
}
