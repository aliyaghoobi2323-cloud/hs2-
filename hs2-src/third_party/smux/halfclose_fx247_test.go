package smux

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// stuckConn: writes block while stuck is set (a dark link before keepalive).
type stuckConn struct {
	io.ReadWriteCloser
	stuck   atomic.Bool
	release chan struct{}
}

func (c *stuckConn) Write(b []byte) (int, error) {
	if c.stuck.Load() {
		<-c.release
	}
	return c.ReadWriteCloser.Write(b)
}

// hs2 fx247 (audit246 L2): after a CloseWriteTimeout failed (the writer
// stuck), Close's abandon frames share one short deadline: Close returns in
// ~abandonCloseTimeout, not 3 x openCloseTimeout.
func TestHalfCloseAbandonDeadline(t *testing.T) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Version = 2
	srv, err := Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sc := &stuckConn{ReadWriteCloser: c1, release: make(chan struct{})}
	c, err := Client(sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(sc.release); c.Close(); srv.Close() }()
	go srv.AcceptStream()
	cs, err := c.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	cs.Write([]byte{0})
	time.Sleep(50 * time.Millisecond)
	cs.SetHalfOpen(true)
	sc.stuck.Store(true)
	go cs.Write([]byte{1}) // the writer blocks on this one
	time.Sleep(50 * time.Millisecond)
	if err := cs.CloseWriteTimeout(200 * time.Millisecond); err == nil {
		t.Fatal("CloseWriteTimeout went through a stuck writer")
	}
	t0 := time.Now()
	cs.Close()
	if d := time.Since(t0); d > abandonCloseTimeout+2*time.Second {
		t.Fatalf("Close took %v on a stuck writer", d)
	}
}
