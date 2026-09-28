package reality

// This file carries the authorised signal INSIDE a real TLS ClientHello,
// instead of as a raw prelude. A prober or browser sends an ordinary
// ClientHello with a random session-id; an authorised client sends a
// ClientHello whose 32-byte session-id IS the auth material, so on the wire the
// two are the same shape — a normal TLS 1.3 handshake — and only the holder of
// the key can produce a session-id that verifies.
//
// Signal layout in the 32-byte session-id:
//   [ nonce : 16 bytes, random per connection ]
//   [ tag   : 16 bytes = HMAC(key, "reality-sid-v1" || client_random || nonce) ]
//
// The client_random (a separate 32-byte field of every ClientHello) is bound
// in, so a captured session-id cannot be replayed onto a new ClientHello: a new
// ClientHello has a new client_random, which changes the required tag.

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"hash"
	"time"

	"golang.org/x/crypto/blake2s"
)

const sidLen = 32

// makeSessionID builds the 32-byte session-id an authorised client puts in its
// ClientHello. nonce must be 16 random bytes, clientRandom is the ClientHello's
// own 32-byte random.
func makeSessionID(sharedKey, clientRandom, nonce16 []byte) []byte {
	sid := make([]byte, sidLen)
	copy(sid[:16], nonce16)
	copy(sid[16:], sidTag(sharedKey, clientRandom, nonce16))
	return sid
}

func sidTag(sharedKey, clientRandom, nonce16 []byte) []byte {
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, sharedKey)
	m.Write([]byte("reality-sid-v1"))
	m.Write(clientRandom)
	m.Write(nonce16)
	return m.Sum(nil)[:16]
}

// verifySessionID checks a ClientHello's session-id. Returns true iff it was
// produced by a holder of the key for this exact client_random.
func verifySessionID(sharedKey, clientRandom, sessionID []byte) bool {
	if len(sessionID) != sidLen {
		return false
	}
	nonce, tag := sessionID[:16], sessionID[16:]
	want := sidTag(sharedKey, clientRandom, nonce)
	return subtle.ConstantTimeCompare(want, tag) == 1
}

// helpers shared by the carrier.
const dialTimeout = 8 * time.Second

func randomNonce() []byte {
	n := make([]byte, 16)
	cryptorand.Read(n)
	return n
}
