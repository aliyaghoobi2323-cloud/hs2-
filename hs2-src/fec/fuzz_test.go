package fec

import (
	"encoding/binary"
	"testing"
	"time"
)

// FuzzDecode feeds arbitrary packet sequences to one decoder. Seeds are real
// shard packets, so the fuzzer mutates from well-formed input. Invariants: no
// panic, and no payload is ever delivered longer than the shard it came from.
//
//	go test -run xxx -fuzz FuzzDecode -fuzztime 30s ./fec
func FuzzDecode(f *testing.F) {
	enc := NewEncoder(Config{K: 4, Depth: 1})
	enc.SetLoss(0.3)
	var seed []byte
	now := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		enc.Encode([]byte{byte(i), 1, 2, 3}, now, func(p []byte) {
			seed = binary.BigEndian.AppendUint16(seed, uint16(len(p)))
			seed = append(seed, p...)
		})
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Fuzz(func(t *testing.T, stream []byte) {
		dec := NewDecoder(time.Second, 64)
		now := time.Unix(0, 0)
		for len(stream) >= 2 {
			n := int(binary.BigEndian.Uint16(stream))
			stream = stream[2:]
			if n > len(stream) {
				n = len(stream)
			}
			pkt := stream[:n]
			stream = stream[n:]
			dec.Decode(pkt, now, func(p []byte) {
				if len(p) > 64 {
					t.Fatalf("delivered %d bytes from shards capped at 64", len(p))
				}
			})
			now = now.Add(time.Millisecond)
		}
		dec.Expire(now.Add(time.Hour))
	})
}
