package core

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestPayloadsBindingAndExporter checks the datagram additions end to end:
// message payloads arrive intact, both ends see the same transcript binding
// and exporter, and the responder learns the initiator's static key.
func TestPayloadsBindingAndExporter(t *testing.T) {
	psk := bytes.Repeat([]byte{7}, 32)
	srv, _ := GenerateStatic()
	cli, _ := GenerateStatic()
	ini, err := NewInitiator(cli, srv.Public, psk)
	if err != nil {
		t.Fatal(err)
	}
	resp := NewResponder(srv, psk)
	m1, err := ini.WriteMessage1Payload([]byte("cid-client"))
	if err != nil {
		t.Fatal(err)
	}
	hs, p1, err := resp.ReadMessage1Payload(m1)
	if err != nil || string(p1) != "cid-client" {
		t.Fatalf("m1 payload: %q %v", p1, err)
	}
	if !bytes.Equal(PeerStatic(hs), cli.Public) {
		t.Fatal("responder did not learn the initiator static key")
	}
	m2, sSec, err := resp.WriteMessage2Payload(hs, []byte("cid-server"))
	if err != nil {
		t.Fatal(err)
	}
	cSec, p2, err := ini.ReadMessage2Payload(m2)
	if err != nil || string(p2) != "cid-server" {
		t.Fatalf("m2 payload: %q %v", p2, err)
	}
	if len(ini.Binding()) != 32 || !bytes.Equal(ini.Binding(), HandshakeBinding(hs)) {
		t.Fatal("transcript bindings differ")
	}
	cs, _ := NewSession(cSec, true, 1)
	ss, _ := NewSession(sSec, false, 1)
	if !bytes.Equal(cs.Exporter("x", 32), ss.Exporter("x", 32)) {
		t.Fatal("exporters differ across ends of one handshake")
	}
	if bytes.Equal(cs.Exporter("x", 32), cs.Exporter("y", 32)) {
		t.Fatal("exporter ignores its label")
	}

	// A second, independent handshake (what a man-in-the-middle would hold
	// with one side) must give an unrelated exporter.
	cs2, _ := doHandshake(t, psk)
	if bytes.Equal(cs.Exporter("x", 32), cs2.Exporter("x", 32)) {
		t.Fatal("two handshakes produced the same exporter")
	}
}

func TestStaticFromSeedDeterministic(t *testing.T) {
	seed := bytes.Repeat([]byte{3}, 32)
	a, err := StaticFromSeed(seed, "responder")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := StaticFromSeed(seed, "responder")
	c, _ := StaticFromSeed(seed, "initiator")
	d, _ := StaticFromSeed(bytes.Repeat([]byte{4}, 32), "responder")
	if !bytes.Equal(a.Public, b.Public) || !bytes.Equal(a.Private, b.Private) {
		t.Fatal("not deterministic")
	}
	if bytes.Equal(a.Public, c.Public) || bytes.Equal(a.Public, d.Public) {
		t.Fatal("label or seed ignored")
	}
	// The derived keys must work in a real handshake.
	ini, _ := NewInitiator(c, a.Public, seed)
	m1, _ := ini.WriteMessage1()
	resp := NewResponder(a, seed)
	hs, err := resp.ReadMessage1(m1)
	if err != nil {
		t.Fatal(err)
	}
	m2, _, _ := resp.WriteMessage2(hs)
	if _, err := ini.ReadMessage2(m2); err != nil {
		t.Fatal(err)
	}
}

// TestReplayWindowReorderAndJump covers what the datagram path needs beyond
// the basic case: heavy reordering inside the window, a jump larger than the
// window, and a slide that must not resurrect old marks.
func TestReplayWindowReorderAndJump(t *testing.T) {
	var w replayWindow
	for s := uint64(100); s < 200; s += 2 {
		if !w.check(s) {
			t.Fatalf("fresh %d refused", s)
		}
	}
	for s := uint64(101); s < 200; s += 2 { // late odd ones
		if !w.check(s) {
			t.Fatalf("late %d refused", s)
		}
	}
	for s := uint64(100); s < 200; s++ {
		if w.check(s) {
			t.Fatalf("replay %d accepted", s)
		}
	}
	if !w.check(200 + replayBits + 5) {
		t.Fatal("jump refused")
	}
	if w.check(199) {
		t.Fatal("seq below the new floor accepted")
	}
	// Positions reused modulo the window must read as unseen after a slide.
	top := uint64(200 + replayBits + 5)
	if !w.check(top - 1) {
		t.Fatal("unseen seq inside window refused after jump")
	}
	for s := top + 1; s < top+3*replayBits; s++ {
		if !w.check(s) {
			t.Fatalf("sequential %d refused", s)
		}
	}
}

func BenchmarkReplayWindowSequential(b *testing.B) {
	var w replayWindow
	for i := 0; i < b.N; i++ {
		w.check(uint64(i))
	}
}

// AppendDatagram is SealDatagram's frame without the masked length, behind
// the sequence: byte for byte, for every padding, and appended after what
// dst already holds — so the wire is the same as before it existed.
func TestAppendDatagramMatchesSealDatagram(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	a, err := NewSession(secret, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSession(secret, true, 1)
	srv, _ := NewSession(secret, false, 1)
	for i, c := range []struct{ n, pad int }{{0, 0}, {1, 0}, {100, 0}, {100, 400}, {1300, 1200}, {1400, 1400}} {
		payload := bytes.Repeat([]byte{byte(i + 1)}, c.n)
		seq, frame, err := a.SealDatagram(TypeData, 3, payload, c.pad)
		if err != nil {
			t.Fatal(err)
		}
		want := binary.BigEndian.AppendUint64(nil, seq)
		want = append(want, frame[lenPrefix:]...)
		prefix := []byte{0xaa, 0xbb}
		dst := append(make([]byte, 0, 4), prefix...) // too small: must grow and keep the prefix
		got, err := b.AppendDatagram(dst, TypeData, 3, payload, c.pad)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:2], prefix) || !bytes.Equal(got[2:], want) {
			t.Fatalf("case %d: AppendDatagram differs from SealDatagram's frame", i)
		}
		if len(got)-2 != DatagramLen(c.n, c.pad) {
			t.Fatalf("case %d: DatagramLen %d, appended %d", i, DatagramLen(c.n, c.pad), len(got)-2)
		}
		big := make([]byte, 0, 2048) // room enough: written in place, no new buffer
		got2, _ := a.AppendDatagram(big, TypeData, 3, payload, c.pad)
		if &got2[0] != &big[:1][0] {
			t.Fatalf("case %d: a buffer with room was not used in place", i)
		}
		seq2 := binary.BigEndian.Uint64(got2[:8])
		ft, fl, p, err := srv.OpenDatagram(seq2, got2[8:])
		if err != nil || ft != TypeData || fl != 3 || !bytes.Equal(p, payload) {
			t.Fatalf("case %d: the peer cannot open it: %v", i, err)
		}
		b.AppendDatagram(nil, TypeData, 0, nil, 0) // keep b's sequence with a's
	}
	if _, err := a.AppendDatagram(nil, TypeData, 0, make([]byte, maxPayload+1), 0); err == nil {
		t.Fatal("an oversized payload was sealed")
	}
}

func BenchmarkSealDatagram(b *testing.B) {
	s, _ := NewSession(bytes.Repeat([]byte{7}, 32), true, 1)
	payload := make([]byte, 1300)
	var scr []byte
	b.Run("SealDatagram+pack", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for i := 0; i < b.N; i++ {
			seq, frame, _ := s.SealDatagram(TypeData, 0, payload, 0)
			scr = binary.BigEndian.AppendUint64(scr[:0], seq)
			scr = append(scr, frame[lenPrefix:]...)
		}
	})
	b.Run("AppendDatagram", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for i := 0; i < b.N; i++ {
			scr, _ = s.AppendDatagram(scr[:0], TypeData, 0, payload, 0)
		}
	})
}
