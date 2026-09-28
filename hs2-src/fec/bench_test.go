package fec

import (
	"fmt"
	"testing"
	"time"
)

// capture encodes n payloads of size bytes at loss estimate p and returns the
// packets in wire order.
func capture(n, size int, p float64) [][]byte {
	enc := NewEncoder(Config{Depth: 4})
	enc.SetLoss(p)
	var out [][]byte
	payload := make([]byte, size)
	now := time.Unix(0, 0)
	for i := 0; i < n; i++ {
		enc.Encode(payload, now, func(b []byte) { out = append(out, append([]byte(nil), b...)) })
	}
	enc.Flush(now.Add(time.Hour), func(b []byte) { out = append(out, append([]byte(nil), b...)) })
	return out
}

// BenchmarkEncode measures the sender per payload, parity included.
func BenchmarkEncode(b *testing.B) {
	for _, p := range []float64{0.03, 0.28} {
		for _, size := range []int{200, 1300} {
			b.Run(fmt.Sprintf("loss%.0f/size%d", p*100, size), func(b *testing.B) {
				enc := NewEncoder(Config{Depth: 4})
				enc.SetLoss(p)
				payload := make([]byte, size)
				now := time.Unix(0, 0)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					enc.Encode(payload, now, func([]byte) {})
				}
			})
		}
	}
}

// BenchmarkDecode measures the receiver per data payload: with no loss (the
// fast path: deliver and drop parity) and with every 5th packet lost (every
// group needs Reed-Solomon reconstruction).
func BenchmarkDecode(b *testing.B) {
	const groups = 256
	for _, lossEvery := range []int{0, 5} { // 5 is coprime with the depth, so losses spread over groups
		name := "noloss"
		if lossEvery > 0 {
			name = fmt.Sprintf("lose1in%d", lossEvery)
		}
		b.Run(name, func(b *testing.B) {
			pkts := capture(groups*32, 1300, 0.28)
			var kept [][]byte
			for i, p := range pkts {
				if lossEvery > 0 && i%lossEvery == 0 {
					continue
				}
				kept = append(kept, p)
			}
			// One long-lived decoder, as on a real connection: each round
			// ends with Expire retiring its groups, so the next round (same
			// group ids) starts clean but with warm free lists.
			dec := NewDecoder(time.Second, 1500)
			now := time.Unix(0, 0)
			b.SetBytes(int64(groups * 32 * 1300))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, p := range kept {
					dec.Decode(p, now, func([]byte) {})
				}
				now = now.Add(2 * time.Second)
				dec.Expire(now)
			}
			b.StopTimer()
			if lossEvery > 0 && dec.Stats().Recovered == 0 {
				b.Fatal("nothing recovered")
			}
		})
	}
}
