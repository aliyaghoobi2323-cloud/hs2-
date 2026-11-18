package engine

import (
	"testing"
	"time"
)

// review-rel69k #1: a link joins the dark set only on its 2nd consecutive
// dark tick; one healthy sample resets the count.
func TestUL268TwoTicks(t *testing.T) {
	type lk struct{ n int }
	a, b := &lk{1}, &lk{2}
	sts := map[any]*ul268Link{a: {}, b: {}}
	acked := map[any]uint64{}
	now := int64(5_000_000) * ms268
	tick := func(i int, frozenA bool) map[any]bool {
		var smp []ul268Sample
		for _, k := range []any{a, b} {
			if k == b || !frozenA {
				acked[k] += 10_000
			}
			p := "pA"
			if k == b {
				p = "pB"
			}
			smp = append(smp, ul268Sample{key: k, path: p, st: sts[k], ok: true,
				ts: tcpStat{bytesAcked: acked[k], unacked: 20, rttUs: 40_000}})
		}
		return ul268DarkSet(smp, now+int64(i)*250*ms268)
	}
	tick(0, false)
	first := -1
	for i := 1; i <= 8; i++ {
		if s := tick(i, true); s[a] && first < 0 {
			first = i
		}
	}
	// frozen since tick 0's ack: judged dark from tick 4 (1 s), in the set at 5
	if first != 5 {
		t.Fatalf("first in set at tick %d, want 5 (2nd judged tick)", first)
	}
	if sts[a].darkN < 2 {
		t.Fatalf("darkN %d", sts[a].darkN)
	}
	tick(9, false)
	if sts[a].darkN != 0 || sts[a].dark {
		t.Fatal("healthy sample must reset the count")
	}
}

// review-rel69k #1: the index lists exactly the conns with a sub on a link;
// a closed/removed sub leaves it.
func TestUL268Index(t *testing.T) {
	m1, m2, m3 := &mConn{iran: true}, &mConn{iran: true}, &mConn{iran: true}
	s1 := &msub{m: m1, key: "L1"}
	s2 := &msub{m: m2, key: "L1"}
	s3 := &msub{m: m2, key: "L2"}
	s4 := &msub{m: m3, key: "L3"}
	for _, s := range []*msub{s1, s2, s3, s4} {
		ul268IdxAdd(s)
		ul268IdxAdd(s) // idempotent
	}
	t.Cleanup(func() {
		for _, s := range []*msub{s1, s2, s3, s4} {
			ul268IdxDel(s)
		}
	})
	got, by := ul268ConnsOn([]any{"L1", "L2"})
	if len(got) != 2 || len(by[m2]) != 2 || len(by[m1]) != 1 || by[m3] != nil {
		t.Fatalf("conns %v by %v", got, by)
	}
	ul268IdxDel(s1)
	ul268IdxDel(s1)
	got, _ = ul268ConnsOn([]any{"L1"})
	if len(got) != 1 || got[0] != m2 {
		t.Fatalf("after del: %v", got)
	}
	ul268IdxDel(s2)
	ul268Idx.mu.Lock()
	_, left := ul268Idx.m["L1"]
	ul268Idx.mu.Unlock()
	if left {
		t.Fatal("empty link entry kept")
	}
}

// review-rel69k #1: a dark-only tick visits only the conns indexed on the
// dark link (not every Iran conn), rescues the one on it, then holds the
// link off; an er92-silent link still runs the full sweep.
func TestUL268TargetedVisit(t *testing.T) {
	old := ul268On.Load()
	ul268On.Store(true)
	t.Cleanup(func() { ul268On.Store(old); ul268DarkOnly.Store(nil); ul268DarkAll.Store(nil); clear(ul268HoldUntil) })
	_, iran, exit := newRescuePair(t)
	_, iran2, exit2 := newRescuePair(t) // another Iran conn, not on the dark link
	a, b := randBytes(4000, 21), randBytes(3000, 22)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
		func() error { return xfer(iran2, b, 1000, 0) },
		func() error { return readN(exit2, b) },
	)
	if !iran.raw1.u268Idx.Load() {
		t.Fatal("Iran raw sub not indexed")
	}
	if exit.raw1 != nil && exit.raw1.u268Idx.Load() {
		t.Fatal("exit sub indexed")
	}
	k := iran.raw1.key
	set := map[any]bool{k: true}
	only := map[any]bool{k: true}
	ul268DarkOnly.Store(&only)
	ul268DarkAll.Store(&only)
	// a conn on another link must not be visited (the test net may share
	// one key between pairs: expect exactly the conns indexed on k)
	other := &msub{m: &mConn{iran: true}, key: "ul268-other-link"}
	ul268IdxAdd(other)
	t.Cleanup(func() { ul268IdxDel(other) })
	on, _ := ul268ConnsOn([]any{k})
	want := uint64(len(on))
	// b279: in a -run set, conns an earlier test left open (b276's leaked
	// conn loops) may be indexed on the shared test key too: they are
	// visited as well (want counts them); only iran must be among them.
	mine := false
	for _, c := range on {
		mine = mine || c == iran
	}
	if want == 0 || !mine {
		t.Fatalf("indexed on k: %d (iran among them: %v)", want, mine)
	}
	if want > 2 {
		t.Logf("indexed on k: %d (conns left by earlier tests)", want)
	}
	v0, c0, r0, f0 := ul268Scale.visits.Load(), ul268Scale.conns.Load(), ul268Stat.rescued.Load(), ul268Scale.full.Load()
	rescueSilentLinks(set)
	if ul268Scale.full.Load() != f0 {
		t.Fatal("dark-only tick ran the full sweep")
	}
	if ul268Scale.visits.Load() != v0+1 || ul268Scale.conns.Load() != c0+want {
		t.Fatalf("visited %d conns, want %d", ul268Scale.conns.Load()-c0, want)
	}
	if ul268Stat.rescued.Load() < r0+1 {
		t.Fatal("the conn on the dark link was not rescued")
	}
	if ul268HoldUntil[k] == 0 {
		t.Fatal("no hold-off after the rescue")
	}
	h0 := ul268Scale.held.Load()
	rescueSilentLinks(set)
	if ul268Scale.held.Load() != h0+1 || ul268Scale.conns.Load() != c0+want {
		t.Fatal("held link visited again")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		iran.mu.Lock()
		done := iran.rscPhase == 0 && !iran.lightOpening
		iran.mu.Unlock()
		iran2.mu.Lock()
		done = done && iran2.rscPhase == 0 && !iran2.lightOpening
		iran2.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// er92 silent link (not dark-only): full sweep, trigger once per episode
	ul268DarkOnly.Store(nil)
	t0 := rescueStat.trigger.Load()
	rescueSilentLinks(map[any]bool{"er92-x": true})
	rescueSilentLinks(map[any]bool{"er92-x": true})
	if ul268Scale.full.Load() != f0+2 || rescueStat.trigger.Load() != t0+1 {
		t.Fatalf("er92: full %d trigger %d", ul268Scale.full.Load()-f0, rescueStat.trigger.Load()-t0)
	}
	rescueSilentLinks(nil)
	closeAndWait(t, iran, exit)
	closeAndWait(t, iran2, exit2)
}

// review-rel69k #2: the cover decision uses the link's real outstanding
// bytes: a ring that dropped bytes covers only while it holds the socket's
// queue plus the margin; budget refusals do not stick.
func TestUL268CoverOutq(t *testing.T) {
	saveF := ul268OutqF
	t.Cleanup(func() { ul268OutqF = saveF })
	q, qok := uint64(0), true
	ul268OutqF = func(*msub) (uint64, bool) { return q, qok }
	m := &mConn{iran: true}
	m.raw1 = &msub{m: m}
	m.ring = rescueRing{on: true, u268: true}
	for i := 0; i < 6; i++ {
		m.ring.add(make([]byte, 64<<10)) // slow-looking writer: ring stays at rescueRingCap
	}
	held := int(m.ring.uHeld.Load())
	if held != m.ring.n || int64(held) >= m.ring.uTotA.Load() {
		t.Fatalf("held %d n %d tot %d: want a ring that dropped bytes", held, m.ring.n, m.ring.uTotA.Load())
	}
	q = uint64(held - ul268CovMargin - 1)
	if !m.ul268CoverNowLocked() {
		t.Fatal("ring holds the socket queue + margin: must cover")
	}
	q = 4 << 20 // a frozen socket with MBs queued
	if m.ul268CoverNowLocked() {
		t.Fatal("socket holds more than the ring: must not cover (would reset the conn)")
	}
	qok = false
	o0 := ul268Scale.outqNo.Load()
	_ = m.ul268CoverNowLocked()
	if ul268Scale.outqNo.Load() != o0+1 {
		t.Fatal("unreadable queue not counted")
	}
	// a ring holding every byte covers whatever the queue says
	s := &mConn{iran: true}
	s.raw1 = &msub{m: s}
	s.ring = rescueRing{on: true, u268: true}
	s.ring.add(make([]byte, 5000))
	q, qok = 1<<30, true
	if !s.ul268CoverNowLocked() {
		t.Fatal("holds every byte: covers")
	}
	// budget refusal: cleared by release
	saveB := ul268MemMaxFix
	ul268MemMaxFix = 1
	r := rescueRing{on: true, u268: true}
	r.uRate = ul268FastBps * 64
	for i := 0; i < 8; i++ {
		r.add(make([]byte, 64<<10))
	}
	ul268MemMaxFix = saveB
	if !r.uOver {
		t.Fatal("budget refusal not marked")
	}
	r.release()
	if r.uOver || r.uRate != 0 || r.uHeld.Load() != 0 {
		t.Fatal("release must clear the refusal and the rate window")
	}
	if r.uTot == 0 {
		t.Fatal("uTot must keep counting across a release")
	}
	// a later growth clears it
	r2 := rescueRing{on: true, u268: true, uOver: true}
	r2.uRate = ul268FastBps * 4
	r2.buf = make([]byte, rescueRingCap())
	rescueStat.mem.Add(int64(rescueRingCap()))
	r2.uT0 = monoNano(time.Now()) - int64(300*time.Millisecond)
	r2.uN = ul268FastBps * 4
	r2.add(make([]byte, rescueRingCap()))
	if len(r2.buf) > rescueRingCap() && r2.uOver {
		t.Fatal("a successful growth must clear uOver")
	}
	r2.release()
}

// review-rel69k #7: ul268's dark-only rescue and darkcall's lite switch
// exclude each other per conn for ul268Excl.
func TestUL268DarkcallExclusive(t *testing.T) {
	m := &mConn{iran: true}
	now := time.Now()
	if m.ul268DcRecentLocked(now) || m.ul268RecentLocked(now) {
		t.Fatal("fresh conn")
	}
	m.dcSwAt = now.Add(-ul268Excl / 2)
	if !m.ul268DcRecentLocked(now) {
		t.Fatal("darkcall switched recently: ul268 must leave it")
	}
	if m.ul268DcRecentLocked(now.Add(ul268Excl)) {
		t.Fatal("exclusion must end")
	}
	m.u268At = now
	if !m.ul268RecentLocked(now.Add(ul268Excl / 2)) {
		t.Fatal("ul268 rescued recently: darkcall must leave it")
	}
}

// review-rel69k #7 end to end: a raw conn on a dark-only link that darkcall
// just switched is not rescued by ul268 (dcskip counted).
func TestUL268SkipAfterDarkcall(t *testing.T) {
	old := ul268On.Load()
	ul268On.Store(true)
	t.Cleanup(func() { ul268On.Store(old); ul268DarkOnly.Store(nil); ul268DarkAll.Store(nil); clear(ul268HoldUntil) })
	_, iran, exit := newRescuePair(t)
	a := randBytes(4000, 31)
	both(t,
		func() error { return xfer(iran, a, 1000, 0) },
		func() error { return readN(exit, a) },
	)
	k := iran.raw1.key
	only := map[any]bool{k: true}
	ul268DarkOnly.Store(&only)
	iran.mu.Lock()
	iran.dcSwAt = time.Now()
	iran.mu.Unlock()
	d0, r0 := ul268Scale.dcSkip.Load(), ul268Stat.rescued.Load()
	on, _ := ul268ConnsOn([]any{k})
	rescueSilentLinks(map[any]bool{k: true})
	iran.mu.Lock()
	rescued := !iran.u268At.IsZero()
	iran.mu.Unlock()
	if len(on) <= 1 && (ul268Scale.dcSkip.Load() != d0+1 || ul268Stat.rescued.Load() != r0) ||
		ul268Scale.dcSkip.Load() < d0+1 || rescued {
		// b279: conns an earlier test left open on the shared test key may
		// be rescued too; exact counts only when iran is alone on k
		t.Fatal("ul268 rescued a conn darkcall just switched")
	}
	rescueSilentLinks(nil)
	closeAndWait(t, iran, exit)
}

// the lock-free pre-filter: a conn with no ring bytes within rescueKeep is
// skipped; a fresh raw or records ring is visited.
func TestUL268RecentPrefilter(t *testing.T) {
	m := &mConn{iran: true}
	now := monoNano(time.Now())
	if m.ul268Recent(now) {
		t.Fatal("no ring bytes: not recent")
	}
	m.ring.last.Store(now - int64(rescueKeep) - 1)
	if m.ul268Recent(now) {
		t.Fatal("stale raw ring")
	}
	m.rk.last.Store(now - int64(time.Second))
	if !m.ul268Recent(now) {
		t.Fatal("fresh records ring")
	}
	m.rk.last.Store(0)
	m.ring.last.Store(now)
	if !m.ul268Recent(now) {
		t.Fatal("fresh raw ring")
	}
}

// ul268i: a bulk conn is never a rescue candidate (raw needs rawW, records
// needs planeLight): its subs stay out of the link index, and a switch to
// bulk drops the light subs it had (smaller index, fewer dark-pass locks).
func TestUL268IndexSkipsBulk(t *testing.T) {
	old := ul268On.Load()
	ul268On.Store(true)
	t.Cleanup(func() { ul268On.Store(old) })
	m := newMConn(268_001, true, nil)
	m.mode = planeLight
	in := func(s *msub) bool {
		ul268Idx.mu.Lock()
		defer ul268Idx.mu.Unlock()
		_, ok := ul268Idx.m[s.key][s]
		return ok
	}
	m.mu.Lock()
	s1 := m.addSubLocked(&bigRW{}, any("IB1"), false)
	sb := m.addSubLocked(&bigRW{}, any("IB2"), true)
	m.mu.Unlock()
	t.Cleanup(func() { ul268IdxDel(s1); ul268IdxDel(sb) })
	if !in(s1) || in(sb) {
		t.Fatalf("light sub indexed %v (want true), bulk sub indexed %v (want false)", in(s1), in(sb))
	}
	m.mu.Lock()
	m.switchLocked(sb)
	s3 := m.addSubLocked(&bigRW{}, any("IB3"), false)
	m.mu.Unlock()
	t.Cleanup(func() { ul268IdxDel(s3) })
	if in(s1) || in(s3) {
		t.Fatalf("after the switch to bulk: s1 indexed %v, a later sub indexed %v (want false, false)", in(s1), in(s3))
	}
}
