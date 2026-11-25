package engine

import (
	"bytes"
	"math/rand"
	"testing"
	"time"
)

// h6Sim runs one request through h6Recv: the raw stream (every byte, in
// random chunks, maybe delayed) and the hedge copy (prefix [0, N) in random
// records, maybe cut at a random point = the hedge sub lost, maybe with
// duplicated records), interleaved at random. The user must read the exact
// request.
func h6Sim(t *testing.T, rng *rand.Rand, L, N int, cut int, dup bool) {
	t.Helper()
	src := make([]byte, L)
	rng.Read(src)
	h := newH6Recv(N)
	type rec struct {
		off int
		b   []byte
		end bool
	}
	var recs []rec
	hl := L
	if hl > N {
		hl = N
	}
	for off := 0; off < hl; {
		n := 1 + rng.Intn(1500)
		if off+n > hl {
			n = hl - off
		}
		recs = append(recs, rec{off: off, b: src[off : off+n]})
		if dup && rng.Intn(3) == 0 {
			recs = append(recs, rec{off: off, b: src[off : off+n]})
		}
		off += n
	}
	recs = append(recs, rec{off: hl, end: true})
	if cut >= 0 && cut < len(recs) {
		recs = recs[:cut] // the hedge sub died: the rest of its copy is lost
	}
	raw, ri := src, 0
	var out []byte
	p := make([]byte, 512+rng.Intn(4096))
	for len(out) < L {
		switch {
		case len(recs) > 0 && (ri >= len(raw) || rng.Intn(2) == 0):
			r := recs[0]
			recs = recs[1:]
			if !h.addHedge(uint64(r.off), r.b, r.end) {
				t.Fatalf("addHedge gap at %d", r.off)
			}
		case h.ready() && rng.Intn(2) == 0:
			n := h.fromHedge(p)
			out = append(out, p[:n]...)
		case ri < len(raw):
			n := 1 + rng.Intn(len(p))
			if ri+n > len(raw) {
				n = len(raw) - ri
			}
			copy(p, raw[ri:ri+n])
			ri += n
			k := h.rawGot(p, n)
			out = append(out, p[:k]...)
		case h.ready():
			n := h.fromHedge(p)
			out = append(out, p[:n]...)
		default:
			t.Fatalf("stuck: out %d of %d, raw %d", len(out), L, ri)
		}
		if len(out) > L {
			t.Fatalf("too many bytes: %d > %d", len(out), L)
		}
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("bytes differ (L=%d N=%d cut=%d dup=%v)", L, N, cut, dup)
	}
	if ri < len(raw) { // drain the rest of the raw stream: all duplicates
		for ri < len(raw) {
			n := copy(p, raw[ri:])
			ri += n
			if k := h.rawGot(p, n); k != 0 {
				t.Fatalf("raw tail delivered %d extra bytes", k)
			}
		}
	}
}

func TestB469mH6Exact(t *testing.T) {
	mm := h6Stat.mismatch.Load()
	rng := rand.New(rand.NewSource(469))
	for i := 0; i < 3000; i++ {
		L := 1 + rng.Intn(12000)
		N := []int{1024, 4096, 16 << 10}[rng.Intn(3)]
		cut := -1
		if rng.Intn(3) == 0 {
			cut = rng.Intn(8) // hedge copy lost after a few records (0: lost whole)
		}
		h6Sim(t, rng, L, N, cut, rng.Intn(2) == 0)
	}
	if d := h6Stat.mismatch.Load() - mm; d != 0 {
		t.Fatalf("mismatch %d", d)
	}
}

// Raw stream lost/late entirely for the prefix: the hedge alone serves it,
// then raw duplicates are dropped and the tail comes from raw.
func TestB469mH6HedgeFirst(t *testing.T) {
	src := bytes.Repeat([]byte("abcdefgh"), 1000) // 8000
	h := newH6Recv(4096)
	h.addHedge(0, src[:3000], false)
	h.addHedge(3000, src[3000:4096], false)
	h.addHedge(4096, nil, true)
	p := make([]byte, 10000)
	n := h.fromHedge(p)
	if n != 4096 || !bytes.Equal(p[:n], src[:4096]) {
		t.Fatalf("hedge read %d", n)
	}
	if h.fromHedge(p) != 0 {
		t.Fatal("hedge read past its end")
	}
	copy(p, src[:5000])
	k := h.rawGot(p, 5000)
	if k != 904 || !bytes.Equal(p[:k], src[4096:5000]) {
		t.Fatalf("raw kept %d", k)
	}
	if !h.freed {
		t.Fatal("hbuf not freed")
	}
	copy(p, src[5000:])
	if k := h.rawGot(p, 3000); k != 3000 || h.d != 8000 {
		t.Fatalf("tail %d d %d", k, h.d)
	}
	if h.switchNext(7000) != 8000 || h.switchNext(9000) != 9000 {
		t.Fatal("switchNext")
	}
}

// Duplicate and overlapping hedge records are dropped; a gap is refused and
// ends the copy (raw carries everything).
func TestB469mH6Dup(t *testing.T) {
	src := []byte("0123456789abcdefghij")
	h := newH6Recv(4096)
	h.addHedge(0, src[:10], false)
	h.addHedge(0, src[:10], false)  // full dup
	h.addHedge(5, src[5:15], false) // overlap
	if h.hEnd != 15 || !bytes.Equal(h.hbuf, src[:15]) {
		t.Fatalf("hbuf %q", h.hbuf)
	}
	if h.addHedge(17, src[17:], false) {
		t.Fatal("gap accepted")
	}
	p := make([]byte, 64)
	n := h.fromHedge(p)
	copy(p[n:], src) // raw from 0
	k := h.rawGot(p[n:], len(src))
	if got := append(append([]byte{}, p[:n]...), p[n:n+k]...); !bytes.Equal(got, src) {
		t.Fatalf("got %q", got)
	}
}

// The copy is capped at max: past it the raw stream carries the rest.
func TestB469mH6Cap(t *testing.T) {
	src := make([]byte, 6000)
	rand.New(rand.NewSource(1)).Read(src)
	h := newH6Recv(4096)
	h.addHedge(0, src[:5000], false)
	if h.hEnd != 4096 || !h.ended {
		t.Fatalf("cap: hEnd %d ended %v", h.hEnd, h.ended)
	}
	p := make([]byte, 12000)
	n := h.fromHedge(p)
	copy(p[n:], src)
	k := h.rawGot(p[n:], len(src))
	if !bytes.Equal(p[:n+k], src) {
		t.Fatal("bytes differ")
	}
}

// Exit raw reader with H6: a reader blocked in the raw Read is woken by a
// hedge record and gets the hedge bytes first; the raw stream's copy of them
// is dropped; the rest comes raw; an upgrade afterwards keeps every byte
// exact both ways.
func TestB469mH6RawReader(t *testing.T) {
	_, iran, exit := newRawPair(t)
	exit.h6 = newH6Recv(4096)
	up := randBytes(1<<20, 469)
	down := randBytes(256<<10, 470)
	got := make(chan []byte, 1)
	go func() {
		p := make([]byte, 64<<10)
		n, _ := exit.Read(p) // blocks in the raw Read: nothing sent yet
		got <- append([]byte(nil), p[:n]...)
	}()
	time.Sleep(50 * time.Millisecond)
	if !exit.h6Add(0, up[:3000], false) || !exit.h6Add(3000, up[3000:4096], false) {
		t.Fatal("h6Add")
	}
	var first []byte
	select {
	case first = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked raw reader not woken by the hedge copy")
	}
	if len(first) == 0 || !bytes.Equal(first, up[:len(first)]) {
		t.Fatalf("first read %d bytes, not the prefix", len(first))
	}
	rest := up[len(first):]
	both(t,
		func() error {
			if err := xfer(iran, up[:200<<10], 7000, 0); err != nil {
				return err
			}
			forceUpgrade(iran)
			return xfer(iran, up[200<<10:], 32<<10, time.Millisecond)
		},
		func() error { return xfer(exit, down, 16<<10, 0) },
		func() error { return readN(exit, rest) },
		func() error { return readN(iran, down) },
	)
	exit.h6mu.Lock()
	d, fh := exit.h6.d, h6Stat.fromHedge.Load()
	exit.h6mu.Unlock()
	if d < 4096 || fh < 4096 {
		t.Fatalf("hedge not used: d=%d fromHedge=%d", d, fh)
	}
	if h6Stat.mismatch.Load() != 0 {
		t.Fatal("raw vs hedge mismatch")
	}
	closeAndWait(t, iran, exit)
}
