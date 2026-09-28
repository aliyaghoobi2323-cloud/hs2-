package core

import (
	"bytes"
	"testing"
)

func doHandshake(t *testing.T, psk []byte) (*Session, *Session) {
	t.Helper()
	srv, err := GenerateStatic()
	if err != nil {
		t.Fatal(err)
	}
	cli, err := GenerateStatic()
	if err != nil {
		t.Fatal(err)
	}
	ini, err := NewInitiator(cli, srv.Public, psk)
	if err != nil {
		t.Fatal(err)
	}
	resp := NewResponder(srv, psk)
	m1, err := ini.WriteMessage1()
	if err != nil {
		t.Fatal(err)
	}
	hs, err := resp.ReadMessage1(m1)
	if err != nil {
		t.Fatalf("responder rejected valid m1: %v", err)
	}
	m2, sSecret, err := resp.WriteMessage2(hs)
	if err != nil {
		t.Fatal(err)
	}
	cSecret, err := ini.ReadMessage2(m2)
	if err != nil {
		t.Fatalf("initiator rejected valid m2: %v", err)
	}
	if !bytes.Equal(sSecret, cSecret) {
		t.Fatalf("secrets differ:\n c=%x\n s=%x", cSecret, sSecret)
	}
	cs, err := NewSession(cSecret, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := NewSession(sSecret, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	return cs, ss
}

func TestHandshakeAndRoundTrip(t *testing.T) {
	psk := bytes.Repeat([]byte{0xA5}, 32)
	cs, ss := doHandshake(t, psk)

	// client -> server, several frames with padding
	msgs := [][]byte{[]byte("hello"), bytes.Repeat([]byte("x"), 1400), {}}
	sr := ss.NewStreamReader()
	var buf bytes.Buffer
	for _, m := range msgs {
		f, err := cs.Seal(TypeData, 0, m, 1500)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(f)
	}
	for _, want := range msgs {
		ft, _, got, err := sr.ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if ft != TypeData {
			t.Fatalf("type=%d", ft)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("payload mismatch: got %d bytes want %d", len(got), len(want))
		}
	}
}

func TestWrongPSKFails(t *testing.T) {
	srv, _ := GenerateStatic()
	cli, _ := GenerateStatic()
	ini, err := NewInitiator(cli, srv.Public, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	resp := NewResponder(srv, bytes.Repeat([]byte{2}, 32))
	m1, _ := ini.WriteMessage1()
	if _, err := resp.ReadMessage1(m1); err == nil {
		t.Fatal("responder accepted a first message under the wrong psk")
	}
}

func TestFirstMessageReplayRejected(t *testing.T) {
	psk := bytes.Repeat([]byte{7}, 32)
	srv, _ := GenerateStatic()
	cli, _ := GenerateStatic()
	ini, _ := NewInitiator(cli, srv.Public, psk)
	resp := NewResponder(srv, psk)
	m1, _ := ini.WriteMessage1()
	if _, err := resp.ReadMessage1(m1); err != nil {
		t.Fatalf("first accept failed: %v", err)
	}
	if _, err := resp.ReadMessage1(m1); err != ErrHandshakeReplay {
		t.Fatalf("replay not rejected, got %v", err)
	}
}

func TestGarbageFirstMessageRejected(t *testing.T) {
	psk := bytes.Repeat([]byte{9}, 32)
	srv, _ := GenerateStatic()
	resp := NewResponder(srv, psk)
	if _, err := resp.ReadMessage1(bytes.Repeat([]byte{0}, 64)); err == nil {
		t.Fatal("responder accepted garbage as a first message")
	}
	if _, err := resp.ReadMessage1([]byte{1, 2, 3}); err == nil {
		t.Fatal("responder accepted a too-short first message")
	}
}

func TestReplayWindowDatagram(t *testing.T) {
	psk := bytes.Repeat([]byte{0x5A}, 32)
	cs, ss := doHandshake(t, psk)

	seq0, f0, _ := cs.SealDatagram(TypeData, 0, []byte("a"), 0)
	seq1, f1, _ := cs.SealDatagram(TypeData, 0, []byte("b"), 0)

	if _, _, _, err := ss.OpenDatagram(seq0, f0[lenPrefix:]); err != nil {
		t.Fatalf("open0: %v", err)
	}
	if _, _, _, err := ss.OpenDatagram(seq1, f1[lenPrefix:]); err != nil {
		t.Fatalf("open1: %v", err)
	}
	// Replay seq0.
	if _, _, _, err := ss.OpenDatagram(seq0, f0[lenPrefix:]); err != errReplayed {
		t.Fatalf("replay not caught, got %v", err)
	}
}

func TestTamperedFrameFails(t *testing.T) {
	psk := bytes.Repeat([]byte{0x33}, 32)
	cs, ss := doHandshake(t, psk)
	f, _ := cs.Seal(TypeData, 0, []byte("secret"), 0)
	f[len(f)-1] ^= 0x80 // flip a tag bit
	sr := ss.NewStreamReader()
	if _, _, _, err := sr.ReadFrame(bytes.NewReader(f)); err == nil {
		t.Fatal("tampered frame decrypted")
	}
}

func TestLengthPrefixIsMasked(t *testing.T) {
	// Two equal-length payloads at different seqs must not produce equal length
	// prefixes on the wire.
	psk := bytes.Repeat([]byte{0x44}, 32)
	cs, _ := doHandshake(t, psk)
	f0, _ := cs.Seal(TypeData, 0, bytes.Repeat([]byte("z"), 100), 0)
	f1, _ := cs.Seal(TypeData, 0, bytes.Repeat([]byte("z"), 100), 0)
	if bytes.Equal(f0[:lenPrefix], f1[:lenPrefix]) {
		t.Fatal("equal-length frames produced identical length prefixes; mask ineffective")
	}
}

// Several clients (or one client reconnecting) must be able to handshake with
// the same responder inside one timestamp bucket; only exact replays fail.
func TestManyHandshakesSameBucket(t *testing.T) {
	psk := bytes.Repeat([]byte{8}, 32)
	srv, _ := GenerateStatic()
	resp := NewResponder(srv, psk)
	topBits := map[byte]bool{}
	for i := 0; i < 32; i++ {
		cli, _ := GenerateStatic()
		ini, _ := NewInitiator(cli, srv.Public, psk)
		m1, err := ini.WriteMessage1()
		if err != nil {
			t.Fatal(err)
		}
		topBits[m1[16+ephemeralLen-1]&0x80] = true
		hs, err := resp.ReadMessage1(m1)
		if err != nil {
			t.Fatalf("handshake %d rejected: %v", i, err)
		}
		m2, sSecret, err := resp.WriteMessage2(hs)
		if err != nil {
			t.Fatal(err)
		}
		cSecret, err := ini.ReadMessage2(m2)
		if err != nil || !bytes.Equal(sSecret, cSecret) {
			t.Fatalf("handshake %d did not complete: %v", i, err)
		}
	}
	if len(topBits) != 2 {
		t.Fatal("ephemeral key top bit is constant on the wire")
	}
}
