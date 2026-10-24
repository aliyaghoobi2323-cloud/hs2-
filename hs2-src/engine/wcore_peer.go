package engine

import "time"

// wcPeer is what the window cores of the links to one peer share (red team,
// round 3, finding 1; WINDOW_CORE §5.7): the runner's registry keeps one per
// peer address (wcSet.peers) and the model one per shared bottleneck. The
// links of the edge's pool to one exit share at least the exit's own path,
// and the exit's record of each of them (srtt − min_rtt of the sending
// socket, q_snd) reads the queue all of them build there, not the link's own
// part of it. Each core used to keep its references relative to the queue it
// found (q_ref, q_snd's reference, τ_hold), so each one took the queue the
// others held for the path's own: with 2 and 4 downloads on separate links
// the bottleneck's queue went to 21–32 ms and crept to 30–48 ms over two
// minutes, and a core that came late found the others' queue past Δq and
// stayed OFF for good (do no harm).
//
// What the cores share: the records of every link (the path's queue, n times
// as often as one link's), and which cores are on and what their fast
// streams carry. Pure as the core: no clock, no locks (the runner steps every
// link from one goroutine), no traffic.
type wcPeer struct {
	recs  []wcPeerRec // the links' records within wcPeerKeep, oldest first
	cores map[*wcore]wcPeerCore
}

// wcPeerRec is one record of the exit for one link: its q_snd.
type wcPeerRec struct {
	at time.Time
	q  time.Duration
	c  *wcore
}

// wcPeerCore is one core's latest evaluation.
type wcPeerCore struct {
	at time.Time
	on bool    // not OFF, with fast streams
	rF float64 // what its fast streams carried (bytes/s)
}

var (
	// wcPeerSpan: what the cores judge the path's queue by: the records of
	// the last two spans (each link's one, n links' 2n)
	wcPeerSpan = 2 * wcSndSpan
	// wcPeerKeep: records are kept this long.
	wcPeerKeep = 3 * wcSndSpan
	// wcPeerStale: a core not evaluated this long counts no more (a link
	// that ended, or an idle one the runner no longer steps).
	wcPeerStale = 3 * wcTickMax
)

func newWcPeer() *wcPeer { return &wcPeer{cores: map[*wcore]wcPeerCore{}} }

// noteRec keeps a record of c's link.
func (p *wcPeer) noteRec(c *wcore, now time.Time, q time.Duration) {
	p.recs = append(p.recs, wcPeerRec{now, q, c})
	cut := 0
	for cut < len(p.recs) && now.Sub(p.recs[cut].at) > wcPeerKeep {
		cut++
	}
	if cut > 0 {
		p.recs = append(p.recs[:0], p.recs[cut:]...)
	}
}

// queue is the p-quantile of the q_snd of the records within span of now
// (interpolated), and how many there were.
func (p *wcPeer) queue(now time.Time, span time.Duration, q float64) (time.Duration, int) {
	var v []time.Duration
	for _, r := range p.recs {
		if d := now.Sub(r.at); d >= 0 && d <= span {
			v = append(v, r.q)
		}
	}
	if len(v) == 0 {
		return 0, 0
	}
	return pctDur(v, q), len(v)
}

// noteCore keeps c's latest evaluation.
func (p *wcPeer) noteCore(c *wcore, now time.Time, on bool, rF float64) {
	p.cores[c] = wcPeerCore{now, on, rF}
	for k, v := range p.cores {
		if now.Sub(v.at) > wcPeerStale {
			delete(p.cores, k)
		}
	}
}

// others is how many cores other than c are on, and what their fast streams
// carry together.
func (p *wcPeer) others(c *wcore, now time.Time) (n int, r float64) {
	for k, v := range p.cores {
		if k != c && v.on && now.Sub(v.at) <= wcPeerStale {
			n++
			r += v.rF
		}
	}
	return n, r
}

// empty: no core and no record is left.
func (p *wcPeer) empty() bool { return len(p.cores) == 0 && len(p.recs) == 0 }

// drop forgets c (its link ended).
func (p *wcPeer) drop(c *wcore) {
	delete(p.cores, c)
	n := 0
	for _, r := range p.recs {
		if r.c != c {
			p.recs[n] = r
			n++
		}
	}
	p.recs = p.recs[:n]
}
