package smux

import (
	"bytes"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeCore plays the window core against sessions until stop closes: every
// few milliseconds it takes a snapshot of each and hands every stream a
// random ceiling (none, at or under the need, up to MaxStreamBuffer) and
// each session a random reference -- what hs2's engine does every few
// hundred milliseconds, only much more often and with no sense, on
// goroutines of its own, as the engine's are.
func fakeCore(seed int64, stop <-chan struct{}, sessions ...*Session) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := rand.New(rand.NewSource(seed))
		var samples []CoreSample
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(1+r.Intn(4)) * time.Millisecond):
			}
			for _, s := range sessions {
				samples, _ = s.CoreSnapshot(samples)
				max := uint32(s.config.MaxStreamBuffer)
				var list []*Stream
				s.streamLock.Lock()
				for _, st := range s.streams {
					list = append(list, st)
				}
				s.streamLock.Unlock()
				for _, st := range list {
					switch r.Intn(4) {
					case 0:
						st.SetCoreCeil(0)
					case 1:
						st.SetCoreCeil(uint32(r.Intn(int(needOf(s.config)) + 1)))
					default:
						st.SetCoreCeil(uint32(r.Int63n(int64(max) + 1)))
					}
				}
				s.SetCoreRef(time.Duration(r.Intn(3))*time.Millisecond, time.Duration(r.Intn(2))*time.Millisecond, r.Intn(4) > 0)
			}
		}
	}()
	return &wg
}

// Windows past NeedStreamBuffer grow and shrink at random ceilings in many
// streams of two sessions sharing one pool while their readers run fast,
// slow down, pause and stop, the streams close at either end at random,
// and the sessions close in the middle of it (sweeping every 50 ms).
// Whatever the order, the pool is whole once both sessions and every stream
// have closed.
func TestCorePoolWholeAfterChaos(t *testing.T) {
	for round := 0; round < 4; round++ {
		coreChaosRound(t, int64(round))
	}
}

func coreChaosRound(t *testing.T, seed int64) {
	const poolSize = 3 << 20
	pool := NewWindowPool(poolSize)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srvCh := make(chan *Session, 1)
	var wg sync.WaitGroup
	// both ends: a stream's peer writes as fast as it can until either end
	// closes it
	push := func(st *Stream) {
		defer wg.Done()
		buf := make([]byte, 16<<10)
		for {
			st.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := st.Write(buf); err != nil {
				return
			}
		}
	}
	// read in one of four ways, then close (or not) at a random moment
	read := func(st *Stream, r *rand.Rand) {
		defer wg.Done()
		mode := r.Intn(4)
		end := time.Now().Add(time.Duration(50+r.Intn(400)) * time.Millisecond)
		buf := make([]byte, 1+r.Intn(32<<10))
		for time.Now().Before(end) {
			st.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			switch mode {
			case 0: // fast
			case 1: // slow
				time.Sleep(2 * time.Millisecond)
			case 2: // fast, then stopped
				if time.Until(end) < 200*time.Millisecond {
					time.Sleep(time.Until(end))
					continue
				}
			case 3: // pauses
				if r.Intn(20) == 0 {
					time.Sleep(30 * time.Millisecond)
				}
			}
			if _, err := st.Read(buf); err != nil && err != ErrTimeout {
				return
			}
		}
		if r.Intn(3) > 0 {
			st.Close()
		}
	}
	cfgOf := func() *Config {
		c := poolConfig(pool)
		c.KeepAliveDisabled = false
		c.KeepAliveInterval = 50 * time.Millisecond // sweeps, often
		c.KeepAliveTimeout = 10 * time.Second
		return c
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
	r := rand.New(rand.NewSource(seed))
	var streams []*Stream
	for i := 0; i < 24; i++ {
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
		time.Sleep(time.Duration(r.Intn(15)) * time.Millisecond)
	}
	time.Sleep(time.Duration(100+r.Intn(300)) * time.Millisecond)
	if r.Intn(2) == 0 {
		cli.Close()
		srv.Close()
	} else {
		srv.Close()
		cli.Close()
	}
	for _, st := range streams {
		st.Close()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("streams did not end after their sessions closed")
	}
	close(stop)
	core.Wait()
	if f := pool.Free(); f != poolSize {
		t.Fatalf("seed %d: pool free %d after both sessions closed, want %d (lost %d)", seed, f, poolSize, poolSize-f)
	}
	for _, s := range []*Session{cli, srv} {
		if e, sp := extraOf(s); e != 0 || sp != 0 {
			t.Fatalf("seed %d: a closed session holds extra %d spare %d", seed, e, sp)
		}
	}
}

// Streams of two sessions sharing one pool move data both ways (each
// session receives on half of them) while a fake core flips their ceilings
// and references at random and readers pause: every byte arrives in order,
// no window ever exceeds the stream's MaxStreamBuffer, and once the
// sessions close the pool is whole. (Run with -race: the core's goroutine
// against the sessions' readers, writers and recvLoops.)
func TestCoreSessionsShareOnePool(t *testing.T) {
	pool := NewWindowPool(1 << 20)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const streams, size = 8, 3 << 20
	data := func(i int) []byte {
		b := make([]byte, size)
		rand.New(rand.NewSource(int64(i))).Read(b)
		return b
	}
	// recv reads size bytes, pausing now and then, and checks them
	recv := func(st *Stream, i int) error {
		got := make([]byte, 0, size)
		buf := make([]byte, 1+rand.Intn(64<<10))
		for len(got) < size {
			st.SetReadDeadline(time.Now().Add(20 * time.Second))
			n, err := st.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil && len(got) < size {
				return err
			}
			if rand.Intn(50) == 0 {
				time.Sleep(time.Duration(rand.Intn(20)) * time.Millisecond)
			}
		}
		if !bytes.Equal(got, data(i)) {
			return io.ErrUnexpectedEOF
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
		for {
			st, err := s.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				var k [1]byte
				if _, err := io.ReadFull(st, k[:]); err != nil {
					errs <- err
					return
				}
				if i := int(k[0]); i%2 == 0 {
					errs <- recv(st, i) // the client uploads
				} else {
					st.Write(data(i)) // the client downloads
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
	core := fakeCore(7, stop, cli, srv)
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
			go func(i int) { errs <- recv(st, i) }(i)
		}
	}
	for i := 0; i < streams; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	core.Wait()
	for _, s := range []*Session{cli, srv} {
		s.streamLock.Lock()
		for _, st := range s.streams {
			if w := rcvWinOf(st); w > uint32(s.config.MaxStreamBuffer) {
				t.Errorf("window %d past MaxStreamBuffer", w)
			}
		}
		s.streamLock.Unlock()
	}
	cli.Close()
	srv.Close()
	if pool.Free() != 1<<20 {
		t.Fatalf("pool free %d after both sessions closed, want all of it", pool.Free())
	}
}
