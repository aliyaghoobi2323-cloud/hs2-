package udpcarrier

import (
	"encoding/binary"
	"errors"
)

// On-wire layout of one hs2 datagram frame, before FEC wraps it in a shard:
//
//	[seq:8][ciphertext]
//
// core.SealDatagram returns the sealed frame as [maskedLen:2][ciphertext]; the
// masked length is only needed by the stream reader to find a frame boundary in
// a byte stream. Datagrams keep their own boundaries, so we drop it and instead
// prepend the explicit 8-byte sequence the receiver needs to unmask and decrypt
// (datagrams arrive out of order, so it cannot be inferred). The whole thing is
// one FEC payload; FEC adds its 9-byte shard header on top.
const seqLen = 8

var errShortDatagram = errors.New("udpcarrier: datagram shorter than its sequence")

// packDatagram builds seq(8) || ct from the sealed frame core returns. It
// writes into dst (reused across sends under the carrier's send mutex) and
// returns the filled slice.
func packDatagram(dst []byte, seq uint64, sealed []byte) []byte {
	// sealed is [maskedLen:2][ct]; keep only ct.
	ct := sealed
	if len(sealed) >= 2 {
		ct = sealed[2:]
	}
	if cap(dst) < seqLen+len(ct) {
		dst = make([]byte, seqLen+len(ct))
	}
	dst = dst[:seqLen+len(ct)]
	binary.BigEndian.PutUint64(dst[:seqLen], seq)
	copy(dst[seqLen:], ct)
	return dst
}

// unpackDatagram splits seq(8) || ct back apart.
func unpackDatagram(b []byte) (seq uint64, ct []byte, err error) {
	if len(b) < seqLen {
		return 0, nil, errShortDatagram
	}
	return binary.BigEndian.Uint64(b[:seqLen]), b[seqLen:], nil
}

// feedback is the periodic receiver report that drives the peer's rate model
// and the transport selector. It is sent as an ordinary sealed TypeFeedback
// frame (encrypted, authenticated, replay-protected) — not in the clear.
//
//	[0:8]   sendNanos       when this report was made (our wall clock)
//	[8:16]  echoNanos       the last sendNanos we received from the peer (0 if none)
//	[16:24] echoDelayNanos  how long we held that report before replying
//	[24:32] rxDataBytes     cumulative TypeData payload bytes delivered at receiver
//	[32:36] lossPPM         wire-loss estimate, parts per million
//
// The peer recovers RTT as (receiveNanos - echoNanos) - echoDelayNanos, which
// removes our processing hold, so the min over a window is the true base RTT.
const feedbackLen = 36

type feedback struct {
	sendNanos      int64
	echoNanos      int64
	echoDelayNanos int64
	rxDataBytes    uint64
	lossPPM        uint32
}

func (f feedback) encode() []byte {
	b := make([]byte, feedbackLen)
	binary.BigEndian.PutUint64(b[0:8], uint64(f.sendNanos))
	binary.BigEndian.PutUint64(b[8:16], uint64(f.echoNanos))
	binary.BigEndian.PutUint64(b[16:24], uint64(f.echoDelayNanos))
	binary.BigEndian.PutUint64(b[24:32], f.rxDataBytes)
	binary.BigEndian.PutUint32(b[32:36], f.lossPPM)
	return b
}

func decodeFeedback(b []byte) (feedback, bool) {
	if len(b) < feedbackLen {
		return feedback{}, false
	}
	return feedback{
		sendNanos:      int64(binary.BigEndian.Uint64(b[0:8])),
		echoNanos:      int64(binary.BigEndian.Uint64(b[8:16])),
		echoDelayNanos: int64(binary.BigEndian.Uint64(b[16:24])),
		rxDataBytes:    binary.BigEndian.Uint64(b[24:32]),
		lossPPM:        binary.BigEndian.Uint32(b[32:36]),
	}, true
}
