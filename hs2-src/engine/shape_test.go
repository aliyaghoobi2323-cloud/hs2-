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
	"github.com/xtaci/smux"
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

// captureConn records the size of every Write (each becomes one TLS record). If
// an underlying Conn is set the write is forwarded, so it can sit on a real link.
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
	if c.Conn != nil {
		return c.Conn.Write(p)
	}
	return len(p), nil
}

func (c *captureConn) snapshot() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.sizes...)
}

var _ io.Writer = (*captureConn)(nil)

// Every stream carrier (mtcp, l3mtcp, tls) builds its smux session through
// newSession, so shaping is applied at that one seam for all of them. This test
// drives a full smux session built by newSession over a real socket and proves
// the underlying record sizes are shaped — i.e. the shaping is not specific to
// mtcp but covers any carrier that rides newSession.
func TestNewSessionShapesAnyStreamCarrier(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan *smux.Session, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s, err := newSession(c, true, nil, nil) // server end (like the exit/kharej)
		if err != nil {
			return
		}
		got <- s
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cap := &captureConn{Conn: raw}
	cli, err := newSession(cap, false, nil, nil) // client end (like the edge/iran)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv := <-got
	defer srv.Close()

	// Server echoes one stream.
	go func() {
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		io.Copy(st, st)
	}()

	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	msg := make([]byte, 256<<10)
	rand.Read(msg)
	done := make(chan error, 1)
	go func() {
		if _, err := st.Write(msg); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	st.SetReadDeadline(time.Now().Add(10 * time.Second))
	back := make([]byte, len(msg))
	if _, err := io.ReadFull(st, back); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(back, msg) {
		t.Fatal("echo mismatch through shaped newSession")
	}

	sizes := cap.snapshot()
	const maxSampler = 1400
	distinct := map[int]int{}
	big := 0
	for _, s := range sizes {
		if s > maxSampler {
			big++
		}
		distinct[s]++
	}
	t.Logf("underlying writes=%d distinct-sizes=%d over-max=%d", len(sizes), len(distinct), big)
	if big > 0 {
		t.Fatalf("%d underlying records exceeded the sampler max %d — shaping not applied on this carrier", big, maxSampler)
	}
	if len(distinct) < 3 {
		t.Fatalf("record sizes not varied (%d distinct) — shaping ineffective", len(distinct))
	}
}
