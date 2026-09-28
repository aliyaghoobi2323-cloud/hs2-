// Package core is the transport-independent heart of the tunnel: the wire
// frame, the cryptographic session, and the replay window. Nothing here knows
// about TCP, UDP, TUN devices, or routing.
package core

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20"
)

// Wire frame, one per carrier record. Nothing on the wire is a fixed constant:
//
//	[ length : 2 bytes, MASKED with a keystream ]
//	[ AEAD ciphertext of:  type(1) flags(1) plen(2) seq(8) payload pad ]  (+16B tag)
//
// The length prefix is masked because a plaintext length counting up with the
// packet size is itself a fingerprint. Padding lives INSIDE the AEAD, so it is
// authenticated; the header's plen field recovers the real payload.

const (
	frameHeaderLen = 12 // type(1) flags(1) plen(2) seq(8)
	tagLen         = 16
	lenPrefix      = 2
	maxPayload     = 60000
)

// Frame types.
const (
	TypeData      = 1
	TypeHandshake = 2
	TypePing      = 3
	TypePong      = 4
	TypeClose     = 5
)

var (
	errShortFrame = errors.New("core: frame shorter than header+tag")
	errLongFrame  = errors.New("core: frame exceeds maximum size")
	errBadInner   = errors.New("core: inner frame malformed")
)

func lengthMask(lenKey []byte, seq uint64) [lenPrefix]byte {
	var nonce [chacha20.NonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)
	c, err := chacha20.NewUnauthenticatedCipher(lenKey, nonce[:])
	if err != nil {
		panic("core: length cipher: " + err.Error())
	}
	var out [lenPrefix]byte
	c.XORKeyStream(out[:], out[:])
	return out
}

func sealFrame(aead cipher.AEAD, lenKey []byte, ftype, flags byte, seq uint64, payload []byte, padTo int) ([]byte, error) {
	if len(payload) > maxPayload {
		return nil, errLongFrame
	}
	plen := len(payload)
	body := plen
	if padTo > body {
		body = padTo
	}
	inner := make([]byte, frameHeaderLen+body)
	inner[0] = ftype
	inner[1] = flags
	binary.BigEndian.PutUint16(inner[2:4], uint16(plen))
	binary.BigEndian.PutUint64(inner[4:frameHeaderLen], seq)
	copy(inner[frameHeaderLen:], payload)

	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)
	ct := aead.Seal(nil, nonce[:], inner, nil)

	frame := make([]byte, lenPrefix+len(ct))
	binary.BigEndian.PutUint16(frame[:lenPrefix], uint16(len(ct)))
	mask := lengthMask(lenKey, seq)
	frame[0] ^= mask[0]
	frame[1] ^= mask[1]
	copy(frame[lenPrefix:], ct)
	return frame, nil
}

// openFrame decrypts a ciphertext read with readFrame. seq is the sequence
// number used to unmask the length prefix and is also the AEAD nonce; the
// header seq must match it.
func openFrame(aead cipher.AEAD, seq uint64, ct []byte) (ftype, flags byte, payload []byte, err error) {
	if len(ct) < frameHeaderLen+tagLen {
		return 0, 0, nil, errShortFrame
	}
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], seq)
	inner, err := aead.Open(nil, nonce[:], ct, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(inner) < frameHeaderLen {
		return 0, 0, nil, errBadInner
	}
	ftype = inner[0]
	flags = inner[1]
	plen := int(binary.BigEndian.Uint16(inner[2:4]))
	hseq := binary.BigEndian.Uint64(inner[4:frameHeaderLen])
	if hseq != seq {
		return 0, 0, nil, errBadInner
	}
	if frameHeaderLen+plen > len(inner) {
		return 0, 0, nil, errBadInner
	}
	return ftype, flags, inner[frameHeaderLen : frameHeaderLen+plen], nil
}

// readFrame reads exactly one frame from a stream carrier using the masked
// length prefix. seqForMask must be the sequence number the sender used.
func readFrame(r io.Reader, lenKey []byte, seqForMask uint64) ([]byte, error) {
	var pfx [lenPrefix]byte
	if _, err := io.ReadFull(r, pfx[:]); err != nil {
		return nil, err
	}
	mask := lengthMask(lenKey, seqForMask)
	clen := binary.BigEndian.Uint16([]byte{pfx[0] ^ mask[0], pfx[1] ^ mask[1]})
	if int(clen) < frameHeaderLen+tagLen {
		return nil, errShortFrame
	}
	if int(clen) > maxPayload+frameHeaderLen+tagLen {
		return nil, errLongFrame
	}
	ct := make([]byte, clen)
	if _, err := io.ReadFull(r, ct); err != nil {
		return nil, err
	}
	return ct, nil
}
