package engine

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func seedTestLB(t *testing.T, now *time.Time) *LB {
	t.Helper()
	b, err := NewLB([]LBPath{
		{Local: "k", Remote: "a", Addr: "a:1", Weight: 34, Primary: true},
		{Local: "k", Remote: "b", Addr: "b:1", Weight: 33},
		{Local: "k", Remote: "c", Addr: "c:1", Weight: 33},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuto(true)
	b.now = func() time.Time { return *now }
	return b
}

func seedFeed(b *LB, now *time.Time, n int, la, lb_, lc float64) {
	for i := 0; i < n; i++ {
		*now = now.Add(5 * time.Second)
		rs := map[int]q2Reading{}
		for j, l := range []float64{la, lb_, lc} {
			if l >= 0 {
				rs[j] = q2Reading{loss: l}
			}
		}
		b.adapt2(rs)
	}
}

func seedDoc(t *testing.T, b *LB, wall time.Time) *lbSeedDoc {
	t.Helper()
	s, ok := b.exportSeed(wall)
	if !ok {
		t.Fatal("nothing to export")
	}
	raw, _ := json.Marshal(lbSeedDoc{At: wall.UnixNano(), L: []lbSeedLB{s}})
	var d lbSeedDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// b438: the new judge starts from the old one's verdicts (cut path low at
// once, not "no reading yet" at 1), and from then on behaves exactly as the
// old judge would have on the same readings.
func TestLBSeedCarriesVerdictsAndContinues(t *testing.T) {
	withAuto2(t)
	t0 := time.Unix(5000, 0)
	tOld := t0
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 3, 0.003, 0.003, 0.003)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	if q := old.judge.p["k>b"]; !q.cut || old.mult[1] > 0.11 {
		t.Fatalf("old judge: cut %v factor %.2f", q.cut, old.mult[1])
	}
	wall := time.Now()
	doc := seedDoc(t, old, wall)

	tNew := tOld.Add(2 * time.Second)
	nw := seedTestLB(t, &tNew)
	if n := nw.applySeed(doc, wall.Add(2*time.Second), false); n != 3 {
		t.Fatalf("seeded %d paths, want 3", n)
	}
	if q := nw.judge.p["k>b"]; !q.cut || !q.seeded || math.Abs(nw.mult[1]-old.mult[1]) > 1e-9 {
		t.Fatalf("new judge: cut %v seeded %v factor %.2f (old %.2f)", q.cut, q.seeded, nw.mult[1], old.mult[1])
	}
	r := nw.report()
	if !r.Paths[1].Cut || r.Paths[1].Factor > 0.11 || r.Paths[0].Factor < 0.99 {
		t.Fatalf("report %+v", r.Paths)
	}
	// Same readings from here: same verdicts, step by step (the old one
	// gets them 2 s earlier on its clock; the new one is 2 s behind).
	tOld = tNew
	for i := 0; i < 30; i++ {
		l := 0.35
		if i >= 4 {
			l = 0.002 // the path heals: both recover it the same way
		}
		seedFeed(old, &tOld, 1, 0.003, l, 0.003)
		seedFeed(nw, &tNew, 1, 0.003, l, 0.003)
		for j := range old.mult {
			if math.Abs(old.mult[j]-nw.mult[j]) > 1e-9 {
				t.Fatalf("tick %d path %d: old %.3f new %.3f", i, j, old.mult[j], nw.mult[j])
			}
		}
	}
	if nw.judge.p["k>b"].seeded || nw.judge.p["k>b"].cut || nw.mult[1] <= 0.11 {
		t.Fatalf("after healing: seeded %v cut %v factor %.2f", nw.judge.p["k>b"].seeded, nw.judge.p["k>b"].cut, nw.mult[1])
	}
}

// b438: an inherited "ok" never delays a real change: a path that goes bad
// after the hand-off is cut as fast as in a fresh judge.
func TestLBSeedNeverBlocksNewVerdict(t *testing.T) {
	withAuto2(t)
	tOld := time.Unix(5000, 0)
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 6, 0.003, 0.003, 0.003)
	wall := time.Now()
	doc := seedDoc(t, old, wall)
	tA, tB := tOld, tOld
	seeded, fresh := seedTestLB(t, &tA), seedTestLB(t, &tB)
	seeded.applySeed(doc, wall, false)
	for i := 0; i < 4; i++ {
		seedFeed(seeded, &tA, 1, 0.003, 0.003, 0.4)
		seedFeed(fresh, &tB, 1, 0.003, 0.003, 0.4)
		if seeded.mult[2] > fresh.mult[2]+1e-9 {
			t.Fatalf("tick %d: seeded factor %.2f above fresh %.2f", i, seeded.mult[2], fresh.mult[2])
		}
	}
	if !seeded.judge.p["k>c"].cut {
		t.Fatal("seeded judge did not cut the new bad path")
	}
}

// b438: a stale snapshot, a different LB, or HS2_ZDU_LBSEED=0: today's
// behaviour (nothing seeded).
func TestLBSeedStaleOrForeign(t *testing.T) {
	withAuto2(t)
	tOld := time.Unix(5000, 0)
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	wall := time.Now()
	doc := seedDoc(t, old, wall)
	tn := tOld
	nw := seedTestLB(t, &tn)
	if n := nw.applySeed(doc, wall.Add(lbSeedMaxAge+time.Second), false); n != 0 || nw.judge != nil {
		t.Fatalf("stale snapshot seeded %d", n)
	}
	other, _ := NewLB([]LBPath{{Local: "k", Remote: "x", Addr: "x:1", Weight: 1, Primary: true}}, nil)
	other.SetAuto(true)
	if n := other.applySeed(doc, wall, false); n != 0 {
		t.Fatalf("foreign LB seeded %d", n)
	}
	on := lbSeedOn
	lbSeedOn = false
	defer func() { lbSeedOn = on }()
	raw, _ := json.Marshal(doc)
	if n := ZDULBStateSeed(raw, false); n != 0 {
		t.Fatalf("seeded %d with HS2_ZDU_LBSEED=0", n)
	}
}

// b438: an inherited cut the new judge never reads stays its own cut at the
// TTL (no jump to "no reading yet" = factor 1, lab b438 run 2); the GO
// refresh leaves a path the new judge read.
func TestLBSeedTTLAndRefresh(t *testing.T) {
	withAuto2(t)
	tOld := time.Unix(5000, 0)
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 3, 0.003, 0.003, 0.003)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	wall := time.Now()
	doc := seedDoc(t, old, wall)
	tn := tOld
	nw := seedTestLB(t, &tn)
	nw.applySeed(doc, wall, false)
	// path a read here; b never read; c read
	steps := int(lbSeedTTL/(5*time.Second)) + 2
	tOld = tn
	for i := 0; i < steps; i++ {
		seedFeed(nw, &tn, 1, 0.003, -1, 0.003)
		seedFeed(old, &tOld, 1, 0.003, -1, 0.003)
		// the flap/ramp history is not handed over: the new judge may probe
		// a little sooner, never jump (b438 v1 went 0.10 -> 1.00 here)
		if nw.mult[1] > old.mult[1]+0.25 {
			t.Fatalf("tick %d: unread inherited cut at %.2f, the old judge at %.2f", i, nw.mult[1], old.mult[1])
		}
	}
	if q := nw.judge.p["k>b"]; q.seeded {
		t.Fatal("unread inherited cut still marked inherited after its TTL")
	}
	if nw.judge.p["k>a"].seeded {
		t.Fatal("read path still marked inherited")
	}
	// refresh with a doc saying a is cut: a is the new judge's own now
	seedFeed(old, &tOld, 3, 0.35, 0.35, 0.003)
	doc2 := seedDoc(t, old, time.Now())
	// b442: b (an unread inherited cut the probe rule lifted) is kept as this
	// judge's own state at the TTL, so the refresh touches neither a nor b.
	n := nw.applySeed(doc2, time.Now(), true)
	if nw.judge.p["k>a"].cut || n != 0 {
		t.Fatalf("refresh touched an own-read path (n %d, a cut %v)", n, nw.judge.p["k>a"].cut)
	}
}

// b442: an inherited cut the quiet-probe rule lifts inside the TTL (a silent
// cut: the old judge had no reading for a while) is on its step-by-step way
// back; the TTL must not reset it to "no reading yet" (lab b442 run 13:
// 0.10 -> 1.00 in 15 s, re-cut, 8 links re-marked).
func TestLBSeedProbeLiftedCutKeptAtTTL(t *testing.T) {
	withAuto2(t)
	tOld := time.Unix(5000, 0)
	old := seedTestLB(t, &tOld)
	seedFeed(old, &tOld, 3, 0.003, 0.003, 0.003)
	seedFeed(old, &tOld, 3, 0.003, 0.35, 0.003)
	seedFeed(old, &tOld, 3, 0.003, -1, 0.003) // silent for 15 s
	if q := old.judge.p["k>b"]; !q.cut {
		t.Fatal("old judge did not cut b")
	}
	wall := time.Now()
	doc := seedDoc(t, old, wall)
	tn := tOld
	nw := seedTestLB(t, &tn)
	if n := nw.applySeed(doc, wall, false); n != 3 {
		t.Fatalf("seeded %d", n)
	}
	lifted := false
	steps := int(lbSeedTTL/(5*time.Second)) + 6
	for i := 0; i < steps; i++ {
		seedFeed(nw, &tn, 1, 0.003, -1, 0.003)
		seedFeed(old, &tOld, 1, 0.003, -1, 0.003)
		if !nw.judge.p["k>b"].cut {
			lifted = true
		}
		if nw.mult[1] > old.mult[1]+0.25 {
			t.Fatalf("tick %d: probe-lifted inherited cut at %.2f, the old judge at %.2f", i, nw.mult[1], old.mult[1])
		}
	}
	if !lifted {
		t.Fatal("the quiet-probe rule never lifted the inherited cut (test does not cover the case)")
	}
	if nw.judge.p["k>b"].seeded {
		t.Fatal("still marked inherited after its TTL")
	}
}
