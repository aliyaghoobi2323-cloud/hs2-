package smux

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A write returns only once the session's writer is done with its buffer, as
// io.Writer requires: the caller may reuse it at once (hs2's relays hand it
// back to a pool that another relay reads into).

// holdConn writes by WriteBuffers, as kcp-go's sessions do, so the session's
// writer hands it the caller's data itself; a data frame it keeps hold of
// until the test lets go, as a slow link does. Its Close does not let go.
type holdConn struct {
	net.Conn // reads only
	entered  chan struct{}
	release  chan struct{}
	holding  int32 // atomic: a data frame is in WriteBuffers
}

func (c *holdConn) WriteBuffers(v [][]byte) (int, error) {
	n := 0
	for _, b := range v {
		n += len(b)
	}
	if v[0][1] == cmdPSH {
		atomic.StoreInt32(&c.holding, 1)
		c.entered <- struct{}{}
		<-c.release
		atomic.StoreInt32(&c.holding, 0)
	}
	return n, nil
}

// The session dies while the connection holds a data frame: the write that
// queued it waits for the connection to let go of its buffer.
func TestWriteOnDyingSessionWaitsForItsBuffer(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	conn := &holdConn{Conn: c1, entered: make(chan struct{}, 1), release: make(chan struct{})}
	config := DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	type ret struct {
		err  error
		held bool
	}
	done := make(chan ret, 1)
	go func() {
		_, err := st.Write(make([]byte, 1000))
		done <- ret{err, atomic.LoadInt32(&conn.holding) != 0}
	}()
	<-conn.entered
	sess.Close()
	select {
	case r := <-done:
		close(conn.release)
		t.Fatalf("Write returned (%v) while the connection still held its buffer (%v)", r.err, r.held)
	case <-time.After(100 * time.Millisecond):
	}
	close(conn.release)
	select {
	case r := <-done:
		if r.held {
			t.Fatal("Write returned while the connection still held its buffer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write did not return once the connection let go")
	}
}

// The same over a connection without WriteBuffers: the session's writer
// copies the frame, then waits for the link. A write the session's death
// cuts short returns after that copy (-race tells).
func TestWriteOnDyingSessionAfterCopy(t *testing.T) {
	p := newPipeSession(t)
	st := p.open()
	b := make([]byte, 4096)
	done := make(chan error, 1)
	go func() {
		_, err := st.Write(b)
		done <- err
	}()
	p.queued(1 + 1) // the SYN, then the frame: in the writer, which the peer does not read
	p.sess.Close()
	if err := <-done; err == nil {
		t.Fatal("Write on a dead session returned no error")
	}
	for i := range b {
		b[i] = 1 // reused, as relayStream's pool reuses it
	}
}

// A write that times out waiting for the session's writer leaves its frame
// queued, and the frame still goes out (engine/control.go counts on that) --
// with the data it was written with, though the caller reused its buffer.
func TestTimedOutWriteFrameKeepsItsData(t *testing.T) {
	p := newPipeSession(t)
	busy, late := p.open(), p.open()
	go busy.Write(make([]byte, 4096))
	p.queued(2 + 1) // the SYNs, then busy's frame, in the writer
	want := bytes.Repeat([]byte{'a'}, 100)
	b := append([]byte(nil), want...)
	late.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := late.Write(b); err != ErrTimeout {
		t.Fatalf("Write returned %v, want a timeout", err)
	}
	for i := range b {
		b[i] = 'b'
	}
	if f := p.read(); f.sid != busy.ID() {
		t.Fatalf("first frame %+v, want busy's", f)
	}
	var hdr rawHeader
	p.peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(p.peer, hdr[:]); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hdr.Length())
	if _, err := io.ReadFull(p.peer, got); err != nil {
		t.Fatal(err)
	}
	if hdr.Cmd() != cmdPSH || hdr.StreamID() != late.ID() || !bytes.Equal(got, want) {
		t.Fatalf("the timed-out frame went out as cmd %d of stream %d with %q, want %q",
			hdr.Cmd(), hdr.StreamID(), got, want)
	}
}
