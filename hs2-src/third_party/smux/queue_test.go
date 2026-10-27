package smux

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// fifoQueue is the plainest FrameQueue: one writer, frames in the order
// pushed. It records what it was handed.
type fifoQueue struct {
	mu     sync.Mutex
	q      []*OutFrame
	wake   chan struct{}
	conn   io.Writer
	size   int // FrameSize for every stream (0: none)
	pushed []pushedFrame
	hold   bool // the writer writes nothing (a stuck link)
	fail   error
}

type pushedFrame struct {
	sid     uint32
	control bool
	fin     bool
	data    int
	user    uint32
	hint    byte
}

func newFifoQueue() *fifoQueue { return &fifoQueue{wake: make(chan struct{}, 1)} }

func (f *fifoQueue) Push(of *OutFrame) {
	f.mu.Lock()
	f.q = append(f.q, of)
	u, h := of.User()
	f.pushed = append(f.pushed, pushedFrame{of.Sid(), of.Control(), of.Fin(), of.DataLen(), u, h})
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fifoQueue) FrameSize(uint32) int { return f.size }

func (f *fifoQueue) frames() []pushedFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushedFrame(nil), f.pushed...)
}

func (f *fifoQueue) run(die <-chan struct{}) {
	var buf []byte
	for {
		f.mu.Lock()
		var of *OutFrame
		if len(f.q) > 0 && !f.hold {
			of, f.q = f.q[0], f.q[1:]
		}
		fail := f.fail
		f.mu.Unlock()
		if of == nil {
			select {
			case <-f.wake:
				continue
			case <-die:
				f.mu.Lock()
				for _, of := range f.q {
					of.Drop()
				}
				f.q = nil
				f.mu.Unlock()
				return
			}
		}
		buf = of.AppendTo(buf[:0])
		if fail != nil {
			of.Done(0, fail)
			continue
		}
		n, err := f.conn.Write(buf)
		of.Done(n, err)
	}
}

// queuedPair is a client and a server session over a pipe, each with a
// fifoQueue.
func queuedPair(t *testing.T, cfg *Config) (cli, srv *Session, cq, sq *fifoQueue) {
	t.Helper()
	a, b := net.Pipe()
	cq, sq = newFifoQueue(), newFifoQueue()
	cq.conn, sq.conn = a, b
	var err error
	if cli, err = ClientQueued(a, cfg, cq); err != nil {
		t.Fatal(err)
	}
	if srv, err = ServerQueued(b, cfg, sq); err != nil {
		t.Fatal(err)
	}
	go cq.run(cli.CloseChan())
	go sq.run(srv.CloseChan())
	t.Cleanup(func() { cli.Close(); srv.Close() })
	return cli, srv, cq, sq
}

func queuedConfig() *Config {
	c := DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 << 10
	c.MaxStreamBuffer = 1 << 20
	return c
}

// A queued session carries streams both ways as the session's own writer
// did; the queue sees every frame, data and control, with the user tag of
// the stream's SYN, and the streams' frames keep to its FrameSize.
func TestQueuedSessionCarriesStreams(t *testing.T) {
	cli, srv, cq, sq := queuedPair(t, queuedConfig())
	cq.size = 3000
	const n = 200 << 10
	want := make([]byte, n)
	rand.Read(want)
	st, err := cli.OpenStreamSyn(EncodeSyn(0xabcd, 1))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		ss, err := srv.AcceptStream()
		if err != nil {
			done <- nil
			return
		}
		got := make([]byte, n)
		if _, err := io.ReadFull(ss, got); err != nil {
			done <- nil
			return
		}
		ss.Write(got) // and back
		done <- got
	}()
	if _, err := st.Write(want); err != nil {
		t.Fatal(err)
	}
	if got := <-done; !bytes.Equal(got, want) {
		t.Fatal("server read other bytes")
	}
	back := make([]byte, n)
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(st, back); err != nil || !bytes.Equal(back, want) {
		t.Fatalf("echo: %v", err)
	}
	st.Close()
	var syn, data, fin int
	for _, f := range cq.frames() {
		switch {
		case !f.control:
			data++
			if f.data > 3000 {
				t.Fatalf("data frame of %d B past FrameSize 3000", f.data)
			}
			if f.user != 0xabcd || f.hint != 1 {
				t.Fatalf("data frame user %#x hint %d", f.user, f.hint)
			}
		case f.fin:
			fin++
		case f.data == SynPayloadLen:
			syn++
		}
	}
	if syn != 1 || fin != 1 || data < n/3000 {
		t.Fatalf("client queue: syn %d fin %d data %d", syn, fin, data)
	}
	// the server's UPDs and its data, whose frames keep MaxFrameSize (its
	// queue sets none)
	var upd, big int
	for _, f := range sq.frames() {
		if f.control && f.data == szCmdUPD {
			upd++
		}
		if !f.control && f.data > 3000 {
			big++
		}
	}
	if upd == 0 || big == 0 {
		t.Fatalf("server queue: upd %d, frames past 3000 B %d", upd, big)
	}
}

// A stream's writer waits for its frame's Done: with a writer that writes
// nothing, a Write does not return, and the session's death ends it.
func TestQueuedWriterWaitsForDone(t *testing.T) {
	cli, srv, cq, _ := queuedPair(t, queuedConfig())
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if s, err := srv.AcceptStream(); err == nil {
			io.Copy(io.Discard, s)
		}
	}()
	cq.mu.Lock()
	cq.hold = true
	cq.mu.Unlock()
	ret := make(chan error, 1)
	go func() { _, err := st.Write(make([]byte, 100)); ret <- err }()
	select {
	case err := <-ret:
		t.Fatalf("Write returned (%v) before its frame was written", err)
	case <-time.After(200 * time.Millisecond):
	}
	cli.Close()
	select {
	case err := <-ret:
		if err == nil {
			t.Fatal("Write succeeded on a dead session")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write still blocked after the session closed")
	}
}

// A failed write reported with Done fails the session's writers.
func TestQueuedDoneErrorFailsSession(t *testing.T) {
	cli, srv, cq, _ := queuedPair(t, queuedConfig())
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if s, err := srv.AcceptStream(); err == nil {
			io.Copy(io.Discard, s)
		}
	}()
	boom := errors.New("boom")
	cq.mu.Lock()
	cq.fail = boom
	cq.mu.Unlock()
	if _, err := st.Write([]byte("x")); !errors.Is(err, boom) {
		t.Fatalf("first write: %v, want boom", err)
	}
	if _, err := st.Write([]byte("y")); err == nil {
		t.Fatal("a write after the failure succeeded")
	}
	if _, err := cli.OpenStream(); err == nil {
		t.Fatal("a stream opened after the failure")
	}
}

// DataFrame: frames the queue schedules on its own, counted against the
// peer's window (Room), and delivered in the stream as written.
func TestQueuedDataFrameWindow(t *testing.T) {
	cli, srv, cq, _ := queuedPair(t, queuedConfig())
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Room(); r != initialPeerWindow {
		t.Fatalf("room %d, want %d", r, initialPeerWindow)
	}
	// the receiver does not read: the window fills
	sent := 0
	for i := 0; ; i++ {
		p := bytes.Repeat([]byte{byte(i)}, 1000)
		of, ok := st.DataFrame(p)
		if !ok {
			break
		}
		cq.Push(of)
		sent += len(p)
		if sent > 2*initialPeerWindow {
			t.Fatal("DataFrame ignores the window")
		}
	}
	if sent < initialPeerWindow-1000 || st.Room() >= 1000 {
		t.Fatalf("sent %d, room %d", sent, st.Room())
	}
	if _, ok := st.DataFrame(nil); ok {
		t.Fatal("an empty DataFrame")
	}
	got := make([]byte, sent)
	ss.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(ss, got); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < sent/1000; i++ {
		if got[i*1000] != byte(i) {
			t.Fatalf("frame %d out of order", i)
		}
	}
	// the reader's updates give the room back
	deadline := time.Now().Add(5 * time.Second)
	for st.Room() < initialPeerWindow/2 {
		if time.Now().After(deadline) {
			t.Fatalf("room %d after the reader read everything", st.Room())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cli.Queue() != cq || st.Session() != cli {
		t.Fatal("Queue or Session")
	}
}

// Many writers of many streams at once: every byte arrives, in order per
// stream (run with -race).
func TestQueuedManyStreams(t *testing.T) {
	cli, srv, cq, _ := queuedPair(t, queuedConfig())
	cq.size = 1500
	const streams, per = 16, 64 << 10
	errs := make(chan error, 2*streams)
	read := make(chan error, streams)
	go func() {
		for {
			ss, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				got := make([]byte, per)
				ss.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(ss, got); err != nil {
					read <- err
					return
				}
				for j := range got {
					if got[j] != byte(j*7+int(got[0])) {
						read <- errors.New("bytes out of order")
						return
					}
				}
				read <- nil
			}()
		}
	}()
	var ww sync.WaitGroup
	for i := 0; i < streams; i++ {
		ww.Add(1)
		go func(i int) {
			defer ww.Done()
			st, err := cli.OpenStream()
			if err != nil {
				errs <- err
				return
			}
			b := make([]byte, per)
			for j := range b {
				b[j] = byte(j*7 + i)
			}
			if _, err := st.Write(b); err != nil {
				errs <- err
			}
		}(i)
	}
	ww.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	for i := 0; i < streams; i++ {
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	}
}
