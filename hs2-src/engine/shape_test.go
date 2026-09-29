package engine

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// A shapedConn writer feeding a shapedConn reader must deliver the exact byte
// stream, for arbitrary write sizes and arbitrary read-buffer sizes (smux reads
// with buffers unrelated to our frame sizes).
func TestShapedConnRoundTrip(t *testing.T) {
	cw, cr := net.Pipe()
	defer cw.Close()
	defer cr.Close()
	w := newShapedConn(cw, obfs.NewHTTPSLengthSampler())
	r := newShapedConn(cr, obfs.NewHTTPSLengthSampler())

	// A mix of tiny control-sized writes and large bulk writes.
	var payload bytes.Buffer
	writes := [][]byte{}
	sizes := []int{1, 7, 40, 500, 1400, 1500, 16 << 10, 3, 65000, 128}
	for _, s := range sizes {
		b := make([]byte, s)
		rand.Read(b)
		writes = append(writes, b)
		payload.Write(b)
	}
	want := payload.Bytes()

	go func() {
		for _, b := range writes {
			if _, err := w.Write(b); err != nil {
				return
			}
		}
	}()

	got := make([]byte, 0, len(want))
	// Read back with deliberately awkward, varying buffer sizes.
	bufSizes := []int{1, 3, 64, 1400, 5000, 200}
	i := 0
	for len(got) < len(want) {
		bs := bufSizes[i%len(bufSizes)]
		i++
		buf := make([]byte, bs)
		cr.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := r.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			t.Fatalf("read error after %d/%d bytes: %v", len(got), len(want), err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

// Every frame shapedConn emits must be a single write whose size is drawn from
// the sampler (so it becomes one TLS record of that size). We capture the raw
// writes and assert each one's size never exceeds the sampler's max, and that a
// bulk transfer actually produces a spread of sizes (not one fixed size).
func TestShapedConnRecordSizes(t *testing.T) {
	cap := &captureConn{}
	w := newShapedConn(cap, obfs.NewHTTPSLengthSampler())

	bulk := make([]byte, 256<<10)
	rand.Read(bulk)
	if _, err := w.Write(bulk); err != nil {
		t.Fatal(err)
	}

	if len(cap.sizes) < 2 {
		t.Fatalf("expected many frames for a 256KiB write, got %d", len(cap.sizes))
	}
	const maxSampler = 1400 // largest size in NewHTTPSLengthSampler
	distinct := map[int]int{}
	overhead := 0
	for _, s := range cap.sizes {
		if s > maxSampler {
			t.Fatalf("a frame of %d bytes exceeds the sampler max %d — record too big", s, maxSampler)
		}
		distinct[s]++
		overhead += shapeHdrLen
	}
	if len(distinct) < 3 {
		t.Fatalf("record sizes not varied (a fingerprint): only %d distinct sizes", len(distinct))
	}
	// Padding+header overhead on a bulk transfer must stay modest.
	ratio := float64(cap.total-len(bulk)) / float64(len(bulk))
	t.Logf("frames=%d distinct-sizes=%d wire-overhead=%.2f%%", len(cap.sizes), len(distinct), ratio*100)
	if ratio > 0.15 {
		t.Fatalf("bulk overhead %.1f%% too high (want <15%%)", ratio*100)
	}
}

// A short, standalone write is padded UP (real bytes added), so it does not
// betray its true small size on the wire.
func TestShapedConnPadsSmallWrite(t *testing.T) {
	cap := &captureConn{}
	w := newShapedConn(cap, obfs.NewHTTPSLengthSampler())
	if _, err := w.Write([]byte("hi")); err != nil { // 2 bytes of real data
		t.Fatal(err)
	}
	if len(cap.sizes) != 1 {
		t.Fatalf("want 1 frame, got %d", len(cap.sizes))
	}
	if cap.sizes[0] <= 2+shapeHdrLen {
		t.Fatalf("small write not padded: on-wire size %d", cap.sizes[0])
	}
}

// captureConn records the size of every Write (each becomes one TLS record).
type captureConn struct {
	net.Conn
	mu    sync.Mutex
	sizes []int
	total int
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.sizes = append(c.sizes, len(p))
	c.total += len(p)
	c.mu.Unlock()
	return len(p), nil
}

var _ io.Writer = (*captureConn)(nil)
