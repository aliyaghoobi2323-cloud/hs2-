package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
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
	min     int
	max     int
	perLink int // desired users per link before scaling up
	log     func(string, ...any)

	mu       sync.RWMutex
	links    []*managedLink
	users    atomic.Int32 // total active user connections across the pool
	closing  atomic.Bool
	scaleCtx context.Context
}

type managedLink struct {
	link Link
	id   int
	dead atomic.Bool
	born time.Time
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

// Run brings the pool up to Min links and then maintains it: rebuilds dead
// links, and scales the count with load until ctx ends.
func (m *LinkManager) Run(ctx context.Context) {
	m.scaleCtx = ctx
	// initial fill
	for i := 0; i < m.min; i++ {
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
	id := len(m.links)
	m.links = append(m.links, &managedLink{link: l, id: id, born: time.Now()})
	m.mu.Unlock()
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
	if idx >= 0 && best == 0 { // only retire a truly idle link, to not cut users
		ml := m.links[idx]
		ml.link.Close()
		m.links = append(m.links[:idx], m.links[idx+1:]...)
	}
}

// Pick returns the least-loaded alive link for a NEW user connection, and a
// release func to call when that user disconnects. This is the load-based
// assignment: fewest active streams wins.
func (m *LinkManager) Pick() (Link, func(), bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var chosen Link
	var best int32 = 1 << 30
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		if a := ml.link.Active(); a < best {
			best, chosen = a, ml.link
		}
	}
	if chosen == nil {
		return nil, func() {}, false
	}
	m.users.Add(1)
	release := func() { m.users.Add(-1) }
	return chosen, release, true
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
