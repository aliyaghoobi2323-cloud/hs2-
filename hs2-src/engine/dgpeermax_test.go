package engine

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// The other server's link-pool ceiling on the datagram pool (display only). It
// rides the two existing control frames as two trailing bytes: the exit's on
// TypeLinkStats, the edge's on TypePoolCtl. These tests pin the wire layout,
// compatibility with the older 2- and 4-byte payloads (and that the sizing
// fields keep their meaning), staleness, and the end-to-end view on BOTH
// servers in direct and reverse.

func u16At(b []byte, off int) int { return int(binary.BigEndian.Uint16(b[off:])) }

// The edge reads the exit's ceiling from a 6-byte TypeLinkStats; an older
// exit's 4-byte frame still feeds sizing and leaves the ceiling unknown; a
// report older than dgPeerMaxStale reads as unknown again.
func TestDgPeerMaxFromLinkStats(t *testing.T) {
	p, _, clk := edgeWithLinks(t, 2)

	p.onLinkStats(statsFrame(1, 2)) // older exit: 4 bytes
	if p.dnServing.Load() != 2 || p.dnPressed.Load() != 1 {
		t.Fatalf("sizing fields lost on a 4-byte frame: pressed=%d serving=%d", p.dnPressed.Load(), p.dnServing.Load())
	}
	if got := p.peerMaxNow(); got != 0 {
		t.Fatalf("older exit: peer max %d, want 0 (unknown)", got)
	}

	f := append(statsFrame(1, 2), 0, 48) // current exit: + ceiling 48
	p.onLinkStats(f)
	if p.dnServing.Load() != 2 || p.dnPressed.Load() != 1 {
		t.Fatal("sizing fields changed meaning on a 6-byte frame")
	}
	if got := p.peerMaxNow(); got != 48 {
		t.Fatalf("peer max %d, want 48", got)
	}
	if got := p.Stats().PeerMax; got != 48 {
		t.Fatalf("Stats().PeerMax %d, want 48", got)
	}

	*clk = clk.Add(dgPeerMaxStale + time.Second)
	if got := p.peerMaxNow(); got != 0 {
		t.Fatalf("stale report: peer max %d, want 0", got)
	}
}

// An exit takes the edge's ceiling from TypePoolCtl's trailing bytes. A
// DIRECT exit (no dialer) still ignores the target, as it always did; a
// REVERSE exit still applies the target, clamped to its own [min,max]; an
// older edge's 2-byte frame works as before and leaves the ceiling unknown.
func TestDgPeerMaxFromPoolCtl(t *testing.T) {
	newExit := func(withDialer bool) *dgPool {
		p := newDgPool(newFakeTUN(1400), 2, 6, 8, func(string, ...any) {})
		p.downSender = true
		if withDialer {
			p.dialer, _ = newDgFakeLink()
		}
		p.target.Store(3)
		return p
	}
	ctl := func(target, max uint16) []byte {
		var b [4]byte
		binary.BigEndian.PutUint16(b[0:], target)
		binary.BigEndian.PutUint16(b[2:], max)
		return b[:]
	}

	direct := newExit(false)
	direct.onPoolCtl(nil, ctl(5, 64))
	if got := direct.peerMaxNow(); got != 64 {
		t.Fatalf("direct exit: peer max %d, want 64", got)
	}
	if got := direct.Target(); got != 3 {
		t.Fatalf("direct exit applied the target (%d); it must ignore it", got)
	}

	rev := newExit(true)
	rev.onPoolCtl(nil, ctl(50, 64)) // above the exit's own max 6
	if got := rev.Target(); got != 6 {
		t.Fatalf("reverse exit target %d, want 6 (clamped to its own max)", got)
	}
	if got := rev.peerMaxNow(); got != 64 {
		t.Fatalf("reverse exit: peer max %d, want 64", got)
	}

	old := newExit(true)
	var two [2]byte
	binary.BigEndian.PutUint16(two[:], 4)
	old.onPoolCtl(nil, two[:]) // an older edge: target only
	if got := old.Target(); got != 4 {
		t.Fatalf("older edge's 2-byte frame: target %d, want 4", got)
	}
	if got := old.peerMaxNow(); got != 0 {
		t.Fatalf("older edge: peer max %d, want 0", got)
	}

	// The edge never takes a ceiling from a pool-control frame (it never gets
	// one; guard anyway).
	edge := newDgPool(newFakeTUN(1400), 2, 6, 8, func(string, ...any) {})
	edge.onPoolCtl(nil, ctl(5, 64))
	if got := edge.peerMaxNow(); got != 0 {
		t.Fatalf("edge stored %d from a pool-control frame, want 0", got)
	}
}

// The frames keep their old fields first; the ceiling is appended, clamped.
func TestDgPeerMaxPayloadLayout(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 2, 64, 8, func(string, ...any) {})
	b := p.poolCtlPayload(17)
	if len(b) != 4 || u16At(b, 0) != 17 || u16At(b, 2) != 64 {
		t.Fatalf("pool-control payload %v, want [target 17][ceiling 64]", b)
	}
	p.max = 70000
	if got := p.ceilingU16(); got != 0xffff {
		t.Fatalf("ceiling %d not clamped to 65535", got)
	}
}

// statsOf captures the StatsFn a Run* function publishes.
func statsOf(dst *atomic.Pointer[StatsFn]) func(StatsFn) {
	return func(f StatsFn) { dst.Store(&f) }
}

func waitPeerMax(t *testing.T, what string, f *atomic.Pointer[StatsFn], want int) {
	t.Helper()
	v2Wait(t, 10*time.Second, what, func() bool {
		fn := f.Load()
		return fn != nil && (*fn)().PeerMax == want
	})
}

// End to end, DIRECT: the edge learns the exit's ceiling from TypeLinkStats,
// the exit learns the edge's from the slow TypePoolCtl info send.
func TestDgPeerMaxDirectBothServers(t *testing.T) {
	edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
	dialer, accepter := newDgFakeLink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var edgeSt, exitSt atomic.Pointer[StatsFn]
	go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 8, PerLink: 8, Dialer: dialer, Log: func(string, ...any) {}, OnStart: statsOf(&edgeSt)})
	go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 12, PerLink: 8, Listener: accepter, Log: func(string, ...any) {}, OnStart: statsOf(&exitSt)})
	waitTunUp(t, edgeTUN, exitTUN)
	waitPeerMax(t, "edge learns the exit's 12", &edgeSt, 12)
	waitPeerMax(t, "exit learns the edge's 8", &exitSt, 8)
}

// End to end, REVERSE: the edge learns the exit's ceiling from TypeLinkStats,
// the exit learns the edge's from the target frames it already receives.
func TestDgPeerMaxReverseBothServers(t *testing.T) {
	edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
	dialer, accepter := newDgFakeLink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var edgeSt, exitSt atomic.Pointer[StatsFn]
	go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 8, PerLink: 8, Reverse: true, Listener: accepter, Log: func(string, ...any) {}, OnStart: statsOf(&edgeSt)})
	go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 6, PerLink: 8, Reverse: true, Dialer: dialer, Log: func(string, ...any) {}, OnStart: statsOf(&exitSt)})
	waitTunUp(t, edgeTUN, exitTUN)
	waitPeerMax(t, "edge learns the exit's 6", &edgeSt, 6)
	waitPeerMax(t, "exit learns the edge's 8", &exitSt, 8)
}
