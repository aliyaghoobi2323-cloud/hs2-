package smux

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Red team round 2: the round-1 chaos runs with more seeds, and a chaos run
// of its own for the fix round's rules: links that die abruptly (the socket
// goes, not Close) or close while their streams close at both ends at once,
// readers that stop for good or never close, the pool watched between the
// checks for a moment past its size (a double return) or below zero.

// red2Seeds is how many seeds the round-2 chaos runs take: HS2_RED2_SEEDS,
// else 0 (only TestRed2AbruptDeathChaos runs, with 6).
func red2Seeds() int {
	n, _ := strconv.Atoi(os.Getenv("HS2_RED2_SEEDS"))
	return n
}

// TestRed2ChaosMoreSeeds: round 1's three chaos runs with HS2_RED2_SEEDS
// seeds each, from 100 (round 1 used 0 to 3) or HS2_RED2_FROM, all three or
// HS2_RED2_ONLY (inv, sweep, core).
func TestRed2ChaosMoreSeeds(t *testing.T) {
	n := red2Seeds()
	if n == 0 {
		t.Skip("HS2_RED2_SEEDS not set")
	}
	defer func(g time.Duration) { closeGrace = g }(closeGrace)
	closeGrace = 300 * time.Millisecond
	from, _ := strconv.Atoi(os.Getenv("HS2_RED2_FROM"))
	if from == 0 {
		from = 100
	}
	only := os.Getenv("HS2_RED2_ONLY") // inv, sweep or core: that chaos only
	for seed := int64(from); seed < int64(from+n); seed++ {
		if only == "" || only == "inv" {
			redChaos(t, seed)
		}
		if only == "" || only == "sweep" {
			redSweepChaos(t, seed)
		}
		if only == "" || only == "core" {
			coreChaosRound(t, seed)
		}
	}
}

// poolStrict checks, with every lock of every session held, the pool's
// books for open and closed sessions alike: the pool and what the sessions
// hold add up to its size, and each session's extra is its streams' claims
// plus its spare -- also once it closed (letGo, dropSpare and dropClaims
// keep it so) -- and nothing is negative.
func poolStrict(pool *WindowPool, size int, sessions ...*Session) error {
	_, err := poolStrictClaims(pool, size, sessions...)
	return err
}

// poolStrictClaims is poolStrict, also returning the claims of all streams.
func poolStrictClaims(pool *WindowPool, size int, sessions ...*Session) (all int, err error) {
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
	sum := pool.free.Load()
	if sum < 0 || sum > int64(size) {
		return 0, fmt.Errorf("pool free %d of %d", sum, size)
	}
	for _, s := range sessions {
		extra, spare := atomic.LoadInt32(&s.extra), atomic.LoadInt32(&s.spare)
		claims := 0
		for _, st := range s.streams {
			if st.claim < 0 {
				return 0, fmt.Errorf("stream %d claim %d", st.id, st.claim)
			}
			claims += st.claim
		}
		if spare < 0 || extra < 0 || int(extra) != claims+int(spare) {
			return 0, fmt.Errorf("session (closed %v) extra %d, claims %d + spare %d", s.IsClosed(), extra, claims, spare)
		}
		all += claims
		sum += int64(extra)
	}
	if sum != int64(size) {
		return 0, fmt.Errorf("pool free %d + held %d = %d, want %d", pool.free.Load(), sum-pool.free.Load(), sum, size)
	}
	return all, nil
}

// TestRed2AbruptDeathChaos: see red2Abrupt; 6 seeds, or HS2_RED2_SEEDS.
func TestRed2AbruptDeathChaos(t *testing.T) {
	defer func(g time.Duration) { closeGrace = g }(closeGrace)
	closeGrace = 300 * time.Millisecond
	n := red2Seeds()
	if n == 0 {
		n = 6
	}
	for seed := int64(0); seed < int64(n); seed++ {
		red2Abrupt(t, seed)
	}
}

// red2Abrupt: two sessions sharing a small pool over a real socket, streams
// both ways under readers that run fast, slow, pause, or stop for good
// after a fast start, a fake core flipping ceilings, sweeps every 20 ms;
// then the link dies one of three ways: its socket closes under both
// sessions (each closes at its keepalive timeout, or, parked on a full
// buffer, when its owner gives up on it, as hs2's suspect check does), one
// session closes while streams close at both ends at once, or both
// sessions close at once with the same. A third of the readers close their
// stream when their reads fail, a third later, a third never. The pool's
// books hold throughout (poolStrict), a watcher never sees the pool past its
// size or below zero, and the pool is whole closeGrace after the end.
func red2Abrupt(t *testing.T, seed int64) {
	const poolSize = 3 << 20
	pool := NewWindowPool(poolSize)
	var bad atomic.Value
	stopWatch := make(chan struct{})
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		for i := 0; ; i++ {
			select {
			case <-stopWatch:
				return
			default:
			}
			if f := pool.free.Load(); f > poolSize || f < 0 {
				bad.CompareAndSwap(nil, fmt.Sprintf("pool free %d of %d", f, poolSize))
			}
			if i%64 == 0 {
				runtime.Gosched()
			}
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfgOf := func() *Config {
		c := poolConfig(pool)
		if seed/3%2 == 0 {
			c.MaxReceiveBuffer = 2 << 20 // room for windows to grow past the need
		} // else 512 KiB: the session runs full and borrows
		c.KeepAliveDisabled = false
		c.KeepAliveInterval = 20 * time.Millisecond
		c.KeepAliveTimeout = 300 * time.Millisecond
		return c
	}
	srvCh := make(chan *Session, 1)
	rawCh := make(chan net.Conn, 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []*Stream
	keep := func(st *Stream) {
		mu.Lock()
		all = append(all, st)
		mu.Unlock()
	}
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
		mode := r.Intn(5)
		if mode == 4 {
			mode = 0 // fast, twice as often
		}
		closing := r.Intn(3)
		buf := make([]byte, 1+r.Intn(64<<10))
		start := time.Now()
		defer func() {
			switch closing {
			case 0:
				st.Close()
			case 1:
				time.Sleep(closeGrace / 2)
				st.Close()
			} // 2: never
		}()
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
			case 3: // fast, then stopped for good
				if time.Since(start) > 150*time.Millisecond {
					<-stopIO
					return
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
		rawCh <- c
		s, _ := Server(c, cfgOf())
		srvCh <- s
		r := rand.New(rand.NewSource(seed + 100))
		for i := 0; ; i++ {
			st, err := s.AcceptStream()
			if err != nil {
				return
			}
			keep(st)
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
	srvRaw := <-rawCh
	srv := <-srvCh
	stop := make(chan struct{})
	core := fakeCore(seed+200, stop, cli, srv)
	checks, maxClaims := 0, 0
	var checkErr atomic.Value
	core.Add(1)
	go func() { // sweeps, as the keepalive tick runs them, and the books
		defer core.Done()
		r := rand.New(rand.NewSource(seed + 300))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(1+r.Intn(4)) * time.Millisecond):
			}
			claims, err := poolStrictClaims(pool, poolSize, cli, srv)
			if err != nil {
				checkErr.CompareAndSwap(nil, err.Error())
				return
			}
			if claims > maxClaims {
				maxClaims = claims
			}
			checks++
		}
	}()
	r := rand.New(rand.NewSource(seed))
	for i := 0; i < 10; i++ {
		st, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		keep(st)
		wg.Add(1)
		if i%2 == 0 {
			go read(st, rand.New(rand.NewSource(r.Int63())))
		} else {
			go push(st)
		}
	}
	time.Sleep(time.Duration(300+r.Intn(900)) * time.Millisecond)
	closeSome := func(k int) {
		mu.Lock()
		list := append([]*Stream(nil), all...)
		mu.Unlock()
		var cw sync.WaitGroup
		for i := 0; i < k && len(list) > 0; i++ {
			st := list[r.Intn(len(list))]
			cw.Add(1)
			go func() { defer cw.Done(); st.Close() }()
		}
		cw.Wait()
	}
	how := int(seed % 3)
	switch how {
	case 0: // the socket goes under both sessions
		c.Close()
		srvRaw.Close()
		time.Sleep(cfgOf().KeepAliveTimeout*2 + 50*time.Millisecond)
		cli.Close() // what keepalive left open (parked), its owner closes
		srv.Close()
	case 1: // one session closes as streams close at both ends
		var cw sync.WaitGroup
		cw.Add(2)
		go func() { defer cw.Done(); cli.Close() }()
		go func() { defer cw.Done(); closeSome(8) }()
		cw.Wait()
		time.Sleep(cfgOf().KeepAliveTimeout*2 + 50*time.Millisecond)
		srv.Close()
	case 2: // both close at once, as streams close at both ends
		var cw sync.WaitGroup
		cw.Add(3)
		go func() { defer cw.Done(); cli.Close() }()
		go func() { defer cw.Done(); srv.Close() }()
		go func() { defer cw.Done(); closeSome(8) }()
		cw.Wait()
	}
	time.Sleep(50 * time.Millisecond)
	close(stopIO)
	close(stop)
	core.Wait()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("seed %d: streams did not end", seed)
	}
	for deadline := time.Now().Add(closeGrace + 2*time.Second); pool.Free() != poolSize && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	close(stopWatch)
	watch.Wait()
	if e := checkErr.Load(); e != nil {
		t.Fatalf("seed %d (death %d): books: %v", seed, how, e)
	}
	if b := bad.Load(); b != nil {
		t.Fatalf("seed %d (death %d): the watcher saw %v", seed, how, b)
	}
	if err := poolStrict(pool, poolSize, cli, srv); err != nil {
		t.Fatalf("seed %d (death %d): at the end: %v", seed, how, err)
	}
	if f := pool.Free(); f != poolSize {
		t.Fatalf("seed %d (death %d): pool free %d closeGrace after the end, want %d", seed, how, f, poolSize)
	}
	for _, s := range []*Session{cli, srv} {
		if e, sp := extraOf(s); e != 0 || sp != 0 {
			t.Fatalf("seed %d (death %d): a closed session holds extra %d spare %d", seed, how, e, sp)
		}
	}
	t.Logf("seed %d (death %d): %d checks, claims up to %d KiB; borrows %d/%d, pool short %d/%d", seed, how, checks, maxClaims>>10,
		cli.Borrows(), srv.Borrows(), atomic.LoadUint32(&cli.poolShorts), atomic.LoadUint32(&srv.poolShorts))
}

// dumpStreams says, for a stall, where every stream of the sessions is.
func dumpStreams(sessions ...*Session) string {
	out := ""
	for k, s := range sessions {
		out += fmt.Sprintf("session %d: closed %v bucket %d extra %d spare %d parks %d borrows %d brakes %d\n", k, s.IsClosed(),
			atomic.LoadInt32(&s.bucket), atomic.LoadInt32(&s.extra), atomic.LoadInt32(&s.spare), s.Parks(), s.Borrows(), s.Brakes())
		s.streamLock.Lock()
		for _, st := range s.streams {
			st.bufferLock.Lock()
			out += fmt.Sprintf("  stream %d: rcvWin %d incr %d step %d numRead %d unread %d updLost %v braking %v settling %v preBrake %d claim %d ceil %d | written %d peerConsumed %d peerWindow %d\n",
				st.id, st.rcvWin, st.incr, st.updStep(s.config), st.numRead, st.unread, st.updLost, st.braking, st.settling, st.preBrake, st.claim,
				st.core.ceil.Load(), atomic.LoadUint32(&st.numWritten), atomic.LoadUint32(&st.peerConsumed), atomic.LoadUint32(&st.peerWindow))
			st.bufferLock.Unlock()
		}
		s.streamLock.Unlock()
	}
	return out
}
