package engine

import (
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// b468 (rc14g, HS2_B468_DRAINFAST): pace the old exit's drain end by user
// connection, not by link.
//
// rc14f lab1 (12k users, ZDU exit, 4 ABBA pairs vs rc12): the b447/b454
// paced close (zduPaceClose over links, fewest users first) takes ~4.7 s in
// ~160 batches. A multi-link connection (HS2_MCONN: calls, games) counts on
// no link (its exitConn has no meter), so the links it rides look idle and
// close first: every such connection loses its subs one link at a time and
// crowds the old generation's last links. Game p99 in those last ~4 s rose
// to 150-245 ms (rc12 ~80), game frames lost +48, call loss +0.29 pp, call
// samples > 200 ms in the last 6 s +5.6 pp — all on connections that end at
// the exit in both versions anyway.
//
// With the key on, the same rate closes the user connections themselves
// (each one's target and stream / mconn, in one instant: a clean end, no
// lost sub), while every link stays up and fully usable for the users not
// yet closed; the links (by then carrying no user) close together right
// after, then the process exits as before. Same rate, same lead, same exit
// instant: the TUN-side ping win of the pace is kept.
//
//	HS2_B468_DRAINFAST=0           the b454 link pace (rc14)
//	HS2_B468_DRAINFAST unset / 1   pace by user connection (default on since
//	                               rc14h: 4 ABBA pairs vs rc12, nothing proven worse)
var zduExitTraffic atomic.Pointer[exitTraffic]

func b468DrainFastOn() bool {
	return strings.TrimSpace(os.Getenv("HS2_B468_DRAINFAST")) != "0"
}

// b468PaceUsers closes t's user connections at zduPaceRate (zduPaceClose
// with one user per entry), then every link of p at once. Returns the links
// closed, the users closed by the pace and the batches used.
func b468PaceUsers(t *exitTraffic, p *linkPeers, sleep func(time.Duration)) (links, users, batches int) {
	ls := b468UserKills(t)
	_, users, batches = zduPaceClose(ls, b470Rate(len(ls)), b469Step(), zduPaceMax, sleep)
	p.mu.Lock()
	cs := make([]func() error, 0, len(p.m))
	for m := range p.m {
		if c := m.tcp; c != nil {
			cs = append(cs, c.Close)
		}
	}
	p.mu.Unlock()
	for _, c := range cs {
		c()
	}
	if users > 0 || len(cs) > 0 {
		batches++
	}
	return len(cs), users, batches
}

// b468UserKills: one pace entry per live user connection that has a kill
// (subs and conns without one are left to the link close).
func b468UserKills(t *exitTraffic) []zduPaceLink {
	var ls []zduPaceLink
	for i := range t.sh {
		sh := &t.sh[i]
		sh.mu.Lock()
		for c := range sh.conns {
			if !c.sub && c.kill != nil {
				ls = append(ls, zduPaceLink{users: 1, close: c.kill})
			}
		}
		sh.mu.Unlock()
	}
	return ls
}

// b469Step (rc14h, HS2_B469_STEP_MS=N, 1..25, default 5; 25 = the b454 step):
// the user pace's smallest step. 24e Iran trace (rc14g + DRAINFAST): the
// TUN stalls in the pace are Iran run-queue waits, ~28% of them on the
// mconn end path (mConn.end, msub.close, subDied), filled by each batch of
// ~66 ends arriving together every 25 ms; no hot lock. A 5 ms step keeps
// the same rate in batches of ~13.
func b469Step() time.Duration {
	n := b469EnvInt("HS2_B469_STEP_MS", 5, 1, 25)
	return time.Duration(n) * time.Millisecond
}
