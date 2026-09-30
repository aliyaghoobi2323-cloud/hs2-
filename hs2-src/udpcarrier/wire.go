package udpcarrier

import (
	"encoding/binary"
	"errors"
	"time"
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
//	[24:32] rxDataBytes     cumulative data datagram bytes received
//	[32:36] lossPPM         wire-loss estimate, parts per million
//	[36:40] owdTicks        minimum stamped one-way delay of the peer's data
//	                        this interval (our stampTick clock minus its
//	                        stamps; wraps at 32 bits)
//	[40]    flags           fbOWD: owdTicks is valid; fbStamps: we understand
//	                        stamped data datagrams (tagDataTS)
//
// The peer recovers RTT as (receiveNanos - echoNanos) - echoDelayNanos, which
// removes our processing hold, so the min over a window is the true base RTT.
// owdTicks gives it the queue on its forward path alone (see rateControl).
// Bytes 36..40 are optional on the wire: a report from an older peer ends at
// byte 36, which also tells us not to send it stamped datagrams.
const (
	feedbackLen    = 36 // minimum (older peers)
	feedbackLenExt = 41

	fbOWD    = 1 << 0
	fbStamps = 1 << 1
)

type feedback struct {
	sendNanos      int64
	echoNanos      int64
	echoDelayNanos int64
	rxDataBytes    uint64
	lossPPM        uint32
	owdTicks       uint32
	flags          byte
}

func (f feedback) encode() []byte {
	b := make([]byte, feedbackLenExt)
	binary.BigEndian.PutUint64(b[0:8], uint64(f.sendNanos))
	binary.BigEndian.PutUint64(b[8:16], uint64(f.echoNanos))
	binary.BigEndian.PutUint64(b[16:24], uint64(f.echoDelayNanos))
	binary.BigEndian.PutUint64(b[24:32], f.rxDataBytes)
	binary.BigEndian.PutUint32(b[32:36], f.lossPPM)
	binary.BigEndian.PutUint32(b[36:40], f.owdTicks)
	b[40] = f.flags
	return b
}

func decodeFeedback(b []byte) (feedback, bool) {
	if len(b) < feedbackLen {
		return feedback{}, false
	}
	f := feedback{
		sendNanos:      int64(binary.BigEndian.Uint64(b[0:8])),
		echoNanos:      int64(binary.BigEndian.Uint64(b[8:16])),
		echoDelayNanos: int64(binary.BigEndian.Uint64(b[16:24])),
		rxDataBytes:    binary.BigEndian.Uint64(b[24:32]),
		lossPPM:        binary.BigEndian.Uint32(b[32:36]),
	}
	if len(b) >= feedbackLenExt {
		f.owdTicks = binary.BigEndian.Uint32(b[36:40])
		f.flags = b[40]
	}
	return f, true
}

// Threat model of the cleartext datagram header (tag, wireSeq, stamp). These
// bytes sit OUTSIDE the AEAD because the receiver reads them before it can
// decrypt (wireSeq for loss measurement, stamp for one-way delay), exactly as
// wireSeq always has. So an ON-PATH attacker can forge them — a low stamp to
// make the path look un-queued, a high one to make it look congested — the
// same class of harm as their existing ability to drop, delay or reorder
// datagrams. It buys no data compromise: the payload's AEAD rejects any tamper,
// and the rate is backstopped by the delivered-byte count inside the SEALED
// feedback frame (deliveryCap in rate.go), which the attacker cannot inflate
// without actually delivering that many bytes. An off-path attacker cannot even
// place a datagram: the keyed framing magic (raw encaps) or the connected
// socket (udp) drops it first. The stamp is a relative tick, not a timestamp
// string, so it is not a fixed fingerprint, though its ~125 µs cadence is a
// distinguisher a future obfuscation layer may want to mask.
//
// stampTick is the unit of the send-time stamp on data datagrams: 125 µs,
// fine enough for queueing delay, and a 32-bit stamp wraps only every ~6 days
// (differences are taken modulo 2^32, so the wrap never matters).
const stampTick = 125 * time.Microsecond

func stampOf(t time.Time) uint32 { return uint32(t.UnixNano() / int64(stampTick)) }
