package main

import (
	"math"
	"math/rand/v2"
	"net"
	"sort"
	"testing"
)

func TestPlayerStartupAndStalls(t *testing.T) {
	p := newPlayer(1000, 0) // 1000 B = 1 media s
	p.recv(0.5, 1000)
	if p.state != 0 || p.startup != -1 {
		t.Fatalf("started with 1 s buffered: state %d startup %v", p.state, p.startup)
	}
	p.recv(1.0, 1000) // 2 s in: starts
	if p.state != 1 || p.startup != 1.0 {
		t.Fatalf("state %d startup %v, want playing at 1.0", p.state, p.startup)
	}
	if b := p.buffer(2.5); math.Abs(b-0.5) > 1e-9 {
		t.Fatalf("buffer at 2.5 = %v, want 0.5", b)
	}
	p.at(3.5) // empty at 3.0
	if p.stalls != 1 || p.stallAt != 3.0 || p.state != 2 {
		t.Fatalf("stalls %d at %v state %d, want 1 at 3.0", p.stalls, p.stallAt, p.state)
	}
	p.recv(3.8, 1500) // 1.5 s: still stalled
	if p.state != 2 {
		t.Fatal("resumed with 1.5 s")
	}
	p.recv(4.0, 1000) // 2.5 s: resumes; stalled 1.0 s
	if p.state != 1 || math.Abs(p.stallS-1.0) > 1e-9 {
		t.Fatalf("state %d stallS %v, want playing after 1.0 s", p.state, p.stallS)
	}
	p.end(10) // empty again at 6.5, stalled to 10
	if p.stalls != 2 || math.Abs(p.stallS-4.5) > 1e-9 {
		t.Fatalf("stalls %d stallS %v, want 2 and 4.5", p.stalls, p.stallS)
	}
}

func TestPlayerNeverStarts(t *testing.T) {
	p := newPlayer(1000, 5)
	p.recv(6, 500)
	p.end(20)
	if p.startup != -1 || p.stalls != 0 || p.stallS != 0 {
		t.Fatalf("startup %v stalls %d stallS %v", p.startup, p.stalls, p.stallS)
	}
}

func TestCallMeanRate(t *testing.T) {
	g := callSpec(0, 1.5)
	r := rand.New(rand.NewPCG(1, 2))
	var sum float64
	n := 100 * 200 // 400 s of frames
	for k := range n {
		s := g.size(k, r)
		if k%100 == 0 && s < 3*3600 {
			t.Fatalf("keyframe %d B", s)
		}
		sum += float64(s)
	}
	mbit := sum * 8 / (float64(n) / g.Hz) / 1e6
	if math.Abs(mbit-1.5) > 0.02 {
		t.Fatalf("mean %.3f Mbit/s, want 1.5", mbit)
	}
}

func TestGameSizes(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	up, dn := gameUp(0), gameDn(0)
	for k := range 5000 {
		if s := up.size(k, r); s < 60 || s > 120 {
			t.Fatalf("up size %d", s)
		}
		if s := dn.size(k, r); s < 100 || s > 600 {
			t.Fatalf("dn size %d", s)
		}
	}
}

func TestWebObjects(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	v := webObjects(r, 20000)
	sort.Ints(v)
	if v[0] < objMin || v[len(v)-1] > objMax {
		t.Fatalf("range %d..%d", v[0], v[len(v)-1])
	}
	med := float64(v[len(v)/2])
	if want := math.Sqrt(objMin * objMax); math.Abs(med-want)/want > 0.05 {
		t.Fatalf("median %.0f, want ~%.0f (log-uniform)", med, want)
	}
}

func TestOwdOf(t *testing.T) {
	f := []sample{
		{s: 1.0, ms: 40, size: 100},
		{s: 1.1, ms: 50, size: 100},
		{s: 1.2, ms: -1, size: 100}, // lost
		{s: 1.3, ms: 45, size: 100},
		{s: 1.4, ms: 160, size: 100}, // late: > 36 + 100
		{s: 9.0, ms: 36, size: 100},  // outside the window
	}
	fl := floorOf([][]sample{f}, 0, 10)
	if fl != 36 {
		t.Fatalf("floor %v", fl)
	}
	o := owdOf([][]sample{f}, 0, 5, fl, 100, true)
	if o.Sent != 5 || o.Lost != 1 || o.N != 4 || o.Late != 1 {
		t.Fatalf("%+v", o)
	}
	if o.Min != 40 || o.Max != 160 || o.P50 != 45 {
		t.Fatalf("min %v p50 %v max %v", o.Min, o.P50, o.Max)
	}
	// jitter over consecutive arrivals: |50-40|, |45-50|, |160-45|
	if o.Jit99 != 115 {
		t.Fatalf("jitter p99 %v", o.Jit99)
	}
	if o.OfferedMbit != round3(500*8/5.0/1e6) || o.DeliveredMbit != round3(400*8/5.0/1e6) {
		t.Fatalf("offered %v delivered %v", o.OfferedMbit, o.DeliveredMbit)
	}
	if owdOf([][]sample{f}, 20, 30, fl, 100, false) != nil {
		t.Fatal("empty window gave a result")
	}
}

func TestWebOf(t *testing.T) {
	var ps []pageRec
	for i := range 19 {
		ps = append(ps, pageRec{S: float64(i), Fb: 80, Load: float64(1000 + i)})
	}
	ps = append(ps, pageRec{S: 19, Fb: -1, Load: -1})
	w := webOf(ps, 0, 100)
	if w.Pages != 20 || w.Failed != 1 || w.LoadP95 != 1018 || w.LoadMax != -1 || w.FbP50 != 80 {
		t.Fatalf("%+v", w)
	}
	ps = append(ps, pageRec{S: 20, Fb: -1, Load: -1})
	if w := webOf(ps, 0, 100); w.LoadP95 != -1 {
		t.Fatalf("2 of 21 failed, p95 %v, want -1", w.LoadP95)
	}
}

// the classes end to end on loopback: the server and the client of this
// package, every kind for a few seconds
func TestClassesLoopback(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer pc.Close()
	go acceptLoop(ln, dir)
	go serveUDP(pc, dir)
	s := &Spec{TotalS: 4, Bulk: addr, Echo: addr,
		Users: []User{
			{ID: "g", Flows: []Flow{{Kind: "game", N: 1}}},
			{ID: "gu", Flows: []Flow{{Kind: "gameudp", N: 1}}},
			{ID: "v", Flows: []Flow{{Kind: "call", N: 1, Rate: 1.5}}},
			{ID: "s", Flows: []Flow{{Kind: "stream", N: 1, Rate: 2}}},
			{ID: "w", Flows: []Flow{{Kind: "web", N: 1, Every: 1}}},
		},
		Windows: []Window{{"main", 0.5, 3.5}, {"all", 0.5, 4}}}
	if err := s.check(); err != nil {
		t.Fatal(err)
	}
	out := runClient(s, dir)
	if len(out.Errs) > 0 {
		t.Fatalf("errors: %v", out.Errs)
	}
	if out.KillS != 0 || out.Conns != nil || out.Recov != nil || out.Outage != nil || out.Frozen != nil {
		t.Fatal("a kill cell's or frozen output without either (cuts.go, frozen.go)")
	}
	for _, k := range []string{"game", "gameudp", "call"} {
		rc, ok := out.Classes[k].(*RTClass)
		if !ok {
			t.Fatalf("no class %s", k)
		}
		if rc.Errs != 0 {
			t.Fatalf("%s: %d flows without a record", k, rc.Errs)
		}
		for _, d := range []struct {
			name string
			o    *Owd
			hz   float64
		}{{"up", rc.Up["main"], 64}, {"dn", rc.Dn["main"], 30}} {
			o := d.o
			if k == "call" {
				d.hz = 50
			}
			if o == nil || o.Lost != 0 || math.Abs(float64(o.Sent)-3*d.hz) > 3 || o.P99 < 0 || o.P99 > 50 {
				t.Fatalf("%s %s: %+v", k, d.name, o)
			}
			if rc.Floor[d.name] < 0 || rc.Floor[d.name] > 5 {
				t.Fatalf("%s %s floor %v ms on loopback", k, d.name, rc.Floor[d.name])
			}
			if k == "call" && math.Abs(o.DeliveredMbit-o.OfferedMbit) > 1e-9 {
				t.Fatalf("call %s delivered %v of %v", d.name, o.DeliveredMbit, o.OfferedMbit)
			}
		}
	}
	// the users' own numbers, and the upload bins filled from the records
	for _, u := range out.Users[:3] {
		if u.RT["up"] == nil || u.RT["dn"] == nil || u.Up["main"] <= 0 || u.Dn["main"] <= 0 {
			t.Fatalf("user %s: rt %v up %v dn %v", u.ID, u.RT, u.Up, u.Dn)
		}
	}
	if call := out.Users[2]; call.Up["main"] < 1.0 || call.Dn["main"] < 1.0 {
		t.Fatalf("call user rates up %v dn %v", call.Up["main"], call.Dn["main"])
	}
	st := out.Users[3].Stream
	if st == nil || st.StartupS < 0 || st.StartupS > 1 || st.Rebuffers != 0 || st.Segs < 2 {
		t.Fatalf("stream %+v", st)
	}
	sc := out.Classes["stream"].(*StreamClass)
	if sc.Users != 1 || sc.NotStarted != 0 {
		t.Fatalf("stream class %+v", sc)
	}
	w := out.Users[4].Web
	if w == nil || w.Pages < 2 || w.Failed != 0 || w.LoadP50 <= 0 || w.FbP50 <= 0 {
		t.Fatalf("web %+v", w)
	}
	if out.Classes["web"].(map[string]*WebStats)["main"] == nil {
		t.Fatal("no web class")
	}
}
