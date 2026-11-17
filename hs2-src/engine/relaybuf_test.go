package engine

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// chunkReader returns data in reads of the given sizes (cycled).
type chunkReader struct {
	data  []byte
	sizes []int
	i     int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.sizes[r.i%len(r.sizes)], len(p), len(r.data))
	r.i++
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestCopyLazyExact(t *testing.T) {
	data := make([]byte, 3<<20)
	rand.Read(data)
	for _, sizes := range [][]int{{1}, {100, 5000}, {relaySmall}, {32 << 10}, {relaySmall, 70000, 3, 4095, 4096, 32 << 10, 32<<10 - 1}} {
		var out bytes.Buffer
		n := copyLazy(&out, &chunkReader{data: data, sizes: sizes})
		if n != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("sizes %v: copied %d, equal=%v", sizes, n, bytes.Equal(out.Bytes(), data))
		}
	}
}

type failWriter struct{ left int }

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		n := w.left
		w.left = 0
		return n, io.ErrClosedPipe
	}
	w.left -= len(p)
	return len(p), nil
}

// sizeReader records the buffer size of every Read; it serves bursts, then
// ends.
type sizeReader struct {
	bursts [][]int // per burst, the read sizes it returns (capped by len(p))
	got    []int
}

func (r *sizeReader) Read(p []byte) (int, error) {
	r.got = append(r.got, len(p))
	for len(r.bursts) > 0 && len(r.bursts[0]) == 0 {
		r.bursts = r.bursts[1:]
	}
	if len(r.bursts) == 0 {
		return 0, io.EOF
	}
	n := min(r.bursts[0][0], len(p))
	r.bursts[0] = r.bursts[0][1:]
	return n, nil
}

// After a burst drains (a read short of the buffer), the next Read  -  where a
// real connection waits  -  uses the small buffer.
func TestCopyLazyWaitsSmall(t *testing.T) {
	big := 32 << 10
	r := &sizeReader{bursts: [][]int{{100}, {big, big, big, big, 5000}, {big, big, 31000}, {7}}}
	copyLazy(io.Discard, r)
	// burst 1: 100 into small; burst 2: small (2 KB of the first 32 KB),
	// then big reads until the short 5000 one; burst 3 the same; burst 4: 7.
	usedBig := false
	for _, n := range r.got {
		usedBig = usedBig || n == big
	}
	if !usedBig || r.got[len(r.got)-1] != relaySmall || r.got[len(r.got)-2] != relaySmall {
		t.Fatalf("read sizes %v: a streaming burst must use the big buffer and every wait the small one", r.got)
	}
}

func TestCopyLazyWriteError(t *testing.T) {
	data := make([]byte, 1<<20)
	n := copyLazy(&failWriter{left: 100000}, &chunkReader{data: data, sizes: []int{32 << 10}})
	if n != 100000 {
		t.Fatalf("written %d, want 100000", n)
	}
}

// TestRelayIdleMemoryBounded: many idle relays (each after a 1 MB burst both
// ways) must not pin a 32 KB buffer per direction, and opening and closing
// many connections must leave the heap where it was.
func TestRelayIdleMemoryBounded(t *testing.T) {
	if !relayLazy.Load() {
		t.Skip("HS2_RELAY_LAZY=0")
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	const n = 1000
	type pair struct{ user, peer, a, b net.Conn }
	open := func() []pair {
		ps := make([]pair, n)
		var wg sync.WaitGroup
		for i := range ps {
			u1, u2 := net.Pipe()
			p1, p2 := net.Pipe()
			ps[i] = pair{u1, p1, u2, p2}
			go relay(u2, p2)
			wg.Add(1)
			go func(u, p net.Conn) { // a burst each way, then idle
				defer wg.Done()
				const up, down = 1<<20 + 1234, 64<<10 + 77 // bursts end mid-buffer
				buf := make([]byte, up)
				go u.Write(buf)
				io.ReadFull(p, make([]byte, up))
				go p.Write(buf[:down])
				io.ReadFull(u, make([]byte, down))
			}(u1, p1)
		}
		wg.Wait()
		time.Sleep(50 * time.Millisecond)
		return ps
	}
	closeAll := func(ps []pair) {
		for _, p := range ps {
			p.user.Close()
			p.peer.Close()
		}
	}
	base := heap()
	for round := 0; round < 3; round++ { // churn: open and close many relays
		closeAll(open())
	}
	time.Sleep(100 * time.Millisecond)
	after := heap()
	ps := open()
	idle := heap()
	per := (int64(idle) - int64(after)) / n
	closeAll(ps)
	t.Logf("heap base %d KB, after 3 churn rounds %d KB, %d idle relays +%d KB (%d B each)", base>>10, after>>10, n, (int64(idle)-int64(after))>>10, per)
	// old: 2 x 32 KB pinned per relay; lazy: 2 x relaySmall + net.Pipe and goroutine bookkeeping
	if per > 16<<10 {
		t.Fatalf("idle relay pins %d B, want <= 16 KB", per)
	}
	if after > base+8<<20 {
		t.Fatalf("heap grew %d KB over 3 open/close rounds of %d relays", (after-base)>>10, n)
	}
}
