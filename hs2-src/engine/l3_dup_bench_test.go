package engine

import (
	"encoding/binary"
	"testing"
)

// d421: the receiver's per-frame cost (one TypeDataDup frame, both copies of
// every id arrive), one sender and two interleaved senders.
func benchDupFrames(n int, streams int) [][]byte {
	out := make([][]byte, 0, n)
	starts := []uint32{0x12345678 & l3DupIDMask, 0x6abcdef0 & l3DupIDMask}
	for i := 0; len(out) < n; i++ {
		s := i % streams
		id := starts[s] + uint32(i/streams)
		for c := 0; c < 2; c++ {
			p := make([]byte, l3DupHdr+64)
			binary.BigEndian.PutUint32(p, id|uint32(c)<<31)
			out = append(out, p)
		}
	}
	return out
}

func BenchmarkL3DupAccept1(b *testing.B) { benchDupAccept(b, 1) }
func BenchmarkL3DupAccept2(b *testing.B) { benchDupAccept(b, 2) }

func benchDupAccept(b *testing.B, streams int) {
	fr := benchDupFrames(1<<16, streams)
	ls := benchRxLinks()
	rx := benchRx()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchRxAccept(rx, ls[(i/2)%streams%len(ls)], fr[i&(1<<16-1)])
	}
}

// d421: the sender's second-link pick over 96 links (the adapter's links:
// before the fix plain dup-capable links, after it two peer processes).
func BenchmarkL3DupSecond(b *testing.B) {
	links := benchSecondLinks(96)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = dupSecond(links, uint32(i)*2654435761, links[i%96])
	}
}
