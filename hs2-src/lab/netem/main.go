//go:build linux

// netem is a userspace network emulator for the hs2 test lab. It bridges two
// interfaces at layer 2 (AF_PACKET) and, per direction, applies:
//
//   - a bottleneck rate with a byte-accurate serialisation clock,
//   - a tail-drop queue bounded in time (how much delay the "ISP" buffers),
//   - a fixed one-way propagation delay,
//   - random loss,
//   - an optional per-connection policer (DPI-style throttling of each single
//     TCP/UDP flow, which is what makes multi-link tunnels worthwhile).
//
// It exists because the lab kernel has no sch_netem. Frames are forwarded
// untouched, so TCP between the two sides behaves as it would across a real
// path. Turn off GSO/TSO/GRO on both bridged interfaces (ethtool -K) so every
// frame fits the MTU.
package main

import (
	"encoding/binary"
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

	"golang.org/x/sys/unix"
)

type dirCfg struct {
	rate      float64 // bits/s, 0 = unlimited
	delay     time.Duration
	queue     time.Duration // max queueing delay before tail drop
	loss      float64
	flowRate  float64 // per-flow policer, bits/s, 0 = off
	flowBurst int     // bytes
}

type pkt struct {
	b  []byte
	at time.Time // delivery time
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func openPacket(ifname string) (int, int) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		log.Fatalf("netem: %s: %v", ifname, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		log.Fatalf("netem: socket: %v", err)
	}
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifi.Index}
	if err := unix.Bind(fd, sa); err != nil {
		log.Fatalf("netem: bind %s: %v", ifname, err)
	}
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 8<<20)
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 8<<20)
	return fd, ifi.Index
}

// flowKey returns an IPv4 5-tuple key for TCP/UDP frames, or 0.
func flowKey(f []byte) uint64 {
	if len(f) < 14+20 || binary.BigEndian.Uint16(f[12:14]) != 0x0800 {
		return 0
	}
	ip := f[14:]
	proto := ip[9]
	if proto != 6 && proto != 17 {
		return 0
	}
	ihl := int(ip[0]&0x0f) * 4
	if len(ip) < ihl+4 {
		return 0
	}
	h := uint64(14695981039346656037)
	for _, c := range append(append([]byte{proto}, ip[12:20]...), ip[ihl:ihl+4]...) {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

type policer struct {
	tokens float64
	last   time.Time
}

func run(name string, inFd, outFd, outIdx int, c dirCfg, stats *counters) {
	q := make(chan pkt, 1<<16)
	// writer: deliver frames at their due time
	go func() {
		sa := &unix.SockaddrLinklayer{Ifindex: outIdx}
		for p := range q {
			if d := time.Until(p.at); d > 0 {
				time.Sleep(d)
			}
			unix.Sendto(outFd, p.b, 0, sa)
		}
	}()
	var nextFree time.Time // when the bottleneck finishes the last frame
	flows := map[uint64]*policer{}
	buf := make([]byte, 65536)
	for {
		n, from, err := unix.Recvfrom(inFd, buf, 0)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Fatalf("netem %s: recv: %v", name, err)
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		now := time.Now()
		stats.add(name, "in", n)
		if c.loss > 0 && rand.Float64() < c.loss {
			stats.add(name, "loss", n)
			continue
		}
		if c.flowRate > 0 {
			if k := flowKey(buf[:n]); k != 0 {
				p := flows[k]
				if p == nil {
					p = &policer{tokens: float64(c.flowBurst), last: now}
					flows[k] = p
				}
				p.tokens += now.Sub(p.last).Seconds() * c.flowRate / 8
				if p.tokens > float64(c.flowBurst) {
					p.tokens = float64(c.flowBurst)
				}
				p.last = now
				if p.tokens < float64(n) {
					stats.add(name, "police", n)
					continue
				}
				p.tokens -= float64(n)
			}
		}
		depart := now
		if c.rate > 0 {
			if nextFree.After(now) {
				depart = nextFree
			}
			if depart.Sub(now) > c.queue {
				stats.add(name, "qdrop", n)
				continue
			}
			nextFree = depart.Add(time.Duration(float64(n*8) / c.rate * float64(time.Second)))
		}
		b := make([]byte, n)
		copy(b, buf[:n])
		q <- pkt{b: b, at: depart.Add(c.delay)}
	}
}

type counters struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *counters) add(dir, k string, n int) {
	c.mu.Lock()
	c.m[dir+"."+k]++
	c.mu.Unlock()
}

func parseRate(s string) float64 {
	if s == "" || s == "0" {
		return 0
	}
	var v float64
	var unit string
	fmt.Sscanf(s, "%f%s", &v, &unit)
	switch unit {
	case "kbit":
		return v * 1e3
	case "mbit":
		return v * 1e6
	case "gbit":
		return v * 1e9
	}
	return v
}

func main() {
	a := flag.String("a", "", "interface A")
	b := flag.String("b", "", "interface B")
	rate := flag.String("rate", "0", "bottleneck rate each way, e.g. 20mbit")
	rateBA := flag.String("rate-ba", "", "rate B->A if different")
	delay := flag.Duration("delay", 0, "one-way delay")
	queue := flag.Duration("queue", 200*time.Millisecond, "max queueing delay")
	loss := flag.Float64("loss", 0, "random loss probability each way")
	flowRate := flag.String("flowrate", "0", "per-flow policer rate each way")
	flowBurst := flag.Int("flowburst", 64<<10, "per-flow policer burst, bytes")
	flag.Parse()
	fa, ia := openPacket(*a)
	fb, ib := openPacket(*b)
	ab := dirCfg{rate: parseRate(*rate), delay: *delay, queue: *queue, loss: *loss,
		flowRate: parseRate(*flowRate), flowBurst: *flowBurst}
	ba := ab
	if *rateBA != "" {
		ba.rate = parseRate(*rateBA)
	}
	st := &counters{m: map[string]int{}}
	go run("ab", fa, fb, ib, ab, st)
	go run("ba", fb, fa, ia, ba, st)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	st.mu.Lock()
	fmt.Fprintln(os.Stderr, "netem stats:", st.m)
	st.mu.Unlock()
}
