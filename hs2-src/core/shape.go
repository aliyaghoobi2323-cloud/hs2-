package core

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
)

// Shaping breaks the three fingerprints a fully-encrypted tunnel still leaks
// even though its bytes are opaque: fixed handshake sizes, a fixed-size fixed-
// period keepalive, and a frame length that tracks the payload. None of this
// touches the crypto; it only changes the SHAPE on the wire.
//
// The philosophy is not to imitate a specific protocol (imitation is brittle —
// "The Parrot is Dead") but to erase the tell-tale regularities so the flow has
// no distinctive shape to match.

// Padding buckets: every data frame is padded up to the next bucket, so the
// distribution of frame sizes on the wire collapses onto a handful of values
// that no longer track the payload. Small packets (acks, keystrokes, game
// state) stop being individually recognisable, and the padding cost is bounded
// because a full-size packet is already near the top bucket.
var padBuckets = []int{64, 128, 256, 512, 1024, maxDataFrame}

const maxDataFrame = 1400

// PadTarget returns the padded plaintext length for a payload of n bytes.
func PadTarget(n int) int {
	for _, b := range padBuckets {
		if n <= b {
			return b
		}
	}
	return n // larger than the top bucket: sent as-is (already full-size)
}

// randInt returns a uniform int in [0,max).
func randInt(max int) int {
	if max <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// HandshakePad returns how many random bytes to append to a handshake message
// so its length is not a constant. The pad is random per connection, drawn from
// a range that overlaps ordinary record sizes, so m1 is no longer "always 112".
// The receiver ignores trailing bytes it does not consume, so the pad needs no
// agreement — it is discarded by the Noise reader, which reads exactly the
// message it expects.
func HandshakePad() int { return 16 + randInt(240) }

// KeepaliveJitter returns the next keepalive interval in milliseconds, drawn
// from a range rather than a constant, so the tunnel has no fixed heartbeat
// period. Centred near the configured base but never exactly it.
func KeepaliveJitter(baseMillis int) int {
	// +/- 40% jitter
	span := baseMillis * 4 / 5
	return baseMillis - span/2 + randInt(span+1)
}

// KeepalivePad returns a random size for a keepalive frame's payload so a
// keepalive is not a fixed 30-byte record. Drawn to look like a small data
// frame, and padded through the same buckets, so on the wire a keepalive is
// indistinguishable from a small real packet.
func KeepalivePad() int { return randInt(96) }

// idNonce is unused here but kept so callers can seed per-connection shaping.
func shapeSeed() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}
