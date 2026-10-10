package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Refill hold: placement of new user connections while the pool refills.
//
// A user connection stays on the link it was opened on until the user
// reconnects. After a start, or after every link died (an outage, the other
// server restarting), users reconnect within a few seconds while links come
// back through the dial gate at about 10 a second — so without this the first
// few links take nearly everyone (measured: up to 1,346 open connections on
// one link out of 300), and those users stay throttled on a crowded link long
// after the pool is full again.
//
// So for a short episode a new connection waits for a link with room instead
// of piling onto the first ones:
//
//   - The episode starts when a link comes up while the pool has no live link
//     (start, or total loss) and the pool wants at least 2 links.
//   - A link has room while it holds fewer OPEN user connections than the cap.
//     The cap is the fair share — open connections (placed + waiting) divided
//     by the links the pool wants — and never below per_link. (per_link is the
//     number of ACTIVE users a link is sized for — the autopilot sizes the
//     pool with it; the cap counts every open connection, active or idle, and
//     only exists during the episode.) It does not rise with time: a cap that
//     rises while links are still coming lets the first links fill up again
//     (measured: a restart with 6,000 connections for 63 links put 192 on the
//     busiest link with a cap doubling every 2 s, 200 with no hold, 96 with
//     the fixed fair share).
//   - Waiting connections are placed first come, first served as links come
//     up or connections end.
//   - The episode ends when the pool has all the links it can get — the
//     links it wants, but on the reverse edge never more than the Kharej
//     server's own max_links, which is all it dials — or when no new link has
//     come for refillStall, or at refillHoldMax at the latest. Then every connection still waiting goes
//     onto the least-loaded existing links — none is refused because of the
//     hold — and the log says so when that puts more on a link than the cap
//     allowed (a slow refill: the new links then take the new connections).
//   - UDP flows are not held (their port's read loop is shared: holding one
//     new flow would stall every other flow on that port).
//   - After an episode that ran into its time limit or stalled, no new one
//     starts for refillRearm, so a path whose links keep dying does not hold users again
//     and again.
var (
	refillHoldMax = 10 * time.Second
	// refillStall: links stopped coming. No new link for this long while some
	// are up means the other server keeps no more (an older hs2 with a fixed
	// count) or the path stopped them; holding users longer only delays them.
	// Above the 2 s tick a failure run waits out (dial gate, linkmanager.go).
	refillStall = 3 * time.Second
	refillTick  = 100 * time.Millisecond
	refillRearm = time.Minute
	refillKeep  = 5 * time.Minute // the last episode's summary stays in the status this long
)

// refillHold is the episode state, under LinkManager.mu.
type refillHold struct {
	on        bool
	start     time.Time
	lastLink  time.Time // the latest link arrival in this episode
	quietTill time.Time // no new episode before this (after a time-limited one)
	queue     []*holdWaiter

	// This episode's record, for the log and the status.
	held     int           // connections that waited
	longest  time.Duration // the longest wait
	topCap   int           // the highest cap applied
	announce bool          // the first wait was logged

	last   string // the last episode's summary (status)
	lastAt time.Time

	wg     sync.WaitGroup // the episode's goroutine (tests wait for it)
	manual bool           // tests step the episode themselves (no goroutine)
	tick   time.Duration  // > 0: overrides refillTick (tests)
}

// holdWaiter is a connection waiting for a link with room. Its link arrives on
// ch already counted (placeLocked); nil means the hold ended with no link to
// place it on — the caller goes back to the ordinary wait for a link.
type holdWaiter struct {
	at time.Time
	ch chan *managedLink
}

// noteArrivalLocked is called for every link that joins the pool, before it is
// added: a link arriving while no link is alive starts an episode. Caller
// holds m.mu.
func (m *LinkManager) noteArrivalLocked(now time.Time) {
	h := &m.hold
	if h.on {
		h.lastLink = now
		return
	}
	if m.aliveLocked() > 0 || now.Before(h.quietTill) || m.target.Load() < 2 {
		return
	}
	h.on, h.start, h.lastLink = true, now, now
	h.held, h.longest, h.topCap, h.announce = 0, 0, 0, false
	ctx := m.scaleCtx
	if ctx == nil {
		ctx = context.Background() // the episode ends on its own within refillHoldMax
	}
	if h.manual {
		return
	}
	tick := refillTick
	if h.tick > 0 {
		tick = h.tick
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		m.runRefill(ctx, tick)
	}()
}

// refillTargetLocked is the number of links an episode waits for: the pool's
// target, but on the reverse edge never more than the Kharej server's own
// ceiling once its links report it (kindInfo) — the exit dials no more than
// that, so waiting for the rest would hold users to the time limit for links
// that never come. Caller holds m.mu.
func (m *LinkManager) refillTargetLocked() (T int, capped bool) {
	T = max(1, int(m.target.Load()))
	if m.accept {
		if pm := m.peerMaxLocked(); pm > 0 && pm < T {
			return pm, true
		}
	}
	return T, false
}

// refillCapLocked is the hold's cap on open user connections per link: the
// fair share of the open connections (placed + waiting) over the links the
// pool can get, never below per_link. Caller holds m.mu.
func (m *LinkManager) refillCapLocked() int {
	T, _ := m.refillTargetLocked()
	D := int(m.users.Load()) + len(m.hold.queue)
	return max(m.perLink, (D+T-1)/T)
}

// pickHeld is Pick for a new TCP user connection. Outside an episode it is
// Pick. During one, it places the connection on a link with room, or queues it
// (w != nil: wait on w.ch, or cancelHeld). It returns neither a link nor a
// waiter when no link takes connections at all (the caller's ordinary wait for
// a link). A connection tried on other links already (tried, openStream) is
// placed on none of them; once queued it goes where the hold puts it (a link
// it waited on is lagging by then, pickLocked).
func (m *LinkManager) pickHeld(tried ...Link) (l Link, release func(), ok bool, w *holdWaiter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hold.on {
		ml := m.pickLocked(0, tried...)
		if ml == nil {
			return nil, nil, false, nil
		}
		m.placeLocked(ml)
		return ml.link, m.releaseFor(ml), true, nil
	}
	now := m.now()
	m.admitLocked(now) // first come, first served: those already waiting go first
	if len(m.hold.queue) == 0 {
		if ml := m.pickLocked(m.refillCapLocked(), tried...); ml != nil {
			m.placeLocked(ml)
			return ml.link, m.releaseFor(ml), true, nil
		}
	}
	if m.pickLocked(0, tried...) == nil {
		return nil, nil, false, nil // no link at all: not the hold's to wait for
	}
	w = &holdWaiter{at: now, ch: make(chan *managedLink, 1)}
	m.hold.queue = append(m.hold.queue, w)
	m.hold.held++
	if !m.hold.announce {
		m.hold.announce = true
		S, _ := m.countsLocked()
		c := m.refillCapLocked()
		m.hold.topCap = max(m.hold.topCap, c)
		T, _ := m.refillTargetLocked()
		m.log("refill: %d of %d links up — new connections wait (%s at most) for a link with room, so they spread over the links still opening instead of piling onto the first ones: at most %d open connections per link (the fair share) until the pool is up",
			S, T, fmtDur(refillHoldMax), c)
	}
	return nil, nil, false, w
}

// cancelHeld withdraws a waiter whose caller gave up; a link it was already
// given is released.
func (m *LinkManager) cancelHeld(w *holdWaiter) {
	m.mu.Lock()
	for i, q := range m.hold.queue {
		if q == w {
			m.hold.queue = append(m.hold.queue[:i], m.hold.queue[i+1:]...)
			m.mu.Unlock()
			return
		}
	}
	m.mu.Unlock()
	// Not queued any more: its answer is already in the channel (sent under
	// the lock).
	if ml := <-w.ch; ml != nil {
		m.releaseFor(ml)()
	}
}

// admitLocked places waiting connections, first come first served, while some
// link has room under the cap. Caller holds m.mu.
func (m *LinkManager) admitLocked(now time.Time) {
	if len(m.hold.queue) == 0 {
		return
	}
	c := m.refillCapLocked() // placing a waiter does not change it
	m.hold.topCap = max(m.hold.topCap, c)
	n := 0
	for _, w := range m.hold.queue {
		ml := m.pickLocked(c)
		if ml == nil {
			break
		}
		m.placeLocked(ml)
		m.hold.longest = max(m.hold.longest, now.Sub(w.at))
		w.ch <- ml
		n++
	}
	m.hold.queue = m.hold.queue[n:]
}

// runRefill drives an episode: places waiters as links come up and ends it.
func (m *LinkManager) runRefill(ctx context.Context, tick time.Duration) {
	tk := time.NewTicker(tick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			m.endRefill(m.now(), "")
			return
		case <-tk.C:
		}
		if m.refillStep(m.now()) {
			return
		}
	}
}

// refillStep is one tick of an episode; true when it ended.
func (m *LinkManager) refillStep(now time.Time) bool {
	m.mu.Lock()
	S, _ := m.countsLocked()
	T, _ := m.refillTargetLocked()
	e := now.Sub(m.hold.start)
	var why string
	switch {
	case !m.hold.on:
		m.mu.Unlock()
		return true
	case S >= T:
		why = "complete"
	case e >= refillHoldMax:
		why = "limit"
	case S > 0 && now.Sub(m.hold.lastLink) >= refillStall:
		why = "stalled"
	default:
		m.admitLocked(now)
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	m.endRefill(now, why)
	return true
}

// refillWhySlow says why a hold ended before the pool had its links. Links
// open at the dial gate's pace (~10 a second, dialgate.go), so a large pool
// cannot be back within the hold however good the path: blaming the path for
// that would send the operator after a fault that is not there. Only a pool
// well behind that pace points at the path or the other server.
func refillWhySlow(up, target int, took time.Duration) string {
	paced := 1 + int(took.Seconds()*gatePerSec)
	if up*10 >= paced*6 {
		return fmt.Sprintf("links open at the dial pace, about %d a second, so %d take about %ds — expected, not a fault", gatePerSec, target, (target+gatePerSec-1)/gatePerSec)
	}
	return fmt.Sprintf("links are coming slower than the dial pace of about %d a second: a slow or lossy path, or the other server still starting", gatePerSec)
}

// endRefill ends the episode: every connection still waiting goes onto the
// least-loaded link there is (none is refused because of the hold), and the
// log says how it went when anyone waited. why: "complete" (the pool has its
// links), "limit" (refillHoldMax) or "" (shutting down).
func (m *LinkManager) endRefill(now time.Time, why string) {
	m.mu.Lock()
	h := &m.hold
	if !h.on {
		m.mu.Unlock()
		return
	}
	h.on = false
	S, _ := m.countsLocked()
	T, capped := m.refillTargetLocked()
	if len(h.queue) > 0 {
		h.topCap = max(h.topCap, m.refillCapLocked())
	}
	late := len(h.queue)
	for _, w := range h.queue {
		ml := m.pickLocked(0)
		if ml != nil {
			m.placeLocked(ml)
			h.longest = max(h.longest, now.Sub(w.at))
		}
		w.ch <- ml // nil: no link at all — the caller's ordinary wait
	}
	h.queue = nil
	most := 0
	for _, ml := range m.links {
		if ml.link.Alive() {
			most = max(most, int(ml.users.Load()))
		}
	}
	took := now.Sub(h.start)
	if why == "limit" || why == "stalled" {
		h.quietTill = now.Add(refillRearm)
	}
	var line string
	switch {
	case h.held == 0 || why == "":
	case why == "complete":
		all := fmt.Sprintf("all %d links", S)
		if capped {
			all = fmt.Sprintf("all %d links the Kharej server allows (its max_links)", T)
		}
		line = fmt.Sprintf("refill: %s up after %s — %d connection(s) waited for a link with room (longest %s); at most %d open connections on one link (the hold allowed %d)",
			all, secs(took), h.held, secs(h.longest), most, h.topCap)
	case why == "stalled":
		line = fmt.Sprintf("refill: no new link for %s with %d of %d links up — the other server keeps no more (an older hs2 with a fixed count, or its max_links) or the path stopped them; the hold ended after %s and the %d connection(s) still waiting went onto the existing links, none refused: at most %d open connections on one link (the hold allowed %d)",
			fmtDur(refillStall), S, T, secs(took), late, most, h.topCap)
	case late > 0 && most > h.topCap:
		line = fmt.Sprintf("refill: hold ended at its %s limit with only %d of %d links up (%s) — the %d connection(s) still waiting went onto the existing links, none refused: up to %d open connections on one link, above the hold's %d, until more links come (new links take the new connections)",
			fmtDur(refillHoldMax), S, T, refillWhySlow(S, T, took), late, most, h.topCap)
	default:
		line = fmt.Sprintf("refill: hold ended at its %s limit with %d of %d links up — %d connection(s) waited (longest %s); at most %d open connections on one link (the hold allowed %d)",
			fmtDur(refillHoldMax), S, T, h.held, secs(h.longest), most, h.topCap)
	}
	if line != "" {
		h.last, h.lastAt = line[len("refill: "):], now
	}
	m.mu.Unlock()
	if line != "" {
		m.log("%s", line)
	}
}

// refillNoteLocked is the status line: the episode in progress, else the last
// one's summary for refillKeep. Caller holds m.mu (read).
func (m *LinkManager) refillNoteLocked(now time.Time) string {
	h := &m.hold
	if h.on {
		if len(h.queue) == 0 && h.held == 0 {
			return ""
		}
		S, _ := m.countsLocked()
		return fmt.Sprintf("refilling after a start or a total loss: %d of %d links up; %d new connection(s) waiting for a link with room (at most %d open per link for now); the hold ends within %s",
			S, m.target.Load(), len(h.queue), m.refillCapLocked(), secs(max(0, refillHoldMax-now.Sub(h.start))))
	}
	if h.last != "" && now.Sub(h.lastAt) < refillKeep {
		return fmt.Sprintf("last refill (%s ago): %s", fmtDur(now.Sub(h.lastAt)), h.last)
	}
	return ""
}

// secs renders a short duration to a tenth of a second ("3.4s").
func secs(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }
