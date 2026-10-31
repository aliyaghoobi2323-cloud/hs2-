package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// R1 round-4 judge, the red team's C4 and C5 (docs/architecture/wip/r1/
// redteam/REPORT.fa.md): what one QUIET link's death, and one user's frozen
// reads, do to the other users of the QUIET links.
//
// C4. Every new connection opens on a QUIET link and stays there until it
// moves to a BULK one; a game, a call, a page's objects and the start of
// every download live there. A QUIET link that dies (a DPI box's reset, a
// keepalive or TCP_USER_TIMEOUT) cuts every connection of it that has not
// moved: what was in flight on its TCP is gone, and nothing at the group
// level keeps it to send again (a resend buffer is R2's: round4-judge.fa.md).
// pick spreads the connections over the QUIET links by count, so with Q
// links a death cuts ~1/Q of every user's connections: nearly every user
// who had several loses some (a page, a game next to a call). QuietByUser
// keeps each user's connections on one QUIET link (its first: the link
// with the fewest users, then connections; another while its own lags or
// is dead), so a death cuts the whole of ~1/Q of the users and none of the
// others: the share of connections is the same, the share of users hit is
// ~1/Q instead of nearly all.
//
// C5. A group stream's bytes wait in its QUIET link's session buffer until
// the app reads them; the buffer (SmuxSessionBuffer, and what the link may
// borrow from the window pool) is shared by every connection of the link,
// and once it is full the link's reader stops: every light user of it
// stalls (the wedge guard frees the link only after seconds). A user whose
// app stops reading (frozen, or a zero window on purpose) with a wave of new
// downloads pins one stream window each (~256 KiB the exit starts with)
// before the exit's move hint takes them off. QuietHoldCap bounds what one
// user may hold unread on QUIET links: past it, every connection of the
// user still on a QUIET link moves to a BULK one at once, and its new ones
// open on a BULK link, until what it holds falls under half the cap. Its
// bytes then wait on a BULK link (as they all did before R1), and the light
// users keep their QUIET links.

// QuietByUser (HS2_TUNE_QUIET_BYUSER): 1 keeps each user's connections on
// one QUIET link (C4 above); 0 (the default until measured): the least
// connections, as round 3.
var QuietByUser = newTuneInt(0)

// QuietHoldCap (HS2_TUNE_QUIET_HOLDCAP, bytes; 0, the default until
// measured: none): the most one user may hold unread on QUIET links before
// its connections move off them (C5 above).
var QuietHoldCap = newTuneInt(0)

// userQuiet is one user's QUIET link (QuietByUser) and its connections
// placed there by it.
type userQuiet struct {
	ql *quietLink
	n  int
}

// pickByUser is pick with QuietByUser: the user's own QUIET link while it
// is up, not in avoid and not lagging; else the link with the fewest users
// (then connections) that does not lag, which becomes the user's when the
// user had none or its link died (a lagging own link keeps the user: this
// connection only goes elsewhere).
func (q *quietSet) pickByUser(avoid []Link, tag uint32) (*quietLink, func(), bool) {
	now := ctrlNow()
	q.mu.Lock()
	defer q.mu.Unlock()
	usable := func(ql *quietLink) bool {
		return ql.link.Alive() && !ql.retiring.Load() && !ql.light.Load() && !slices.Contains(avoid, Link(ql.link))
	}
	u := q.byUser[tag]
	var best *quietLink
	if u != nil && usable(u.ql) && !meterLagging(u.ql.link.mtr, now) {
		best = u.ql
	} else {
		bestLag := true
		for _, ql := range q.links {
			if !usable(ql) {
				continue
			}
			lag := meterLagging(ql.link.mtr, now)
			if best == nil || bestLag && !lag ||
				lag == bestLag && (ql.nusers < best.nusers || ql.nusers == best.nusers && ql.users.Load() < best.users.Load()) {
				best, bestLag = ql, lag
			}
		}
		if best == nil {
			return nil, nil, false
		}
		if u == nil || !u.ql.link.Alive() || u.ql.retiring.Load() {
			if u != nil {
				u.ql.nusers-- // its link died or retires (QuietFollow): the user moves
			}
			u = &userQuiet{ql: best}
			best.nusers++
			if q.byUser == nil {
				q.byUser = map[uint32]*userQuiet{}
			}
			q.byUser[tag] = u
		}
	}
	stick := best == u.ql
	best.users.Add(1)
	q.m.users.Add(1)
	if stick {
		u.n++
	}
	var once sync.Once
	return best, func() {
		once.Do(func() {
			best.users.Add(-1)
			q.m.users.Add(-1)
			if !stick {
				return
			}
			q.mu.Lock()
			if u.n--; u.n <= 0 && q.byUser[tag] == u {
				delete(q.byUser, tag)
				u.ql.nusers--
			}
			q.mu.Unlock()
		})
	}, true
}

// holdState is the users past QuietHoldCap.
type holdState struct {
	mu    sync.Mutex
	users map[uint32]bool // held now
	// moved: the groups that moved off QUIET while their main stream there
	// may still hold bytes the app has not read (they count toward their
	// user's share until it has)
	moved  []*groupStream
	logAt  time.Time
	logged int64

	opened  atomic.Int64 // connections of held users opened on a BULK link
	movedN  atomic.Int64 // connections moved for their user's hold
	heldN   atomic.Int64 // users held, in all
	mostKiB atomic.Int64 // the most one user held unread on QUIET links
}

// held says whether user tag is past QuietHoldCap now.
func (q *quietSet) held(tag uint32) bool {
	if q == nil || tag == 0 || QuietHoldCap.Get() <= 0 {
		return false
	}
	q.hold.mu.Lock()
	defer q.hold.mu.Unlock()
	return q.hold.users[tag]
}

// holdMoved: gs just moved off its QUIET link; what its main stream there
// holds unread still counts (QuietHoldCap).
func (q *quietSet) holdMoved(gs *groupStream) {
	if QuietHoldCap.Get() <= 0 {
		return
	}
	q.hold.mu.Lock()
	q.hold.moved = append(q.hold.moved, gs)
	q.hold.mu.Unlock()
}

// unreadQuiet is what group gs, not moved (or moved back), holds unread on
// QUIET links: its main stream's bytes, and its move stream's if it moved
// back (both on QUIET links then).
func unreadQuiet(gs *groupStream) int {
	n := gs.g.Main().Unread()
	if s1 := gs.g.Moved(); s1 != nil {
		n += s1.Unread()
	}
	return n
}

// holdTick is the mover's look at what each user holds unread on QUIET
// links (groups: those not moved): the users held now (nil: none, or
// QuietHoldCap off).
func (q *quietSet) holdTick(groups []*groupStream) map[uint32]bool {
	limit := QuietHoldCap.Get()
	h := &q.hold
	if limit <= 0 {
		h.mu.Lock()
		h.users, h.moved = nil, nil
		h.mu.Unlock()
		return nil
	}
	per := map[uint32]int{}
	for _, s := range groups {
		s.mu.Lock()
		gone := s.closed || s.bcs != nil
		s.mu.Unlock()
		if !gone && s.tag != 0 {
			per[s.tag] += unreadQuiet(s)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := h.moved[:0:0]
	for _, s := range h.moved {
		s.mu.Lock()
		gone, back := s.closed, s.bcs == nil
		s.mu.Unlock()
		if gone || back {
			continue // ended, or moved back (then among groups)
		}
		n := s.g.Main().Unread()
		if n == 0 && s.g.Retired() {
			continue // its QUIET stream is done with
		}
		per[s.tag] += n
		keep = append(keep, s)
	}
	h.moved = keep
	most, newly := 0, 0
	for tag, n := range per {
		most = max(most, n)
		if n > limit && !h.users[tag] {
			if h.users == nil {
				h.users = map[uint32]bool{}
			}
			h.users[tag] = true
			h.heldN.Add(1)
			newly++
		}
	}
	if k := int64(most >> 10); k > h.mostKiB.Load() {
		h.mostKiB.Store(k)
	}
	for tag := range h.users {
		if per[tag] < limit/2 {
			delete(h.users, tag)
		}
	}
	if newly > 0 {
		h.logged += int64(newly)
		if now := q.m.now(); h.logAt.IsZero() || now.Sub(h.logAt) >= 30*time.Second {
			q.m.log("mtcp: %d user(s) held more than %d KiB unread on QUIET links (an app not reading): their connections move to BULK links, their new ones open there (HS2_TUNE_QUIET_HOLDCAP)", h.logged, limit>>10)
			h.logAt, h.logged = now, 0
		}
	}
	if len(h.users) == 0 {
		return nil
	}
	out := make(map[uint32]bool, len(h.users))
	for tag := range h.users {
		out[tag] = true
	}
	return out
}

// userLine is the placement line's part for QuietByUser and QuietHoldCap
// ("" when both are off).
func (q *quietSet) userLine() string {
	var b strings.Builder
	if QuietByUser.Get() != 0 {
		q.mu.Lock()
		n := len(q.byUser)
		var per []string
		links := slices.Clone(q.links)
		sort.Slice(links, func(i, j int) bool { return links[i].id < links[j].id })
		for _, ql := range links {
			per = append(per, fmt.Sprintf("q%d %d", ql.id, ql.nusers))
		}
		q.mu.Unlock()
		fmt.Fprintf(&b, "; by user: %d users on QUIET links (%s)", n, strings.Join(per, ", "))
	}
	if QuietHoldCap.Get() > 0 {
		q.hold.mu.Lock()
		now := len(q.hold.users)
		q.hold.mu.Unlock()
		fmt.Fprintf(&b, "; held users %d now, %d in all (most unread %d KiB), opened on bulk %d, moved %d",
			now, q.hold.heldN.Load(), q.hold.mostKiB.Load(), q.hold.opened.Load(), q.hold.movedN.Load())
	}
	return b.String()
}
