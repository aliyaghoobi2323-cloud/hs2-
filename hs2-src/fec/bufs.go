package fec

// bufFree is a bounded free list of byte buffers of one capacity. Shard
// buffers live only while their group is open, so recycling them removes
// nearly every per-packet allocation; the bound keeps an idle encoder or
// decoder from holding on to the memory of a past burst. It is not
// synchronised: each Encoder uses it under its lock, each Decoder from its
// single caller.
type bufFree struct {
	size int // capacity of every buffer handed out
	max  int // most buffers kept for reuse
	free [][]byte
}

func (f *bufFree) get() []byte {
	if n := len(f.free); n > 0 {
		b := f.free[n-1]
		f.free[n-1] = nil
		f.free = f.free[:n-1]
		return b[:0]
	}
	return make([]byte, 0, f.size)
}

func (f *bufFree) put(b []byte) {
	if cap(b) < f.size || len(f.free) >= f.max {
		return
	}
	f.free = append(f.free, b[:0])
}
