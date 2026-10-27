package engine

import (
	"bytes"
	"testing"
	"time"
)

// FuzzOWDDecode: the 16-byte kindOWD message is the other server's data
// (owd.go), so its decoder must take any bytes without a panic (REDESIGN
// §5.10 step 5; the round-2 judge's open item 5): a message is either
// refused (short, or a version but 1) or read back as encoded — the stamp
// whole, the queue in µs or none — and a link that takes it keeps a queue
// that is never negative and never past what the wire can say.
func FuzzOWDDecode(f *testing.F) {
	var m [owdMsgLen]byte
	owdEncode(m[:], 123456789, int64(2500*time.Microsecond), true)
	f.Add(m[:])
	owdEncode(m[:], -1, 0, false)
	f.Add(m[:])
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add(bytes.Repeat([]byte{0xff}, owdMsgLen))
	f.Add(append([]byte{1}, bytes.Repeat([]byte{0xff}, 40)...))
	f.Fuzz(func(t *testing.T, b []byte) {
		stamp, q, have, ok := owdDecode(b)
		if !ok {
			if len(b) >= owdMsgLen && b[0] == 1 {
				t.Fatalf("a message of version 1 and %d bytes refused", len(b))
			}
			if stamp != 0 || q != 0 || have {
				t.Fatalf("a refused message read as stamp %d, queue %v, %v", stamp, q, have)
			}
			return
		}
		if len(b) < owdMsgLen || b[0] != 1 {
			t.Fatalf("taken: %d bytes, version %d", len(b), b[0])
		}
		if !have && q != 0 || have && (q < 0 || q >= time.Duration(owdNone)*time.Microsecond) {
			t.Fatalf("queue %v (have %v)", q, have)
		}
		// encoded back, the same message (the three bytes after the version
		// are not read)
		var e [owdMsgLen]byte
		owdEncode(e[:], stamp, int64(q), have)
		s2, q2, h2, ok2 := owdDecode(e[:])
		if !ok2 || s2 != stamp || q2 != q || h2 != have {
			t.Fatalf("round trip: %d %v %v -> %d %v %v %v", stamp, q, have, s2, q2, h2, ok2)
		}
		if !bytes.Equal(e[4:], b[4:owdMsgLen]) {
			t.Fatalf("encoded back %x, came as %x", e[4:], b[4:owdMsgLen])
		}
		// a link that hears it: any stamp, at a fresh clock
		var o owdLink
		if !o.recv(ctrlNow(), b) {
			t.Fatal("a link refused a message the decoder took")
		}
		if qi := o.qIn.Load(); qi < 0 {
			t.Fatalf("the way in read %d ns", qi)
		}
		if have {
			if qo, okq := o.queue(); !okq && !OWDOff() || okq && (qo < 0 || qo != q) {
				t.Fatalf("the way out read %v %v, sent %v", qo, okq, q)
			}
		}
	})
}
