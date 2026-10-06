package core

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"sync"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
)

// Session is one direction-pair of keys plus the counters that keep a datagram
// carrier safe. It is created from the 32-byte secret the handshake produces
// (see handshake.go) and is what the pumps use to turn payloads into frames and
// back.
//
// Two AEAD keys and two length-mask keys are derived from the handshake secret
// by domain-separated hashing, one set per direction, so the two directions
// never share a keystream and a frame captured in one direction cannot be
// replayed into the other.

type Session struct {
	sendAEAD cipher.AEAD
	recvAEAD cipher.AEAD
	sendLen  []byte // length-mask key, send direction
	recvLen  []byte // length-mask key, recv direction

	mu      sync.Mutex
	sendSeq uint64
	replay  replayWindow

	// id is the stable session identifier, independent of any carrier. A
	// migration to a new path presents this so the peer continues the session
	// rather than starting a new one. Not yet wired to carriers in v0.1.
	id uint64

	// exporter is the root of Exporter values (see datagram.go): a secret
	// both ends derive from the handshake, never used as a traffic key.
	exporter []byte
}

var errReplayed = errors.New("core: replayed or stale frame")

// deriveKey hashes the secret with a label to produce one 32-byte subkey.
func deriveKey(secret []byte, label string) []byte {
	h, _ := blake2s.New256(nil)
	h.Write([]byte(label))
	h.Write(secret)
	return h.Sum(nil)
}

// NewSession builds a session from the handshake secret. initiator decides
// which direction label each side uses, so the two ends agree on which key is
// "send" for whom.
func NewSession(secret []byte, initiator bool, id uint64) (*Session, error) {
	i2r := deriveKey(secret, "hs2 i->r aead")
	r2i := deriveKey(secret, "hs2 r->i aead")
	i2rLen := deriveKey(secret, "hs2 i->r len")
	r2iLen := deriveKey(secret, "hs2 r->i len")

	var sendKey, recvKey, sendLen, recvLen []byte
	if initiator {
		sendKey, recvKey, sendLen, recvLen = i2r, r2i, i2rLen, r2iLen
	} else {
		sendKey, recvKey, sendLen, recvLen = r2i, i2r, r2iLen, i2rLen
	}
	sa, err := chacha20poly1305.New(sendKey)
	if err != nil {
		return nil, err
	}
	ra, err := chacha20poly1305.New(recvKey)
	if err != nil {
		return nil, err
	}
	return &Session{
		sendAEAD: sa, recvAEAD: ra,
		sendLen: sendLen, recvLen: recvLen,
		id:       id,
		exporter: deriveKey(secret, "hs2 exporter root"),
	}, nil
}

// ID returns the stable session identifier.
func (s *Session) ID() uint64 { return s.id }

// Seal turns a payload into a wire frame, assigning the next send sequence.
// padTo pads the plaintext so the frame length hides the real size; pass 0 for
// no padding.
func (s *Session) Seal(ftype, flags byte, payload []byte, padTo int) ([]byte, error) {
	s.mu.Lock()
	seq := s.sendSeq
	s.sendSeq++
	s.mu.Unlock()
	return sealFrame(s.sendAEAD, s.sendLen, ftype, flags, seq, payload, padTo)
}

// nextRecvSeqForMask is the sequence the receiver expects for the next frame's
// length mask. Because datagrams arrive out of order, a stream carrier and a
// datagram carrier read the length differently: this v0.1 targets a stream
// carrier (TCP), where frames arrive in order, so the receive-side mask counter
// simply counts up. carrier_udp.go will replace this with the explicit-seq
// datagram path.
type StreamReader struct {
	sess    *Session
	recvSeq uint64
}

// NewStreamReader makes a reader for an in-order (stream) carrier.
func (s *Session) NewStreamReader() *StreamReader { return &StreamReader{sess: s} }

// ReadFrame reads and decrypts one frame from an in-order carrier.
func (r *StreamReader) ReadFrame(src interface {
	Read([]byte) (int, error)
}) (ftype, flags byte, payload []byte, err error) {
	ct, err := readFrame(readerFunc(src.Read), r.sess.recvLen, r.recvSeq)
	if err != nil {
		return 0, 0, nil, err
	}
	ftype, flags, payload, err = openFrame(r.sess.recvAEAD, r.recvSeq, ct)
	if err != nil {
		return 0, 0, nil, err
	}
	if !r.sess.replay.check(r.recvSeq) {
		return 0, 0, nil, errReplayed
	}
	r.recvSeq++
	return ftype, flags, payload, nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// OpenDatagram decrypts a single self-contained datagram frame, where the
// sequence is explicit and frames may arrive out of order. Used by the UDP
// carrier. The length prefix is unmasked using the seq carried in the frame,
// which we can read because the first 2 bytes are the masked length and the
// seq is inside — so for datagrams the sender also prepends the raw seq masked
// the same way. v0.1 stream path does not use this; kept for carrier_udp.go.
func (s *Session) OpenDatagram(seq uint64, ct []byte) (ftype, flags byte, payload []byte, err error) {
	ftype, flags, payload, err = openFrame(s.recvAEAD, seq, ct)
	if err != nil {
		return 0, 0, nil, err
	}
	if !s.replay.check(seq) {
		return 0, 0, nil, errReplayed
	}
	return ftype, flags, payload, nil
}

// SealDatagram is Seal but also returns the sequence, which a datagram carrier
// must place on the wire so the receiver can position the mask and nonce.
func (s *Session) SealDatagram(ftype, flags byte, payload []byte, padTo int) (seq uint64, frame []byte, err error) {
	s.mu.Lock()
	seq = s.sendSeq
	s.sendSeq++
	s.mu.Unlock()
	frame, err = sealFrame(s.sendAEAD, s.sendLen, ftype, flags, seq, payload, padTo)
	return seq, frame, err
}

// AppendDatagram seals a frame and appends it to dst in a datagram carrier's
// wire layout, [seq:8][ciphertext] — the bytes SealDatagram's frame carries
// after its masked length, behind the sequence the receiver needs. The frame
// is built in dst and encrypted over itself: one buffer and one copy of the
// payload, where SealDatagram makes three and copies four times. dst grows
// only when its capacity is short (see DatagramLen).
func (s *Session) AppendDatagram(dst []byte, ftype, flags byte, payload []byte, padTo int) ([]byte, error) {
	if len(payload) > maxPayload {
		return dst, errLongFrame
	}
	s.mu.Lock()
	seq := s.sendSeq
	s.sendSeq++
	s.mu.Unlock()
	return appendSealed(dst, s.sendAEAD, ftype, flags, seq, payload, padTo), nil
}

// DatagramLen is the length AppendDatagram adds for a payload of n bytes
// padded to padTo.
func DatagramLen(n, padTo int) int { return datagramSeqLen + frameHeaderLen + max(n, padTo) + tagLen }

// datagramSeqLen is the explicit sequence in front of a datagram's ciphertext.
const datagramSeqLen = 8

// appendSealed is sealFrame without the length prefix, in place (see
// AppendDatagram): the same inner frame — header, payload, zero padding —
// and the same nonce, so the ciphertext is byte for byte the one sealFrame
// returns.
func appendSealed(dst []byte, aead cipher.AEAD, ftype, flags byte, seq uint64, payload []byte, padTo int) []byte {
	plen := len(payload)
	body := max(plen, padTo)
	n := len(dst)
	inLen := frameHeaderLen + body
	end := n + datagramSeqLen + inLen + aead.Overhead()
	if cap(dst) < end {
		nd := make([]byte, n, end)
		copy(nd, dst)
		dst = nd
	}
	dst = dst[:n+datagramSeqLen+inLen]
	binary.BigEndian.PutUint64(dst[n:], seq)
	inner := dst[n+datagramSeqLen:]
	inner[0] = ftype
	inner[1] = flags
	binary.BigEndian.PutUint16(inner[2:4], uint16(plen))
	binary.BigEndian.PutUint64(inner[4:frameHeaderLen], seq)
	copy(inner[frameHeaderLen:], payload)
	clear(inner[frameHeaderLen+plen:])
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)
	aead.Seal(inner[:0], nonce[:], inner, nil)
	return dst[:end]
}

// SendSeq exposes the current send counter for tests.
func (s *Session) SendSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendSeq
}

// idBytes is a helper for carriers that put the session id on the wire.
func idBytes(id uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, id)
	return b
}
