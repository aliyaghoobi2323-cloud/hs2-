package engine

import (
	"fmt"
	"math/rand/v2"
	"sync"
)

// R1 round 4, video over several links, the second approach (decision T21
// item 1): where a connection that moves to BULK goes.
//
// β2's move takes the pool's pick (pickLocked): the link with the fewest
// flows moving data. That spreads every heavy user's 16 connections over
// every BULK link, so each link counts every heavy user among its active
// ones, and a viewer whose one connection is on one link takes C/N of it
// (xshare.go has the numbers). XShare weighs the floors; PlaceUsers puts the
// connections where fewer users meet:
//
//   - a user may spread over at most m = ceil(L / U) of the L serving BULK
//     links, U the users with moved connections now (the edge's own book of
//     its moves, with the users due to move at the same mover tick): a lone
//     heavy user (U = 1) takes every link, as today (a per-connection
//     policer on the path wants that), eight heavy users on eight links one
//     each;
//   - while it is on fewer than m links, the next of its connections goes to
//     the serving link with the fewest other users there (ties: the fewest
//     moved connections, then at random); once on m, to the one of its own
//     with the fewest moved connections.
//
// PlaceUsers 1 (HS2_TUNE_PLACE_USERS=1) turns it on (the edge's alone: the
// exit takes a move stream on whichever link it comes).
//
// PlaceUsers 2: only heavy users are held to m links, U counting the heavy
// ones: a user with placeHeavy connections moved or moving at once (the
// rig's measure: with every user held, a browser's six connections of a
// page went onto one link and its page load p95 rose 1.25 times). Every
// other user's connection goes to the link with the fewest other users.

// PlaceUsers 3: heavy users as with 2; every other user's connection takes
// the pool's pick, as without the placement (the rig: with 2, a browser's
// page loads were 1.1 to 1.9 times slower; the heavy users held a link
// each is what spares the viewers).
//
// PlaceUsers: 1 every user held, 2 heavy users held, 3 heavy users held and
// the rest by the pool's pick (HS2_TUNE_PLACE_USERS), 0 the pool's pick.
var PlaceUsers = 2 // the default since R1 round 4 (round4/REPORT.fa.md)

// placeHeavy: a user with this many connections moved (or moving at the
// same tick) is heavy (PlaceUsers 2).
const placeHeavy = 8

// placeBook is the edge's moved connections by BULK link and user.
type placeBook struct {
	mu  sync.Mutex
	by  map[*mtcpLink]map[uint32]int
	due map[uint32]int // connections due to move at the mover's tick now, by user
}

func (b *placeBook) add(l *mtcpLink, tag uint32) {
	if l == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.by == nil {
		b.by = map[*mtcpLink]map[uint32]int{}
	}
	u := b.by[l]
	if u == nil {
		u = map[uint32]int{}
		b.by[l] = u
	}
	u[tag]++
}

func (b *placeBook) drop(l *mtcpLink, tag uint32) {
	if l == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.by[l]
	if u == nil {
		return
	}
	if u[tag]--; u[tag] <= 0 {
		delete(u, tag)
	}
	if len(u) == 0 {
		delete(b.by, l)
	}
}

// line is the book's part of the placement line: users with moved
// connections, and the users per BULK link (least, most).
func (b *placeBook) line() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	all := map[uint32]bool{}
	lo, hi := 0, 0
	for _, us := range b.by {
		for u := range us {
			all[u] = true
		}
		if n := len(us); lo == 0 || n < lo {
			lo = n
		}
		hi = max(hi, len(us))
	}
	return fmt.Sprintf("placed by users: %d users moved, %d to %d users a bulk link", len(all), lo, hi)
}

// heavy says whether tag has placeHeavy connections moved or moving now.
func (b *placeBook) heavy(tag uint32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.due[tag]
	for _, us := range b.by {
		n += us[tag]
	}
	return n >= placeHeavy
}

// setDue sets the connections due to move at the mover's tick, by user
// (nil: none).
func (b *placeBook) setDue(due map[uint32]int) {
	b.mu.Lock()
	b.due = due
	b.mu.Unlock()
}

// placeCand is a serving BULK link as the placement sees it.
type placeCand struct {
	ml    *managedLink
	link  *mtcpLink
	users map[uint32]int // moved connections there by user (the book's; nil: none)
	conns int
}

// choose picks among cands the link for a connection of user tag (the
// rule at the top of the file), under b.mu; nil with no candidate.
func (b *placeBook) choose(cands []*placeCand, tag uint32) *placeCand {
	if len(cands) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// each user's connections moved and moving now
	conns := map[uint32]int{tag: 1}
	for u, n := range b.due {
		conns[u] += n
	}
	for _, us := range b.by {
		for u, n := range us {
			conns[u] += n
		}
	}
	heavyOnly := PlaceUsers >= 2
	held := !heavyOnly || conns[tag] >= placeHeavy
	users := 0
	for _, n := range conns {
		if !heavyOnly || n >= placeHeavy {
			users++
		}
	}
	for _, c := range cands {
		c.users, c.conns = b.by[c.link], 0
		for _, n := range c.users {
			c.conns += n
		}
	}
	m := (len(cands) + max(users, 1) - 1) / max(users, 1)
	var own []*placeCand
	for _, c := range cands {
		if c.users[tag] > 0 {
			own = append(own, c)
		}
	}
	pool := cands
	if held && len(own) >= max(m, 1) {
		pool = own
	}
	var best *placeCand
	ties := 0
	for _, c := range pool {
		switch {
		case best == nil || placeLess(c, best, tag):
			best, ties = c, 1
		case !placeLess(best, c, tag):
			ties++
			if rand.IntN(ties) == 0 {
				best = c
			}
		}
	}
	return best
}

// placeLess orders two links for a connection of tag: fewer other users,
// then fewer moved connections.
func placeLess(a, b *placeCand, tag uint32) bool {
	oa, ob := len(a.users), len(b.users)
	if a.users[tag] > 0 {
		oa--
	}
	if b.users[tag] > 0 {
		ob--
	}
	if oa != ob {
		return oa < ob
	}
	return a.conns < b.conns
}

// pickBulk is the BULK link a group connection of user tag moves to
// (PlaceUsers), counted as the pool's pick counts it; ok false: none (the
// pool's pick then, the caller's).
func (q *quietSet) pickBulk(tag uint32) (*mtcpLink, func(), bool) {
	m := q.m
	now := ctrlNow()
	m.mu.Lock()
	defer m.mu.Unlock()
	var serving, prompt []*placeCand
	for _, ml := range m.links {
		l, ok := ml.link.(*mtcpLink)
		if !ok || !l.Alive() || !l.bulkClass() || ml.retiring || ml.degraded || ml.draining || ml.suspect {
			continue
		}
		c := &placeCand{ml: ml, link: l}
		serving = append(serving, c)
		if !ml.lagging(now) {
			prompt = append(prompt, c)
		}
	}
	// as pickLocked: links that lag are passed over while at most half lag
	cands := serving
	if len(prompt)*2 >= len(serving) && len(prompt) > 0 {
		cands = prompt
	}
	c := q.book.choose(cands, tag)
	if c == nil {
		return nil, nil, false
	}
	m.placeLocked(c.ml)
	return c.link, m.releaseFor(c.ml), true
}
