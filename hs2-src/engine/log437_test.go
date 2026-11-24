package engine

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// b437 F9: an application silence (healthy links) is counted but logs no
// onset and no end under dc254SilentLogLong; one with link evidence logs
// both; the length buckets count every episode.
func TestLog437DarkcallSilent(t *testing.T) {
	setDc254(t, true)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	dc254Log.min.Store(0)
	now := time.Now()
	nowc := int64(1_000_000_000_000)
	ok := &msub{idx: 1}
	ok.dc = dcSub{ok: true, ackAt: now, lastRd: nowc}
	n0, a0, g20, g50 := dc254Stat.silentRx.Load(), dc254Stat.silentApp.Load(), dc254Stat.silent2s.Load(), dc254Stat.silent5s.Load()

	// app silence of 3 s: counted (>= 2 s), no line
	m := &mConn{}
	m.dc.rxAt = now.Add(-time.Second)
	m.dc254SilenceLocked(nil, []*msub{ok}, now, nowc, 0)
	m.dc254SilenceLocked(nil, []*msub{ok}, now.Add(2*time.Second), nowc, 0)
	m.dc.rxAt = now.Add(2100 * time.Millisecond)
	m.dc254SilenceLocked(nil, []*msub{ok}, now.Add(2100*time.Millisecond), nowc, 0)
	if strings.Contains(buf.String(), "darkcall silent") {
		t.Fatalf("app silence < 5 s logged: %q", buf.String())
	}
	if dc254Stat.silentRx.Load()-n0 != 1 || dc254Stat.silentApp.Load()-a0 != 1 || dc254Stat.silent2s.Load()-g20 != 1 || dc254Stat.silent5s.Load() != g50 {
		t.Fatal("app silence not counted")
	}
	// app silence of 6 s: the end is logged
	m.dc254SilenceLocked(nil, []*msub{ok}, now.Add(3*time.Second), nowc, 0)
	m.dc254SilenceLocked(nil, []*msub{ok}, now.Add(8*time.Second), nowc, 0)
	m.dc.rxAt = now.Add(8100 * time.Millisecond)
	m.dc254SilenceLocked(nil, []*msub{ok}, now.Add(8100*time.Millisecond), nowc, 0)
	if !strings.Contains(buf.String(), "ended after") || strings.Contains(buf.String(), "rx stalled") || dc254Stat.silent5s.Load()-g50 != 1 {
		t.Fatalf("long app silence: %q", buf.String())
	}
	// link evidence: onset and end both logged
	buf.Reset()
	bad := &msub{idx: 2}
	bad.dc = dcSub{ok: true, unacked: 10, ackAt: now.Add(-time.Second), lastRd: nowc}
	m2 := &mConn{}
	m2.dc.rxAt = now.Add(-time.Second)
	m2.dc254SilenceLocked(nil, []*msub{ok, bad}, now, nowc, 0)
	m2.dc.rxAt = now.Add(300 * time.Millisecond)
	m2.dc254SilenceLocked(nil, []*msub{ok, bad}, now.Add(300*time.Millisecond), nowc, 0)
	if !strings.Contains(buf.String(), "rx stalled") || !strings.Contains(buf.String(), "evidence link") {
		t.Fatalf("link-evidence silence: %q", buf.String())
	}
	if n, _, _, _, _, _ := DC254Silent(); n < 3 {
		t.Fatal("DC254Silent")
	}
}

// b437 F10: every width a connection reaches is counted once; the line only
// from stripeLogWide.
func TestLog437StripeReach(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	var r0 [7]uint64
	for i := range r0 {
		r0[i] = stripeStat.reach[i].Load()
	}
	m := &mConn{}
	for _, w := range []int{2, 3, 3, 2, 4, 9} { // 5..8 skipped by one jump
		m.stripeReachLocked(w)
	}
	for i := range r0 {
		want := uint64(1)
		if i == 6 {
			want = 2 // 8 and 9
		}
		if got := stripeStat.reach[i].Load() - r0[i]; got != want {
			t.Fatalf("reach[%d] +%d want %d", i, got, want)
		}
	}
	if r := stripeReach(); len(r) != 7 {
		t.Fatalf("stripeReach %v", r)
	}
	if l := buf.String(); strings.Count(l, "striped over") != 1 || !strings.Contains(l, "striped over 9 subs") {
		t.Fatalf("lines %q", l)
	}
}
