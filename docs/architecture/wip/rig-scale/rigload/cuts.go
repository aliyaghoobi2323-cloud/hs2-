package main

import (
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// cuts.go: a kill cell (run.sh KILLQ; R1 round-4 judge, the red team's C4):
// one hs2 link is cut KillS s after t0, and what the users lose is counted.
//
// Only in a kill cell (Spec.KillS > 0; elsewhere every flow runs as before):
//
//   - every TCP connection of a flow is logged: its user, kind, when it
//     opened and ended, and whether it was cut — ended (an error, or the
//     peer's end) more than 200 ms before its flow's stop;
//   - a cut connection is opened again rtRedial (200 ms) later, as an app
//     would: a game, a call or a probe on a new connection (a game or call
//     numbers its messages on; the server keeps each connection's record
//     apart: rtConnFlow), a viewer's player and a bulk flow as before (they
//     always did), a browser with its next page (the page that was cut
//     fails);
//   - each cut flow's recovery: from its cut to the first byte it carried
//     again (a browser: its next whole page; -1: none by the end);
//   - each game, call and probe flow's longest gap between two messages it
//     got, per direction, from KillS - 2 s on: the outage as its user felt it
//     (a gameudp flow, which has no connection to cut, too).
const rtRedial = 200 * time.Millisecond

// connRec is one connection of a kill cell.
type connRec struct {
	U int     `json:"u"` // the user (spec order, from 1)
	K string  `json:"k"` // the flow's kind
	O float64 `json:"o"` // opened, s from t0
	C float64 `json:"c"` // ended
	X float64 `json:"x"` // cut at (s from t0; -1: not cut)
}

// Recov is one cut flow's recovery.
type Recov struct {
	U    int     `json:"u"`
	K    string  `json:"k"`
	Cut  float64 `json:"cut"`  // s from t0
	Back float64 `json:"back"` // s from the cut until it carried data again; -1: never
}

// Outage is one flow's longest gap between two messages it got, one
// direction ("up" what the server got, "dn" what the client got).
type Outage struct {
	U     int     `json:"u"`
	K     string  `json:"k"`
	Dir   string  `json:"dir"`
	GapMs float64 `json:"gap_ms"`
	At    float64 `json:"at"` // the gap's start, s from t0
}

// track says whether this is a kill cell.
func (c *client) track() bool { return c.s.KillS > 0 }

// opened logs a connection of us's flow kind (nil: not a kill cell).
func (c *client) opened(us *ustate, kind string) *connRec {
	if !c.track() {
		return nil
	}
	return &connRec{U: us.idx, K: kind, O: round3(c.off().Seconds()), X: -1}
}

// closed ends r; cut: before its flow's stop.
func (c *client) closed(r *connRec, cut bool) {
	if r == nil {
		return
	}
	now := round3(c.off().Seconds())
	c.mu.Lock()
	r.C = now
	if cut && r.X < 0 {
		r.X = now
	}
	c.conns = append(c.conns, r)
	c.mu.Unlock()
}

// flowCut is one flow's cut waiting for its recovery.
type flowCut struct {
	pend atomic.Bool
	at   float64 // under the client's mu
}

// cut: the flow lost a connection now (the first cut until it recovers).
func (f *flowCut) cut(c *client) {
	if !c.track() {
		return
	}
	c.mu.Lock()
	if !f.pend.Load() {
		f.at = c.off().Seconds()
		f.pend.Store(true)
	}
	c.mu.Unlock()
}

// recovered: the flow carried data again (no-op unless it was cut).
func (c *client) recovered(us *ustate, kind string, f *flowCut) {
	if !f.pend.Load() {
		return
	}
	c.mu.Lock()
	if f.pend.Load() {
		f.pend.Store(false)
		c.recov = append(c.recov, Recov{U: us.idx, K: kind, Cut: round3(f.at), Back: round3(c.off().Seconds() - f.at)})
	}
	c.mu.Unlock()
}

// flowEnd: the flow ended; a cut it never recovered from counts as never.
func (c *client) flowEnd(us *ustate, kind string, f *flowCut) {
	if !f.pend.Load() {
		return
	}
	c.mu.Lock()
	if f.pend.Load() {
		f.pend.Store(false)
		c.recov = append(c.recov, Recov{U: us.idx, K: kind, Cut: round3(f.at), Back: -1})
	}
	c.mu.Unlock()
}

// longestGap is the longest interval between two of the sorted times ts
// (s) within [lo, hi], and where it starts; a gap from lo to the first time
// counts too when the first comes late.
func longestGap(ts []float64, lo, hi float64) (gap, at float64) {
	prev := lo
	for _, t := range ts {
		if t < lo {
			continue
		}
		if t > hi {
			break
		}
		if d := t - prev; d > gap {
			gap, at = d, prev
		}
		prev = t
	}
	return gap, at
}

// cutsOut puts a kill cell's log, recoveries and outages into out.
func (c *client) cutsOut(out *Out) {
	if !c.track() {
		return
	}
	out.KillS = c.s.KillS
	c.mu.Lock()
	out.Conns, out.Recov = c.conns, c.recov
	c.mu.Unlock()
	lo, hi := c.s.KillS-2, math.Min(c.s.KillS+25, c.s.TotalS+grace.Seconds())
	add := func(u int, kind, dir string, ts []float64) {
		if len(ts) == 0 {
			return
		}
		sort.Float64s(ts)
		g, at := longestGap(ts, lo, hi)
		out.Outage = append(out.Outage, Outage{U: u, K: kind, Dir: dir, GapMs: round1(g * 1000), At: round3(at)})
	}
	sec := func(ns int64) float64 { return float64(ns-c.t0m) / 1e9 }
	for _, fl := range c.rts {
		fl.mu.Lock()
		var dn []float64
		for _, at := range fl.rx {
			dn = append(dn, sec(at))
		}
		var up []float64
		if fl.srv != nil {
			for _, r := range fl.srv.Rx {
				up = append(up, sec(r[1]))
			}
		}
		fl.mu.Unlock()
		add(fl.us.idx, fl.kind, "dn", dn)
		add(fl.us.idx, fl.kind, "up", up)
	}
	for _, us := range c.users {
		us.probe.mu.Lock()
		var ts []float64
		for i, s := range us.probe.sampS {
			ts = append(ts, s+us.probe.ms[i]/1000)
		}
		us.probe.mu.Unlock()
		add(us.idx, "probe", "rtt", ts)
	}
}
