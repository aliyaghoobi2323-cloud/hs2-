package engine

import (
	"runtime"
	"testing"
)

// mem55: a chunk takes the smallest size class that fits its record, so the
// chunks held for small records (a lite sub's send queue, a writer's batch, a
// receiver's waiting list) cost about their size, not 16 KB each.
func TestMChunkClasses(t *testing.T) {
	for _, tc := range []struct{ size, want int }{
		{1, 512}, {mFinLen, 512}, {512, 512}, {513, 2 << 10}, {mDataHdr + 100, 512},
		{2 << 10, 2 << 10}, {2<<10 + 1, 8 << 10}, {8 << 10, 8 << 10}, {8<<10 + 1, mRecMax},
		{mDataHdr + mChunkMax, mRecMax}, {mRecMax, mRecMax},
	} {
		c := getChunkN(mRecData, tc.size)
		if len(c.buf) != tc.want || len(c.buf) < tc.size {
			t.Errorf("size %d: chunk of %d B, want %d", tc.size, len(c.buf), tc.want)
		}
		if c.buf[0] != mRecData || c.n != 0 || c.off != 0 || c.refs.Load() != 0 {
			t.Errorf("size %d: chunk not reset", tc.size)
		}
		c.n, c.off = 7, 9
		putChunk(c)
	}
	// a chunk goes back to its own class: a small one never comes back for a
	// full record
	for i := 0; i < 100; i++ {
		putChunk(getChunk(mRecFin))
		if c := getChunkN(mRecData, mDataHdr+mChunkMax); len(c.buf) != mRecMax {
			t.Fatalf("full record got a %d B chunk", len(c.buf))
		} else {
			putChunk(c)
		}
	}
	if c := getChunk(mRecFin); len(c.wire()) != mFinLen || len(c.buf) != 512 {
		t.Fatalf("FIN wire %d B in a %d B chunk", len(c.wire()), len(c.buf))
	}
	if c := getChunk(mRecData); len(c.buf) != mRecMax {
		t.Fatalf("getChunk(DATA): %d B, want a full chunk", len(c.buf))
	}
}

// What Write queues for small messages: 2000 chunks of 100-byte records held at
// once cost ~1 MB, not ~36 MB (the old fixed 16 KB chunk, an 18 KB allocation).
func TestMChunkSmallWriteHeldMemory(t *testing.T) {
	const N = 2000
	held := make([]*mchunk, 0, N)
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	msg := make([]byte, 100)
	for i := 0; i < N; i++ {
		c := getChunkN(mRecData, mDataHdr+min(len(msg), mChunkMax))
		c.n = copy(c.buf[mDataHdr:], msg)
		held = append(held, c)
	}
	runtime.ReadMemStats(&b)
	per := float64(b.TotalAlloc-a.TotalAlloc) / N
	t.Logf("%d small chunks: %.0f B allocated each", N, per)
	if per > 2<<10 {
		t.Fatalf("a 100-byte record holds %.0f B", per)
	}
	for _, c := range held {
		if c.n != len(msg) || len(c.wire()) != mDataHdr+len(msg) {
			t.Fatalf("record %d B, wire %d B", c.n, len(c.wire()))
		}
		putChunk(c)
	}
}
