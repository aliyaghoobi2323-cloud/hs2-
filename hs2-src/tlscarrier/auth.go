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
	"os"
	"time"

	"golang.org/x/crypto/blake2s"
)

// Authentication happens INSIDE the TLS stream, after an ordinary handshake,
// and is bound to that exact TLS session (channel binding) in both directions.
// This is wire protocol v3 (docs/architecture/REDESIGN.fa.md §4.1).
//
// Client -> server, the first application-data record:
//
//	[ nonce  : 16 random bytes ]
//	[ tag    : 16 = HMAC(key, "hs2-auth-v3" || nonce || minute || EKM || hdr) ]
//	[ hdr    : 7 = ver(3) || min_level || max_level || caps u32 BE ]
//	[ padlen : 2 ]
//	[ pad    : padlen random bytes ]            (sized like an HTTP request)
//
// Server -> client, only after the client's tag verified:
//
//	[ tag    : 16 = HMAC(key, "hs2-srv-v3" || nonce || EKM || hdr) ]
//	[ hdr    : 6 = ver(3) || level || caps u32 BE ]
//	[ padlen : 2 ]
//	[ pad    : padlen random bytes ]            (sized like response headers)
//
// The headers are under the HMAC, and the pad ranges are shorter by exactly
// the header, so both records keep the lengths v2 had on the wire (client
// 314..754, server 138..498). The server picks the highest level both ends
// speak and the caps both have (negotiate); with none in common it answers
// level 0 and closes the link, so the client can say why.
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
// real backend and sees an ordinary website. A v2 hs2 with the right key is
// told apart only to log it and close the link: there is no v2 mode.
//
// HMAC-BLAKE2s-256 truncated to 128 bits; the minute bucket bounds how long a
// record stays valid, and the nonce memory rejects exact repeats (already
// impossible across connections because of the EKM binding).

const (
	protoVersion byte = 3

	authNonceLen  = 16
	authTagLen    = 16
	authHdrLen    = 7 // ver, min_level, max_level, caps u32
	proofHdrLen   = 6 // ver, level, caps u32
	authHeadLen   = authNonceLen + authTagLen + authHdrLen + 2
	proofHeadLen  = authTagLen + proofHdrLen + 2
	authClockSkew = 2 // accept current minute +/- this many
	authTimeout   = 10 * time.Second
	exporterLabel = "EXPORTER-hs2-channel-binding-v2"

	// v2 had no header and 7/6 more bytes of pad: the totals are the same.
	clientPadMin, clientPadMax = 280 - authHdrLen, 720 - authHdrLen   // a typical HTTP/1.1 GET
	serverPadMin, serverPadMax = 120 - proofHdrLen, 480 - proofHdrLen // typical response headers

	// The protocol levels this binary speaks (§4.1): one up with every change
	// of the wire format; min stays down to the previous build's max.
	levelMin, levelMax byte = 1, 1

	legacyRecordLen = 32 // v1 clients: nonce + tag, no binding
)

// The caps bits: what this side's cores can do on a link. One is used only
// when both ends have it; each core's kill switch takes its bit out.
const (
	CapClass   uint32 = 1 << iota // C1, the classifier (HS2_TUNE_CLASS)
	CapSched                      // C2, the link scheduler (HS2_TUNE_SCHED)
	CapHostQ                      // C3, the host queue guard (HS2_TUNE_HQG)
	CapSendCap                    // C4-lite, the pacing cap (HS2_TUNE_SENDCAP)
)

// AuthHdr is the header of the client's record (7 B on the wire).
type AuthHdr struct {
	Ver, MinLevel, MaxLevel byte
	Caps                    uint32
}

// ProofHdr is the header of the server's proof (6 B on the wire): the level
// it chose (0: none in common) and the caps both ends have.
type ProofHdr struct {
	Ver, Level byte
	Caps       uint32
}

var (
	// ErrServerProof means the peer did not prove knowledge of the shared key
	// for this TLS session: wrong key, an interceptor, or not an hs2 server.
	ErrServerProof = errors.New("tlscarrier: server did not prove the shared key (wrong shared_key, man-in-the-middle, or not hs2)")
	// ErrOldServer means the peer answered with its cover website — what an hs2
	// server does for anyone who cannot prove the shared key. That is a
	// DIFFERENT shared_key (most often: the link was pasted from an earlier
	// setup of the other server), or an hs2 older than protocol v3, whose
	// server cannot read a v3 record. Which side is the TLS server depends on
	// the direction, so the message names neither.
	ErrOldServer = errors.New("tlscarrier: the other server answered as a plain website, not as the tunnel: " +
		"its shared_key differs from this one (paste the CURRENT link from it — running its setup again makes a new key), " +
		"or it runs an older hs2 (protocol v2 or earlier: upgrade both servers)")
	// ErrPeerV2 is what a server says of a v2 hs2 that proved the key.
	ErrPeerV2 = errors.New("peer runs protocol v2; upgrade both servers")
	// ErrLevel means the two servers speak no protocol level in common.
	ErrLevel = errors.New("no common protocol level with the peer")
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

func clientTag(key, nonce, ekm []byte, bucket int64, hdr []byte) []byte {
	return mac(key, []byte("hs2-auth-v3"), nonce, be64(bucket), ekm, hdr)
}

// clientTagV2 is the v2 client tag: checked only to name a v2 peer in the log.
func clientTagV2(key, nonce, ekm []byte, bucket int64) []byte {
	return mac(key, []byte("hs2-auth-v2"), nonce, be64(bucket), ekm)
}

func serverTag(key, nonce, ekm, hdr []byte) []byte {
	return mac(key, []byte("hs2-srv-v3"), nonce, ekm, hdr)
}

func (h AuthHdr) bytes() []byte {
	b := []byte{h.Ver, h.MinLevel, h.MaxLevel, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[3:], h.Caps)
	return b
}

func (p ProofHdr) bytes() []byte {
	b := []byte{p.Ver, p.Level, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[2:], p.Caps)
	return b
}

// localCaps is every cap this binary has, less those whose kill switch is set.
func localCaps() uint32 {
	caps := CapClass | CapSched | CapHostQ | CapSendCap
	for _, k := range []struct {
		env string
		bit uint32
	}{{"HS2_TUNE_CLASS", CapClass}, {"HS2_TUNE_SCHED", CapSched}, {"HS2_TUNE_HQG", CapHostQ}, {"HS2_TUNE_SENDCAP", CapSendCap}} {
		if os.Getenv(k.env) == "0" {
			caps &^= k.bit
		}
	}
	return caps
}

// LocalAuthHdr is the header this binary sends: its levels and its caps.
func LocalAuthHdr() AuthHdr {
	return AuthHdr{Ver: protoVersion, MinLevel: levelMin, MaxLevel: levelMax, Caps: localCaps()}
}

// negotiate picks the level and caps for a link (§4.1): the highest level
// both ranges hold, and the caps both ends have.
func negotiate(c AuthHdr, srvMin, srvMax byte, srvCaps uint32) (level byte, caps uint32, ok bool) {
	level = min(c.MaxLevel, srvMax)
	if level < max(c.MinLevel, srvMin) {
		return 0, 0, false
	}
	return level, c.Caps & srvCaps, true
}

// checkProofLevel is the client's check of the level the server chose: it
// must be one the client speaks (0, the server's "none in common", is not).
func checkProofLevel(h AuthHdr, p ProofHdr) error {
	if p.Level < h.MinLevel || p.Level > h.MaxLevel {
		return fmt.Errorf("%w (this server speaks levels %d-%d, the other server none of them): upgrade the older server",
			ErrLevel, h.MinLevel, h.MaxLevel)
	}
	return nil
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
func makeClientAuth(key, ekm []byte, h AuthHdr) (rec, nonce []byte) {
	nonce = make([]byte, authNonceLen)
	rand.Read(nonce)
	hdr := h.bytes()
	head := append(append([]byte{}, nonce...), clientTag(key, nonce, ekm, minuteBucket(), hdr)...)
	return padded(append(head, hdr...), clientPadMin, clientPadMax), nonce
}

// parseClientAuth checks the head of the client's first record. On success it
// returns the nonce, the client's header and how many pad bytes are still to
// be read beyond first. v2 reports a v2 hs2 that proved the key: refused, but
// told apart from a probe for the log.
func parseClientAuth(key, ekm, first []byte) (nonce []byte, h AuthHdr, more int, v2 bool, ok bool) {
	if len(first) < authNonceLen+authTagLen {
		return nil, AuthHdr{}, 0, false, false
	}
	n, tag := first[:authNonceLen], first[authNonceLen:authNonceLen+authTagLen]
	now := minuteBucket()
	if len(first) >= authHeadLen {
		hdr := first[authNonceLen+authTagLen : authNonceLen+authTagLen+authHdrLen]
		match := 0
		for d := int64(-authClockSkew); d <= authClockSkew; d++ {
			match |= subtle.ConstantTimeCompare(tag, clientTag(key, n, ekm, now+d, hdr))
		}
		if match == 1 {
			h = AuthHdr{Ver: hdr[0], MinLevel: hdr[1], MaxLevel: hdr[2], Caps: binary.BigEndian.Uint32(hdr[3:])}
			pad := int(binary.BigEndian.Uint16(first[authHeadLen-2 : authHeadLen]))
			more = authHeadLen + pad - len(first)
			if more < 0 || h.Ver != protoVersion {
				return nil, AuthHdr{}, 0, false, false // trailing bytes, or not our version: not our framing
			}
			return n, h, more, false, true
		}
	}
	match := 0
	for d := int64(-authClockSkew); d <= authClockSkew; d++ {
		match |= subtle.ConstantTimeCompare(tag, clientTagV2(key, n, ekm, now+d))
	}
	return nil, AuthHdr{}, 0, match == 1, false
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
func makeServerProof(key, nonce, ekm []byte, p ProofHdr) []byte {
	hdr := p.bytes()
	return padded(append(serverTag(key, nonce, ekm, hdr), hdr...), serverPadMin, serverPadMax)
}

// readServerProof reads and verifies the server's answer and returns its
// header; the level in it is for the caller to check (checkProofLevel).
func readServerProof(r io.Reader, key, nonce, ekm []byte) (ProofHdr, error) {
	var head [proofHeadLen]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return ProofHdr{}, fmt.Errorf("tlscarrier: reading server proof: %w", err)
	}
	if string(head[:5]) == "HTTP/" {
		return ProofHdr{}, ErrOldServer
	}
	hdr := head[authTagLen : authTagLen+proofHdrLen]
	if subtle.ConstantTimeCompare(head[:authTagLen], serverTag(key, nonce, ekm, hdr)) != 1 || hdr[0] != protoVersion {
		return ProofHdr{}, ErrServerProof
	}
	p := ProofHdr{Ver: hdr[0], Level: hdr[1], Caps: binary.BigEndian.Uint32(hdr[2:])}
	pad := int(binary.BigEndian.Uint16(head[authTagLen+proofHdrLen:]))
	if _, err := io.CopyN(io.Discard, r, int64(pad)); err != nil {
		return ProofHdr{}, fmt.Errorf("tlscarrier: reading server proof: %w", err)
	}
	return p, nil
}
