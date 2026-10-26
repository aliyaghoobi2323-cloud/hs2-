package engine

import (
	"fmt"
	"math"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// C3's model tests (REDESIGN §3.3): the guard's pure part, and hostQ over
// a synthetic link socket in virtual time.

// (mbit: bytes/s in a Mbit/s, autopilot_sim_test.go)

// skbSock is a link socket in virtual time: it sends its unsent bytes at r
// and keeps TCP_NOTSENT_LOWAT the way Linux does (the skb rule): a write
// appends to the unsent tail buffer while that holds less than the size
// goal (the sender's pacing over ~1 ms, at least one or two segments, at
// most 64 KiB), and starts a new one only while less than the low-water
// mark is unsent; otherwise it waits (Go's netpoller) until the socket
// polls writable again, less than half the mark unsent (Linux 5.10 on).
// It is also the guard's hostSys (setsockopt, SIOCOUTQNSD, the netpoller's
// wait).
type skbSock struct {
	net.Conn
	now      time.Duration
	r        float64 // bytes/s
	goal     int
	lowat    int
	unsent   float64
	tail     float64 // the unsent tail buffer's bytes
	sets     []int   // setLowat calls
	setAt    []time.Duration
	err      error // setLowat's and notsent's (ENOPROTOOPT: an old kernel)
	nsErr    error // notsent's alone
	written  int64
	waits    int
	failWait bool
}

// newSkbSock: a socket at r whose sender paces at pace times r.
func newSkbSock(r float64, mss int, pace float64) *skbSock {
	segs := 2
	if pace*r < 150000 { // BBR's one-segment TSO under 1.2 Mbit/s
		segs = 1
	}
	goal := int(math.Min(math.Max(pace*r/1000, float64(segs*mss)), 64<<10))
	return &skbSock{r: r, goal: goal, lowat: 32 << 10}
}

// advance lets d go by.
func (s *skbSock) advance(d time.Duration) {
	s.now += d
	s.unsent = math.Max(0, s.unsent-s.r*d.Seconds())
	s.tail = math.Min(s.tail, s.unsent)
}

// drainTo lets time go by until less than n bytes are unsent.
func (s *skbSock) drainTo(n float64) {
	if s.unsent < n {
		return
	}
	s.advance(time.Duration((s.unsent-n)/s.r*float64(time.Second)) + time.Microsecond)
}

func (s *skbSock) Write(p []byte) (int, error) {
	rest := float64(len(p))
	for rest > 0 {
		switch {
		case s.tail > 0 && s.tail < float64(s.goal):
			n := math.Min(float64(s.goal)-s.tail, rest)
			s.tail += n
			s.unsent += n
			rest -= n
		case s.unsent < float64(s.lowat):
			n := math.Min(float64(s.goal), rest)
			s.tail = n
			s.unsent += n
			rest -= n
		default:
			s.drainTo(float64(s.lowat) / 2)
		}
	}
	s.written += int64(len(p))
	return len(p), nil
}

func (s *skbSock) setLowat(n int) error {
	if s.err != nil {
		return s.err
	}
	s.lowat = n
	s.sets = append(s.sets, n)
	s.setAt = append(s.setAt, s.now)
	return nil
}

func (s *skbSock) notsent() (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.nsErr != nil {
		return 0, s.nsErr
	}
	return int(math.Ceil(s.unsent)), nil
}

// waitBelow is the netpoller's wait as hostSysTCP's: back at once if less
// than n is unsent or the socket polls writable, else at the next wake.
func (s *skbSock) waitBelow(n int) error {
	s.waits++
	if s.unsent < float64(n) {
		return nil
	}
	s.drainTo(float64(s.lowat) / 2)
	return nil
}

// TestLowatTable: the low-water mark is 2 ms of the rate, at least 4 KiB
// and two segments, at most 256 KiB (MSS 1448).
func TestLowatTable(t *testing.T) {
	for _, c := range []struct {
		mbit float64
		want int
	}{{0, 4096}, {1, 4096}, {16, 4096}, {64, 16000}, {100, 25000}, {1000, 250000}, {2000, 262144}} {
		if got := Lowat(c.mbit*mbit, 1448); got != c.want {
			t.Fatalf("%.0f Mbit/s: %d, want %d", c.mbit, got, c.want)
		}
	}
	if got := Lowat(1*mbit, 2900); got != 5800 {
		t.Fatalf("MSS 2900: %d, want two segments", got)
	}
}

// TestGateCap: within the gate the socket holds at most cap = 2 ms of the
// rate and at least two segments, and its low-water mark is half of that
// (at least a segment), so each wake has room for half a cap.
func TestGateCap(t *testing.T) {
	var g hostGate
	for _, c := range []struct {
		mbit       float64
		cap, lowat int
	}{{1, 2896, 1448}, {5, 2896, 1448}, {10, 2896, 1448}, {20, 5000, 2500}} {
		on, cp, lw := g.Update(c.mbit*mbit, 1448)
		if !on || cp != c.cap || lw != c.lowat {
			t.Fatalf("%.0f Mbit/s: on %v cap %d lowat %d, want on, %d, %d", c.mbit, on, cp, lw, c.cap, c.lowat)
		}
	}
	if r := g.Room(1000); r != 4000 {
		t.Fatalf("room under 1000 B of 5000: %d", r)
	}
	if r := g.Room(7000); r != 0 {
		t.Fatalf("room past the cap: %d", r)
	}
	// unknown rate: the gate is on at the floor
	var u hostGate
	if on, cp, lw := u.Update(0, 1448); !on || cp != 2896 || lw != 1448 {
		t.Fatalf("unknown rate: on %v cap %d lowat %d", on, cp, lw)
	}
}

// TestGateHysteresis: on below 20 Mbit/s, off above 30, and a rate that
// swings between 22 and 28 never flips it either way.
func TestGateHysteresis(t *testing.T) {
	var g hostGate
	swing := func(want bool) {
		t.Helper()
		for i := range 50 {
			r := 22 + 6*float64(i%2) + float64(i%5)/2
			if on, _, _ := g.Update(r*mbit, 1448); on != want {
				t.Fatalf("%.1f Mbit/s: on %v, want %v", r, on, want)
			}
		}
	}
	swing(true) // from the start (an unknown rate is low)
	if on, _, lw := g.Update(31*mbit, 1448); on || lw != Lowat(31*mbit, 1448) {
		t.Fatalf("31 Mbit/s: on %v lowat %d", on, lw)
	}
	swing(false)
	if on, _, _ := g.Update(30*mbit, 1448); on {
		t.Fatal("30 Mbit/s turned it on")
	}
	if on, _, _ := g.Update(20*mbit, 1448); on {
		t.Fatal("20 Mbit/s turned it on")
	}
	if on, _, _ := g.Update(19.9*mbit, 1448); !on {
		t.Fatal("19.9 Mbit/s left it off")
	}
}

// hostWait runs a writer that writes bulk frames of BulkFrameSize(r) back
// to back through hostQ into an skbSock at r, the guard updated each 500 ms
// (from rate0, then r), and after every seventh a light frame of 100 B
// that came as the bulk one began: its wait on the host is until its first
// byte leaves (the frame in progress, the gate's wait and what the socket
// holds ahead of it). It returns the worst.
func hostWait(r, rate0 float64) (worst time.Duration, on bool) {
	s := newSkbSock(r, 1448, wmStartPace)
	h := newHostQ(s, s)
	h.Update(rate0, 1448)
	next := time.Duration(0)
	bulk, light := make([]byte, BulkFrameSize(r)), make([]byte, 100)
	for i := 0; s.now < 20*time.Second; i++ {
		if s.now >= next {
			h.Update(r, 1448)
			next += hostqTick
		}
		t0 := s.now
		h.Write(bulk)
		if i%7 == 6 {
			h.Write(light)
			// its first byte leaves once what was ahead of it is out
			out := s.now + time.Duration((s.unsent-float64(len(light)))/s.r*float64(time.Second))
			worst = max(worst, out-t0)
		}
	}
	return worst, h.on.Load()
}

// TestHostWaitBound: a light frame waits on the host (the frame in
// progress and what the socket holds unsent, by the skb rule) no more than
// max(20 ms, 3·1500 B/R), the bound of R1's criterion, from 1 Mbit/s to
// 1 Gbit/s; at 25 Mbit/s with the gate on (from below) and off (from
// above).
func TestHostWaitBound(t *testing.T) {
	for _, c := range []struct{ mbit, from float64 }{{1, 0}, {5, 0}, {10, 0}, {25, 0}, {25, 100}, {100, 0}, {1000, 0}} {
		r := c.mbit * mbit
		bound := max(20*time.Millisecond, time.Duration(3*1500/r*float64(time.Second)))
		worst, on := hostWait(r, c.from*mbit)
		t.Logf("%4.0f Mbit/s (from %3.0f): gate %-5v worst wait %v, bound %v", c.mbit, c.from, on, worst, bound)
		if worst > bound {
			t.Fatalf("%.0f Mbit/s (from %.0f): a light frame waited %v on the host, more than %v", c.mbit, c.from, worst, bound)
		}
	}
}

// TestLowatAdapts: the rate drops tenfold (100 → 10 Mbit/s: the gate comes
// on; 1000 → 100: the mark only): the mark and the cap follow within a
// 500 ms update.
func TestLowatAdapts(t *testing.T) {
	for _, c := range []struct{ from, to float64 }{{100, 10}, {1000, 100}} {
		s := newSkbSock(c.from*mbit, 1448, 1)
		h := newHostQ(s, s)
		drop := 2300 * time.Millisecond
		var at time.Duration = -1
		for now := time.Duration(0); now < 5*time.Second; now += hostqTick {
			s.now = now
			r := c.from * mbit
			if now >= drop {
				r = c.to * mbit
			}
			h.Update(r, 1448)
			if now >= drop && at < 0 {
				at = now
			}
		}
		var g hostGate
		g.Update(c.from*mbit, 1448)
		on, cp, lw := g.Update(c.to*mbit, 1448)
		k := len(s.sets) - 1
		if s.sets[k] != lw || s.setAt[k] > drop+hostqTick || h.on.Load() != on || int(h.cap.Load()) != cp {
			t.Fatalf("%.0f → %.0f Mbit/s at %v: lowat %d at %v (want %d), gate %v cap %d (want %v %d)",
				c.from, c.to, drop, s.sets[k], s.setAt[k], lw, h.on.Load(), h.cap.Load(), on, cp)
		}
	}
}

// TestLowatWriteOnChange: the mark is written only when it moves by 25% or
// more from the one written.
func TestLowatWriteOnChange(t *testing.T) {
	s := newSkbSock(100*mbit, 1448, 1)
	h := newHostQ(s, s)
	h.Update(100*mbit, 1448)
	n := len(s.sets)
	for _, r := range []float64{110, 90, 120, 80, 105} { // within 25% of 25000 B
		h.Update(r*mbit, 1448)
	}
	if len(s.sets) != n {
		t.Fatalf("setLowat called %d times for moves under 25%%: %v", len(s.sets)-n, s.sets[n:])
	}
	h.Update(130*mbit, 1448) // 32500: +30%
	if len(s.sets) != n+1 || s.sets[n] != 32500 {
		t.Fatalf("a move of 30%%: %v", s.sets[n:])
	}
}

// TestHostQNoSyscalls: a kernel without TCP_NOTSENT_LOWAT (or without
// SIOCOUTQNSD) keeps today's behaviour: the fixed low-water mark and the old
// send gate, with one log line for the process.
func TestHostQNoSyscalls(t *testing.T) {
	var lines []string
	f := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	coreLog.Store(&f)
	defer coreLog.Store(nil)
	hostqNoSys.Store(false)
	for i, c := range []struct{ set, ns error }{{syscall.ENOPROTOOPT, nil}, {nil, syscall.ENOPROTOOPT}, {syscall.ENOPROTOOPT, nil}} {
		s := newSkbSock(10*mbit, 1448, 1)
		s.err, s.nsErr = c.set, c.ns
		h := newHostQ(s, s)
		h.Update(5*mbit, 1448)
		if _, err := h.Write(make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
		h.Update(50*mbit, 1448)
		h.Write(make([]byte, 1000))
		if h.fb.Load() == nil {
			t.Fatalf("case %d: no fallback to the send gate", i)
		}
		if s.written != 2000 {
			t.Fatalf("case %d: %d B reached the socket", i, s.written)
		}
		if c.set != nil && len(s.sets) != 0 {
			t.Fatalf("case %d: the mark was set: %v", i, s.sets)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "TCP_NOTSENT_LOWAT") {
		t.Fatalf("log lines: %q", lines)
	}
	t.Log(lines[0])
	// a socket that is gone (EBADF) is no old kernel: no fallback, no line,
	// and the write goes on to the conn (whose error it is to report)
	s := newSkbSock(10*mbit, 1448, 1)
	h := newHostQ(s, s)
	h.Update(5*mbit, 1448)
	s.nsErr = syscall.EBADF
	h.Write(make([]byte, 1000))
	s.err = syscall.EBADF
	h.Update(50*mbit, 1448)
	if h.fb.Load() != nil || len(lines) != 1 || s.written != 1000 {
		t.Fatalf("EBADF: fallback %v, %d lines, %d B written", h.fb.Load() != nil, len(lines), s.written)
	}
}

// TestHostQOffSwitch: HS2_TUNE_HQG=0 (TuneHQG 0) says off.
func TestHostQOffSwitch(t *testing.T) {
	defer func(v int) { TuneHQG = v }(TuneHQG)
	if HostQOff() {
		t.Fatal("off by default")
	}
	TuneHQG = 0
	if !HostQOff() {
		t.Fatal("TuneHQG 0 and HostQOff false")
	}
}

// TestHostQStatusLine: the document's line (§3.3).
func TestHostQStatusLine(t *testing.T) {
	s := newSkbSock(10*mbit, 1448, 1)
	h := newHostQ(s, s)
	h.Update(10*mbit, 1448)
	for range 200 {
		h.Write(make([]byte, 6250))
	}
	l := h.line(3, time.Second)
	if !strings.HasPrefix(l, "hostq link 3: lowat 1.4 KiB, gate on (cap 2896 B), notsent p95 ") || !strings.Contains(l, " B, blocked ratio ") {
		t.Fatalf("%q", l)
	}
	t.Log(l)
}
