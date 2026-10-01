package encap

import (
	"crypto/rand"
	"encoding/binary"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20"
)

// Header obfuscation for the ICMP encapsulation.
//
// The problem it solves: without it, an ICMP tunnel packet carries, before the
// AEAD ciphertext, a structured and mostly-cleartext header a DPI can match
// with one cheap rule — a fixed keyed "magic" at a fixed offset, plus the
// carrier's and FEC's monotonic sequence counters in the clear. The data is
// encrypted, but the frame around it is a stable fingerprint.
//
// What it does: it erases that fingerprint. Nothing a passive observer reads is
// a fixed constant or a readable counter.
//
//   - The 2-byte framing magic is removed. Its two jobs — rejecting a host's
//     other ICMP cheaply, and telling the two directions apart so a side never
//     mistakes its own packets (or a host echoing them back) for the peer's —
//     move into a keyed prefix in the ECHO SEQUENCE field (obfSeqPrefix), 12
//     keyed bits per direction. The low 4 bits are a small counter, so the
//     sequence still increments a little like an ordinary ping. The echo
//     IDENTIFIER keeps naming the link, exactly as before, so routing and
//     demux are unchanged.
//   - Every byte above the ICMP header — the carrier tag, wire sequence, FEC
//     header and the sealed frame — is XORed with a keystream derived from an
//     8-byte per-packet nonce carried in the clear at the front of the ICMP
//     payload. Random XOR keystream is random, so the ciphertext stays
//     ciphertext and the counters in front of it become indistinguishable from
//     it. The receiver reads the nonce, regenerates the keystream and unmasks.
//
// The obfuscation key is derived from the tunnel's shared secret, so it is in
// place from the very first handshake packet. It is NOT a security boundary:
// the payload's AEAD is still the only thing that authenticates data. Its sole
// job is to leave no cheap pattern on the wire. A directional prefix is 12 bits
// rather than the old magic's 16, so 1 in 4096 of a host's unrelated ICMP slips
// the cheap filter and costs one keystream-and-AEAD attempt that then fails —
// negligible against the ICMP a server actually receives.

const (
	// obfNonceLen is the per-packet nonce carried in the clear at the front of
	// the obfuscated ICMP payload; it keys the keystream over everything after
	// it. 8 bytes: a birthday collision needs ~2^32 packets, and a collision
	// leaks only to a holder of the obfuscation key (i.e. the peer).
	obfNonceLen = 8

	// obfSeqCounterBits of the 16-bit echo sequence are a small per-packet
	// counter (so the sequence still moves like a ping); the rest are the keyed
	// directional prefix.
	obfSeqCounterBits = 4
	obfSeqCounterMask = uint16(1)<<obfSeqCounterBits - 1 // low 4 bits
	obfSeqPrefixMask  = ^obfSeqCounterMask               // top 12 bits
)

// obfKeyFromSecret derives the 32-byte keystream key from the tunnel's shared
// secret. A distinct label keeps it independent of every other value derived
// from the same secret (the framing magics, the static keys).
func obfKeyFromSecret(secret []byte) []byte {
	h, _ := blake2s.New256(nonEmpty(secret))
	h.Write([]byte("hs2-icmp-obfs-key-v1"))
	return h.Sum(nil)
}

// obfSeqPrefixes derives the two directional prefixes (already shifted into the
// top 12 bits of a 16-bit sequence) from the secret: one a side puts in the
// requests it sends, the other in the replies. They are never equal, so echo
// suppression can drop the kernel's copies of our requests without touching our
// own replies, and a side never accepts its own direction back.
func obfSeqPrefixes(secret []byte) (c2s, s2c uint16) {
	p := func(dir string) uint16 {
		h, _ := blake2s.New256(nonEmpty(secret))
		h.Write([]byte("hs2-icmp-obfs-seq-v1\x00" + dir))
		return binary.BigEndian.Uint16(h.Sum(nil)[:2]) & obfSeqPrefixMask
	}
	c2s, s2c = p("c2s"), p("s2c")
	if c2s == s2c {
		s2c ^= obfSeqPrefixMask // flip all prefix bits: still distinct, still masked
	}
	return c2s, s2c
}

// obfSeq builds an echo sequence: the keyed prefix in the top bits, counter in
// the low bits.
func obfSeq(prefix, counter uint16) uint16 {
	return (prefix & obfSeqPrefixMask) | (counter & obfSeqCounterMask)
}

// obfSeqHasPrefix reports whether an echo sequence carries prefix in its top
// bits — the cheap, keyed "is this ours, in this direction" check.
func obfSeqHasPrefix(seq, prefix uint16) bool {
	return seq&obfSeqPrefixMask == prefix&obfSeqPrefixMask
}

// obfNonce fills an 8-byte per-packet nonce.
func obfNonce(dst []byte) {
	rand.Read(dst[:obfNonceLen])
}

// obfMask XORs buf in place with the keystream for (obfKey, nonce). It is its
// own inverse, so the same call masks on send and unmasks on receive.
func obfMask(obfKey, nonce, buf []byte) {
	var n [chacha20.NonceSize]byte // 12 bytes; the 8-byte packet nonce sits in the low end
	copy(n[chacha20.NonceSize-obfNonceLen:], nonce[:obfNonceLen])
	c, err := chacha20.NewUnauthenticatedCipher(obfKey, n[:])
	if err != nil {
		return // obfKey is always 32 bytes from obfKeyFromSecret; unreachable
	}
	c.XORKeyStream(buf, buf)
}

// nonEmpty returns a non-nil key for blake2s (an empty key is allowed but we
// never want a nil slice to change the keyed/unkeyed mode).
func nonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte{0}
	}
	if len(b) > 32 {
		return b[:32] // blake2s key is at most 32 bytes
	}
	return b
}
