package engine

import (
	"bytes"
	"testing"
)

func withB340(t *testing.T, skip, clear bool) {
	t.Helper()
	os, oc := b340SkipCut.Load(), b340CutClear.Load()
	b340SkipCut.Store(skip)
	b340CutClear.Store(clear)
	t.Cleanup(func() { b340SkipCut.Store(os); b340CutClear.Store(oc) })
}

// b340 (lab/rb339 R4 31006): a light copy / call copy / rescue placement
// goes off cut-marked links while an unmarked one takes it; only with none
// left does it land on a marked one. The marked links here are the ones the
// usual order prefers (no download), so without the key the picks land
// there.
func TestB340SkipCutPlacement(t *testing.T) {
	withRescue(t, true)
	withRescueCut(t, true)
	withB340(t, true, true)
	m, ls := pickLinks([]int{0, 0, 2, 2}, []bool{false, false, false, false})
	rescueCutMark(nil, []any{Link(ls[0]), Link(ls[1])})
	if rescueCutCount.Load() != 2 {
		t.Fatalf("mark count %d", rescueCutCount.Load())
	}
	for _, avoid := range [][]any{nil, {callAvoidMark{}}, {callRescueMark{}}, {rescueAvoidMark{}}} {
		for i := 0; i < 4; i++ {
			l, rel, ok := m.pickPlaneCall(planeLight, avoid)
			if !ok || rescueCutKey(l) {
				t.Fatalf("avoid %v: light sub on link %d (cut-marked)", avoid, linkIndex(ls, l))
			}
			rel()
		}
	}
	if b340Skipped.Load() == 0 {
		t.Fatal("skip pass not counted")
	}
	// the caller's avoid list is never written through
	av := make([]any, 1, 4)
	av[0] = Link(ls[2])
	l, rel, ok := m.pickPlaneCall(planeLight, av)
	if !ok || linkIndex(ls, l) != 3 || len(av) != 1 || av[:2][1] != nil {
		t.Fatalf("avoided link 2: got %d, avoid %v", linkIndex(ls, l), av[:2])
	}
	rel()
	// every unmarked link avoided: the fallback takes a marked one
	fb := b340Fallback.Load()
	l, rel, ok = m.pickPlaneCall(planeLight, []any{Link(ls[2]), Link(ls[3])})
	if !ok || !rescueCutKey(l) {
		t.Fatalf("fallback: ok %v link %d", ok, linkIndex(ls, l))
	}
	rel()
	if b340Fallback.Load() != fb+1 {
		t.Fatal("fallback not counted")
	}
	// heavy plane: untouched
	if skipCutOn(planeBulk) {
		t.Fatal("skip pass on the bulk plane")
	}
	// key off: the usual order (the marked, download-free links)
	b340SkipCut.Store(false)
	onCut := b340OnCut.Load()
	l, rel, ok = m.pickPlaneCall(planeLight, nil)
	if !ok || !rescueCutKey(l) {
		t.Fatalf("key off: link %d, want a marked one (the test's premise)", linkIndex(ls, l))
	}
	rel()
	if b340OnCut.Load() != onCut+1 {
		t.Fatal("placement on a cut link not counted")
	}
	// marks gone: no skip pass at all
	b340SkipCut.Store(true)
	rescueCutMark(map[any]bool{Link(ls[0]): true, Link(ls[1]): true}, nil)
	if rescueCutCount.Load() != 0 || skipCutOn(planeLight) {
		t.Fatalf("unmarked: count %d", rescueCutCount.Load())
	}
}

// b340: a path still reported cut whose readings are clean again (the cut
// over, only the recovery hold running) leaves its links lowered but not
// cut-marked; with HS2_B340_CUTCLEAR=0 they stay marked.
func TestB340CutClearMarks(t *testing.T) {
	withRescue(t, true)
	withRescueCut(t, true)
	withB340(t, true, true)
	m := &LinkManager{steer: newSteerState()}
	st := m.steer
	la, lb, lc := &fakeLink{alive: true}, &fakeLink{alive: true}, &fakeLink{alive: true}
	byPath := map[string][]*managedLink{"a": {{link: la}}, "b": {{link: lb}}, "c": {{link: lc}}}
	st.active = true
	st.repFactor = map[string]float64{"a": 1, "b": 0.05, "c": 0.05}
	st.repCut = map[string]bool{"b": true, "c": true}
	st.repOK = map[string]bool{"c": true}
	m.steerRescueLocked(byPath)
	if !rescueCutKey(Link(lb)) || rescueCutKey(Link(lc)) {
		t.Fatal("b (cut) must be marked, c (cut, clean again) must not")
	}
	if !rescueLowKey(Link(lc)) {
		t.Fatal("c must stay lowered until it recovers")
	}
	// c marked earlier, then its readings clean: the mark goes
	st.repOK = map[string]bool{}
	m.steerRescueLocked(byPath)
	if !rescueCutKey(Link(lc)) {
		t.Fatal("c cut again: not marked")
	}
	st.repOK = map[string]bool{"c": true}
	m.steerRescueLocked(byPath)
	if rescueCutKey(Link(lc)) || rescueCutCount.Load() != 1 {
		t.Fatalf("c clean: still marked (count %d)", rescueCutCount.Load())
	}
	b340CutClear.Store(false)
	m.steerRescueLocked(byPath)
	if !rescueCutKey(Link(lc)) {
		t.Fatal("key off: c must be marked")
	}
}

// The dialing side reports a cut path's clean readings (cutReadsClean) in a
// bit older readers ignore; the steering side keeps it per path.
func TestB340ReportOK(t *testing.T) {
	old := lbFastRecover
	lbFastRecover = true
	t.Cleanup(func() { lbFastRecover = old })
	q := &q2Path{cut: true, okRuns: lbFastOKRuns - 1}
	if cutReadsClean(q) {
		t.Fatal("clean before lbFastOKRuns readings")
	}
	q.okRuns = lbFastOKRuns
	if !cutReadsClean(q) {
		t.Fatal("not clean after lbFastOKRuns readings")
	}
	q.cut = false
	if cutReadsClean(q) {
		t.Fatal("clean on a path not cut")
	}
	rep := LBReport{Auto: true, Paths: []LBPeerPath{
		{Local: "10.0.0.1", Remote: "10.1.0.1", Cut: true, OK: true},
		{Local: "10.0.0.2", Remote: "10.1.0.2", Cut: true},
		{Local: "10.0.0.3", Remote: "10.1.0.3"},
	}}
	got, ok := readReport(bytes.NewReader(encodeReport(rep)))
	if !ok || len(got.Paths) != 3 {
		t.Fatalf("decode: %v %d", ok, len(got.Paths))
	}
	if !got.Paths[0].Cut || !got.Paths[0].OK || !got.Paths[1].Cut || got.Paths[1].OK || got.Paths[2].Cut || got.Paths[2].OK {
		t.Fatalf("bits: %+v", got.Paths)
	}
}
