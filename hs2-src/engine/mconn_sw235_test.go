package engine

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// sw235 tests: the sweeper reads a link socket's TCP_INFO once per bucket
// tick, covers every bucket once per round, and skips quiet resident conns
// without m.mu until a keep or a gap sets their hint.

// sw235Hooks: the production subLinkAck (cached path), subSweepTCP and
// subTCPAck replaced by fakes; n counts the TCP_INFO reads.
func sw235Hooks(t *testing.T, tcpOf func(*msub) *net.TCPConn, la linkAck) *atomic.Int64 {
	fx212SubKeys(t)
	subLinkAck = subLinkAckSys
	subSweepPaused.Store(true)
	t.Cleanup(func() { subSweepPaused.Store(false) })
	oldT, oldA := subSweepTCP, subTCPAck
	n := &atomic.Int64{}
	subSweepTCP = tcpOf
	subTCPAck = func(*net.TCPConn) (linkAck, bool) { n.Add(1); return la, true }
	t.Cleanup(func() { subSweepTCP, subTCPAck = oldT, oldA })
	return n
}

// sw235Conn: a sub-resend conn with one sub keeping one chunk aged d, armed
// in the sweeper.
func sw235Conn(t *testing.T, id uint64, d time.Duration) (*mConn, *msub) {
	m := &mConn{id: id, mode: planeLight, kick: make(chan struct{}, 1)}
	s := &msub{m: m}
	m.subs = []*msub{s}
	m.mu.Lock()
	fx212Keep(m, s, 1000)
	s.sent[0].t = time.Now().Add(-d)
	m.keepIdleArmLocked()
	sw := m.ksSwept
	m.mu.Unlock()
	if !sw {
		t.Fatal("not swept")
	}
	t.Cleanup(func() {
		m.mu.Lock()
		m.ended = true
		if len(s.sent) > 0 {
			m.dropSentLocked(s, len(s.sent))
		}
		m.subSweepDropLocked()
		m.mu.Unlock()
	})
	return m, s
}

// One TCP_INFO per link socket per tick, shared by every sub riding it.
func TestSw235LinkCache(t *testing.T) {
	links := []*net.TCPConn{{}, {}, {}}
	tcpOf := map[*msub]*net.TCPConn{}
	n := sw235Hooks(t, func(s *msub) *net.TCPConn { return tcpOf[s] }, linkAck{srtt: 10 * time.Millisecond, unacked: 1})
	var ms []*mConn
	for i := 0; i < 30; i++ {
		m, s := sw235Conn(t, uint64(1000+i), 0) // young: kept across the tick
		tcpOf[s] = links[i%3]
		ms = append(ms, m)
	}
	c0 := fx212Stat.tcpCached.Load()
	w := &subSweepWork{}
	for b := 0; b < subSweepBuckets; b++ {
		n.Store(0)
		w.bucket(time.Now(), b, false)
		if n.Load() > 3 {
			t.Fatalf("bucket %d: %d TCP_INFO reads for 3 links", b, n.Load())
		}
	}
	if d := fx212Stat.tcpCached.Load() - c0; d < 30-3*subSweepBuckets {
		t.Fatalf("cached looks %d", d)
	}
	// a full tick: still at most one read per link per bucket
	n.Store(0)
	subSweepTick(time.Now())
	if n.Load() > 3*subSweepBuckets {
		t.Fatalf("full tick: %d reads", n.Load())
	}
	_ = ms
}

// Every swept conn is visited exactly once per round of subSweepBuckets
// bucket ticks.
func TestSw235BucketCoverage(t *testing.T) {
	sw235Hooks(t, func(*msub) *net.TCPConn { return nil }, linkAck{})
	const N = 57
	for i := 0; i < N; i++ {
		sw235Conn(t, uint64(2000+i), 0)
	}
	seen := map[*mConn]int{}
	for b := 0; b < subSweepBuckets; b++ {
		subSweepConns[b].Range(func(k, _ any) bool {
			m := k.(*mConn)
			if m.id >= 2000 && m.id < 2000+N {
				seen[m]++
				if int(m.id%subSweepBuckets) != b {
					t.Fatalf("conn %d in bucket %d", m.id, b)
				}
			}
			return true
		})
	}
	if len(seen) != N {
		t.Fatalf("%d of %d conns in the buckets", len(seen), N)
	}
	for m, c := range seen {
		if c != 1 {
			t.Fatalf("conn %d in %d buckets", m.id, c)
		}
	}
	v0 := fx212Stat.sweeps.Load()
	w := &subSweepWork{}
	for b := 0; b < subSweepBuckets; b++ {
		w.bucket(time.Now(), b, false)
	}
	if d := fx212Stat.sweeps.Load() - v0; d < N {
		t.Fatalf("one round visited %d < %d", d, N)
	}
}

// A conn whose look found nothing kept stays resident with its hint clear,
// is skipped without m.mu, comes back on a keep, and leaves on a gc round.
func TestSw235QuietHint(t *testing.T) {
	sw235Hooks(t, func(*msub) *net.TCPConn { return nil }, linkAck{})
	m, s := sw235Conn(t, 3003, 0)
	b := int(m.id % subSweepBuckets)
	if !m.swKept.Load() {
		t.Fatal("hint not set by the arm")
	}
	// drain what it keeps (the write path's trim), then a plain visit
	m.mu.Lock()
	m.dropSentLocked(s, len(s.sent))
	m.mu.Unlock()
	w := &subSweepWork{}
	w.bucket(time.Now(), b, false)
	if m.swKept.Load() || !subSweepHas(m) {
		t.Fatalf("quiet: hint %v resident %v", m.swKept.Load(), subSweepHas(m))
	}
	// skipped without m.mu: hold the lock, the visit must not block
	m.mu.Lock()
	i0 := fx212Stat.idle.Load()
	done := make(chan struct{})
	go func() { w.bucket(time.Now(), b, false); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		m.mu.Unlock()
		t.Fatal("a quiet conn's visit took m.mu")
	}
	if fx212Stat.idle.Load() == i0 {
		m.mu.Unlock()
		t.Fatal("not counted as skipped")
	}
	// a keep sets the hint again (keepSentLocked's swept branch)
	fx212Keep(m, s, 500)
	if m.ksSwept {
		m.noteSwKeptLocked()
	}
	m.mu.Unlock()
	if !m.swKept.Load() {
		t.Fatal("keep did not set the hint")
	}
	// a gap sets it too
	m.mu.Lock()
	m.dropSentLocked(s, len(s.sent))
	m.swKept.Store(false)
	subGapLocked(s, time.Now())
	m.mu.Unlock()
	if !m.swKept.Load() {
		t.Fatal("gap did not set the hint")
	}
	m.mu.Lock()
	s.subGapT = time.Time{}
	m.mu.Unlock()
	// a gc round: quiet and resident -> leaves
	w.bucket(time.Now(), b, true)
	if subSweepHas(m) || m.ksSwept {
		t.Fatal("quiet conn still in the sweeper after a gc round")
	}
}

// A resident quiet conn that is no longer sweepable (striped) leaves on its
// next arm, back to its manager's poll.
func TestSw235ArmUnsweepable(t *testing.T) {
	sw235Hooks(t, func(*msub) *net.TCPConn { return nil }, linkAck{})
	m, _ := sw235Conn(t, 4004, 0)
	m.mu.Lock()
	m.stN = 1
	ok := m.subSweepArmLocked()
	sw := m.ksSwept
	m.stN = 0
	m.mu.Unlock()
	if ok || sw || subSweepHas(m) {
		t.Fatalf("arm %v swept %v has %v", ok, sw, subSweepHas(m))
	}
}

// ---- benchmark: the old 1 s tick vs one sw235 round, N conns, 300 links ----

// sw235OldTick: the rel69a subSweepTick body over the same set (one TCP_INFO
// per sub, a map per conn, m.mu twice).
func sw235OldTick(now time.Time) {
	subKeptMaxNote(subResendStat.kept.Load())
	var xs []*msub
	for b := range subSweepConns {
		subSweepConns[b].Range(func(k, _ any) bool {
			m := k.(*mConn)
			m.mu.Lock()
			if !m.subSweepableLocked() {
				m.subSweepDropLocked()
				m.mu.Unlock()
				return true
			}
			xs = xs[:0]
			for _, x := range m.subs {
				if !x.dead && (len(x.sent) > 0 || !x.subGapT.IsZero()) {
					xs = append(xs, x)
				}
			}
			tcs := make([]*net.TCPConn, len(xs))
			for i, x := range xs {
				tcs[i] = subSweepTCP(x)
			}
			m.mu.Unlock()
			looks := make(map[*msub]fx212Look, len(xs))
			for i, x := range xs {
				var l fx212Look
				if tcs[i] != nil {
					l.la, l.ok = subTCPAck(tcs[i])
				}
				looks[x] = l
			}
			m.mu.Lock()
			m.keepStallTrimLocked(nil, now)
			m.keepIdleLookLocked(now, func(x *msub) (linkAck, bool) {
				l := looks[x]
				return l.la, l.ok
			})
			m.mu.Unlock()
			return true
		})
	}
}

func sw235BenchSet(b *testing.B, n, links int, keptFrac int) {
	stripeResend.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	subLinkAck = subLinkAckSys
	subSweepPaused.Store(true)
	defer subSweepPaused.Store(false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Skip(err)
	}
	defer ln.Close()
	var tcs []*net.TCPConn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	for i := 0; i < links; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			b.Skip(err)
		}
		defer c.Close()
		tcs = append(tcs, c.(*net.TCPConn))
	}
	tcpOf := map[*msub]*net.TCPConn{}
	oldT := subSweepTCP
	subSweepTCP = func(s *msub) *net.TCPConn { return tcpOf[s] }
	defer func() { subSweepTCP = oldT }()
	var ms []*mConn
	for i := 0; i < n; i++ {
		m := &mConn{id: uint64(900000 + i), mode: planeLight, kick: make(chan struct{}, 1)}
		for j := 0; j < 2; j++ {
			s := &msub{m: m}
			m.subs = append(m.subs, s)
			tcpOf[s] = tcs[(2*i+j)%links]
		}
		m.mu.Lock()
		if i%100 < keptFrac {
			c := getChunk(mRecData)
			c.n = 100
			c.refs.Store(1)
			m.subs[0].sent = append(m.subs[0].sent, sentRec{c: c, t: time.Now().Add(time.Hour), sub: true}) // young forever
			m.keptBytes += 100
		}
		m.ksSwept = true
		m.swKept.Store(true)
		subSweepBucketOf(m).Store(m, struct{}{})
		m.mu.Unlock()
		ms = append(ms, m)
	}
	defer func() {
		for _, m := range ms {
			subSweepBucketOf(m).Delete(m)
		}
	}()
	w := &subSweepWork{}
	// settle the hints (a real round)
	for bk := 0; bk < subSweepBuckets; bk++ {
		w.bucket(time.Now(), bk, false)
	}
	b.Run("old", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sw235OldTick(time.Now())
		}
	})
	b.Run("new", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for bk := 0; bk < subSweepBuckets; bk++ {
				w.bucket(time.Now(), bk, false)
			}
		}
	})
}

// 10k conns x 2 subs over 300 link sockets, all keeping (worst case).
func BenchmarkSw235TickAllKept(b *testing.B) { sw235BenchSet(b, 10000, 300, 100) }

// 10k conns, 30 % keeping (the rest quiet residents).
func BenchmarkSw235TickPart(b *testing.B) { sw235BenchSet(b, 10000, 300, 30) }
