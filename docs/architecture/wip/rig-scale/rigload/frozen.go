package main

import (
	"errors"
	"math"
	"net"
	"sort"
	"sync"
	"time"
)

// frozen.go: a user whose app stops reading (R1 round-4 judge, the red
// team's C5; MIX frozen in ana.py). Each "frozen" connection is a download
// that opens at StartS + k/Wave s (Wave a second), reads for ReadS s (1),
// then reads no more and holds the connection to the end: its socket's
// receive buffer (Rcvbuf, 16 KiB: a zero window soon, as an app or a client
// that announces one) fills, and the tunnel has to keep what the exit sent
// after it. What it costs the light users next to it is their probes'
// longest gap between two echoes (FrozenOut.ProbeGapMs).

const (
	frozenRead   = time.Second
	frozenRcvbuf = 16 << 10
)

// frozenStats is the client's frozen downloads, under its mu.
type frozenStats struct {
	mu       sync.Mutex
	present  bool
	opened   int
	dialErrs int
	frozen   int   // stopped reading (held to the end)
	ended    int   // ended before they froze (the peer, or an error)
	bytes    int64 // read before they froze
	readMbit []float64
}

// FrozenOut is the frozen downloads and the light users' probe gaps.
type FrozenOut struct {
	Conns    int     `json:"conns"`
	DialErrs int     `json:"dial_errs"`
	Frozen   int     `json:"frozen"`
	Ended    int     `json:"ended"`
	MBytes   float64 `json:"mbytes"`
	ReadMbit float64 `json:"read_mbit_mean"` // a connection's rate while it read
	// the light users' probes in the main window: each user's longest gap
	// between two echoes, ms — the most and the median over users — and
	// the probes' p99 and max there
	ProbeGapMs map[string]float64 `json:"probe_gap_ms"`
}

// frozen runs frozen connection k of us's flow f (fid its flow id).
func (c *client) frozen(us *ustate, fid, k int, f Flow) {
	c.frz.mu.Lock()
	c.frz.present = true
	c.frz.mu.Unlock()
	wave := max(f.Wave, 1)
	read := frozenRead
	if f.ReadS > 0 {
		read = time.Duration(f.ReadS * float64(time.Second))
	}
	rcv := f.Rcvbuf
	if rcv <= 0 {
		rcv = frozenRcvbuf
	}
	stop := c.stopOf(f)
	sleepUntil(c.at(f.StartS + float64(k/wave)))
	if time.Until(stop) <= 0 {
		return
	}
	addr := c.target(us, false)
	conn, err := c.dialWith(us, addr, rcvbufControl(rcv))
	if err != nil {
		us.dialErrs.Add(1)
		c.err("dial frozen %s: %v", addr, err)
		c.frz.mu.Lock()
		c.frz.dialErrs++
		c.frz.mu.Unlock()
		return
	}
	us.conns.Add(1)
	defer conn.Close()
	c.frz.mu.Lock()
	c.frz.opened++
	c.frz.mu.Unlock()
	conn.SetDeadline(stop)
	if _, err := conn.Write(header('D', us.idx, fid)); err != nil {
		c.err("frozen header: %v", err)
		return
	}
	t0 := time.Now()
	conn.SetReadDeadline(t0.Add(read))
	buf := make([]byte, 64<<10)
	var got int64
	ended := false
	for {
		n, err := conn.Read(buf)
		us.dn.add(c.off(), n)
		got += int64(n)
		if err != nil {
			var ne net.Error
			ended = !(errors.As(err, &ne) && ne.Timeout())
			break
		}
	}
	dt := time.Since(t0).Seconds()
	c.frz.mu.Lock()
	c.frz.bytes += got
	if dt > 0 {
		c.frz.readMbit = append(c.frz.readMbit, float64(got)*8/dt/1e6)
	}
	if ended {
		c.frz.ended++
	} else {
		c.frz.frozen++
	}
	c.frz.mu.Unlock()
	if !ended {
		sleepUntil(stop) // frozen: the connection stays, unread
	}
}

// frozenOut puts the frozen downloads and the probe gaps into out.
func (c *client) frozenOut(out *Out) {
	c.frz.mu.Lock()
	defer c.frz.mu.Unlock()
	if !c.frz.present {
		return
	}
	o := &FrozenOut{Conns: c.frz.opened, DialErrs: c.frz.dialErrs, Frozen: c.frz.frozen, Ended: c.frz.ended,
		MBytes: round3(float64(c.frz.bytes) / 1e6), ProbeGapMs: map[string]float64{}}
	if len(c.frz.readMbit) > 0 {
		var s float64
		for _, v := range c.frz.readMbit {
			s += v
		}
		o.ReadMbit = round3(s / float64(len(c.frz.readMbit)))
	}
	w := c.window("main")
	var gaps []float64
	var all []*rtts
	for _, us := range c.users {
		us.probe.mu.Lock()
		n := len(us.probe.sendS)
		var ts []float64
		for i, s := range us.probe.sampS {
			ts = append(ts, s+us.probe.ms[i]/1000)
		}
		us.probe.mu.Unlock()
		if n == 0 {
			continue
		}
		all = append(all, &us.probe)
		sort.Float64s(ts)
		g, _ := longestGap(ts, w.From, w.To)
		if len(ts) == 0 || ts[len(ts)-1] < w.To {
			// no echo since: the gap runs to the window's end
			last := w.From
			if len(ts) > 0 {
				last = math.Max(last, ts[len(ts)-1])
			}
			g = math.Max(g, w.To-last)
		}
		gaps = append(gaps, g*1000)
	}
	if len(gaps) > 0 {
		sort.Float64s(gaps)
		o.ProbeGapMs["max"] = round1(gaps[len(gaps)-1])
		o.ProbeGapMs["p50"] = round1(gaps[len(gaps)/2])
		o.ProbeGapMs["users"] = float64(len(gaps))
	}
	if l := latOf(all, w.From, w.To); l != nil {
		o.ProbeGapMs["probe_p99"] = l.P99
		o.ProbeGapMs["probe_max"] = l.Max
		o.ProbeGapMs["probe_lost"] = float64(l.Lost)
	}
	out.Frozen = o
}
