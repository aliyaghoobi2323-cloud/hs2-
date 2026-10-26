//go:build linux

package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Flow table

func seqIDs() func() uint64 {
	var n uint64
	return func() uint64 { n++; return n }
}

func collect(tr *tracker, t int64) map[uint64]rec {
	got := map[uint64]rec{}
	tr.flush(t, func(r rec) { got[r.f] = r })
	return got
}

// TestTrackerIntervalDeltas: the bytes of an interval are differences of
// cumulative counters; bytes from before start never appear; an idle flow
// writes nothing; a counter that goes backwards never gives a negative value;
// a closed flow writes its last bytes once and then stops appearing.
func TestTrackerIntervalDeltas(t *testing.T) {
	tr := newTracker(seqIDs())
	tr.baseline([]sample{{key: 101, up: 1000, dn: 5000}, {key: 102, up: 10, dn: 10}})
	idA, idB := tr.flows[101].id, tr.flows[102].id

	tr.observe(100, []sample{{101, 1100, 5200}, {102, 10, 10}, {103, 50, 60}})
	got := collect(tr, 100)
	idC := tr.flows[103].id
	if len(got) != 2 || got[idA] != (rec{idA, 100, 100, 200}) || got[idC] != (rec{idC, 100, 50, 60}) {
		t.Fatalf("tick 1: %+v", got)
	}
	if _, ok := got[idB]; ok {
		t.Fatalf("idle flow written: %+v", got)
	}

	// Counter of A goes backwards (up) while dn grows: up must read 0, never negative.
	tr.observe(200, []sample{{101, 1050, 5300}, {102, 10, 25}, {103, 50, 60}})
	got = collect(tr, 200)
	if got[idA] != (rec{idA, 200, 0, 100}) || got[idB] != (rec{idB, 200, 0, 15}) || len(got) != 2 {
		t.Fatalf("tick 2: %+v", got)
	}
	if tr.backwards != 1 {
		t.Fatalf("backwards = %d", tr.backwards)
	}
	// Back above the old high-water mark: only the growth past 1100 counts.
	tr.observe(300, []sample{{101, 1150, 5300}, {103, 50, 60}})
	got = collect(tr, 300)
	if got[idA] != (rec{idA, 300, 50, 0}) || len(got) != 1 {
		t.Fatalf("tick 3: %+v", got)
	}

	// C closes with final counters: its last bytes come once, then never again.
	tr.closed(sample{103, 80, 90})
	got = collect(tr, 400)
	if got[idC] != (rec{idC, 400, 30, 30}) || len(got) != 1 {
		t.Fatalf("close: %+v", got)
	}
	for i := int64(5); i < 10; i++ {
		tr.observe(i*100, []sample{{101, 1150 + uint64(i), 5300}})
		if r, ok := collect(tr, i*100)[idC]; ok {
			t.Fatalf("closed flow written again: %+v", r)
		}
	}
	if tr.has(103) {
		t.Fatal("closed flow still in the table")
	}

	// A flow that lived and died between two dumps is written whole.
	tr.closed(sample{104, 7, 9})
	got = collect(tr, 1000)
	var short []rec
	for _, r := range got {
		if r.up == 7 && r.dn == 9 {
			short = append(short, r)
		}
	}
	if len(short) != 1 {
		t.Fatalf("short flow: %+v", got)
	}

	// B missing from the dumps without a close event: nothing written, and
	// dropped once the wait for its close event runs out.
	tr.ttl = 1000
	for i := int64(11); i <= 25; i++ {
		tr.observe(i*100, []sample{{101, 2000, 6000}})
		if r, ok := collect(tr, i*100)[idB]; ok {
			t.Fatalf("missing flow written: %+v", r)
		}
	}
	if tr.has(102) || tr.vanished != 1 {
		t.Fatalf("missing flow kept: has=%v vanished=%d", tr.has(102), tr.vanished)
	}
}

// TestTrackerRandom: random monotone counters, random births and deaths,
// close events and lost close events. For every flow the written bytes sum to
// (last counter - counter at start), every line is non-negative and nonzero,
// and no line follows a close.
func TestTrackerRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	type sock struct {
		up, dn, up0, dn0 uint64
		alive            bool
	}
	socks := map[uint64]*sock{}
	tr := newTracker(func() uint64 { return rng.Uint64() })
	var base []sample
	for k := uint64(1); k <= 50; k++ {
		s := &sock{up: rng.Uint64() % 1e6, dn: rng.Uint64() % 1e6, alive: true}
		s.up0, s.dn0 = s.up, s.dn
		socks[k] = s
		base = append(base, sample{k, s.up, s.dn})
	}
	tr.baseline(base)
	keyOf := map[uint64]uint64{}
	for k := range socks {
		keyOf[tr.flows[k].id] = k
	}
	sumUp, sumDn := map[uint64]uint64{}, map[uint64]uint64{}
	closedAt := map[uint64]int64{}
	next := uint64(51)
	for tick := int64(1); tick <= 400; tick++ {
		now := tick * 100
		for k, s := range socks {
			if !s.alive {
				continue
			}
			if rng.Intn(3) == 0 {
				s.up += uint64(rng.Intn(5000))
			}
			if rng.Intn(3) == 0 {
				s.dn += uint64(rng.Intn(50000))
			}
			if rng.Intn(40) == 0 { // dies; its close event arrives
				s.alive = false
				tr.closed(sample{k, s.up, s.dn})
				closedAt[k] = now
			}
		}
		if rng.Intn(4) == 0 { // a new flow, sometimes dead before any dump
			k := next
			next++
			s := &sock{up: uint64(rng.Intn(3000)), dn: uint64(rng.Intn(3000)), alive: true}
			socks[k] = s
			if rng.Intn(3) == 0 {
				s.alive = false
				tr.closed(sample{k, s.up, s.dn})
				closedAt[k] = now
			}
		}
		var ss []sample
		for k, s := range socks {
			if s.alive {
				ss = append(ss, sample{k, s.up, s.dn})
			}
		}
		rng.Shuffle(len(ss), func(i, j int) { ss[i], ss[j] = ss[j], ss[i] })
		tr.observe(now, ss)
		for k := range socks {
			if f := tr.flows[k]; f != nil {
				keyOf[f.id] = k
			}
		}
		tr.flush(now, func(r rec) {
			k, ok := keyOf[r.f]
			if !ok {
				t.Fatalf("unknown id %d", r.f)
			}
			if r.up == 0 && r.dn == 0 {
				t.Fatalf("line for a flow that moved nothing: %+v", r)
			}
			if c, ok := closedAt[k]; ok && r.t > c {
				t.Fatalf("flow %d written at %d after its close at %d", k, r.t, c)
			}
			sumUp[k] += r.up
			sumDn[k] += r.dn
		})
	}
	for k, s := range socks {
		if sumUp[k] != s.up-s.up0 || sumDn[k] != s.dn-s.dn0 {
			t.Fatalf("flow %d: wrote up %d dn %d, want %d %d", k, sumUp[k], sumDn[k], s.up-s.up0, s.dn-s.dn0)
		}
	}
}

func TestParsePorts(t *testing.T) {
	ps, err := parsePorts(" 9443, 8443,8443 ,")
	if err != nil || len(ps) != 2 || ps[0] != 8443 || ps[1] != 9443 {
		t.Fatalf("got %v %v", ps, err)
	}
	for _, bad := range []string{"", "22", "8443,22", "x", "0", "70000"} {
		if _, err := parsePorts(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"mode":"dial","forward_ports":"8443,2053","shared_key":"00ff"}`), 0o600)
	ps, err = portsFromConfig(p)
	if err != nil || len(ps) != 2 || ps[0] != 2053 || ps[1] != 8443 {
		t.Fatalf("config: %v %v", ps, err)
	}
}

func TestBytecodeShape(t *testing.T) {
	bc := bytecode([]uint16{8443, 2053, 443})
	// S_EQ(8) JMP(4) S_EQ(8) JMP(4) S_EQ(8)
	if len(bc) != 32 || bc[0] != bcSEq || bc[8] != bcJmp || bc[12] != bcSEq || bc[20] != bcJmp || bc[24] != bcSEq {
		t.Fatalf("bytecode % x", bc)
	}
	if ne.Uint16(bc[10:]) != 24 || ne.Uint16(bc[22:]) != 12 { // jump to the end
		t.Fatalf("jumps % x", bc)
	}
}

// ---------------------------------------------------------------------------
// Real sockets on loopback

type srv struct {
	ln   net.Listener
	port uint16
}

// listen starts a server that answers each request of n bytes with resp(n)
// bytes, and closes the connection when the client does.
func listen(t *testing.T, network, addr string, resp func(int) int) *srv {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("listen %s: %v", network, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					var hdr [4]byte
					if _, err := io.ReadFull(r, hdr[:]); err != nil {
						return
					}
					n := int(ne.Uint32(hdr[:]))
					if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
						return
					}
					if _, err := c.Write(make([]byte, resp(4+n))); err != nil {
						return
					}
				}
			}()
		}
	}()
	return &srv{ln: ln, port: uint16(ln.Addr().(*net.TCPAddr).Port)}
}

// exchange does rounds of request (4+req bytes) / response, with a pause.
func exchange(c net.Conn, rounds, req, resp int, pause time.Duration) error {
	b := make([]byte, 4+req)
	ne.PutUint32(b, uint32(req))
	rb := make([]byte, resp)
	for i := 0; i < rounds; i++ {
		if _, err := c.Write(b); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, rb); err != nil {
			return err
		}
		time.Sleep(pause)
	}
	return nil
}

type line struct {
	f, up, dn uint64
	t         int64
}

// readCapture decodes a capture and checks the shape of every line: an
// object with exactly the numeric fields f, t, up, dn, and no token that
// parses as an IP address.
func readCapture(t *testing.T, path string) ([]line, string) {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	if zr.Name != "" || zr.Comment != "" || len(zr.Extra) != 0 || !zr.ModTime.IsZero() {
		t.Fatalf("gzip header carries metadata: %+v", zr.Header)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out []line
	for i, ln := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if ln == "" {
			continue
		}
		for _, tok := range strings.FieldsFunc(ln, func(r rune) bool { return strings.ContainsRune("{}\",", r) }) {
			for _, s := range []string{tok, strings.TrimLeft(tok, ":")} {
				if _, err := netip.ParseAddr(s); err == nil {
					t.Fatalf("line %d: %q parses as an address: %s", i, s, ln)
				}
				if _, err := netip.ParseAddrPort(s); err == nil {
					t.Fatalf("line %d: %q parses as an address:port: %s", i, s, ln)
				}
			}
		}
		var m map[string]json.RawMessage
		dec := json.NewDecoder(strings.NewReader(ln))
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("line %d not JSON: %v: %s", i, err, ln)
		}
		if len(m) != 4 {
			t.Fatalf("line %d has %d fields: %s", i, len(m), ln)
		}
		var l line
		for k, v := range m {
			n, err := strconv.ParseUint(string(v), 10, 64)
			if err != nil {
				t.Fatalf("line %d field %s=%s is not a non-negative integer", i, k, v)
			}
			switch k {
			case "f":
				l.f = n
			case "t":
				l.t = int64(n)
			case "up":
				l.up = n
			case "dn":
				l.dn = n
			default:
				t.Fatalf("line %d has field %q: %s", i, k, ln)
			}
		}
		if l.up == 0 && l.dn == 0 {
			t.Fatalf("line %d: a flow that moved nothing: %s", i, ln)
		}
		if l.f == 0 || l.f >= 1<<53 {
			t.Fatalf("line %d: id out of range: %s", i, ln)
		}
		if n := len(out); n > 0 && l.t < out[n-1].t {
			t.Fatalf("line %d: t goes back: %s", i, ln)
		}
		out = append(out, l)
	}
	return out, string(raw)
}

// TestFlowcapNoAddresses runs a real capture on loopback: request/response
// flows on two "user ports" (IPv4 and IPv6), some long, some closing during
// the run, some living less than one interval, and traffic on a third port
// that is not a user port. The output must carry no address and no field
// other than f, t, up, dn; every user flow must appear with exactly the bytes
// it moved; the other port must not appear; a closed flow must stop appearing.
func TestFlowcapNoAddresses(t *testing.T) {
	resp := func(n int) int { return 3*n + 1000 }
	a := listen(t, "tcp4", "127.0.0.1:0", resp)
	var b *srv
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		ln.Close()
		b = listen(t, "tcp6", "[::1]:0", resp)
	}
	x := listen(t, "tcp4", "127.0.0.1:0", resp) // not a user port

	ports := []uint16{a.port}
	srvs := []*srv{a}
	if b != nil {
		ports = append(ports, b.port)
		srvs = append(srvs, b)
	}
	out := filepath.Join(t.TempDir(), "cap.jsonl.gz")
	cfg := config{Out: out, Interval: 100 * time.Millisecond, Ports: ports, Closed: true,
		FlushEvery: 300 * time.Millisecond, StatusEvery: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startCh := make(chan time.Time, 1)
	type result struct {
		s   summary
		err error
	}
	resCh := make(chan result, 1)
	// A connection alive before start: its bytes before start must not count.
	pre, err := net.Dial("tcp4", a.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := exchange(pre, 1, 777777, 0, 0); err != nil { // 777781 bytes up, response read below
		t.Fatal(err)
	}
	if _, err := io.ReadFull(pre, make([]byte, resp(4+777777))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // its last ACKs land before start
	go func() {
		s, err := run(ctx, cfg, t.Logf, func(s time.Time) { startCh <- s })
		resCh <- result{s, err}
	}()
	start := <-startCh

	type want struct {
		up, dn  uint64
		closeAt int64 // ms since start, -1 = open at the end
	}
	var mu sync.Mutex
	var wants []want
	var otherUp []uint64
	var wg sync.WaitGroup
	fail := make(chan error, 64)
	flowNo := 0
	for _, s := range append(srvs, x) {
		for i := 0; i < 6; i++ {
			flowNo++
			req := 1000 + 37*flowNo
			rounds := 3 + i%3
			closing := i%2 == 0
			short := i == 5
			if short {
				rounds = 1
				closing = true
			}
			s, other := s, s == x
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := net.Dial(s.ln.Addr().Network(), s.ln.Addr().String())
				if err != nil {
					fail <- err
					return
				}
				pause := 150 * time.Millisecond
				if short {
					pause = 0
				}
				if err := exchange(c, rounds, req, resp(4+req), pause); err != nil {
					fail <- err
					return
				}
				w := want{up: uint64(rounds * (4 + req)), dn: uint64(rounds * resp(4+req)), closeAt: -1}
				if closing {
					c.Close()
					w.closeAt = time.Since(start).Milliseconds()
				} else {
					t.Cleanup(func() { c.Close() })
				}
				mu.Lock()
				if other {
					otherUp = append(otherUp, w.up)
				} else {
					wants = append(wants, w)
				}
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	close(fail)
	for err := range fail {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	cancel()
	res := <-resCh
	if res.err != nil {
		t.Fatal(res.err)
	}
	t.Logf("summary: %+v", res.s)
	pre.Close()

	lines, raw := readCapture(t, out)
	for _, s := range []string{"127.0.0.1", "::1", "localhost"} {
		if strings.Contains(raw, s) {
			t.Fatalf("output contains %q", s)
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" && strings.Contains(raw, h) {
		t.Fatalf("output contains the host name")
	}

	type agg struct {
		up, dn uint64
		last   int64
	}
	byID := map[uint64]*agg{}
	for _, l := range lines {
		g := byID[l.f]
		if g == nil {
			g = &agg{}
			byID[l.f] = g
		}
		g.up += l.up
		g.dn += l.dn
		g.last = l.t
	}
	if res.s.Lost != 0 {
		t.Logf("lost close events: %d", res.s.Lost)
	}
	// The kernel counts a FIN as one byte of sequence space: a flow that
	// closed may carry one more byte in each direction, an open one none.
	slack := func(w want) uint64 {
		if w.closeAt >= 0 {
			return 1
		}
		return 0
	}
	matched := map[uint64]bool{}
	for _, w := range wants {
		var id uint64
		for f, g := range byID {
			if !matched[f] && g.up >= w.up && g.up <= w.up+slack(w) && g.dn >= w.dn && g.dn <= w.dn+slack(w) {
				id = f
				break
			}
		}
		if id == 0 {
			for _, g := range byID {
				t.Logf("written flow: up=%d dn=%d last=%d", g.up, g.dn, g.last)
			}
			t.Fatalf("user flow up=%d dn=%d closed=%v not found with its bytes", w.up, w.dn, w.closeAt >= 0)
		}
		matched[id] = true
		if w.closeAt >= 0 && byID[id].last > w.closeAt+400 {
			t.Fatalf("flow closed at %dms still written at %dms", w.closeAt, byID[id].last)
		}
	}
	for f, g := range byID {
		if matched[f] {
			continue
		}
		for _, u := range otherUp {
			if g.up == u || g.up == u+1 {
				t.Fatalf("a flow of the non-user port was captured (up=%d)", u)
			}
		}
		if g.up >= 777781 {
			t.Fatalf("bytes from before start were written (up=%d)", g.up)
		}
		t.Fatalf("unexpected flow up=%d dn=%d", g.up, g.dn)
	}
	if len(byID) != len(wants) {
		t.Fatalf("%d flows written, want %d", len(byID), len(wants))
	}
	if res.s.Fallback {
		t.Fatal("the kernel refused the port filter")
	}
}

// TestDiagFilter: the in-kernel port filter returns only the sockets of the
// user ports, and all of them.
func TestDiagFilter(t *testing.T) {
	resp := func(n int) int { return n }
	a := listen(t, "tcp4", "127.0.0.1:0", resp)
	x := listen(t, "tcp4", "127.0.0.1:0", resp)
	for _, s := range []*srv{a, a, a, x, x} {
		c, err := net.Dial("tcp4", s.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := exchange(c, 1, 10, 14, 0); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // the last ACKs land
	d, err := openDiag([]uint16{a.port})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	ss, err := d.dump(liveStates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.fallback || d.raw != len(ss) || len(ss) != 3 {
		t.Fatalf("fallback=%v raw=%d kept=%d, want 3 and 3", d.fallback, d.raw, len(ss))
	}
	for _, s := range ss {
		if s.up != 14 || s.dn != 14 {
			t.Fatalf("counters %+v, want 14/14", s)
		}
	}
	lis, err := d.dump(listenStates, nil)
	if err != nil || len(lis) != 1 {
		t.Fatalf("listeners %d %v", len(lis), err)
	}
}

// TestStopsAtSizeCap: the size limit ends the capture.
func TestStopsAtSizeCap(t *testing.T) {
	resp := func(n int) int { return n }
	a := listen(t, "tcp4", "127.0.0.1:0", resp)
	c, err := net.Dial("tcp4", a.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if exchange(c, 1, 100, 104, 5*time.Millisecond) != nil {
				return
			}
		}
	}()
	out := filepath.Join(t.TempDir(), "cap.jsonl.gz")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := run(ctx, config{Out: out, Interval: 50 * time.Millisecond, Ports: []uint16{a.port},
		MaxSize: 1, FlushEvery: 200 * time.Millisecond, StatusEvery: time.Hour}, t.Logf, nil)
	if err != nil || s.Reason != "max-size" {
		t.Fatalf("reason %q err %v", s.Reason, err)
	}
	if _, err := run(ctx, config{Out: out, Ports: []uint16{a.port}}, t.Logf, nil); !errors.Is(err, os.ErrExist) {
		t.Fatalf("an existing capture was not protected: %v", err)
	}
}
