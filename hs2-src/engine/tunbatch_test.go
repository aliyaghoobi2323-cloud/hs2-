package engine

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// batchTUN is a fake TUN with WriteBatch that records each batch.
type batchTUN struct {
	*fakeTUN
	mu      sync.Mutex
	batches [][]string
}

func (d *batchTUN) WriteBatch(ps [][]byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b []string
	for _, p := range ps {
		b = append(b, string(p))
	}
	d.batches = append(d.batches, b)
	return len(ps), nil
}

func (d *batchTUN) all() (n int, batches int, biggest int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.batches {
		n += len(b)
		biggest = max(biggest, len(b))
	}
	return n, len(d.batches), biggest
}

// tryCarrier is a fake carrier with TryReadFrame (frames already waiting).
type tryCarrier struct{ *dgFakeCarrier }

func (c tryCarrier) TryReadFrame() (byte, []byte, bool) {
	select {
	case f := <-c.in:
		return f.ft, f.p, true
	default:
		return 0, nil, false
	}
}

// What arrived together reaches the TUN in one WriteBatch — every packet,
// once; and a packet the reorderer releases on its timer is written then,
// not left in the batch until more traffic comes.
func TestDgReadLoopBatchesTunWrites(t *testing.T) {
	dev := &batchTUN{fakeTUN: newFakeTUN(1400)}
	p := newDgPool(dev, 1, 4, 8, func(string, ...any) {})
	a, b := newDgFakePair()
	for i := 0; i < 40; i++ { // queued before the loop reads: one burst
		a.SendFrame(core.TypeData, []byte{0x45, byte(i)}) // not TCP: passes the reorderer
	}
	ctx := t.Context()
	l := p.add(ctx, tryCarrier{b}, "test")
	within(t, 2*time.Second, "all 40 written", func() bool { n, _, _ := dev.all(); return n == 40 })
	if _, nb, big := dev.all(); big < 10 {
		t.Fatalf("40 packets that arrived together went out in %d batches (biggest %d)", nb, big)
	}
	// A TCP segment ahead of a gap waits in the reorderer; when its hold
	// expires the timer releases it and the batch is flushed.
	seg := func(seq uint32, n int) []byte {
		pk := make([]byte, 40+n)
		pk[0], pk[9] = 0x45, 6
		pk[2], pk[3] = byte((40+n)>>8), byte(40+n)
		pk[32] = 5 << 4
		pk[24], pk[25], pk[26], pk[27] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		pk[33] = 0x10
		return pk
	}
	a.SendFrame(core.TypeData, seg(1000, 10)) // opens the flow
	within(t, 2*time.Second, "first segment", func() bool { n, _, _ := dev.all(); return n == 41 })
	a.SendFrame(core.TypeData, seg(1100, 10)) // ahead of a gap at 1010: held
	time.Sleep(5 * time.Millisecond)
	if n, _, _ := dev.all(); n != 41 {
		t.Fatalf("a segment behind a gap was written at once (%d)", n)
	}
	within(t, time.Second, "released by the reorderer's timer", func() bool { n, _, _ := dev.all(); return n == 42 })
	_ = l
}

// refusingTUN takes every packet but the ones starting 0x00 (as a kernel
// refuses a non-IP packet), reporting how many went in.
type refusingTUN struct{ batchTUN }

func (d *refusingTUN) WriteBatch(ps [][]byte) (int, error) {
	var ok [][]byte
	for _, p := range ps {
		if p[0] != 0 {
			ok = append(ok, p)
		}
	}
	d.batchTUN.WriteBatch(ok)
	if len(ok) < len(ps) {
		return len(ok), errors.New("refused")
	}
	return len(ok), nil
}

// A refused packet costs only itself: the rest of its batch is written and
// counted.
func TestTunBatchCountsWhatWentIn(t *testing.T) {
	dev := &refusingTUN{batchTUN{fakeTUN: newFakeTUN(1400)}}
	var written int
	b := newTunBatch(dev, func(n int) { written += n })
	for i := 0; i < 10; i++ {
		v := byte(0x45)
		if i == 3 || i == 7 {
			v = 0
		}
		b.add([]byte{v, byte(i)})
	}
	b.flush()
	if n, _, _ := dev.all(); n != 8 || written != 8 {
		t.Fatalf("wrote %d, counted %d; want 8 and 8", n, written)
	}
}
