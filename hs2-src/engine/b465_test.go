package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

func setHedge7(t *testing.T, on bool) {
	o7 := liteHedge7
	setHedge4(t, on)
	liteHedge7 = on
	t.Cleanup(func() { liteHedge7 = o7 })
}

// b465: the download sampler runs on a heavy hedged records conn (Iran) for
// stripeDlWindow from its start with the key, never with the key off, never
// on a light (not heavy) hedged conn, an old one, or at the exit; a raw conn
// is unchanged.
func TestB465DlOnHedged(t *testing.T) {
	dlFastTest(t, true)
	t0 := time.Now()
	mk := func(iran bool) *mConn {
		m := &mConn{iran: iran, lite: true, h3: true, mode: planeLight}
		m.stK = 8
		m.h7Born = t0
		m.r.next = hedge3Bytes + 1
		return m
	}
	for _, on := range []bool{false, true} {
		setHedge7(t, on)
		m := mk(true)
		if got := m.stripeDlOnLocked(t0.Add(time.Second)); got != on {
			t.Fatalf("key %v: on=%v", on, got)
		}
		want := time.Second
		if on {
			want = stripeDlStep
		}
		if d := m.hedge7NextLocked(t0.Add(time.Second), time.Second); d != want {
			t.Fatalf("key %v: next %v", on, d)
		}
	}
	setHedge7(t, true)
	if m := mk(true); m.stripeDlOnLocked(t0.Add(stripeDlWindow)) {
		t.Fatal("past the window")
	}
	light := mk(true)
	light.r.next = hedge3Bytes / 2
	if light.stripeDlOnLocked(t0.Add(time.Second)) || light.hedge7NextLocked(t0.Add(time.Second), time.Second) != time.Second {
		t.Fatal("a light hedged conn got the sampler")
	}
	if ex := mk(false); ex.stripeDlOnLocked(t0.Add(time.Second)) {
		t.Fatal("the exit sampled")
	}
	nh := mk(true)
	nh.h3 = false
	if nh.stripeDlOnLocked(t0.Add(time.Second)) {
		t.Fatal("a conn that was not hedged")
	}
	raw := &mConn{iran: true, lite: true}
	raw.stK = 8
	raw.rawBorn = t0
	raw.rawW.Store(true)
	if !raw.stripeDlOnLocked(t0.Add(time.Second)) || raw.stripeDlOnLocked(t0.Add(stripeDlWindow)) {
		t.Fatal("raw rule changed")
	}
	// the sampler's decision on the hedged conn: two hot 200 ms samples
	m := mk(true)
	const fast = 100e6 / 8 / 5
	base := hedge3Bytes + 1
	at := func(i int) time.Time { return t0.Add(400*time.Millisecond + time.Duration(i)*stripeDlStep) }
	m.stripeDlSampleLocked(at(0), 100, base)
	if g, _ := m.stripeDlSampleLocked(at(1), 100, base+fast); g {
		t.Fatal("one sample")
	}
	if g, r := m.stripeDlSampleLocked(at(2), 100, base+2*fast+fast/10); !g || r < 80e6/8 {
		t.Fatalf("two hot samples: %v %v", g, r)
	}
	if b := m.stripeDlBornLocked(); !b.Equal(t0) {
		t.Fatal("born")
	}
}

// b465: a heavy download over a hedged pair with HEDGE7 is delivered byte
// exact, and the download sampler decides its stripe within the first
// ~1.5 s (as a raw conn's does); with HEDGE4 alone it never does.
func TestB465HedgedDownloadStripe(t *testing.T) {
	for _, on := range []bool{true, false} {
		dlFastTest(t, true)
		setHedge7(t, on)
		if !on {
			setHedge4(t, true)
		}
		ok := stripeK.Load()
		_, iran, exit := newHedge2Pair(t, nil)
		iran.mu.Lock()
		iran.stK = 8
		iran.mu.Unlock()
		mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
		exit.exitHedge3Mark(false)
		e0 := stripeDlStat.early.Load()
		resp := randBytes(24<<20, 31)
		t0 := time.Now()
		var gotA atomic.Int64
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				if gotA.Load() == 0 && stripeDlStat.early.Load() != e0 {
					gotA.Store(int64(time.Since(t0)))
				}
			}
		}()
		both(t, func() error { return xfer(exit, resp, 32<<10, 2*time.Millisecond) }, func() error { return readN(iran, resp) })
		close(stop)
		time.Sleep(30 * time.Millisecond)
		early := stripeDlStat.early.Load() - e0
		got := time.Duration(gotA.Load())
		t.Logf("key %v: sampler decisions %d, first at %v (stripeK %d)", on, early, got, ok)
		if on && (early == 0 || got > 2*time.Second) {
			t.Fatalf("HEDGE7: no early download stripe decision (%d, %v)", early, got)
		}
		if !on && early != 0 {
			t.Fatalf("HEDGE4 alone: sampler ran on a records conn (%d)", early)
		}
		iran.end(errMConnClosed, false)
		exit.end(errMConnClosed, false)
	}
}
