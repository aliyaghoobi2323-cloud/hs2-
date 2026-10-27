package engine

import (
	"bytes"
	"io"
	"math/rand/v2"
	"net"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// The shaper in carry mode (a link with C2's scheduler): frame after frame
// it writes full records of the sampled sizes, with no padding, holding the
// last short tail for the next frame; only Flush (the writer out of frames)
// pads, once. The peer reads back the same bytes. Padded per write, as
// without carry, the same frames cost about half a record each.
func TestShapedConnCarry(t *testing.T) {
	frames := make([][]byte, 300)
	var want []byte
	for i := range frames {
		f := make([]byte, 1+rand.IntN(16<<10))
		for k := range f {
			f[k] = byte(i + k)
		}
		frames[i] = f
		want = append(want, f...)
	}
	pad := func(carry bool) int {
		a, b := net.Pipe()
		defer a.Close()
		cc := &captureConn{Conn: a}
		w := newShapedConn(cc, obfs.NewHTTPSLengthSampler())
		w.carry = carry
		r := newShapedConn(b, nil)
		got := make(chan []byte, 1)
		go func() {
			buf := make([]byte, len(want))
			io.ReadFull(r, buf)
			got <- buf
		}()
		for _, f := range frames {
			if n, err := w.Write(f); err != nil || n != len(f) {
				t.Fatalf("write: %d, %v", n, err)
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(<-got, want) {
			t.Fatalf("carry %v: the bytes read back differ", carry)
		}
		sizes := cc.snapshot()
		wire := 0
		for _, s := range sizes {
			wire += s
		}
		return wire - len(want) - shapeHdrLen*len(sizes)
	}
	withCarry, without := pad(true), pad(false)
	t.Logf("padding over %d frames (%d B): carried %d B, padded per write %d B", len(frames), len(want), withCarry, without)
	if withCarry >= 1400 {
		t.Fatalf("carry mode padded %d B: more than the one record of its Flush", withCarry)
	}
}

// Carry mode keeps the sampled sizes: a record is never larger than the
// largest target, and no frame's end shows as a short record.
func TestShapedConnCarryRecordSizes(t *testing.T) {
	cc := &captureConn{}
	w := newShapedConn(cc, obfs.NewHTTPSLengthSampler())
	w.carry = true
	for i := 0; i < 2000; i++ {
		w.Write(make([]byte, 1+rand.IntN(6000)))
	}
	targets := map[int]bool{1400: true, 1200: true, 900: true, 600: true, 400: true, 250: true, 150: true, 80: true, 40: true}
	for _, s := range cc.snapshot() {
		if !targets[s] {
			t.Fatalf("a record of %d B: not a sampled size", s)
		}
	}
}
