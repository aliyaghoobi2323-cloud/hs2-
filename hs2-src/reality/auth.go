package reality

import (
	"crypto/hmac"
	"crypto/subtle"
	"hash"

	"golang.org/x/crypto/blake2s"
)

// authTag proves the client holds the shared key WITHOUT sending the key and
// without a value a prober could replay usefully. It is an HMAC over the TLS
// client-random (fresh every connection) keyed by the shared secret. Binding
// the client-random in means a captured tag cannot be reused on a new
// connection; the server recomputes from the client-random it sees and compares
// in constant time. A prober lacks the key, cannot produce a valid tag, and is
// forwarded to the cover site.
func authTag(sharedKey, clientRandom []byte) []byte {
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, sharedKey)
	m.Write([]byte("reality-auth-v1"))
	m.Write(clientRandom)
	return m.Sum(nil)[:16]
}

// verifyTag checks a presented tag in constant time.
func verifyTag(sharedKey, clientRandom, presented []byte) bool {
	if len(presented) != 16 {
		return false
	}
	want := authTag(sharedKey, clientRandom)
	return subtle.ConstantTimeCompare(want, presented) == 1
}
