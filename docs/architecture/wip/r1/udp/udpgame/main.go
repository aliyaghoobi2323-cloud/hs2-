// udpgame: a game-like UDP flow for the r1-udp measurements (R1 round 3,
// docs/architecture/wip/r1/udp). Standard library only.
//
//	udpgame server -listen 127.0.0.1:5211 -dur 40 -out server.json
//	udpgame client -to 10.201.0.1:5311 -src 10.100.0.2 -flow 1 -dur 40 -out client.json
//
// The client sends 64 datagrams a second of 60-120 bytes (a game client's
// input); the server answers every client it has heard from with 30 a second
// of 100-600 bytes (the world state). Every datagram carries
//
//	"UG" dir(1) 0 flow(u32) seq(u32) sent(unix ns, u64)   (20 bytes, then padding)
//
// and each receiver records, for every datagram, its seq, when it was sent and
// when it arrived. Both ends run on the same host (network namespaces share
// the clock), so arrival - sent is the one-way delay. Each side writes one
// JSON file: what it sent (per flow) and what it received (per flow).
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	hdr        = 20
	upHz       = 64
	downHz     = 30
	upMin      = 60
	upMax      = 120
	downMin    = 100
	downMax    = 600
	dirUp byte = 'u'
	dirDn byte = 'd'
)

// Rx is one received datagram: seq, sent and arrival (unix ns).
type Rx [3]int64

type flowLog struct {
	Sent  int    `json:"sent"`  // datagrams this side sent on the flow
	First int64  `json:"first"` // when the first was sent (unix ns)
	Recv  []Rx   `json:"recv"`  // what this side received on the flow
	Peer  string `json:"peer,omitempty"`
}

type out struct {
	Role  string              `json:"role"`
	Start int64               `json:"start"`
	Flows map[string]*flowLog `json:"flows"`
}

func put(b []byte, dir byte, flow, seq uint32) {
	b[0], b[1], b[2], b[3] = 'U', 'G', dir, 0
	binary.BigEndian.PutUint32(b[4:], flow)
	binary.BigEndian.PutUint32(b[8:], seq)
	binary.BigEndian.PutUint64(b[12:], uint64(time.Now().UnixNano()))
}

func parse(b []byte) (dir byte, flow, seq uint32, sent int64, ok bool) {
	if len(b) < hdr || b[0] != 'U' || b[1] != 'G' {
		return 0, 0, 0, 0, false
	}
	return b[2], binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:]), int64(binary.BigEndian.Uint64(b[12:])), true
}

func write(path string, o *out) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(o); err != nil {
		log.Fatal(err)
	}
}

// pace calls send every 1/hz from start until end, on schedule (a late tick
// is sent at once, never skipped: a game client does not drop its inputs).
func pace(hz int, end time.Time, send func()) {
	gap := time.Second / time.Duration(hz)
	next := time.Now()
	for time.Now().Before(end) {
		send()
		next = next.Add(gap)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
	}
}

func server(listen string, dur time.Duration, path string) {
	pc, err := net.ListenPacket("udp", listen)
	if err != nil {
		log.Fatal(err)
	}
	o := &out{Role: "server", Start: time.Now().UnixNano(), Flows: map[string]*flowLog{}}
	var mu sync.Mutex
	end := time.Now().Add(dur)
	started := map[uint32]bool{}
	go func() {
		b := make([]byte, 65536)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			now := time.Now().UnixNano()
			dir, flow, seq, sent, ok := parse(b[:n])
			if !ok || dir != dirUp {
				continue
			}
			k := fmt.Sprint(flow)
			mu.Lock()
			fl := o.Flows[k]
			if fl == nil {
				fl = &flowLog{}
				o.Flows[k] = fl
			}
			fl.Recv = append(fl.Recv, Rx{int64(seq), sent, now})
			fl.Peer = a.String()
			start := !started[flow]
			started[flow] = true
			mu.Unlock()
			if start {
				// the world state for this client, to the address it came from
				go func(flow uint32, a net.Addr, fl *flowLog) {
					p := make([]byte, downMax)
					var seq uint32
					first := true
					pace(downHz, end, func() {
						sz := downMin + rand.IntN(downMax-downMin+1)
						put(p, dirDn, flow, seq)
						pc.WriteTo(p[:sz], a)
						seq++
						mu.Lock()
						if first {
							fl.First, first = time.Now().UnixNano(), false
						}
						fl.Sent++
						mu.Unlock()
					})
				}(flow, a, fl)
			}
		}
	}()
	// the file is written at -dur + 3 s, or at once on SIGTERM / SIGINT
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-time.After(time.Until(end) + 3*time.Second):
	case <-sig:
	}
	mu.Lock()
	write(path, o)
	mu.Unlock()
	pc.Close()
}

func client(to, src string, flow uint32, dur time.Duration, path string) {
	var la *net.UDPAddr
	if src != "" {
		la = &net.UDPAddr{IP: net.ParseIP(src)}
	}
	ra, err := net.ResolveUDPAddr("udp", to)
	if err != nil {
		log.Fatal(err)
	}
	c, err := net.DialUDP("udp", la, ra)
	if err != nil {
		log.Fatal(err)
	}
	k := fmt.Sprint(flow)
	fl := &flowLog{}
	o := &out{Role: "client", Start: time.Now().UnixNano(), Flows: map[string]*flowLog{k: fl}}
	var mu sync.Mutex
	go func() {
		b := make([]byte, 65536)
		for {
			n, err := c.Read(b)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					return
				}
				continue // ECONNREFUSED while the far end is not there yet
			}
			now := time.Now().UnixNano()
			dir, f, seq, sent, ok := parse(b[:n])
			if !ok || dir != dirDn || f != flow {
				continue
			}
			mu.Lock()
			fl.Recv = append(fl.Recv, Rx{int64(seq), sent, now})
			mu.Unlock()
		}
	}()
	end := time.Now().Add(dur)
	p := make([]byte, upMax)
	var seq uint32
	fl.First = time.Now().UnixNano()
	pace(upHz, end, func() {
		sz := upMin + rand.IntN(upMax-upMin+1)
		put(p, dirUp, flow, seq)
		c.Write(p[:sz])
		seq++
		mu.Lock()
		fl.Sent++
		mu.Unlock()
	})
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	time.Sleep(3200 * time.Millisecond)
	mu.Lock()
	write(path, o)
	mu.Unlock()
	c.Close()
}

func main() {
	log.SetFlags(log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: udpgame server|client ...")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:5211", "server: UDP address")
	to := fs.String("to", "", "client: the edge's UDP user port")
	src := fs.String("src", "", "client: source IP")
	flow := fs.Uint("flow", 1, "client: flow id")
	dur := fs.Float64("dur", 40, "seconds")
	path := fs.String("out", "", "JSON output")
	fs.Parse(os.Args[2:])
	d := time.Duration(*dur * float64(time.Second))
	switch os.Args[1] {
	case "server":
		server(*listen, d, *path)
	case "client":
		client(*to, *src, uint32(*flow), d, *path)
	default:
		fmt.Fprintln(os.Stderr, "unknown role", os.Args[1])
		os.Exit(2)
	}
}
