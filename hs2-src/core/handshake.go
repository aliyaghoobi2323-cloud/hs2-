package core

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	"github.com/flynn/noise"
	"golang.org/x/crypto/blake2s"
)

// The handshake is Noise IK with a pre-shared key (psk2 placement), over
// 25519 / ChaCha20-Poly1305 / BLAKE2s. IK means the initiator already knows
// the responder's static key (pinned in the client config) and transmits its
// own static key encrypted; the psk folds in a second, out-of-band secret so
// that knowing the static keypair alone is not enough to complete it.
//
// Probe resistance is layered on top, in the prologue and a first-message MAC:
//
//   - The prologue binds a protocol label and a coarse timestamp. A responder
//     rejects a first message whose timestamp is outside a window, and remembers
//     recent (timestamp,mac) pairs so a captured first message cannot be
//     replayed. Both are checked BEFORE any Noise processing, so a probe with a
//     random or stale first message is indistinguishable, to the prober, from a
//     host that simply is not there.
//   - A responder that rejects a first message returns NOTHING and (in the
//     carrier layer, not here) hands the raw connection to a decoy. Silence and
//     RST are themselves fingerprints; the decoy is added in carrier code.

const (
	hsPattern      = "hs2-IKpsk2-v1"
	tsWindow       = 90 * time.Second
	firstMsgMinLen = 32
)

var (
	ErrHandshakeStale  = errors.New("core: handshake timestamp outside window")
	ErrHandshakeReplay = errors.New("core: handshake first message replayed")
	ErrHandshakeAuth   = errors.New("core: handshake authentication failed")
)

// StaticKey is a 25519 keypair.
type StaticKey struct {
	Public  []byte
	Private []byte
}

// GenerateStatic makes a fresh static keypair.
func GenerateStatic() (StaticKey, error) {
	dh := noise.DH25519
	kp, err := dh.GenerateKeypair(nil)
	if err != nil {
		return StaticKey{}, err
	}
	return StaticKey{Public: kp.Public, Private: kp.Private}, nil
}

func cipherSuite() noise.CipherSuite {
	return noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
}

// prologue binds the protocol label and a coarse timestamp bucket. Both ends
// compute it from the same bucket, so it authenticates the timestamp as
// additional data without putting a forgeable field in the clear.
func prologue(bucket int64) []byte {
	p := make([]byte, 0, len(hsPattern)+8)
	p = append(p, []byte(hsPattern)...)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(bucket))
	return append(p, b[:]...)
}

func nowBucket() int64 { return time.Now().Unix() / 30 }

// firstMAC is a keyed tag over the timestamp bucket, keyed by the psk. A
// responder can check it before touching Noise, which is what makes an invalid
// probe cheap to reject and silent.
//
// It also covers the handshake's ephemeral key, so every handshake carries a
// different tag: a tag over the bucket alone was identical for all handshakes
// in a 30s window, which made the replay memory reject every legitimate
// reconnect after the first and gave observers a repeating 16-byte prefix.
func firstMAC(psk []byte, bucket int64, ephemeral []byte) []byte {
	h, _ := blake2s.New256(psk)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(bucket))
	h.Write([]byte("hs2 first mac v2"))
	h.Write(b[:])
	h.Write(ephemeral)
	return h.Sum(nil)[:16]
}

// ephemeralLen is the size of the X25519 ephemeral key that opens an IK
// message 1. Its top bit is always 0 as generated; the sender randomises it
// on the wire (X25519 ignores that bit, RFC 7748 §5) and the receiver clears
// it again, so the key does not carry a constant bit observers could count.
const ephemeralLen = 32

func randomiseTopBit(msg []byte) {
	var r [1]byte
	rand.Read(r[:])
	msg[ephemeralLen-1] |= r[0] & 0x80
}

// Initiator drives the dialling side.
type Initiator struct {
	hs  *noise.HandshakeState
	psk []byte
}

// NewInitiator builds the initiator state. remoteStatic is the responder's
// pinned public key; psk is the shared pre-shared key.
func NewInitiator(local StaticKey, remoteStatic, psk []byte) (*Initiator, error) {
	bucket := nowBucket()
	cfg := noise.Config{
		CipherSuite:   cipherSuite(),
		Pattern:       noise.HandshakeIK,
		Initiator:     true,
		Prologue:      prologue(bucket),
		StaticKeypair: noise.DHKey{Public: local.Public, Private: local.Private},
		PeerStatic:    remoteStatic,
		PresharedKey:  psk,
		// psk placement 2: mixed after the first DH, i.e. IKpsk2.
		PresharedKeyPlacement: 2,
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, err
	}
	return &Initiator{hs: hs, psk: psk}, nil
}

// WriteMessage1 produces the first handshake message, with the probe-resistance
// MAC prepended.
func (i *Initiator) WriteMessage1() ([]byte, error) {
	bucket := nowBucket()
	msg, _, _, err := i.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	randomiseTopBit(msg)
	out := append(firstMAC(i.psk, bucket, msg[:ephemeralLen]), msg...)
	return out, nil
}

// ReadMessage2 consumes the responder's reply and, on success, returns the two
// cipher states as a single 64-byte secret (32 per direction) folded together.
func (i *Initiator) ReadMessage2(msg []byte) (secret []byte, err error) {
	_, cs1, cs2, err := i.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, ErrHandshakeAuth
	}
	return foldSecret(cs1, cs2), nil
}

// Responder drives the listening side and holds the anti-replay memory.
type Responder struct {
	local StaticKey
	psk   []byte
	seen  *replayMemory
}

// NewResponder builds responder state. It keeps a small memory of recently
// accepted first-message MACs to refuse replays.
func NewResponder(local StaticKey, psk []byte) *Responder {
	return &Responder{local: local, psk: psk, seen: newReplayMemory()}
}

// ReadMessage1 validates and consumes a first message. It checks the cheap
// probe-resistance MAC and timestamp before any Noise work; a failure here
// returns an error and the carrier is expected to fall through to the decoy
// without replying.
func (r *Responder) ReadMessage1(in []byte) (hs *noise.HandshakeState, err error) {
	if len(in) < 16+firstMsgMinLen {
		return nil, ErrHandshakeAuth
	}
	mac := in[:16]
	msg := append([]byte(nil), in[16:]...)
	// Accept the current bucket or the one before it, to tolerate clock skew
	// and a message in flight across a boundary.
	now := nowBucket()
	var okBucket int64
	matched := false
	for _, b := range []int64{now, now - 1, now + 1} {
		if constEq(mac, firstMAC(r.psk, b, msg[:ephemeralLen])) {
			okBucket, matched = b, true
			break
		}
	}
	if !matched {
		return nil, ErrHandshakeAuth
	}
	if !withinWindow(okBucket) {
		return nil, ErrHandshakeStale
	}
	if !r.seen.add(mac) {
		return nil, ErrHandshakeReplay
	}
	msg[ephemeralLen-1] &^= 0x80
	cfg := noise.Config{
		CipherSuite:           cipherSuite(),
		Pattern:               noise.HandshakeIK,
		Initiator:             false,
		Prologue:              prologue(okBucket),
		StaticKeypair:         noise.DHKey{Public: r.local.Public, Private: r.local.Private},
		PresharedKey:          r.psk,
		PresharedKeyPlacement: 2,
	}
	hs, err = noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg); err != nil {
		return nil, ErrHandshakeAuth
	}
	return hs, nil
}

// WriteMessage2 produces the responder's reply and the folded secret.
func (r *Responder) WriteMessage2(hs *noise.HandshakeState) (msg, secret []byte, err error) {
	msg, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return msg, foldSecret(cs1, cs2), nil
}

// foldSecret turns the two cipher states into one 32-byte secret both ends can
// derive identically. We hash the two cipher keys under a label; the session
// then derives its four subkeys from this.
func foldSecret(cs1, cs2 *noise.CipherState) []byte {
	h, _ := blake2s.New256(nil)
	h.Write([]byte("hs2 traffic secret"))
	// CipherState does not expose its key directly; use an encryption of a
	// fixed block as a stable, secret-derived value. Both ends produce the same
	// because they hold the same keys in the same order.
	h.Write(csFingerprint(cs1))
	h.Write(csFingerprint(cs2))
	return h.Sum(nil)
}

// csFingerprint derives a stable secret value from a cipher state by encrypting
// a fixed zero block at nonce 0. Deterministic and identical on both ends.
func csFingerprint(cs *noise.CipherState) []byte {
	if cs == nil {
		return make([]byte, 16)
	}
	var zero [32]byte
	ct, err := cs.Encrypt(nil, nil, zero[:])
	if err != nil {
		return make([]byte, 16)
	}
	h, _ := blake2s.New256(nil)
	h.Write(ct)
	return h.Sum(nil)
}

func withinWindow(bucket int64) bool {
	skew := time.Duration(nowBucket()-bucket) * 30 * time.Second
	if skew < 0 {
		skew = -skew
	}
	return skew <= tsWindow
}

// constEq is a constant-time compare.
func constEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
