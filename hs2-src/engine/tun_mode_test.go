package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// fakeTUN stands in for a real TUN device (which needs root + /dev/net/tun).
// Read returns packets injected with inject(); Write collects packets the
// tunnel delivered, exposed via recv().
type fakeTUN struct {
	mtu    int
	in     chan []byte
	out    chan []byte
	closed chan struct{}
}

func newFakeTUN(mtu int) *fakeTUN {
	return &fakeTUN{mtu: mtu, in: make(chan []byte, 256), out: make(chan []byte, 256), closed: make(chan struct{})}
}
func (f *fakeTUN) MTU() int { return f.mtu }
func (f *fakeTUN) Read(p []byte) (int, error) {
	select {
	case pkt := <-f.in:
		return copy(p, pkt), nil
	case <-f.closed:
		return 0, io.EOF
	}
}
func (f *fakeTUN) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	select {
	case f.out <- b:
	case <-f.closed:
	}
	return len(p), nil
}
func (f *fakeTUN) inject(pkt []byte) { f.in <- pkt }
func (f *fakeTUN) recv(d time.Duration) []byte {
	select {
	case b := <-f.out:
		return b
	case <-time.After(d):
		return nil
	}
}
func (f *fakeTUN) close() { close(f.closed) }

// ipPacket builds a minimal well-formed IPv4/UDP packet so flowHash keys it to a
// link and the payload survives the round trip for an exact-bytes check.
func ipPacket(srcPort, dstPort uint16, payload []byte) []byte {
	hdr := make([]byte, 28) // 20 IP + 8 UDP
	hdr[0] = 0x45           // v4, IHL=5
	total := len(hdr) + len(payload)
	binary.BigEndian.PutUint16(hdr[2:], uint16(total))
	hdr[9] = 17 // UDP
	copy(hdr[12:16], []byte{10, 0, 0, 1})
	copy(hdr[16:20], []byte{10, 0, 0, 2})
	binary.BigEndian.PutUint16(hdr[20:], srcPort)
	binary.BigEndian.PutUint16(hdr[22:], dstPort)
	binary.BigEndian.PutUint16(hdr[24:], uint16(8+len(payload)))
	return append(hdr, payload...)
}

// waitTunUp injects a probe packet from a->b until one is delivered, so the test
// only asserts once at least one l3 link is carrying traffic.
func waitTunUp(t *testing.T, a, b *fakeTUN) {
	t.Helper()
	for i := 0; i < 200; i++ {
		a.inject(ipPacket(1111, 2222, []byte("probe")))
		if b.recv(50*time.Millisecond) != nil {
			return
		}
	}
	t.Fatal("tun tunnel never carried a packet")
}

// drain empties any queued probe packets so the exact-match phase starts clean.
func drain(f *fakeTUN) {
	for {
		select {
		case <-f.out:
		default:
			return
		}
	}
}

// exerciseTun sends many distinct packets a->b and checks each that arrives is
// byte-identical to what was sent (payload keyed by its index marker). The L3
// path is deliberately lossy under congestion, so a few drops are tolerated, but
// every delivered packet must be intact.
func exerciseTun(t *testing.T, a, b *fakeTUN, label string) {
	t.Helper()
	drain(b)
	const n = 60
	sent := make(map[byte][]byte, n)
	for i := 0; i < n; i++ {
		payload := append([]byte(label+"-"), byte(i))
		payload = append(payload, bytes.Repeat([]byte{byte(i)}, 200+i)...)
		sent[byte(i)] = payload
		a.inject(ipPacket(uint16(4000+i), 5555, payload))
	}
	got := 0
	deadline := time.Now().Add(5 * time.Second)
	for got < n && time.Now().Before(deadline) {
		pkt := b.recv(200 * time.Millisecond)
		if pkt == nil || len(pkt) <= 28 {
			continue
		}
		payload := pkt[28:] // strip the 20+8 IP/UDP header
		idx := payload[len(label)+1]
		want, ok := sent[idx]
		if !ok {
			continue
		}
		if !bytes.Equal(payload, want) {
			t.Fatalf("%s: packet %d corrupted (got %d bytes, want %d)", label, idx, len(payload), len(want))
		}
		got++
	}
	if got < n-5 { // tolerate a few congestion drops, never corruption
		t.Fatalf("%s: only %d/%d packets arrived intact", label, got, n)
	}
}

func startTunTunnel(t *testing.T, min, max int) (*fakeTUN, *fakeTUN, func()) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	itun := newFakeTUN(1320)
	ktun := newFakeTUN(1320)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, TUN: ktun})
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    min, Max: max, PerLink: 50,
		TUN: itun,
	})
	cleanup := func() { cancel(); raw.Close(); itun.close(); ktun.close() }
	return itun, ktun, cleanup
}

// mtcp behind a TUN (l3mtcp), direct direction: a full L3 pipe over the shaped,
// authenticated multi-link carrier — the "tun mode" the installer exposes.
func TestTunModeDirect(t *testing.T) {
	itun, ktun, cleanup := startTunTunnel(t, 3, 3)
	defer cleanup()
	waitTunUp(t, itun, ktun) // iran -> kharej
	waitTunUp(t, ktun, itun) // kharej -> iran
	exerciseTun(t, itun, ktun, "i2k")
	exerciseTun(t, ktun, itun, "k2i")
}

func startTunReverse(t *testing.T, nLinks int) (*fakeTUN, *fakeTUN, func()) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	itun := newFakeTUN(1320)
	ktun := newFakeTUN(1320)

	iranLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	iranSrv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunIran(ctx, IranConfig{RevServer: iranSrv, RevListener: iranLn, PerLink: 50, TUN: itun})

	addr := iranLn.Addr().String()
	go RunKharej(ctx, KharejConfig{
		TUN:      ktun,
		RevLinks: nLinks,
		RevDial:  func() (*tlscarrier.Carrier, error) { return tlscarrier.DialFrom(addr, "lab.example.com", key, "") },
	})
	cleanup := func() { cancel(); iranLn.Close(); itun.close(); ktun.close() }
	return itun, ktun, cleanup
}

// Same L3 pipe but REVERSE: the kharej dials in to the iran edge.
func TestTunModeReverse(t *testing.T) {
	itun, ktun, cleanup := startTunReverse(t, 3)
	defer cleanup()
	waitTunUp(t, itun, ktun)
	waitTunUp(t, ktun, itun)
	exerciseTun(t, itun, ktun, "i2k")
	exerciseTun(t, ktun, itun, "k2i")
}

// The mode the installer builds from "tcp" transport + TLS mode "l3mtcp" in
// REVERSE: user TCP ports carried on the multi-link stream path (no TCP inside
// TCP), and at the same time the hs0 L3 interface for everything else (ping,
// UDP, other ports routed via 10.77.0.x). Both must work over the same links.
func TestTunModeReverseWithUserPorts(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	itun, ktun := newFakeTUN(1380), newFakeTUN(1380)
	defer itun.close()
	defer ktun.close()
	panel := echoPanel(t)
	port := freePort(t)

	iranLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer iranLn.Close()
	iranSrv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunIran(ctx, IranConfig{RevServer: iranSrv, RevListener: iranLn, Min: 2, Max: 8, PerLink: 8,
		ListenIP: "127.0.0.1", Ports: []string{port}, TUN: itun})
	addr := iranLn.Addr().String()
	go RunKharej(ctx, KharejConfig{
		Panel: panel, TUN: ktun,
		RevLinks: 4, RevMin: 2, RevMax: 8,
		RevDial: func() (*tlscarrier.Carrier, error) { return tlscarrier.DialFrom(addr, "lab.example.com", key, "") },
	})

	waitTunUp(t, itun, ktun)
	waitTunUp(t, ktun, itun)
	for i := 0; i < 50 && !echoOnce("127.0.0.1:"+port, 1024); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	tcpEcho(t, "127.0.0.1:"+port, 256<<10) // a user connection on the stream path
	exerciseTun(t, itun, ktun, "i2k")      // and the L3 pipe, both ways
	exerciseTun(t, ktun, itun, "k2i")
	tcpEcho(t, "127.0.0.1:"+port, 64<<10)
}
