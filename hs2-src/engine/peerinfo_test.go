package engine

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Tests for the per-link peer info (kindInfo, peerinfo.go): the wire format and
// its forward compatibility, the exchange over a real smux link, the reverse
// exit reporting the max it applies, compatibility with an older hs2 on either
// side, and the end-to-end view on BOTH servers in direct and reverse.

func TestPeerInfoWire(t *testing.T) {
	for _, n := range []int{0, 2, 32, 48, 64, 1024, 65535} {
		got, err := readInfo(bytes.NewReader(encodeInfo(n)))
		if err != nil || got != n {
			t.Fatalf("round trip %d: got %d, %v", n, got, err)
		}
	}
	// Out-of-range values are clamped, never wrapped.
	if got, _ := readInfo(bytes.NewReader(encodeInfo(70000))); got != 65535 {
		t.Fatalf("70000 encoded as %d, want 65535 (clamped)", got)
	}
	if got, _ := readInfo(bytes.NewReader(encodeInfo(-5))); got != 0 {
		t.Fatalf("-5 encoded as %d, want 0", got)
	}
	// A later version appends fields: the ceiling is still read, the rest is
	// skipped, and the stream is left exactly after the message.
	future := []byte{2, 5, 0, 48, 0xaa, 0xbb, 0xcc, 0x99}
	r := bytes.NewReader(future)
	if got, err := readInfo(r); err != nil || got != 48 {
		t.Fatalf("future message: got %d, %v; want 48", got, err)
	}
	if r.Len() != 1 {
		t.Fatalf("future message: %d byte(s) left, want 1 (exactly the message consumed)", r.Len())
	}
	// A payload too short to hold the field means "not reported", not an error.
	if got, err := readInfo(bytes.NewReader([]byte{1, 0})); err != nil || got != 0 {
		t.Fatalf("empty payload: got %d, %v; want 0, nil", got, err)
	}
	// Version 0 is not a peer-info message.
	if _, err := readInfo(bytes.NewReader([]byte{0, 2, 0, 1})); err != errInfoVersion {
		t.Fatalf("version 0: err %v, want errInfoVersion", err)
	}
	// A truncated message is an error (EOF), never a half-read value.
	if _, err := readInfo(bytes.NewReader([]byte{1, 2, 0})); err == nil {
		t.Fatal("truncated message read without error")
	}
}

// The exchange over a real smux link, stacked as production stacks it: each
// side ends up with the other's ceiling.
func TestPeerInfoExchange(t *testing.T) {
	ctx := context.Background()
	exit := func(ctx context.Context, _ *smux.Session, st *smux.Stream, mtr *linkMeter) {
		serveStream(ctx, st, KharejConfig{MaxLinks: 48}, nil, nil, nil, mtr)
	}
	p := newStatsPair(t, ctx, nil, exit)
	openInfo(ctx, p.edge, 64)
	if got := p.edge.m.peerMax.Load(); got != 48 {
		t.Fatalf("edge learned exit max %d, want 48", got)
	}
	if got := p.exitMtr.peerMax.Load(); got != 64 {
		t.Fatalf("exit link learned edge max %d, want 64", got)
	}
}

// The exit reads the edge's ceiling only from links that are up NOW: when the
// links that reported a value are gone (e.g. the Iran server was rolled back
// to a release that does not report it), the old number goes with them.
func TestPeerInfoExitForgetsGoneLinks(t *testing.T) {
	var ps linkPeers
	a, b := &linkMeter{}, &linkMeter{}
	a.peerMax.Store(64)
	ps.add(a)
	ps.add(b)
	if got := ps.max(); got != 64 {
		t.Fatalf("max %d, want 64", got)
	}
	ps.remove(a) // the reporting link went away; b (an older edge's link) reports nothing
	if got := ps.max(); got != 0 {
		t.Fatalf("after the reporting link went: %d, want 0 (never a gone link's value)", got)
	}
	var nilPeers *linkPeers // nil-safe (tests call serveStream without one)
	nilPeers.add(a)
	nilPeers.remove(a)
	if nilPeers.max() != 0 {
		t.Fatal("nil linkPeers must report 0")
	}
	// The reverse exit pool reads the same set.
	pool := &exitPool{peers: &ps, min: 2, max: 8}
	ps.add(a)
	if got := pool.stats().PeerMax; got != 64 {
		t.Fatalf("exit pool PeerMax %d, want 64", got)
	}
}

// A reverse exit reports the max its pool actually clamps to, not a config
// value it might also carry.
func TestPeerInfoReverseExitReportsPoolMax(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := newExitPool(ctx, 2, 40, func() (dialedLink, error) { return nil, io.EOF }, func(string, ...any) {})
	exit := func(ctx context.Context, _ *smux.Session, st *smux.Stream, mtr *linkMeter) {
		serveStream(ctx, st, KharejConfig{MaxLinks: 99}, nil, nil, pool, mtr)
	}
	p := newStatsPair(t, ctx, nil, exit)
	openInfo(ctx, p.edge, 64)
	if got := p.edge.m.peerMax.Load(); got != 40 {
		t.Fatalf("edge learned %d, want the pool's applied max 40", got)
	}
}

// An exit older than kindInfo closes the stream: the edge gives up at once
// (no retry loop, no hang), leaves the value unknown, and the link stays up.
func TestPeerInfoOlderExit(t *testing.T) {
	ctx := context.Background()
	older := func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
		var kind [1]byte
		io.ReadFull(st, kind[:])
		st.Close() // its serveStream has no kindInfo case
	}
	p := newStatsPair(t, ctx, nil, older)
	t0 := time.Now()
	openInfo(ctx, p.edge, 64)
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("openInfo took %s against an older exit; it must give up at once", d)
	}
	if got := p.edge.m.peerMax.Load(); got != 0 {
		t.Fatalf("edge stored %d from an older exit, want 0 (unknown)", got)
	}
	if !p.edge.Alive() {
		t.Fatal("the link died: refusing kindInfo must only close that stream")
	}
}

// A reply that never comes (a congested link) is retried, then given up —
// bounded, and the link is not touched.
func TestPeerInfoNoReplyIsBounded(t *testing.T) {
	oldRetry, oldTries := infoRetry, infoTries
	infoRetry, infoTries = 50*time.Millisecond, 2
	defer func() { infoRetry, infoTries = oldRetry, oldTries }()
	ctx := context.Background()
	var opened atomic.Int32
	silent := func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
		opened.Add(1)
		io.Copy(io.Discard, st) // reads, never answers
	}
	p := newStatsPair(t, ctx, nil, silent)
	t0 := time.Now()
	openInfo(ctx, p.edge, 64)
	el := time.Since(t0)
	if el > time.Duration(infoTries)*infoTimeout+2*time.Second {
		t.Fatalf("openInfo ran %s; must be bounded by tries × timeout", el)
	}
	if n := opened.Load(); n != int32(infoTries) {
		t.Fatalf("opened %d info streams, want %d (one per try)", n, infoTries)
	}
	if p.edge.m.peerMax.Load() != 0 || !p.edge.Alive() {
		t.Fatal("no reply must leave the value unknown and the link up")
	}
}

// LinkManager.Stats reports the exit's ceiling from the live links' meters.
func TestPeerInfoInPoolStats(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 8, false)
	f := newMeteredFake()
	addManaged(m, f)
	if got := m.Stats().PeerMax; got != 0 {
		t.Fatalf("before any exchange: PeerMax %d, want 0", got)
	}
	f.m.peerMax.Store(48)
	if got := m.Stats().PeerMax; got != 48 {
		t.Fatalf("PeerMax %d, want 48", got)
	}
}

// End to end over real TLS links, DIRECT: the edge sees the exit's configured
// max (reported, not applied) and the exit sees the edge's.
func TestPeerInfoDirectBothServers(t *testing.T) {
	r := startV2(t, v2Opts{direct: true, min: 2, max: 6, perLink: 8, exitCfg: 9})
	r.waitFor("edge learns the exit's 9", 10*time.Second, func() bool { return r.lm.Stats().PeerMax == 9 })
	r.waitFor("exit learns the edge's 6", 10*time.Second, func() bool { return r.exitView().PeerMax == 6 })
}

// End to end, REVERSE: the edge sees the exit's applied max, the exit the edge's.
func TestPeerInfoReverseBothServers(t *testing.T) {
	r := startV2(t, v2Opts{min: 2, max: 6, perLink: 8, exitLinks: 2, exitMin: 2, exitMax: 4})
	r.waitFor("edge learns the exit's 4", 10*time.Second, func() bool { return r.lm.Stats().PeerMax == 4 })
	r.waitFor("exit learns the edge's 6", 10*time.Second, func() bool { return r.exitView().PeerMax == 6 })
}

// Mixed versions: an older exit or an older edge leaves the other side's
// value unknown on the newer one, and the tunnel still carries traffic.
func TestPeerInfoMixedVersions(t *testing.T) {
	t.Run("older exit", func(t *testing.T) {
		r := startV2(t, v2Opts{direct: true, min: 2, max: 6, perLink: 8, oldExit: true})
		r.waitFor("links up", 10*time.Second, func() bool { return r.lm.Stats().Links >= 2 })
		time.Sleep(time.Second)
		if got := r.lm.Stats().PeerMax; got != 0 {
			t.Fatalf("edge PeerMax %d against an older exit, want 0", got)
		}
		if !echoOnce(r.userAddr, 4096) {
			t.Fatal("the tunnel does not carry traffic")
		}
	})
	t.Run("older edge", func(t *testing.T) {
		r := startV2(t, v2Opts{direct: true, min: 2, max: 6, perLink: 8, exitCfg: 9, oldEdge: true})
		r.waitFor("links up", 10*time.Second, func() bool { return r.exitView().Links >= 2 })
		time.Sleep(time.Second)
		if got := r.exitView().PeerMax; got != 0 {
			t.Fatalf("exit PeerMax %d from an older edge, want 0", got)
		}
		if !echoOnce(r.userAddr, 4096) {
			t.Fatal("the tunnel does not carry traffic")
		}
	})
}
