package tlscarrier

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"hash"
	"io"
	"time"

	"golang.org/x/crypto/blake2s"
)

// Authentication happens INSIDE the TLS stream, after an ordinary handshake.
// The client's first application-data bytes are an auth record:
//
//	[ nonce : 16 random bytes ]
//	[ tag   : 16 bytes = HMAC(sharedKey, "hs2-tls-auth" || nonce || minuteBucket) ]
//
// The minute bucket binds a coarse timestamp so a captured record cannot be
// replayed indefinitely; the server keeps a small replay memory of recent
// nonces. A probe never sends this record (it speaks HTTP), so it fails the
// check and is forwarded to the real backend.

const (
	authRecordLen = 32
	authClockSkew = 2 // accept current minute +/- this many
)

func minuteBucket() int64 { return time.Now().Unix() / 60 }

func authTag(sharedKey, nonce []byte, bucket int64) []byte {
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, sharedKey)
	m.Write([]byte("hs2-tls-auth"))
	m.Write(nonce)
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(bucket >> (8 * (7 - i)))
	}
	m.Write(b[:])
	return m.Sum(nil)[:16]
}

// makeAuthRecord builds the client's auth record for the current minute.
func makeAuthRecord(sharedKey []byte) []byte {
	rec := make([]byte, authRecordLen)
	rand.Read(rec[:16])
	copy(rec[16:], authTag(sharedKey, rec[:16], minuteBucket()))
	return rec
}

// verifyAuthRecord checks a presented record against the shared key for the
// current minute +/- skew. Returns the nonce (for replay tracking) and ok.
func verifyAuthRecord(sharedKey, rec []byte) (nonce []byte, ok bool) {
	if len(rec) != authRecordLen {
		return nil, false
	}
	nonce, tag := rec[:16], rec[16:]
	now := minuteBucket()
	for d := int64(-authClockSkew); d <= authClockSkew; d++ {
		if subtle.ConstantTimeCompare(tag, authTag(sharedKey, nonce, now+d)) == 1 {
			return nonce, true
		}
	}
	return nil, false
}

// readAuthRecord reads exactly authRecordLen bytes (the client sends them first
// inside TLS). Returns the bytes read for forwarding if it turns out invalid.
func readAuthRecord(r io.Reader) ([]byte, error) {
	buf := make([]byte, authRecordLen)
	n, err := io.ReadFull(r, buf)
	return buf[:n], err
}
