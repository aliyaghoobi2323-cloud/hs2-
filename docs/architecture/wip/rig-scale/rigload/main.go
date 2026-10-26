// rigload: the synthetic users of the big rig (R1 task 2,
// docs/architecture/wip/rig-scale). Standard library only.
//
//	rigload server -listen 127.0.0.1:5211,127.0.0.1:5212,0.0.0.0:5213
//	rigload client -spec spec.json        (one JSON object on stdout)
//
// A user is a source address (IPv4, or rotating addresses of one IPv6 /64).
// Each user has flows; every connection starts with a 12 byte header
// "RL" cmd 1 user(u32) flow(u32), so one server handles every port:
//
//	dn     bulk download: the server writes 32 KiB buffers until the connection
//	       ends; counted where it arrives (the client)
//	up     bulk upload: the client writes 32 KiB buffers; the server counts what
//	       it reads and every 100 ms sends back (its wall clock, bytes so far),
//	       so an upload is also counted where it arrives
//	probe  interactive: 32 B every 200 ms on one connection, echoed; one RTT
//	       per probe, sent on schedule whether or not the last one came back
//
// Not tied to a user address: "tun_flows" (dn/up to the exit's hs0 address,
// through the TUN) and "ping_tun" (ICMP echo every 200 ms to the exit's hs0
// address). Bytes are counted in 100 ms bins (both ends run on the same host,
// so the server's wall clock places an upload's bytes) and reported per
// measurement window; probe and ping samples belong to the window they were
// sent in, and one that has no answer 2 s after the run counts as lost.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	hdrLen   = 12
	bulkBuf  = 32 << 10
	binDur   = 100 * time.Millisecond
	grace    = 2 * time.Second
	dialWait = 10 * time.Second
)

func main() {
	log.SetFlags(log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rigload server -listen A,B,.. | rigload client -spec FILE")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		fs := flag.NewFlagSet("server", flag.ExitOnError)
		listen := fs.String("listen", "127.0.0.1:5211,127.0.0.1:5212", "addresses to serve, comma separated")
		fs.Parse(os.Args[2:])
		server(strings.Split(*listen, ","))
	case "client":
		fs := flag.NewFlagSet("client", flag.ExitOnError)
		spec := fs.String("spec", "", "spec JSON file")
		fs.Parse(os.Args[2:])
		b, err := os.ReadFile(*spec)
		if err != nil {
			log.Fatal(err)
		}
		var s Spec
		if err := json.Unmarshal(b, &s); err != nil {
			log.Fatalf("spec: %v", err)
		}
		out := runClient(&s)
		enc := json.NewEncoder(os.Stdout)
		if err := enc.Encode(out); err != nil {
			log.Fatal(err)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown role", os.Args[1])
		os.Exit(2)
	}
}

// ---------------------------------------------------------------- server

func server(addrs []string) {
	var wg sync.WaitGroup
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Fatalf("listen %s: %v", a, err)
		}
		log.Printf("serving %s", a)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					log.Printf("accept %s: %v", a, err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				go serveConn(c)
			}
		}()
	}
	wg.Wait()
}

func serveConn(c net.Conn) {
	defer c.Close()
	var h [hdrLen]byte
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 'R' || h[1] != 'L' {
		return
	}
	c.SetReadDeadline(time.Time{})
	switch h[2] {
	case 'D':
		buf := make([]byte, bulkBuf)
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	case 'U':
		var n atomic.Int64
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(binDur)
			defer t.Stop()
			var b [16]byte
			for {
				select {
				case <-done:
					return
				case <-t.C:
				}
				binary.BigEndian.PutUint64(b[0:], uint64(time.Now().UnixNano()))
				binary.BigEndian.PutUint64(b[8:], uint64(n.Load()))
				if _, err := c.Write(b[:]); err != nil {
					return
				}
			}
		}()
		buf := make([]byte, 64<<10)
		for {
			k, err := c.Read(buf)
			n.Add(int64(k))
			if err != nil {
				break
			}
		}
		close(done)
	case 'E':
		io.Copy(c, c)
	}
}

// ---------------------------------------------------------------- client

type Flow struct {
	Kind   string  `json:"kind"` // dn | up | probe
	N      int     `json:"n"`
	StartS float64 `json:"start_s"`
	StopS  float64 `json:"stop_s"` // 0: the end of the run
}

type User struct {
	ID    string `json:"id"`
	Src   string `json:"src"` // IPv4 source address; empty with V6
	V6    bool   `json:"v6"`  // every connection from a fresh address in V6Prefix/64
	Flows []Flow `json:"flows"`
}

type Window struct {
	Name string  `json:"name"`
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

type Spec struct {
	TotalS     float64  `json:"total_s"`
	Bulk       string   `json:"bulk"`  // edge user port for dn/up (IPv4)
	Echo       string   `json:"echo"`  // edge user port for probes (IPv4)
	Bulk6      string   `json:"bulk6"` // the same over IPv6
	Echo6      string   `json:"echo6"`
	V6Prefix   string   `json:"v6_prefix"` // e.g. "fd77:1::"
	TunBulk    string   `json:"tun_bulk"`  // exit's hs0 address:port
	TunPing    string   `json:"tun_ping"`  // exit's hs0 address
	PingTun    bool     `json:"ping_tun"`
	ProbeMs    int      `json:"probe_ms"`
	ProbeBytes int      `json:"probe_bytes"`
	PingMs     int      `json:"ping_ms"`
	Users      []User   `json:"users"`
	TunFlows   []Flow   `json:"tun_flows"`
	Windows    []Window `json:"windows"`
}

type bins struct{ b []atomic.Int64 }

func newBins(total float64) *bins {
	return &bins{b: make([]atomic.Int64, int((total+5)/binDur.Seconds())+1)}
}

func (z *bins) add(off time.Duration, n int) {
	if n <= 0 {
		return
	}
	i := int(off / binDur)
	if i < 0 {
		i = 0
	}
	if i >= len(z.b) {
		i = len(z.b) - 1
	}
	z.b[i].Add(int64(n))
}

func (z *bins) mbit(from, to float64) float64 {
	var s int64
	for i := int(math.Round(from * 10)); i < int(math.Round(to*10)) && i < len(z.b); i++ {
		s += z.b[i].Load()
	}
	if to <= from {
		return 0
	}
	return round3(float64(s) * 8 / (to - from) / 1e6)
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
func round1(f float64) float64 { return math.Round(f*10) / 10 }

// rtts: samples by send time (s from t0) and every send, for the lost count
type rtts struct {
	mu    sync.Mutex
	sendS []float64
	sampS []float64
	ms    []float64
}

func (r *rtts) sent(s float64) {
	r.mu.Lock()
	r.sendS = append(r.sendS, s)
	r.mu.Unlock()
}

func (r *rtts) got(sendS, ms float64) {
	r.mu.Lock()
	r.sampS = append(r.sampS, sendS)
	r.ms = append(r.ms, ms)
	r.mu.Unlock()
}

type Lat struct {
	N    int     `json:"n"`
	Sent int     `json:"sent"`
	Lost int     `json:"lost"`
	P1   float64 `json:"p1"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -1
	}
	i := int(math.Ceil(p*float64(len(v)))) - 1
	if i < 0 {
		i = 0
	}
	return round1(v[i])
}

func latOf(all []*rtts, from, to float64) *Lat {
	var v []float64
	sent := 0
	for _, r := range all {
		r.mu.Lock()
		for i, s := range r.sampS {
			if s >= from && s < to {
				v = append(v, r.ms[i])
			}
		}
		for _, s := range r.sendS {
			if s >= from && s < to {
				sent++
			}
		}
		r.mu.Unlock()
	}
	if sent == 0 {
		return nil
	}
	sort.Float64s(v)
	return &Lat{N: len(v), Sent: sent, Lost: sent - len(v),
		P1: pct(v, 0.01), P50: pct(v, 0.5), P90: pct(v, 0.9), P99: pct(v, 0.99), Max: pct(v, 1)}
}

type ustate struct {
	u         User
	dn, up    *bins
	probe     rtts
	conns     atomic.Int32 // connections made
	dialErrs  atomic.Int32
	shortEnds atomic.Int32 // bulk connections that ended before their stop
	probeErrs atomic.Int32
}

type client struct {
	s     *Spec
	t0    time.Time
	end   time.Time
	mu    sync.Mutex
	errs  map[string]int
	users []*ustate
	tun   *ustate
	ping  rtts
}

func (c *client) err(f string, a ...any) {
	m := fmt.Sprintf(f, a...)
	c.mu.Lock()
	c.errs[m]++
	c.mu.Unlock()
}

func (c *client) off() time.Duration { return time.Since(c.t0) }

func (c *client) at(s float64) time.Time { return c.t0.Add(time.Duration(s * float64(time.Second))) }

func sleepUntil(t time.Time) {
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}

func randV6(prefix string) net.IP {
	ip := net.ParseIP(prefix).To16()
	if ip == nil {
		return nil
	}
	out := make(net.IP, 16)
	copy(out, ip[:8])
	rand.Read(out[8:])
	if out[15] == 0 {
		out[15] = 1
	}
	return out
}

func (c *client) dial(us *ustate, port string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialWait}
	addr := port
	switch {
	case us == c.tun:
	case us.u.V6:
		d.LocalAddr = &net.TCPAddr{IP: randV6(c.s.V6Prefix)}
	default:
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(us.u.Src)}
	}
	return d.Dial("tcp", addr)
}

func header(cmd byte, user, flow int) []byte {
	h := make([]byte, hdrLen)
	h[0], h[1], h[2], h[3] = 'R', 'L', cmd, 1
	binary.BigEndian.PutUint32(h[4:], uint32(user))
	binary.BigEndian.PutUint32(h[8:], uint32(flow))
	return h
}

func (c *client) bulk(us *ustate, ui, fi int, f Flow, addr string) {
	stop := c.end
	if f.StopS > 0 {
		stop = c.at(f.StopS)
	}
	sleepUntil(c.at(f.StartS))
	for time.Until(stop) > 100*time.Millisecond {
		conn, err := c.dial(us, addr)
		if err != nil {
			us.dialErrs.Add(1)
			c.err("dial %s %s: %v", f.Kind, addr, err)
			time.Sleep(time.Second)
			continue
		}
		us.conns.Add(1)
		conn.SetDeadline(stop)
		cmd := byte('D')
		if f.Kind == "up" {
			cmd = 'U'
		}
		if _, err := conn.Write(header(cmd, ui, fi)); err != nil {
			conn.Close()
			c.err("header %s: %v", f.Kind, err)
			continue
		}
		if f.Kind == "dn" {
			buf := make([]byte, 64<<10)
			for {
				n, err := conn.Read(buf)
				us.dn.add(c.off(), n)
				if err != nil {
					break
				}
			}
		} else {
			go func() {
				buf := make([]byte, bulkBuf)
				for {
					if _, err := conn.Write(buf); err != nil {
						return
					}
				}
			}()
			var rep [16]byte
			var last int64
			t0 := c.t0.UnixNano()
			for {
				if _, err := io.ReadFull(conn, rep[:]); err != nil {
					break
				}
				wall := int64(binary.BigEndian.Uint64(rep[0:]))
				cum := int64(binary.BigEndian.Uint64(rep[8:]))
				us.up.add(time.Duration(wall-t0), int(cum-last))
				last = cum
			}
		}
		conn.Close()
		if time.Until(stop) > 200*time.Millisecond {
			us.shortEnds.Add(1)
			c.err("%s connection ended %.1fs before its stop", f.Kind, time.Until(stop).Seconds())
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (c *client) probe(us *ustate, ui, fi int, f Flow, addr string) {
	stop := c.end
	if f.StopS > 0 {
		stop = c.at(f.StopS)
	}
	every := time.Duration(c.s.ProbeMs) * time.Millisecond
	size := c.s.ProbeBytes
	if size < 8 {
		size = 8
	}
	// a random phase, so hundreds of probes do not fire together
	var ph [2]byte
	rand.Read(ph[:])
	sleepUntil(c.at(f.StartS).Add(time.Duration(binary.BigEndian.Uint16(ph[:])) * every / 65536))
	conn, err := c.dial(us, addr)
	if err != nil {
		us.dialErrs.Add(1)
		c.err("dial probe %s: %v", addr, err)
		return
	}
	us.conns.Add(1)
	defer conn.Close()
	if _, err := conn.Write(header('E', ui, fi)); err != nil {
		c.err("probe header: %v", err)
		return
	}
	var mu sync.Mutex
	sentAt := map[uint32]time.Duration{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.SetReadDeadline(stop.Add(grace))
		b := make([]byte, size)
		for {
			if _, err := io.ReadFull(conn, b); err != nil {
				return
			}
			now := c.off()
			seq := binary.BigEndian.Uint32(b)
			mu.Lock()
			s, ok := sentAt[seq]
			delete(sentAt, seq)
			mu.Unlock()
			if ok {
				us.probe.got(s.Seconds(), float64(now-s)/1e6)
			}
		}
	}()
	b := make([]byte, size)
	t := time.NewTicker(every)
	defer t.Stop()
	for seq := uint32(0); time.Now().Before(stop); seq++ {
		binary.BigEndian.PutUint32(b, seq)
		s := c.off()
		mu.Lock()
		sentAt[seq] = s
		mu.Unlock()
		us.probe.sent(s.Seconds())
		conn.SetWriteDeadline(stop.Add(grace))
		if _, err := conn.Write(b); err != nil {
			us.probeErrs.Add(1)
			c.err("probe write: %v", err)
			break
		}
		<-t.C
	}
	<-done
}

func csum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func (c *client) pingTun() {
	dst := net.ParseIP(c.s.TunPing)
	pc, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		c.err("icmp socket: %v", err)
		return
	}
	defer pc.Close()
	id := uint16(os.Getpid())
	every := time.Duration(c.s.PingMs) * time.Millisecond
	var mu sync.Mutex
	sentAt := map[uint16]time.Duration{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pc.SetReadDeadline(c.end.Add(grace))
		b := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			now := c.off()
			if n < 8 || b[0] != 0 || binary.BigEndian.Uint16(b[4:]) != id {
				continue
			}
			seq := binary.BigEndian.Uint16(b[6:])
			mu.Lock()
			s, ok := sentAt[seq]
			delete(sentAt, seq)
			mu.Unlock()
			if ok {
				c.ping.got(s.Seconds(), float64(now-s)/1e6)
			}
		}
	}()
	msg := make([]byte, 8+56)
	t := time.NewTicker(every)
	defer t.Stop()
	for seq := uint16(0); time.Now().Before(c.end); seq++ {
		msg[0], msg[1], msg[2], msg[3] = 8, 0, 0, 0
		binary.BigEndian.PutUint16(msg[4:], id)
		binary.BigEndian.PutUint16(msg[6:], seq)
		binary.BigEndian.PutUint64(msg[8:], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint16(msg[2:], csum(msg))
		s := c.off()
		mu.Lock()
		sentAt[seq] = s
		mu.Unlock()
		c.ping.sent(s.Seconds())
		if _, err := pc.WriteTo(msg, &net.IPAddr{IP: dst}); err != nil {
			c.err("icmp send: %v", err)
		}
		<-t.C
	}
	<-done
}

type UserOut struct {
	ID        string             `json:"id"`
	Src       string             `json:"src"`
	Flows     []Flow             `json:"flows"`
	Dn        map[string]float64 `json:"dn_mbit"`
	Up        map[string]float64 `json:"up_mbit"`
	Probe     map[string]*Lat    `json:"probe,omitempty"`
	Conns     int32              `json:"conns"`
	DialErrs  int32              `json:"dial_errs,omitempty"`
	ShortEnds int32              `json:"short_ends,omitempty"`
	ProbeErrs int32              `json:"probe_errs,omitempty"`
}

type Out struct {
	T0       int64              `json:"t0_unix_ms"`
	TotalS   float64            `json:"total_s"`
	Windows  []Window           `json:"windows"`
	Users    []UserOut          `json:"users"`
	DnSum    map[string]float64 `json:"dn_sum_mbit"`
	UpSum    map[string]float64 `json:"up_sum_mbit"`
	ProbeAll map[string]*Lat    `json:"probe_all,omitempty"`
	Tun      *UserOut           `json:"tun,omitempty"`
	PingTun  map[string]*Lat    `json:"ping_tun,omitempty"`
	Dn1s     []float64          `json:"dn_1s"` // users' downloads together, per second from t0
	Up1s     []float64          `json:"up_1s"`
	Errs     map[string]int     `json:"errs,omitempty"`
}

func runClient(s *Spec) *Out {
	if s.ProbeMs <= 0 {
		s.ProbeMs = 200
	}
	if s.ProbeBytes <= 0 {
		s.ProbeBytes = 32
	}
	if s.PingMs <= 0 {
		s.PingMs = 200
	}
	if len(s.Windows) == 0 {
		s.Windows = []Window{{"all", 0, s.TotalS}}
	}
	c := &client{s: s, errs: map[string]int{}}
	c.t0 = time.Now()
	c.end = c.at(s.TotalS)
	for _, u := range s.Users {
		c.users = append(c.users, &ustate{u: u, dn: newBins(s.TotalS), up: newBins(s.TotalS)})
	}
	c.tun = &ustate{u: User{ID: "tun", Flows: s.TunFlows}, dn: newBins(s.TotalS), up: newBins(s.TotalS)}
	var wg sync.WaitGroup
	start := func(us *ustate, ui int, bulk, echo string) {
		for fi, f := range us.u.Flows {
			for k := 0; k < max(f.N, 1); k++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					switch f.Kind {
					case "dn", "up":
						c.bulk(us, ui, fi*1000+k, f, bulk)
					case "probe":
						c.probe(us, ui, fi*1000+k, f, echo)
					default:
						c.err("unknown flow kind %q", f.Kind)
					}
				}()
			}
		}
	}
	for i, us := range c.users {
		bulk, echo := s.Bulk, s.Echo
		if us.u.V6 {
			bulk, echo = s.Bulk6, s.Echo6
		}
		start(us, i+1, bulk, echo)
	}
	if len(s.TunFlows) > 0 {
		start(c.tun, 0, s.TunBulk, s.TunBulk)
	}
	if s.PingTun {
		wg.Add(1)
		go func() { defer wg.Done(); c.pingTun() }()
	}
	wg.Wait()

	out := &Out{T0: c.t0.UnixMilli(), TotalS: s.TotalS, Windows: s.Windows,
		DnSum: map[string]float64{}, UpSum: map[string]float64{}, Errs: c.errs}
	uo := func(us *ustate) UserOut {
		o := UserOut{ID: us.u.ID, Src: us.u.Src, Flows: us.u.Flows,
			Dn: map[string]float64{}, Up: map[string]float64{},
			Conns: us.conns.Load(), DialErrs: us.dialErrs.Load(), ShortEnds: us.shortEnds.Load(), ProbeErrs: us.probeErrs.Load()}
		if us.u.V6 {
			o.Src = s.V6Prefix + "/64"
		}
		for _, w := range s.Windows {
			o.Dn[w.Name] = us.dn.mbit(w.From, w.To)
			o.Up[w.Name] = us.up.mbit(w.From, w.To)
			if l := latOf([]*rtts{&us.probe}, w.From, w.To); l != nil {
				if o.Probe == nil {
					o.Probe = map[string]*Lat{}
				}
				o.Probe[w.Name] = l
			}
		}
		return o
	}
	var all []*rtts
	for _, us := range c.users {
		out.Users = append(out.Users, uo(us))
		all = append(all, &us.probe)
	}
	for _, w := range s.Windows {
		var d, u float64
		for _, x := range out.Users {
			d += x.Dn[w.Name]
			u += x.Up[w.Name]
		}
		out.DnSum[w.Name], out.UpSum[w.Name] = round3(d), round3(u)
		if l := latOf(all, w.From, w.To); l != nil {
			if out.ProbeAll == nil {
				out.ProbeAll = map[string]*Lat{}
			}
			out.ProbeAll[w.Name] = l
		}
		if s.PingTun {
			if out.PingTun == nil {
				out.PingTun = map[string]*Lat{}
			}
			out.PingTun[w.Name] = latOf([]*rtts{&c.ping}, w.From, w.To)
		}
	}
	if len(s.TunFlows) > 0 {
		t := uo(c.tun)
		out.Tun = &t
	}
	for sec := 0; float64(sec) < s.TotalS; sec++ {
		var d, u float64
		for _, us := range c.users {
			d += us.dn.mbit(float64(sec), float64(sec+1))
			u += us.up.mbit(float64(sec), float64(sec+1))
		}
		out.Dn1s = append(out.Dn1s, round1(d))
		out.Up1s = append(out.Up1s, round1(u))
	}
	return out
}
