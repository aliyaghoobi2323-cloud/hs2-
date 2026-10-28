package engine

import (
	"sync"
	"time"
)

// R1 round 4, video over several links (decision T21 item 1): a user's
// share of a link weighed by the links it is active on.
//
// C2 (sched.go) gives each user active on a link a floor of C/N, C the
// link's rate and N the users with bulk demand there. Each link counts on
// its own. With β2's placement a heavy user's 16 connections spread over
// every BULK link, so on each link it is one of N, and it takes N shares
// over N links, where a viewer whose one connection sits on one link takes
// one. Measured (round-3 judge's runs, round4/REPORT.fa.md): the home mix
// at N=8 has 14 to 17 users active on each BULK link of 12 to 15 Mbit/s, a
// floor C/N of 0.73 to 1.04 Mbit/s, and the viewers fetched their segments
// at 0.64 to 0.77, under the 1.2 Mbit/s bitrate (51 to 64 rebuffers), while
// each heavy user took 4.3 to 4.9 Mbit/s; in stream_vs_dl (25 heavy users of
// 16 connections) C/N was 0.57 to 0.68 and the segments 0.63 to 0.66, with
// the heavy users at 3.0. On one link (N=1) both mixes had no rebuffer.
//
// XShare 1 (HS2_TUNE_XSHARE=1): each active user's weight on a link is 1/k,
// k the links it is active on now (a process-wide count, xshareSet), and its
// floor C·w/Σw: a user active on every one of L links takes about one share
// in all, as a user on one link does. With every user on one link this is
// C/N as before. Nothing goes on the wire: each end weighs what it sends.

// XShare: 1 on (HS2_TUNE_XSHARE), 0 off.
var XShare = 0

// xshareSpan: a user counts as active on a link it was active on at a tick
// of that link within the latest two spans (each link ticks each
// schedTick, so twice a span).
const xshareSpan = 2 * schedTick

// xshareIdle: a user not active on any link this long is forgotten.
const xshareIdle = 30 * time.Second

// xshareEnt is one user's links.
type xshareEnt struct {
	mu        sync.Mutex
	gen       int64         // the current span (now / xshareSpan)
	cur, prev int           // the links counted in this span and the one before
	seen      map[int]int64 // link → the span it was last counted in
	used      int64         // now of the latest note (ns)
	dead      bool          // swept: a scheduler holding it takes a new one
}

// note counts link as active for the user at now (ns of a clock all links
// share) and says on how many links the user is active: the links of this
// span or the one before, whichever more, at least 1; 0 when the entry was
// swept (the caller takes the tag's entry again).
func (e *xshareEnt) note(link int, now int64) int {
	g := now / int64(xshareSpan)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return 0
	}
	if g > e.gen {
		if g == e.gen+1 {
			e.prev = e.cur
		} else {
			e.prev = 0
		}
		e.cur, e.gen = 0, g
		for l, at := range e.seen {
			if at < g-1 {
				delete(e.seen, l) // links that ended or went idle
			}
		}
	}
	if e.seen == nil {
		e.seen = map[int]int64{}
	}
	if last, ok := e.seen[link]; !ok || last != g {
		e.seen[link] = g
		e.cur++
	}
	e.used = now
	return max(e.cur, e.prev, 1)
}

// xshareSet is the process's users, by tag.
type xshareSet struct {
	mu      sync.Mutex
	m       map[uint32]*xshareEnt
	sweepAt int64
}

var xshares xshareSet

// ent is the user tag's entry (made on first use); now (ns) sweeps the idle
// ones now and then.
func (s *xshareSet) ent(tag uint32, now int64) *xshareEnt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[uint32]*xshareEnt{}
	}
	if now-s.sweepAt > int64(xshareIdle) {
		s.sweepAt = now
		for t, e := range s.m {
			e.mu.Lock()
			idle := now-e.used > int64(xshareIdle)
			if idle {
				e.dead = true
			}
			e.mu.Unlock()
			if idle {
				delete(s.m, t)
			}
		}
	}
	e := s.m[tag]
	if e == nil {
		e = &xshareEnt{used: now}
		s.m[tag] = e
	}
	return e
}

// users is how many users the set holds (tests, status).
func (s *xshareSet) users() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
