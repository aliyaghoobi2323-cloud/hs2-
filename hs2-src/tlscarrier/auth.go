package tlscarrier

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"time"

	"golang.org/x/crypto/blake2s"
)

// Authentication happens INSIDE the TLS stream, after an ordinary handshake,
// and is bound to that exact TLS session (channel binding) in both directions.
//
// Client -> server, the first application-data record:
//
//	[ nonce  : 16 random bytes ]
//	[ tag    : 16 = HMAC(key, "hs2-auth-v2" || nonce || minute || EKM) ]
//	[ padlen : 2 ]
//	[ pad    : padlen random bytes ]            (sized like an HTTP request)
//
// Server -> client, only after the client's tag verified:
//
//	[ tag    : 16 = HMAC(key, "hs2-srv-v2" || nonce || EKM) ]
//	[ padlen : 2 ]
//	[ pad    : padlen random bytes ]            (sized like response headers)
//
// EKM is the TLS exporter secret for this connection (RFC 8446 §7.5). Both
// ends of one TLS session derive the same EKM; a man-in-the-middle holds two
// different TLS sessions and so two different EKMs. That is what makes the
// scheme safe even though the client does not validate the certificate chain:
//
//   - an interceptor cannot relay the client's tag to the real server (the
//     server checks it against its own session's EKM, and it will not match);
//   - an interceptor cannot answer the client itself (it cannot produce the
//     server tag without the shared key).
//
// So nobody without the shared key can read, inject into, or hijack a link,
// and the client refuses to send any traffic until the server has proven
// itself. A probe never sends a valid first record, so it is forwarded to the
// real backend and sees an ordinary website.
//
// HMAC-BLAKE2s-256 truncated to 128 bits; the minute bucket bounds how long a
// record stays valid, and the nonce memory rejects exact repeats (already
// impossible across connections because of the EKM binding).

const (
	authNonceLen  = 16
	authTagLen    = 16
	authHeadLen   = authNonceLen + authTagLen + 2
	proofHeadLen  = authTagLen + 2
	authClockSkew = 2 // accept current minute +/- this many
	authTimeout   = 10 * time.Second
	exporterLabel = "EXPORTER-hs2-channel-binding-v2"

	clientPadMin, clientPadMax = 280, 720 // a typical HTTP/1.1 GET
	serverPadMin, serverPadMax = 120, 480 // typical response headers

	legacyRecordLen = 32 // v1 clients: nonce + tag, no binding
)

var (
	// ErrServerProof means the peer did not prove knowledge of the shared key
	// for this TLS session: wrong key, an interceptor, or not an hs2 server.
	ErrServerProof = errors.New("tlscarrier: server did not prove the shared key (wrong shared_key, man-in-the-middle, or not hs2)")
	// ErrOldServer means the peer answered with its cover website — what an hs2
	// server does for anyone who cannot prove the shared key. That is a
	// DIFFERENT shared_key (most often: the link was pasted from an earlier
	// setup of the other server), or a pre-v2 hs2 that treats v2 auth as a
	// browser. Which side is the TLS server depends on the direction, so the
	// message names neither.
	ErrOldServer = errors.New("tlscarrier: the other server answered as a plain website, not as the tunnel: " +
		"its shared_key differs from this one (paste the CURRENT link from it — running its setup again makes a new key), " +
		"or it runs an hs2 older than v2 (upgrade it)")
)

type ekmSource interface {
	ExportKeyingMaterial(label string, context []byte, length int) ([]byte, error)
}

func exportEKM(cs ekmSource) ([]byte, error) {
	return cs.ExportKeyingMaterial(exporterLabel, nil, 32)
}

func minuteBucket() int64 { return time.Now().Unix() / 60 }

func mac(key []byte, parts ...[]byte) []byte {
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)[:authTagLen]
}

func be64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func clientTag(key, nonce, ekm []byte, bucket int64) []byte {
	return mac(key, []byte("hs2-auth-v2"), nonce, be64(bucket), ekm)
}

func serverTag(key, nonce, ekm []byte) []byte {
	return mac(key, []byte("hs2-srv-v2"), nonce, ekm)
}

func randRange(lo, hi int) int {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(hi-lo+1)))
	return lo + int(n.Int64())
}

// padded appends a 2-byte length and that many random bytes to head.
func padded(head []byte, lo, hi int) []byte {
	pad := randRange(lo, hi)
	out := make([]byte, len(head)+2+pad)
	copy(out, head)
	binary.BigEndian.PutUint16(out[len(head):], uint16(pad))
	rand.Read(out[len(head)+2:])
	return out
}

// makeClientAuth builds the client's first record and returns its nonce.
func makeClientAuth(key, ekm []byte) (rec, nonce []byte) {
	nonce = make([]byte, authNonceLen)
	rand.Read(nonce)
	head := append(append([]byte{}, nonce...), clientTag(key, nonce, ekm, minuteBucket())...)
	return padded(head, clientPadMin, clientPadMax), nonce
}

// parseClientAuth checks the head of the client's first record. On success it
// returns the nonce and how many pad bytes are still to be read beyond first.
func parseClientAuth(key, ekm, first []byte) (nonce []byte, more int, ok bool) {
	if len(first) < authHeadLen {
		return nil, 0, false
	}
	nonce, tag := first[:authNonceLen], first[authNonceLen:authNonceLen+authTagLen]
	now := minuteBucket()
	match := 0
	for d := int64(-authClockSkew); d <= authClockSkew; d++ {
		match |= subtle.ConstantTimeCompare(tag, clientTag(key, nonce, ekm, now+d))
	}
	if match != 1 {
		return nil, 0, false
	}
	pad := int(binary.BigEndian.Uint16(first[authHeadLen-2 : authHeadLen]))
	more = authHeadLen + pad - len(first)
	if more < 0 {
		return nil, 0, false // trailing bytes: not our framing
	}
	return nonce, more, true
}

// isLegacyAuth reports whether first is a valid pre-v2 (unbound) auth record.
// Such clients are refused; this only lets the server log a clear reason.
func isLegacyAuth(key, first []byte) bool {
	if len(first) != legacyRecordLen {
		return false
	}
	nonce, tag := first[:16], first[16:]
	now := minuteBucket()
	for d := int64(-authClockSkew); d <= authClockSkew; d++ {
		m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, key)
		m.Write([]byte("hs2-tls-auth"))
		m.Write(nonce)
		m.Write(be64(now + d))
		if subtle.ConstantTimeCompare(tag, m.Sum(nil)[:16]) == 1 {
			return true
		}
	}
	return false
}

// makeServerProof builds the server's answer for an authenticated client.
func makeServerProof(key, nonce, ekm []byte) []byte {
	return padded(serverTag(key, nonce, ekm), serverPadMin, serverPadMax)
}

// readServerProof reads and verifies the server's answer.
func readServerProof(r io.Reader, key, nonce, ekm []byte) error {
	var head [proofHeadLen]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return fmt.Errorf("tlscarrier: reading server proof: %w", err)
	}
	if string(head[:5]) == "HTTP/" {
		return ErrOldServer
	}
	if subtle.ConstantTimeCompare(head[:authTagLen], serverTag(key, nonce, ekm)) != 1 {
		return ErrServerProof
	}
	pad := int(binary.BigEndian.Uint16(head[authTagLen:]))
	if _, err := io.CopyN(io.Discard, r, int64(pad)); err != nil {
		return fmt.Errorf("tlscarrier: reading server proof: %w", err)
	}
	return nil
}
