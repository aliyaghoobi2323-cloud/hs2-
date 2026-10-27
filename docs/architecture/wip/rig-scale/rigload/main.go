// rigload: the synthetic users of the big rig (R1 task 2,
// docs/architecture/wip/rig-scale). Standard library only.
//
//	rigload server -listen 127.0.0.1:5211,127.0.0.1:5212,:5213
//	rigload client -spec spec.json        (one JSON object on stdout)
//
// A user is a source address. A user takes one of two paths:
//
//	port  through the edge's user ports (hs2 relays the TCP connection): an
//	      IPv4 address, or with v6 one IPv6 /64, a fresh address in it for
//	      every connection
//	tun   through the TUN (hs0), from its own inner address (src; IPv4, or
//	      IPv6 with v6), to the exit's hs0 address; empty src: hs0's own
//
// Every connection starts with a 12 byte header "RL" cmd 1 user(u32) flow(u32),
// so one server handles every port. Flows:
//
//	dn     bulk download: the server writes 32 KiB buffers until the connection
//	       ends; counted where it arrives (the client)
//	up     bulk upload: the client writes 32 KiB buffers; the server counts what
//	       it reads and every 100 ms sends back (its wall clock, bytes so far),
//	       so an upload is also counted where it arrives
//	probe  interactive: 32 B every 200 ms on one connection, echoed; one RTT
//	       per probe, sent on schedule whether or not the last one came back
//	ping   tun users only: ICMP (ICMPv6 with v6) echo every 200 ms to the
//	       exit's hs0 address, from the user's inner address
//
// Bytes are counted in 100 ms bins (both ends run on the same host, so the
// server's wall clock places an upload's bytes) and reported per measurement
// window; probe and ping samples belong to the window they were sent in, and
// one that has no answer 2 s after the run counts as lost.
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
		if err := s.check(); err != nil {
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
	Kind   string  `json:"kind"` // dn | up | probe | ping
	N      int     `json:"n"`
	StartS float64 `json:"start_s"`
	StopS  float64 `json:"stop_s"` // 0: the end of the run
}

type User struct {
	ID    string `json:"id"`
	Path  string `json:"path"` // port (default) | tun
	Src   string `json:"src"`  // source address; port+v6: empty (rotating); tun: empty = hs0's own
	V6    bool   `json:"v6"`   // IPv6 (port: rotating over V6Prefix/64)
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
	TunBulk    string   `json:"tun_bulk"`  // exit's hs0 address:port (dn, up and probe)
	TunBulk6   string   `json:"tun_bulk6"`
	TunPing    string   `json:"tun_ping"` // exit's hs0 address
	TunPing6   string   `json:"tun_ping6"`
	ProbeMs    int      `json:"probe_ms"`
	ProbeBytes int      `json:"probe_bytes"`
	PingMs     int      `json:"ping_ms"`
	Users      []User   `json:"users"`
	Windows    []Window `json:"windows"`
}

func (s *Spec) check() error {
	for _, u := range s.Users {
		if u.Path != "" && u.Path != "port" && u.Path != "tun" {
			return fmt.Errorf("user %s: path %q", u.ID, u.Path)
		}
		for _, f := range u.Flows {
			switch f.Kind {
			case "dn", "up", "probe":
			case "ping":
				if u.Path != "tun" {
					return fmt.Errorf("user %s: ping on the port path (ICMP does not cross user ports)", u.ID)
				}
			default:
				return fmt.Errorf("user %s: flow kind %q", u.ID, f.Kind)
			}
		}
	}
	return nil
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
	idx       int
	dn, up    *bins
	probe     rtts
	ping      rtts
	conns     atomic.Int32 // connections made
	dialErrs  atomic.Int32
	shortEnds atomic.Int32 // bulk connections that ended before their stop
	probeErrs atomic.Int32
}

func (us *ustate) tun() bool { return us.u.Path == "tun" }

type client struct {
	s     *Spec
	t0    time.Time
	end   time.Time
	mu    sync.Mutex
	errs  map[string]int
	users []*ustate
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

// target: where a flow of this user goes (bulk/echo for TCP flows)
func (c *client) target(us *ustate, probe bool) string {
	s := c.s
	switch {
	case us.tun() && us.u.V6:
		return s.TunBulk6
	case us.tun():
		return s.TunBulk
	case us.u.V6 && probe:
		return s.Echo6
	case us.u.V6:
		return s.Bulk6
	case probe:
		return s.Echo
	}
	return s.Bulk
}

func (c *client) dial(us *ustate, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialWait}
	switch {
	case us.u.Src != "":
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(us.u.Src)}
	case !us.tun() && us.u.V6:
		d.LocalAddr = &net.TCPAddr{IP: randV6(c.s.V6Prefix)}
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

func (c *client) stopOf(f Flow) time.Time {
	if f.StopS > 0 {
		return c.at(f.StopS)
	}
	return c.end
}

func (c *client) bulk(us *ustate, fi int, f Flow) {
	stop := c.stopOf(f)
	addr := c.target(us, false)
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
		if _, err := conn.Write(header(cmd, us.idx, fi)); err != nil {
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

// phase: a random start within one period, so hundreds of probes do not fire together
func phase(every time.Duration) time.Duration {
	var ph [2]byte
	rand.Read(ph[:])
	return time.Duration(binary.BigEndian.Uint16(ph[:])) * every / 65536
}

func (c *client) probe(us *ustate, fi int, f Flow) {
	stop := c.stopOf(f)
	addr := c.target(us, true)
	every := time.Duration(c.s.ProbeMs) * time.Millisecond
	size := c.s.ProbeBytes
	if size < 8 {
		size = 8
	}
	sleepUntil(c.at(f.StartS).Add(phase(every)))
	conn, err := c.dial(us, addr)
	if err != nil {
		us.dialErrs.Add(1)
		c.err("dial probe %s: %v", addr, err)
		return
	}
	us.conns.Add(1)
	defer conn.Close()
	if _, err := conn.Write(header('E', us.idx, fi)); err != nil {
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

var pingIDs atomic.Uint32

// ping: ICMP echo from the user's inner address (ICMPv6 with v6; the kernel
// fills the ICMPv6 checksum), each ping flow with its own echo id
func (c *client) ping(us *ustate, f Flow) {
	stop := c.stopOf(f)
	netw, dst, bind, req, rep := "ip4:icmp", c.s.TunPing, "0.0.0.0", byte(8), byte(0)
	if us.u.V6 {
		netw, dst, bind, req, rep = "ip6:ipv6-icmp", c.s.TunPing6, "::", 128, 129
	}
	if us.u.Src != "" {
		bind = us.u.Src
	}
	pc, err := net.ListenPacket(netw, bind)
	if err != nil {
		c.err("icmp socket %s %s: %v", netw, bind, err)
		return
	}
	defer pc.Close()
	id := uint16(os.Getpid()) + uint16(pingIDs.Add(1))*7919
	every := time.Duration(c.s.PingMs) * time.Millisecond
	var mu sync.Mutex
	sentAt := map[uint16]time.Duration{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pc.SetReadDeadline(stop.Add(grace))
		b := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			now := c.off()
			if n < 8 || b[0] != rep || binary.BigEndian.Uint16(b[4:]) != id {
				continue
			}
			seq := binary.BigEndian.Uint16(b[6:])
			mu.Lock()
			s, ok := sentAt[seq]
			delete(sentAt, seq)
			mu.Unlock()
			if ok {
				us.ping.got(s.Seconds(), float64(now-s)/1e6)
			}
		}
	}()
	to := &net.IPAddr{IP: net.ParseIP(dst)}
	sleepUntil(c.at(f.StartS).Add(phase(every)))
	msg := make([]byte, 8+56)
	t := time.NewTicker(every)
	defer t.Stop()
	for seq := uint16(0); time.Now().Before(stop); seq++ {
		msg[0], msg[1], msg[2], msg[3] = req, 0, 0, 0
		binary.BigEndian.PutUint16(msg[4:], id)
		binary.BigEndian.PutUint16(msg[6:], seq)
		binary.BigEndian.PutUint64(msg[8:], uint64(time.Now().UnixNano()))
		if !us.u.V6 {
			binary.BigEndian.PutUint16(msg[2:], csum(msg))
		}
		s := c.off()
		mu.Lock()
		sentAt[seq] = s
		mu.Unlock()
		us.ping.sent(s.Seconds())
		if _, err := pc.WriteTo(msg, to); err != nil {
			c.err("icmp send: %v", err)
		}
		<-t.C
	}
	<-done
}

type UserOut struct {
	ID        string             `json:"id"`
	Path      string             `json:"path"`
	Src       string             `json:"src"`
	Flows     []Flow             `json:"flows"`
	Dn        map[string]float64 `json:"dn_mbit"`
	Up        map[string]float64 `json:"up_mbit"`
	Probe     map[string]*Lat    `json:"probe,omitempty"`
	Ping      map[string]*Lat    `json:"ping,omitempty"`
	Conns     int32              `json:"conns"`
	DialErrs  int32              `json:"dial_errs,omitempty"`
	ShortEnds int32              `json:"short_ends,omitempty"`
	ProbeErrs int32              `json:"probe_errs,omitempty"`
	// a bulk user's own goodput per second from t0 (the judge, R1 task 17:
	// the existing bulk user's first seconds after a heavy user joins)
	Dn1s []float64 `json:"dn_1s,omitempty"`
	Up1s []float64 `json:"up_1s,omitempty"`
}

type Out struct {
	T0       int64              `json:"t0_unix_ms"`
	TotalS   float64            `json:"total_s"`
	Windows  []Window           `json:"windows"`
	Users    []UserOut          `json:"users"`
	DnSum    map[string]float64 `json:"dn_sum_mbit"` // port users
	UpSum    map[string]float64 `json:"up_sum_mbit"`
	TunDnSum map[string]float64 `json:"tun_dn_sum_mbit"` // tun users (inner goodput)
	TunUpSum map[string]float64 `json:"tun_up_sum_mbit"`
	ProbeAll map[string]*Lat    `json:"probe_all,omitempty"` // port users' probes together
	ProbeTun map[string]*Lat    `json:"probe_tun,omitempty"` // tun users' TCP probes together
	PingTun  map[string]*Lat    `json:"ping_tun,omitempty"`  // every ICMP ping together
	Dn1s     []float64          `json:"dn_1s"`               // every user's downloads, per second from t0
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
	for i, u := range s.Users {
		if u.Path == "" {
			u.Path = "port"
		}
		c.users = append(c.users, &ustate{u: u, idx: i + 1, dn: newBins(s.TotalS), up: newBins(s.TotalS)})
	}
	var wg sync.WaitGroup
	for _, us := range c.users {
		for fi, f := range us.u.Flows {
			for k := 0; k < max(f.N, 1); k++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					switch f.Kind {
					case "dn", "up":
						c.bulk(us, fi*1000+k, f)
					case "probe":
						c.probe(us, fi*1000+k, f)
					case "ping":
						c.ping(us, f)
					}
				}()
			}
		}
	}
	wg.Wait()
	if p := os.Getenv("RIGLOAD_PROBE_DUMP"); p != "" {
		c.dump(p)
	}

	out := &Out{T0: c.t0.UnixMilli(), TotalS: s.TotalS, Windows: s.Windows,
		DnSum: map[string]float64{}, UpSum: map[string]float64{},
		TunDnSum: map[string]float64{}, TunUpSum: map[string]float64{}, Errs: c.errs}
	lat := func(r []*rtts, w Window, m *map[string]*Lat) {
		if l := latOf(r, w.From, w.To); l != nil {
			if *m == nil {
				*m = map[string]*Lat{}
			}
			(*m)[w.Name] = l
		}
	}
	var probes, tprobes, pings []*rtts
	for _, us := range c.users {
		o := UserOut{ID: us.u.ID, Path: us.u.Path, Src: us.u.Src, Flows: us.u.Flows,
			Dn: map[string]float64{}, Up: map[string]float64{},
			Conns: us.conns.Load(), DialErrs: us.dialErrs.Load(), ShortEnds: us.shortEnds.Load(), ProbeErrs: us.probeErrs.Load()}
		switch {
		case !us.tun() && us.u.V6:
			o.Src = s.V6Prefix + "/64"
		case us.tun() && us.u.Src == "":
			o.Src = "hs0"
		}
		for _, f := range us.u.Flows {
			if f.Kind != "dn" && f.Kind != "up" {
				continue
			}
			for sec := 0; float64(sec) < s.TotalS; sec++ {
				o.Dn1s = append(o.Dn1s, round1(us.dn.mbit(float64(sec), float64(sec+1))))
				o.Up1s = append(o.Up1s, round1(us.up.mbit(float64(sec), float64(sec+1))))
			}
			break
		}
		for _, w := range s.Windows {
			o.Dn[w.Name] = us.dn.mbit(w.From, w.To)
			o.Up[w.Name] = us.up.mbit(w.From, w.To)
			lat([]*rtts{&us.probe}, w, &o.Probe)
			lat([]*rtts{&us.ping}, w, &o.Ping)
		}
		out.Users = append(out.Users, o)
		pings = append(pings, &us.ping)
		if us.tun() {
			tprobes = append(tprobes, &us.probe)
		} else {
			probes = append(probes, &us.probe)
		}
	}
	for _, w := range s.Windows {
		var d, u, td, tu float64
		for i, x := range out.Users {
			if c.users[i].tun() {
				td += x.Dn[w.Name]
				tu += x.Up[w.Name]
			} else {
				d += x.Dn[w.Name]
				u += x.Up[w.Name]
			}
		}
		out.DnSum[w.Name], out.UpSum[w.Name] = round3(d), round3(u)
		out.TunDnSum[w.Name], out.TunUpSum[w.Name] = round3(td), round3(tu)
		lat(probes, w, &out.ProbeAll)
		lat(tprobes, w, &out.ProbeTun)
		lat(pings, w, &out.PingTun)
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

// dump (lab only, round 2 step 1): every probe's and ping's send time (s
// from t0) and, for those answered, send time and RTT (ms); a probe's
// sequence number is its place among the sends.
func (c *client) dump(path string) {
	type rd struct {
		Send []float64 `json:"send"`
		Samp []float64 `json:"samp"`
		Ms   []float64 `json:"ms"`
	}
	type ud struct {
		Idx   int    `json:"idx"`
		ID    string `json:"id"`
		Path  string `json:"path"`
		Probe *rd    `json:"probe,omitempty"`
		Ping  *rd    `json:"ping,omitempty"`
	}
	of := func(r *rtts) *rd {
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.sendS) == 0 {
			return nil
		}
		return &rd{append([]float64(nil), r.sendS...), append([]float64(nil), r.sampS...), append([]float64(nil), r.ms...)}
	}
	out := struct {
		T0    int64 `json:"t0_ns"`
		Users []ud  `json:"users"`
	}{T0: c.t0.UnixNano()}
	for _, us := range c.users {
		u := ud{Idx: us.idx, ID: us.u.ID, Path: us.u.Path, Probe: of(&us.probe), Ping: of(&us.ping)}
		if u.Probe != nil || u.Ping != nil {
			out.Users = append(out.Users, u)
		}
	}
	if b, err := json.Marshal(out); err == nil {
		os.WriteFile(path, b, 0o644)
	}
}
