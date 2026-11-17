package smux

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// hs2 hc183: CloseWrite sends a FIN and keeps reading; with SetHalfOpen the
// peer's writes outlive that FIN past the window (v2), without it they end
// at the FIN as upstream.
func hcPair(t *testing.T) (*Stream, *Stream) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Version = 2
	s, err := Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Client(c1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	acc := make(chan *Stream, 1)
	go func() { ss, _ := s.AcceptStream(); acc <- ss }()
	cs, err := c.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	cs.Write([]byte{0}) // the SYN goes; the server stream exists
	ss := <-acc
	b := make([]byte, 1)
	io.ReadFull(ss, b)
	return cs, ss
}

func TestHalfCloseWrite(t *testing.T) {
	for _, open := range []bool{true, false} {
		cs, ss := hcPair(t)
		ss.SetHalfOpen(open)
		cs.Write([]byte("req"))
		if err := cs.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Write([]byte("x")); err != io.ErrClosedPipe {
			t.Fatalf("write after CloseWrite: %v", err)
		}
		req, err := io.ReadAll(ss) // the FIN ends the server's reads
		if err != nil || string(req) != "req" {
			t.Fatalf("req %q %v", req, err)
		}
		resp := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB > the window
		werr := make(chan error, 1)
		go func() { _, err := ss.Write(resp); werr <- err }()
		if !open {
			select {
			case err := <-werr:
				if err != io.EOF {
					t.Fatalf("upstream: write after the peer's FIN: %v, want EOF", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("upstream: write never gave up")
			}
			continue
		}
		cs.SetReadDeadline(time.Now().Add(10 * time.Second))
		got := make([]byte, len(resp))
		if _, err := io.ReadFull(cs, got); err != nil || !bytes.Equal(got, resp) {
			t.Fatalf("half-open: client read %v", err)
		}
		if err := <-werr; err != nil {
			t.Fatal(err)
		}
		ss.Close()
		if n, err := cs.Read(got); n != 0 || err != io.EOF {
			t.Fatalf("end: %d %v", n, err)
		}
		cs.Close() // no second FIN; nothing breaks
	}
}

// hs2 hc183b: a half-open side that sent its FIN (CloseWrite) and then drops
// the stream (Close) sends a second FIN: the peer's PeerGone closes. A plain
// Close (one FIN) or a peer not half-open does not.
func TestHalfClosePeerGone(t *testing.T) {
	cs, ss := hcPair(t)
	cs.SetHalfOpen(true)
	cs.CloseWrite()
	if _, err := io.ReadAll(ss); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ss.PeerGone():
		t.Fatal("PeerGone on the first FIN")
	case <-time.After(200 * time.Millisecond):
	}
	cs.Close()
	select {
	case <-ss.PeerGone():
	case <-time.After(5 * time.Second):
		t.Fatal("no PeerGone after the half-closed peer's Close")
	}

	cs, ss = hcPair(t) // not half-open: Close after CloseWrite sends no second FIN
	cs.CloseWrite()
	io.ReadAll(ss)
	cs.Close()
	select {
	case <-ss.PeerGone():
		t.Fatal("PeerGone from a peer that is not half-open")
	case <-time.After(300 * time.Millisecond):
	}

	// hc213 (audit211 H1): a plain Close of a half-open stream (no
	// CloseWrite first) abandons it: PeerGone, and already when the
	// reader sees the EOF (the empty push goes ahead of the FIN)
	for i := 0; i < 50; i++ {
		cs, ss = hcPair(t)
		cs.SetHalfOpen(true)
		cs.Close()
		io.ReadAll(ss)
		select {
		case <-ss.PeerAbandoned():
		default:
			t.Fatalf("run %d: no PeerAbandoned at the EOF of an abandoned stream", i)
		}
		<-ss.PeerGone()
	}

	cs, ss = hcPair(t) // not half-open: a plain Close is one FIN, as upstream
	cs.Close()
	io.ReadAll(ss)
	select {
	case <-ss.PeerGone():
		t.Fatal("PeerGone from a plain Close toward a peer that is not half-open")
	case <-time.After(300 * time.Millisecond):
	}
}

// hc213 (audit211 L1): CloseWriteTimeout sends the FIN like CloseWrite; a
// later Close then is the "dropped" FIN only.
func TestHalfCloseWriteTimeout(t *testing.T) {
	cs, ss := hcPair(t)
	cs.SetHalfOpen(true)
	if err := cs.CloseWriteTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(ss); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ss.PeerGone():
		t.Fatal("PeerGone after a CloseWrite")
	case <-time.After(200 * time.Millisecond):
	}
	if err := cs.CloseWriteTimeout(time.Second); err != io.ErrClosedPipe {
		t.Fatalf("second CloseWriteTimeout: %v", err)
	}
	cs.Close()
	select {
	case <-ss.PeerGone():
	case <-time.After(5 * time.Second):
		t.Fatal("no PeerGone after Close")
	}
	select {
	case <-ss.PeerAbandoned():
		t.Fatal("PeerAbandoned after a CloseWrite and Close (a normal end)")
	default:
	}
}
