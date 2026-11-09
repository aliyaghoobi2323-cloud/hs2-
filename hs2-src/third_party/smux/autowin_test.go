package smux

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// delayPair returns two connected net.Conns whose bytes arrive oneWay late
// in each direction (a loopback TCP pair with a delaying relay between).
func delayPair(t *testing.T, oneWay time.Duration) (net.Conn, net.Conn) {
	return ratePair(t, oneWay, func() float64 { return 0 })
}

// ratePair: delayPair through a bottleneck of rate bytes/s each way (0: none)
// with an unbounded queue in front of it.
func ratePair(t *testing.T, oneWay time.Duration, rateOf func() float64) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dial := func() (net.Conn, net.Conn) {
		ch := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				ch <- nil
				return
			}
			ch <- c
		}()
		a, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		b := <-ch
		if b == nil {
			t.Fatal("accept failed")
		}
		return a, b
	}
	a, pa := dial() // a <-> pa (relay side 1)
	pb, b := dial() // pb (relay side 2) <-> b
	relay := func(dst, src net.Conn) {
		type chunk struct {
			due time.Time
			b   []byte
		}
		q := make(chan chunk, 1<<16)
		go func() {
			defer dst.Close()
			for c := range q {
				if d := time.Until(c.due); d > 0 {
					time.Sleep(d)
				}
				if _, err := dst.Write(c.b); err != nil {
					return
				}
			}
		}()
		go func() {
			defer close(q)
			var free time.Time // when the bottleneck is free again
			for {
				buf := make([]byte, 64<<10)
				n, err := src.Read(buf)
				if n > 0 {
					due := time.Now()
					if rate := rateOf(); rate > 0 {
						if free.After(due) {
							due = free
						}
						due = due.Add(time.Duration(float64(n) / rate * float64(time.Second)))
						free = due
					}
					q <- chunk{due.Add(oneWay), buf[:n]}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	relay(pb, pa)
	relay(pa, pb)
	t.Cleanup(func() { a.Close(); b.Close(); pa.Close(); pb.Close() })
	return a, b
}

func awConfig(aw *AutoWindow) *Config {
	c := DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 << 10
	c.MaxStreamBuffer = 64 << 10
	c.MaxReceiveBuffer = 256 << 10
	c.AutoWindow = aw
	return c
}

func newAW(budget int64, max int) *AutoWindow {
	b := NewWindowBudget(budget)
	b.hold, b.holdP, b.every = 300*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	return &AutoWindow{Max: max, UpdateDiv: 4, Budget: b}
}

// transfer opens a stream on cli, writes n random bytes to the server's
// accepted stream reader and returns the elapsed time; it checks the bytes.
func transfer(t *testing.T, cli, srv *Session, n int, useWriteTo bool) time.Duration {
	t.Helper()
	data := make([]byte, n)
	rand.Read(data)
	want := sha256.Sum256(data)
	done := make(chan [32]byte, 1)
	go func() {
		st, err := srv.AcceptStream()
		if err != nil {
			done <- [32]byte{}
			return
		}
		h := sha256.New()
		if useWriteTo {
			st.WriteTo(&limitWriter{h, int64(n), st})
		} else {
			io.CopyN(h, st, int64(n))
		}
		var s [32]byte
		copy(s[:], h.Sum(nil))
		done <- s
		st.Close()
	}()
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := st.Write(data); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != want {
			t.Fatal("data mismatch")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("transfer timed out")
	}
	el := time.Since(t0)
	st.Close()
	return el
}

// limitWriter ends a WriteTo after n bytes by closing the stream.
type limitWriter struct {
	w  io.Writer
	n  int64
	st *Stream
}

func (l *limitWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	l.n -= int64(n)
	if l.n <= 0 {
		l.st.Close()
	}
	return n, err
}

func pair(t *testing.T, oneWay time.Duration, cliCfg, srvCfg *Config) (*Session, *Session) {
	a, b := delayPair(t, oneWay)
	cli, err := Client(a, cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Server(b, srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close(); srv.Close() })
	return cli, srv
}

// The window grows for a window-bound stream: a bulk transfer over a 40 ms
// round trip with a 64 KiB base window is several times faster, the data
// is intact, and everything goes back to the budget and the bucket after.
func TestAutoWindowGrowsAndReturns(t *testing.T) {
	const n = 6 << 20
	cliOff, srvOff := pair(t, 20*time.Millisecond, awConfig(nil), awConfig(nil))
	off := transfer(t, cliOff, srvOff, n, false)

	aw := newAW(64<<20, 2<<20)
	cli, srv := pair(t, 20*time.Millisecond, awConfig(nil), awConfig(aw)) // only the receiver has it
	on := transfer(t, cli, srv, n, false)
	st := aw.Budget.Stats()
	t.Logf("off %v  on %v  stats %+v", off, on, st)
	if st.Grows == 0 {
		t.Fatal("window never grew")
	}
	if on*3 > off {
		t.Fatalf("auto window not faster enough: off %v on %v", off, on)
	}
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "budget returned")
	waitFor(t, func() bool { return atomic.LoadInt32(&srv.bucket) == int32(srv.config.MaxReceiveBuffer) }, "bucket back to base")
	if st := aw.Budget.Stats(); st.Peak > st.Limit || st.Peak > 2<<20-64<<10 {
		t.Fatalf("peak %d over limit or per-stream max", st.Peak)
	}
}

// WriteTo (io.Copy's path) grows the window too.
func TestAutoWindowWriteTo(t *testing.T) {
	aw := newAW(64<<20, 2<<20)
	cli, srv := pair(t, 20*time.Millisecond, awConfig(nil), awConfig(aw))
	transfer(t, cli, srv, 4<<20, true)
	if aw.Budget.Stats().Grows == 0 {
		t.Fatal("window never grew via WriteTo")
	}
}

// An idle stream that grew gives its window back without being closed.
func TestAutoWindowDecaysWhenIdle(t *testing.T) {
	aw := newAW(64<<20, 2<<20)
	cli, srv := pair(t, 10*time.Millisecond, awConfig(nil), awConfig(aw))
	data := make([]byte, 4<<20)
	acc := make(chan *Stream, 1)
	go func() {
		st, _ := srv.AcceptStream()
		io.ReadFull(st, make([]byte, len(data)))
		acc <- st
	}()
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st.Write(data)
	rs := <-acc
	if aw.Budget.Stats().Used == 0 {
		t.Fatal("expected a grown window")
	}
	// both streams stay open, idle
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "idle window decayed")
	if aw.Budget.Stats().Shrinks == 0 {
		t.Fatal("no shrink counted")
	}
	// the stream still works at its base window afterwards
	go st.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(rs, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("after decay: %q %v", buf, err)
	}
}

// The budget bounds the sum of all growth, and several streams share it.
func TestAutoWindowBudgetBounds(t *testing.T) {
	aw := newAW(512<<10, 4<<20) // room for very little growth
	cli, srv := pair(t, 20*time.Millisecond, awConfig(nil), awConfig(aw))
	var wg sync.WaitGroup
	var peakSeen atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				if u := aw.Budget.Stats().Used; u > peakSeen.Load() {
					peakSeen.Store(u)
				}
			}
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			transfer(t, cli, srv, 3<<20, false)
		}()
	}
	wg.Wait()
	close(stop)
	st := aw.Budget.Stats()
	t.Logf("%+v seen %d", st, peakSeen.Load())
	if st.Peak > 512<<10 || peakSeen.Load() > 512<<10 {
		t.Fatalf("budget exceeded: peak %d", st.Peak)
	}
	if st.Grows == 0 {
		t.Fatal("no growth at all")
	}
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "budget returned")
}

// Mixed peers: the side with AutoWindow talks to an upstream side both ways
// (it may be either end), and both directions at once stay intact.
func TestAutoWindowMixedPeers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cli, srv *AutoWindow
	}{
		{"server-new", nil, newAW(64<<20, 1<<20)},
		{"client-new", newAW(64<<20, 1<<20), nil},
		{"both-new", newAW(64<<20, 1<<20), newAW(64<<20, 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, srv := pair(t, 5*time.Millisecond, awConfig(tc.cli), awConfig(tc.srv))
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); transfer(t, cli, srv, 3<<20, false) }()
			go func() { defer wg.Done(); transfer(t, srv, cli, 3<<20, false) }()
			wg.Wait()
		})
	}
}

// With AutoWindow off every window update carries MaxStreamBuffer and goes
// out after half of it was read (upstream's bytes); with it on, the first
// update still carries the base window.
func TestAutoWindowOffWireUnchanged(t *testing.T) {
	for _, mode := range []string{"off", "on", "on-then-SetAutoWindowOff"} {
		on := mode == "on"
		var aw *AutoWindow
		if mode != "off" {
			aw = newAW(64<<20, 1<<20)
		}
		a, b := net.Pipe()
		srv, err := Server(b, awConfig(aw))
		if err != nil {
			t.Fatal(err)
		}
		// a is a hand-driven client: SYN, then 200 KiB of PSH
		go func() {
			hdr := func(cmd byte, sid uint32, n int) []byte {
				h := make([]byte, headerSize)
				h[0], h[1] = 2, cmd
				binary.LittleEndian.PutUint16(h[2:], uint16(n))
				binary.LittleEndian.PutUint32(h[4:], sid)
				return h
			}
			a.Write(hdr(cmdSYN, 1, 0))
			chunk := bytes.Repeat([]byte{7}, 16<<10)
			for i := 0; i < 12; i++ {
				a.Write(append(hdr(cmdPSH, 1, len(chunk)), chunk...))
			}
		}()
		var upds [][2]uint32
		updCh := make(chan [2]uint32, 64)
		go func() {
			h := make([]byte, headerSize)
			for {
				if _, err := io.ReadFull(a, h); err != nil {
					close(updCh)
					return
				}
				n := binary.LittleEndian.Uint16(h[2:])
				body := make([]byte, n)
				if h[1] == cmdUPD {
					body = make([]byte, 8)
				}
				if _, err := io.ReadFull(a, body); err != nil {
					close(updCh)
					return
				}
				if h[1] == cmdUPD {
					updCh <- [2]uint32{binary.LittleEndian.Uint32(body), binary.LittleEndian.Uint32(body[4:])}
				}
			}
		}()
		st, err := srv.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16<<10)
		if mode == "on-then-SetAutoWindowOff" {
			st.SetAutoWindowOff()
		}
		for i := 0; i < 12; i++ {
			if _, err := io.ReadFull(st, buf); err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.After(time.Second)
	collect:
		for {
			select {
			case u, ok := <-updCh:
				if !ok {
					break collect
				}
				upds = append(upds, u)
			case <-deadline:
				break collect
			}
		}
		srv.Close()
		a.Close()
		if on {
			// base window, every 16 KiB (64 KiB / 4)
			if len(upds) < 6 || upds[0] != [2]uint32{16 << 10, 64 << 10} {
				t.Fatalf("on: %v", upds)
			}
			continue
		}
		want := [][2]uint32{{16 << 10, 64 << 10}, {16<<10 + 32<<10, 64 << 10}, {16<<10 + 64<<10, 64 << 10},
			{16<<10 + 96<<10, 64 << 10}, {16<<10 + 128<<10, 64 << 10}, {16<<10 + 160<<10, 64 << 10}}
		if len(upds) != len(want) {
			t.Fatalf("%s: got %v want %v", mode, upds, want)
		}
		for i := range want {
			if upds[i] != want[i] {
				t.Fatalf("%s: got %v want %v", mode, upds, want)
			}
		}
	}
}

func TestAutoWindowVerify(t *testing.T) {
	c := awConfig(&AutoWindow{Max: 1 << 20})
	if VerifyConfig(c) == nil {
		t.Fatal("a nil budget must be refused")
	}
	c.AutoWindow.Budget = NewWindowBudget(1 << 20)
	if err := VerifyConfig(c); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, f func() bool, what string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

// The queue rule: a window that would only fill a bottleneck's queue does
// not grow (over an 8 MB/s path it stays far below the maximum), and a grown
// window shrinks when the path slows down under it.
func TestAutoWindowQueueRule(t *testing.T) {
	t.Run("no-growth-into-a-queue", func(t *testing.T) {
		aw := newAW(64<<20, 4<<20)
		a, b := ratePair(t, 20*time.Millisecond, func() float64 { return 8e6 })
		cli, _ := Client(a, awConfig(nil))
		srv, _ := Server(b, awConfig(aw))
		t.Cleanup(func() { cli.Close(); srv.Close() })
		transfer(t, cli, srv, 16<<20, false)
		st := aw.Budget.Stats()
		t.Logf("%+v", st)
		if st.Peak >= 1<<20 {
			t.Fatalf("grew into the queue: peak %d (BDP ~320 KiB)", st.Peak)
		}
	})
	t.Run("shrinks-when-the-path-slows", func(t *testing.T) {
		aw := newAW(64<<20, 4<<20)
		var slow atomic.Bool
		a, b := ratePair(t, 20*time.Millisecond, func() float64 {
			if slow.Load() {
				return 4e6
			}
			return 0
		})
		cli, _ := Client(a, awConfig(nil))
		srv, _ := Server(b, awConfig(aw))
		t.Cleanup(func() { cli.Close(); srv.Close() })
		go func() {
			for i := 0; i < 1000 && aw.Budget.Stats().Used < 1<<20; i++ {
				time.Sleep(5 * time.Millisecond)
			}
			slow.Store(true)
		}()
		transfer(t, cli, srv, 40<<20, false)
		st := aw.Budget.Stats()
		t.Logf("%+v", st)
		if st.QueueCuts == 0 {
			t.Fatal("the queue rule never cut")
		}
	})
}

// A light user receiving on the same session holds a heavy stream at
// NeighborMax; background streams do not.
func TestAutoWindowNeighbourCap(t *testing.T) {
	for _, bg := range []bool{false, true} {
		aw := newAW(64<<20, 2<<20)
		aw.NeighborMax = 256 << 10
		cli, srv := pair(t, 20*time.Millisecond, awConfig(nil), awConfig(aw))
		// the light user: a few hundred bytes every 20 ms, server-side reader
		lightAcc := make(chan *Stream, 1)
		go func() {
			st, _ := srv.AcceptStream()
			if bg {
				st.SetAutoWindowBackground()
			}
			lightAcc <- st
			io.Copy(io.Discard, st)
		}()
		lc, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		lc.Write([]byte("x"))
		<-lightAcc
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
					lc.Write(make([]byte, 300))
				}
			}
		}()
		transfer(t, cli, srv, 8<<20, false)
		close(stop)
		st := aw.Budget.Stats()
		t.Logf("bg=%v %+v", bg, st)
		if !bg && st.Peak > 256<<10-64<<10 {
			t.Fatalf("grew past the neighbour cap: %d", st.Peak)
		}
		if bg && st.Peak <= 256<<10-64<<10 {
			t.Fatalf("a background stream capped the window: %d", st.Peak)
		}
	}
}

// SetAutoWindowOff then SetAutoWindowOn: the stream grows again (a settled
// stripe in hs2), and nothing is left in the budget once it closes.
func TestAutoWindowOffThenOn(t *testing.T) {
	aw := newAW(64<<20, 2<<20)
	cli, srv := pair(t, 20*time.Millisecond, awConfig(nil), awConfig(aw))
	acc := make(chan *Stream, 1)
	go func() {
		st, _ := srv.AcceptStream()
		st.SetAutoWindowOff()
		acc <- st
	}()
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st.Write([]byte("x"))
	rs := <-acc
	done := make(chan struct{})
	go func() {
		io.ReadFull(rs, make([]byte, 1+2<<20))
		rs.SetAutoWindowOn()
		io.ReadFull(rs, make([]byte, 6<<20))
		close(done)
	}()
	st.Write(make([]byte, 2<<20))
	if g := aw.Budget.Stats().Grows; g != 0 {
		t.Fatalf("grew while off: %d", g)
	}
	st.Write(make([]byte, 6<<20))
	<-done
	if aw.Budget.Stats().Grows == 0 {
		t.Fatal("never grew after SetAutoWindowOn")
	}
	rs.Close()
	waitFor(t, func() bool { return aw.Budget.Stats().Used == 0 }, "budget returned")
}
