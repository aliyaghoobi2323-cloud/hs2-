package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// meteredFakeLink is a fakeLink carrying a linkMeter plus a test-controlled
// retransmit counter, so the manager's loss-based health logic can be driven
// deterministically without real sockets. The pool tests also set its open
// stream count, its TCP_INFO chrono counters and its per-stream activity.
type meteredFakeLink struct {
	alive   atomic.Bool
	m       *linkMeter
	retrans atomic.Uint64
	act     atomic.Int32 // Active(): user streams open
	closes  atomic.Int32 // Close calls

	mu sync.Mutex
	ts tcpStat   // TCP_INFO besides retrans (setTCP)
	fs *flowSnap // flowStats result (setFlows); nil: open = Active(), none flowing
}

func newMeteredFake() *meteredFakeLink {
	f := &meteredFakeLink{m: &linkMeter{}}
	f.alive.Store(true)
	return f
}
func (f *meteredFakeLink) OpenStream() (stream, error) { return nil, nil }
func (f *meteredFakeLink) Active() int32               { return f.act.Load() }
func (f *meteredFakeLink) Alive() bool                 { return f.alive.Load() }
func (f *meteredFakeLink) Close() error {
	f.closes.Add(1)
	f.alive.Store(false)
	return nil
}
func (f *meteredFakeLink) meter() *linkMeter { return f.m }
func (f *meteredFakeLink) tcpStats() (tcpStat, bool) {
	f.mu.Lock()
	st := f.ts
	f.mu.Unlock()
	st.retrans = f.retrans.Load()
	return st, true
}

// setTCP sets the chrono part of the fake's TCP_INFO (retrans stays separate).
func (f *meteredFakeLink) setTCP(st tcpStat) {
	f.mu.Lock()
	f.ts = st
	f.mu.Unlock()
}

// setFlows fixes what the fake reports as its per-stream activity.
func (f *meteredFakeLink) setFlows(fs flowSnap) {
	f.mu.Lock()
	f.fs = &fs
	f.mu.Unlock()
}

func (f *meteredFakeLink) flowStats(now time.Time, dt, recent time.Duration) flowSnap {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fs == nil {
		return flowSnap{open: int(f.act.Load())}
	}
	return *f.fs
}

// active simulates one health interval of UPLOAD traffic: `bytes` sent and `rt`
// local (upload-path) retransmits added.
func (f *meteredFakeLink) active(bytes, rt uint64) {
	f.m.wrBytes.Add(bytes)
	f.retrans.Add(rt)
}

// download simulates one interval of DOWNLOAD traffic with `rt` peer-reported
// (download-path) retransmits, as the control channel would deliver them.
func (f *meteredFakeLink) download(bytes, rt uint64) {
	f.m.rdBytes.Add(bytes)
	f.m.peerRetrans.Add(rt)
	f.m.peerSeen.Store(true)
}

type fakeDialer struct{ dials atomic.Int32 }

func (d *fakeDialer) DialLink(ctx context.Context) (Link, error) {
	d.dials.Add(1)
	return newMeteredFake(), nil
}

// A degraded link must be skipped for new users, unless every link is degraded.
func TestPickSkipsDegraded(t *testing.T) {
	m := NewLinkManager(nil, 3, 3, 50, nil)
	a, b, c := newMeteredFake(), newMeteredFake(), newMeteredFake()
	m.links = []*managedLink{
		{link: a, mtr: a.m},
		{link: b, mtr: b.m, degraded: true},
		{link: c, mtr: c.m},
	}
	for i := 0; i < 200; i++ {
		l, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no link")
		}
		if l == b {
			t.Fatal("degraded link b was chosen for a new user")
		}
		rel()
	}
	m.links[0].degraded, m.links[2].degraded = true, true
	if _, _, ok := m.Pick(); !ok {
		t.Fatal("fallback failed: no link when all are degraded")
	}
}

// An ACTIVE link with high retransmit loss is flagged degraded, then healed
// make-before-break: a replacement is dialed before it is dropped, and its users
// are never yanked. Healthy active links are left alone.
func TestDegradeAndHeal(t *testing.T) {
	d := &fakeDialer{}
	m := NewLinkManager(d, 4, 8, 50, nil)
	m.gate = instantGate()
	good := []*meteredFakeLink{newMeteredFake(), newMeteredFake(), newMeteredFake()}
	bad := newMeteredFake()
	for _, g := range good {
		ml := &managedLink{link: g, mtr: g.m}
		ml.users.Store(5)
		m.links = append(m.links, ml)
	}
	badML := &managedLink{link: bad, mtr: bad.m}
	badML.users.Store(5)
	m.links = append(m.links, badML)

	// ~200 KiB/sample ≈ 146 packets; 30 retrans on the bad link ≈ 20% loss (> 12%).
	step := func() {
		for _, g := range good {
			g.active(200<<10, 0)
		}
		bad.active(200<<10, 30)
		m.sampleHealth()
	}
	step() // seed
	for i := 0; i < degradeStreak; i++ {
		step()
	}
	if !badML.degraded {
		t.Fatalf("high-loss link not flagged degraded (streak=%d)", badML.lowStreak)
	}
	for i := range good {
		if m.links[i].degraded {
			t.Fatalf("healthy active link %d wrongly flagged degraded", i)
		}
	}

	before := len(m.links)
	m.heal(context.Background())
	settle(m)
	if d.dials.Load() != 1 {
		t.Fatalf("expected 1 replacement dial (make-before-break), got %d", d.dials.Load())
	}
	if !badML.draining {
		t.Fatal("degraded link not marked draining")
	}
	if !bad.Alive() {
		t.Fatal("degraded link with users was closed prematurely")
	}
	if len(m.links) != before+1 {
		t.Fatalf("replacement not added: have %d, want %d", len(m.links), before+1)
	}

	badML.users.Store(0)
	m.heal(context.Background())
	settle(m)
	for _, ml := range m.links {
		if ml == badML {
			t.Fatal("drained link was not retired")
		}
	}
	if bad.Alive() {
		t.Fatal("retired link not closed")
	}
}

// An IDLE link (below the activity gate) must NOT be degraded even with high loss
// ratio — its low throughput is its users being quiet, not a bad link.
func TestNoDegradeIdleLink(t *testing.T) {
	m := NewLinkManager(nil, 2, 2, 50, nil)
	idle := newMeteredFake()
	im := &managedLink{link: idle, mtr: idle.m}
	im.users.Store(5)
	m.links = []*managedLink{im}
	for i := 0; i < 6; i++ {
		idle.active(1<<10, 50) // tiny traffic, lots of retrans ratio, but under activeBytes
		m.sampleHealth()
	}
	if im.degraded {
		t.Fatal("idle link degraded despite being below the activity gate")
	}
}

// Phase 3: a link that is lossy only in the DOWNLOAD direction (invisible to the
// local upload-path retransmit signal) is degraded via the peer's retransmits
// delivered by the control channel.
func TestDegradeOnDownloadLoss(t *testing.T) {
	m := NewLinkManager(&fakeDialer{}, 4, 8, 50, nil)
	good := []*meteredFakeLink{newMeteredFake(), newMeteredFake(), newMeteredFake()}
	bad := newMeteredFake()
	for _, g := range good {
		gm := &managedLink{link: g, mtr: g.m}
		gm.users.Store(5)
		m.links = append(m.links, gm)
	}
	badML := &managedLink{link: bad, mtr: bad.m}
	badML.users.Store(5)
	m.links = append(m.links, badML)

	step := func() {
		for _, g := range good {
			g.download(200<<10, 0) // clean download
		}
		bad.download(200<<10, 30) // ~20% download loss
		m.sampleHealth()
	}
	step()
	for i := 0; i < degradeStreak; i++ {
		step()
	}
	if !badML.degraded {
		t.Fatalf("download-lossy link not degraded (streak=%d)", badML.lowStreak)
	}
	for i := range good {
		if m.links[i].degraded {
			t.Fatalf("clean-download link %d wrongly degraded", i)
		}
	}
}

// An active link with zero loss is never degraded.
func TestNoDegradeHealthy(t *testing.T) {
	m := NewLinkManager(nil, 2, 2, 50, nil)
	h := newMeteredFake()
	hm := &managedLink{link: h, mtr: h.m}
	hm.users.Store(5)
	m.links = []*managedLink{hm}
	for i := 0; i < 6; i++ {
		h.active(500<<10, 0)
		m.sampleHealth()
	}
	if hm.degraded {
		t.Fatal("healthy active link wrongly degraded")
	}
}
