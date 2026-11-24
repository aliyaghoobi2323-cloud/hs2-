package censor

import (
	"bufio"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// b437 Q2: the detector and a ZDU of the other server (or of this one).
// Prod 2026-11-20 12:41:47: the old Kharej t1 process (rc7) ended its drain
// and closed the 120 links it had drained; 5 s later Iran t1 opened two
// episodes, "rst-inject conf=low rst=3/4 eof=16/11", on 2 Iran IPs.

// drainEnd feeds that close burst: on each IP eof EOFs and rst resets at
// injection timing (Iran side: reset 5 ms after our last send on a 90 ms
// path), on long-lived links. old: they are links of the peer's older
// process (what censorDeath sends for a peerOld link).
func (s *sim) drainEnd(ip netip.Addr, eof, rst int, old bool) {
	feed := func(d Death) {
		if !old {
			s.death(ip, ipK1, d)
			return
		}
		d.Local, d.Remote = s.lr(ip, ipK1)
		s.ev(event{kind: evGen, stage: genOldEnd, local: d.Local, remote: d.Remote, d: d})
	}
	for i := 0; i < eof; i++ {
		feed(Death{Class: DeathEOF, LifeMs: 1800000, Bytes: 1 << 22, HaveTCP: true, LastSentMs: 900, MinRTTUs: 90000})
	}
	for i := 0; i < rst; i++ {
		feed(Death{Class: DeathRST, LifeMs: 1800000, Bytes: 1 << 22, HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
	}
}

func (s *sim) censorshipLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.lines {
		if !strings.Contains(l, " ep=") {
			continue
		}
		for i := 0; i < NClasses; i++ {
			if IsCensorship(i) && field(l, "class") == className[i] {
				out = append(out, l)
			}
		}
	}
	return out
}

// The same burst twice: as plain deaths (the rc10 path; the control) and
// as the old peer generation's link ends. The second must give no
// censorship verdict and no episode at all (nothing else went wrong).
func TestGen437DrainEndNotCensorship(t *testing.T) {
	run := func(old bool) *sim {
		s := newSim(t, SideIran)
		s.quiet(6)
		s.drainEnd(ipA, 16, 3, old)
		s.drainEnd(ipB, 11, 4, old)
		s.quiet(30)
		return s
	}
	ctl := run(false)
	t.Logf("control (plain deaths): %d censorship lines %q", len(ctl.censorshipLines()), ctl.censorshipLines())
	s := run(true)
	if l := s.censorshipLines(); len(l) != 0 {
		t.Fatalf("old generation's drain end judged censorship:\n%s", strings.Join(l, "\n"))
	}
	if n := len(s.closes(ipA)) + len(s.closes(ipB)); n != 0 {
		t.Fatalf("old generation's drain end opened %d episodes:\n%s", n, strings.Join(s.lines, "\n"))
	}
	if s.e.nGenOld != uint64(16+3+11+4) {
		t.Fatalf("gen_old %d, want %d", s.e.nGenOld, 16+3+11+4)
	}
}

// A peer generation change is handled like a peer restart: an episode it
// opens gets the peer grace (labelled peer-zdu), and censorship is capped
// at low. A burst on plain (current generation) links right after a new
// peer generation keeps the evidence (the deaths count) but not high.
func TestGen437PeerNewGenerationGrace(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	s.ev(event{kind: evGen, stage: genPeerNew})
	s.drainEnd(ipA, 4, 8, false)
	s.tick()
	s.quiet(30)
	cl := s.closes(ipA)
	if len(cl) == 0 {
		t.Fatalf("no episode:\n%s", strings.Join(s.lines, "\n"))
	}
	if g := field(cl[0], "grace"); g != "peer-zdu" {
		t.Fatalf("grace %q, want peer-zdu:\n%s", g, cl[0])
	}
	s.noCensorship(ipA, confLow)
	if s.e.nGenNew != 1 {
		t.Fatalf("gen_new %d", s.e.nGenNew)
	}
}

// One grace per change: the rest of a close burst spread over seconds does
// not move the grace past the episode its first part opened.
func TestGen437BurstOpensOneGrace(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	s.drainEnd(ipA, 2, 0, true)
	first := s.e.peerRestart
	for i := 0; i < 4; i++ {
		s.tick()
		s.drainEnd(ipA, 2, 1, true)
	}
	if !s.e.peerRestart.Equal(first) || !s.e.peerZDU {
		t.Fatalf("peer grace moved from %v to %v (zdu %v)", first, s.e.peerRestart, s.e.peerZDU)
	}
	s.now = s.now.Add(peerGrace)
	s.drainEnd(ipA, 1, 0, true) // a later old-link end (idle reclaim): a new grace
	if !s.e.peerRestart.After(first) {
		t.Fatal("a later generation event opened no grace")
	}
}

// Prod verdict #7 (Kharej t1 12:14:47): real loss 3m50s after a ZDU
// takeover read "own-restart high grace=own" (5 min start grace). With the
// handover's end told (probation passed ~50 s after the start) the own
// grace ends there, and the loss is judged as loss. Control: a cold start
// (no handover told) keeps the 5 min own grace.
func TestGen437OwnGraceScopedToHandover(t *testing.T) {
	run := func(zdu bool) *sim {
		s := newSim(t, SideKharej)
		s.now = time.Date(2026, 11, 19, 17, 10, 0, 0, time.UTC)
		s.e.start = s.now
		if zdu {
			s.ev(event{kind: evGen, stage: genOwnTakeover})
		}
		s.quiet(10) // 50 s: probation
		if zdu {
			s.ev(event{kind: evGen, stage: genOwnDone})
		}
		s.quiet(36) // 3 min later
		for i := 0; i < 60; i++ {
			s.healthy(ipB)
			q := 20 + float64(i%12)*10
			for _, kh := range []netip.Addr{ipK1, ipK2} {
				s.path(ipA, kh, PathSample{Loss: 0.01 + q/3000, QueueMs: q, RateBps: 7e6})
			}
			s.tick()
		}
		s.quiet(24)
		return s
	}
	ctl := run(false)
	if cl := ctl.closes(ipA); len(cl) != 1 || field(cl[0], "grace") != "own" {
		t.Fatalf("control (cold start): want one episode with grace=own:\n%s", strings.Join(ctl.lines, "\n"))
	}
	s := run(true)
	s.expect(ipA, "congestion", confMed)
	if g := field(s.closes(ipA)[0], "grace"); g != "no" {
		t.Fatalf("grace %q after the handover ended", g)
	}
}

// Inside the handover the own grace holds and is labelled own-zdu.
func TestGen437OwnGraceLabel(t *testing.T) {
	s := newSim(t, SideKharej)
	s.e.start = s.now
	s.ev(event{kind: evGen, stage: genOwnTakeover})
	for i := 0; i < 6; i++ {
		s.healthy(ipB)
		s.dial(ipA, ipK1, DialConnTimeout)
		s.dial(ipA, ipK2, DialConnTimeout)
		s.tick()
	}
	s.quiet(24)
	cl := s.closes(ipA)
	if len(cl) != 1 || field(cl[0], "grace") != "own-zdu" {
		t.Fatalf("want one own-zdu episode:\n%s", strings.Join(s.lines, "\n"))
	}
}

// The recorder keeps a generation event, death included, for replays.
func TestGen437RecordRoundTrip(t *testing.T) {
	r := recorder{w: bufio.NewWriter(io.Discard)}
	d := Death{Local: ipA, Remote: ipK1, Class: DeathRST, LifeMs: 7, Bytes: 9, HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000}
	ev := event{kind: evGen, stage: genOldEnd, local: ipA, remote: ipK1, d: d, at: 42}
	r.event(&ev)
	got := decode(&r.buf)
	if got.kind != evGen || got.stage != genOldEnd || got.d != d || got.at != 42 {
		t.Fatalf("round trip %+v", got)
	}
}
