package engine

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// tcpPacket builds a minimal IPv4/TCP header for the given ports.
func tcpPacket(sport, dport uint16) []byte {
	p := make([]byte, 40)
	p[0] = 0x45
	p[9] = 6
	copy(p[12:16], []byte{10, 77, 0, 1})
	copy(p[16:20], []byte{10, 77, 0, 2})
	binary.BigEndian.PutUint16(p[20:], sport)
	binary.BigEndian.PutUint16(p[22:], dport)
	return p
}

func TestFlowHashMatchesFNV(t *testing.T) {
	pkt := tcpPacket(40000, 9999)
	h := fnv.New32a()
	h.Write(pkt[12:20])
	h.Write([]byte{6})
	h.Write(pkt[20:24])
	if got, want := flowHash(pkt), h.Sum32(); got != want {
		t.Fatalf("flowHash = %x, want %x", got, want)
	}
}

// Killing one link must move only the flows that were on it.
func TestPickMovesOnlyDeadLinksFlows(t *testing.T) {
	var s l3Set
	links := make([]*l3Link, 8)
	for i := range links {
		links[i] = &l3Link{id: uint32(i+1) * 0x9e3779b9}
		s.add(links[i])
	}
	const flows = 20000
	before := make([]*l3Link, flows)
	perLink := map[*l3Link]int{}
	for i := range before {
		before[i] = s.pick(tcpPacket(uint16(i), 9999))
		perLink[before[i]]++
	}
	for _, l := range links {
		if n := perLink[l]; n < flows/8/2 || n > flows/8*2 {
			t.Fatalf("unbalanced: link has %d of %d flows", n, flows)
		}
	}
	victim := links[3]
	victim.dead.Store(true)
	for i := range before {
		after := s.pick(tcpPacket(uint16(i), 9999))
		if after == victim {
			t.Fatal("picked a dead link")
		}
		if before[i] != victim && after != before[i] {
			t.Fatalf("flow %d moved off a live link", i)
		}
	}
}

func TestPickAllDead(t *testing.T) {
	var s l3Set
	l := &l3Link{id: 1}
	l.dead.Store(true)
	s.add(l)
	if s.pick(tcpPacket(1, 2)) != nil {
		t.Fatal("expected no link")
	}
}

// fakeTun serves n packets as fast as they are read, then blocks.
type fakeTun struct {
	n    int
	mu   sync.Mutex
	read int
	ctx  context.Context
}

func (f *fakeTun) MTU() int                    { return 1380 }
func (f *fakeTun) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeTun) Read(p []byte) (int, error) {
	f.mu.Lock()
	if f.read < f.n {
		f.read++
		f.mu.Unlock()
		return copy(p, tcpPacket(1, 2)), nil
	}
	f.mu.Unlock()
	<-f.ctx.Done()
	return 0, f.ctx.Err()
}

func (f *fakeTun) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.read
}

// A link whose writer is stuck must not stop the TUN reader: excess packets
// are dropped instead.
func TestPumpNeverBlocksOnStuckLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var s l3Set
	s.add(&l3Link{id: 1, q: make(chan qpkt, l3QueueLen)}) // no writer
	dev := &fakeTun{n: 5000, ctx: ctx}
	go s.pumpTun(ctx, dev)
	deadline := time.Now().Add(2 * time.Second)
	for dev.reads() < dev.n {
		if time.Now().After(deadline) {
			t.Fatalf("TUN reader stalled after %d packets", dev.reads())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := s.drops.Load(), uint64(dev.n-l3QueueLen); got != want {
		t.Fatalf("drops = %d, want %d", got, want)
	}
}

// Queued packets arrive in order; an idle link sends keepalives.
func TestWriteLoopDeliversAndKeepsAlive(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := newL3Link(tlscarrier.NewCarrier(a))
	l.ka = 50 * time.Millisecond
	var pool sync.Pool
	for i := 0; i < 10; i++ {
		p := tcpPacket(uint16(i), 1)
		if !l.enqueue(&p) {
			t.Fatal("enqueue failed")
		}
	}
	var drops atomic.Uint64
	go l.writeLoop(&pool, &drops)
	defer l.markDead()

	peer := tlscarrier.NewCarrier(b)
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 10; i++ {
		ft, p, err := peer.ReadFrame()
		if err != nil || ft != core.TypeData {
			t.Fatalf("frame %d: ft=%d err=%v", i, ft, err)
		}
		if got := binary.BigEndian.Uint16(p[20:]); got != uint16(i) {
			t.Fatalf("frame %d out of order: sport %d", i, got)
		}
	}
	ft, _, err := peer.ReadFrame()
	if err != nil || ft != core.TypePing {
		t.Fatalf("expected keepalive, got ft=%d err=%v", ft, err)
	}
}

// Packets that waited longer than l3MaxSojourn are dropped, not sent late.
func TestWriteLoopDropsStalePackets(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	l := newL3Link(tlscarrier.NewCarrier(a))
	stale := tcpPacket(1, 1)
	fresh := tcpPacket(2, 1)
	l.q <- qpkt{&stale, time.Now().Add(-time.Second)}
	l.q <- qpkt{&fresh, time.Now()}
	var pool sync.Pool
	var drops atomic.Uint64
	go l.writeLoop(&pool, &drops)
	defer l.markDead()
	peer := tlscarrier.NewCarrier(b)
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	ft, p, err := peer.ReadFrame()
	if err != nil || ft != core.TypeData || binary.BigEndian.Uint16(p[20:]) != 2 {
		t.Fatalf("expected only the fresh packet, got ft=%d err=%v", ft, err)
	}
	if drops.Load() != 1 {
		t.Fatalf("drops = %d, want 1", drops.Load())
	}
}

// In stream mode the TUN side channel sends quiet keepalives only once the
// other server said (kindInfo capL3Quiet) that its reader allows them; its own
// reader always allows 30 s.
func TestStreamL3QuietKeepaliveNeedsPeerCap(t *testing.T) {
	m := &linkMeter{}
	l := newStreamL3Link(nil, peerL3Quiet(m))
	if l.deadAfter != l3StreamDeadAfter {
		t.Fatalf("deadAfter=%s, want %s", l.deadAfter, l3StreamDeadAfter)
	}
	if ka := l.keepalive(); ka != l3KeepaliveEvery {
		t.Fatalf("keepalive before the peer's info = %s, want %s", ka, l3KeepaliveEvery)
	}
	m.peerInfo.Store(&peerInfo{V2: true, Caps: capPortTags}) // an older new-format peer
	if ka := l.keepalive(); ka != l3KeepaliveEvery {
		t.Fatalf("keepalive to a peer without capL3Quiet = %s", ka)
	}
	m.peerInfo.Store(&peerInfo{V2: true, Caps: capPortTags | capL3Quiet})
	for i := 0; i < 50; i++ {
		if ka := l.keepalive(); ka < 8*time.Second || ka > 12*time.Second {
			t.Fatalf("quiet keepalive %s outside 10s ±20%%", ka)
		}
	}
	// The non-stream (pool L3) link keeps its fixed values.
	p := newL3Link(nil)
	if p.keepalive() != l3KeepaliveEvery || p.deadAfter != 0 {
		t.Fatal("pool L3 link changed")
	}
}
