package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// refillRig is a LinkManager on a synthetic clock whose refill episodes the
// test drives itself (refillStep), so a 10 s refill runs in milliseconds.
type refillRig struct {
	t     *testing.T
	m     *LinkManager
	clock atomic.Int64
	links []*fakeLink

	mu   sync.Mutex
	logs []string
}

func newRefillRig(t *testing.T, target, perLink int) *refillRig {
	t.Helper()
	r := &refillRig{t: t}
	r.clock.Store(time.Unix(1_000_000, 0).UnixNano())
	r.m = NewLinkManager(nil, 1, max(target, 1), perLink, func(f string, a ...any) {
		r.mu.Lock()
		r.logs = append(r.logs, fmt.Sprintf(f, a...))
		r.mu.Unlock()
	})
	r.m.clock = r.now
	r.m.target.Store(int32(target))
	r.m.hold.manual = true // the test steps the episode itself
	return r
}

func (r *refillRig) now() time.Time          { return time.Unix(0, r.clock.Load()) }
func (r *refillRig) advance(d time.Duration) { r.clock.Add(int64(d)) }
func (r *refillRig) addLink() *fakeLink {
	l := &fakeLink{alive: true}
	r.links = append(r.links, l)
	r.m.AddLink(l, "test")
	return l
}
func (r *refillRig) step() bool              { return r.m.refillStep(r.now()) }
func (r *refillRig) episode() bool           { r.m.mu.RLock(); defer r.m.mu.RUnlock(); return r.m.hold.on }
func (r *refillRig) usersOn(l *fakeLink) int { return int(r.m.mlOf(l).users.Load()) }
func (r *refillRig) log() string             { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.logs, "\n") }
func (m *LinkManager) mlOf(l Link) *managedLink {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if ml.link == l {
			return ml
		}
	}
	return nil
}

// refillResult is how a simulated refill ended.
type refillResult struct {
	placed, refused int
	longest         time.Duration
	most            int     // open connections on the busiest link
	top15           float64 // share of all connections on the 15 busiest links
	allAt           time.Duration
	links           int
}

// simRefill replays a restart: D user connections arrive evenly over
// arriveOver while links come up one every linkEvery from linksFrom on (up to
// maxLinks), with the pool wanting T. hold: new connections go through the refill hold (as a
// TCP connection does); otherwise through Pick (the old placement). A
// connection that finds no link at all waits up to 6 s for one, as pickWait
// does, and is then refused.
func simRefill(t *testing.T, T, D int, arriveOver, linksFrom, linkEvery time.Duration, maxLinks int, hold bool) (refillResult, *refillRig) {
	r := newRefillRig(t, T, 8)
	const tick = 10 * time.Millisecond
	type conn struct {
		at time.Duration
		w  *holdWaiter
	}
	var noLink, waiting []*conn
	var res refillResult
	nextConn, nextLink := 0, 0
	try := func(c *conn, now time.Duration) bool {
		if hold {
			_, _, ok, w := r.m.pickHeld()
			if ok {
				return true
			}
			if w != nil {
				c.w = w
				waiting = append(waiting, c)
				return true
			}
			return false
		}
		_, _, ok := r.m.Pick()
		return ok
	}
	done := func(c *conn, now time.Duration) {
		res.placed++
		res.longest = max(res.longest, now-c.at)
		if res.placed == D {
			res.allAt = now
		}
	}
	for now := time.Duration(0); now <= 40*time.Second; now += tick {
		if nextLink < maxLinks && linksFrom+time.Duration(nextLink)*linkEvery <= now {
			r.addLink()
			nextLink++
		}
		for nextConn < D && time.Duration(nextConn)*arriveOver/time.Duration(D) <= now {
			c := &conn{at: now}
			nextConn++
			if try(c, now) {
				if c.w == nil {
					done(c, now)
				}
			} else {
				noLink = append(noLink, c)
			}
		}
		kept := noLink[:0]
		for _, c := range noLink {
			switch {
			case try(c, now):
				if c.w == nil {
					done(c, now)
				}
			case now-c.at > 6*time.Second:
				res.refused++
			default:
				kept = append(kept, c)
			}
		}
		noLink = kept
		if hold && now%(100*time.Millisecond) == 0 { // the episode's production tick
			r.step()
		}
		kw := waiting[:0]
		for _, c := range waiting {
			select {
			case ml := <-c.w.ch:
				if ml != nil {
					done(c, now)
				} else {
					noLink = append(noLink, c)
				}
			default:
				kw = append(kw, c)
			}
		}
		waiting = kw
		r.advance(tick)
	}
	var per []int
	for _, l := range r.links {
		per = append(per, r.usersOn(l))
	}
	slices.Sort(per)
	slices.Reverse(per)
	res.links = len(per)
	if len(per) > 0 {
		res.most = per[0]
	}
	top := 0
	for i := 0; i < 15 && i < len(per); i++ {
		top += per[i]
	}
	if res.placed > 0 {
		res.top15 = float64(top) / float64(res.placed)
	}
	if res.placed+res.refused+len(waiting)+len(noLink) != D {
		t.Fatalf("lost connections: placed %d refused %d waiting %d no-link %d of %d", res.placed, res.refused, len(waiting), len(noLink), D)
	}
	return res, r
}

// The load-test outage (2,400 connections, a pool of 300): every user is
// already waiting when the path comes back, and links return at the gate's
// ~10 a second. Without the hold the first link takes nearly everyone (the
// review measured 1,346 on one link); with it nobody is refused, nobody waits
// past the limit after the first link, and the busiest link holds a small
// multiple of the fair share.
func TestRefillHoldSpreadsAfterOutage(t *testing.T) {
	old, _ := simRefill(t, 300, 2400, 500*time.Millisecond, time.Second, 100*time.Millisecond, 300, false)
	got, r := simRefill(t, 300, 2400, 500*time.Millisecond, time.Second, 100*time.Millisecond, 300, true)
	t.Logf("without hold: busiest link %d, top-15 share %.0f%%", old.most, old.top15*100)
	t.Logf("with hold:    busiest link %d, top-15 share %.0f%%, longest wait %s, all placed at %s, refused %d",
		got.most, got.top15*100, got.longest, got.allAt, got.refused)
	if got.refused != 0 || got.placed != 2400 {
		t.Fatalf("placed %d, refused %d of 2400", got.placed, got.refused)
	}
	if got.allAt > time.Second+refillHoldMax+200*time.Millisecond {
		t.Fatalf("the last connection was placed at %s: the hold outlived its limit", got.allAt)
	}
	if got.most > 32 || got.top15 > 0.2 {
		t.Fatalf("busiest link %d open connections, top-15 share %.0f%% — the hold did not spread the refill", got.most, got.top15*100)
	}
	if old.most < 20*got.most {
		t.Fatalf("baseline busiest link %d vs %d with the hold: the scenario does not show the pile-up", old.most, got.most)
	}
	lg := r.log()
	if !strings.Contains(lg, "refill: 1 of 300 links up — new connections wait (10s at most)") || !strings.Contains(lg, "at most 8 open connections per link (the fair share)") {
		t.Fatalf("no start line:\n%s", lg)
	}
	if !strings.Contains(lg, "hold ended at its 10s limit with only") || !strings.Contains(lg, "none refused") {
		t.Fatalf("no explicit limit line:\n%s", lg)
	}
}

// Production's shape after a warm restart: 6,000 open connections (most of
// them idle) for a pool of 63. The cap counts open connections, so the fair
// share is ~96 a link — far above per_link (8 active users a link is sized
// for) — and the pool is complete within the limit, so every link ends at
// about the fair share.
func TestRefillHoldProductionRestart(t *testing.T) {
	old, _ := simRefill(t, 63, 6000, 3*time.Second, 300*time.Millisecond, 100*time.Millisecond, 63, false)
	got, r := simRefill(t, 63, 6000, 3*time.Second, 300*time.Millisecond, 100*time.Millisecond, 63, true)
	t.Logf("without hold: busiest link %d, top-15 share %.0f%%", old.most, old.top15*100)
	t.Logf("with hold:    busiest link %d, top-15 share %.0f%%, longest wait %s, all placed at %s",
		got.most, got.top15*100, got.longest, got.allAt)
	if got.refused != 0 || got.placed != 6000 {
		t.Fatalf("placed %d, refused %d of 6000", got.placed, got.refused)
	}
	if fair := (6000 + 62) / 63; got.most > fair || old.most < 2*got.most {
		t.Fatalf("busiest link %d (fair share %d), %d without the hold", got.most, fair, old.most)
	}
	if got.longest > 7*time.Second {
		t.Fatalf("a connection waited %s", got.longest)
	}
	if lg := r.log(); !strings.Contains(lg, "refill: all 63 links up after") {
		t.Fatalf("no end line:\n%s", lg)
	}
}

// A bad network: links come at 2 a second, so after 10 s only ~20 of 300 are
// up. The hold must end at its limit, put everyone still waiting onto the
// links there are — none refused — and say plainly that this is more per link
// than the cap.
func TestRefillHoldSlowRefillNeverRefuses(t *testing.T) {
	got, r := simRefill(t, 300, 2400, 3*time.Second, 0, 500*time.Millisecond, 300, true)
	t.Logf("slow refill: busiest link %d, longest wait %s, all placed at %s, refused %d", got.most, got.longest, got.allAt, got.refused)
	if got.refused != 0 || got.placed != 2400 {
		t.Fatalf("placed %d, refused %d of 2400", got.placed, got.refused)
	}
	if got.longest > refillHoldMax+refillTick {
		t.Fatalf("a connection waited %s (limit %s)", got.longest, refillHoldMax)
	}
	lg := r.log()
	if !strings.Contains(lg, "hold ended at its 10s limit with only 21 of 300 links up") || !strings.Contains(lg, "none refused") || !strings.Contains(lg, "above the hold's 8") {
		t.Fatalf("the limit line is missing or vague:\n%s", lg)
	}
	if st := r.m.Stats(); !strings.Contains(st.Refill, "last refill") || !strings.Contains(st.Refill, "above the hold's") {
		t.Fatalf("status after the episode: %q", st.Refill)
	}
	// After a time-limited episode a total loss does not start another one for
	// a while (a path whose links keep dying must not hold users each time).
	for _, l := range r.links {
		l.alive = false
	}
	r.advance(10 * time.Second)
	r.addLink()
	if r.episode() {
		t.Fatal("a new episode started right after a time-limited one")
	}
	r.advance(refillRearm)
	for _, l := range r.links {
		l.alive = false
	}
	r.addLink()
	if !r.episode() {
		t.Fatal("no episode after the re-arm time")
	}
}

// The hold belongs to a start or a total loss only: a link joining a pool
// that has live links never starts one, a total loss does.
func TestRefillHoldOnlyAfterStartOrTotalLoss(t *testing.T) {
	r := newRefillRig(t, 8, 8)
	a := r.addLink()
	if !r.episode() {
		t.Fatal("the first link did not start an episode")
	}
	for r.addLink(); len(r.links) < 8; r.addLink() {
	}
	if !r.step() || r.episode() {
		t.Fatal("the episode did not end once the pool had its links")
	}
	r.addLink() // a replacement into a live pool
	if r.episode() {
		t.Fatal("a link joining a live pool started an episode")
	}
	for _, l := range r.links {
		l.alive = false
	}
	_ = a
	r.addLink()
	if !r.episode() {
		t.Fatal("a total loss did not start an episode")
	}
	// A pool that wants a single link has nothing to spread.
	r1 := newRefillRig(t, 1, 8)
	r1.addLink()
	if r1.episode() {
		t.Fatal("an episode for a one-link pool")
	}
}

// The cap counts OPEN connections, never goes below per_link, and does not
// rise with time.
func TestRefillCap(t *testing.T) {
	r := newRefillRig(t, 63, 8)
	r.addLink()
	cap := func() int { r.m.mu.RLock(); defer r.m.mu.RUnlock(); return r.m.refillCapLocked() }
	if c := cap(); c != 8 {
		t.Fatalf("no connections yet: cap %d, want per_link 8", c)
	}
	r.m.users.Store(6000)
	if c := cap(); c != 96 {
		t.Fatalf("6000 open over 63 links: cap %d, want 96", c)
	}
	r.advance(9 * time.Second)
	if c := cap(); c != 96 {
		t.Fatalf("after 9s: cap %d, want still 96", c)
	}
}

// A waiting connection whose caller gives up leaves the queue; one that was
// already given a link gives it back.
func TestRefillHoldCancel(t *testing.T) {
	r := newRefillRig(t, 4, 1)
	r.addLink()
	if _, _, ok, w := r.m.pickHeld(); !ok || w != nil {
		t.Fatal("the first connection did not get the link")
	}
	_, _, ok, w1 := r.m.pickHeld()
	_, _, _, w2 := r.m.pickHeld()
	if ok || w1 == nil || w2 == nil {
		t.Fatal("connections past the cap were not queued")
	}
	r.m.cancelHeld(w1) // still queued
	r.addLink()
	r.step() // admits w2 onto the new link
	r.m.cancelHeld(w2)
	if u := r.m.users.Load(); u != 1 {
		t.Fatalf("%d open connections counted, want 1", u)
	}
	if len(r.m.hold.queue) != 0 {
		t.Fatal("queue not empty")
	}
}

// pickWait end to end on the real episode goroutine: TCP connections past the
// cap wait and are placed as links come; a UDP flow is never held.
func TestPickWaitRefillHoldLive(t *testing.T) {
	m := NewLinkManager(nil, 1, 4, 1, nil)
	m.target.Store(4)
	m.hold.tick = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	m.scaleCtx = ctx
	defer func() { cancel(); m.hold.wg.Wait() }()
	add := func() { m.AddLink(&fakeLink{alive: true}, "test") }
	add()
	type got struct {
		ok bool
		at time.Duration
	}
	start := time.Now()
	res := make(chan got, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, _, ok := pickWait(ctx, m, true)
			res <- got{ok, time.Since(start)}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	// One link, cap 1: one placed, three waiting. A UDP flow still goes now.
	if _, _, ok := pickWait(ctx, m, false); !ok {
		t.Fatal("a UDP flow was held")
	}
	for i := 0; i < 3; i++ {
		time.Sleep(50 * time.Millisecond)
		add()
	}
	for i := 0; i < 4; i++ {
		select {
		case g := <-res:
			if !g.ok {
				t.Fatal("a held connection was refused")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("held connections were not placed as links came")
		}
	}
	m.hold.wg.Wait()
	if m.hold.on {
		t.Fatal("the episode is still on with the pool full")
	}
}
