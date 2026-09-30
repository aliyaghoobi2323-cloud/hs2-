package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Unit tests for the link-pool controller v2 actuator (LinkManager): the
// pressure signals sampleHealth derives, per-stream activity, Pick, reconcile /
// reap / heal, drainTick with idle reclaim, and the reverse-mode close guards.
// Everything runs on a fake clock (LinkManager.clock) with fake or in-memory
// links, so no test depends on wall-clock timing except where a real smux
// session is involved.

// ---- helpers ---------------------------------------------------------------

// v2Clock is a settable clock for LinkManager.clock. It starts at the real
// time so it agrees with the timestamps real streams take when they open.
type v2Clock struct {
	mu sync.Mutex
	t  time.Time
}

func newV2Clock() *v2Clock { return &v2Clock{t: time.Now()} }

func (c *v2Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *v2Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// v2Log records log lines.
type v2Log struct {
	mu    sync.Mutex
	lines []string
}

func (l *v2Log) logf(format string, a ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
	l.mu.Unlock()
}

func (l *v2Log) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *v2Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// newV2Manager builds a LinkManager (per_link 8) on a fake clock that logs
// into a recorder.
func newV2Manager(dialer LinkDialer, min, max int, accept bool) (*LinkManager, *v2Clock, *v2Log) {
	lg := &v2Log{}
	m := NewLinkManager(dialer, min, max, 8, lg.logf)
	m.accept = accept
	clk := newV2Clock()
	m.clock = clk.Now
	return m, clk, lg
}

// addManaged puts l into the pool as a serving link, the way dialLink does,
// and returns its entry.
func addManaged(m *LinkManager, l Link) *managedLink {
	m.mu.Lock()
	defer m.mu.Unlock()
	ml := m.newManaged(l, m.linkSeq, m.now())
	m.linkSeq++
	m.links = append(m.links, ml)
	return ml
}

// entryOf returns the pool entry holding l, or nil if l is not in the pool.
func entryOf(m *LinkManager, l Link) *managedLink {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if ml.link == l {
			return ml
		}
	}
	return nil
}

func inPool(m *LinkManager, l Link) bool { return entryOf(m, l) != nil }

func isPressed(m *LinkManager, ml *managedLink) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return ml.pressed
}

func isRetiring(m *LinkManager, ml *managedLink) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return ml.retiring
}

func poolCounts(m *LinkManager) (serving, retiring int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.countsLocked()
}

// v2Wait polls ok until it holds, failing the test after d.
func v2Wait(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", d, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// v2PipeLink is a real mtcpLink — an smux client over an in-memory pipe,
// stacked as production stacks it (newSession) — whose exit side swallows
// every stream and records how each one ended: with the session still up,
// io.EOF means the edge closed that stream with a FIN.
type v2PipeLink struct {
	*mtcpLink
	srv  *smux.Session
	mu   sync.Mutex
	ends map[uint32]*v2StreamEnd
}

type v2StreamEnd struct {
	done chan struct{}
	err  error
}

func newV2PipeLink(t *testing.T) *v2PipeLink {
	t.Helper()
	a, b := net.Pipe()
	cli, _, err := newSession(a, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(b, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &v2PipeLink{mtcpLink: &mtcpLink{sess: cli}, srv: srv, ends: map[uint32]*v2StreamEnd{}}
	go func() {
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			e := p.end(st.ID())
			go func() {
				buf := make([]byte, 32<<10)
				for e.err == nil {
					_, e.err = st.Read(buf)
				}
				close(e.done)
			}()
		}
	}()
	t.Cleanup(func() { p.Close() })
	return p
}

func (p *v2PipeLink) end(id uint32) *v2StreamEnd {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ends[id]
	if e == nil {
		e = &v2StreamEnd{done: make(chan struct{})}
		p.ends[id] = e
	}
	return e
}

// Close closes both ends (the embedded mtcpLink would also close its TLS
// carrier, which this link does not have).
func (p *v2PipeLink) Close() error {
	p.sess.Close()
	return p.srv.Close()
}

// open opens one user stream.
func (p *v2PipeLink) open(t *testing.T) *countedStream {
	t.Helper()
	s, err := p.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	return s.(*countedStream)
}

// exitEnded waits up to d for the exit side of cs to end and reports how.
func (p *v2PipeLink) exitEnded(cs *countedStream, d time.Duration) (ended bool, err error) {
	e := p.end(cs.ID())
	select {
	case <-e.done:
		return true, e.err
	case <-time.After(d):
		return false, nil
	}
}

// ---- 19: sampleHealth pressure signals ---------------------------------------

// upDriver feeds a metered fake one health tick of upload at a time: bytes
// written, the fraction of the tick its writer was blocked, and the share of
// the socket's busy time limited by the peer's receive window.
type upDriver struct {
	m          *LinkManager
	clk        *v2Clock
	f          *meteredFakeLink
	ml         *managedLink
	chrono     bool
	busy, rwnd uint64 // cumulative TCP_INFO chrono counters, µs
}

func newUpDriver(chrono bool) *upDriver {
	m, clk, _ := newV2Manager(nil, 1, 8, false)
	f := newMeteredFake()
	d := &upDriver{m: m, clk: clk, f: f, ml: addManaged(m, f), chrono: chrono}
	m.sampleHealth() // first sample only seeds the counters
	return d
}

func (d *upDriver) tick(bytes uint64, blocked, rwndShare float64) bool {
	d.clk.Advance(healthTick)
	d.f.m.wrBytes.Add(bytes)
	d.f.m.wrBlocked.Add(int64(blocked * float64(healthTick)))
	busy := uint64(healthTick / time.Microsecond)
	d.busy += busy
	d.rwnd += uint64(rwndShare * float64(busy))
	d.f.setTCP(tcpStat{busyUs: d.busy, rwndUs: d.rwnd, chronoValid: d.chrono})
	d.m.sampleHealth()
	return isPressed(d.m, d.ml)
}

// Upload pressure: the edge's own writer blocked for at least half the tick,
// while moving at least 16 KiB, and not because the receiver's window was
// full; sticky over 2 of the last 3 samples.
func TestSampleHealthUploadPressure(t *testing.T) {
	cases := []struct {
		name    string
		bytes   uint64
		blocked float64
		rwnd    float64
		chrono  bool
		want    bool
	}{
		{"blocked and moving", 64 << 10, 0.8, 0, true, true},
		{"under the 16 KiB byte gate", 15 << 10, 1.0, 0, true, false},
		{"exactly the byte gate", 16 << 10, 0.8, 0, true, true},
		{"moving but not blocked", 4 << 20, 0.3, 0, true, false},
		{"receive-window limited", 64 << 10, 0.8, 0.6, true, false},
		{"rwnd share under half", 64 << 10, 0.8, 0.3, true, true},
		{"no chrono counters (kernel < 4.10): rwnd exclusion off", 64 << 10, 0.8, 0.9, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newUpDriver(c.chrono)
			if d.tick(c.bytes, c.blocked, c.rwnd) {
				t.Fatal("pressed after a single raw sample (needs 2 of 3)")
			}
			for i := 2; i <= 4; i++ {
				if got := d.tick(c.bytes, c.blocked, c.rwnd); got != c.want {
					t.Fatalf("sample %d: pressed=%v, want %v", i, got, c.want)
				}
			}
		})
	}
}

// 1 raw sample of the last 3 is not pressure, 2 of 3 is.
func TestSampleHealthUploadPressureTwoOfThree(t *testing.T) {
	d := newUpDriver(true)
	raw := func() bool { return d.tick(64<<10, 0.9, 0) }
	calm := func() bool { return d.tick(64<<10, 0, 0) }
	steps := []struct {
		f    func() bool
		want bool
		hist string
	}{
		{raw, false, "R"}, {calm, false, "RC"}, {raw, true, "RCR"}, {calm, false, "CRC"},
		{raw, true, "RCR"}, {raw, true, "CRR"}, {calm, true, "RRC"}, {calm, false, "RCC"},
	}
	for i, s := range steps {
		if got := s.f(); got != s.want {
			t.Fatalf("step %d (last samples %s): pressed=%v, want %v", i+1, s.hist, got, s.want)
		}
	}
}

// exitSim produces the cumulative records an exit reports for one link.
type exitSim struct {
	seq                       uint32
	mono, tx, blk, busy, rwnd uint64
	noChrono                  bool             // the exit's kernel has no chrono counters
	atFn                      func() time.Time // when the edge receives each record
}

// next advances the exit by dt, during which it sent tx bytes with its writer
// blocked for `blocked` of the time and rwndShare of its busy time limited by
// the edge's receive window.
func (e *exitSim) next(dt time.Duration, tx uint64, blocked, rwndShare float64) *statsRec {
	if e.mono == 0 {
		e.mono = uint64(time.Hour) // the exit has been up a while
	}
	e.seq++
	e.mono += uint64(dt)
	e.tx += tx
	e.blk += uint64(blocked * float64(dt))
	busy := uint64(dt / time.Microsecond)
	e.busy += busy
	e.rwnd += uint64(rwndShare * float64(busy))
	r := &statsRec{seq: e.seq, flags: statsFlagTCPInfo | statsFlagChrono, mono: e.mono, tx: e.tx,
		txBlocked: e.blk, busy: e.busy, rwnd: e.rwnd, at: e.atFn()}
	if e.noChrono {
		r.flags = statsFlagTCPInfo
	}
	return r
}

// dnDriver feeds a metered fake one exit record per health tick.
type dnDriver struct {
	m   *LinkManager
	clk *v2Clock
	lg  *v2Log
	f   *meteredFakeLink
	ml  *managedLink
	ex  *exitSim
}

func newDnDriver(state int32) *dnDriver {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	f := newMeteredFake()
	f.m.statsState.Store(state)
	d := &dnDriver{m: m, clk: clk, lg: lg, f: f, ml: addManaged(m, f), ex: &exitSim{atFn: clk.Now}}
	m.sampleHealth() // seeds the meter counters (no record consumed yet)
	d.tick(0, 0, 0)  // the first record is only a baseline
	return d
}

// tick advances one health tick with a fresh exit record covering it.
func (d *dnDriver) tick(tx uint64, blocked, rwndShare float64) bool {
	d.clk.Advance(healthTick)
	d.f.m.peer.Store(d.ex.next(healthTick, tx, blocked, rwndShare))
	d.m.sampleHealth()
	return isPressed(d.m, d.ml)
}

// quiet advances one health tick with no new record from the exit.
func (d *dnDriver) quiet() bool {
	d.clk.Advance(healthTick)
	d.m.sampleHealth()
	return isPressed(d.m, d.ml)
}

// Download pressure comes from the exit's records: its writer blocked for at
// least half the time while sending at least 16 KiB per tick, and not because
// this side's receive window was full.
func TestSampleHealthDownloadPressure(t *testing.T) {
	cases := []struct {
		name     string
		tx       uint64
		blocked  float64
		rwnd     float64
		noChrono bool
		want     bool
	}{
		{"blocked and moving", 256 << 10, 0.9, 0, false, true},
		{"under the 16 KiB byte gate", 15 << 10, 1.0, 0, false, false},
		{"exactly the byte gate", 16 << 10, 0.9, 0, false, true},
		{"moving but not blocked", 4 << 20, 0.4, 0, false, false},
		{"receive-window limited", 256 << 10, 0.9, 0.75, false, false},
		{"no chrono counters on the exit: rwnd exclusion off", 256 << 10, 0.9, 0.75, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDnDriver(statsOK)
			d.ex.noChrono = c.noChrono
			if d.tick(c.tx, c.blocked, c.rwnd) {
				t.Fatal("pressed after a single raw sample (needs 2 of 3)")
			}
			for i := 2; i <= 4; i++ {
				if got := d.tick(c.tx, c.blocked, c.rwnd); got != c.want {
					t.Fatalf("sample %d: pressed=%v, want %v", i, got, c.want)
				}
			}
		})
	}
}

// Download pressure is sticky exactly like upload: 2 of the last 3 records;
// and a link that is not serving is never counted as pressed.
func TestSampleHealthDownloadPressureTwoOfThree(t *testing.T) {
	d := newDnDriver(statsOK)
	raw := func() bool { return d.tick(256<<10, 0.9, 0) }
	calm := func() bool { return d.tick(256<<10, 0.1, 0) }
	steps := []struct {
		f    func() bool
		want bool
		hist string
	}{
		{raw, false, "R"}, {calm, false, "RC"}, {raw, true, "RCR"}, {calm, false, "CRC"},
		{raw, true, "RCR"}, {raw, true, "CRR"}, {calm, true, "RRC"}, {calm, false, "RCC"},
	}
	for i, s := range steps {
		if got := s.f(); got != s.want {
			t.Fatalf("step %d (last records %s): pressed=%v, want %v", i+1, s.hist, got, s.want)
		}
	}
	raw()
	raw()
	d.m.mu.Lock()
	d.ml.retiring = true
	d.m.mu.Unlock()
	if raw() {
		t.Fatal("a retiring link was counted as pressed")
	}
}

// A receive-window-limited download is not pressure, and it logs the tuning
// hint — once, not every tick (then again only after rwndHintEvery).
func TestSampleHealthRwndHintLoggedOnce(t *testing.T) {
	d := newDnDriver(statsOK)
	const hint = "limited by this server's receive side"
	for i := 0; i < 20; i++ {
		if d.tick(1<<20, 0.95, 0.8) {
			t.Fatalf("tick %d: a receive-window-limited link counted as pressed", i)
		}
	}
	if n := d.lg.count(hint); n != 1 {
		t.Fatalf("rwnd hint logged %d times over 40 s, want once:\n%s", n, d.lg)
	}
	// Ten minutes later (a gap: the next record only re-baselines) it may be
	// logged again.
	d.clk.Advance(rwndHintEvery)
	d.f.m.peer.Store(d.ex.next(rwndHintEvery, 1<<20, 0.95, 0.8))
	d.m.sampleHealth()
	d.tick(1<<20, 0.95, 0.8)
	if n := d.lg.count(hint); n != 2 {
		t.Fatalf("rwnd hint logged %d times after %s, want 2", n, rwndHintEvery)
	}
	// A blocked writer that is not rwnd-limited gives no hint.
	d2 := newDnDriver(statsOK)
	for i := 0; i < 5; i++ {
		d2.tick(1<<20, 0.95, 0.1)
	}
	if n := d2.lg.count(hint); n != 0 {
		t.Fatalf("rwnd hint logged for a path-limited link:\n%s", d2.lg)
	}
}

// A record older than 6 s no longer counts: when the exit's records stop
// arriving (the link went idle, or the stats stream stalled) the link stops
// being pressed on the first tick past the staleness bound.
func TestSampleHealthStaleRecordNotPressed(t *testing.T) {
	d := newDnDriver(statsOK)
	d.tick(256<<10, 0.9, 0)
	if !d.tick(256<<10, 0.9, 0) {
		t.Fatal("setup: two raw records did not press the link")
	}
	for age := healthTick; age <= statsStale; age += healthTick {
		if !d.quiet() {
			t.Fatalf("unpressed with a record only %s old (stale after %s)", age, statsStale)
		}
	}
	if d.quiet() {
		t.Fatalf("still pressed on a record %s old (stale after %s)", statsStale+healthTick, statsStale)
	}
}

// Unsupported (older exit) or not-yet-negotiated stats never press a link,
// whatever the records say: unknown never triggers growth.
func TestSampleHealthUnsupportedStatsNotPressed(t *testing.T) {
	for _, st := range []struct {
		name  string
		state int32
	}{{"unsupported", statsUnsupported}, {"pending", statsPending}} {
		t.Run(st.name, func(t *testing.T) {
			d := newDnDriver(st.state)
			for i := 0; i < 5; i++ {
				if d.tick(1<<20, 1.0, 0) {
					t.Fatalf("tick %d: pressed with stats %s", i, st.name)
				}
			}
		})
	}
}

// Records further apart than 7 s are not compared: the history is cleared and
// the late record is only a new baseline. An exit whose counters went
// backwards (it restarted) re-baselines the same way.
func TestSampleHealthRecordGapRebaselines(t *testing.T) {
	for _, c := range []struct {
		name string
		jump func(e *exitSim)
	}{
		{"gap over 7 s", func(e *exitSim) { e.mono += uint64(statsGap) }},
		{"exit restarted", func(e *exitSim) { e.mono, e.tx, e.blk = uint64(time.Second), 0, 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDnDriver(statsOK)
			d.tick(256<<10, 0.9, 0)
			if !d.tick(256<<10, 0.9, 0) {
				t.Fatal("setup: two raw records did not press the link")
			}
			c.jump(d.ex)
			if d.tick(256<<10, 1.0, 0) {
				t.Fatal("still pressed across the discontinuity: history not cleared")
			}
			// Had the late record counted as a sample, one more raw record
			// would make 2 of 3.
			if d.tick(256<<10, 0.9, 0) {
				t.Fatal("pressed on the first record after the discontinuity: it was not just a baseline")
			}
			if !d.tick(256<<10, 0.9, 0) {
				t.Fatal("not pressed after two raw records past the new baseline")
			}
		})
	}
}

// ---- 20: per-stream activity ---------------------------------------------------

// flowStats counts a stream as flowing only while its rate EWMA is at least
// 2 KiB/s: a 64 KiB/s transfer within 2 ticks, never a 4 KiB handshake or a
// 100 B-every-30 s heartbeat, and a finished transfer decays out.
func TestFlowStatsFlowingThresholds(t *testing.T) {
	pl := newV2PipeLink(t)
	hs := pl.open(t)   // a reconnect handshake: 4 KiB once
	bulk := pl.open(t) // a 64 KiB/s transfer for 10 s
	beat := pl.open(t) // a heartbeat: 100 B every 30 s
	write := func(s *countedStream, n int) {
		if _, err := s.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	const dt = 2 * time.Second
	t0 := time.Now()
	var fs flowSnap
	for tick := 1; tick <= 300; tick++ { // 10 minutes
		now := t0.Add(time.Duration(tick) * dt)
		if tick == 1 {
			write(hs, 4<<10)
		}
		if tick <= 5 {
			write(bulk, 128<<10)
		}
		if tick%15 == 0 {
			write(beat, 100)
		}
		fs = pl.flowStats(now, dt, drainIdleDefault)
		if fs.open != 3 {
			t.Fatalf("tick %d: open=%d, want 3", tick, fs.open)
		}
		if float64(hs.ewma) >= flowingRate {
			t.Fatalf("tick %d: a 4 KiB handshake counts as flowing (ewma %.0f B/s)", tick, hs.ewma)
		}
		if float64(beat.ewma) >= flowingRate {
			t.Fatalf("tick %d: a heartbeat counts as flowing (ewma %.0f B/s)", tick, beat.ewma)
		}
		switch {
		case tick >= 2 && tick <= 5 && fs.flowing != 1:
			t.Fatalf("tick %d: flowing=%d, want 1 (the 64 KiB/s transfer)", tick, fs.flowing)
		case tick >= 30 && fs.flowing != 0:
			t.Fatalf("tick %d: flowing=%d, want 0 (%s after the transfer ended)", tick, fs.flowing, time.Duration(tick-5)*dt)
		}
	}
	if fs.recent != 1 {
		t.Fatalf("recent=%d at the end, want 1 (only the heartbeat moved within %s)", fs.recent, drainIdleDefault)
	}
	if want := t0.Add(300 * dt); !fs.last.Equal(want) {
		t.Fatalf("last=%v, want the heartbeat's %v", fs.last, want)
	}
}

// idleStreams returns only streams that have not moved a byte for drainIdle —
// not one that moved after the last sample — each with its current byte count,
// and at most max of them.
func TestIdleStreamsOnlyLongIdle(t *testing.T) {
	pl := newV2PipeLink(t)
	busy, idle1, idle2, woke := pl.open(t), pl.open(t), pl.open(t), pl.open(t)
	idle1.Write([]byte("hello")) // bytes before going idle: the snapshot must carry them
	const dt = 2 * time.Second
	t0 := time.Now()
	var now time.Time
	for tick := 1; tick <= 160; tick++ { // 320 s
		now = t0.Add(time.Duration(tick) * dt)
		if _, err := busy.Write(make([]byte, 1<<10)); err != nil {
			t.Fatal(err)
		}
		pl.flowStats(now, dt, drainIdleDefault)
		if tick == 150 { // 300 s: nothing has been idle for drain_idle yet
			if c := pl.idleStreams(now, drainIdleDefault, idleReclaimMax); len(c) != 0 {
				t.Fatalf("at 300 s, %d stream(s) reported idle for %s", len(c), drainIdleDefault)
			}
		}
	}
	woke.Write([]byte("late")) // moved after the last sample
	cands := pl.idleStreams(now, drainIdleDefault, idleReclaimMax)
	got := map[*countedStream]uint64{}
	for _, c := range cands {
		got[c.cs] = c.snap
	}
	if len(got) != 2 || !hasKey(got, idle1) || !hasKey(got, idle2) {
		t.Fatalf("idle candidates: got %d (%v), want exactly the two idle streams", len(got), got)
	}
	for cs, snap := range got {
		if snap != cs.bytes.Load() {
			t.Fatalf("snapshot %d, stream has moved %d", snap, cs.bytes.Load())
		}
	}
	if c := pl.idleStreams(now, drainIdleDefault, 1); len(c) != 1 {
		t.Fatalf("max=1 returned %d candidates", len(c))
	}
}

func hasKey(m map[*countedStream]uint64, k *countedStream) bool { _, ok := m[k]; return ok }

// ---- 22: reconcile, reap, heal -------------------------------------------------

// Growing brings retiring links back before dialing anything: the one with the
// most open connections first, ties to the most recently active, and it counts
// as newly serving (probe attribution). Only the remainder is dialed.
func TestReconcileUnretiresBeforeDialing(t *testing.T) {
	d := &fakeDialer{}
	m, clk, _ := newV2Manager(d, 1, 8, false)
	ctx := context.Background()
	addManaged(m, newMeteredFake())
	addManaged(m, newMeteredFake())
	t0 := clk.Now()
	retire := func(open int, last time.Time) *managedLink {
		ml := addManaged(m, newMeteredFake())
		ml.retiring, ml.retireSince, ml.open, ml.lastByte = true, t0, open, last
		ml.heldLogAt = t0
		return ml
	}
	few := retire(1, t0.Add(-time.Second))
	older := retire(4, t0.Add(-time.Minute))
	newer := retire(4, t0.Add(-time.Second))
	now := clk.Advance(time.Second)

	m.reconcile(ctx, 3)
	if isRetiring(m, newer) || !isRetiring(m, older) || !isRetiring(m, few) {
		t.Fatalf("T=3: wrong link back in service (want the most open, most recently active one)")
	}
	if !newer.servingSince.Equal(now) || !newer.heldLogAt.IsZero() {
		t.Fatalf("un-retired link: servingSince=%v heldLogAt=%v, want now and reset", newer.servingSince, newer.heldLogAt)
	}
	m.reconcile(ctx, 4)
	if isRetiring(m, older) || !isRetiring(m, few) {
		t.Fatal("T=4: the second most-open retiring link was not brought back first")
	}
	if n := d.dials.Load(); n != 0 {
		t.Fatalf("dialed %d link(s) while retiring links could be brought back", n)
	}
	m.reconcile(ctx, 6)
	if isRetiring(m, few) {
		t.Fatal("T=6: the last retiring link was not brought back")
	}
	if n := d.dials.Load(); n != 1 {
		t.Fatalf("T=6 with 5 serving after un-retiring: dialed %d, want 1", n)
	}
	if S, R := poolCounts(m); S != 6 || R != 0 {
		t.Fatalf("after T=6: %d serving, %d retiring; want 6, 0", S, R)
	}
}

// Shrinking marks as retiring the links that will empty soonest: fewest
// flowing, then fewest recently active, then fewest open, then lowest rate.
func TestReconcileVictimOrder(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	mk := func(flowing, recent, open int, rate10 float64) *managedLink {
		ml := addManaged(m, newMeteredFake())
		ml.flowing, ml.recent, ml.open, ml.rate10 = flowing, recent, open, rate10
		return ml
	}
	a := mk(0, 0, 5, 0)
	b := mk(0, 1, 0, 0)
	c := mk(1, 0, 0, 0)
	d := mk(0, 0, 1, 900)
	e := mk(0, 0, 1, 100)
	e.pressed = true
	now := clk.Advance(time.Second)
	m.reconcile(context.Background(), 2)
	for _, v := range []*managedLink{e, d, a} {
		if !isRetiring(m, v) {
			t.Fatalf("link %d (flowing %d recent %d open %d rate %.0f) should retire first", v.id, v.flowing, v.recent, v.open, v.rate10)
		}
		if !v.retireSince.Equal(now) || v.pressed {
			t.Fatalf("link %d: retireSince=%v pressed=%v, want now and false", v.id, v.retireSince, v.pressed)
		}
	}
	for _, v := range []*managedLink{b, c} {
		if isRetiring(m, v) {
			t.Fatalf("link %d (flowing %d recent %d) retired before emptier links", v.id, v.flowing, v.recent)
		}
	}
	if lg.count("retiring — no new connections") != 1 {
		t.Fatalf("expected one retire log line:\n%s", lg)
	}
	if S, R := poolCounts(m); S != 2 || R != 3 {
		t.Fatalf("%d serving, %d retiring; want 2, 3", S, R)
	}
}

// Dialing never takes the physical count past max: retiring, degraded and
// draining links all count.
func TestReconcileNeverExceedsMax(t *testing.T) {
	ctx := context.Background()
	t.Run("draining links fill the envelope", func(t *testing.T) {
		d := &fakeDialer{}
		m, _, _ := newV2Manager(d, 1, 6, false)
		addManaged(m, newMeteredFake())
		addManaged(m, newMeteredFake())
		for i := 0; i < 3; i++ {
			ml := addManaged(m, newMeteredFake())
			ml.degraded, ml.draining = true, true
		}
		for i := 0; i < 5; i++ {
			m.reconcile(ctx, 6)
		}
		if n := d.dials.Load(); n != 1 {
			t.Fatalf("dialed %d, want 1 (5 physical, max 6)", n)
		}
		if n := m.count(); n != 6 {
			t.Fatalf("physical=%d, want 6 (= max)", n)
		}
	})
	t.Run("un-retired links fill the envelope", func(t *testing.T) {
		d := &fakeDialer{}
		m, _, _ := newV2Manager(d, 1, 6, false)
		for i := 0; i < 6; i++ {
			ml := addManaged(m, newMeteredFake())
			switch {
			case i >= 4:
				ml.degraded, ml.draining = true, true
			case i >= 2:
				ml.retiring = true
			}
		}
		m.reconcile(ctx, 6)
		if n := d.dials.Load(); n != 0 {
			t.Fatalf("dialed %d with the pool at max", n)
		}
		if S, R := poolCounts(m); S != 4 || R != 0 {
			t.Fatalf("%d serving, %d retiring; want 4, 0 (both retiring links back)", S, R)
		}
	})
}

// A dead link is redialed only up to the serving target: 500 open connections
// no longer make the pool redial toward ceil(500/per_link).
func TestReapRedialsOnlyToTarget(t *testing.T) {
	d := &fakeDialer{}
	m, clk, lg := newV2Manager(d, 2, 32, false)
	m.ap.T = 3
	m.target.Store(3)
	fs := []*meteredFakeLink{newMeteredFake(), newMeteredFake(), newMeteredFake()}
	for i, f := range fs {
		ml := addManaged(m, f)
		ml.users.Store(int32(166 + i%2))
		f.act.Store(166)
	}
	m.users.Store(500)
	fs[0].alive.Store(false) // the link dies
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		clk.Advance(healthTick)
		m.reap()
		m.sampleHealth()
		m.heal(ctx)
		m.autoscale(ctx)
		m.drainTick()
	}
	if n := d.dials.Load(); n != 1 {
		t.Fatalf("redialed %d link(s) with T=3 and 500 users, want 1", n)
	}
	if S, R := poolCounts(m); S != 3 || R != 0 || m.count() != 3 {
		t.Fatalf("pool: %d serving, %d retiring, %d physical; want 3, 0, 3", S, R, m.count())
	}
	if m.Target() != 3 {
		t.Fatalf("target moved to %d", m.Target())
	}
	if lg.count("down:") != 1 {
		t.Fatalf("the dead link's reap was not logged once:\n%s", lg)
	}
}

// heal replaces a newly degraded serving link by bringing a retiring link
// back (the one with most open connections) instead of dialing; it dials only
// when no retiring link is left; and a degraded retiring link needs no
// replacement at all.
func TestHealUnretiresInsteadOfDialing(t *testing.T) {
	d := &fakeDialer{}
	m, _, lg := newV2Manager(d, 1, 8, false)
	ctx := context.Background()
	bad := addManaged(m, newMeteredFake())
	addManaged(m, newMeteredFake())
	r1 := addManaged(m, newMeteredFake())
	r1.retiring, r1.open = true, 3
	r2 := addManaged(m, newMeteredFake())
	r2.retiring, r2.open = true, 1
	bad.users.Store(4) // users on it keep it draining, not dropped

	bad.degraded = true
	m.heal(ctx)
	if !bad.draining || bad.retiring {
		t.Fatalf("degraded link: draining=%v retiring=%v, want draining", bad.draining, bad.retiring)
	}
	if isRetiring(m, r1) || !isRetiring(m, r2) {
		t.Fatal("heal did not bring back the retiring link with the most open connections")
	}
	if n := d.dials.Load(); n != 0 {
		t.Fatalf("heal dialed %d replacement(s) while a retiring link was available", n)
	}
	if lg.count("back in service to replace a degraded link") != 1 {
		t.Fatalf("un-retire for heal not logged:\n%s", lg)
	}

	// A degraded RETIRING link takes no replacement.
	r2.degraded = true
	m.heal(ctx)
	if !r2.draining || r2.retiring || d.dials.Load() != 0 {
		t.Fatalf("degraded retiring link: draining=%v retiring=%v dials=%d; want draining, no dial", r2.draining, r2.retiring, d.dials.Load())
	}

	// No retiring link left: the replacement is dialed (make-before-break).
	r1.degraded = true
	r1.users.Store(1)
	m.heal(ctx)
	if n := d.dials.Load(); n != 1 {
		t.Fatalf("with nothing to un-retire, heal dialed %d, want 1", n)
	}
	if !inPool(m, bad.link) || !inPool(m, r1.link) {
		t.Fatal("a draining link with users was dropped")
	}
}

// ---- 23: drainTick ------------------------------------------------------------------

// Only an EMPTY retiring link (no users, no open streams) is closed; it leaves
// the pool under the lock before drainTick returns; at most 2 per tick; the
// close itself happens later, off the pool goroutine.
func TestDrainTickClosesOnlyEmptyRetiring(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	var empty []*meteredFakeLink
	for i := 0; i < 4; i++ {
		f := newMeteredFake()
		ml := addManaged(m, f)
		ml.retiring, ml.retireSince = true, clk.Now()
		empty = append(empty, f)
	}
	withUser, withStream := newMeteredFake(), newMeteredFake()
	mu := addManaged(m, withUser)
	mu.retiring = true
	mu.users.Store(1)
	ma := addManaged(m, withStream)
	ma.retiring = true
	withStream.act.Store(1)
	serving := newMeteredFake()
	addManaged(m, serving)
	draining := newMeteredFake()
	md := addManaged(m, draining)
	md.degraded, md.draining = true, true

	pooled := func() int {
		n := 0
		for _, f := range empty {
			if inPool(m, f) {
				n++
			}
		}
		return n
	}
	clk.Advance(5 * time.Minute)
	for tick, want := range []int{2, 0, 0} {
		m.drainTick()
		if n := pooled(); n != want {
			t.Fatalf("drain tick %d: %d empty retiring links still pooled, want %d (≤ %d closes per tick)", tick+1, n, want, maxClosesPerTick)
		}
	}
	for _, f := range empty {
		v2Wait(t, 2*time.Second, "retired link closed", func() bool { return f.closes.Load() == 1 })
	}
	for name, f := range map[string]*meteredFakeLink{"with a user": withUser, "with an open stream": withStream, "serving": serving, "draining": draining} {
		if !inPool(m, f) || f.closes.Load() != 0 {
			t.Fatalf("link %s was closed by drainTick", name)
		}
	}
	if n := lg.count("retired: its connections ended (5m after retiring)"); n != 4 {
		t.Fatalf("want 4 retired-drained log lines, got %d:\n%s", n, lg)
	}

	// Once they empty, they go too.
	mu.users.Store(0)
	withStream.act.Store(0)
	m.drainTick()
	if inPool(m, withUser) || inPool(m, withStream) {
		t.Fatal("retiring links that emptied were not closed")
	}
}

// drainTick must not wait for a Close that blocks (a real link's Close can
// block up to smux's close timeout): Pick and the pool tick carry on.
func TestDrainTickBlockingCloseDoesNotStallPick(t *testing.T) {
	m, clk, _ := newV2Manager(nil, 1, 8, false)
	s := addManaged(m, newMeteredFake())
	slow := &slowCloseLink{meteredFakeLink: newMeteredFake(), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(slow.release) })
	r := addManaged(m, slow)
	r.retiring = true

	t0 := time.Now()
	m.drainTick()
	if d := time.Since(t0); d > 50*time.Millisecond {
		t.Fatalf("drainTick took %s", d)
	}
	select {
	case <-slow.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the empty retiring link was never closed")
	}
	// Close is now blocked for up to 5 s.
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		l, rel, ok := m.Pick()
		if d := time.Since(t0); d > 50*time.Millisecond {
			t.Fatalf("Pick took %s while a link was closing", d)
		}
		if !ok || l != s.link {
			t.Fatal("Pick did not return the serving link")
		}
		rel()
	}
	t0 = time.Now()
	clk.Advance(healthTick)
	m.sampleHealth()
	m.reconcile(context.Background(), 1)
	m.drainTick()
	_ = m.Stats()
	if d := time.Since(t0); d > 50*time.Millisecond {
		t.Fatalf("a pool tick took %s while a link was closing", d)
	}
}

// slowCloseLink's Close blocks until released or 5 s.
type slowCloseLink struct {
	*meteredFakeLink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *slowCloseLink) Close() error {
	l.once.Do(func() { close(l.entered) })
	select {
	case <-l.release:
	case <-time.After(5 * time.Second):
	}
	return l.meteredFakeLink.Close()
}

// runDrainTicks runs n pool ticks (sampleHealth + drainTick), waiting after
// each for the idle-reclaim pass it may have started.
func runDrainTicks(t *testing.T, m *LinkManager, clk *v2Clock, ml *managedLink, n int, each func(tick int)) {
	t.Helper()
	for tick := 1; tick <= n; tick++ {
		clk.Advance(healthTick)
		if each != nil {
			each(tick)
		}
		m.sampleHealth()
		m.drainTick()
		v2Wait(t, 3*time.Second, "idle-reclaim pass", func() bool { return !ml.reclaiming.Load() })
	}
}

// Idle reclaim on a retiring link: a stream that moves bytes every tick
// survives 10 simulated minutes; a stream idle for drain_idle is closed with a
// FIN; the link stays, held by the busy one.
func TestDrainTickReclaimSparesBusyStream(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	pl := newV2PipeLink(t)
	busy, idle := pl.open(t), pl.open(t)
	ml := addManaged(m, pl)
	ml.retiring, ml.retireSince = true, clk.Now()
	m.sampleHealth()
	runDrainTicks(t, m, clk, ml, 300, func(tick int) {
		if _, err := busy.Write(make([]byte, 512)); err != nil {
			t.Fatalf("tick %d: the busy stream broke: %v", tick, err)
		}
		if tick == 150 && pl.Active() != 2 {
			t.Fatalf("at 300 s a stream was closed before drain_idle (%s)", drainIdleDefault)
		}
	})
	if ended, err := pl.exitEnded(idle, 2*time.Second); !ended || err != io.EOF {
		t.Fatalf("idle stream: ended=%v err=%v, want closed with a FIN (EOF)", ended, err)
	}
	if ended, _ := pl.exitEnded(busy, 50*time.Millisecond); ended {
		t.Fatal("the busy stream was closed")
	}
	if _, err := busy.Write([]byte("still here")); err != nil {
		t.Fatalf("busy stream: %v", err)
	}
	if !inPool(m, pl) || pl.Active() != 1 {
		t.Fatalf("link pooled=%v active=%d, want it kept for its busy stream", inPool(m, pl), pl.Active())
	}
	if lg.count("closed 1 connection(s) idle for over 5m") != 1 {
		t.Fatalf("idle reclaim not logged:\n%s", lg)
	}
}

// wakingLink moves a byte on the first idle candidate right after it is
// collected, as a connection waking up between collection and close would.
type wakingLink struct {
	*v2PipeLink
	woke *countedStream
}

func (w *wakingLink) idleStreams(now time.Time, idle time.Duration, max int) []idleCand {
	c := w.v2PipeLink.idleStreams(now, idle, max)
	if len(c) > 0 {
		w.woke = c[0].cs
		w.woke.Write([]byte("x"))
	}
	return c
}

// A stream whose counter moves between being collected and being closed is
// spared.
func TestReclaimSparesStreamThatWakes(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 8, false)
	pl := newV2PipeLink(t)
	a, b := pl.open(t), pl.open(t)
	wl := &wakingLink{v2PipeLink: pl}
	ml := addManaged(m, wl)
	ml.retiring = true
	ml.reclaiming.Store(true) // as drainTick's CAS leaves it
	m.reclaimIdle(ml, time.Now().Add(time.Hour), drainIdleDefault)
	if ml.reclaiming.Load() {
		t.Fatal("reclaimIdle did not release the per-link reclaim flag")
	}
	if wl.woke == nil {
		t.Fatal("no idle candidates were collected")
	}
	other := a
	if wl.woke == a {
		other = b
	}
	if ended, _ := pl.exitEnded(wl.woke, 100*time.Millisecond); ended {
		t.Fatal("a stream that moved after collection was closed")
	}
	if ended, err := pl.exitEnded(other, 2*time.Second); !ended || err != io.EOF {
		t.Fatalf("the idle stream: ended=%v err=%v, want closed with a FIN (EOF)", ended, err)
	}
	if pl.Active() != 1 {
		t.Fatalf("active=%d, want 1", pl.Active())
	}
}

// drain_idle_sec = 0 disables the reclaim: idle connections on a retiring link
// are never closed, and the link is reported as held once at 15 min.
func TestDrainIdleZeroDisablesReclaim(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	m.SetDrainIdle(0)
	pl := newV2PipeLink(t)
	idle := pl.open(t)
	ml := addManaged(m, pl)
	ml.retiring, ml.retireSince = true, clk.Now()
	m.sampleHealth()
	runDrainTicks(t, m, clk, ml, 600, func(int) { // 20 minutes
		if ml.reclaiming.Load() {
			t.Fatal("an idle-reclaim pass started with drain_idle 0")
		}
	})
	if ended, _ := pl.exitEnded(idle, 50*time.Millisecond); ended || pl.Active() != 1 {
		t.Fatal("an idle connection was closed with drain_idle 0")
	}
	if !inPool(m, pl) {
		t.Fatal("the held link was closed")
	}
	if n := lg.count("retiring 15m: held by 1 open connection(s), 0 active"); n != 1 {
		t.Fatalf("held log: %d lines, want 1:\n%s", n, lg)
	}
}

// A retiring link held by a connection is reported at 15 min, then hourly.
func TestDrainTickHeldLogCadence(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	f := newMeteredFake()
	ml := addManaged(m, f)
	ml.retiring, ml.retireSince = true, clk.Now()
	ml.users.Store(1)
	m.SetDrainIdle(0)
	for i := 0; i < 80*30; i++ { // 80 minutes
		clk.Advance(healthTick)
		m.drainTick()
	}
	if n := lg.count("held by"); n != 2 {
		t.Fatalf("held log lines over 80 min: %d, want 2 (15m, 1h15m):\n%s", n, lg)
	}
	if lg.count("retiring 15m:") != 1 || lg.count("retiring 1h15m:") != 1 {
		t.Fatalf("held log at the wrong ages:\n%s", lg)
	}
}

// ---- 24: reverse close guards -----------------------------------------------------

// A link that arrives beyond the target is born retiring and never gets users;
// it is closed only once it is at least 4 s old AND the last target drop is at
// least 4 s old, so the exit has learned the lower target and retires the slot
// instead of redialing it. DropLink afterwards stays quiet.
func TestReverseBornRetiringCloseGuards(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 2, 8, true)
	m.setTarget(2) // the warm 8 drops to 2 at t0
	clk.Advance(time.Second)
	a, b, x := newMeteredFake(), newMeteredFake(), newMeteredFake()
	m.AddLink(a, "exit")
	m.AddLink(b, "exit")
	m.AddLink(x, "exit") // t0+1 s
	if mx := entryOf(m, x); mx == nil || !mx.retiring {
		t.Fatal("a link beyond the target was not born retiring")
	}
	if lg.count("spare") != 1 {
		t.Fatalf("born-retiring arrival not logged:\n%s", lg)
	}
	for i := 0; i < 10; i++ {
		if l, rel, _ := m.Pick(); l == x {
			t.Fatal("a born-retiring link got a user")
		} else {
			rel()
		}
	}
	for _, step := range []struct {
		adv  time.Duration
		kept bool
		why  string
	}{
		{2 * time.Second, true, "t0+3 s: the target drop is 3 s old"},
		{1500 * time.Millisecond, true, "t0+4.5 s: the link is only 3.5 s old"},
		{500 * time.Millisecond, false, "t0+5 s: both guards pass"},
	} {
		clk.Advance(step.adv)
		m.drainTick()
		if inPool(m, x) != step.kept {
			t.Fatalf("%s: pooled=%v, want %v", step.why, !step.kept, step.kept)
		}
	}
	v2Wait(t, 2*time.Second, "retire-close", func() bool { return x.closes.Load() == 1 })
	m.DropLink(x, "exit")
	if n := lg.count("down:"); n != 0 {
		t.Fatalf("DropLink logged a retired link as down:\n%s", lg)
	}

	// An old surplus link is still held for 4 s after a fresh target drop.
	y := newMeteredFake()
	m.AddLink(y, "exit")
	clk.Advance(10 * time.Second)
	m.setTarget(3)
	m.setTarget(2) // drop now
	for i, adv := range []time.Duration{0, 3 * time.Second} {
		clk.Advance(adv)
		m.drainTick()
		if !inPool(m, y) {
			t.Fatalf("step %d: closed %s after a target drop", i, adv)
		}
	}
	clk.Advance(time.Second)
	m.drainTick()
	if inPool(m, y) {
		t.Fatal("not closed 4 s after the target drop")
	}
}

// An exit whose own minimum is above the edge's target redials every link the
// edge retires. 3 surplus links arriving within 10 s of a retire-close, inside
// 60 s, stop retire-closes for 10 min (logged once); slower re-arrivals don't.
func TestReverseChurnGuard(t *testing.T) {
	const tripLog = "the exit redials links this server retires"
	setup := func() (*LinkManager, *v2Clock, *v2Log) {
		m, clk, lg := newV2Manager(nil, 2, 8, true)
		m.setTarget(2)
		m.AddLink(newMeteredFake(), "exit")
		m.AddLink(newMeteredFake(), "exit")
		return m, clk, lg
	}
	surplus := func(t *testing.T, m *LinkManager) *meteredFakeLink {
		t.Helper()
		x := newMeteredFake()
		m.AddLink(x, "exit")
		if ml := entryOf(m, x); ml == nil || !ml.retiring {
			t.Fatal("surplus arrival not born retiring")
		}
		return x
	}

	t.Run("trips on the third quick re-arrival", func(t *testing.T) {
		m, clk, lg := setup()
		clk.Advance(5 * time.Second)
		x := surplus(t, m)
		for i := 1; i <= churnTrips; i++ {
			clk.Advance(retireAfterDrop)
			m.drainTick()
			if inPool(m, x) {
				t.Fatalf("cycle %d: surplus link not retire-closed", i)
			}
			clk.Advance(2 * time.Second) // the exit redials it
			x = surplus(t, m)
			trips := lg.count(tripLog)
			if i < churnTrips && trips != 0 {
				t.Fatalf("tripped after only %d re-arrival(s)", i)
			}
			if i == churnTrips && trips != 1 {
				t.Fatalf("did not trip on re-arrival %d:\n%s", i, lg)
			}
		}
		if lg.count("keeping 3 up for 10m") != 1 {
			t.Fatalf("trip log lacks the count and hold:\n%s", lg)
		}
		for k := 1; k <= 30; k++ { // 9.5 min: held
			clk.Advance(19 * time.Second)
			m.drainTick()
			if !inPool(m, x) {
				t.Fatalf("retire-closed %s into the %s churn hold", time.Duration(k)*19*time.Second, churnHold)
			}
		}
		clk.Advance(31 * time.Second)
		m.drainTick()
		if inPool(m, x) {
			t.Fatal("still held after the churn hold expired")
		}
	})

	t.Run("slow re-arrivals do not trip", func(t *testing.T) {
		m, clk, lg := setup()
		clk.Advance(5 * time.Second)
		x := surplus(t, m)
		for i := 1; i <= 5; i++ {
			clk.Advance(retireAfterDrop)
			m.drainTick()
			if inPool(m, x) {
				t.Fatalf("cycle %d: surplus link not retire-closed", i)
			}
			clk.Advance(churnReArrive + time.Second)
			x = surplus(t, m)
		}
		if lg.count(tripLog) != 0 {
			t.Fatalf("tripped on re-arrivals %s after each close:\n%s", churnReArrive+time.Second, lg)
		}
	})
}

// When no link has pool control (the exit refused kindPool: a pre-pool-control
// exit that keeps its own fixed count), the edge never closes links for
// sizing — they would be redialed — and reports the pool as not growable,
// logging it once. One link with pool control lifts both.
func TestReversePoolRefusedNoClosesNotGrowable(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 2, 8, true)
	m.setTarget(2)
	a, b, x := newMeteredFake(), newMeteredFake(), newMeteredFake()
	for _, l := range []Link{a, b, x} {
		m.AddLink(l, "exit")
		m.markPoolRefused(l)
	}
	for i := 0; i < 10; i++ {
		clk.Advance(healthTick)
		m.sampleHealth()
		m.drainTick()
	}
	if !inPool(m, x) {
		t.Fatal("closed a surplus link although the exit has no pool control")
	}
	if m.sample.growable {
		t.Fatal("growable with no link taking pool control")
	}
	if n := lg.count("no pool control"); n != 1 {
		t.Fatalf("no-pool-control log: %d lines, want 1:\n%s", n, lg)
	}
	if T := m.decideTarget(); T > 3 {
		t.Fatalf("target %d with 3 links up and an exit that cannot add links", T)
	}

	m.setTarget(3)
	c := newMeteredFake()
	m.AddLink(c, "exit") // a link on which pool control works
	clk.Advance(healthTick)
	m.sampleHealth()
	if !m.sample.growable {
		t.Fatal("not growable with a link that takes pool control")
	}
	clk.Advance(retireAfterDrop)
	m.drainTick()
	if inPool(m, x) {
		t.Fatal("surplus link not closed once pool control works")
	}
}

// End to end over smux: an exit older than pool control closes the kindPool
// stream (EOF while the link is up) and the edge marks that link refused; a
// current exit, or a link that dies, is not mistaken for a refusal.
func TestOpenPoolCtlDetectsOldExit(t *testing.T) {
	type exitFn func(ctx context.Context, srv *smux.Session, st *smux.Stream)
	run := func(t *testing.T, exit exitFn) (*LinkManager, *managedLink, context.CancelFunc) {
		t.Helper()
		ca, cb := net.Pipe()
		cli, _, err := newSession(ca, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv, _, err := newSession(cb, true, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); cli.Close(); srv.Close() })
		go func() {
			for {
				st, err := srv.AcceptStream()
				if err != nil {
					return
				}
				var kind [1]byte
				if _, err := io.ReadFull(st, kind[:]); err != nil || kind[0] != kindPool {
					st.Close()
					continue
				}
				go exit(ctx, srv, st)
			}
		}()
		m, _, _ := newV2Manager(nil, 2, 8, true)
		l := &ctrlFakeLink{sess: cli, m: &linkMeter{}}
		m.AddLink(l, "exit")
		go openPoolCtl(ctx, l, m.Target, nil, func() { m.markPoolRefused(l) })
		return m, entryOf(m, l), cancel
	}

	t.Run("old exit refuses", func(t *testing.T) {
		m, ml, _ := run(t, func(_ context.Context, _ *smux.Session, st *smux.Stream) { st.Close() })
		v2Wait(t, 3*time.Second, "kindPool refusal noticed", func() bool { return ml.poolRefused.Load() })
		m.sampleHealth()
		if m.sample.growable {
			t.Fatal("growable although the only link's exit refused pool control")
		}
	})
	t.Run("current exit", func(t *testing.T) {
		tp := newTestPool(t, 1, 8)
		m, ml, cancel := run(t, func(ctx context.Context, _ *smux.Session, st *smux.Stream) { servePoolCtl(ctx, st, tp.exitPool) })
		v2Wait(t, 3*time.Second, "target delivered", func() bool {
			tp.exitPool.mu.Lock()
			defer tp.exitPool.mu.Unlock()
			return tp.exitPool.want == m.Target()
		})
		time.Sleep(300 * time.Millisecond)
		cancel() // the edge shutting down closes the stream itself
		time.Sleep(400 * time.Millisecond)
		if ml.poolRefused.Load() {
			t.Fatal("a pool-control-capable exit was marked refused")
		}
	})
	t.Run("link dies", func(t *testing.T) {
		_, ml, _ := run(t, func(_ context.Context, srv *smux.Session, _ *smux.Stream) {
			time.Sleep(100 * time.Millisecond)
			srv.Close()
		})
		time.Sleep(700 * time.Millisecond)
		if ml.poolRefused.Load() {
			t.Fatal("a link that died was taken for a refusal")
		}
	})
}

// ---- Stats ------------------------------------------------------------------------

// Stats counts serving, retiring (with the connections holding them) and
// pressed links live, and keeps Saturated for older readers.
func TestStatsCountsServingRetiring(t *testing.T) {
	m, _, _ := newV2Manager(nil, 2, 8, false)
	p := addManaged(m, newMeteredFake())
	p.pressed = true
	addManaged(m, newMeteredFake())
	r := addManaged(m, newMeteredFake())
	r.retiring, r.open, r.flowing = true, 5, 1
	d := addManaged(m, newMeteredFake())
	d.degraded = true
	dead := newMeteredFake()
	addManaged(m, dead)
	dead.alive.Store(false)
	m.users.Store(9)
	m.target.Store(2)
	m.publishStats()
	st := m.Stats()
	want := PoolStats{Links: 4, Serving: 2, Retiring: 1, HeldBy: 5, HeldActive: 1, Pressed: 1, Saturated: true, Target: 2, Users: 9, Min: 2, Max: 8}
	if st.Links != want.Links || st.Serving != want.Serving || st.Retiring != want.Retiring || st.HeldBy != want.HeldBy ||
		st.HeldActive != want.HeldActive || st.Pressed != want.Pressed || st.Saturated != want.Saturated ||
		st.Target != want.Target || st.Users != want.Users || st.Min != want.Min || st.Max != want.Max {
		t.Fatalf("Stats() = %+v\nwant (subset) %+v", st, want)
	}
}
