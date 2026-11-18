package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func withSparePath(t *testing.T, on bool) {
	t.Helper()
	old := b331SparePath
	b331SparePath = on
	t.Cleanup(func() { b331SparePath = old })
}

// sparePathLB: an exit balancer over n paths of equal weight from two exit
// IPs to edge IPs, with a settable clock and a counting past-the-cap dial.
type spRig struct {
	b     *LB
	now   time.Time
	mu    sync.Mutex
	dials int
	lines []string
	pool  func() int // nil: always starts
}

func newSPRig(t *testing.T, n int) *spRig {
	t.Helper()
	withAuto2(t)
	var ps []LBPath
	for i := 0; i < n; i++ {
		loc := fmt.Sprintf("10.1.0.%d", 1+i%2)
		r := fmt.Sprintf("10.0.0.%d", 1+i/2)
		ps = append(ps, LBPath{Local: loc, Remote: r, Addr: r + ":2082", Weight: 10, Primary: i == 0})
	}
	b, err := NewLB(ps, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuto(true)
	r := &spRig{b: b, now: time.Unix(1_800_000_000, 0)}
	b.mu.Lock()
	b.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
	b.links = make([]int, n)
	for i := range b.links {
		b.links[i] = 12
	}
	b.upAt = r.now
	b.log = func(f string, a ...any) { r.mu.Lock(); r.lines = append(r.lines, fmt.Sprintf(f, a...)); r.mu.Unlock() }
	b.mu.Unlock()
	b.setSparePast(func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.pool != nil && r.pool() == 0 {
			return 0
		}
		r.dials++
		return 1
	})
	return r
}

func (r *spRig) advance(d time.Duration) { r.mu.Lock(); r.now = r.now.Add(d); r.mu.Unlock() }
func (r *spRig) nDials() int             { r.mu.Lock(); defer r.mu.Unlock(); return r.dials }

// counts: serving per path index, as the edge reports them.
func (r *spRig) report(serv []int) {
	var cs []edgePathCount
	for j, p := range r.b.paths {
		cs = append(cs, edgePathCount{Edge: p.Remote, Exit: p.Local, Serving: serv[j], Retiring: 3})
	}
	r.b.setEdgeCounts(cs)
}

// The recovered path (serving 3 against a share of 10) gets a spare past the
// slot cap, and the next dial goes to that path (the pin).
func TestB331SparePathDialsUnderPath(t *testing.T) {
	withSparePath(t, true)
	r := newSPRig(t, 6)
	serv := []int{10, 10, 3, 10, 12, 15} // S = 60, share 10 each: path 2 is 7 under
	r.report(serv)
	if !r.b.sparePathTick(r.b.now()) || r.nDials() != 1 {
		t.Fatalf("no spare dial for the under path (dials %d)", r.nDials())
	}
	if i, _ := r.b.acquire(); i != 2 {
		t.Fatalf("pinned dial went to path %d, want 2", i)
	}
	r.b.mu.Lock()
	np := len(r.b.sp331.pins)
	r.b.mu.Unlock()
	if np != 0 {
		t.Fatalf("pin not used up: %d left", np)
	}
	if !strings.Contains(strings.Join(r.lines, "\n"), "lb sparepath: 10.1.0.1>10.0.0.2 has 3 serving") {
		t.Fatalf("log: %v", r.lines)
	}
	// 1 under only (below sparePathUnder): nothing
	r2 := newSPRig(t, 6)
	r2.report([]int{10, 10, 9, 10, 11, 10})
	if r2.b.sparePathTick(r2.b.now()) {
		t.Fatal("dialed for a path only 1 link under")
	}
}

// Rate limits: 1 per path per 30 s, 6 per minute in all.
func TestB331SparePathRate(t *testing.T) {
	withSparePath(t, true)
	r := newSPRig(t, 10)
	serv := []int{2, 2, 2, 2, 2, 2, 2, 2, 40, 40} // S = 96, share 9.6: eight paths under
	r.report(serv)
	for i := 0; i < 20; i++ {
		r.b.sparePathTick(r.b.now())
	}
	if r.nDials() != 6 {
		t.Fatalf("dials in one minute window: %d, want 6", r.nDials())
	}
	r.advance(20 * time.Second)
	r.report(serv)
	r.b.sparePathTick(r.b.now())
	if r.nDials() != 6 {
		t.Fatalf("dial within the minute cap: %d", r.nDials())
	}
	r.advance(41 * time.Second) // the first 6 are a minute old
	r.report(serv)
	for i := 0; i < 20; i++ {
		r.b.sparePathTick(r.b.now())
	}
	if r.nDials() != 12 {
		t.Fatalf("dials after a minute: %d, want 12", r.nDials())
	}
	// one path only under: at most one per 30 s
	r3 := newSPRig(t, 6)
	s3 := []int{2, 12, 12, 12, 12, 12}
	for k := 0; k < 12; k++ { // 2 minutes in 10 s steps, the spare never serves
		r3.report(s3)
		r3.b.sparePathTick(r3.b.now())
		r3.advance(10 * time.Second)
	}
	if n := r3.nDials(); n > 4 || n < 3 {
		t.Fatalf("one path, 2 min: %d dials, want 3-4 (1 per 30 s)", n)
	}
}

// No counts from the edge (an old edge), stale counts or the key off: no
// sparepath dial, and spareForGap's room check is the old one.
func TestB331SparePathOldPeer(t *testing.T) {
	withSparePath(t, true)
	r := newSPRig(t, 6)
	if r.b.sparePathTick(r.b.now()) || r.nDials() != 0 {
		t.Fatal("dialed without the edge's counts")
	}
	r.b.mu.Lock()
	eff := r.b.effWeightsLocked()
	_, known := r.b.edgeRoomServLocked(2, eff, r.b.now())
	r.b.mu.Unlock()
	if known {
		t.Fatal("serving frame without counts")
	}
	r.report([]int{10, 10, 3, 10, 12, 15})
	r.advance(sparePathFresh + time.Second)
	if r.b.sparePathTick(r.b.now()) {
		t.Fatal("dialed on stale counts")
	}
	withSparePath(t, false)
	r.report([]int{10, 10, 3, 10, 12, 15})
	if r.b.sparePathTick(r.b.now()) {
		t.Fatal("dialed with the key off")
	}
	r.b.mu.Lock()
	_, known = r.b.edgeRoomServLocked(2, eff, r.b.now())
	r.b.mu.Unlock()
	if known {
		t.Fatal("serving frame with the key off")
	}
	// key on, fresh: the room check counts the path's serving links only
	withSparePath(t, true)
	r.report([]int{10, 10, 3, 10, 12, 15})
	r.b.mu.Lock()
	k, known := r.b.edgeRoomServLocked(2, eff, r.b.now())
	r.b.mu.Unlock()
	if !known || k < 6 {
		t.Fatalf("serving-frame room %d (known %v), want >= 6", k, known)
	}
}

// No dial storm: 10 minutes of 10 s rounds; spares that the edge serves
// close the gap and the dials stop; spares the pool cannot start are not
// counted and never exceed the rate either.
func TestB331SparePathNoStorm(t *testing.T) {
	withSparePath(t, true)
	r := newSPRig(t, 13)
	serv := []int{2, 12, 12, 12, 12, 12, 12, 12, 12, 12, 12, 12, 8} // S = 142
	perMin := map[int64]int{}
	for k := 0; k < 60; k++ {
		r.report(serv)
		before := r.nDials()
		r.b.sparePathTick(r.b.now())
		if d := r.nDials() - before; d > 0 { // served at once: the edge retires one on the fullest path
			perMin[r.now.Unix()/60] += d
			r.b.mu.Lock()
			p := r.b.sp331.last
			r.b.mu.Unlock()
			for j, pp := range r.b.paths {
				if t0, ok := p[pp.Name()]; ok && t0.Equal(r.now) {
					serv[j]++
				}
			}
			mx := 0
			for j := range serv {
				if serv[j] > serv[mx] {
					mx = j
				}
			}
			serv[mx]--
		}
		r.advance(10 * time.Second)
	}
	if n := r.nDials(); n > 12 {
		t.Fatalf("%d dials for a gap of ~13 links", n)
	}
	for m, c := range perMin {
		if c > sparePathPerMin {
			t.Fatalf("minute %d: %d dials", m, c)
		}
	}
	if serv[0] < 9 || serv[12] < 9 {
		t.Fatalf("gap not closed: %v", serv)
	}
	n := r.nDials()
	for k := 0; k < 30; k++ { // balanced now: nothing more
		r.report(serv)
		r.b.sparePathTick(r.b.now())
		r.advance(10 * time.Second)
	}
	if r.nDials() != n {
		t.Fatalf("dials after balance: %d -> %d", n, r.nDials())
	}
	// the pool refuses (cap): nothing booked, pins dropped
	r2 := newSPRig(t, 6)
	r2.pool = func() int { return 0 }
	r2.report([]int{2, 12, 12, 12, 12, 12})
	for k := 0; k < 10; k++ {
		r2.b.sparePathTick(r2.b.now())
	}
	r2.b.mu.Lock()
	np, nr := len(r2.b.sp331.pins), len(r2.b.sp331.recent)
	r2.b.mu.Unlock()
	if r2.nDials() != 0 || np != 0 || nr != 0 {
		t.Fatalf("refused dials booked: dials %d pins %d recent %d", r2.nDials(), np, nr)
	}
}

// The pool: a spare past fillLimit, bounded by max_links and 2·target+8.
func TestB331AddSparePast(t *testing.T) {
	old := lbFillPct
	lbFillPct = 25
	t.Cleanup(func() { lbFillPct = old })
	tp := newTestPool(t, 1, 28)
	tp.setTarget(20)
	eventually(t, "20 links", func() bool { return tp.liveCount() == 20 })
	for i := 0; i < 10; i++ {
		tp.addSpares(2)
		eventually(t, "spares dialed", func() bool { return tp.liveCount() == tp.slots() })
	}
	if s := tp.slots(); s != fillLimit(20) {
		t.Fatalf("slots %d, want the cap %d", s, fillLimit(20))
	}
	if tp.addSpares(1) != 0 {
		t.Fatal("addSpares past the cap")
	}
	got := 0
	for i := 0; i < 10; i++ {
		got += tp.addSparePast()
		eventually(t, "spare dialed", func() bool { return tp.liveCount() == tp.slots() })
	}
	if got != 28-fillLimit(20) || tp.slots() != 28 {
		t.Fatalf("past the cap: started %d, slots %d (max 28)", got, tp.slots())
	}
}

// Wire: the block round-trips; an old exit (ver >= 2: front ext reader) keeps
// the advert and its fronts; a new exit reads both, and nothing from an old
// edge.
func TestB331ServBlockCompat(t *testing.T) {
	cs := []edgePathCount{{Edge: "10.0.0.1", Exit: "10.1.0.1", Serving: 7, Retiring: 2}, {Edge: "2001:db8::1", Exit: "2001:db8::2", Serving: 3}}
	blk := encodeServBlock(cs)
	got, err := readServBlock(bytes.NewReader(blk))
	if err != nil || len(got) != 2 || got[0] != cs[0] || got[1] != cs[1] {
		t.Fatalf("round trip: %v %v", got, err)
	}
	adv := []LBAdvertIP{{IP: "10.0.0.1", Weight: 50}, {IP: "10.0.0.2", Weight: 50}}
	msg := encodeAdvert(adv)
	msg[0] = lbAdvertVerServ
	msg = append(msg, blk...)
	// old exit (stage 6 + fronts): readAdvertVer, then readFrontExt on ver >= 2
	rd := bytes.NewReader(msg)
	l, ver, err := readAdvertVer(rd)
	if err != nil || len(l) != 2 || ver != lbAdvertVerServ {
		t.Fatalf("old exit advert: %v %d %v", l, ver, err)
	}
	if _, err := readFrontExt(rd); err == nil {
		t.Fatal("old exit took the block as a front ext")
	}
	// new exit: serve() over a pipe gets the counts
	var mu sync.Mutex
	var seen []edgePathCount
	a := NewLBAdvert(nil, nil, nil)
	a.OnServing(func(c []edgePathCount) { mu.Lock(); seen = c; mu.Unlock() })
	run := func(msg []byte) {
		c1, c2 := net.Pipe()
		done := make(chan struct{})
		go func() { a.serve(c2); close(done) }()
		go c1.Write(msg)
		io.Copy(io.Discard, c1)
		<-done
		c1.Close()
	}
	run(msg)
	mu.Lock()
	if len(seen) != 2 || seen[0].Serving != 7 {
		t.Fatalf("new exit counts: %v", seen)
	}
	seen = nil
	mu.Unlock()
	if st := a.State(); len(st.Peer) != 2 {
		t.Fatalf("new exit advert: %+v", st)
	}
	// old edge (ver 1, nothing after): no counts
	run(encodeAdvert(adv))
	mu.Lock()
	if seen != nil {
		t.Fatalf("counts from an old edge: %v", seen)
	}
	mu.Unlock()
	// LB side: exit-name mapping
	r := newSPRig(t, 4)
	r.b.setEdgeCounts([]edgePathCount{{Edge: "10.0.0.2", Exit: "10.1.0.1", Serving: 5}})
	r.b.mu.Lock()
	v := r.b.sp331.serv["10.1.0.1>10.0.0.2"]
	r.b.mu.Unlock()
	if v != 5 {
		t.Fatalf("exit path name mapping: %d", v)
	}
}

// Edge: an arriving link serves when its path is under its share of the
// serving links; over: the per-address rule decides; key off: unknown.
func TestB331ArrivalPerPath(t *testing.T) {
	withSparePath(t, true)
	r := newSwapRig(t)
	r.m.mu.Lock()
	var serving []*managedLink
	for _, ml := range r.m.links {
		if ml.serving() {
			serving = append(serving, ml)
		}
	}
	okA, knownA := r.m.arrivalPathLocked("a>x", serving)
	okB, knownB := r.m.arrivalPathLocked("b>x", serving)
	r.m.mu.Unlock()
	if !knownA || !okA {
		t.Fatalf("a>x (2 of share 6): ok %v known %v", okA, knownA)
	}
	if !knownB || okB {
		t.Fatalf("b>x (9 of share 6): ok %v known %v", okB, knownB)
	}
	cs := r.m.edgePathCounts()
	got := map[string]edgePathCount{}
	for _, c := range cs {
		got[c.Edge+">"+c.Exit] = c
	}
	if got["a>x"].Serving != 2 || got["a>x"].Retiring != 5 || got["b>x"].Serving != 9 {
		t.Fatalf("edge counts: %+v", cs)
	}
	withSparePath(t, false)
	r.m.mu.Lock()
	_, known := r.m.arrivalPathLocked("a>x", serving)
	r.m.mu.Unlock()
	if known {
		t.Fatal("per-path arrival with the key off")
	}
}

// A path whose weight factor dipped under 0.95 in the last minute gets no
// spare (rb331 v4: a loss-flapping path got spares the edge refused).
func TestB331SparePathFactorSteady(t *testing.T) {
	withSparePath(t, true)
	r := newSPRig(t, 6)
	serv := []int{10, 10, 3, 10, 12, 15}
	setMult := func(v float64) {
		r.b.mu.Lock()
		if len(r.b.mult) < 6 {
			r.b.mult = []float64{1, 1, 1, 1, 1, 1}
		}
		r.b.mult[2] = v
		r.b.mu.Unlock()
	}
	setMult(0.4)
	r.report(serv)
	if r.b.sparePathTick(r.b.now()) || r.nDials() != 0 {
		t.Fatalf("spare for a path at factor 0.4 (dials %d)", r.nDials())
	}
	setMult(1)
	r.advance(30 * time.Second)
	r.report(serv)
	if r.b.sparePathTick(r.b.now()) || r.nDials() != 0 {
		t.Fatalf("spare 30 s after the factor was low (dials %d)", r.nDials())
	}
	r.advance(35 * time.Second)
	r.report(serv)
	if !r.b.sparePathTick(r.b.now()) || r.nDials() != 1 {
		t.Fatalf("no spare once the factor was steady a minute (dials %d)", r.nDials())
	}
}

// b336: the spare goes only to a path whose edge IP is also under its target
// (the edge's own, block ver 2); at/over target: skipped (the v6 dial loop).
func TestB336SparePathEdgeIPGate(t *testing.T) {
	withSparePath(t, true)
	rep := func(r *spRig, serv []int, tgt map[string]int) {
		var cs []edgePathCount
		for j, p := range r.b.paths {
			c := edgePathCount{Edge: p.Remote, Exit: p.Local, Serving: serv[j]}
			if v, ok := tgt[p.Remote]; ok {
				c.EdgeTarget, c.HasTarget = v, true
			}
			cs = append(cs, c)
		}
		r.b.setEdgeCounts(cs)
	}
	// path 2 (10.1.0.1>10.0.0.2) 7 under its path share, but its IP 10.0.0.2
	// (paths 2+3: 3+17 = 20) is at the edge's target 20: no dial
	r := newSPRig(t, 6)
	rep(r, []int{10, 10, 3, 17, 10, 10}, map[string]int{"10.0.0.1": 20, "10.0.0.2": 20, "10.0.0.3": 20})
	if r.b.sparePathTick(r.b.now()) || r.nDials() != 0 {
		t.Fatalf("dialed for a path whose edge IP is at its target (dials %d)", r.nDials())
	}
	r.b.mu.Lock()
	sk := r.b.sp331.ipSkip
	r.b.mu.Unlock()
	if sk == 0 {
		t.Fatal("skip not counted")
	}
	// over target too
	r = newSPRig(t, 6)
	rep(r, []int{10, 10, 3, 17, 10, 10}, map[string]int{"10.0.0.2": 18})
	if r.b.sparePathTick(r.b.now()) {
		t.Fatal("dialed for an edge IP over its target")
	}
	// IP 1 under its target: dial
	r = newSPRig(t, 6)
	rep(r, []int{10, 10, 3, 16, 10, 10}, map[string]int{"10.0.0.2": 20})
	if !r.b.sparePathTick(r.b.now()) || r.nDials() != 1 {
		t.Fatal("no dial for a path whose edge IP is under its target")
	}
	// old edge (no targets): the exit's per-IP share (2 paths x 10 = 20)
	r = newSPRig(t, 6)
	rep(r, []int{10, 10, 3, 17, 10, 10}, nil)
	if r.b.sparePathTick(r.b.now()) {
		t.Fatal("old edge: dialed for an IP at its share sum")
	}
	r = newSPRig(t, 6)
	rep(r, []int{10, 10, 3, 10, 10, 10}, nil)
	if !r.b.sparePathTick(r.b.now()) {
		t.Fatal("old edge: no dial for an IP under its share sum")
	}
}

// b336: block ver 2 carries the per-IP targets; a ver-1 reader (old exit)
// still reads the counts (it reads len bytes and n entries only).
func TestB336ServBlockTargets(t *testing.T) {
	cs := []edgePathCount{
		{Edge: "10.0.0.1", Exit: "10.1.0.1", Serving: 7, Retiring: 2, EdgeTarget: 9, HasTarget: true},
		{Edge: "10.0.0.1", Exit: "10.1.0.2", Serving: 3, EdgeTarget: 9, HasTarget: true},
		{Edge: "2001:db8::1", Exit: "2001:db8::2", Serving: 3},
	}
	blk := encodeServBlock(cs)
	got, err := readServBlock(bytes.NewReader(blk))
	if err != nil || len(got) != 3 || got[0] != cs[0] || got[1] != cs[1] || got[2] != cs[2] {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// ver-1 reader: same bytes with ver 1 -> counts, no targets
	old := append([]byte(nil), blk...)
	old[1] = 1
	got, err = readServBlock(bytes.NewReader(old))
	if err != nil || len(got) != 3 || got[0].Serving != 7 || got[0].HasTarget {
		t.Fatalf("ver-1 read: %+v %v", got, err)
	}
	// truncated targets: counts kept
	bad := append([]byte(nil), blk[:len(blk)-1]...)
	binary.BigEndian.PutUint16(bad[2:], uint16(len(bad)-4))
	got, err = readServBlock(bytes.NewReader(bad))
	if err != nil || len(got) != 3 || got[1].Serving != 3 {
		t.Fatalf("truncated targets: %+v %v", got, err)
	}
}
