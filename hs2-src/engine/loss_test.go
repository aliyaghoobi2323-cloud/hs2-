package engine

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// tick advances the rig one health sample without the good links' traffic
// (each test drives every link itself).
func (r *stuckRig) tick() {
	r.clk.Advance(healthTick)
	r.m.sampleHealth()
}

// downLossy adds one tick of download (bytes) to f with frac of what the exit
// sends resent: R/(pkts+R) = frac.
func downLossy(f *meteredFakeLink, bytes uint64, frac float64) {
	pkts := float64(bytes) / mss
	f.m.rdBytes.Add(bytes)
	f.m.peerRetrans.Add(uint64(frac*pkts/(1-frac) + 0.5))
}

// The download loss is judged over the window between two pongs, which come
// every 3 s on a busy link, not on the 2 s tick: a link resending 30% with
// steady pongs was never drained (every third tick had no pong, read as no
// loss), and one resending 9% with jittery pongs (two in one tick: 6 s of
// resends over 2 s of bytes) was.
func TestDownLossFollowsThePongWindow(t *testing.T) {
	run := func(t *testing.T, frac float64, jitter bool, ticks int) (*stuckRig, *managedLink, int) {
		r := newStuckRig(t, 0)
		var goods []*meteredFakeLink
		for i := 0; i < 5; i++ {
			g, _ := r.add()
			goods = append(goods, g)
		}
		f, ml := r.add()
		rng := rand.New(rand.NewPCG(1, 2))
		next := 3 * time.Second // when the next pong is read
		if jitter {
			next += time.Duration(rng.Int64N(int64(2500 * time.Millisecond)))
		}
		var el, last time.Duration
		for i := 0; i < ticks; i++ {
			for _, g := range goods {
				g.download(200<<10, 0)
			}
			downLossy(f, 200<<10, frac)
			el += healthTick
			for next <= el { // the pongs read during this tick
				f.pong(int64(next-last), 0)
				last = next
				next += 3 * time.Second
				if jitter {
					next = last + 500*time.Millisecond + time.Duration(rng.Int64N(int64(4500*time.Millisecond)))
				}
			}
			r.tick()
			if ml.degraded {
				return r, ml, i + 1
			}
		}
		return r, ml, -1
	}
	t.Run("30% with steady pongs is drained", func(t *testing.T) {
		r, ml, at := run(t, 0.30, false, 15)
		if at < 0 || ml.stuck {
			t.Fatalf("a link resending 30%% was not drained in 30 s (streak %d):\n%s", ml.dnStreak, r.lg)
		}
		if !strings.Contains(r.lg.String(), "down-loss 30% of") {
			t.Fatalf("the line does not give the download loss:\n%s", r.lg)
		}
	})
	t.Run("9% with jittery pongs is not", func(t *testing.T) {
		r, _, at := run(t, 0.09, true, 150)
		if at >= 0 {
			t.Fatalf("a link resending 9%% (under %.0f%%) was drained at tick %d:\n%s", lossFrac*100, at, r.lg)
		}
	})
	t.Run("20% with jittery pongs is drained", func(t *testing.T) {
		r, ml, at := run(t, 0.20, true, 30)
		if at < 0 {
			t.Fatalf("a link resending 20%% was not drained in 60 s (streak %d):\n%s", ml.dnStreak, r.lg)
		}
	})
}

// A short window (two pongs read together after a wait) stays open: what
// little came in between them is no sample of its own.
func TestDownLossShortWindowStaysOpen(t *testing.T) {
	ml := &managedLink{}
	r1 := &peerLossRec{n: 1, rt: 0, rd: 0, at: 0}
	if _, fresh := ml.judgeDownLoss(r1); fresh {
		t.Fatal("the first pong closed a window")
	}
	r2 := &peerLossRec{n: 2, rt: 50, rd: 10 << 10, at: int64(300 * time.Millisecond)}
	if d, fresh := ml.judgeDownLoss(r2); fresh || d.judged {
		t.Fatalf("a 300 ms window was judged: %+v", d)
	}
	r3 := &peerLossRec{n: 3, rt: 50, rd: 400 << 10, at: int64(3 * time.Second)}
	d, fresh := ml.judgeDownLoss(r3)
	if !fresh || !d.judged || d.resent != 50 {
		t.Fatalf("the 3 s window from the first pong: fresh %v %+v", fresh, d)
	}
	// Segments counted by the kernel replace bytes / mss.
	r4 := &peerLossRec{n: 4, rt: 60, rd: 800 << 10, segsIn: 1000, at: int64(6 * time.Second)}
	ml.judgeDownLoss(r4)
	r5 := &peerLossRec{n: 5, rt: 160, rd: 1200 << 10, segsIn: 1900, at: int64(9 * time.Second)}
	d, _ = ml.judgeDownLoss(r5)
	if got, want := d.frac(), 100.0/1000; got < want-0.001 || got > want+0.001 {
		t.Fatalf("100 resent, 900 segments in: %.3f, want %.3f", got, want)
	}
	// A counter going back (a new socket's) starts a window, judges nothing.
	r6 := &peerLossRec{n: 6, rt: 5, rd: 1300 << 10, at: int64(12 * time.Second)}
	if d, fresh := ml.judgeDownLoss(r6); fresh || d.judged {
		t.Fatalf("judged across a counter reset: %+v", d)
	}
}

// Upload loss counts segments the way the retransmits do: a link of small
// frames resending 40 of 600 segments (7%) is not lossy, though its bytes
// over mss (146 "packets") made it 27%.
func TestUpLossCountsSegments(t *testing.T) {
	for _, c := range []struct {
		name    string
		resent  uint64
		drained bool
	}{{"7% of segments", 40, false}, {"20% of segments", 120, true}} {
		t.Run(c.name, func(t *testing.T) {
			r := newStuckRig(t, 0)
			f, ml := r.add()
			var segs uint32 = 1000
			for i := 0; i < 8 && !ml.degraded; i++ {
				f.active(200<<10, c.resent)
				segs += 600
				f.setTCP(tcpStat{segsOut: segs})
				r.tick()
			}
			if ml.degraded != c.drained {
				t.Fatalf("drained %v, want %v (streak %d):\n%s", ml.degraded, c.drained, ml.upStreak, r.lg)
			}
		})
	}
}

// When most busy links resend over lossFrac it is the path (or a throttle
// every connection meets), not those links: none is drained, and the log
// says so once a minute.
func TestLossPathWideDrainsNone(t *testing.T) {
	r := newStuckRig(t, 0)
	var fs []*meteredFakeLink
	var mls []*managedLink
	for i := 0; i < 10; i++ {
		f, ml := r.add()
		fs, mls = append(fs, f), append(mls, ml)
	}
	for i := 0; i < 40; i++ { // 80 s
		for j, f := range fs {
			frac := 0.15
			if j >= 7 {
				frac = 0.01
			}
			downLossy(f, 200<<10, frac)
			f.pong(int64(healthTick), 0)
		}
		r.tick()
	}
	for _, ml := range mls {
		if ml.degraded {
			t.Fatalf("link %d drained while 7 of 10 busy links resend 15%%:\n%s", ml.id, r.lg)
		}
	}
	if n := r.lg.count("busy links resend more than 12%"); n < 1 || n > 2 {
		t.Fatalf("%d path-wide lines in 80 s, want 1-2 (once a minute):\n%s", n, r.lg)
	}
}

// A link is drained only if it resends well above the busy links around it:
// with most busy links at 8% (a per-connection throttle), one at 14% stays
// and one at 25% goes.
func TestLossRelativeToTheBusyLinks(t *testing.T) {
	r := newStuckRig(t, 0)
	var fs []*meteredFakeLink
	for i := 0; i < 10; i++ {
		f, _ := r.add()
		fs = append(fs, f)
	}
	mid, mid2 := r.add()
	hi, hi2 := r.add()
	for i := 0; i < 10; i++ {
		for _, f := range fs {
			downLossy(f, 200<<10, 0.08)
			f.pong(int64(healthTick), 0)
		}
		downLossy(mid, 200<<10, 0.14)
		mid.pong(int64(healthTick), 0)
		downLossy(hi, 200<<10, 0.25)
		hi.pong(int64(healthTick), 0)
		r.tick()
	}
	if mid2.degraded {
		t.Fatalf("a link at 14%% was drained while the busy links resend 8%%:\n%s", r.lg)
	}
	if !hi2.degraded {
		t.Fatalf("a link at 25%% was not drained (streak %d):\n%s", hi2.dnStreak, r.lg)
	}
	if !strings.Contains(r.lg.String(), "busy links resend 8% at the median") {
		t.Fatalf("the line does not give the median:\n%s", r.lg)
	}
}

// At most drainHeadroom degraded links drain at a time, the lossiest first;
// the rest wait for room.
func TestLossDrainsAtMostHeadroom(t *testing.T) {
	r := newStuckRig(t, 0)
	var clean, lossy []*meteredFakeLink
	var lml []*managedLink
	for i := 0; i < 20; i++ {
		f, _ := r.add()
		clean = append(clean, f)
	}
	for i := 0; i < 10; i++ {
		f, ml := r.add()
		lossy, lml = append(lossy, f), append(lml, ml)
	}
	for i := 0; i < 6; i++ {
		for _, f := range clean {
			downLossy(f, 200<<10, 0)
			f.pong(int64(healthTick), 0)
		}
		for j, f := range lossy {
			downLossy(f, 200<<10, 0.20+0.02*float64(j))
			f.pong(int64(healthTick), 0)
		}
		r.tick()
	}
	room := drainHeadroom(r.m.max)
	n := 0
	for j, ml := range lml {
		if ml.degraded {
			n++
			if j < len(lml)-room {
				t.Fatalf("link at %.0f%% drained before the lossier ones:\n%s", 100*(0.20+0.02*float64(j)), r.lg)
			}
		}
	}
	if n != room {
		t.Fatalf("%d of 10 lossy links drained at once, want drainHeadroom(%d) = %d:\n%s", n, r.m.max, room, r.lg)
	}
}

// Two or three busy links at night are no sample of the path: lossy ones are
// drained by lossFrac alone, as before.
func TestLossFewBusyLinksJudgedAlone(t *testing.T) {
	r := newStuckRig(t, 0)
	a, aml := r.add()
	b, bml := r.add()
	for i := 0; i < 5; i++ {
		downLossy(a, 200<<10, 0.2)
		a.pong(int64(healthTick), 0)
		downLossy(b, 200<<10, 0.2)
		b.pong(int64(healthTick), 0)
		r.tick()
	}
	if !aml.degraded || !bml.degraded {
		t.Fatalf("two lossy busy links (the only ones) not drained: %v %v\n%s", aml.degraded, bml.degraded, r.lg)
	}
}

// upPressed adds one tick of upload to f: bytes, of segs segments resent
// resent, its writer waiting for the path most of the tick (pressed).
func upPressed(f *meteredFakeLink, bytes, resent uint64, segs *uint32, n uint32) {
	f.active(bytes, resent)
	f.m.wrBlocked.Add(int64(healthTick) * 9 / 10)
	*segs += n
	f.setTCP(tcpStat{segsOut: *segs})
}

// A lossy link still moving what a link gets on this path (the pressed
// links' rate) is kept — its resends are what a throttle dropping what goes
// over its rate costs, and its users would get no more elsewhere; one that
// loses so much it moves a fraction of that is drained.
func TestLossKeepsALinkAtThePathsRate(t *testing.T) {
	r := newStuckRig(t, 0)
	var fs []*meteredFakeLink
	segs := make([]uint32, 12)
	for i := 0; i < 10; i++ {
		f, _ := r.add()
		fs = append(fs, f)
	}
	atRate, atML := r.add()
	slow, slowML := r.add()
	for i := range segs {
		segs[i] = 1000
	}
	for i := 0; i < 8; i++ {
		for j, f := range fs {
			upPressed(f, 700<<10, 10, &segs[j], 512) // 2%: 350 KB/s
		}
		upPressed(atRate, 680<<10, 128, &segs[10], 512) // 25% at the path's rate
		upPressed(slow, 120<<10, 30, &segs[11], 120)    // 25% at a sixth of it
		r.tick()
	}
	if atML.degraded {
		t.Fatalf("a link resending 25%% at the pressed links' rate was drained:\n%s", r.lg)
	}
	if !slowML.degraded {
		t.Fatalf("a link resending 25%% at a sixth of the path's rate was kept (streak %d):\n%s", slowML.upStreak, r.lg)
	}
}
