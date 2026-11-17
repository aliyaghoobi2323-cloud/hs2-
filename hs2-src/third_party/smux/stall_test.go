package smux

import (
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// hs2 mem67: the frozen-session guard (stall.go).

func stallConfig(stall time.Duration) *Config {
	c := DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 8 << 10
	c.MaxReceiveBuffer = 64 << 10
	c.MaxStreamBuffer = 64 << 10 // one unread stream can empty the bucket
	c.KeepAliveInterval = 200 * time.Millisecond
	c.KeepAliveTimeout = 5 * time.Second
	c.StallTimeout = stall
	return c
}

func stallPair(t *testing.T, stall time.Duration) (cli, srv *Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc := <-ch
	if srv, err = Server(sc, stallConfig(stall)); err != nil {
		t.Fatal(err)
	}
	if cli, err = Client(cc, stallConfig(stall)); err != nil {
		t.Fatal(err)
	}
	return
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base+2 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines %d, baseline %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A peer that stops reading one stream must not freeze the whole session:
// without the guard stream B never gets a byte once unread stream A emptied
// the bucket (TestStallWithoutGuardFreezes shows it); with it A is evicted and
// B flows.
func TestStallUnreadStreamEvicted(t *testing.T) {
	base := runtime.NumGoroutine()
	baseStreams, baseSess := LiveStreams(), LiveSessions()
	ev0, _, _ := StallCounts()
	cli, srv := stallPair(t, 600*time.Millisecond)

	a, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	sa, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := cli.OpenStream()
	sb, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	// A: written, never read on the server, until its window is full
	aerr := make(chan error, 1)
	go func() {
		chunk := make([]byte, 16<<10)
		for {
			if _, err := a.Write(chunk); err != nil {
				aerr <- err
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond) // A has emptied the server's bucket
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	sb.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, 5)
	t0 := time.Now()
	if _, err := io.ReadFull(sb, got); err != nil {
		t.Fatalf("stream B frozen behind unread stream A: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("B got %q", got)
	}
	t.Logf("B unfrozen after %v", time.Since(t0))
	if ev, _, _ := StallCounts(); ev <= ev0 {
		t.Fatalf("no eviction counted")
	}
	if _, err := sa.Read(make([]byte, 10)); !errors.Is(err, ErrStreamEvicted) {
		t.Fatalf("evicted stream read: %v, want ErrStreamEvicted", err)
	}
	select { // the FIN reaches the writer of A
	case err := <-aerr:
		t.Logf("writer of A: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("writer of A still blocked after the eviction")
	}
	if srv.IsClosed() || cli.IsClosed() {
		t.Fatal("the guard closed a session that only had an unread stream")
	}
	// the session keeps working both ways
	for i := 0; i < 20; i++ {
		c, _ := cli.OpenStream()
		s, err := srv.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		c.Write(make([]byte, 32<<10))
		if _, err := io.ReadFull(s, make([]byte, 32<<10)); err != nil {
			t.Fatal(err)
		}
		c.Close()
		s.Close()
	}
	b.Close()
	sb.Close()
	cli.Close()
	srv.Close()
	waitGoroutines(t, base)
	if LiveStreams() != baseStreams || LiveSessions() != baseSess {
		t.Fatalf("streams %d sessions %d, baseline %d %d", LiveStreams(), LiveSessions(), baseStreams, baseSess)
	}
}

// Control: with the guard off the same pattern freezes B (the bug).
func TestStallWithoutGuardFreezes(t *testing.T) {
	base := runtime.NumGoroutine()
	cli, srv := stallPair(t, 0)
	a, _ := cli.OpenStream()
	if _, err := srv.AcceptStream(); err != nil {
		t.Fatal(err)
	}
	b, _ := cli.OpenStream()
	sb, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		chunk := make([]byte, 16<<10)
		for {
			if _, err := a.Write(chunk); err != nil {
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	// the write itself may block for good (the client's socket is full: the
	// Iran-side symptom), so it runs on its own
	go b.Write([]byte("hello"))
	sb.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := io.ReadFull(sb, make([]byte, 5)); err == nil {
		t.Fatal("B got data with the guard off: the test no longer reproduces the freeze")
	}
	cli.Close()
	srv.Close()
	waitGoroutines(t, base)
}

// A slow reader that keeps reading is never evicted, however full the bucket.
func TestStallSlowReaderKept(t *testing.T) {
	base := runtime.NumGoroutine()
	ev0, _, _ := StallCounts()
	cli, srv := stallPair(t, 400*time.Millisecond)
	a, _ := cli.OpenStream()
	sa, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		chunk := make([]byte, 16<<10)
		for {
			if _, err := a.Write(chunk); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 512)
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); {
		if _, err := sa.Read(buf); err != nil {
			t.Fatalf("slow reader: %v", err)
		}
		time.Sleep(100 * time.Millisecond) // well under StallTimeout/2 between reads
	}
	if ev, _, _ := StallCounts(); ev != ev0 {
		t.Fatalf("slow but live reader evicted")
	}
	cli.Close()
	srv.Close()
	waitGoroutines(t, base)
}

// stallConn: reads block until closed; writes block while frozen, else take
// delay each (a slow but moving socket).
type stallConn struct {
	mu     sync.Mutex
	frozen bool
	delay  time.Duration
	die    chan struct{}
	once   sync.Once
	wrote  int
}

func newStallConn() *stallConn { return &stallConn{die: make(chan struct{})} }

func (c *stallConn) Read(b []byte) (int, error) { <-c.die; return 0, io.EOF }
func (c *stallConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	frozen, d := c.frozen, c.delay
	c.mu.Unlock()
	if frozen {
		<-c.die
		return 0, io.ErrClosedPipe
	}
	select {
	case <-time.After(d):
	case <-c.die:
		return 0, io.ErrClosedPipe
	}
	c.mu.Lock()
	c.wrote += len(b)
	c.mu.Unlock()
	return len(b), nil
}
func (c *stallConn) Close() error { c.once.Do(func() { close(c.die) }); return nil }

// The watchdog closes a session whose socket stopped taking bytes, and every
// writer blocked on it returns.
func TestStallFrozenSendClosesSession(t *testing.T) {
	base := runtime.NumGoroutine()
	_, _, sc0 := StallCounts()
	conn := newStallConn()
	conn.frozen = true
	cfg := stallConfig(300 * time.Millisecond)
	cfg.KeepAliveDisabled = true
	s, err := Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			st, err := s.OpenStream()
			if err != nil {
				errs <- err
				return
			}
			_, err = st.Write(make([]byte, 4096))
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("write on a frozen session succeeded")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("writers still blocked: watchdog did not close the frozen session")
		}
	}
	if !s.IsClosed() {
		t.Fatal("session not closed")
	}
	if _, _, sc := StallCounts(); sc <= sc0 {
		t.Fatal("send close not counted")
	}
	waitGoroutines(t, base)
}

// A slow but moving socket is never taken for a frozen one: progress, not
// rate.
func TestStallSlowSendKept(t *testing.T) {
	base := runtime.NumGoroutine()
	conn := newStallConn()
	conn.delay = 150 * time.Millisecond // each frame write well under StallTimeout
	cfg := stallConfig(300 * time.Millisecond)
	cfg.KeepAliveDisabled = true
	s, err := Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st.SetWriteDeadline(time.Now().Add(1500 * time.Millisecond))
	st.Write(make([]byte, 64<<10)) // ~8 frames, ~1.2 s of writing
	if s.IsClosed() {
		t.Fatal("watchdog closed a slow but moving session")
	}
	s.Close()
	waitGoroutines(t, base)
}
