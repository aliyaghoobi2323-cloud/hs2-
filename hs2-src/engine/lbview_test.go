package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func lossOf(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

// The dialing side (the reverse exit): rows are the balancer's paths, links
// come from its held links (meters + TCP_INFO). The exit sends the download.
func TestLBViewDialingExit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	lb, err := NewLB([]LBPath{
		{Local: "10.0.0.9", Remote: "10.0.0.1", Addr: "10.0.0.1:443", Weight: 50, Primary: true, Source: "advert"},
		{Local: "10.0.0.9", Remote: "10.0.0.2", Addr: "10.0.0.2:443", Weight: 50, Source: "advert"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lb.now = clk.now
	type lk struct {
		mtr *linkMeter
		ts  tcpStat
	}
	var links []*lk
	for i := 0; i < 4; i++ { // largest remainder: 2 + 2
		l := &lk{mtr: &linkMeter{}}
		v, err := lb.dialVia(func(p LBPath) (any, error) {
			l.mtr.path = p.Local + ">" + p.Remote
			return l, nil
		})
		if err != nil || v != l {
			t.Fatal(err)
		}
		lb.attach(l, lbHooks{stats: func() (tcpStat, bool) { return l.ts, true }, mtr: l.mtr})
		links = append(links, l)
	}
	view := newPathView(true)
	view.now = clk.now
	// first write: counters as the links stand (since they started)
	for _, l := range links {
		l.mtr.wrBytes.Store(1_000_000)
		l.mtr.rdBytes.Store(100_000)
		l.ts = tcpStat{segsOut: 1000, retrans: 0, rttUs: 40_000, minRttUs: 30_000}
	}
	links[0].mtr.users.Store(3)
	view.build(lb.viewDefs(), lb.viewLinks())

	// 10 s later: every link sent 12.5 MB more (10 Mbit/s); links on path 2
	// resent 100 of 1000 new segments (10%), path 1's none.
	clk.t = clk.t.Add(10 * time.Second)
	for _, l := range links {
		l.mtr.wrBytes.Add(12_500_000)
		l.mtr.rdBytes.Add(1_250_000)
		l.ts.segsOut += 1000
		if strings.HasSuffix(l.mtr.path, "10.0.0.2") {
			l.ts.retrans += 100
			l.ts.rttUs = 90_000
		}
	}
	lb.mu.Lock()
	lb.until[1] = clk.t.Add(42 * time.Second) // cooled (its links stay until they end)
	lb.mu.Unlock()
	rows, tot := view.build(lb.viewDefs(), lb.viewLinks())
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	p1, p2 := rows[0], rows[1]
	if p1.Local != "10.0.0.9" || p1.Remote != "10.0.0.1" || !p1.Primary || p1.Source != "advert" || p1.SharePct != 50 {
		t.Errorf("row 1 identity: %+v", p1)
	}
	if p1.Links != 2 || p2.Links != 2 || p1.Users != 3 || p2.Users != 0 {
		t.Errorf("links/users: %d/%d users %d/%d", p1.Links, p2.Links, p1.Users, p2.Users)
	}
	// the exit's sending direction is the users' download: 2 links × 10 Mbit/s
	if p1.DownMbit != 20 || p1.UpMbit != 2 {
		t.Errorf("row 1 rates: down %.2f up %.2f, want 20 / 2", p1.DownMbit, p1.UpMbit)
	}
	if p1.DownMB != 27 || p1.UpMB != 2.7 { // 2 × (1 + 12.5) MB since start
		t.Errorf("row 1 totals: down %.1f MB up %.1f MB, want 27 / 2.7", p1.DownMB, p1.UpMB)
	}
	if lossOf(p1.LossDown) != 0 || lossOf(p2.LossDown) != 10 || p1.LossUp != nil {
		t.Errorf("loss: row1 down %v up %v, row2 down %v", lossOf(p1.LossDown), lossOf(p1.LossUp), lossOf(p2.LossDown))
	}
	if p1.RTTp50 != 40 || p2.RTTp50 != 90 || p2.MinRTT != 30 {
		t.Errorf("rtt: %v %v min %v", p1.RTTp50, p2.RTTp50, p2.MinRTT)
	}
	if p2.State != "cooling" || p2.CooledS != 42 || p2.Target != 0 || p1.Target != 4 {
		t.Errorf("cooled path: state %q %ds target %d (row 1 target %d)", p2.State, p2.CooledS, p2.Target, p1.Target)
	}
	if tot.Links != 4 || tot.Users != 3 || tot.DownMbit != 40 || tot.UpMbit != 4 || lossOf(tot.LossDown) != 5 || tot.SharePct != 100 {
		t.Errorf("totals: %+v loss %v", *tot, lossOf(tot.LossDown))
	}
	// a link that ends keeps its bytes in its path's total
	lb.release(links[0])
	clk.t = clk.t.Add(2 * time.Second)
	rows, _ = view.build(lb.viewDefs(), lb.viewLinks())
	if rows[0].Links != 1 || rows[0].DownMB != 27 {
		t.Errorf("after a link ended: links %d, down %.1f MB (want 1, 27)", rows[0].Links, rows[0].DownMB)
	}
	// JSON: no key leaks, absent readings stay absent
	b, _ := json.Marshal(rows[0])
	if strings.Contains(string(b), "key") || strings.Contains(string(b), "loss_up_pct") {
		t.Errorf("json: %s", b)
	}
}

// The listening side (the reverse edge): a row per address it is reached on,
// shares from its announced list, the peer's sources as the remote, the
// edge's verdicts counted. The edge receives the download.
func TestLBViewListeningEdge(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	view := newPathView(false)
	view.now = clk.now
	mine := []LBAdvertIP{{IP: "81.0.0.1", Weight: 50}, {IP: "37.0.0.2", Weight: 50}}
	type L struct{ id int }
	mk := func(id int, local string, rx, tx uint64, segsIn, segsOut uint32, peerRt uint64) pathLink {
		return pathLink{key: L{id}, path: listenKey(local + ">116.0.0.5"), local: local, remote: "116.0.0.5",
			rx: rx, tx: tx, users: 1, serving: true, tsOK: true,
			ts:     tcpStat{segsIn: segsIn, segsOut: segsOut, rttUs: 50_000, minRttUs: 45_000},
			peerOK: true, peerRt: peerRt}
	}
	var ls []pathLink
	for i := 0; i < 6; i++ {
		ip := mine[i%2].IP
		ls = append(ls, mk(i, ip, 0, 0, 0, 0, 0))
	}
	view.build(listenDefs(mine, len(ls)), ls)
	clk.t = clk.t.Add(10 * time.Second)
	for i := range ls {
		ls[i].rx, ls[i].tx = 6_250_000, 625_000 // 5 Mbit/s down, 0.5 up per link
		ls[i].ts.segsIn, ls[i].ts.segsOut = 4000, 400
		if ls[i].local == "37.0.0.2" {
			ls[i].peerRt = 400 // the exit resent 10% of what this address received
		}
	}
	ls[1].degraded, ls[1].serving = true, false
	ls[3].draining, ls[3].serving = true, false
	rows, tot := view.build(listenDefs(mine, len(ls)), ls)
	if len(rows) != 2 || rows[0].Local != "81.0.0.1" || rows[1].Local != "37.0.0.2" {
		t.Fatalf("rows: %+v", rows)
	}
	a, b := rows[0], rows[1]
	if a.Remote != "116.0.0.5" || !a.Primary || b.Primary || a.SharePct != 50 || a.Target != 3 || b.Target != 3 {
		t.Errorf("identity/share: %+v / %+v", a, b)
	}
	if a.Links != 3 || a.Serving != 3 || b.Links != 3 || b.Serving != 1 || b.Degraded != 1 || b.Draining != 1 || a.Users != 3 {
		t.Errorf("counts: a %+v b %+v", a, b)
	}
	if a.DownMbit != 15 || a.UpMbit != 1.5 { // 3 links × 5 / 0.5
		t.Errorf("rates: down %.2f up %.2f", a.DownMbit, a.UpMbit)
	}
	if lossOf(a.LossDown) != 0 || lossOf(b.LossDown) != 10 || lossOf(a.LossUp) != 0 {
		t.Errorf("loss: a down %v up %v, b down %v", lossOf(a.LossDown), lossOf(a.LossUp), lossOf(b.LossDown))
	}
	if a.RTTp50 != 50 || a.MinRTT != 45 {
		t.Errorf("rtt: %v / %v", a.RTTp50, a.MinRTT)
	}
	if tot.Links != 6 || tot.Serving != 4 || tot.DownMbit != 30 || tot.Degraded != 1 || tot.Target != 6 || lossOf(tot.LossDown) != 5 {
		t.Errorf("totals: %+v", *tot)
	}
	// a source address not in the list gets its own row
	ls = append(ls, mk(9, "5.5.5.5", 0, 0, 0, 0, 0))
	rows, _ = view.build(listenDefs(mine, len(ls)), ls)
	if len(rows) != 3 || rows[2].Local != "5.5.5.5" || rows[2].Links != 1 || rows[2].SharePct != 0 {
		t.Errorf("unlisted address: %+v", rows)
	}
}

func TestLBViewListenTargets(t *testing.T) {
	cases := []struct {
		w     []int
		total int
		want  []int
	}{
		{[]int{50, 50}, 92, []int{46, 46}},
		{[]int{70, 30}, 10, []int{7, 3}},
		{[]int{34, 33, 33}, 10, []int{4, 3, 3}},
		{[]int{50, 50}, 0, []int{0, 0}},
	}
	for _, c := range cases {
		var mine []LBAdvertIP
		for i, w := range c.w {
			mine = append(mine, LBAdvertIP{IP: string(rune('a' + i)), Weight: w})
		}
		d := listenDefs(mine, c.total)
		for i := range d {
			if d[i].Target != c.want[i] {
				t.Errorf("weights %v total %d: targets %v, want %v", c.w, c.total, d, c.want)
				break
			}
		}
	}
	if d := listenDefs(nil, 5); len(d) != 0 {
		t.Errorf("no list: %v", d)
	}
}

func TestLBViewPercentile(t *testing.T) {
	xs := []float64{}
	for i := 1; i <= 100; i++ {
		xs = append(xs, float64(i))
	}
	if p := pctl(append([]float64(nil), xs...), 50); p != 50 {
		t.Errorf("p50 = %v", p)
	}
	if p := pctl(append([]float64(nil), xs...), 99); p != 99 {
		t.Errorf("p99 = %v", p)
	}
	if p := pctl([]float64{7}, 99); p != 7 {
		t.Errorf("one: %v", p)
	}
}
