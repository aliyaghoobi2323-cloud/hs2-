package core

import (
	"github.com/flynn/noise"
	"golang.org/x/crypto/blake2s"
)

// Datagram carrier support: the pieces the UDP carrier (udpcarrier/) needs on
// top of the stream path. Nothing here changes how the stream path behaves.

// Frame types used only by the datagram carrier. They live next to the stream
// types so the numbering stays in one place.
const (
	TypeShard    = 8  // FEC data shard (payload: fec header + shard)
	TypeParity   = 9  // FEC parity shard
	TypeFeedback = 10 // receiver report: loss, delivered bytes, RTT echo
	TypeAuth     = 11 // post-handshake key confirmation (see udpcarrier/auth.go)
	TypePathChal = 12 // path validation challenge (address change)
	TypePathResp = 13 // path validation response
)

// Exporter returns an n-byte (n <= 32) value bound to this session's handshake
// transcript, the datagram equivalent of a TLS exporter (RFC 8446 §7.5). Both
// ends of one handshake derive the same value for the same label; ends of two
// different handshakes (as a man-in-the-middle would hold) derive unrelated
// values. It is derived from its own root, so publishing a MAC over it reveals
// nothing about the traffic keys.
func (s *Session) Exporter(label string, n int) []byte {
	if n > 32 {
		n = 32
	}
	h, _ := blake2s.New256(s.exporter)
	h.Write([]byte("hs2 exporter v1"))
	h.Write([]byte(label))
	return h.Sum(nil)[:n]
}

// ReadMessage2Payload is ReadMessage2 that also returns the responder's
// decrypted message 2 payload. On success the transcript hash is kept for
// Binding.
func (i *Initiator) ReadMessage2Payload(msg []byte) (secret, payload []byte, err error) {
	payload, cs1, cs2, err := i.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, nil, ErrHandshakeAuth
	}
	i.binding = append([]byte(nil), i.hs.ChannelBinding()...)
	return foldSecret(cs1, cs2), payload, nil
}

// Binding returns the Noise handshake hash once message 2 has been verified
// (nil before). It covers the prologue, both static keys, both ephemerals and
// the psk, so it names this exact handshake.
func (i *Initiator) Binding() []byte { return i.binding }

// HandshakeBinding returns the handshake hash of a responder-side handshake
// state after WriteMessage2.
func HandshakeBinding(hs *noise.HandshakeState) []byte {
	return append([]byte(nil), hs.ChannelBinding()...)
}

// PeerStatic returns the initiator's static public key as authenticated by
// message 1, so a responder can pin which initiators it accepts.
func PeerStatic(hs *noise.HandshakeState) []byte {
	return append([]byte(nil), hs.PeerStatic()...)
}

// StaticFromSeed derives a 25519 keypair deterministically from a secret seed
// and a label. It lets both ends of a tunnel that already share a secret agree
// on pinned static keys without a second key exchange (see udpcarrier).
func StaticFromSeed(seed []byte, label string) (StaticKey, error) {
	h, _ := blake2s.New256(seed)
	h.Write([]byte("hs2 static from seed v1"))
	h.Write([]byte(label))
	kp, err := noise.DH25519.GenerateKeypair(&seedReader{b: h.Sum(nil)})
	if err != nil {
		return StaticKey{}, err
	}
	return StaticKey{Public: kp.Public, Private: kp.Private}, nil
}

// seedReader hands out a fixed 32-byte seed as the key generator's randomness.
type seedReader struct {
	b   []byte
	off int
}

func (r *seedReader) Read(p []byte) (int, error) {
	n := copy(p, r.b[r.off:])
	r.off += n
	for i := n; i < len(p); i++ {
		p[i] = 0
	}
	return len(p), nil
}
