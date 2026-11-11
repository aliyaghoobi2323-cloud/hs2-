package tls

import (
	"bytes"
	"testing"
)

// [hs2] the SNI ack in EncryptedExtensions: OpenSSL's bytes, round trip, and
// nothing new when the field is off.
func TestHS2EncryptedExtensionsServerNameAck(t *testing.T) {
	m := &encryptedExtensionsMsg{serverNameAck: true, alpnProtocol: "h2"}
	b, err := m.marshal()
	if err != nil {
		t.Fatal(err)
	}
	// type 8, len 15; ext list 13: server_name (0,0) then ALPN h2 (19 B; a
	// 36 B record, as OpenSSL's)
	want := []byte{8, 0, 0, 15, 0, 13, 0, 0, 0, 0, 0, 16, 0, 5, 0, 3, 2, 'h', '2'}
	if !bytes.Equal(b, want) {
		t.Fatalf("got % x\nwant % x", b, want)
	}
	var u encryptedExtensionsMsg
	if !u.unmarshal(b) || !u.serverNameAck || u.alpnProtocol != "h2" {
		t.Fatalf("unmarshal: %+v", u)
	}
	m.serverNameAck = false
	b, _ = m.marshal()
	if len(b) != 15 {
		t.Fatalf("without the ack: %d B, want upstream's 15", len(b))
	}
	// a non-empty server_name in EE is ignored, as upstream does
	odd := []byte{8, 0, 0, 7, 0, 5, 0, 0, 0, 1, 0}
	if !u.unmarshal(odd) || u.serverNameAck {
		t.Fatal("a non-empty server_name in EE must be ignored")
	}
}
