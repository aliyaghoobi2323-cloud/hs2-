package engine

import (
	"testing"
	"time"
)

// fakeWatchClock drives watchRelease without real time: sleep advances now,
// and the fake TCP_INFO reader answers from a script keyed on that time.
type fakeWatchClock struct{ t time.Time }

func (c *fakeWatchClock) now() time.Time                   { return c.t }
func (c *fakeWatchClock) sleep(d time.Duration)            { c.t = c.t.Add(d) }
func (c *fakeWatchClock) since(t0 time.Time) time.Duration { return c.t.Sub(t0) }

// Data from the peer arrives 150 ms after the release: no stall.
func TestWatchReleaseProgressFast(t *testing.T) {
	c := &fakeWatchClock{t: time.Unix(1000, 0)}
	t0 := c.t
	read := func() (tcpStat, bool) {
		st := tcpStat{bytesRcvd: 5000, rtoUs: 204000}
		if c.since(t0) >= 150*time.Millisecond {
			st.bytesRcvd = 9000
		}
		return st, true
	}
	r, ok := watchRelease(read, c.sleep, c.now)
	if !ok || !r.resumed || r.stalled() {
		t.Fatalf("got %+v ok=%v, want resumed and not stalled", r, ok)
	}
	if r.waited < 150*time.Millisecond || r.waited > 200*time.Millisecond {
		t.Fatalf("waited %v, want ~150 ms", r.waited)
	}
	if r.maxBackoff != 0 || r.loss || r.maxRtoMs != 204 {
		t.Fatalf("got %+v, want no backoff, no loss, rto 204 ms", r)
	}
}

// Nothing arrives for 2 s while the socket sits in RTO backoff, then data
// does: a stall of ~2 s, with the backoff and rto recorded.
func TestWatchReleaseNoProgressTwoSeconds(t *testing.T) {
	c := &fakeWatchClock{t: time.Unix(1000, 0)}
	t0 := c.t
	read := func() (tcpStat, bool) {
		el := c.since(t0)
		st := tcpStat{bytesRcvd: 5000, backoff: 5, caState: tcpCALoss, rtoUs: 6700000}
		if el >= time.Second {
			st.backoff, st.rtoUs = 6, 13400000
		}
		if el >= 2*time.Second {
			st = tcpStat{bytesRcvd: 70000, rtoUs: 204000}
		}
		return st, true
	}
	r, ok := watchRelease(read, c.sleep, c.now)
	if !ok || !r.resumed || !r.stalled() {
		t.Fatalf("got %+v ok=%v, want a stall that resumed", r, ok)
	}
	if r.waited < 2*time.Second || r.waited > 2050*time.Millisecond {
		t.Fatalf("waited %v, want ~2 s", r.waited)
	}
	if r.backoff0 != 5 || r.maxBackoff != 6 || !r.loss || r.maxRtoMs != 13400 {
		t.Fatalf("got %+v, want backoff 5 at release, max 6, loss, rto 13400 ms", r)
	}
}

// No data at all: the watch gives up after releaseWatchFor, still a stall.
func TestWatchReleaseNeverResumes(t *testing.T) {
	c := &fakeWatchClock{t: time.Unix(1000, 0)}
	reads := 0
	read := func() (tcpStat, bool) { reads++; return tcpStat{bytesRcvd: 1}, true }
	r, ok := watchRelease(read, c.sleep, c.now)
	if !ok || r.resumed || !r.stalled() || r.waited != releaseWatchFor {
		t.Fatalf("got %+v ok=%v, want an unresumed stall of %v", r, ok, releaseWatchFor)
	}
	if want := int(releaseWatchFor/releaseWatchStep) + 1; reads != want {
		t.Fatalf("%d TCP_INFO reads, want %d", reads, want)
	}
}

// Without TCP_INFO (non-Linux, a bare session) there is no watch.
func TestWatchReleaseNoTCPInfo(t *testing.T) {
	c := &fakeWatchClock{t: time.Unix(1000, 0)}
	if _, ok := watchRelease(func() (tcpStat, bool) { return tcpStat{}, false }, c.sleep, c.now); ok {
		t.Fatal("watch ran without TCP_INFO")
	}
}

// A wedged release is counted even when the guard has no socket to watch.
func TestWedgeReleaseCounted(t *testing.T) {
	before := obs.wedgeReleases.Load()
	g := &sessGuard{}
	g.noteRelease()
	if got := obs.wedgeReleases.Load() - before; got != 1 {
		t.Fatalf("releases +%d, want +1", got)
	}
	if g.watching.Load() {
		t.Fatal("a watch started without a socket")
	}
}
