package engine

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

// b436 (zdu_report436.go): during a Kharej drain the edge takes the LB path
// report from the current instance's links, and a draining exit sends none.

func TestZDUReportLinksCurrentFirst(t *testing.T) {
	setZDU(t, true)
	m, ls := zduPool(3, 2)
	var in []Link
	for _, l := range ls { // old, old, old, new, new: the map order can be any
		in = append(in, l)
	}
	before := zduRepCur.Load()
	out := zduReportLinks(in, m.zduCurrent)
	if len(out) != len(in) {
		t.Fatalf("len %d, want %d (no link dropped: old ones stay the fallback)", len(out), len(in))
	}
	for i, l := range out {
		want := zduInstNew
		if i >= 2 {
			want = zduInstOld
		}
		if g := linkZDUInst(l); g != want {
			t.Fatalf("link %d is %+v, want %+v (current first, then old)", i, g, want)
		}
	}
	if zduRepCur.Load() != before+1 {
		t.Fatalf("zdu_rep_cur did not count the reorder")
	}
	// the new process died in probation: the old instance is current again
	zduDropInst(m, zduInstNew)
	var olds []Link
	for _, l := range ls[:3] {
		olds = append(olds, l)
	}
	if out := zduReportLinks(olds, m.zduCurrent); len(out) != 3 || &out[0] != &olds[0] {
		t.Fatalf("one instance only: order must be unchanged")
	}
}

func TestZDUReportLinksOffUnchanged(t *testing.T) {
	setZDU(t, false)
	m, ls := zduPool(3, 2)
	var in []Link
	for _, l := range ls {
		in = append(in, l)
	}
	out := zduReportLinks(in, m.zduCurrent)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("HS2_ZDU off: order changed at %d", i)
		}
	}
	setZDU(t, true)
	if out := zduReportLinks(in, nil); out[0] != in[0] {
		t.Fatalf("no current func: order changed")
	}
	mp, _ := zduPool(0, 0)
	pre := []Link{zduLink(zduInst{}), zduLink(zduInst{})}
	if out := zduReportLinks(pre, mp.zduCurrent); out[0] != pre[0] || out[1] != pre[1] {
		t.Fatalf("pre-ZDU peer: order changed")
	}
}

func TestZDUReportOutDraining(t *testing.T) {
	setZDU(t, true)
	rep := encodeReport(LBReport{Auto2: true, Paths: []LBPeerPath{{Local: "192.168.97.2", Remote: "192.168.97.14", Cut: true}}})
	f := func() []byte { return rep }
	if got := zduReportOut(f); !bytes.Equal(got, rep) {
		t.Fatalf("not draining: report must be sent")
	}
	zduDraining.Store(true)
	t.Cleanup(func() { zduDraining.Store(false) })
	held := zduRepHeld.Load()
	if got := zduReportOut(f); got != nil {
		t.Fatalf("draining: report sent (%d bytes)", len(got))
	}
	if zduRepHeld.Load() != held+1 {
		t.Fatalf("zdu_rep_held did not count")
	}
	// wire: an answer without the report reads as "no report" (the edge
	// keeps the last one), with or without the front ext after it
	if _, got := readFrontTail(bufio.NewReader(bytes.NewReader(nil)), nil); got {
		t.Fatalf("empty tail read as a report")
	}
	r, got := readFrontTail(bufio.NewReader(bytes.NewReader(rep)), nil)
	if !got || len(r.Paths) != 1 || !r.Paths[0].Cut {
		t.Fatalf("report tail not read back: %+v %v", r, got)
	}
}

// The steer keeps the last fresh report when an answer carries none: a held
// report from the draining exit does not clear the new one's.
func TestZDUReportHeldKeepsLast(t *testing.T) {
	a := NewLBAdvert(nil, nil, nil)
	var seen []LBReport
	a.OnReport(func(r LBReport) { seen = append(seen, r) })
	a.gotReport(LBReport{Auto2: true, Paths: []LBPeerPath{{Local: "k", Remote: "i"}}})
	if _, got := readFrontTail(bufio.NewReader(bytes.NewReader(nil)), nil); got {
		a.gotReport(LBReport{})
	}
	r, ok := a.PeerReport()
	if !ok || len(r.Paths) != 1 || len(seen) != 1 || time.Since(r.At) > time.Minute {
		t.Fatalf("last report lost: %+v %v seen %d", r, ok, len(seen))
	}
}
