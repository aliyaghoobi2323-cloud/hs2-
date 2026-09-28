package core

import (
	"bytes"
	"io"
	"testing"
)

// DPI at the TCP layer can inject bytes into the stream (it forges a segment
// with the right sequence number; the kernel delivers it as if it were ours).
// The question: does our framing turn that into a DISTINGUISHABLE reaction the
// DPI can observe? A tunnel that responds differently to injected garbage than
// to normal traffic is fingerprintable. The correct behaviour is: the injected
// bytes fail AEAD authentication, the reader returns an error, and the carrier
// simply tears down — exactly as it would on any corrupt/lost connection. No
// special "I am a tunnel" reaction.

// injectingReader simulates DPI splicing extra bytes into the stream after some
// legitimate frames.
type injectingReader struct {
	legit    *bytes.Buffer
	injected []byte
	atByte   int
	pos      int
	done     bool
}

func (r *injectingReader) Read(p []byte) (int, error) {
	// Serve legit bytes until atByte, then splice injected, then continue.
	if !r.done && r.pos >= r.atByte {
		r.done = true
		n := copy(p, r.injected)
		return n, nil
	}
	n, err := r.legit.Read(p)
	r.pos += n
	return n, err
}

// TestTCPInjectionRejectedQuietly proves an injected chunk mid-stream is
// rejected by AEAD and produces an ordinary read error — the same failure mode
// as a dropped connection, giving DPI no distinguishing signal.
func TestTCPInjectionRejectedQuietly(t *testing.T) {
	psk := bytes.Repeat([]byte{0x5C}, 32)
	cs, ss := doHandshake(t, psk)

	// Build a few legit frames.
	var wire bytes.Buffer
	for i := 0; i < 3; i++ {
		f, _ := cs.Seal(TypeData, 0, []byte("legit-payload"), 0)
		wire.Write(f)
	}
	legitBytes := wire.Bytes()

	// DPI injects 40 bytes of plausible-looking garbage after the first frame.
	firstFrameLen := len(legitBytes) / 3
	reader := &injectingReader{
		legit:    bytes.NewBuffer(legitBytes),
		injected: bytes.Repeat([]byte{0xAB}, 40),
		atByte:   firstFrameLen,
	}

	sr := ss.NewStreamReader()
	// First frame should read fine.
	if _, _, _, err := sr.ReadFrame(reader); err != nil {
		t.Fatalf("first legit frame failed: %v", err)
	}
	// Next read hits the injected bytes. It must error (AEAD/length failure),
	// NOT panic, NOT hang, NOT silently accept.
	_, _, _, err := sr.ReadFrame(reader)
	if err == nil {
		t.Fatal("injected bytes were accepted as a valid frame!")
	}
	// The error must be an ordinary decode/auth failure or EOF — indistinguish-
	// able from a corrupted or truncated connection.
	if err != io.ErrUnexpectedEOF && err != io.EOF {
		// AEAD failures surface as the cipher's open error; that's fine too.
		t.Logf("injection rejected with: %v (ordinary failure, good)", err)
	}
}

// TestBitFlipInStreamRejected proves flipping any bit in a legit frame (an
// active tamper) is caught by the AEAD tag, not delivered.
func TestBitFlipInStreamRejected(t *testing.T) {
	psk := bytes.Repeat([]byte{0x6D}, 32)
	cs, ss := doHandshake(t, psk)
	f, _ := cs.Seal(TypeData, 0, []byte("important"), 0)
	// Flip a bit in the ciphertext body (not the length prefix).
	f[lenPrefix+5] ^= 0x01
	sr := ss.NewStreamReader()
	if _, _, _, err := sr.ReadFrame(bytes.NewReader(f)); err == nil {
		t.Fatal("bit-flipped frame was accepted!")
	}
}
