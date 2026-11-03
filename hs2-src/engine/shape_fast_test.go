package engine

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	"io"
	"math"
	mrand "math/rand/v2"
	"net"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// The per-link generator draws record sizes from the same distribution as the
// shared crypto/rand sampler: a chi-square test over 200k draws of each.
func TestShapeFastSameSizeDistribution(t *testing.T) {
	s := obfs.NewHTTPSLengthSampler()
	var seed [32]byte
	crand.Read(seed[:])
	r := mrand.New(mrand.NewChaCha8(seed))
	const n = 200000
	slow, fast := map[int]float64{}, map[int]float64{}
	for i := 0; i < n; i++ {
		slow[s.Sample()]++
		fast[s.At(r.Float64())]++
	}
	if len(slow) != len(fast) {
		t.Fatalf("different size sets: %v vs %v", slow, fast)
	}
	chi := 0.0
	for size, a := range slow {
		b := fast[size]
		chi += (a - b) * (a - b) / (a + b)
	}
	// 9 sizes: 8 degrees of freedom; 26.1 is the 0.1% critical value.
	if chi > 26.1 {
		t.Fatalf("size distributions differ: chi-square %.1f (8 dof, p<0.001)\nslow %v\nfast %v", chi, slow, fast)
	}
	if math.IsNaN(chi) {
		t.Fatal("chi-square NaN")
	}
}

// recBatcher stands in for tlscarrier's batchConn under a shaped conn: it
// records each record written and what leaves at each flush.
type recBatcher struct {
	net.Conn
	corked  bool
	pending [][]byte
	records [][]byte
	flushes [][]byte
	failAt  int // flush number that fails (0: none)
}

func (b *recBatcher) Write(p []byte) (int, error) {
	r := append([]byte(nil), p...)
	b.records = append(b.records, r)
	if b.corked {
		b.pending = append(b.pending, r)
		return len(p), nil
	}
	b.flushes = append(b.flushes, r)
	return len(p), nil
}
func (b *recBatcher) Cork() { b.corked = true }
func (b *recBatcher) Flush() error {
	b.corked = false
	if b.failAt == len(b.flushes)+1 {
		b.pending = nil
		return errors.New("socket gone")
	}
	b.flushes = append(b.flushes, bytes.Join(b.pending, nil))
	b.pending = nil
	return nil
}
func (b *recBatcher) BufferReads() {}

// With batching, one shaped write is one flush holding exactly its records in
// order, and the stream decodes to what was written; a failed flush fails the
// write.
func TestShapeFastBatchesRecords(t *testing.T) {
	rb := &recBatcher{}
	sc := newShapedConn(rb, nil)
	var seed [32]byte
	sc.rng = mrand.New(mrand.NewChaCha8(seed))
	sc.batch = rb
	payload := bytes.Repeat([]byte("xyz"), 16<<10/3) // ~16 KiB, one smux frame
	n, err := sc.Write(payload)
	if n != len(payload) || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	if len(rb.records) < 10 || len(rb.flushes) != 1 {
		t.Fatalf("%d records, %d flushes; want many records in one flush", len(rb.records), len(rb.flushes))
	}
	if !bytes.Equal(rb.flushes[0], bytes.Join(rb.records, nil)) {
		t.Fatal("the flush is not the records in order")
	}
	a, b := net.Pipe()
	go func() { a.Write(rb.flushes[0]); a.Close() }()
	out, _ := io.ReadAll(newShapedConn(b, nil))
	if !bytes.Equal(out, payload) {
		t.Fatal("the batched stream does not decode to the payload")
	}

	rb.failAt = len(rb.flushes) + 1
	if n, err := sc.Write([]byte("lost")); err == nil || n != 0 {
		t.Fatalf("a failed flush returned %d, %v", n, err)
	}
}

// With HS2_SHAPE_SMALL a tiny write is one small record of a varying size
// (no size of its own on the wire), larger writes keep the bulk shape, and
// the stream decodes as before.
func TestShapeSmallRecords(t *testing.T) {
	old := shapeSmall
	shapeSmall = true
	defer func() { shapeSmall = old }()
	rb := &recBatcher{}
	sc := newShapedConn(rb, nil)
	var want []byte
	for i := 0; i < 400; i++ {
		p := []byte{byte(i), 1, 2, 3, 4, 5, 6, 7} // an smux NOP / window update: 8 bytes
		want = append(want, p...)
		if n, err := sc.Write(p); n != 8 || err != nil {
			t.Fatal(n, err)
		}
	}
	sizes := map[int]bool{}
	for _, r := range rb.records {
		if len(r) < 12 || len(r) > obfs.SmallMax {
			t.Fatalf("small write became a %d-byte record", len(r))
		}
		sizes[len(r)] = true
	}
	if len(rb.records) != 400 || len(sizes) < 100 {
		t.Fatalf("%d records in %d sizes: want one each, sizes spread", len(rb.records), len(sizes))
	}
	big := bytes.Repeat([]byte{9}, 4000)
	want = append(want, big...)
	before := len(rb.records)
	sc.Write(big)
	bulkSizes := map[int]bool{1400: true, 1200: true, 900: true, 600: true, 400: true, 250: true, 150: true, 80: true, 40: true}
	for _, r := range rb.records[before:] {
		if !bulkSizes[len(r)] {
			t.Fatalf("a 4000-byte write produced a %d-byte record, not one of the bulk sizes", len(r))
		}
	}
	a, b := net.Pipe()
	go func() { a.Write(bytes.Join(rb.records, nil)); a.Close() }()
	out, _ := io.ReadAll(newShapedConn(b, nil))
	if !bytes.Equal(out, want) {
		t.Fatal("the stream does not decode to what was written")
	}
}
