package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func withSilent(t *testing.T, on bool) {
	t.Helper()
	o := lbSilent
	lbSilent = on
	t.Cleanup(func() { lbSilent = o })
}

// silFeed feeds n readings: loss per path, the paths in sil silent, the
// paths in neither absent.
func silFeed(j *q2Judge, now *time.Time, n int, loss map[string]float64, sil ...string) {
	for i := 0; i < n; i++ {
		*now = now.Add(5 * time.Second)
		rs := map[string]q2Reading{}
		for k, l := range loss {
			rs[k] = q2Reading{loss: l, segs: 1000}
		}
		for _, k := range sil {
			rs[k] = q2Reading{loss: silentLoss, silent: true}
		}
		j.update(*now, 5*time.Second, rs)
	}
}

func without(m map[string]float64, ks ...string) map[string]float64 {
	o := map[string]float64{}
	for k, v := range m {
		o[k] = v
	}
	for _, k := range ks {
		delete(o, k)
	}
	return o
}

func TestSilentAcc(t *testing.T) {
	var a silentAcc
	if a.silent() {
		t.Fatal("no links: not silent")
	}
	a.add(0, 0, 0) // idle link: nothing waits for an ACK
	if a.silent() {
		t.Fatal("unused path read silent")
	}
	a.add(0, 0, 2) // RTO backoff, nothing acked
	if !a.silent() {
		t.Fatal("blackholed path not silent")
	}
	a.add(1500, 3, 0) // another link of the path got acks (lossy, alive)
	if a.silent() {
		t.Fatal("alive path read silent")
	}
	var b silentAcc
	b.add(0, 4, 0) // resent, nothing acked
	if !b.silent() {
		t.Fatal("resent-only path not silent")
	}
}

// A single path blackholed in the lab mesh: lowered on the first silent
// reading, cut on the second; it recovers when bytes flow again.
func TestSilentBlackholePath(t *testing.T) {
	withSilent(t, true)
	const bh = "192.168.52.1>192.168.50.2"
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 1, without(mesh, bh), bh)
	if q := j.p[bh]; q.target >= 0.5 {
		t.Fatalf("not lowered after one silent reading: target %.2f", q.target)
	}
	silFeed(j, &now, 1, without(mesh, bh), bh)
	if c := cutSet(j); !c[bh] || len(c) != 1 {
		t.Fatalf("after two silent readings cut = %v", c)
	}
	if w := j.p[bh].cutWhy; w == "" {
		t.Fatal("no cut reason")
	}
	// Bytes flow again: clean readings recover it after the hold.
	silFeed(j, &now, 12, mesh)
	if j.p[bh].cut {
		t.Fatalf("silent path not recovered after clean readings (ewma %.3f)", j.p[bh].ewma)
	}
}

// A whole Iran IP blackholed: both its paths silent -> cut within two readings.
func TestSilentBlackholeIP(t *testing.T) {
	withSilent(t, true)
	a, b := "192.168.52.1>192.168.50.2", "192.168.52.1>192.168.49.2"
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 2, without(mesh, a, b), a, b)
	if c := cutSet(j); !c[a] || !c[b] || len(c) != 2 {
		t.Fatalf("IP blackhole cut = %v", c)
	}
}

// Off: the silent reading never reaches the judge (the producers are gated);
// the old judge sees the path absent and never cuts it.
func TestSilentOffAbsent(t *testing.T) {
	withSilent(t, false)
	const bh = "192.168.52.1>192.168.50.2"
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 4, without(mesh, bh))
	if c := cutSet(j); len(c) != 0 {
		t.Fatalf("absent path cut: %v", c)
	}
}

// Idle tunnel: every path silent or absent, none normal -> nothing counts.
func TestSilentIdleTunnel(t *testing.T) {
	withSilent(t, true)
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	var all []string
	for k := range mesh {
		all = append(all, k)
	}
	silFeed(j, &now, 10, nil) // quiet: no readings at all
	silFeed(j, &now, 4, nil, all[:3]...)
	silFeed(j, &now, 4, nil, all...)
	if c := cutSet(j); len(c) != 0 {
		t.Fatalf("idle tunnel cut: %v", c)
	}
}

// A path with no traffic assigned (absent from every reading) and a lossy
// but alive path (2.5 %, the lab's lossy pair): no silent reading is made for
// either (silentAcc), and the judge never cuts the unused one.
func TestSilentUnusedAndLossyAlive(t *testing.T) {
	withSilent(t, true)
	const unused, lossy = "192.168.53.1>192.168.49.2", "192.168.51.1>192.168.49.2"
	var u, l silentAcc
	u.add(0, 0, 0)
	l.add(64<<10, 25, 1)
	if u.silent() || l.silent() {
		t.Fatal("unused or lossy-alive path read silent")
	}
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(map[string]float64{lossy: 0.025})
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 360, without(mesh, unused)) // 30 min
	if c := cutSet(j); c[unused] {
		t.Fatalf("unused path cut: %v", c)
	}
	for _, q := range j.p {
		if q.silentRuns != 0 {
			t.Fatal("silent runs without a silent reading")
		}
	}
}

// The edge: partial readings (paths missing from a tick) and its own silence
// for a path no fresh report covers; a fresh report takes the dialing side's
// word (it does not cut the path).
func TestSilentEdgePartial(t *testing.T) {
	withAuto2(t)
	withSilent(t, true)
	run := func(report bool) bool {
		m, _ := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
		m.steer = newSteerState()
		now := time.Unix(1000, 0)
		for i := 0; i < 8; i++ {
			m.mu.Lock()
			st := m.steer
			st.upRt, st.upSegs = map[string]uint64{}, map[string]uint64{}
			st.dnRt, st.dnSegs = map[string]uint64{}, map[string]uint64{}
			// partial: c reads only every other tick
			st.upSegs["a>x"], st.upRt["a>x"] = 1000, 2
			if i%2 == 0 {
				st.upSegs["c>x"], st.upRt["c>x"] = 1000, 3
			}
			if i >= 3 {
				st.sil = map[string]*silentAcc{"b>x": {links: 2, stalled: true}}
			} else {
				st.upSegs["b>x"], st.upRt["b>x"] = 1000, 2
			}
			if report {
				r := LBReport{Auto: true, Auto2: true, At: now}
				r.Paths = []LBPeerPath{
					{Local: "x", Remote: "a", EffPct: 34, Factor: 1, RawPct: 0.2, raw: true},
					{Local: "x", Remote: "b", EffPct: 33, Factor: 1, RawPct: 0.2, raw: true},
					{Local: "x", Remote: "c", EffPct: 33, Factor: 1, RawPct: 0.3, raw: true},
				}
				st.rep = &r
			}
			m.steerTickLocked(now, lb2QualEvery)
			st.takeLogs()
			m.mu.Unlock()
			now = now.Add(lb2QualEvery)
		}
		return m.steer.judge.p["b>x"].cut
	}
	if !run(false) {
		t.Fatal("edge without a report: silent path not cut")
	}
	if run(true) {
		t.Fatal("edge with a fresh report: cut a path the dialing side keeps")
	}
}

// silFeedNoAck feeds n readings like silFeed, the paths in na read normally
// (raw loss 0 %, segments out) but with not one byte acked (noAck).
func silFeedNoAck(j *q2Judge, now *time.Time, n int, loss map[string]float64, na ...string) {
	for i := 0; i < n; i++ {
		*now = now.Add(5 * time.Second)
		rs := map[string]q2Reading{}
		for k, l := range loss {
			rs[k] = q2Reading{loss: l, segs: 1000}
		}
		for _, k := range na {
			rs[k] = q2Reading{loss: 0, segs: 400, noAck: true}
		}
		j.update(*now, 5*time.Second, rs)
	}
}

// bh98: a blackholed path drained by its silent cut reads 0 % raw loss with
// nothing acked (its segments out are ACKs for the other side's resends): it
// must stay cut for the whole blackhole (120 s), then come back within a
// couple of readings once bytes are acked again.
func TestSilentDrainedBlackholeStaysCut(t *testing.T) {
	withSilent(t, true)
	withFastRecover(t, true)
	const bh = "192.168.52.1>192.168.50.2"
	var logs []string
	j := newQ2Judge(0.1, "Iran", "kharej", func(f string, a ...any) { logs = append(logs, f) })
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 2, without(mesh, bh), bh)
	if !j.p[bh].cut {
		t.Fatal("not cut after two silent readings")
	}
	for i := 0; i < 24; i++ { // 120 s of drained-blackhole readings
		silFeedNoAck(j, &now, 1, without(mesh, bh), bh)
		if !j.p[bh].cut {
			t.Fatalf("still-blackholed path re-admitted after %d noAck readings", i+1)
		}
	}
	if j.p[bh].silentRuns < 24 {
		t.Fatalf("noAck readings not counted silent: silentRuns %d", j.p[bh].silentRuns)
	}
	held := 0
	for _, l := range logs {
		if strings.Contains(l, "still silent") {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("hold logged %d times, want once", held)
	}
	// The blackhole clears: bytes acked again -> recovered quickly.
	cut0 := now
	for i := 0; i < 12 && j.p[bh].cut; i++ {
		silFeed(j, &now, 1, mesh)
	}
	if j.p[bh].cut {
		t.Fatalf("cleared path not recovered (ewma %.3f)", j.p[bh].ewma)
	}
	t.Logf("recovered %s after the clear", now.Sub(cut0))
	if j.p[bh].silentRuns != 0 || j.p[bh].silentHeld {
		t.Fatal("silence not ended by acked bytes")
	}
}

// The fix must not delay a genuine recovery more than the plain silent cut
// did: same number of readings with and without noAck readings before the clear.
func TestSilentGenuineRecoverySameSpeed(t *testing.T) {
	withSilent(t, true)
	withFastRecover(t, true)
	const bh = "192.168.52.1>192.168.50.2"
	rec := func(drained int) int {
		j := newQ2Judge(0.1, "Iran", "kharej", nil)
		now := time.Unix(1000, 0)
		mesh := labMesh(nil)
		silFeed(j, &now, 6, mesh)
		silFeed(j, &now, 2, without(mesh, bh), bh)
		silFeedNoAck(j, &now, drained, without(mesh, bh), bh)
		n := 0
		for ; n < 40 && j.p[bh].cut; n++ {
			silFeed(j, &now, 1, mesh)
		}
		return n
	}
	a, b := rec(0), rec(24)
	if b > a+1 {
		t.Fatalf("recovery after a drained blackhole took %d readings vs %d", b, a)
	}
}

// noAck readings of a path that never read silent are judged as before (an
// unused path, the receive-only side of a flow): no cut.
func TestSilentNoAckWithoutSilenceIgnored(t *testing.T) {
	withSilent(t, true)
	const p = "192.168.53.1>192.168.49.2"
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	silFeedNoAck(j, &now, 60, without(mesh, p), p)
	if c := cutSet(j); len(c) != 0 || j.p[p].silentRuns != 0 {
		t.Fatalf("noAck alone cut / counted: %v runs %d", c, j.p[p].silentRuns)
	}
}

// Idle tunnel after a silent cut: noAck readings with no normal reading beside
// them are dropped like silent ones (nothing changes; past lb2Quiet the usual
// quiet probe re-admits the path to measure it), and a lossy-alive path
// (bytes acked) is never held.
func TestSilentHoldIdleAndLossy(t *testing.T) {
	withSilent(t, true)
	const bh, lossy = "192.168.52.1>192.168.50.2", "192.168.51.1>192.168.49.2"
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(map[string]float64{lossy: 0.025})
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 2, without(mesh, bh), bh)
	if !j.p[bh].cut {
		t.Fatalf("not cut before idle: %v", cutSet(j))
	}
	r0 := j.p[bh].silentRuns
	// idle: only the held path reads (dropped like a silent one); under
	// lb2Quiet, so the quiet probe does not re-admit it either
	silFeedNoAck(j, &now, 4, nil, bh)
	if j.p[bh].silentRuns != r0 || !j.p[bh].cut {
		t.Fatalf("idle tunnel changed the held path: runs %d->%d cut %v", r0, j.p[bh].silentRuns, j.p[bh].cut)
	}
	silFeed(j, &now, 60, without(mesh, bh))
	if j.p[lossy].cut || j.p[lossy].silentRuns != 0 {
		t.Fatal("lossy-alive path cut or held")
	}
	var a silentAcc
	a.add(64<<10, 25, 1)
	if a.noAck() {
		t.Fatal("acked path read noAck")
	}
	var b silentAcc
	if b.noAck() {
		t.Fatal("no links read noAck")
	}
	b.add(0, 0, 0)
	if !b.noAck() {
		t.Fatal("measured link with nothing acked not noAck")
	}
}

// The measure side: a carried reading keeps noAck only when no tick of its
// window got bytes acked; a full reading carries this tick's own counts.
func TestSilentCarryNoAck(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &LB{paths: []LBPath{{Local: "a", Remote: "x"}}, now: func() time.Time { return now }}
	none := silentAcc{links: 2}
	acked := silentAcc{links: 2, acked: 1500}
	if _, _, na, ok := b.carryLocked(0, lb2MinSegs, 0, none); !ok || !na {
		t.Fatal("full noAck reading lost its noAck")
	}
	if _, _, na, ok := b.carryLocked(0, lb2MinSegs, 0, acked); !ok || na {
		t.Fatal("full acked reading read noAck")
	}
	if _, _, _, ok := b.carryLocked(0, lb2MinSegs/2, 0, acked); ok {
		t.Fatal("half a reading emitted")
	}
	if _, _, na, ok := b.carryLocked(0, lb2MinSegs/2, 0, none); !ok || na {
		t.Fatal("carried window with acked bytes read noAck")
	}
	b.carryLocked(0, lb2MinSegs/2, 0, none)
	if _, _, na, ok := b.carryLocked(0, lb2MinSegs/2, 0, none); !ok || !na {
		t.Fatal("carried window with nothing acked lost its noAck")
	}
}

func withFastRecover(t *testing.T, on bool) {
	t.Helper()
	o1, o2 := lbFastRecover, lbRecConfirm
	lbFastRecover, lbRecConfirm = on, on
	t.Cleanup(func() { lbFastRecover, lbRecConfirm = o1, o2 })
}

// bh98: after a long blackhole clears, a silent-cut path's kept links are
// idle (a few keepalive bytes, too few segments for a reading); bytes acked on
// them (ackOnly) end the silence and the path recovers within a few readings.
// ackOnly readings of a path that never read silent are dropped.
func TestSilentAckOnlyRecovers(t *testing.T) {
	withSilent(t, true)
	withFastRecover(t, true)
	const bh, other = "192.168.52.1>192.168.50.2", "192.168.53.1>192.168.49.2"
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	mesh := labMesh(nil)
	silFeed(j, &now, 6, mesh)
	silFeed(j, &now, 2, without(mesh, bh), bh)
	silFeedNoAck(j, &now, 24, without(mesh, bh), bh)
	if !j.p[bh].cut {
		t.Fatal("not held")
	}
	n := 0
	for ; n < 12 && j.p[bh].cut; n++ {
		now = now.Add(5 * time.Second)
		rs := map[string]q2Reading{}
		for k, l := range without(mesh, bh) {
			rs[k] = q2Reading{loss: l, segs: 1000}
		}
		rs[bh] = q2Reading{segs: 6, ackOnly: true}
		j.update(now, 5*time.Second, rs)
	}
	if j.p[bh].cut {
		t.Fatal("ackOnly readings did not recover the cleared path")
	}
	t.Logf("recovered after %d ackOnly readings", n)
	// another path: ackOnly dropped, nothing changes
	j2 := newQ2Judge(0.1, "Iran", "kharej", nil)
	now = time.Unix(1000, 0)
	silFeed(j2, &now, 6, mesh)
	r := j2.silentAckOnly(map[string]q2Reading{other: {ackOnly: true}, bh: {loss: 0.01, segs: 900}})
	if _, ok := r[other]; ok || len(r) != 1 {
		t.Fatalf("ackOnly of a non-silent path kept: %v", r)
	}
}

// labMesh: the cg86 mesh, 5 Iran IPs x 2 kharej IPs = 10 paths (copied from
// the cg86/fc88 lb_burst_test.go, which rel60 does not carry).
func labMesh(over map[string]float64) map[string]float64 {
	m := map[string]float64{}
	base := []float64{0.002, 0.005, 0.01, 0.003, 0.008}
	for i := 1; i <= 5; i++ {
		for _, k := range []string{"192.168.50.2", "192.168.49.2"} {
			m[fmt.Sprintf("192.168.5%d.1>%s", i, k)] = base[i-1]
		}
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

// cutSet: the paths the judge has cut (copied from the cg86 lb_multiconfirm_test.go).
func cutSet(j *q2Judge) map[string]bool {
	c := map[string]bool{}
	for k, q := range j.p {
		if q.cut {
			c[k] = true
		}
	}
	return c
}
