package engine

import (
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// Light-user evacuation off cut paths (HS2_LB_EVAC=1, with HS2_LB_CUT_DRAIN).
//
// The cut drain (lb_cutdrain.go) stops new connections on a cut path's
// surplus links, but the users already there stay until they idle for
// drainIdle (310 s) or, trickling, until cutDrainForce (60 s), a few links a
// tick. A light user that keeps doing small requests (a browser tab, an app's
// API polling: the lab crowd, a request every ~0.7 s) never idles that long,
// so it sat behind the bad path for the whole cut: phase-4 lab, path B +80 ms
// and 3% corrupt, cut at ~12 s, 25 links cut-drained holding 1831
// connections, B still 28 links at the end, crowd p99 1.23 s.
//
// With the key on, once a link has been cut-drained for evacAfter, its light
// connections are closed BETWEEN requests, a bounded number a tick, so they
// reconnect at once onto a serving link of a good path:
//   - light: not heavy (cs.heavy) and a rate EWMA under evacLightRate — a
//     download, an upload, a call's media are never touched (a heavy user
//     stays where it is, as in the cut drain);
//   - between requests: its byte counter did not move for evacQuiet, checked
//     right before the close (a stream that moved meanwhile is spared, as in
//     reclaimIdle), so no request is cut in the middle by this rule;
//   - copy-only streams (an mconn second copy) are left alone;
//   - at most evacPerLink a link and evacPerTick over the whole pool a drain
//     tick (healthTick, 2 s), spaced a few ms apart: a cut path's hundreds of
//     users reconnect over tens of seconds, never as one burst (a reconnect
//     is a new stream on a live link, not a new link dial).
//
// Only cut paths (the dialing side's cut, two bad readings in a row, relative
// to the best path) are evacuated, not merely lowered ones: a lowered path is
// still a fair path for the users already on it. Nothing happens when every
// path is cut (cutPathsFromReport is nil then).
//
// Off by default. Lab (reality phase 4c, docs/reality-architecture.md 8.4):
// of the ~1770 connections on B's 24 cut-drained links, ~85% were mconn
// lite second copies (copy-only: the connection already reads whichever
// link delivers first) and ~200 real light first subs; the key moved those
// ~200 within ~25 s (crowd failures 0-4: a moved sub with a live copy just
// survives), but the crowd p99 after the cut was the same with and without
// it (~320 ms): the 1.2-1.5 s whole-run p99 is the ~12 s BEFORE the cut,
// when a third of the crowd is on B and the judge still needs its readings.
var lbEvac bool // loadEnvKeys: HS2_LB_EVAC

// evacSkipCopy / evacSkipBusy: streams lightStreams passed over (copy-only;
// heavy or over evacLightRate), for the evac log line.
var evacSkipCopy, evacSkipBusy, evacSpared atomic.Int64

var (
	evacAfter     = 4 * time.Second
	evacQuiet     = 400 * time.Millisecond
	evacLightRate = float32(32 << 10) // bytes/s
	evacPerLink   = 24
	evacCandX     = 4 // candidates looked at per close allowed (most are mid-request at a given moment)
	evacPerTick   = 240
)

// evacReclaimer is implemented by links that can name their light user
// streams (lb_evac.go).
type evacReclaimer interface {
	lightStreams(now time.Time, max int) []idleCand
}

// lightStreams returns up to max light user streams (not heavy, rate EWMA
// under evacLightRate, not copy-only) with their byte counters.
func (l *mtcpLink) lightStreams(now time.Time, max int) []idleCand {
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	var out []idleCand
	for cs := range l.flows {
		if len(out) >= max {
			break
		}
		switch {
		case cs.copyOnly.Load():
			evacSkipCopy.Add(1)
			continue
		case cs.heavy(now) || cs.ewma >= evacLightRate:
			evacSkipBusy.Add(1)
			continue
		}
		out = append(out, idleCand{cs: cs, snap: cs.bytes.Load()})
	}
	return out
}

// evacDue reports whether a cut-drained link's light users are due to move.
func evacDue(ml *managedLink, now time.Time) bool {
	return lbEvac && ml.cutDrain && ml.retiring && now.Sub(ml.retireSince) >= evacAfter
}

// evacLight closes up to n light connections on ml that stay quiet for
// evacQuiet. It runs in its own goroutine and owns ml.reclaiming (taken by
// drainTick). It returns the connections closed.
func (m *LinkManager) evacLight(ml *managedLink, now time.Time, n int, sleep func(time.Duration)) int {
	defer ml.reclaiming.Store(false)
	er, ok := ml.link.(evacReclaimer)
	if !ok || n <= 0 {
		return 0
	}
	cands := er.lightStreams(now, n*evacCandX)
	if len(cands) == 0 {
		return 0
	}
	sleep(evacQuiet)
	closed := 0
	for _, c := range cands {
		if closed >= n {
			break
		}
		if c.cs.bytes.Load() != c.snap {
			evacSpared.Add(1)
			continue // mid-request: spared
		}
		if closed > 0 {
			sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
			if c.cs.bytes.Load() != c.snap {
				continue
			}
		}
		c.cs.Close()
		closed++
	}
	m.evacuated.Add(int64(closed))
	m.evacTotal.Add(int64(closed))
	return closed
}
