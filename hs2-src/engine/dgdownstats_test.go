package engine

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// The download-stats backchannel (reverse-asym fix). The pool is sized by the
// EDGE, which is the download RECEIVER and so never sees the download sender's
// send-queue pressure; a download-bound pool would shrink to min and throttle
// the download to a few carriers. These tests exercise the wiring that carries
// the download sender's pressure to the sizer: the producer (exit) encodes it,
// the consumer (edge) stores it, and sampleHealth folds it in.

// edgeWithLinks builds an edge pool (the sizer/consumer) with n serving
// carriers and a fake clock, and returns the pool and its links. No goroutines
// run: the test drives sampleHealth by hand.
func edgeWithLinks(t *testing.T, n int) (*dgPool, []*dgLink, *time.Time) {
	t.Helper()
	p := newDgPool(newFakeTUN(1400), 2, 32, 8, func(string, ...any) {})
	clk := time.Unix(1_000_000, 0)
	p.clock = func() time.Time { return clk }
	var ls []*dgLink
	for i := 0; i < n; i++ {
		a, _ := newDgFakePair() // warm carrier; its channels are unused here
		l := newDgLink(a, clk)
		p.set = append(p.set, l)
		ls = append(ls, l)
	}
	return p, ls, &clk
}

// tickDownload advances one health tick: link i receives (i+1)*unit download
// bytes (so link n-1 is the busiest), then sampleHealth runs. It returns the
// number of serving links the sample marks pressed.
func tickDownload(p *dgPool, ls []*dgLink, clk *time.Time, unit int) int {
	*clk = clk.Add(healthTick)
	now := *clk
	for i, l := range ls {
		l.noteFlowRecv(uint32(i+1), (i+1)*unit, now)
	}
	s := p.sampleHealth()
	pressed := 0
	for _, l := range s.links {
		if l.serving && l.pressed {
			pressed++
		}
	}
	return pressed
}

// Without a report the edge sees no download pressure; a fresh report marks the
// busiest-by-download carriers pressed; a stale report is ignored again.
func TestDgDownStatsFoldsInPressure(t *testing.T) {
	p, ls, clk := edgeWithLinks(t, 4)

	// Two ticks of pure download, no report: the edge is the receiver, so none
	// of its carriers is pressed (pressure is a send-side signal).
	for i := 0; i < 2; i++ {
		if got := tickDownload(p, ls, clk, 50_000); got != 0 {
			t.Fatalf("no report: %d links pressed, want 0 (the edge never sends the download)", got)
		}
	}

	// The exit reports 3 of its 4 serving carriers pressed on the download.
	p.onLinkStats(statsFrame(3, 4))
	if got := tickDownload(p, ls, clk, 50_000); got != 3 {
		t.Fatalf("fresh report of 3: %d links pressed, want 3", got)
	}
	// The least-busy carrier (link 0) must be the one left unpressed.
	s := p.sampleHealth()
	if s.links[0].pressed {
		t.Fatalf("the least-busy carrier was marked pressed; want the 3 busiest only")
	}

	// Let the report go stale (older than dgDownStatsStale): back to upload-only
	// sizing — no download pressure.
	*clk = clk.Add(dgDownStatsStale + healthTick)
	if got := tickDownload(p, ls, clk, 50_000); got != 0 {
		t.Fatalf("stale report: %d links pressed, want 0", got)
	}
}

// The folded pressure is max(up, down): an edge carrier already pressed on the
// upload counts toward the exit's reported count rather than adding to it, so a
// shared pool is never over-sized by double-counting the two directions.
func TestDgDownStatsMaxOfBothDirections(t *testing.T) {
	p, ls, clk := edgeWithLinks(t, 4)
	// Warm up the per-link rate model with a tick of download.
	tickDownload(p, ls, clk, 50_000)

	// Exit says 3 carriers are download-pressed; meanwhile ONE edge carrier is
	// genuinely upload-pressed (its send queue overflowed this tick).
	p.onLinkStats(statsFrame(3, 4))
	*clk = clk.Add(healthTick)
	now := *clk
	for i, l := range ls {
		l.noteFlowRecv(uint32(i+1), (i+1)*50_000, now)
	}
	ls[0].droppedAt.Store(now.UnixNano()) // link 0: real upload pressure

	s := p.sampleHealth()
	pressed := 0
	for _, l := range s.links {
		if l.serving && l.pressed {
			pressed++
		}
	}
	if pressed != 3 {
		t.Fatalf("1 upload-pressed + report of 3 => %d pressed, want max(1,3)=3 (no double count)", pressed)
	}
	if !s.links[0].pressed {
		t.Fatalf("the genuinely upload-pressed carrier was not counted")
	}
}

// The producer encodes the download pressure the consumer decodes: a pre-
// TypeLinkStats peer would drop the frame, so the round trip is the contract.
func TestDgDownStatsWireRoundTrip(t *testing.T) {
	// Exit side: a pool that sends, with one carrier over a fake pair.
	exit := newDgPool(newFakeTUN(1400), 2, 32, 8, func(string, ...any) {})
	exit.downSender = true
	a, b := newDgFakePair()
	exitLink := newDgLink(a, time.Unix(1_000_000, 0))
	exit.set = append(exit.set, exitLink)

	// A sample in which 2 of 3 serving carriers are pressed on the download.
	s := apSample{links: []apLink{
		{serving: true, pressed: true},
		{serving: true, pressed: true},
		{serving: true, pressed: false},
		{serving: false, pressed: false}, // retiring: not counted
	}}
	exit.publishDownStats(s)

	ft, payload, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("no frame reached the peer: %v", err)
	}
	if ft != core.TypeLinkStats {
		t.Fatalf("frame type %d, want TypeLinkStats (%d)", ft, core.TypeLinkStats)
	}
	if got := binary.BigEndian.Uint16(payload[0:]); got != 2 {
		t.Fatalf("encoded pressed=%d, want 2", got)
	}
	if got := binary.BigEndian.Uint16(payload[2:]); got != 3 {
		t.Fatalf("encoded serving=%d, want 3", got)
	}

	// Edge side decodes it.
	edge := newDgPool(newFakeTUN(1400), 2, 32, 8, func(string, ...any) {})
	edge.onLinkStats(payload)
	if edge.dnPressed.Load() != 2 || edge.dnServing.Load() != 3 {
		t.Fatalf("edge stored pressed=%d serving=%d, want 2 and 3", edge.dnPressed.Load(), edge.dnServing.Load())
	}
	if edge.dnStatsAt.Load() == 0 {
		t.Fatal("edge did not timestamp the report")
	}
}

// The exit never acts on a report, and a short/old-peer frame is ignored.
func TestDgDownStatsGuards(t *testing.T) {
	exit := newDgPool(newFakeTUN(1400), 2, 32, 8, func(string, ...any) {})
	exit.downSender = true
	exit.onLinkStats(statsFrame(5, 5))
	if exit.dnStatsAt.Load() != 0 {
		t.Fatal("the download sender stored a report; it should only produce them")
	}
	edge := newDgPool(newFakeTUN(1400), 2, 32, 8, func(string, ...any) {})
	edge.onLinkStats([]byte{0x00}) // too short
	if edge.dnStatsAt.Load() != 0 {
		t.Fatal("a short frame was accepted")
	}
}

func statsFrame(pressed, serving uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], pressed)
	binary.BigEndian.PutUint16(b[2:], serving)
	return b[:]
}
