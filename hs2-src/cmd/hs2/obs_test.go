package main

import (
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// The window reports what happened since its oldest snapshot, and forgets
// what is older than a minute.
func TestObsWindow(t *testing.T) {
	var w obsWindow
	var ls liveStatus
	s := engine.ObsSnap{ShapeWrites: 10, ShapeRecords: 30, ShapeData: 1000, ShapePad: 500, ExitDials: 4, ExitNoAddr: 2}
	w.fill(&ls, s)
	if ls.ObsShapeRecPerWrite != 0 || ls.ObsExitDials != 0 {
		t.Fatalf("first snapshot has no window yet: %+v", ls)
	}
	if ls.ObsExitNoAddr != 2 {
		t.Fatalf("failures since start: %d, want 2", ls.ObsExitNoAddr)
	}
	s.ShapeWrites, s.ShapeRecords, s.ShapeData, s.ShapePad = 20, 60, 3000, 700
	s.ExitDials = 9
	ls = liveStatus{}
	w.fill(&ls, s)
	if ls.ObsShapeRecPerWrite != 3 || ls.ObsShapeAddPct != 10 || ls.ObsExitDials != 5 {
		t.Fatalf("window: %.1f rec/write, %.1f%% added, %d dials; want 3, 10, 5", ls.ObsShapeRecPerWrite, ls.ObsShapeAddPct, ls.ObsExitDials)
	}
	for i := 0; i < obsWindowLen+3; i++ {
		w.fill(&ls, s)
	}
	if len(w.ring) != obsWindowLen {
		t.Fatalf("ring holds %d, want %d", len(w.ring), obsWindowLen)
	}
	if ls.ObsShapeRecPerWrite != 0 || ls.ObsExitDials != 0 {
		t.Fatalf("a quiet minute reports nothing: %+v", ls)
	}
}

func TestObsLines(t *testing.T) {
	if l := obsLines(liveStatus{}); l != nil {
		t.Fatalf("nothing measured, lines %q", l)
	}
	l := obsLines(liveStatus{ObsShapeRecPerWrite: 2.5, ObsShapeAddPct: 40, ObsCtrlRTTP50: 90, ObsCtrlRTTP99: 400, ObsExitNoAddr: 3})
	all := strings.Join(l, "\n")
	for _, want := range []string{"2.5 TLS record(s) per write, +40.0%", "p50 90 ms · p99 400 ms", "3 no free local port"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}
