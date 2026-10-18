package smux

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Red team (Z5 window core, part A): invariants of the window pool and the
// window core's arm, checked while a fake core, readers and writers race.

// poolInvariants checks, with every lock of every session held, what the
// pool's bookkeeping promises at any moment: the pool and what the sessions
// hold from it add up to its size; a session's extra is its streams' claims
// plus its spare; a window past NeedStreamBuffer is covered by its claim;
// nothing is negative; no window is past MaxStreamBuffer; and the bucket is
// never more than a frame below zero.
func poolInvariants(pool *WindowPool, size int, sessions ...*Session) error {
	for _, s := range sessions {
		s.streamLock.Lock()
		defer s.streamLock.Unlock()
	}
	for _, s := range sessions {
		for _, st := range s.streams {
			st.bufferLock.Lock()
			defer st.bufferLock.Unlock()
		}
	}
	for _, s := range sessions {
		s.spareLock.Lock()
		defer s.spareLock.Unlock()
	}
	sum := int64(pool.free.Load())
	for _, s := range sessions {
		extra, spare := atomic.LoadInt32(&s.extra), atomic.LoadInt32(&s.spare)
		if spare < 0 || extra < 0 {
			return fmt.Errorf("session extra %d spare %d", extra, spare)
		}
		claims := 0
		need := uint32(s.config.NeedStreamBuffer)
		for _, st := range s.streams {
			if st.claim < 0 {
				return fmt.Errorf("stream %d claim %d", st.id, st.claim)
			}
			claims += st.claim
			if st.rcvWin > uint32(s.config.MaxStreamBuffer) {
				return fmt.Errorf("stream %d window %d past MaxStreamBuffer", st.id, st.rcvWin)
			}
			closed := false
			select {
			case <-st.die:
				closed = true
			default:
			}
			if !closed && st.rcvWin > need && int(st.rcvWin-need) > st.claim {
				return fmt.Errorf("stream %d window %d past need %d by more than its claim %d", st.id, st.rcvWin, need, st.claim)
			}
		}
		if s.IsClosed() {
			sum += int64(extra) // what a closing session still holds
			continue
		}
		if int(extra) != claims+int(spare) {
			return fmt.Errorf("session extra %d, claims %d + spare %d", extra, claims, spare)
		}
		if b := atomic.LoadInt32(&s.bucket); int(b) < -s.config.MaxFrameSize {
			return fmt.Errorf("bucket %d", b)
		}
		sum += int64(extra)
	}
	if sum != int64(size) {
		return fmt.Errorf("pool free %d + held %d = %d, want %d", pool.free.Load(), sum-pool.free.Load(), sum, size)
	}
	return nil
}

// TestRedPoolInvariantsUnderChaos: two sessions sharing a pool over a real
// socket, many streams moving data both ways under readers that run fast,
// slow, pause and stop, a fake core flipping ceilings and references every
// few ms, sweeps every 20 ms: the pool's bookkeeping holds at every check.
func TestRedPoolInvariantsUnderChaos(t *testing.T) {
	for round := 0; round < 3; round++ {
		redChaos(t, int64(round))
	}
}

func redChaos(t *testing.T, seed int64) {
	const poolSize = 2 << 20
	pool := NewWindowPool(poolSize)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfgOf := func() *Config {
		c := poolConfig(pool)
		c.KeepAliveDisabled = false
		c.KeepAliveInterval = 20 * time.Millisecond
		c.KeepAliveTimeout = 30 * time.Second
		return c
	}
	srvCh := make(chan *Session, 1)
	var wg sync.WaitGroup
	stopIO := make(chan struct{})
	push := func(st *Stream) {
		defer wg.Done()
		buf := make([]byte, 16<<10)
		for {
			select {
			case <-stopIO:
				return
			default:
			}
			st.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			if _, err := st.Write(buf); err != nil && err != ErrTimeout {
				return
			}
		}
	}
	read := func(st *Stream, r *rand.Rand) {
		defer wg.Done()
		mode := r.Intn(4)
		buf := make([]byte, 1+r.Intn(64<<10))
		for {
			select {
			case <-stopIO:
				return
			default:
			}
			switch mode {
			case 1:
				time.Sleep(time.Millisecond)
			case 2:
				if r.Intn(200) == 0 {
					time.Sleep(time.Duration(r.Intn(300)) * time.Millisecond)
				}
			case 3:
				if r.Intn(2000) == 0 {
					mode = 0
				}
			}
			st.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := st.Read(buf); err != nil && err != ErrTimeout {
				return
			}
		}
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s, _ := Server(c, cfgOf())
		srvCh <- s
		r := rand.New(rand.NewSource(seed + 100))
		for i := 0; ; i++ {
			st, err := s.AcceptStream()
			if err != nil {
				return
			}
			wg.Add(1)
			if i%2 == 0 {
				go push(st)
			} else {
				go read(st, rand.New(rand.NewSource(r.Int63())))
			}
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cli, err := Client(c, cfgOf())
	if err != nil {
		t.Fatal(err)
	}
	srv := <-srvCh
	stop := make(chan struct{})
	core := fakeCore(seed+200, stop, cli, srv)
	core.Add(1)
	go func() { // the pacing-cap experiment's reader, on a goroutine of its own
		defer core.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			cli.PeerWindows()
			srv.PeerWindows()
		}
	}()
	r := rand.New(rand.NewSource(seed))
	var streams []*Stream
	for i := 0; i < 16; i++ {
		st, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, st)
		wg.Add(1)
		if i%2 == 0 {
			go read(st, rand.New(rand.NewSource(r.Int63())))
		} else {
			go push(st)
		}
	}
	end := time.Now().Add(2 * time.Second)
	checks, maxHeld := 0, 0
	for time.Now().Before(end) {
		if h := poolSize - pool.Free(); h > maxHeld {
			maxHeld = h
		}
		time.Sleep(time.Duration(1+r.Intn(5)) * time.Millisecond)
		if err := poolInvariants(pool, poolSize, cli, srv); err != nil {
			t.Fatalf("seed %d, check %d: %v", seed, checks, err)
		}
		checks++
		// now and then a stream closes and another opens
		if r.Intn(50) == 0 && len(streams) > 0 {
			k := r.Intn(len(streams))
			streams[k].Close()
			streams = append(streams[:k], streams[k+1:]...)
			st, err := cli.OpenStream()
			if err == nil {
				streams = append(streams, st)
				wg.Add(1)
				go read(st, rand.New(rand.NewSource(r.Int63())))
			}
		}
	}
	close(stopIO)
	close(stop)
	core.Wait()
	cli.Close()
	srv.Close()
	for _, st := range streams {
		st.Close()
	}
	wg.Wait()
	if f := pool.Free(); f != poolSize {
		t.Fatalf("seed %d: pool free %d after both sessions closed, want %d", seed, f, poolSize)
	}
	t.Logf("seed %d: %d checks, at most %d KiB of the pool held, borrows %d/%d, pool short %d/%d", seed, checks, maxHeld>>10,
		cli.Borrows(), srv.Borrows(), atomic.LoadUint32(&cli.poolShorts), atomic.LoadUint32(&srv.poolShorts))
}

// A stream that carries one transfer, idles (a keep-alive connection, a
// video player between segments), then carries the next: the first data of
// the next transfer passes the limits of the updates still pending from the
// end of the first, and their "turnarounds" are the idle time, not the
// window's way to the peer and back. The core must not be told a turnaround
// of a second on a path whose turnaround is well under a millisecond, nor
// should growth (turnOK) or the claim's hold (claimTurn) go by it.
func TestRedTurnaroundAfterIdle(t *testing.T) {
	const idle = time.Second
	cfg := poolConfig(NewWindowPool(4 << 20))
	session, stream, p := newRawPeer(t, cfg)
	keepUp(t, p, stream, 1<<20+12345) // ends anywhere: not at a limit
	p.sync(stream)
	stream.bufferLock.Lock()
	pending := stream.core.n
	stream.bufferLock.Unlock()
	before, _ := session.CoreSnapshot(nil)
	var turn time.Duration
	for _, s := range before {
		if s.TurnLast > turn {
			turn = s.TurnLast
		}
		if s.TurnMin > turn {
			turn = s.TurnMin
		}
	}
	time.Sleep(idle)
	// the core looks often (its wakes): the latest turnaround of each look
	var after CoreSample
	var seen, hold time.Duration
	for n := 0; n < 512<<10; n += 4096 {
		keepUp(t, p, stream, 4096)
		s, _ := session.CoreSnapshot(nil)
		if len(s) == 1 && (s[0].TurnLast > after.TurnLast || s[0].TurnMin > after.TurnMin) {
			after = s[0]
		}
		stream.bufferLock.Lock()
		if d := time.Duration(stream.core.turnSeen); d > seen {
			seen = d
			hold = claimHold * stream.claimTurn()
		}
		stream.bufferLock.Unlock()
	}
	t.Logf("%d updates pending at the idle; turnarounds before it up to %v; after it: least %v, latest %v; stream's own up to %v (claim hold %v)",
		pending, turn, after.TurnMin, after.TurnLast, seen, hold)
	if after.TurnMin >= idle/2 || after.TurnLast >= idle/2 {
		t.Errorf("a turnaround of the idle time reached the core: least %v, latest %v (before the idle: up to %v)", after.TurnMin, after.TurnLast, turn)
	}
	if seen >= idle/2 {
		t.Errorf("the stream's own turnaround (growth past the need goes by it) was %v", seen)
	}
	if hold >= idle {
		t.Errorf("a claim would be held %v after its window came down", hold)
	}
}

// The other side of it: a peer held at its limit for over turnIdle (its
// update was that slow to come) is a turnaround, and is sampled; one whose
// data stopped short of the limit for as long is not.
func TestRedLongTurnaroundAtTheLimitKept(t *testing.T) {
	const w = 1 << 20
	for _, atLimit := range []bool{true, false} {
		s := coreBare(4096)
		s.arrive(64 << 10)
		s.readAndAnnounce(64<<10, w)
		recv := s.core.readTot.Load() + uint64(s.unread)
		short := 0
		if !atLimit {
			short = 64 << 10
		}
		s.arrive(int(s.core.ring[s.core.head].lim-recv) - short) // up to (or short of) the limit ...
		time.Sleep(turnIdle + 100*time.Millisecond)              // ... a long wait ...
		s.arrive(short + 4096)                                   // ... past it
		d := time.Duration(s.core.turnLast.Load())
		if atLimit && (d < turnIdle || s.core.n != 0) {
			t.Errorf("held at the limit for %v: turnaround %v, ring %d", turnIdle, d, s.core.n)
		}
		if !atLimit && (d != 0 || s.core.n != 0) {
			t.Errorf("stopped short of the limit for %v: turnaround %v, ring %d, want none", turnIdle, d, s.core.n)
		}
	}
}

// TestRedNoStallUnderSweepsAndChaos: streams of two sessions sharing a small
// pool move data both ways while a fake core flips ceilings and references,
// and a sweeper takes random streams' windows down as if they had idled
// (sweepClaims, mid-transfer), and readers pause: every byte arrives, in
// order, within the deadline, the pool's bookkeeping holds throughout, and
// the pool is whole at the end.
func TestRedNoStallUnderSweepsAndChaos(t *testing.T) {
	for seed := int64(0); seed < 3; seed++ {
		redSweepChaos(t, seed)
	}
}

func redSweepChaos(t *testing.T, seed int64) {
	const poolSize = 384 << 10
	pool := NewWindowPool(poolSize)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const streams, size = 10, 3 << 20
	data := func(i int) []byte {
		b := make([]byte, size)
		rand.New(rand.NewSource(int64(i) + 1000*seed)).Read(b)
		return b
	}
	recv := func(st *Stream, i int, r *rand.Rand) error {
		want := data(i)
		got := 0
		buf := make([]byte, 1+r.Intn(64<<10))
		for got < size {
			st.SetReadDeadline(time.Now().Add(15 * time.Second))
			n, err := st.Read(buf)
			for k := 0; k < n; k++ {
				if buf[k] != want[got+k] {
					return fmt.Errorf("stream %d: byte %d wrong", i, got+k)
				}
			}
			got += n
			if err != nil && got < size {
				return fmt.Errorf("stream %d: %v after %d", i, err, got)
			}
			if r.Intn(40) == 0 {
				time.Sleep(time.Duration(r.Intn(30)) * time.Millisecond)
			}
		}
		return nil
	}
	errs := make(chan error, streams)
	srvDone := make(chan *Session, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s, _ := Server(c, poolConfig(pool))
		srvDone <- s
		r := rand.New(rand.NewSource(seed + 50))
		for {
			st, err := s.AcceptStream()
			if err != nil {
				return
			}
			rr := rand.New(rand.NewSource(r.Int63()))
			go func() {
				var k [1]byte
				if _, err := st.Read(k[:]); err != nil {
					errs <- err
					return
				}
				if i := int(k[0]); i%2 == 0 {
					errs <- recv(st, i, rr)
				} else {
					st.Write(data(i))
				}
			}()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cli, err := Client(c, poolConfig(pool))
	if err != nil {
		t.Fatal(err)
	}
	srv := <-srvDone
	stop := make(chan struct{})
	core := fakeCore(seed+7, stop, cli, srv)
	var sweeps sync.WaitGroup
	sweeps.Add(1)
	go func() { // the sweeper, and checks of the bookkeeping
		defer sweeps.Done()
		r := rand.New(rand.NewSource(seed + 9))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(1+r.Intn(10)) * time.Millisecond):
			}
			for _, s := range []*Session{cli, srv} {
				s.streamLock.Lock()
				for _, st := range s.streams {
					if r.Intn(4) == 0 {
						st.bufferLock.Lock()
						st.readAt = monoNow() - int64(claimIdle)
						st.bufferLock.Unlock()
					}
				}
				s.streamLock.Unlock()
				s.sweepClaims()
			}
			if err := poolInvariants(pool, poolSize, cli, srv); err != nil {
				errs <- err
				return
			}
		}
	}()
	rr := rand.New(rand.NewSource(seed + 11))
	for i := 0; i < streams; i++ {
		st, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		st.Write([]byte{byte(i)})
		if i%2 == 0 {
			go st.Write(data(i))
		} else {
			r := rand.New(rand.NewSource(rr.Int63()))
			go func(i int) { errs <- recv(st, i, r) }(i)
		}
	}
	for i := 0; i < streams; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
	close(stop)
	core.Wait()
	sweeps.Wait()
	cli.Close()
	srv.Close()
	if pool.Free() != poolSize {
		t.Fatalf("seed %d: pool free %d after both sessions closed, want all of it", seed, pool.Free())
	}
}
