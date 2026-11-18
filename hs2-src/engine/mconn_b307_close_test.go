package engine

import (
	"bytes"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// blockRW: a sub whose Close blocks until release is closed.
type blockRW struct {
	release chan struct{}
	closed  atomic.Bool
	calls   atomic.Int32
}

func (b *blockRW) Read(p []byte) (int, error)  { <-b.release; return 0, io.EOF }
func (b *blockRW) Write(p []byte) (int, error) { return len(p), nil }
func (b *blockRW) Close() error {
	b.calls.Add(1)
	<-b.release
	b.closed.Store(true)
	return nil
}

func b307Sub(rw mSub) *msub {
	m := newMConn(0xb307, false, nil)
	m.mu.Lock()
	s := m.addSubLocked(rw, nil, false)
	m.mu.Unlock()
	return s
}

// b307KeyOn turns the async close on for one test (the default is off since
// b310) and restores the previous value afterwards.
func b307KeyOn(t *testing.T) {
	t.Helper()
	prev := b307AsyncClose.Load()
	b307AsyncClose.Store(true)
	t.Cleanup(func() { b307AsyncClose.Store(prev) })
}

// b315: the key is on by default; envdef lists it as "1".
func TestB307CloseDefaultOn(t *testing.T) {
	if !b307AsyncCloseDefault {
		t.Fatal("HS2_B307_ASYNCCLOSE must default to on")
	}
}

func b307WaitGoroutines(t *testing.T, base int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines: %d > %d", runtime.NumGoroutine(), base)
}

// A close whose stream close blocks returns at once for every caller; the
// stream close finishes later, in order (rw.Close, onClose, rwClosed),
// once, with no goroutine left.
func TestB307CloseNonBlocking(t *testing.T) {
	b307KeyOn(t)
	base := runtime.NumGoroutine()
	rw := &blockRW{release: make(chan struct{})}
	s := b307Sub(rw)
	var onAt atomic.Int32 // 1: onClose ran after rw.Close finished
	s.onClose = func() {
		if rw.closed.Load() {
			onAt.Store(1)
		} else {
			onAt.Store(2)
		}
	}
	a0, l0 := b307Close.async.Load(), b307Close.late.Load()
	t0 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.close() }()
	}
	wg.Wait()
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("close blocked %v", d)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("done not closed")
	}
	if s.rwClosed.Load() || onAt.Load() != 0 {
		t.Fatal("rwClosed/onClose before the stream close finished")
	}
	if b307Close.async.Load() != a0+1 {
		t.Fatalf("async %d", b307Close.async.Load()-a0)
	}
	close(rw.release)
	for i := 0; i < 200 && !s.rwClosed.Load(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.rwClosed.Load() || onAt.Load() != 1 || rw.calls.Load() != 1 {
		t.Fatalf("rwClosed=%v onClose=%d calls=%d", s.rwClosed.Load(), onAt.Load(), rw.calls.Load())
	}
	for i := 0; i < 200 && b307Close.late.Load() != l0+1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if b307Close.late.Load() != l0+1 {
		t.Fatal("late not counted")
	}
	b307WaitGoroutines(t, base)
}

// A fast stream close is finished when close returns (as before b307).
func TestB307CloseFastSync(t *testing.T) {
	b307KeyOn(t)
	rw := &blockRW{release: make(chan struct{})}
	close(rw.release)
	s := b307Sub(rw)
	ran := false
	s.onClose = func() { ran = true }
	s.close()
	if !s.rwClosed.Load() || !ran || !rw.closed.Load() {
		t.Fatal("fast close not finished on return")
	}
}

// The key off: the synchronous close of rel69m.
func TestB307CloseKeyOff(t *testing.T) {
	prev := b307AsyncClose.Load()
	b307AsyncClose.Store(false)
	defer b307AsyncClose.Store(prev)
	rw := &blockRW{release: make(chan struct{})}
	s := b307Sub(rw)
	ret := make(chan struct{})
	go func() { s.close(); close(ret) }()
	select {
	case <-ret:
		t.Fatal("key off: close returned before the stream close")
	case <-time.After(50 * time.Millisecond):
	}
	close(rw.release)
	<-ret
	if !s.rwClosed.Load() {
		t.Fatal("rwClosed")
	}
}

// A smux stream FIN, queued behind a writer that is stuck, still follows
// all its data: the peer reads every byte, then EOF; close itself did not
// wait for the writer.
func TestB307CloseFINAfterData(t *testing.T) {
	b307KeyOn(t)
	base := runtime.NumGoroutine()
	ca, cb := net.Pipe()
	cli, err := smux.Client(ca, newSmuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smux.Server(cb, newSmuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	acc := make(chan *smux.Stream, 1)
	go func() {
		st, err := srv.AcceptStream()
		if err == nil {
			acc <- st
		}
	}()
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("b307-fin-order."), 4000) // 60 kB
	wErr := make(chan error, 1)
	go func() { _, err := st.Write(data); wErr <- err }()
	peer := <-acc
	time.Sleep(20 * time.Millisecond)
	s := b307Sub(st)
	t0 := time.Now()
	s.close()
	s.close()
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("close waited %v", d)
	}
	got, err := io.ReadAll(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-wErr; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("peer got %d of %d bytes", len(got), len(data))
	}
	for i := 0; i < 400 && !s.rwClosed.Load(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.rwClosed.Load() {
		t.Fatal("stream close never finished")
	}
	peer.Close()
	cli.Close()
	srv.Close()
	b307WaitGoroutines(t, base)
}
