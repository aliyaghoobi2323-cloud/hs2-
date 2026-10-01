package encap

import (
	"bytes"
	"testing"
)

func TestObfKeyFromSecret(t *testing.T) {
	a := obfKeyFromSecret([]byte("secret-one"))
	b := obfKeyFromSecret([]byte("secret-one"))
	c := obfKeyFromSecret([]byte("secret-two"))
	if len(a) != 32 {
		t.Fatalf("key length = %d, want 32", len(a))
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same secret gave different keys")
	}
	if bytes.Equal(a, c) {
		t.Fatal("different secrets gave the same key")
	}
	// An empty secret must not panic and must still give a usable 32-byte key.
	if len(obfKeyFromSecret(nil)) != 32 {
		t.Fatal("nil secret: bad key length")
	}
}

func TestObfSeqPrefixes(t *testing.T) {
	c2s, s2c := obfSeqPrefixes([]byte("k"))
	if c2s == s2c {
		t.Fatal("the two directional prefixes are equal")
	}
	// Prefixes occupy only the top 12 bits; the low 4 are free for the counter.
	if c2s&obfSeqCounterMask != 0 || s2c&obfSeqCounterMask != 0 {
		t.Fatalf("prefix bleeds into the counter bits: c2s=%#04x s2c=%#04x", c2s, s2c)
	}
	// Stable for one secret, different across secrets.
	c2s2, _ := obfSeqPrefixes([]byte("k"))
	if c2s2 != c2s {
		t.Fatal("prefix not stable for the same secret")
	}
	if o, _ := obfSeqPrefixes([]byte("other")); o == c2s {
		t.Fatal("different secrets gave the same prefix")
	}
}

func TestObfSeqBuildAndMatch(t *testing.T) {
	c2s, s2c := obfSeqPrefixes([]byte("k"))
	for ctr := uint16(0); ctr < 64; ctr++ {
		seq := obfSeq(c2s, ctr)
		if !obfSeqHasPrefix(seq, c2s) {
			t.Fatalf("own prefix not matched for counter %d (seq %#04x)", ctr, seq)
		}
		if obfSeqHasPrefix(seq, s2c) {
			t.Fatalf("the other direction's prefix matched (counter %d)", ctr)
		}
		if got := seq & obfSeqCounterMask; got != ctr&obfSeqCounterMask {
			t.Fatalf("counter not preserved: put %d got %d", ctr&obfSeqCounterMask, got)
		}
	}
}

func TestObfMaskInverse(t *testing.T) {
	key := obfKeyFromSecret([]byte("k"))
	var nonce [obfNonceLen]byte
	obfNonce(nonce[:])
	orig := []byte("the quick brown fox jumps over the lazy dog, and then some more")
	buf := append([]byte(nil), orig...)

	obfMask(key, nonce[:], buf) // mask
	if bytes.Equal(buf, orig) {
		t.Fatal("masking did not change the bytes")
	}
	obfMask(key, nonce[:], buf) // unmask with the same key+nonce
	if !bytes.Equal(buf, orig) {
		t.Fatal("mask is not its own inverse")
	}
}

// A different nonce yields a different keystream, and the wrong key cannot
// recover the plaintext.
func TestObfMaskKeyAndNonceSeparate(t *testing.T) {
	key := obfKeyFromSecret([]byte("k"))
	wrong := obfKeyFromSecret([]byte("k2"))
	orig := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 32)

	var n1, n2 [obfNonceLen]byte
	obfNonce(n1[:])
	obfNonce(n2[:])

	a := append([]byte(nil), orig...)
	b := append([]byte(nil), orig...)
	obfMask(key, n1[:], a)
	obfMask(key, n2[:], b)
	if bytes.Equal(a, b) {
		t.Fatal("different nonces produced the same ciphertext")
	}

	// Unmask a with the wrong key: must not recover orig.
	obfMask(wrong, n1[:], a)
	if bytes.Equal(a, orig) {
		t.Fatal("wrong key recovered the plaintext")
	}
}

// The masked output of a structured, low-entropy payload (a monotonic counter,
// exactly what the carrier's wire sequence looks like) must leave no fixed byte
// at any position across many packets — the property a DPI rule needs and must
// not find.
func TestObfMaskErasesCounterStructure(t *testing.T) {
	key := obfKeyFromSecret([]byte("k"))
	const n = 4096
	const plen = 24
	// Collect, per byte position, the set of values seen across n masked
	// packets whose plaintext is a simple incrementing counter.
	seen := make([]map[byte]bool, plen)
	for i := range seen {
		seen[i] = map[byte]bool{}
	}
	for p := 0; p < n; p++ {
		buf := make([]byte, plen)
		for i := range buf {
			buf[i] = byte(p) // same low-entropy value across the whole payload
		}
		var nonce [obfNonceLen]byte
		obfNonce(nonce[:])
		obfMask(key, nonce[:], buf)
		for i := range buf {
			seen[i][buf[i]] = true
		}
	}
	// With a random keystream each position should take the large majority of
	// the 256 possible values over 4096 packets; a fixed field would show 1.
	for i := range seen {
		if len(seen[i]) < 200 {
			t.Fatalf("byte %d took only %d distinct values across %d packets — looks fixed", i, len(seen[i]), n)
		}
	}
}
