// dglab is the hs2 lab's datagram-carrier load tool. It runs the real UDP
// carrier (Noise IKpsk2 + ChaCha20-Poly1305 + adaptive FEC + pacer) over any
// encapsulation (udp/icmp/gre/ipip/ipx) between two network namespaces, drives
// bulk traffic in one or both directions over one or more parallel links, and
// reports goodput, residual loss and one-way latency as one JSON line. Both
// namespaces share the host clock, so one-way delay is measured directly.
//
//	kh# dglab -server -encap icmp -listen 192.168.50.2 -key <hex>
//	ir# dglab -encap icmp -addr 192.168.50.2 -key <hex> -links 4 -dir both -t 10s
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

// Frame ops (first byte of a TypeData payload).
const (
	opData   = 0xd0 // [op][seq:8][sendNanos:8][pad]
	opStart  = 0xc0 // client->server: [op][flags:1][rateKbps:4][size:2][durMs:4][run:4]
	opReport = 0xe0 // client->server: send me your upload stats
	opStats  = 0xe1 // server->client: [op][json]
)

type rx struct {
	mu     sync.Mutex
	seen   map[uint64]bool
	bytes  int64
	delays []float64 // ms
	first  time.Time
	last   time.Time
}

func newRx() *rx { return &rx{seen: map[uint64]bool{}} }

func (r *rx) add(p []byte, now time.Time) {
	if len(p) < 17 {
		return
	}
	seq := binary.BigEndian.Uint64(p[1:])
	sent := int64(binary.BigEndian.Uint64(p[9:]))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[seq] {
		return
	}
	r.seen[seq] = true
	r.bytes += int64(len(p))
	if r.first.IsZero() {
		r.first = now
	}
	r.last = now
	r.delays = append(r.delays, float64(now.UnixNano()-sent)/1e6)
}

type dirStats struct {
	Sent        int64   `json:"sent"`
	Delivered   int64   `json:"delivered"`
	ResidualPct float64 `json:"residual_loss_pct"`
	GoodputMbps float64 `json:"goodput_mbps"`
	P50         float64 `json:"p50_ms"`
	P95         float64 `json:"p95_ms"`
	P99         float64 `json:"p99_ms"`
}

func (r *rx) stats(sent int64, dur time.Duration) dirStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := dirStats{Sent: sent, Delivered: int64(len(r.seen))}
	if sent > 0 {
		d.ResidualPct = 100 * float64(sent-d.Delivered) / float64(sent)
		if d.ResidualPct < 0 {
			d.ResidualPct = 0
		}
	}
	if dur > 0 {
		d.GoodputMbps = float64(r.bytes) * 8 / dur.Seconds() / 1e6
	}
	sort.Float64s(r.delays)
	pc := func(p float64) float64 {
		if len(r.delays) == 0 {
			return 0
		}
		return math.Round(r.delays[int(p*float64(len(r.delays)-1))]*10) / 10
	}
	d.P50, d.P95, d.P99 = pc(0.5), pc(0.95), pc(0.99)
	return d
}

// sender pushes numbered frames on one carrier at rateKbps (0 = as fast as the
// carrier accepts) until the deadline. Sequence numbers are drawn from next so
// several links share one numbering.
func sender(c *udpcarrier.Conn, next *atomic.Uint64, sent *atomic.Int64, size int, rateKbps float64, until time.Time) {
	if size < 17 {
		size = 17
	}
	buf := make([]byte, size)
	buf[0] = opData
	var gap time.Duration
	if rateKbps > 0 {
		gap = time.Duration(float64(size*8) / (rateKbps * 1000) * float64(time.Second))
	}
	t := time.Now()
	for time.Now().Before(until) {
		binary.BigEndian.PutUint64(buf[1:], next.Add(1))
		binary.BigEndian.PutUint64(buf[9:], uint64(time.Now().UnixNano()))
		if err := c.SendFrame(core.TypeData, buf); err != nil {
			return
		}
		sent.Add(1)
		if gap > 0 {
			t = t.Add(gap)
			if d := time.Until(t); d > 0 {
				time.Sleep(d)
			}
		}
	}
}

func main() {
	server := flag.Bool("server", false, "run the server side")
	kind := flag.String("encap", "udp", "encapsulation: udp, icmp, gre, ipip, ipx")
	proto := flag.Int("proto", 0, "ipx IP protocol (0 = default)")
	listen := flag.String("listen", "0.0.0.0:2096", "server listen address")
	addr := flag.String("addr", "", "client: server address")
	bind := flag.String("bind", "", "local source IP")
	keyHex := flag.String("key", "", "shared key, hex")
	links := flag.Int("links", 1, "client: parallel links")
	dir := flag.String("dir", "up", "client: up, down or both")
	rate := flag.Float64("rate", 0, "offered rate per direction, Mbit/s (0 = as fast as accepted)")
	size := flag.Int("size", 1200, "frame size, bytes")
	dur := flag.Duration("t", 10*time.Second, "test duration")
	mtu := flag.Int("mtu", 0, "carrier inner MTU (0 = default)")
	flag.Parse()
	key, err := hex.DecodeString(*keyHex)
	if err != nil || len(key) == 0 {
		log.Fatal("dglab: -key <hex> required")
	}
	ec := udpcarrier.EncapConfig{Kind: *kind, BindIP: *bind, Proto: *proto}
	if *server {
		runServer(*listen, ec, key, *mtu)
		return
	}
	runClient(*addr, ec, key, *mtu, *links, *dir, *rate, *size, *dur)
}

func runServer(listen string, ec udpcarrier.EncapConfig, key []byte, mtu int) {
	ln, err := udpcarrier.ListenCfg(listen, ec, key, mtu)
	if err != nil {
		log.Fatal(err)
	}
	// One upload tally shared by every link of a client run (links of one run
	// share the sequence numbering); a new opStart with reset resets it.
	var mu sync.Mutex
	up := newRx()
	var run uint32
	for {
		c, err := ln.Accept(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		go func(c *udpcarrier.Conn) {
			defer c.Close()
			var next atomic.Uint64
			var sent atomic.Int64
			var started bool
			for {
				ft, p, err := c.ReadFrame()
				if err != nil {
					return
				}
				if ft != core.TypeData || len(p) == 0 {
					continue
				}
				switch p[0] {
				case opData:
					mu.Lock()
					r := up
					mu.Unlock()
					r.add(p, time.Now())
				case opStart:
					if len(p) < 16 || started {
						continue
					}
					started = true
					if id := binary.BigEndian.Uint32(p[12:]); true {
						mu.Lock()
						if id != run {
							run, up = id, newRx()
						}
						mu.Unlock()
					}
					if p[1]&1 != 0 {
						rateKbps := float64(binary.BigEndian.Uint32(p[2:]))
						sz := int(binary.BigEndian.Uint16(p[6:]))
						d := time.Duration(binary.BigEndian.Uint32(p[8:])) * time.Millisecond
						next.Store(uint64(p[1]>>2) << 56) // per-link numbering space
						go sender(c, &next, &sent, sz, rateKbps, time.Now().Add(d))
					}
				case opReport:
					mu.Lock()
					r := up
					mu.Unlock()
					r.mu.Lock()
					res := map[string]any{"delivered": len(r.seen), "bytes": r.bytes,
						"first": r.first.UnixNano(), "last": r.last.UnixNano(), "down_sent": sent.Load()}
					ds := append([]float64(nil), r.delays...)
					r.mu.Unlock()
					sort.Float64s(ds)
					if len(ds) > 0 {
						res["p50"] = ds[int(0.5*float64(len(ds)-1))]
						res["p95"] = ds[int(0.95*float64(len(ds)-1))]
						res["p99"] = ds[int(0.99*float64(len(ds)-1))]
					}
					b, _ := json.Marshal(res)
					c.SendFrame(core.TypeData, append([]byte{opStats}, b...))
				}
			}
		}(c)
	}
}

func runClient(addr string, ec udpcarrier.EncapConfig, key []byte, mtu, links int, dir string, rate float64, size int, dur time.Duration) {
	if links < 1 {
		links = 1
	}
	t0 := time.Now()
	var cs []*udpcarrier.Conn
	for i := 0; i < links; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		c, err := udpcarrier.DialCfg(ctx, addr, ec, key, mtu)
		cancel()
		if err != nil {
			fmt.Printf(`{"encap":%q,"error":%q}`+"\n", ec.Kind, err.Error())
			os.Exit(1)
		}
		cs = append(cs, c)
	}
	setup := time.Since(t0)
	doUp, doDown := dir == "up" || dir == "both", dir == "down" || dir == "both"
	down := newRx()
	statsCh := make(chan []byte, links)
	for _, c := range cs {
		go func(c *udpcarrier.Conn) {
			for {
				ft, p, err := c.ReadFrame()
				if err != nil {
					return
				}
				if ft != core.TypeData || len(p) == 0 {
					continue
				}
				switch p[0] {
				case opData:
					down.add(p, time.Now())
				case opStats:
					statsCh <- append([]byte(nil), p[1:]...)
				}
			}
		}(c)
	}
	perLinkKbps := rate * 1000 / float64(links)
	runID := uint32(time.Now().UnixNano())
	for i, c := range cs {
		st := make([]byte, 16)
		st[0] = opStart
		var flags byte
		if doDown {
			flags |= 1
		}
		flags |= byte(i) << 2
		st[1] = flags
		binary.BigEndian.PutUint32(st[2:], uint32(perLinkKbps))
		binary.BigEndian.PutUint16(st[6:], uint16(size))
		binary.BigEndian.PutUint32(st[8:], uint32(dur/time.Millisecond))
		binary.BigEndian.PutUint32(st[12:], runID)
		for k := 0; k < 3; k++ { // control frames ride without FEC: repeat
			c.SendFrame(core.TypeData, st)
		}
	}
	if os.Getenv("HS2_DGTRACE") != "" {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for range t.C {
				s := cs[0].Stats()
				fmt.Fprintf(os.Stderr, "trace loss=%dppm parity=%.2f btlbw=%.2fMbit rtt=%s\n", s.LossPPM, s.ParityRatio, s.BtlBwBytes*8/1e6, s.RTProp)
			}
		}()
	}
	until := time.Now().Add(dur)
	var next atomic.Uint64
	var upSent atomic.Int64
	var wg sync.WaitGroup
	if doUp {
		for _, c := range cs {
			wg.Add(1)
			go func(c *udpcarrier.Conn) { defer wg.Done(); sender(c, &next, &upSent, size, perLinkKbps, until) }(c)
		}
	}
	wg.Wait()
	time.Sleep(time.Until(until))
	time.Sleep(1500 * time.Millisecond) // drain: FEC recovery and in-flight frames
	out := map[string]any{"encap": ec.Kind, "links": links, "dir": dir, "rate_mbps": rate,
		"size": size, "t": dur.String(), "setup_ms": setup.Milliseconds()}
	if doUp {
		var rep map[string]any
		for try := 0; try < 10 && rep == nil; try++ {
			cs[0].SendFrame(core.TypeData, []byte{opReport})
			select {
			case b := <-statsCh:
				json.Unmarshal(b, &rep)
			case <-time.After(500 * time.Millisecond):
			}
		}
		u := dirStats{Sent: upSent.Load()}
		if rep != nil {
			u.Delivered = int64(rep["delivered"].(float64))
			if u.Sent > 0 {
				u.ResidualPct = math.Max(0, 100*float64(u.Sent-u.Delivered)/float64(u.Sent))
			}
			u.GoodputMbps = rep["bytes"].(float64) * 8 / dur.Seconds() / 1e6
			for k, dst := range map[string]*float64{"p50": &u.P50, "p95": &u.P95, "p99": &u.P99} {
				if v, ok := rep[k].(float64); ok {
					*dst = math.Round(v*10) / 10
				}
			}
		}
		out["up"] = u
	}
	if doDown {
		var dsent int64
		for try := 0; try < 10; try++ {
			cs[0].SendFrame(core.TypeData, []byte{opReport})
			select {
			case b := <-statsCh:
				var rep map[string]any
				json.Unmarshal(b, &rep)
				if v, ok := rep["down_sent"].(float64); ok {
					dsent = int64(v)
				}
				try = 10
			case <-time.After(500 * time.Millisecond):
			}
		}
		// down_sent is per link on the server; with several links the per-link
		// counters are summed by asking each link.
		if links > 1 {
			dsent = 0
			for _, c := range cs {
				for try := 0; try < 10; try++ {
					c.SendFrame(core.TypeData, []byte{opReport})
					select {
					case b := <-statsCh:
						var rep map[string]any
						json.Unmarshal(b, &rep)
						if v, ok := rep["down_sent"].(float64); ok {
							dsent += int64(v)
						}
						try = 10
					case <-time.After(500 * time.Millisecond):
					}
				}
			}
		}
		out["down"] = down.stats(dsent, dur)
	}
	var carrier []map[string]any
	for _, c := range cs {
		s := c.Stats()
		carrier = append(carrier, map[string]any{"loss_ppm": s.LossPPM, "parity": math.Round(s.ParityRatio*100) / 100,
			"btlbw_mbps": math.Round(s.BtlBwBytes*8/1e5) / 10, "rtt_ms": s.RTProp.Milliseconds(),
			"send_mbps": math.Round(s.SendBytes*8/1e5) / 10, "queue_ms": math.Round(s.QueueSec * 1000),
			"pacer_held_ms": s.PacerHeld.Milliseconds(), "pacer_write_ms": s.PacerWrite.Milliseconds(), "pacer_writes": s.PacerWrites})
		c.Close()
	}
	out["carrier"] = carrier
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
