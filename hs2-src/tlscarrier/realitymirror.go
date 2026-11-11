package tlscarrier

// Phase 4 fingerprint mirroring helpers of the reality listener: the target's
// leaf key type/size (CertificateVerify size), its post-handshake
// NewSessionTicket records (count and sizes), and the pad records that
// mirror them on our side.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// certKey is the throw-away certificate's key: ECDSA P-256 by default, or the
// target leaf's type and size (RSA 2048 -> a 256-byte rsa_pss
// CertificateVerify like the target's).
type certKey struct {
	signer crypto.Signer
	kind   string // "ecdsa" | "rsa"
	bits   int
	sigLen int // ECDSA: the signature DER length re-signed for (0 = any)
	base   int // DER length of an unpadded normal-form cert (ServerNames[0])
	minLen int // DER length of the minimal-form cert (ServerNames[0])
}

// keyCache holds generated keys per kind/size (an RSA 4096 key takes seconds:
// generate once per process).
var keyCache struct {
	mu sync.Mutex
	m  map[string]crypto.Signer
}

func cachedKey(kind string, bits int) (crypto.Signer, error) {
	k := fmt.Sprintf("%s-%d", kind, bits)
	keyCache.mu.Lock()
	defer keyCache.mu.Unlock()
	if s := keyCache.m[k]; s != nil {
		return s, nil
	}
	var s crypto.Signer
	var err error
	switch kind {
	case "rsa":
		if bits < 1024 || bits > 8192 {
			return nil, fmt.Errorf("rsa %d bits", bits)
		}
		s, err = rsa.GenerateKey(rand.Reader, bits)
	case "ecdsa":
		var c elliptic.Curve
		switch bits {
		case 256:
			c = elliptic.P256()
		case 384:
			c = elliptic.P384()
		case 521:
			c = elliptic.P521()
		default:
			return nil, fmt.Errorf("ecdsa %d bits", bits)
		}
		s, err = ecdsa.GenerateKey(c, rand.Reader)
	default:
		return nil, fmt.Errorf("key kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	if keyCache.m == nil {
		keyCache.m = map[string]crypto.Signer{}
	}
	keyCache.m[k] = s
	return s, nil
}

// ecdsaSigLen: the most common DER length of an ECDSA signature per curve
// (both integers without a leading zero byte); re-signing until it matches
// keeps the cert size exact.
func ecdsaSigLen(bits int) int {
	switch bits {
	case 256:
		return 71
	case 384:
		return 103
	}
	return 0
}

// leafKey names a certificate's key: kind and size.
func leafKey(c *x509.Certificate) (string, int) {
	switch k := c.PublicKey.(type) {
	case *rsa.PublicKey:
		return "rsa", k.N.BitLen()
	case *ecdsa.PublicKey:
		return "ecdsa", k.Curve.Params().BitSize
	}
	return "", 0
}

// recordTap records the TLS record lengths (header length field) read through
// a connection, in order.
type recordTap struct {
	net.Conn
	mu   sync.Mutex
	buf  []byte
	lens []int
}

func (t *recordTap) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	t.mu.Lock()
	t.buf = append(t.buf, b[:n]...)
	for len(t.buf) >= 5 {
		l := int(binary.BigEndian.Uint16(t.buf[3:5]))
		if len(t.buf) < 5+l {
			break
		}
		if t.buf[0] == 23 {
			t.lens = append(t.lens, l)
		} else {
			t.lens = append(t.lens, -l) // not application_data (ServerHello, CCS)
		}
		t.buf = t.buf[5+l:]
	}
	t.mu.Unlock()
	return n, err
}

func (t *recordTap) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.lens)
}

func (t *recordTap) since(i int) []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []int
	for _, l := range t.lens[i:] {
		if l > 0 {
			out = append(out, l)
		}
	}
	return out
}

// nstWait is how long the mirror probe waits for the target's tickets after
// its Finished (OpenSSL sends them at once, on the client Finished).
const nstWait = 300 * time.Millisecond

// Pad records: an application_data record of the target's NewSessionTicket
// record length (TLS 1.3 hides the inner type, so to a passive observer it is
// a ticket), sent right after the client's Finished, only to a dialer that
// announced realityCapPad. Content: padMarker(ekm) || u16 n || n random bytes;
// the dialer skips them before the server proof (readPadsThenProof).
const padOverhead = 17 // inner content type + AES-GCM/ChaCha tag

func padMarker(ekm []byte) []byte {
	m := hmac.New(sha256.New, ekm)
	m.Write([]byte("hs2-reality-pad"))
	return m.Sum(nil)[:authTagLen]
}

// padRecord is the plaintext of a pad whose record is recLen bytes long
// (nil when recLen is too short to carry the marker).
func padRecord(ekm []byte, recLen int) []byte {
	n := recLen - padOverhead - authTagLen - 2
	if n < 0 || recLen > 1<<14 {
		return nil
	}
	b := make([]byte, authTagLen+2+n)
	copy(b, padMarker(ekm))
	binary.BigEndian.PutUint16(b[authTagLen:], uint16(n))
	rand.Read(b[authTagLen+2:])
	return b
}

// readPadsThenProof skips any pad records, then reads the server proof.
func readPadsThenProof(r io.Reader, key, nonce, ekm []byte) error {
	mk := padMarker(ekm)
	for i := 0; i < 8; i++ {
		var head [proofHeadLen]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return fmt.Errorf("tlscarrier: reading server proof: %w", err)
		}
		if !bytes.Equal(head[:authTagLen], mk) {
			return readServerProof(io.MultiReader(bytes.NewReader(head[:]), r), key, nonce, ekm)
		}
		n := int64(binary.BigEndian.Uint16(head[authTagLen:]))
		if _, err := io.CopyN(io.Discard, r, n); err != nil {
			return fmt.Errorf("tlscarrier: reading pad: %w", err)
		}
	}
	return ErrServerProof
}
