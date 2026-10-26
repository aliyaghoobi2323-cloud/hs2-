package tlscarrier

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

// A TUN frame's descriptor is 15 bytes and carries the user tag and the flow
// hash; frames coalesced into one write read back one by one.
func TestTunDescRoundTrip(t *testing.T) {
	pkt := []byte("an IP packet, more or less")
	f := AppendTunFrame(nil, 1, 0xA1B2C3D4, 0x01020304, pkt)
	if len(f) != tunDescLen+len(pkt) || tunDescLen != 15 {
		t.Fatalf("frame of %d bytes for a %d-byte packet", len(f), len(pkt))
	}
	ft, n, tag, flow, err := ParseTunDesc(f[:tunDescLen])
	if err != nil || ft != 1 || n != len(pkt) || tag != 0xA1B2C3D4 || flow != 0x01020304 {
		t.Fatalf("parsed ft=%d n=%d tag=%#x flow=%#x err=%v", ft, n, tag, flow, err)
	}
	if !bytes.Equal(f[tunDescLen:], pkt) {
		t.Fatal("payload changed")
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	big := bytes.Repeat([]byte{7}, 3000)
	go func() {
		batch := AppendTunFrame(nil, 3, 0, 0, make([]byte, 40))
		batch = AppendTunFrame(batch, 1, 7, 9, pkt)
		batch = AppendTunFrame(batch, 1, 8, 10, big)
		NewCarrier(a).WriteRaw(batch)
	}()
	r := NewCarrier(b)
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i, w := range []struct {
		ft        byte
		tag, flow uint32
		p         []byte
	}{{3, 0, 0, make([]byte, 40)}, {1, 7, 9, pkt}, {1, 8, 10, big}} {
		ft, p, tag, flow, err := r.ReadTunFrame()
		if err != nil || ft != w.ft || tag != w.tag || flow != w.flow || !bytes.Equal(p, w.p) {
			t.Fatalf("frame %d: ft=%d tag=%d flow=%d len=%d err=%v", i, ft, tag, flow, len(p), err)
		}
	}
}

// The 7-byte descriptor of wire v2 is no v3 descriptor, and neither is one
// whose lengths no TUN packet has (a stream out of step).
func TestTunDescOldRejected(t *testing.T) {
	old := AppendFrame(nil, 1, []byte("pkt"))[:7]
	if _, _, _, _, err := ParseTunDesc(old); !errors.Is(err, ErrTunDesc) {
		t.Fatalf("7-byte descriptor: err=%v", err)
	}
	h := AppendTunFrame(nil, 1, 0, 0, nil)
	h[1] = 0xFF // a 16 MiB "packet"
	if _, _, _, _, err := ParseTunDesc(h); !errors.Is(err, ErrTunDesc) {
		t.Fatalf("oversized length: err=%v", err)
	}
	h = AppendTunFrame(nil, 1, 0, 0, nil)
	h[4] = 0x10 // a 1 MiB pad
	if _, _, _, _, err := ParseTunDesc(h); !errors.Is(err, ErrTunDesc) {
		t.Fatalf("oversized pad: err=%v", err)
	}
	// On a stream the reader refuses such a descriptor rather than read a
	// packet past it.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	bad := AppendTunFrame(nil, 1, 0, 0, nil)
	bad[1] = 0xFF
	go NewCarrier(a).WriteRaw(bad)
	r := NewCarrier(b)
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, _, _, err := r.ReadTunFrame(); !errors.Is(err, ErrTunDesc) {
		t.Fatalf("a bad descriptor on a stream: err=%v", err)
	}
}

func FuzzParseTunDesc(f *testing.F) {
	f.Add(AppendTunFrame(nil, 1, 1, 2, nil))
	f.Add(AppendTunFrame(nil, 3, 0, 0, make([]byte, 5))[:tunDescLen])
	f.Add(AppendFrame(nil, 1, nil))
	f.Add(make([]byte, tunDescLen))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, h []byte) {
		h = h[:len(h):len(h)] // a read past it panics
		ft, n, tag, flow, err := ParseTunDesc(h)
		if err != nil {
			return
		}
		if len(h) != tunDescLen || n < 0 || n > maxTunFrame {
			t.Fatalf("accepted %d bytes with length %d", len(h), n)
		}
		// What parses is what AppendTunFrame writes (the pad aside).
		re := AppendTunFrame(nil, ft, tag, flow, make([]byte, n))[:tunDescLen]
		if !bytes.Equal(re[:4], h[:4]) || !bytes.Equal(re[7:], h[7:]) {
			t.Fatalf("descriptor %x parsed to ft=%d n=%d tag=%#x flow=%#x", h, ft, n, tag, flow)
		}
	})
}
