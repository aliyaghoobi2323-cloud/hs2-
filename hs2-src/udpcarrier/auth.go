package udpcarrier

import (
	"crypto/hmac"
	"crypto/subtle"
	"hash"

	"golang.org/x/crypto/blake2s"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// Post-handshake key confirmation, the datagram equivalent of the tlscarrier
// channel binding (see tlscarrier/auth.go). The Noise IKpsk2 handshake with
// pinned static keys already authenticates both ends, so this is the second
// layer that makes a *terminating* man-in-the-middle fail the same way it does
// on the TCP carrier: a relay that completes two separate handshakes (one with
// each side) holds two unrelated session exporters and two unrelated transcript
// bindings, so a confirmation tag it receives from one side cannot be made to
// verify on the other, and it cannot forge one without the shared key.
//
//	client -> server (TypeAuth):  HMAC(shared, "hs2-udp-cli-v1" || binding || exporter)
//	server -> client (TypeAuth):  HMAC(shared, "hs2-udp-srv-v1" || binding || exporter)
//
// exporter = session.Exporter("hs2-udp-channel-binding", 32), derived from the
// handshake's own exporter root; binding = the Noise transcript hash. Both are
// identical on the two ends of ONE handshake and unrelated across two.

const (
	confirmTagLen  = 16
	exporterLabel  = "hs2-udp-channel-binding"
	clientAuthDom  = "hs2-udp-cli-v1"
	serverAuthDom  = "hs2-udp-srv-v1"
	exporterLength = 32
)

func confirmMAC(shared []byte, domain string, binding, exporter []byte) []byte {
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, shared)
	m.Write([]byte(domain))
	m.Write(binding)
	m.Write(exporter)
	return m.Sum(nil)[:confirmTagLen]
}

// clientConfirm and serverConfirm produce the two confirmation tags for a
// session whose handshake produced the given transcript binding.
func clientConfirm(shared []byte, sess *core.Session, binding []byte) []byte {
	return confirmMAC(shared, clientAuthDom, binding, sess.Exporter(exporterLabel, exporterLength))
}

func serverConfirm(shared []byte, sess *core.Session, binding []byte) []byte {
	return confirmMAC(shared, serverAuthDom, binding, sess.Exporter(exporterLabel, exporterLength))
}

func macEqual(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}
