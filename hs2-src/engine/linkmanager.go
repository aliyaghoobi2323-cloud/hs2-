package engine

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// LinkManager maintains a pool of N parallel TLS links to the same kharej
// endpoint (mtcp). Each link is an independent real TLS session with its own
// Chrome fingerprint and its own traffic shaping — so on the wire the pool looks
// like a browser with several tabs open to one site, not one fat tunnel.
//
// User connections are load-balanced across links and PINNED: a user stays on
// its chosen link for the whole connection, so there is no cross-link packet
// reordering (the head-of-line-blocking trap of real aggregation). Throughput
// gain comes from defeating per-connection throttling: N links carry up to N×
// the per-connection cap.
//
// The pool auto-scales between Min and Max links based on active user count,
// adds links quickly under load and removes them slowly (hysteresis), and
// rebuilds any link that dies in the background.

// linkDialer opens one authenticated link to the endpoint. It returns something
// that can carry user connections (a stream-muxed session) — abstracted so the
// TLS carrier provides it.
type LinkDialer interface {
	DialLink(ctx context.Context) (Link, error)
}

// Link is one live parallel connection. It can open a new multiplexed stream
// per user connection, and reports liveness and load.
type Link interface {
	// OpenStream starts a new logical stream for one user connection. The
	// returned ReadWriteCloser carries that user's bytes to the kharej panel.
	OpenStream() (stream, error)
	// Active returns the number of streams currently open (its load).
	Active() int32
	// Alive reports whether the link is usable.
	Alive() bool
	Close() error
}

type stream interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

type LinkManager struct {
	dialer  LinkDialer
	accept  bool // reverse edge: links are injected via AddLink, never dialed
	min     int
	max     int
	perLink int // desired users per link before scaling up
	log     func(string, ...any)

	mu       sync.RWMutex
	links    []*managedLink
	linkSeq  int          // monotonic id source, so ids never collide after reaps
	users    atomic.Int32 // total active user connections across the pool
	closing  atomic.Bool
	scaleCtx context.Context

	// OnLink, if set, is called (in its own goroutine) for every new link.
	OnLink func(Link)
}

// rawStreamOpener is implemented by links that can open a stream which does
// not count as user load (e.g. the TUN side channel).
type rawStreamOpener interface {
	OpenRawStream() (*smux.Stream, error)
}

type managedLink struct {
	link Link
	id   int
	dead atomic.Bool
	born time.Time
	// users is the number of user connections assigned here. It is raised
	// inside Pick, under the lock, so a burst of simultaneous connections
	// spreads across links instead of every one seeing the same counts.
	users atomic.Int32
}

func NewLinkManager(dialer LinkDialer, min, max, perLink int, logf func(string, ...any)) *LinkManager {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if perLink < 1 {
		perLink = 50
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &LinkManager{dialer: dialer, min: min, max: max, perLink: perLink, log: logf}
}

// jitterGap returns a randomized inter-dial gap (~120–480ms) used to stagger
// link establishment so the pool does not appear as one synchronized burst of
// identical connections.
func jitterGap() time.Duration {
	return 120*time.Millisecond + time.Duration(rand.IntN(360))*time.Millisecond
}

// AddLink injects an externally acquired link into the pool. It is used by the
// reverse edge, which does not dial links but accepts them from the peer that
// dials in; the pool, load-balancing and reaping are otherwise identical.
func (m *LinkManager) AddLink(l Link) {
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.links = append(m.links, &managedLink{link: l, id: id, born: time.Now()})
	n := len(m.links)
	m.mu.Unlock()
	m.log("mtcp: accepted reverse link (now %d)", n)
	if m.OnLink != nil {
		go m.OnLink(l)
	}
}

// Run brings the pool up to Min links and then maintains it: rebuilds dead
// links, and scales the count with load until ctx ends. In accept mode (reverse
// edge) it never dials or scales — it only reaps links the peer has dropped.
func (m *LinkManager) Run(ctx context.Context) {
	m.scaleCtx = ctx
	if m.accept {
		m.runAccept(ctx)
		return
	}
	// Initial fill, staggered with jitter. Opening the whole pool as one
	// simultaneous burst of identical TLS connections is a behavioral tell, so
	// establishment is spread over a short randomized window. Steady-state
	// capacity is unchanged (still m.min links, scaling to m.max under load), so
	// this costs only a one-time startup ramp — never throughput once warm.
	for i := 0; i < m.min; i++ {
		if i > 0 && !sleepCtx(ctx, jitterGap()) {
			m.closeAll()
			return
		}
		m.addLink(ctx)
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	lowSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-tick.C:
			m.reap(ctx)
			want := m.desiredCount()
			have := m.count()
			if want > have {
				// scale up quickly
				for i := 0; i < want-have; i++ {
					m.addLink(ctx)
				}
				lowSince = time.Now()
				m.log("mtcp: scaled up to %d links (users=%d)", m.count(), m.users.Load())
			} else if want < have {
				// scale down slowly: only after load stayed low for a while
				if time.Since(lowSince) > 30*time.Second {
					m.removeIdleLink()
					lowSince = time.Now()
					m.log("mtcp: scaled down to %d links (users=%d)", m.count(), m.users.Load())
				}
			} else {
				lowSince = time.Now()
			}
		}
	}
}

// runAccept maintains the reverse-edge pool: it only drops dead links (the peer
// dials new ones in via AddLink), never dials or scales.
func (m *LinkManager) runAccept(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-tick.C:
			m.mu.Lock()
			alive := m.links[:0]
			for _, ml := range m.links {
				if ml.link.Alive() {
					alive = append(alive, ml)
				} else {
					ml.link.Close()
				}
			}
			m.links = alive
			m.mu.Unlock()
		}
	}
}

// desiredCount maps active users to a link count, clamped to [min,max].
func (m *LinkManager) desiredCount() int {
	u := int(m.users.Load())
	want := (u + m.perLink - 1) / m.perLink // ceil(u/perLink)
	if want < m.min {
		want = m.min
	}
	if want > m.max {
		want = m.max
	}
	return want
}

func (m *LinkManager) addLink(ctx context.Context) {
	if m.count() >= m.max {
		return
	}
	l, err := m.dialer.DialLink(ctx)
	if err != nil {
		m.log("mtcp: link dial failed: %v", err)
		return
	}
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.links = append(m.links, &managedLink{link: l, id: id, born: time.Now()})
	m.mu.Unlock()
	if m.OnLink != nil {
		go m.OnLink(l)
	}
}

// reap rebuilds links that died, keeping the pool at least Min.
func (m *LinkManager) reap(ctx context.Context) {
	m.mu.Lock()
	alive := m.links[:0]
	dead := 0
	for _, ml := range m.links {
		if ml.link.Alive() {
			alive = append(alive, ml)
		} else {
			ml.link.Close()
			dead++
		}
	}
	m.links = alive
	m.mu.Unlock()
	for i := 0; i < dead; i++ {
		if m.count() < m.desiredCount() || m.count() < m.min {
			m.addLink(ctx)
		}
	}
	if dead > 0 {
		m.log("mtcp: rebuilt %d dead link(s), now %d", dead, m.count())
	}
}

func (m *LinkManager) removeIdleLink() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.links) <= m.min {
		return
	}
	// pick the least-loaded link to retire
	idx := -1
	var best int32 = 1 << 30
	for i, ml := range m.links {
		if a := ml.link.Active(); a < best {
			best, idx = a, i
		}
	}
	if idx >= 0 && best == 0 && m.links[idx].users.Load() == 0 { // only retire a truly idle link
		ml := m.links[idx]
		ml.link.Close()
		m.links = append(m.links[:idx], m.links[idx+1:]...)
	}
}

// Pick returns the least-loaded alive link for a NEW user connection, and a
// release func to call when that user disconnects. Choosing and counting
// happen under one lock, so N connections arriving together land on N
// different links (the old read-then-open-later count sent a whole burst to
// the first link, which per-connection throttling then capped). Ties are
// broken at random so load does not pile onto the oldest link.
func (m *LinkManager) Pick() (Link, func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var chosen *managedLink
	var best int32 = 1 << 30
	ties := 0
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		switch u := ml.users.Load(); {
		case u < best:
			best, chosen, ties = u, ml, 1
		case u == best:
			ties++
			if rand.IntN(ties) == 0 {
				chosen = ml
			}
		}
	}
	if chosen == nil {
		return nil, func() {}, false
	}
	chosen.users.Add(1)
	m.users.Add(1)
	var once sync.Once
	release := func() {
		once.Do(func() {
			chosen.users.Add(-1)
			m.users.Add(-1)
		})
	}
	return chosen.link, release, true
}

func (m *LinkManager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.links)
}

func (m *LinkManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ml := range m.links {
		ml.link.Close()
	}
	m.links = nil
}
