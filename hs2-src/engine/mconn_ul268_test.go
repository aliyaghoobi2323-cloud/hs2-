package engine

import (
	"testing"
	"time"
)

const ms268 = int64(time.Millisecond)

// TestUL268JudgeTrigger: frozen acks with unacked data trigger after
// max(1 s, 8 x srtt); progress, nothing outstanding or a sampling gap never.
func TestUL268JudgeTrigger(t *testing.T) {
	ts := tcpStat{bytesAcked: 1000, unacked: 10, rttUs: 50_000}
	var st ul268Link
	now := int64(1_000_000) * ms268
	if ul268Judge(&st, ts, now) {
		t.Fatal("first sample primes only")
	}
	for i := 1; i <= 3; i++ { // 750 ms frozen
		if ul268Judge(&st, ts, now+int64(i)*250*ms268) {
			t.Fatalf("dark after %d ms", i*250)
		}
	}
	if !ul268Judge(&st, ts, now+1000*ms268) {
		t.Fatal("1 s frozen with unacked data and rtt 50 ms: want dark")
	}
	ts.bytesAcked += 1448 // ack progress resets
	if ul268Judge(&st, ts, now+1250*ms268) {
		t.Fatal("progress: not dark")
	}
	// long rtt: 8 x 300 ms = 2.4 s
	st = ul268Link{}
	ts = tcpStat{bytesAcked: 5, unacked: 3, rttUs: 300_000}
	ul268Judge(&st, ts, now)
	for d := int64(250); d < 2400; d += 250 {
		if ul268Judge(&st, ts, now+d*ms268) {
			t.Fatalf("rtt 300 ms: dark at %d ms", d)
		}
	}
	if !ul268Judge(&st, ts, now+2500*ms268) {
		t.Fatal("rtt 300 ms: want dark at 2.5 s")
	}
	// backoff >= 2 lowers the bar to 1 s
	st = ul268Link{}
	ts.backoff = 2
	ul268Judge(&st, ts, now)
	for d := int64(250); d <= 1000; d += 250 {
		if got := ul268Judge(&st, ts, now+d*ms268); got != (d >= 1000) {
			t.Fatalf("backoff 2 at %d ms: dark=%v", d, got)
		}
	}
	// nothing outstanding: an idle link is never dark
	st = ul268Link{}
	idle := tcpStat{bytesAcked: 7, rttUs: 1000}
	var d int64
	for d = 0; d < 10_000; d += 250 {
		if ul268Judge(&st, idle, now+d*ms268) {
			t.Fatal("idle link dark")
		}
	}
	// new data right after an idle stretch: the clock starts at the last
	// sample with nothing outstanding
	busy := idle
	busy.unacked = 4
	if ul268Judge(&st, busy, now+d*ms268) {
		t.Fatal("fresh unacked data after idle: not dark")
	}
	// a sampling gap re-primes (the watch skipped ticks)
	st = ul268Link{}
	ul268Judge(&st, idle, now)
	if ul268Judge(&st, busy, now+5000*ms268) {
		t.Fatal("after a gap: re-prime, not dark")
	}
}

// TestUL268DarkSetNeedsAlt: a dark link counts only while another path acks;
// a healthy run (acks moving everywhere) gives no trigger at all.
func TestUL268DarkSetNeedsAlt(t *testing.T) {
	type lk struct{ n int }
	a1, a2, b1 := &lk{1}, &lk{2}, &lk{3}
	keys := []any{a1, a2, b1}
	sts := map[any]*ul268Link{a1: {}, a2: {}, b1: {}}
	acked := map[any]uint64{}
	now := int64(1_000_000) * ms268
	tick := func(at int64, frozen map[any]bool) map[any]bool {
		var smp []ul268Sample
		for _, k := range keys {
			if !frozen[k] {
				acked[k] += 10_000
			}
			p := "pA"
			if k == b1 {
				p = "pB"
			}
			smp = append(smp, ul268Sample{key: k, path: p, st: sts[k], ok: true,
				ts: tcpStat{bytesAcked: acked[k], unacked: 20, rttUs: 40_000}})
		}
		return ul268DarkSet(smp, at)
	}
	ev0 := ul268Stat.darkEv.Load()
	for i := 0; i < 160; i++ { // healthy control: 40 s
		if s := tick(now+int64(i)*250*ms268, nil); s != nil {
			t.Fatalf("healthy tick %d: dark %v", i, s)
		}
	}
	if ul268Stat.darkEv.Load() != ev0 {
		t.Fatal("false trigger in the healthy control")
	}
	now += 40_000 * ms268
	var got map[any]bool
	for i := 1; i <= 5; i++ { // path A data-dark, B alive
		got = tick(now+int64(i)*250*ms268, map[any]bool{a1: true, a2: true})
	}
	if !got[a1] || !got[a2] || got[b1] {
		t.Fatalf("A dark, B alive: %v", got)
	}
	if d := ul268Stat.darkEv.Load() - ev0; d != 2 {
		t.Fatalf("dark episodes %d, want 2", d)
	}
	now += 10_000 * ms268      // gap: re-prime
	for i := 1; i <= 16; i++ { // whole tunnel frozen: no alternative
		got = tick(now+int64(i)*250*ms268, map[any]bool{a1: true, a2: true, b1: true})
	}
	if got != nil {
		t.Fatalf("everything frozen: %v (a whole-tunnel stall is not a dark path)", got)
	}
}

func TestUL268RawCap(t *testing.T) {
	if c := ul268RawCap(100 << 10); c != rescueRingCap() {
		t.Fatalf("slow cap %d", c)
	}
	if c := ul268RawCap(4 << 20); c != 2<<20 {
		t.Fatalf("4 MB/s cap %d, want 2 MB", c)
	}
	if c := ul268RawCap(100 << 20); c != ul268ConnMax {
		t.Fatalf("100 MB/s cap %d, want %d", c, ul268ConnMax)
	}
	// ul268i: the cap moves in power-of-two steps (rounded up, never under
	// half a second), so a rising rate re-makes the ring a few times, not
	// on every new estimate (lab2: 752 MB/min of ring re-allocations)
	if c := ul268RawCap(3 << 20); c != 2<<20 {
		t.Fatalf("3 MB/s cap %d, want 2 MB (1.5 MB rounded up)", c)
	}
	seen := map[int]bool{}
	for rate := ul268FastBps; rate <= 64<<20; rate += 7 << 10 {
		c := ul268RawCap(rate)
		if c < min(rate/2, ul268ConnMax) || c > ul268ConnMax {
			t.Fatalf("rate %d: cap %d out of [rate/2, ConnMax]", rate, c)
		}
		seen[c] = true
	}
	if len(seen) > 8 {
		t.Fatalf("%d distinct caps over a rising rate, want <= 8 (power-of-two steps)", len(seen))
	}
}

func fillFast(r *rescueRing, chunk []byte, n int) {
	for i := 0; i < n; i++ {
		r.add(chunk)
		if i == 2 {
			time.Sleep(260 * time.Millisecond) // the running window becomes a rate
		}
	}
}

// TestUL268RingBudget: a fast Iran raw ring grows past rescueRingCap up to
// ul268ConnMax from its own budget, keeps the newest bytes in order, and
// returns every byte on release; a key-off ring stays at rescueRingCap; the
// budget refuses growth beyond its cap without turning the ring off.
func TestUL268RingBudget(t *testing.T) {
	memBase, ulBase := rescueStat.mem.Load(), ul268Stat.mem.Load()
	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	fast := rescueRing{on: true, u268: true}
	fillFast(&fast, chunk, 256) // 16 MB
	// timing: under -race the rate (so the cap) is lower
	if len(fast.buf) < 1<<20 || len(fast.buf) > ul268ConnMax || fast.n != len(fast.buf) {
		t.Fatalf("fast ring %d holds %d, want 1 MB..%d full", len(fast.buf), fast.n, ul268ConnMax)
	}
	const total = 16 << 20
	got, ok := fast.get(total-100_000, total)
	if !ok || len(got) != 100_000 {
		t.Fatalf("get: ok=%v n=%d", ok, len(got))
	}
	for i, b := range got {
		if want := byte((total - 100_000 + i) % (64 << 10)); b != want {
			t.Fatalf("byte %d: %d want %d", i, b, want)
		}
	}
	if !fast.uCov.Load() {
		t.Fatal("a ring sized to its rate must cover")
	}
	if d := ul268Stat.mem.Load() - ulBase; d != int64(len(fast.buf)-rescueRingCap()) {
		t.Fatalf("budget holds %d, want %d", d, len(fast.buf)-rescueRingCap())
	}
	fast.release()
	if ul268Stat.mem.Load() != ulBase || rescueStat.mem.Load() != memBase {
		t.Fatalf("leak: ul %d->%d raw %d->%d", ulBase, ul268Stat.mem.Load(), memBase, rescueStat.mem.Load())
	}
	off := rescueRing{on: true}
	fillFast(&off, chunk, 64)
	if len(off.buf) != rescueRingCap() {
		t.Fatalf("u268 off ring %d", len(off.buf))
	}
	off.release()
	save := ul268MemMaxFix
	ul268MemMaxFix = 1 << 20
	defer func() { ul268MemMaxFix = save }()
	over0 := ul268Stat.over.Load()
	b := rescueRing{on: true, u268: true}
	fillFast(&b, chunk, 256)
	if b.uCov.Load() {
		t.Fatal("a ring cut short by the budget must not cover (a rescue would end the conn)")
	}
	if len(b.buf) > rescueRingCap()+(1<<20) || ul268Stat.over.Load() == over0 || !b.on {
		t.Fatalf("budget: ring %d over %d on %v", len(b.buf), ul268Stat.over.Load()-over0, b.on)
	}
	b.release()
	if ul268Stat.mem.Load() != ulBase || rescueStat.mem.Load() != memBase {
		t.Fatal("leak after the budget run")
	}
}

// TestUL268SlowRingStays: a writer under ul268FastBps keeps rescueRingCap.
func TestUL268SlowRingStays(t *testing.T) {
	r := rescueRing{on: true, u268: true}
	p := make([]byte, 16<<10)
	for i := 0; i < 12; i++ { // 16 KB / 100 ms = 160 KB/s
		r.add(p)
		time.Sleep(100 * time.Millisecond)
	}
	if len(r.buf) > rescueRingCap() {
		t.Fatalf("slow ring grew to %d", len(r.buf))
	}
	if !r.uCov.Load() {
		t.Fatal("slow ring of 192 KB at 160 KB/s: sized to its rate, must cover")
	}
	r.release()
}

// TestUL268BigRingRescueNoDuplicate: with the key on, a fast raw upload's
// Iran ring is sized past rescueRingCap; a rescue on the live path (a false
// trigger) delivers every byte once, in order, both ways.
func TestUL268BigRingRescueNoDuplicate(t *testing.T) {
	old := ul268On.Load()
	ul268On.Store(true)
	t.Cleanup(func() { ul268On.Store(old) })
	_, iran, exit := newRescuePair(t)
	a := randBytes(3<<20, 11)
	b := randBytes(20000, 12)
	both(t,
		func() error { return xfer(iran, a, 64<<10, 6*time.Millisecond) }, // ~300 ms: a rate
		func() error { return readN(exit, a) },
		func() error { return xfer(exit, b, 3000, 0) },
		func() error { return readN(iran, b) },
	)
	iran.wmu.Lock()
	big, u := len(iran.ring.buf), iran.ring.u268
	iran.wmu.Unlock()
	if !u {
		t.Fatal("Iran ring not ul268")
	}
	if big <= rescueRingCap() {
		t.Fatalf("iran ring %d KB after a fast 3 MB: want > %d KB", big>>10, rescueRingCap()>>10)
	}
	rescueNow(t, iran)
	c, d := randBytes(1<<20, 13), randBytes(10000, 14)
	both(t,
		func() error { return xfer(iran, c, 32<<10, 0) },
		func() error { return readN(exit, c) },
		func() error { return xfer(exit, d, 2000, 0) },
		func() error { return readN(iran, d) },
	)
	for _, m := range []*mConn{iran, exit} {
		m.r.mu.Lock()
		extra := m.r.readyBytes
		m.r.mu.Unlock()
		if extra != 0 {
			t.Fatalf("%d bytes delivered twice", extra)
		}
	}
	exit.wmu.Lock()
	if exit.ring.u268 {
		t.Error("exit ring must stay small (Iran only)")
	}
	exit.wmu.Unlock()
	closeAndWait(t, iran, exit)
}

// TestUL268CoverSmall: a ring holding every byte written covers; one that
// dropped bytes while unsized to the rate does not.
func TestUL268CoverSmall(t *testing.T) {
	r := rescueRing{on: true, u268: true}
	r.add(make([]byte, 1000))
	if !r.uCov.Load() {
		t.Fatal("holds every byte: covers")
	}
	r.release()
	if r.uCov.Load() {
		t.Fatal("released: no cover")
	}
	// the budget refuses the growth the rate wants: bytes dropped, no cover
	save := ul268MemMaxFix
	ul268MemMaxFix = 1
	defer func() { ul268MemMaxFix = save }()
	q := rescueRing{on: true, u268: true}
	q.uRate = ul268FastBps * 64
	for i := 0; i < 8; i++ {
		q.add(make([]byte, 64<<10))
	}
	if q.uCov.Load() || len(q.buf) != rescueRingCap() {
		t.Fatalf("dropped bytes, ring %d short of the rate: must not cover", len(q.buf))
	}
	q.release()
}

// TestUL268StripeEvac: a striped upload closes its sub on a data-dark link
// (stripe sub before the primary), never the last sub off the dark set,
// never with the key off, a download, or no dark set; one per gap.
func TestUL268StripeEvac(t *testing.T) {
	setDark := func(keys ...int) {
		d := map[any]bool{}
		for _, k := range keys {
			d[k] = true
		}
		ul268DarkAll.Store(&d)
	}
	old := ul268On.Load()
	ul268On.Store(true)
	t.Cleanup(func() { ul268On.Store(old); ul268DarkAll.Store(nil) })
	m, subs := evacConn(t, 3, nil)
	stripeEvacOn.Store(false) // ul268 does not need HS2_MCONN_STRIPE_EVAC
	now := time.Now()
	ul268DarkAll.Store(nil)
	if s := m.ul268StripeEvacLocked(now); s != nil {
		t.Fatalf("no dark set: %v", s)
	}
	setDark(0, 2)
	if s := m.ul268StripeEvacLocked(now); s != subs[2] {
		t.Fatalf("evac %v, want stripe sub 2 before the primary", s)
	}
	if s := m.ul268StripeEvacLocked(now.Add(stripeEvacGap / 2)); s != nil {
		t.Fatalf("inside the gap: %v", s)
	}
	setDark(0, 1, 2, 3)
	if s := m.ul268StripeEvacLocked(now.Add(2 * stripeEvacGap)); s != nil {
		t.Fatalf("every sub dark: %v", s)
	}
	setDark(1)
	m.stUpNow = false
	if s := m.ul268StripeEvacLocked(now.Add(3 * stripeEvacGap)); s != nil {
		t.Fatalf("download: %v", s)
	}
	m.stUpNow = true
	ul268On.Store(false)
	if s := m.ul268StripeEvacLocked(now.Add(4 * stripeEvacGap)); s != nil {
		t.Fatalf("key off: %v", s)
	}
	ul268On.Store(true)
	if s := m.ul268StripeEvacLocked(now.Add(5 * stripeEvacGap)); s != subs[1] {
		t.Fatalf("dark sub 1: %v", s)
	}
}

// TestUL268CallNeverTouched: a call (callOn, or call-like within callFreeTTL
// after the stall dropped callOn) sees only the er92 set: its data-dark-only
// links are gone from what rescue / rs146 retry / rrEligible look at, and the
// stripe evac leaves it alone. Non-calls see the full set.
func TestUL268CallNeverTouched(t *testing.T) {
	old := ul268On.Load()
	ul268On.Store(true)
	t.Cleanup(func() { ul268On.Store(old); ul268DarkAll.Store(nil); ul268DarkOnly.Store(nil) })
	m, subs := evacConn(t, 3, nil)
	stripeEvacOn.Store(false)
	now := time.Now()
	k0, k1 := subs[0].key, subs[1].key
	silent := map[any]bool{"er92": true}
	set := map[any]bool{"er92": true, k0: true, k1: true}
	only := map[any]bool{k0: true, k1: true}
	all := map[any]bool{k0: true, k1: true}
	ul268DarkOnly.Store(&only)
	ul268DarkAll.Store(&all)
	cs := ul268CallSet(set)
	if len(cs) != 1 || !cs["er92"] {
		t.Fatalf("call set %v, want only the er92 link", cs)
	}
	if len(set) != 3 {
		t.Fatalf("ul268CallSet changed its input: %v", set)
	}
	if got := ul268CallSet(only); got != nil {
		t.Fatalf("all dark-only: %v, want nil", got)
	}
	// not a call: the full set, no callskip
	m.callOn, m.callLikeAt = false, time.Time{}
	n0 := ul268Stat.callSkip.Load()
	if got := m.ul268SetForLocked(now, set, cs); len(got) != 3 {
		t.Fatalf("non-call: %v", got)
	}
	if ul268Stat.callSkip.Load() != n0 {
		t.Fatal("non-call counted in callskip")
	}
	if s := m.ul268StripeEvacLocked(now); s == nil {
		t.Fatal("non-call striped upload on a dark link: no evac")
	}
	// a copying call
	m.callOn = true
	if got := m.ul268SetForLocked(now, set, cs); len(got) != 1 || got[k0] || got[k1] {
		t.Fatalf("call: %v", got)
	}
	if ul268Stat.callSkip.Load() != n0+1 {
		t.Fatal("call on a dark link: callskip not counted")
	}
	m.stEvacAt = time.Time{}
	if s := m.ul268StripeEvacLocked(now.Add(2 * stripeEvacGap)); s != nil {
		t.Fatalf("call: stripe evac %v", s)
	}
	// the stall dropped callOn; still call-like within callFreeTTL
	m.callOn, m.callLikeAt = false, now.Add(-callFreeTTL/2)
	if got := m.ul268SetForLocked(now, set, cs); len(got) != 1 {
		t.Fatalf("call-like: %v", got)
	}
	// a call whose links are er92-silent: the bin264 rescue stays
	if got := m.ul268SetForLocked(now, silent, ul268CallSet(silent)); !got["er92"] {
		t.Fatalf("call, er92 set: %v", got)
	}
	// no dark-only links: the set as it is
	ul268DarkOnly.Store(nil)
	if got := m.ul268SetForLocked(now, set, cs); len(got) != 3 {
		t.Fatalf("no dark-only: %v", got)
	}
}
