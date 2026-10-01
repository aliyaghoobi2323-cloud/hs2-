package encap

import "testing"

// BenchmarkObfMask measures the per-packet cost of the icmp header obfuscation
// mask alone (ChaCha20 over the first maskSpan bytes). It should be a small
// constant — one ChaCha20 block — not a line-rate bottleneck.
func BenchmarkObfMask(b *testing.B) {
	key := obfKeyFromSecret([]byte("bench-secret"))
	var nonce [obfNonceLen]byte
	buf := make([]byte, obfMaskLen)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		obfMask(key, nonce[:], buf)
	}
}

// benchBuild measures the full per-packet framing cost for one kind at a
// full-size payload: for icmp this includes the obfs nonce, the mask and the
// ICMP checksum; a non-obfuscated kind (gre) is the baseline to compare against.
func benchBuild(b *testing.B, kind string) {
	f, err := newFramer(kind, Options{Key: []byte("bench-key")}, true)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 1280)
	dst := make([]byte, 0, 2048)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = f.build(dst[:0], 0x1234, uint16(i), payload)
	}
	_ = dst
}

func BenchmarkFramerBuildICMP(b *testing.B) { benchBuild(b, KindICMP) } // obfs + checksum
func BenchmarkFramerBuildGRE(b *testing.B)  { benchBuild(b, KindGRE) }  // baseline, no obfs
