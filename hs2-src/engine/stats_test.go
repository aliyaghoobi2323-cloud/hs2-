package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Tests for the exit link stats (kindStats, stats.go): the wire format, the
// edge/exit round trip over a real smux link, the fallback against an exit
// that predates kind 6, forward compatibility with longer records, and that an
// idle link is never polled.

// statsPair is one link over an in-memory pipe, stacked on both ends as
// production stacks it (newSession: shaper, meter, watchConn). The edge side
// is a Link openStats accepts; every stream the edge opens goes to exit.
type statsPair struct {
	edge    *ctrlFakeLink
	exitMtr *linkMeter
	srv     *smux.Session
}

type statsExit func(ctx context.Context, srv *smux.Session, st *smux.Stream, mtr *linkMeter)

func newStatsPair(t *testing.T, ctx context.Context, wrapEdge func(net.Conn) net.Conn, exit statsExit) *statsPair {
	t.Helper()
	a, b := net.Pipe()
	var ec net.Conn = a
	if wrapEdge != nil {
		ec = wrapEdge(a)
	}
	edgeMtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	cli, _, err := newSession(ec, false, nil, edgeMtr)
	if err != nil {
		t.Fatal(err)
	}
	exitMtr := &linkMeter{}
	srv, _, err := newSession(b, true, nil, exitMtr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go exit(ctx, srv, st, exitMtr)
		}
	}()
	t.Cleanup(func() { cli.Close(); srv.Close() })
	return &statsPair{edge: &ctrlFakeLink{sess: cli, m: edgeMtr}, exitMtr: exitMtr, srv: srv}
}

// startStats runs openStats for the edge; the test waits for it to return
// before it ends.
func (p *statsPair) startStats(t *testing.T, ctx context.Context, logf func(string, ...any)) <-chan struct{} {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		openStats(ctx, p.edge, logf)
	}()
	t.Cleanup(func() {
		cancel()
		p.edge.Close() // unblocks a handshake read
		<-done
	})
	return done
}

func waitStatsState(t *testing.T, mtr *linkMeter, want int32, d time.Duration) {
	t.Helper()
	v2Wait(t, d, fmt.Sprintf("stats state %d", want), func() bool { return mtr.statsState.Load() == want })
}

// pollRecord asks the exit for a record and waits for it to arrive.
func pollRecord(t *testing.T, mtr *linkMeter, seq uint32) statsRec {
	t.Helper()
	pollStats(mtr)
	v2Wait(t, 5*time.Second, fmt.Sprintf("stats record %d", seq), func() bool {
		r := mtr.peer.Load()
		return r != nil && r.seq >= seq
	})
	return *mtr.peer.Load()
}

// currentExit serves every stream as the exit does today (serveStream),
// delivering TCP streams to panel.
func currentExit(panel string) statsExit {
	return func(ctx context.Context, _ *smux.Session, st *smux.Stream, mtr *linkMeter) {
		serveStream(ctx, st, KharejConfig{Panel: panel}, nil, nil, nil, mtr)
	}
}

// previousExit is an exit from before kindStats: its serveStream closes a
// stream whose kind it does not know.
func previousExit(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil || kind[0] == kindStats {
		st.Close()
		return
	}
	io.Copy(io.Discard, st)
}

// The fallback against a previous-release exit, which closes kind 6: each
// link is marked unsupported within the 5 s handshake bound while the link
// itself stays up, and the process logs the fallback exactly once. A link
// that dies during the handshake is not misreported as an older exit.
func TestStatsFallbackPreviousExit(t *testing.T) {
	// The fallback line is logged once per process; this test owns it. Every
	// openStats goroutine of the tests in this file is joined before its test
	// ends, so resetting it here races with none of them.
	statsUnsupportedOnce = sync.Once{}
	lg := &v2Log{}
	ctx := context.Background()
	for i := 0; i < 2; i++ { // a pool of two links to the older exit
		p := newStatsPair(t, ctx, nil, previousExit)
		t0 := time.Now()
		done := p.startStats(t, ctx, lg.logf)
		waitStatsState(t, p.edge.m, statsUnsupported, statsHandshakeTimeout)
		t.Logf("link %d: unsupported after %s", i, time.Since(t0).Round(time.Millisecond))
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("openStats kept running after the fallback")
		}
		if !p.edge.Alive() {
			t.Fatal("the link died: the refusal must only close the stats stream")
		}
		pollStats(p.edge.m) // must be a no-op now
		if len(p.edge.m.statsPoll) != 0 {
			t.Fatal("an unsupported link was polled")
		}
	}
	if n := lg.count("does not report link stats"); n != 1 {
		t.Fatalf("fallback logged %d times for 2 links, want once:\n%s", n, lg)
	}

	// The link dies during the handshake: not an older exit.
	dying := func(_ context.Context, srv *smux.Session, st *smux.Stream, _ *linkMeter) { srv.Close() }
	p := newStatsPair(t, ctx, nil, dying)
	done := p.startStats(t, ctx, lg.logf)
	select {
	case <-done:
	case <-time.After(statsHandshakeTimeout + time.Second):
		t.Fatal("openStats did not return when the link died")
	}
	if s := p.edge.m.statsState.Load(); s != statsPending {
		t.Fatalf("a link lost during the handshake was marked %d, want pending", s)
	}
}

// pacedPanel serves every connection a download of chunk bytes every gap.
func pacedPanel(t *testing.T, chunk int, gap time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, chunk)
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
					time.Sleep(gap)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// slowReadConn is a slow path into the edge: its reader gets at most max bytes
// per read, after a pause.
type slowReadConn struct {
	net.Conn
	max   int
	pause time.Duration
}

func (c *slowReadConn) Read(p []byte) (int, error) {
	time.Sleep(c.pause)
	if len(p) > c.max {
		p = p[:c.max]
	}
	return c.Conn.Read(p)
}

// Round trip over a real smux link: the edge polls, the exit (serveStream's
// kind 6 case) answers with its link meter's counters, and consecutive
// records measure the exit writer's blocked share. Behind a slow path into the
// edge it reads > 60% blocked (download pressure); with a reader that keeps up
// it reads < 10%, though the link moves megabytes.
func TestStatsRoundTripSlowVsFastReader(t *testing.T) {
	measure := func(t *testing.T, slow bool) (blk float64, r1, r2 statsRec, exit *linkMeter) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		panel := pacedPanel(t, 16<<10, 5*time.Millisecond) // ~3 MB/s offered
		var wrap func(net.Conn) net.Conn
		if slow { // ~400 KB/s path
			wrap = func(c net.Conn) net.Conn { return &slowReadConn{Conn: c, max: 4 << 10, pause: 10 * time.Millisecond} }
		}
		p := newStatsPair(t, ctx, wrap, currentExit(panel))
		p.startStats(t, ctx, nil)
		waitStatsState(t, p.edge.m, statsOK, statsHandshakeTimeout)
		st, err := p.edge.OpenRawStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write([]byte{kindTCP}); err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, st)
		time.Sleep(300 * time.Millisecond) // reach steady state
		r1 = pollRecord(t, p.edge.m, 1)
		time.Sleep(time.Second)
		r2 = pollRecord(t, p.edge.m, 2)
		if r1.seq != 1 || r2.seq != 2 || r2.mono <= r1.mono || r2.tx <= r1.tx || r1.tx == 0 {
			t.Fatalf("records do not advance: %+v then %+v", r1, r2)
		}
		if r1.flags != 0 {
			t.Fatalf("flags %#x: no socket under a pipe, so no TCP_INFO", r1.flags)
		}
		if r2.tx > p.exitMtr.wrBytes.Load() || r2.txBlocked > uint64(p.exitMtr.wrBlocked.Load()) {
			t.Fatal("record counters ahead of the exit's meter")
		}
		return float64(r2.txBlocked-r1.txBlocked) / float64(r2.mono-r1.mono), r1, r2, p.exitMtr
	}
	// pressedPair feeds the two records to the edge's consumer and reports
	// whether they make one raw download-pressure sample.
	pressedPair := func(r1, r2 statsRec) bool {
		ml := &managedLink{}
		now := time.Now()
		ml.consumeRecord(&r1, now)
		ml.consumeRecord(&r2, now)
		return ml.dnHist&1 == 1
	}
	t.Run("fast reader", func(t *testing.T) {
		blk, r1, r2, _ := measure(t, false)
		rate := float64(r2.tx-r1.tx) / time.Duration(r2.mono-r1.mono).Seconds()
		t.Logf("blocked %.1f%%, %.2f MB/s", blk*100, rate/1e6)
		if blk >= 0.1 {
			t.Fatalf("a link whose reader keeps up reads %.0f%% blocked, want < 10%%", blk*100)
		}
		if rate*healthTick.Seconds() < pressMinBytes {
			t.Fatalf("only %.0f B/s moved: the case does not exercise a busy link", rate)
		}
		if pressedPair(r1, r2) {
			t.Fatal("records of an unconstrained link read as download pressure")
		}
	})
	t.Run("slow reader", func(t *testing.T) {
		blk, r1, r2, _ := measure(t, true)
		t.Logf("blocked %.1f%%, %.2f MB/s", blk*100, float64(r2.tx-r1.tx)/time.Duration(r2.mono-r1.mono).Seconds()/1e6)
		if blk <= 0.6 {
			t.Fatalf("behind a slow path the exit writer reads %.0f%% blocked, want > 60%%", blk*100)
		}
		if !pressedPair(r1, r2) {
			t.Fatal("records of a path-limited link do not read as download pressure")
		}
	})
}

// A record longer than the 64 bytes this version knows (a newer exit) is read
// whole: the known prefix is parsed and the rest skipped, so every following
// record stays aligned. A reply this version cannot use falls back.
func TestStatsLongerRecordParsesKnownPrefix(t *testing.T) {
	want := func(seq uint32) statsRec {
		s := uint64(seq)
		return statsRec{seq: seq, flags: statsFlagChrono | statsFlagTCPInfo, mono: s * 1e9, tx: s << 20,
			txBlocked: s * 7e8, busy: s * 1e6, rwnd: s * 3e5, sndbuf: s * 2e5, delivery: s * 12345}
	}
	newerExit := func(ver, recLen byte) statsExit {
		return func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
			defer st.Close()
			var hs [2]byte // kind, version
			if _, err := io.ReadFull(st, hs[:]); err != nil {
				return
			}
			st.Write([]byte{ver, recLen, 3})
			var p [4]byte
			for {
				if _, err := io.ReadFull(st, p[:]); err != nil {
					return
				}
				rec := make([]byte, recLen)
				putStatsRec(rec, want(binary.BigEndian.Uint32(p[:])))
				for i := statsRecLen; i < len(rec); i++ {
					rec[i] = 0xEE
				}
				if _, err := st.Write(rec); err != nil {
					return
				}
			}
		}
	}
	ctx := context.Background()
	t.Run("recLen 72", func(t *testing.T) {
		p := newStatsPair(t, ctx, nil, newerExit(statsVer, 72))
		p.startStats(t, ctx, nil)
		waitStatsState(t, p.edge.m, statsOK, statsHandshakeTimeout)
		for seq := uint32(1); seq <= 3; seq++ {
			got := pollRecord(t, p.edge.m, seq)
			if got.at.IsZero() {
				t.Fatal("record without its receive time")
			}
			got.at = time.Time{}
			if got != want(seq) {
				t.Fatalf("record %d parsed as %+v, want %+v", seq, got, want(seq))
			}
		}
	})
	for _, c := range []struct {
		name        string
		ver, recLen byte
	}{{"recLen 32 (shorter than known)", statsVer, 32}, {"unknown version", statsVer + 1, statsRecLen}} {
		t.Run(c.name, func(t *testing.T) {
			p := newStatsPair(t, ctx, nil, newerExit(c.ver, c.recLen))
			p.startStats(t, ctx, nil)
			waitStatsState(t, p.edge.m, statsUnsupported, statsHandshakeTimeout)
		})
	}
}

// The record layout is the wire contract of §8: fixed offsets, big-endian,
// reserved bytes zero.
func TestStatsRecordWireLayout(t *testing.T) {
	r := statsRec{seq: 0x01020304, flags: 0x0506, mono: 0x0708090a0b0c0d0e, tx: 1, txBlocked: 2, busy: 3, rwnd: 4, sndbuf: 5, delivery: 6}
	b := make([]byte, statsRecLen)
	for i := range b {
		b[i] = 0xFF
	}
	putStatsRec(b, r)
	be := binary.BigEndian
	if be.Uint32(b[0:]) != r.seq || be.Uint16(b[4:]) != r.flags || be.Uint16(b[6:]) != 0 || be.Uint64(b[8:]) != r.mono {
		t.Fatalf("header fields misplaced: % x", b[:16])
	}
	for i, v := range []uint64{r.tx, r.txBlocked, r.busy, r.rwnd, r.sndbuf, r.delivery} {
		if got := be.Uint64(b[16+8*i:]); got != v {
			t.Fatalf("field at offset %d = %d, want %d", 16+8*i, got, v)
		}
	}
	if got := parseStatsRec(b); got != r {
		t.Fatalf("round trip: %+v, want %+v", got, r)
	}
}

// pollCounter counts what the exit reads on its stats stream.
type pollCounter struct {
	io.ReadWriteCloser
	n *atomic.Int64
}

func (c pollCounter) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// An idle link sends the exit no polls — no extra periodic beat on the wire;
// a tick in which the link moved at least 16 KiB sends exactly one.
func TestStatsIdleLinkSendsNoPolls(t *testing.T) {
	var read atomic.Int64
	exit := func(ctx context.Context, _ *smux.Session, st *smux.Stream, mtr *linkMeter) {
		var kind [1]byte
		if _, err := io.ReadFull(st, kind[:]); err != nil || kind[0] != kindStats {
			st.Close()
			return
		}
		serveStats(ctx, pollCounter{st, &read}, nil, mtr)
	}
	polls := func() int64 { return (read.Load() - 1) / 4 } // after the version byte, 4 bytes a poll
	ctx := context.Background()
	p := newStatsPair(t, ctx, nil, exit)
	m, clk, _ := newV2Manager(nil, 1, 8, false)
	addManaged(m, p.edge)
	p.startStats(t, ctx, nil)
	waitStatsState(t, p.edge.m, statsOK, statsHandshakeTimeout)

	m.sampleHealth()
	for i := 0; i < 5; i++ {
		clk.Advance(healthTick)
		m.sampleHealth()
	}
	time.Sleep(200 * time.Millisecond)
	if n := polls(); n != 0 {
		t.Fatalf("an idle link polled the exit %d times", n)
	}
	p.edge.m.rdBytes.Add(64 << 10) // a busy tick
	clk.Advance(healthTick)
	m.sampleHealth()
	pollRecordWait := func() {
		v2Wait(t, 5*time.Second, "the busy tick's record", func() bool { return p.edge.m.peer.Load() != nil })
	}
	pollRecordWait()
	for i := 0; i < 3; i++ {
		clk.Advance(healthTick)
		m.sampleHealth()
	}
	time.Sleep(200 * time.Millisecond)
	if n := polls(); n != 1 {
		t.Fatalf("polls=%d, want exactly 1 (one busy tick)", n)
	}
}

// Unit level: sampleHealth signals a poll only after a tick that moved at least
// 16 KiB, only while stats are negotiated, and never queues more than one.
func TestSampleHealthPollsOnlyBusyLinks(t *testing.T) {
	m, clk, _ := newV2Manager(nil, 1, 8, false)
	f := newMeteredFake()
	f.m.statsPoll = make(chan struct{}, 1)
	f.m.statsState.Store(statsOK)
	addManaged(m, f)
	m.sampleHealth()
	tick := func(rd, wr uint64) int {
		clk.Advance(healthTick)
		f.m.rdBytes.Add(rd)
		f.m.wrBytes.Add(wr)
		m.sampleHealth()
		return len(f.m.statsPoll)
	}
	if n := tick(8<<10, 7<<10); n != 0 {
		t.Fatal("polled after a tick that moved 15 KiB")
	}
	if n := tick(8<<10, 8<<10); n != 1 {
		t.Fatal("no poll after a tick that moved 16 KiB")
	}
	if n := tick(1<<20, 0); n != 1 {
		t.Fatalf("%d polls queued, want at most 1", n)
	}
	<-f.m.statsPoll
	f.m.statsState.Store(statsUnsupported)
	if n := tick(1<<20, 1<<20); n != 0 {
		t.Fatal("polled a link whose exit does not report stats")
	}
}
