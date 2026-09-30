package udpcarrier

import (
	"bytes"
	"testing"
)

// The carrier must key the raw encapsulation's framing magic FROM the tunnel's
// shared secret: EncapConfig.dialOptions/listenOptions put the secret into
// encap.Options.Key, which encap hashes into a per-deployment, per-direction
// magic. If they passed a constant instead, every tunnel on a host would share
// one magic and two tunnels with different secrets would collide at the socket
// (and a deployment's magic would be a fixed fingerprint). This pins the mapping
// the socket-level separation relies on; that different keys yield different
// magics that drop foreign packets is encap's TestRawFrameKeySeparation and
// TestRawFramingMagicKeyedBySecret.
func TestEncapFramingKeyedBySecret(t *testing.T) {
	a := bytes.Repeat([]byte{0x11}, 32)
	b := bytes.Repeat([]byte{0x22}, 32)
	ec := EncapConfig{Kind: "gre"}

	// The framing key must depend on the shared secret (differs for a vs b)...
	if bytes.Equal(ec.dialOptions(a).Key, ec.dialOptions(b).Key) {
		t.Fatal("dial framing key does not depend on the shared secret (constant Key?)")
	}
	if bytes.Equal(ec.listenOptions(a).Key, ec.listenOptions(b).Key) {
		t.Fatal("listen framing key does not depend on the shared secret (constant Key?)")
	}
	// ...and must BE the secret that encap hashes into the magic, not some other
	// per-call value.
	if !bytes.Equal(ec.dialOptions(a).Key, a) || !bytes.Equal(ec.listenOptions(b).Key, b) {
		t.Fatal("framing key is not the tunnel's shared secret")
	}
}
