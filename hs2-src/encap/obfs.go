package encap

import (
	"crypto/rand"
	"encoding/binary"
	"sync"

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
//     move into a keyed prefix in the ECHO SEQUENCE field (obfSeqPrefix), 8
//     keyed bits per direction — the sequence HIGH byte. The low 8 bits (the
//     LOW byte) are a small counter, so the sequence still increments a little
//     like an ordinary ping. The echo IDENTIFIER keeps naming the link, exactly
//     as before, so routing and demux are unchanged.
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
// job is to leave no cheap pattern on the wire. A directional prefix is 8 bits
// rather than the old magic's 16, so 1 in 256 of a host's unrelated ICMP slips
// the cheap filter and costs one keystream-and-AEAD attempt that then fails —
// negligible against the ICMP a server actually receives.

const (
	// obfNonceLen is the per-packet nonce carried in the clear at the front of
	// the obfuscated ICMP payload; it keys the keystream over everything after
	// it. 8 bytes: a birthday collision needs ~2^32 packets, and a collision
	// leaks only to a holder of the obfuscation key (i.e. the peer).
	obfNonceLen = 8

	// obfSeqCounterBits of the 16-bit echo sequence are a per-packet counter (so
	// the sequence still moves like a ping); the rest are the keyed directional
	// prefix. 8/8 is byte-aligned: the prefix is exactly the sequence HIGH byte
	// (ICMP offset 6), so the kernel cheap-reject (recvFilter) and the echo-guard
	// rule are single-byte compares that need no BPF/nft AND opcode, and the
	// counter is a full byte that climbs 1..255 before it wraps, like a ping.
	obfSeqCounterBits = 8
	obfSeqCounterMask = uint16(1)<<obfSeqCounterBits - 1 // low 8 bits
	obfSeqPrefixMask  = ^obfSeqCounterMask               // top 8 bits

	// obfMaskLen is how many bytes at the front of the payload the mask covers:
	// the carrier's structured header (tag, wireSeq, optional stamp, FEC header,
	// datagram sequence — ~26 bytes) plus margin, rounded to one ChaCha20 block.
	// The bytes beyond it are the sealed AEAD ciphertext, already uniform random,
	// so masking them would only cost CPU at line rate and hide nothing more.
	obfMaskLen = 64
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
// top 8 bits of a 16-bit sequence) from the secret: one a side puts in the
// requests it sends, the other in the replies. They are never equal, so echo
// suppression can drop the kernel's copies of our requests without touching our
// own replies, and a side never accepts its own direction back.
func obfSeqPrefixes(secret []byte) (c2s, s2c uint16) {
	// A non-zero prefix byte keeps an ordinary short ping answerable: the echo
	// guard drops replies whose sequence high byte equals the c2s prefix, and a
	// `ping -c N` for the common small N stays in sequences 1..255 (high byte 0),
	// so it is never caught. Only a long ping (>255 packets) meets the prefix on
	// its high-byte wrap — the accepted ~1/256 collateral.
	lowestPrefix := obfSeqPrefixMask & 0x0100 // high byte 0x01, the smallest non-zero prefix
	p := func(dir string) uint16 {
		h, _ := blake2s.New256(nonEmpty(secret))
		h.Write([]byte("hs2-icmp-obfs-seq-v1\x00" + dir))
		v := binary.BigEndian.Uint16(h.Sum(nil)[:2]) & obfSeqPrefixMask
		if v == 0 {
			v = lowestPrefix
		}
		return v
	}
	c2s, s2c = p("c2s"), p("s2c")
	if c2s == s2c {
		s2c ^= obfSeqPrefixMask // flip all prefix bits: still distinct, still masked
		if s2c == 0 {
			s2c = lowestPrefix
		}
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

// obfNonce fills an 8-byte per-packet nonce. Used by tests; the hot path uses
// obfNoncer, which amortises the CSPRNG cost over many packets.
func obfNonce(dst []byte) {
	rand.Read(dst[:obfNonceLen])
}

// obfNoncer hands out per-packet nonces from a buffer refilled from crypto/rand
// in bulk (512 nonces at a time), so a line-rate sender never makes a syscall
// per packet. It is safe for concurrent use: one framer is shared by the pacer
// and the control writer.
type obfNoncer struct {
	mu  sync.Mutex
	buf [obfNonceLen * 512]byte
	off int
}

// newObfNoncer returns a noncer whose first next() refills the buffer (the
// zero value would otherwise hand out zero bytes before the first refill).
func newObfNoncer() *obfNoncer {
	n := &obfNoncer{}
	n.off = len(n.buf)
	return n
}

// next copies the next 8 random bytes into dst, refilling in bulk when spent.
func (n *obfNoncer) next(dst []byte) {
	n.mu.Lock()
	if n.off+obfNonceLen > len(n.buf) {
		rand.Read(n.buf[:])
		n.off = 0
	}
	copy(dst[:obfNonceLen], n.buf[n.off:n.off+obfNonceLen])
	n.off += obfNonceLen
	n.mu.Unlock()
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
