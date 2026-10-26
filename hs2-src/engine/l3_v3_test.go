package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// extHdr is one IPv6 extension header of type typ and size bytes (a multiple
// of 8; a fragment header is 8).
type extHdr struct {
	typ  byte
	size int
}

// ipv6Packet builds an IPv6 packet from src to dst: the extension headers
// ext, then a transport header of proto with the ports.
func ipv6Packet(src, dst string, proto byte, sport, dport uint16, ext ...extHdr) []byte {
	p := make([]byte, 40)
	p[0] = 0x60
	copy(p[8:24], netip.MustParseAddr(src).AsSlice())
	copy(p[24:40], netip.MustParseAddr(dst).AsSlice())
	next := 6 // where the next header type goes
	for _, e := range ext {
		p[next] = e.typ
		next = len(p)
		h := make([]byte, e.size)
		if e.typ == 51 { // AH counts 4-byte words, less two
			h[1] = byte(e.size/4 - 2)
		} else if e.typ != 44 {
			h[1] = byte(e.size/8 - 1)
		}
		p = append(p, h...)
	}
	p[next] = proto
	l4 := make([]byte, 20)
	binary.BigEndian.PutUint16(l4, sport)
	binary.BigEndian.PutUint16(l4[2:], dport)
	p = append(p, l4...)
	binary.BigEndian.PutUint16(p[4:], uint16(len(p)-40))
	return p
}

func flowOf(t *testing.T, pkt []byte) uint32 {
	t.Helper()
	f, _, ok := flowKey(pkt, false)
	if !ok {
		t.Fatalf("not an IP packet: %x", pkt[:8])
	}
	return f
}

// Two connections between the same two IPv6 hosts are two flows (they were
// one: the key had only the addresses), also behind extension headers.
func TestFlowKeyIPv6FiveTuple(t *testing.T) {
	const a, b = "2001:db8::1", "2001:db8::2"
	if flowOf(t, ipv6Packet(a, b, 6, 40000, 443)) == flowOf(t, ipv6Packet(a, b, 6, 40001, 443)) {
		t.Fatal("two TCP connections share a flow")
	}
	if flowOf(t, ipv6Packet(a, b, 6, 40000, 443)) != flowOf(t, ipv6Packet(a, b, 6, 40000, 443)) {
		t.Fatal("one connection is two flows")
	}
	if flowOf(t, ipv6Packet(a, b, 6, 40000, 443)) == flowOf(t, ipv6Packet(a, b, 17, 40000, 443)) {
		t.Fatal("TCP and UDP on the same ports share a flow")
	}
	eight := make([]extHdr, 8)
	for i := range eight {
		eight[i] = extHdr{60, 8}
	}
	eight[0] = extHdr{0, 16} // hop-by-hop first, as it must be
	eight[3] = extHdr{51, 16}
	if flowOf(t, ipv6Packet(a, b, 6, 40000, 443, eight...)) == flowOf(t, ipv6Packet(a, b, 6, 40001, 443, eight...)) {
		t.Fatal("behind 8 extension headers two connections share a flow")
	}
	if flowOf(t, ipv6Packet(a, b, 6, 40000, 443, eight...)) != flowOf(t, ipv6Packet(a, b, 6, 40000, 443)) {
		t.Fatal("extension headers changed the flow")
	}
}

// The walk is bounded: past 8 extension headers or 256 bytes of them, and
// for a fragment, the key is the 3-tuple — every port the same flow.
func TestFlowKeyIPv6ExtBound(t *testing.T) {
	const a, b = "2001:db8::1", "2001:db8::2"
	nine := make([]extHdr, 9)
	for i := range nine {
		nine[i] = extHdr{60, 8}
	}
	for name, ext := range map[string][]extHdr{
		"9 headers":    nine,
		"264 bytes":    {{60, 264}},
		"128+136":      {{0, 128}, {60, 136}},
		"a fragment":   {{44, 8}},
		"cut off":      {{60, 16}}, // the packet ends inside it (below)
		"AH, too long": {{51, 260}},
	} {
		p1, p2 := ipv6Packet(a, b, 6, 40000, 443, ext...), ipv6Packet(a, b, 6, 50000, 80, ext...)
		p3 := ipv6Packet("2001:db8::3", b, 6, 40000, 443, ext...)
		if name == "cut off" {
			p1, p2, p3 = p1[:50], p2[:50], p3[:50]
		}
		if flowOf(t, p1) != flowOf(t, p2) {
			t.Errorf("%s: ports still split the flow", name)
		}
		if flowOf(t, p1) == flowOf(t, p3) {
			t.Errorf("%s: another host in the same flow", name)
		}
	}
	// 256 bytes is still walked.
	if flowOf(t, ipv6Packet(a, b, 6, 40000, 443, extHdr{60, 256})) == flowOf(t, ipv6Packet(a, b, 6, 50000, 80, extHdr{60, 256})) {
		t.Error("256 bytes of extension headers: not walked")
	}
}

// The user's inner address is the source of a packet from the user, the
// destination of one to the user; the flow does not depend on which.
func TestFlowKeyUserSide(t *testing.T) {
	for _, c := range []struct {
		pkt      []byte
		src, dst string
	}{
		{tcpPacket(40000, 443), "10.77.0.1", "10.77.0.2"},
		{ipv6Packet("2001:db8::1", "2001:db8::2", 17, 5000, 53), "2001:db8::1", "2001:db8::2"},
	} {
		fromUser, ip1, ok1 := flowKey(c.pkt, false)
		toUser, ip2, ok2 := flowKey(c.pkt, true)
		if !ok1 || !ok2 || ip1 != netip.MustParseAddr(c.src) || ip2 != netip.MustParseAddr(c.dst) {
			t.Fatalf("user side: from the user %v (%v), to the user %v (%v); want %s, %s", ip1, ok1, ip2, ok2, c.src, c.dst)
		}
		if fromUser != toUser || fromUser != flowHash(c.pkt) {
			t.Fatalf("flow %x / %x / flowHash %x", fromUser, toUser, flowHash(c.pkt))
		}
	}
	if _, ip, ok := flowKey([]byte{0x45, 0, 0}, false); ok || ip.IsValid() {
		t.Fatal("a 3-byte packet has a user")
	}
}

// What the TUN reader of each side sends: the user tag of the user-side
// address and the packet's flow, in the 15-byte descriptor.
func TestTunFrameCarriesTagAndFlow(t *testing.T) {
	for _, toUser := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		a, b := net.Pipe()
		s := &l3Set{toUser: toUser}
		l := newL3Link(tlscarrier.NewCarrier(a))
		s.add(l)
		var pool sync.Pool
		var drops atomic.Uint64
		go l.writeLoop(&pool, &drops)
		go s.pumpTun(ctx, &fakeTun{n: 1, ctx: ctx})
		peer := tlscarrier.NewCarrier(b)
		peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		ft, p, tag, flow, err := peer.ReadTunFrame()
		user := "10.77.0.1" // tcpPacket's source
		if toUser {
			user = "10.77.0.2"
		}
		if err != nil || ft != core.TypeData || tag != UserTag(netip.MustParseAddr(user), 64) || flow != flowHash(p) || flow != flowHash(tcpPacket(1, 2)) {
			t.Errorf("toUser=%v: ft=%d tag=%#x flow=%#x err=%v; want the tag of %s", toUser, ft, tag, flow, err, user)
		}
		cancel()
		l.markDead()
		b.Close()
	}
}

func FuzzFlowKey(f *testing.F) {
	f.Add(tcpPacket(40000, 443))
	f.Add(ipv6Packet("2001:db8::1", "2001:db8::2", 6, 1, 2))
	f.Add(ipv6Packet("2001:db8::1", "2001:db8::2", 6, 1, 2, extHdr{0, 8}, extHdr{43, 24}, extHdr{51, 12}, extHdr{60, 8}))
	f.Add(ipv6Packet("2001:db8::1", "2001:db8::2", 17, 1, 2, extHdr{44, 8}))
	f.Add([]byte{0x60})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, pkt []byte) {
		pkt = pkt[:len(pkt):len(pkt)] // a read past it panics
		flow, ip, ok := flowKey(pkt, false)
		flow2, ip2, ok2 := flowKey(pkt, true)
		if flow != flow2 || ok != ok2 || ok != ip.IsValid() || ok != ip2.IsValid() || flow != flowHash(pkt) {
			t.Fatalf("flow %x/%x ok %v/%v ips %v/%v", flow, flow2, ok, ok2, ip, ip2)
		}
		if ok && (ip.Is4() != (pkt[0]>>4 == 4) || ip.Is6() != (pkt[0]>>4 == 6)) {
			t.Fatalf("version %d gave address %v", pkt[0]>>4, ip)
		}
	})
}

// A frame without the v3 descriptor ends the side channel it came on (the
// stream is out of step: nothing after it can be read), counted and logged
// at most once a minute.
func TestBadTunFrameDropped(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, before := WireCounters()
	go logWireEvery(ctx, func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // it has its baseline
	for i := 0; i < 2; i++ {
		a, b := net.Pipe()
		s := &l3Set{}
		l := newL3Link(tlscarrier.NewCarrier(a))
		done := make(chan struct{})
		go func() { s.linkToTun(ctx, l, &fakeTun{ctx: ctx}); close(done) }()
		bad := tlscarrier.AppendTunFrame(nil, core.TypeData, 0, 0, nil)
		bad[1] = 0xFF // a 16 MiB packet: no v3 descriptor
		tlscarrier.NewCarrier(b).WriteRaw(bad)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("still reading after a bad descriptor")
		}
		if l.Alive() {
			t.Fatal("the side channel lives on out of step")
		}
		b.Close()
	}
	if _, n := WireCounters(); n != before+2 {
		t.Fatalf("bad tun frames counted %d, want 2", n-before)
	}
	time.Sleep(100 * time.Millisecond) // several ticks
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "tun frame with v2 descriptor" {
		t.Fatalf("logged %q, want the line once", lines)
	}
}
