package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// fx212Keep puts n bytes of kept chunks on s the way keepSentLocked counts
// them (sub-resend-only connection).
func fx212Keep(m *mConn, s *msub, n int) {
	c := getChunk(mRecData)
	c.n = n
	c.refs.Store(1)
	s.sent = append(s.sent, sentRec{c: c, t: time.Now(), sub: true})
	m.keptBytes += n
	keepStallStat.kept.Add(int64(n))
	subKeptAdd(int64(n))
}

// fx212 (audit211 H2): kept chunks of subs a records rescue abandoned are
// released (at the abandon, at their death, at the connection's end): the
// process-global keep gauges come back to where they were.
func TestFx212AbandonedKeptReleased(t *testing.T) {
	k0, s0 := keepStallStat.kept.Load(), subResendStat.kept.Load()
	m := &mConn{mode: planeLight, die: make(chan struct{})}
	a, b, c := &msub{m: m, idx: 0}, &msub{m: m, idx: 1}, &msub{m: m, idx: 2}
	m.subs = []*msub{a, b, c}
	m.mu.Lock()
	fx212Keep(m, a, 1000)
	fx212Keep(m, b, 2000)
	fx212Keep(m, c, 3000)
	// a: abandoned by a rescue (the rescue_rec site)
	a.retired, a.rrAband = true, true
	m.removeSubLocked(a)
	m.abandonSentLocked(a)
	if len(a.sent) != 0 || m.keptBytes != 5000 {
		t.Fatalf("abandon: sent %d kept %d", len(a.sent), m.keptBytes)
	}
	if m.stripeKeepOnLocked(a) {
		t.Fatal("an abandoned sub keeps")
	}
	// b: abandoned without the release (an older path), parked in rrOld,
	// then dies: subGone's rrAband branch releases
	b.retired, b.rrAband = true, true
	m.removeSubLocked(b)
	m.rrOld = append(m.rrOld, b)
	m.mu.Unlock()
	m.subGone(b, nil)
	m.mu.Lock()
	if len(b.sent) != 0 || m.keptBytes != 3000 {
		t.Fatalf("death: sent %d kept %d", len(b.sent), m.keptBytes)
	}
	// c: abandoned and parked, the connection ends while it lives
	c.retired, c.rrAband = true, true
	m.removeSubLocked(c)
	m.rscAband = append(m.rscAband, c)
	m.releaseSentLocked()
	m.mu.Unlock()
	if m.keptBytes != 0 || len(c.sent) != 0 {
		t.Fatalf("end: kept %d", m.keptBytes)
	}
	if k, s := keepStallStat.kept.Load(), subResendStat.kept.Load(); k != k0 || s != s0 {
		t.Fatalf("global gauges drifted: keepstall %d -> %d, sub %d -> %d", k0, k, s0, s)
	}
}

// fx212 (an210 Q2): the cap is at most 3/4 of the records ring.
func TestFx212DarkcallCapFor(t *testing.T) {
	if got := dcCapFor(256<<10, false, 0); got != 256<<10 {
		t.Fatalf("ring off: %d", got)
	}
	if got := dcCapFor(256<<10, true, 0); got != min(256<<10, rrRingCap*3/4) {
		t.Fatalf("ring empty: %d", got)
	}
	if got := dcCapFor(256<<10, true, 1<<20); got != 256<<10 {
		t.Fatalf("big ring: %d", got)
	}
}

// fx212: healthy links (ack progress every look) are never capped, even far
// over the cap by the smux marks (the peer reports consumption only every
// win/div bytes); two links without ack progress are not both capped.
func TestFx212DarkcallCapNeedsStall(t *testing.T) {
	setDarkcall(t)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	var unc atomic.Uint32
	unc.Store(uint32(darkcallCapMin) + 300<<10)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return unc.Load(), 0, true })
	dcVisit(exit, 3)
	exit.mu.Lock()
	live := exit.rrLiveSubsLocked()
	for _, s := range live {
		if s.dc.full {
			exit.mu.Unlock()
			t.Fatalf("healthy sub %d capped", s.idx)
		}
	}
	for _, s := range live {
		f.set(s, "lossy")
	}
	exit.mu.Unlock()
	u0 := darkcallStat.uncapAll.Load()
	dcVisit(exit, 3)
	exit.mu.Lock()
	n := 0
	for _, s := range live {
		if s.dc.full {
			n++
		}
	}
	exit.mu.Unlock()
	if n == 0 || n >= len(live) {
		t.Fatalf("capped %d of %d (want some, never all)", n, len(live))
	}
	if darkcallStat.uncapAll.Load() == u0 {
		t.Fatal("uncapall not counted")
	}
	closeAndWait(t, iran, exit)
}

// fx212 (an210 Q1): a download's DL_COPIES open takes a copies' link only if
// no light primary rides it; an upload's open is unchanged, and links
// without copies keep their old rule (the download still stripes there).
func TestFx212DlCopiesNoPrimary(t *testing.T) {
	old, oldS, oldC, oldF, oldP := stripeK.Load(), stripeShare, stripeShareCopies.Load(), stripeLinkCopies, stripeLinkPrims
	t.Cleanup(func() {
		stripeK.Store(old)
		stripeShare = oldS
		stripeShareCopies.Store(oldC)
		stripeLinkCopies = oldF
		stripeLinkPrims = oldP
	})
	stripeK.Store(4)
	stripeShare = true
	stripeShareCopies.Store(true)
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	copies := map[Link]int{Link(ls[0]): 1, Link(ls[1]): 3, Link(ls[2]): 2}
	prims := map[Link]int{Link(ls[0]): 2, Link(ls[1]): 0, Link(ls[2]): 1}
	stripeLinkCopies = func(l Link) int { return copies[l] }
	stripeLinkPrims = func(l Link) int { return prims[l] }
	pick := func(avoid ...any) (*managedLink, stripeShareWhy) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.pickStripeSharedWhyLocked(avoid, m.exitInfo.Load())
	}
	// upload: fewest copies first (link 0), primaries do not matter
	if ml, _ := pick(stripeCopyUpHint{}); ml != m.links[0] {
		t.Fatalf("upload got %v, want link 0", ml)
	}
	// download: only link 1 (copies only)
	ml, why := pick(stripeCopyUpHint{}, stripeCopyDlHint{})
	if ml != m.links[1] || why.dlPrim != 2 {
		t.Fatalf("download got %v why=%+v, want link 1, dlprim 2", ml, why)
	}
	// no copies-only link: none on a copies' link
	prims[Link(ls[1])] = 1
	if ml, why = pick(stripeCopyUpHint{}, stripeCopyDlHint{}); ml != nil || why.dlPrim != 3 {
		t.Fatalf("download got %v why=%+v, want none", ml, why)
	}
	// a link without copies keeps its old rule: the download stripes there
	copies[Link(ls[2])] = 0
	if ml, _ = pick(stripeCopyUpHint{}, stripeCopyDlHint{}); ml != m.links[2] {
		t.Fatalf("download got %v, want link 2 (no copies)", ml)
	}
	// key off: as before
	oldK := fx212DlPrim
	fx212DlPrim = false
	t.Cleanup(func() { fx212DlPrim = oldK })
	copies[Link(ls[2])] = 2
	if ml, _ = pick(stripeCopyUpHint{}, stripeCopyDlHint{}); ml != m.links[0] {
		t.Fatalf("key off got %v, want link 0", ml)
	}
}

// fx212: the download hint rides with the copies hint on a download's
// opens only.
func TestFx212DlHint(t *testing.T) {
	oldC, oldD, oldU := stripeShareCopies.Load(), stripeDlCopies.Load(), stripeUpRawCopies.Load()
	t.Cleanup(func() { stripeShareCopies.Store(oldC); stripeDlCopies.Store(oldD); stripeUpRawCopies.Store(oldU) })
	stripeShareCopies.Store(true)
	stripeDlCopies.Store(true)
	stripeUpRawCopies.Store(true)
	m := &mConn{iran: true}
	if a := m.stripeRawCopyHintLocked(nil); !stripeCopyUp(a) || !stripeCopyDl(a) {
		t.Fatalf("download raw: %v", a)
	}
	if a := m.stripeCopyUpHintLocked(nil); !stripeCopyUp(a) || !stripeCopyDl(a) {
		t.Fatalf("download grow: %v", a)
	}
	m.stGrowUp = true
	if a := m.stripeRawCopyHintLocked(nil); !stripeCopyUp(a) || stripeCopyDl(a) {
		t.Fatalf("upload raw: %v", a)
	}
	if a := m.stripeCopyUpHintLocked(nil); !stripeCopyUp(a) || stripeCopyDl(a) {
		t.Fatalf("upload grow: %v", a)
	}
}

func fx212SubKeys(t *testing.T) {
	oldR, oldS, oldK, oldL := stripeResend.Load(), mconnSubResend.Load(), keepStallOn.Load(), subLinkAck
	stripeResend.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	t.Cleanup(func() {
		stripeResend.Store(oldR)
		mconnSubResend.Store(oldS)
		keepStallOn.Store(oldK)
		subLinkAck = oldL
	})
}

// fx212 (an210 Q1 #2a): a given-up sub of a sub-resend connection resends
// at its death only if it may hold undelivered bytes.
func TestFx212SubUndelivered(t *testing.T) {
	fx212SubKeys(t)
	la, lok := linkAck{srtt: 50 * time.Millisecond}, true
	subLinkAck = func(*msub) (linkAck, bool) { return la, lok }
	m := &mConn{mode: planeLight, iran: true}
	s := &msub{m: m, closeHS: true}
	m.subs = []*msub{s}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.callUncleanLocked(s) {
		t.Fatal("nothing kept, nothing queued: unclean")
	}
	fx212Keep(m, s, 500)
	s.sent[0].t = now.Add(-2 * time.Second)
	if m.callUncleanLocked(s) {
		t.Fatal("old chunk, link fine: unclean")
	}
	la.unacked = 10
	if m.subUndeliveredLocked(s, now) {
		t.Fatal("2 s old with in flight, no RTO loss: undelivered")
	}
	s.sent[0].t = now.Add(-100 * time.Millisecond)
	if !m.subUndeliveredLocked(s, now) {
		t.Fatal("young chunk with bytes in flight: delivered")
	}
	s.sent[0].t = now.Add(-2 * time.Second)
	la.rtoLoss = true
	if !m.callUncleanLocked(s) {
		t.Fatal("RTO loss: clean")
	}
	la.rtoLoss, lok = false, false
	if !m.subUndeliveredLocked(s, now) {
		t.Fatal("link unknown: delivered")
	}
	lok = true
	c := getChunk(mRecData)
	c.refs.Store(1)
	s.q = []*mchunk{c}
	if !m.subUndeliveredLocked(s, now) {
		t.Fatal("queued chunk: delivered")
	}
	s.q = nil
	c.unref()
	old := fx212SubResend.Load()
	fx212SubResend.Store(false)
	if !m.callUncleanLocked(s) {
		t.Fatal("key off: the r68fix2 rule")
	}
	fx212SubResend.Store(old)
	m.releaseSentLocked()
}

// fx212 (#2d): chunks the target carries itself are not queued again.
func TestFx212SkipHeld(t *testing.T) {
	a, b, c, d := getChunk(mRecData), getChunk(mRecData), getChunk(mRecData), getChunk(mRecData)
	x := &msub{q: []*mchunk{a}, sent: []sentRec{{c: b}}, cur: c}
	s0 := fx212Stat.skipped.Load()
	out := fx212SkipHeld([]*mchunk{a, b, c, d}, x)
	if len(out) != 1 || out[0] != d || fx212Stat.skipped.Load() != s0+3 {
		t.Fatalf("got %d chunks", len(out))
	}
}

// fx212 (#2b, #2c): a sub-resend connection's keep poll runs in the shared
// sweeper (not its manager), reads the link without m.mu, trims what is
// delivered, and leaves the sweeper once nothing is kept; the kept max is
// noted per tick, not per write.
func TestFx212SubSweeper(t *testing.T) {
	fx212SubKeys(t)
	m := &mConn{mode: planeLight, kick: make(chan struct{}, 1)}
	s := &msub{m: m}
	m.subs = []*msub{s}
	unlocked := true
	subLinkAck = func(*msub) (linkAck, bool) {
		if !m.mu.TryLock() {
			unlocked = false
		} else {
			m.mu.Unlock()
		}
		return linkAck{srtt: 10 * time.Millisecond}, true
	}
	mx0 := subResendStat.keptMax.Swap(0)
	t.Cleanup(func() { subResendStat.keptMax.Store(mx0) })
	m.mu.Lock()
	fx212Keep(m, s, 4000)
	if subResendStat.keptMax.Load() != 0 {
		m.mu.Unlock()
		t.Fatal("keptMax moved per write")
	}
	m.keepIdleArmLocked()
	swept, idle := m.ksSwept, m.ksIdle
	s.sent[0].t = time.Now().Add(-time.Second)
	m.mu.Unlock()
	select {
	case <-m.kick:
		t.Fatal("the manager was kicked")
	default:
	}
	if !swept || idle {
		t.Fatalf("swept %v idle %v", swept, idle)
	}
	k0 := subResendStat.kept.Load()
	subSweepTick(time.Now())
	if !unlocked {
		t.Fatal("TCP_INFO read under m.mu")
	}
	if subResendStat.keptMax.Load() < k0 {
		t.Fatal("keptMax not noted per tick")
	}
	m.mu.Lock()
	kept, swept := m.keptBytes, m.ksSwept
	m.mu.Unlock()
	if kept != 0 || swept {
		t.Fatalf("after the tick: kept %d swept %v", kept, swept)
	}
	if subSweepHas(m) {
		t.Fatal("still in the sweeper")
	}
}

// fx212 (audit211 M2): the write-path age trim (no TCP look) opens a gap;
// only the TCP look heals it.
func TestFx212AgeTrimGap(t *testing.T) {
	fx212SubKeys(t)
	oldF := b449KeepFloor.Load()
	b449KeepFloor.Store(0) // b449c: the floor holds a small connection's chunks (mconn_b449_floor_test.go)
	t.Cleanup(func() { b449KeepFloor.Store(oldF) })
	la := linkAck{srtt: 10 * time.Millisecond}
	subLinkAck = func(*msub) (linkAck, bool) { return la, true }
	m := &mConn{mode: planeLight}
	s := &msub{m: m}
	m.subs = []*msub{s}
	now := time.Now()
	s.lastRecv.Store(monoNano(now))
	m.mu.Lock()
	defer m.mu.Unlock()
	fx212Keep(m, s, 700)
	s.sent[0].t = now.Add(-2 * time.Second) // older than subKeepAge (no srtt yet)
	g0 := subResendStat.ageGap.Load()
	m.subAgeTrimLocked(now, false)
	if m.keptBytes != 0 || s.subGapT.IsZero() || subResendStat.ageGap.Load() != g0+1 {
		t.Fatalf("kept %d gap %v", m.keptBytes, s.subGapT)
	}
	m.subAgeTrimLocked(now, false)
	if s.subGapT.IsZero() {
		t.Fatal("healed without TCP evidence")
	}
	la.rtoLoss = true
	m.subAgeTrimLocked(now, true)
	if s.subGapT.IsZero() {
		t.Fatal("healed in an RTO loss")
	}
	la.rtoLoss = false
	m.subAgeTrimLocked(now, true)
	if !s.subGapT.IsZero() {
		t.Fatal("not healed by the TCP look")
	}
}
