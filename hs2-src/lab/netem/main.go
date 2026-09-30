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
	"strconv"
	"strings"
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
	// Bursty (Gilbert-Elliott) loss: when burstBadMs>0 the path alternates
	// between a good state (loss `loss`) and a bad state (loss `burstLoss`),
	// each lasting an exponentially distributed time. This makes genuine loss
	// bursts, which correlated random loss cannot, matching the real target
	// path far better than i.i.d. loss.
	burstLoss   float64
	burstGoodMs float64
	burstBadMs  float64
	dropUDP     bool // drop all UDP frames (isolation test: block the UDP carrier)
	// allow, when non-nil, is the set of IP protocols the path passes; every
	// other IPv4 frame is dropped (ARP always passes). {1} models a path that
	// carries only ICMP, like some Iran<->abroad links.
	allow map[int]bool
}

// isUDP reports whether an Ethernet frame carries IPv4 UDP.
func isUDP(f []byte) bool {
	if len(f) < 14+20 || binary.BigEndian.Uint16(f[12:14]) != 0x0800 {
		return false
	}
	return f[14+9] == 17
}

// geState is one direction's Gilbert-Elliott channel.
type geState struct {
	bad   bool
	until time.Time
}

// drop decides whether a frame is lost under the direction's loss model.
func (c *dirCfg) drop(g *geState, now time.Time) bool {
	if c.burstBadMs <= 0 {
		return c.loss > 0 && rand.Float64() < c.loss
	}
	if g.until.IsZero() {
		g.until = now
	}
	for !now.Before(g.until) {
		g.bad = !g.bad
		m := c.burstGoodMs
		if g.bad {
			m = c.burstBadMs
		}
		g.until = g.until.Add(time.Duration(rand.ExpFloat64() * m * float64(time.Millisecond)))
	}
	p := c.loss
	if g.bad {
		p = c.burstLoss
	}
	return p > 0 && rand.Float64() < p
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

// flowKey returns a per-flow key for an IPv4 frame, the way a DPI box that
// polices each flow would see it: the 5-tuple for TCP/UDP, (src, dst, echo id)
// for ICMP echo, (src, dst, key) for keyed GRE, and (src, dst, proto) for any
// other IP protocol — which is why several links over a portless protocol are
// still one flow to such a box. 0 = not IPv4.
func flowKey(f []byte) uint64 {
	if len(f) < 14+20 || binary.BigEndian.Uint16(f[12:14]) != 0x0800 {
		return 0
	}
	ip := f[14:]
	proto := ip[9]
	ihl := int(ip[0]&0x0f) * 4
	key := append([]byte{proto}, ip[12:20]...)
	switch {
	case (proto == 6 || proto == 17) && len(ip) >= ihl+4:
		key = append(key, ip[ihl:ihl+4]...) // ports
	case proto == 1 && len(ip) >= ihl+8 && (ip[ihl] == 0 || ip[ihl] == 8):
		key = append(key, ip[ihl+4:ihl+6]...) // echo identifier
	case proto == 47 && len(ip) >= ihl+8 && ip[ihl]&0x20 != 0:
		key = append(key, ip[ihl+4:ihl+8]...) // GRE key
	}
	h := uint64(14695981039346656037)
	for _, c := range key {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// ipProto returns the IPv4 protocol of a frame, or -1 for non-IPv4 (ARP etc.).
func ipProto(f []byte) int {
	if len(f) < 14+20 || binary.BigEndian.Uint16(f[12:14]) != 0x0800 {
		return -1
	}
	return int(f[14+9])
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
	ge := &geState{}
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
		if c.dropUDP && isUDP(buf[:n]) {
			stats.add(name, "udpblock", n)
			continue
		}
		if c.allow != nil {
			if p := ipProto(buf[:n]); p >= 0 && !c.allow[p] {
				stats.add(name, "blocked", n)
				continue
			}
		}
		if c.drop(ge, now) {
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
	loss := flag.Float64("loss", 0, "random (good-state) loss probability each way")
	burstLoss := flag.Float64("burstloss", 0, "loss probability in the bad state (enables bursty GE loss)")
	goodMs := flag.Float64("goodms", 150, "mean good-state duration, ms (bursty loss)")
	badMs := flag.Float64("badms", 0, "mean bad-state duration, ms (bursty loss; >0 enables it)")
	flowRate := flag.String("flowrate", "0", "per-flow policer rate each way")
	flowBurst := flag.Int("flowburst", 64<<10, "per-flow policer burst, bytes")
	dropUDP := flag.Bool("dropudp", false, "drop all UDP frames (block the UDP carrier)")
	allowP := flag.String("allow", "", "comma-separated IP protocol numbers the path passes (others dropped), e.g. 1 = ICMP only")
	flag.Parse()
	var allow map[int]bool
	if *allowP != "" {
		allow = map[int]bool{}
		for _, f := range strings.Split(*allowP, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				log.Fatalf("netem: -allow %q: %v", *allowP, err)
			}
			allow[n] = true
		}
	}
	fa, ia := openPacket(*a)
	fb, ib := openPacket(*b)
	ab := dirCfg{rate: parseRate(*rate), delay: *delay, queue: *queue, loss: *loss,
		flowRate: parseRate(*flowRate), flowBurst: *flowBurst,
		burstLoss: *burstLoss, burstGoodMs: *goodMs, burstBadMs: *badMs, dropUDP: *dropUDP, allow: allow}
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
