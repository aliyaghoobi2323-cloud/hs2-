package engine

import (
	"slices"
	"time"
)

// Two ends of different QUIET counts (R1 round-4 judge, item 4).
//
// The QUIET count is each build's own (QuietCount: HS2_TUNE_QUIET_MANY,
// HS2_TUNE_QUIET_N, ...), and until round 4 both ends had to agree: in
// reverse the exit dials the links and holds at most QuietTotal(max_links)
// of them with ITS count, while the edge gives each arriving link its class
// with ITS count, QUIET first. An edge that wants more QUIET links than the
// exit counts then ends short of BULK links (c4's edge, 4 past 5 links, with
// j3g's exit, 2: 4 QUIET and max_links − 2 BULK), and at one link with no
// BULK link at all (an edge of 2 QUIET links with an exit of 1: both links
// QUIET, every connection stays on QUIET and no move finds a BULK link).
//
// With QuietFollow (HS2_TUNE_QUIET_FOLLOW, default 1) neither end has to
// match the other; both run the smaller count, with no new wire field:
//   - the reverse edge fits its QUIET links to the ceiling the exit reports
//     over kindInfo (its max_links and ITS QUIET count, as every R1 exit
//     reports): QUIET = min(own, ceiling − BULK max), at least 1
//     (followWant). The edge asks the exit for that many past its BULK
//     target, and a QUIET link past it takes no new connection and is
//     closed once its connections ended (followTick), so the exit dials a
//     BULK one in its place;
//   - the reverse exit dials up to quietPeerMax QUIET links past max_links
//     when the edge asks for them (exitRevCeilings), while it still REPORTS
//     its own count: an edge of an earlier build, which does not fit, keeps
//     its count and every BULK link (c4's edge with this exit of 2: 4 QUIET
//     and max_links BULK); an edge of this build fits to the report, so the
//     operator's max_links bounds the BULK links exactly;
//   - a direct edge dials its QUIET links itself and a direct exit takes
//     every link it is dialed (it applies no ceiling: stream_kharej.go), so
//     the edge's count simply holds there; nothing changes in direct.
// The cost: an edge of this build never runs more QUIET links than the exit
// counts; the count both run is the smaller one, which is the earlier,
// measured, behaviour of at least one of them.

// QuietFollow 0 (HS2_TUNE_QUIET_FOLLOW=0): both ends must set the QUIET count
// alike, as up to R1 round 4.
var QuietFollow = newTuneInt(1)

// quietPeerMax is the most QUIET links past max_links the reverse exit dials
// for an edge that asks for them, beyond its own count: the most any earlier
// R1 build keeps (round 4's c4: HS2_TUNE_QUIET_MANY=4 past 5 links). An edge
// of this build never asks for more than the exit reports (followWant).
const quietPeerMax = 4

// exitRevCeilings is the reverse exit's two ceilings for max_links maxLinks
// and its own QUIET count own: report, the most links it tells the edge it
// holds (kindInfo; QuietTotal with its own count, as every R1 exit), and
// hi, the most it dials when the edge asks for them (QuietFollow: room for
// up to quietPeerMax QUIET links; the edge's own ceiling lowers it further,
// exitPool.bounds).
func exitRevCeilings(maxLinks, own int, follow bool) (report, hi int) {
	if QuietExtra != 0 {
		report = maxLinks + own
	} else {
		report = max(maxLinks, 1+own)
	}
	hi = report
	if follow && QuietExtra != 0 {
		hi = max(report, maxLinks+quietPeerMax)
	}
	return report, hi
}

// followWant is the QUIET links an edge whose own count is want keeps next
// to bulkMax BULK links when the other server reports the ceiling peerMax
// (0: none heard yet): what the ceiling leaves past the BULK links, at
// least one, at most want.
func followWant(want, bulkMax, peerMax int) int {
	if peerMax <= 0 || want <= 1 {
		return want
	}
	return min(want, max(1, peerMax-bulkMax))
}

// wantNow is the QUIET links the edge keeps now.
func (q *quietSet) wantNow() int {
	if q.follow {
		if e := q.eff.Load(); e > 0 {
			return int(e)
		}
	}
	return q.want
}

// peerMax is the other server's ceiling as any live link reports it, BULK or
// QUIET (an edge whose links all came up QUIET has no BULK one to hear it on).
func (q *quietSet) peerMax() int {
	best := q.m.peerMax()
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, ql := range q.links {
		if ql.link.Alive() && ql.link.mtr != nil {
			best = max(best, int(ql.link.mtr.peerMax.Load()))
		}
	}
	return best
}

// QuietFollowGraceMs (HS2_TUNE_QUIET_FOLLOW_GRACE, ms; 0: none): a retiring
// QUIET link is closed only after it had no connection for this long. A
// connection's slot on its QUIET link is let go the moment it moves to a
// BULK link (quietSet.move), but its main stream still takes the frames the
// exit sent before it switched (a round trip, more behind a resend): a
// link closed at once cut them, the group's numbers had a hole
// (ErrGroupHole) and the user's connection ended — on the very start of a
// reverse edge next to an exit with fewer QUIET links, when the downloads
// of the first second move (the R1 red team's second pass, R2-3).
var QuietFollowGraceMs = newTuneInt(5000)

// followCloseNow says whether a retiring QUIET link with users connections
// may be closed at now (ctrlNow), noting when it was first found empty.
func followCloseNow(ql *quietLink, users int32, now int64) bool {
	if users > 0 {
		ql.emptySince.Store(0)
		return false
	}
	g := int64(max(QuietFollowGraceMs.Get(), 0)) * int64(time.Millisecond)
	if g == 0 {
		return true
	}
	at := ql.emptySince.Load()
	if at == 0 {
		ql.emptySince.Store(now)
		return false
	}
	return now-at >= g
}

// followTick is the reverse edge's tick of QuietFollow: it fits the count to
// the ceiling the exit reports (logging each change, and telling the exit
// the new count at once), marks the QUIET links past it as retiring (the
// fewest connections, then the newest, first; a retiring one comes back if
// the kept ones fall short), and closes each retiring link that has no
// connection left: the exit's slot dials again and the link comes up BULK.
func (q *quietSet) followTick() {
	if q == nil || !q.follow || !q.active() {
		return
	}
	if pm := q.peerMax(); pm > 0 {
		e := followWant(q.want, q.m.bulkMax(), pm)
		prev := int(q.eff.Swap(int32(e)))
		if prev == 0 {
			prev = q.want
		}
		if prev != e {
			if e < q.want {
				q.m.log("mtcp: the Kharej server holds at most %d links (its max_links and its own QUIET count): this server keeps %d QUIET links next to its %d BULK ones, not %d (HS2_TUNE_QUIET_FOLLOW)", pm, e, q.m.bulkMax(), q.want)
			} else {
				q.m.log("mtcp: the Kharej server's ceiling (%d links) leaves room for this server's %d QUIET links again", pm, e)
			}
			q.m.notifyCtl()
		}
	}
	keep := q.wantNow()
	q.mu.Lock()
	var kept, ret []*quietLink
	for _, ql := range q.links {
		if !ql.link.Alive() {
			continue
		}
		if ql.retiring.Load() {
			ret = append(ret, ql)
		} else {
			kept = append(kept, ql)
		}
	}
	if len(kept) > keep {
		// the fewest connections first, then the newest
		slices.SortStableFunc(kept, func(a, b *quietLink) int {
			if ua, ub := a.users.Load(), b.users.Load(); ua != ub {
				return int(ua - ub)
			}
			return b.born.Compare(a.born)
		})
		for _, ql := range kept[:len(kept)-keep] {
			ql.retiring.Store(true)
			ret = append(ret, ql)
		}
	} else if len(kept) < keep && len(ret) > 0 {
		// the most connections first
		slices.SortStableFunc(ret, func(a, b *quietLink) int { return int(b.users.Load() - a.users.Load()) })
		n := min(keep-len(kept), len(ret))
		for _, ql := range ret[:n] {
			ql.retiring.Store(false)
		}
		ret = ret[n:]
	}
	var closing []*quietLink
	now := ctrlNow()
	for _, ql := range ret {
		if ql.retiring.Load() && followCloseNow(ql, ql.users.Load(), now) {
			closing = append(closing, ql)
		}
	}
	q.mu.Unlock()
	for _, ql := range closing {
		q.m.log("mtcp: QUIET link q%d closed: past the %d QUIET links this server keeps now (no connection on it)", ql.id, keep)
		ql.link.Close()
	}
}
