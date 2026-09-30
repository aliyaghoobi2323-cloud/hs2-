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

	// The adaptive sizing brain and the measurements that feed it — all touched
	// only by the Run/runAccept goroutine, except target and the monitor fields
	// which are atomic so the pool-control sender and the status reporter can
	// read them from other goroutines.
	ap            *autopilot // pure controller: samples in, desired link count out
	aggGoodput    float64    // EWMA-summed goodput across links, bytes/sec
	poolSaturated bool       // last sample: links are network-limited (want to push more)

	target atomic.Int32 // latest desired link count (published for the reverse exit)
	// pin, when > 0, overrides the autopilot's decision (tests only).
	pin atomic.Int32
	// targetDropAt is when the reverse edge's target last went down; an idle link
	// is only retired once the exit has had time to learn the lower target, so it
	// retires that slot instead of redialing it.
	targetDropAt time.Time
	phase        atomic.Int32  // last apPhase, for the live monitor
	lastAgg      atomic.Uint64 // last aggregate goodput, bytes/sec, for the live monitor
	lastSat      atomic.Bool   // last saturation reading, for the live monitor
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

	// Health fields, read/written only under LinkManager.mu in the maintain
	// tick and in Pick, so they need no atomics.
	mtr             *linkMeter // nil for links without metering (never soft-degrades)
	prevRd          uint64     // download byte counter at the last sample
	prevWr          uint64     // upload byte counter at the last sample
	prevRetrans     uint64     // local (upload) TCP retransmit counter at last sample
	prevPeerRetrans uint64     // peer (download) TCP retransmit counter at last sample
	goodput         float64    // EWMA bytes/sec, both directions (diagnostic)
	lowStreak       int        // consecutive high-loss samples (either direction)
	sampled         bool       // prev* are valid (skips the first delta)
	degraded        bool       // soft-bad: excluded from new-user routing
	draining        bool       // being retired after its replacement is up
	drainSince      time.Time
}

// newManaged wraps a Link with a managed entry, capturing its meter (if any).
func (m *LinkManager) newManaged(l Link, id int) *managedLink {
	return &managedLink{link: l, id: id, born: time.Now(), mtr: linkMeterOf(l)}
}

func NewLinkManager(dialer LinkDialer, min, max, perLink int, logf func(string, ...any)) *LinkManager {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if perLink < 1 {
		perLink = 8
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &LinkManager{dialer: dialer, min: min, max: max, perLink: perLink, log: logf,
		ap: newAutopilot(min, max, perLink)}
	m.target.Store(int32(warmSize(min, max)))
	return m
}

// jitterGap returns a randomized inter-dial gap (~40–160ms) used to stagger link
// establishment so the pool does not appear as one synchronized burst. The window
// is deliberately small: over a typical pool it spreads establishment across
// roughly a second — enough to defeat a "N connections within a few ms" burst
// detector, but not so long that the spread itself becomes anomalous versus a
// real page load opening its parallel connections.
func jitterGap() time.Duration {
	return 40*time.Millisecond + time.Duration(rand.IntN(120))*time.Millisecond
}

// AddLink injects an externally acquired link into the pool. It is used by the
// reverse edge, which does not dial links but accepts them from the peer that
// dials in; the pool, load-balancing and reaping are otherwise identical. from
// names the peer for the log. It returns the link's id.
func (m *LinkManager) AddLink(l Link, from string) int {
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.links = append(m.links, m.newManaged(l, id))
	n := m.aliveLocked()
	m.mu.Unlock()
	m.log("mtcp: reverse link %d up from %s (now %d)", id, from, n)
	if m.OnLink != nil {
		go m.OnLink(l)
	}
	return id
}

// DropLink removes a reverse link as soon as it closes, and logs why, so the
// pool and its "now N" count never include links that are already gone.
func (m *LinkManager) DropLink(l Link, from string) {
	m.mu.Lock()
	id := -1
	kept := m.links[:0]
	for _, ml := range m.links {
		if ml.link == l {
			id = ml.id
			continue
		}
		kept = append(kept, ml)
	}
	m.links = kept
	n := m.aliveLocked()
	m.mu.Unlock()
	l.Close()
	if id < 0 || m.closing.Load() {
		return // already reaped, or the whole pool is shutting down
	}
	m.log("mtcp: reverse link %d from %s down: %s (now %d)", id, from, linkDownReason(l), n)
}

// aliveLocked counts usable links. Caller holds m.mu.
func (m *LinkManager) aliveLocked() int {
	n := 0
	for _, ml := range m.links {
		if ml.link.Alive() {
			n++
		}
	}
	return n
}

// linkDownReason asks a link why it stopped, when it can tell.
func linkDownReason(l Link) string {
	if r, ok := l.(interface{ downReason() string }); ok {
		return r.downReason()
	}
	return "closed"
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
	// establishment is spread over a short randomized window. The pool comes up
	// warm (warmStartLinks, clamped to the envelope) rather than at min, so a
	// burst of user connections arriving right after start spreads across enough
	// links to beat per-connection throttling at once; the autopilot then shrinks
	// toward min if the tunnel is idle. This costs only a one-time startup ramp.
	warm := warmSize(m.min, m.max)
	for i := 0; i < warm; i++ {
		if i > 0 && !sleepCtx(ctx, jitterGap()) {
			m.closeAll()
			return
		}
		m.addLink(ctx)
	}
	tick := time.NewTicker(healthTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-tick.C:
			m.reap(ctx)
			m.sampleHealth() // per-link goodput + loss; flags soft-bad links
			m.heal(ctx)      // dial replacements, then drain the bad ones
			m.autoscale(ctx) // size the pool by user-count floor + throughput demand
		}
	}
}

// decideTarget runs one autopilot tick from the current measurements, publishes
// the result for the reverse exit and the live monitor, logs a note when the
// controller has one to make, and returns the desired link count. It is the one
// place both the direct edge (which then dials to match) and the reverse edge
// (which sends the target to the exit) get their number, so both directions size
// the pattern by exactly the same logic.
func (m *LinkManager) decideTarget(have int) int {
	if p := int(m.pin.Load()); p > 0 {
		m.setTarget(p)
		return p
	}
	d := m.ap.decide(apSample{
		now:        time.Now(),
		users:      int(m.users.Load()),
		have:       have,
		aggGoodput: m.aggGoodput,
		saturated:  m.poolSaturated,
	})
	m.setTarget(d.target)
	m.phase.Store(int32(d.phase))
	m.lastAgg.Store(uint64(m.aggGoodput))
	m.lastSat.Store(m.poolSaturated)
	if d.note != "" {
		m.log("mtcp: %s", d.note)
	}
	return d.target
}

// setTarget publishes a new desired link count and remembers when it went down.
func (m *LinkManager) setTarget(n int) {
	if old := int(m.target.Swap(int32(n))); n < old {
		m.targetDropAt = time.Now()
	}
}

// warmSize is the pool size a tunnel comes up at: warmStartLinks, clamped to the
// envelope. Both ends use it so the exit's first dial count and the edge's first
// published target agree — no churn at start.
func warmSize(min, max int) int {
	w := warmStartLinks
	if w < min {
		w = min
	}
	if w > max {
		w = max
	}
	return w
}

// retireIdleReverse shrinks the REVERSE pool safely. The exit cannot tell which
// of its links carry users, so it never closes one on its own when the target
// drops; the edge — which knows every link's users — closes one link that has
// none, and the exit then retires that slot instead of redialing it. At most one
// link per tick, and only after the exit has had time to learn the lower target.
// A link carrying a user is never closed to shrink, so shrinking cannot reset a
// connection.
func (m *LinkManager) retireIdleReverse(now time.Time) {
	want := int(m.target.Load())
	if now.Sub(m.targetDropAt) < retireAfterDrop {
		return
	}
	m.mu.Lock()
	live := 0
	for _, ml := range m.links {
		if ml.link.Alive() {
			live++
		}
	}
	if live <= want || live <= m.min {
		m.mu.Unlock()
		return
	}
	idx := -1
	for i, ml := range m.links {
		if ml.link.Alive() && !ml.draining && ml.users.Load() == 0 && ml.link.Active() == 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return // every link is busy: keep them, never cut a live connection
	}
	ml := m.links[idx]
	m.links = append(m.links[:idx], m.links[idx+1:]...)
	m.mu.Unlock()
	ml.link.Close() // DropLink finds it already gone and stays quiet
	m.log("mtcp: pattern → %d links: retired idle reverse link %d (%d up)", want, ml.id, live-1)
}

// autoscale sizes the DIRECT pool: it asks the autopilot for the desired link
// count and moves the real pool toward it — dialing up quickly (adding capacity
// disturbs no one) and shrinking one idle link at a time. The autopilot itself
// decides by the connection floor and a throughput probe (see autopilot.go).
func (m *LinkManager) autoscale(ctx context.Context) {
	have := m.count()
	want := m.decideTarget(have)
	switch {
	case want > have:
		for i := 0; i < want-have && m.count() < m.max; i++ {
			m.addLink(ctx)
		}
		if got := m.count(); got > have {
			m.log("mtcp: pattern now %d links (%s; users=%d, ~%.1f Mbit/s)",
				got, apPhase(m.phase.Load()), m.users.Load(), mbitps(m.aggGoodput))
		} else if want > have {
			m.log("mtcp: want %d links but only %d up — dials failing (peer down or path blocked; see dial errors)", want, got)
		}
	case want < have:
		if m.removeIdleLink() {
			m.log("mtcp: pattern now %d links (shrinking; users=%d)", m.count(), m.users.Load())
		}
	}
}

// runAccept maintains the reverse-edge pool: it never dials, but it still watches
// health so it can route users away from a soft-bad link and drop it — the peer
// (which does dial) then redials a fresh one to keep its link count.
func (m *LinkManager) runAccept(ctx context.Context) {
	tick := time.NewTicker(healthTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-tick.C:
			m.sampleHealth()
			// The reverse edge cannot dial, but it is still the brain: it runs the
			// same autopilot and publishes the desired count for the exit to match
			// over the pool-control channel (see exit_pool.go / openPoolCtl).
			// It decides from the COMMITTED size (its current target), not the
			// momentary link count: links arrive one by one after a restart, and
			// "hold what is up right now" would tell the exit to cut back to
			// whatever had connected so far.
			m.decideTarget(int(m.target.Load()))
			m.retireIdleReverse(time.Now())
			m.mu.Lock()
			alive := m.links[:0]
			now := time.Now()
			for _, ml := range m.links {
				if !ml.link.Alive() {
					ml.link.Close()
					m.log("mtcp: reverse link %d down: %s", ml.id, linkDownReason(ml.link))
					continue
				}
				if ml.degraded && !ml.draining {
					ml.draining = true
					ml.drainSince = now
					m.log("mtcp: reverse link %d degraded — draining (peer will redial)", ml.id)
				}
				if ml.draining && (ml.users.Load() == 0 || now.Sub(ml.drainSince) > maxDrain) {
					ml.link.Close() // peer's maintainExitLink redials to restore the count
					continue
				}
				alive = append(alive, ml)
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
	m.links = append(m.links, m.newManaged(l, id))
	m.mu.Unlock()
	if m.OnLink != nil {
		go m.OnLink(l)
	}
}

// addReplacement dials a fresh link for make-before-break healing. Unlike
// addLink it bypasses the max cap: it is a temporary over-provision so a degraded
// link's users keep full capacity until they drain onto the new link, after which
// heal drops the degraded one and the pool returns to size.
func (m *LinkManager) addReplacement(ctx context.Context) {
	l, err := m.dialer.DialLink(ctx)
	if err != nil {
		m.log("mtcp: replacement dial failed: %v", err)
		return
	}
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.links = append(m.links, m.newManaged(l, id))
	m.mu.Unlock()
	if m.OnLink != nil {
		go m.OnLink(l)
	}
	m.log("mtcp: dialed replacement link %d (make-before-break)", id)
}

// reap rebuilds links that died, keeping the pool at least Min. Each death is
// logged with its reason.
func (m *LinkManager) reap(ctx context.Context) {
	m.mu.Lock()
	alive := m.links[:0]
	var dead []*managedLink
	for _, ml := range m.links {
		if ml.link.Alive() {
			alive = append(alive, ml)
		} else {
			ml.link.Close()
			dead = append(dead, ml)
		}
	}
	m.links = alive
	m.mu.Unlock()
	for _, ml := range dead {
		m.log("mtcp: link %d down: %s", ml.id, linkDownReason(ml.link))
	}
	for range dead {
		if m.count() < m.desiredCount() || m.count() < m.min {
			m.addLink(ctx)
		}
	}
	if len(dead) > 0 {
		m.log("mtcp: redialed after %d link(s) went down — now %d up", len(dead), m.count())
	}
}

// removeIdleLink retires one truly-idle link (no active streams, no assigned
// users) when the pool is above its floor, and reports whether it did. A busy
// link is never dropped, so shrinking cannot disturb a live connection.
func (m *LinkManager) removeIdleLink() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.links) <= m.min {
		return false
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
		return true
	}
	return false
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
	// Prefer a healthy link with headroom; only if every link is degraded or
	// draining do we fall back to one of those (a soft-bad link still beats
	// refusing the user).
	chosen := m.leastLoaded(false)
	if chosen == nil {
		chosen = m.leastLoaded(true)
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

// leastLoaded returns the alive link carrying the fewest users, breaking ties at
// random. With allowBad=false it skips degraded/draining links (new users go to
// healthy links only); allowBad=true considers every alive link as a fallback.
// Caller holds m.mu.
func (m *LinkManager) leastLoaded(allowBad bool) *managedLink {
	var chosen *managedLink
	var best int32 = 1 << 30
	ties := 0
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		if !allowBad && (ml.degraded || ml.draining) {
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
	return chosen
}

// sampleHealth measures each metered link's throughput and, from the kernel's
// TCP retransmit counter, its path loss. A link that is actively moving data and
// retransmitting more than lossFrac of its packets for degradeStreak samples is
// flagged degraded. The activity gate (activeBytes) is the key safety property:
// an idle link — low throughput because its users are quiet, not because the link
// is bad — is never judged, so healthy users are never drained by mistake.
func (m *LinkManager) sampleHealth() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var agg float64
	activeLinks, pressLinks := 0, 0
	for _, ml := range m.links {
		if ml.mtr == nil {
			continue
		}
		rd := ml.mtr.rdBytes.Load()
		wr := ml.mtr.wrBytes.Load()
		upRT, haveRT := ml.linkRetransSource() // local: upload-path retransmits
		peerRT := ml.mtr.peerRetrans.Load()    // from control channel: download-path
		peerSeen := ml.mtr.peerSeen.Load()
		if !ml.sampled {
			ml.prevRd, ml.prevWr, ml.prevRetrans, ml.prevPeerRetrans, ml.sampled = rd, wr, upRT, peerRT, true
			continue
		}
		dRd := rd - ml.prevRd
		dWr := wr - ml.prevWr
		dUp := upRT - ml.prevRetrans
		dDown := peerRT - ml.prevPeerRetrans
		ml.prevRd, ml.prevWr, ml.prevRetrans, ml.prevPeerRetrans = rd, wr, upRT, peerRT

		rate := float64(dRd+dWr) / healthTick.Seconds()
		if ml.goodput == 0 {
			ml.goodput = rate
		} else {
			ml.goodput = gpAlpha*rate + (1-gpAlpha)*ml.goodput
		}
		agg += ml.goodput

		// Saturation signals for the autopilot: a link that moved a real amount of
		// data this tick is "active"; one whose socket still has bytes the network
		// would not take is "pressing" (a per-connection cap biting). Either way,
		// another link might carry more — so the pool is a candidate to grow.
		if dRd+dWr >= activeBytes {
			activeLinks++
		}
		if p, ok := linkPressureOf(ml.link); ok && p {
			pressLinks++
		}

		if ml.degraded || ml.draining {
			continue
		}
		// A direction is "bad" when it is actively moving data and retransmitting
		// more than lossFrac of its packets. Upload uses local TCP_INFO; download
		// uses the exit's retransmits from the control channel (phase 3), so a link
		// bad only on the download path is caught too.
		bad := false
		if haveRT && dWr >= activeBytes {
			if pkts := float64(dWr) / mss; pkts > 0 && float64(dUp)/pkts > lossFrac {
				bad = true
			}
		}
		if peerSeen && dRd >= activeBytes {
			if pkts := float64(dRd) / mss; pkts > 0 && float64(dDown)/pkts > lossFrac {
				bad = true
			}
		}
		if bad {
			ml.lowStreak++
		} else {
			ml.lowStreak = 0
		}
		if ml.lowStreak >= degradeStreak {
			ml.degraded = true
			m.log("mtcp: link %d degraded (up-loss +%d/%dKB, down-loss +%d/%dKB, rtt %dms) — draining",
				ml.id, dUp, dWr>>10, dDown, dRd>>10, ml.mtr.rttMicros.Load()/1000)
		}
	}
	m.aggGoodput = agg
	// The pool is saturated (worth probing bigger) when there is user load and
	// the links are actually pushing data or straining against a cap. With no
	// load it is never saturated, so an idle tunnel is free to shrink.
	m.poolSaturated = m.users.Load() > 0 && (activeLinks > 0 || pressLinks > 0)
}

// linkRetransSource reads the link's retransmit counter (0,false if the link has
// no metered source). Split out so tests can drive it.
func (ml *managedLink) linkRetransSource() (uint64, bool) { return linkRetransOf(ml.link) }

// heal performs make-before-break on degraded links: it dials a replacement for
// each newly-degraded link FIRST (so capacity never dips), then drops any
// draining link that has emptied or overstayed maxDrain. Existing users on a
// draining link are never yanked — they finish or reconnect onto a fresh link.
func (m *LinkManager) heal(ctx context.Context) {
	m.mu.Lock()
	now := time.Now()
	newDrain := 0
	for _, ml := range m.links {
		if ml.degraded && !ml.draining {
			ml.draining = true
			ml.drainSince = now
			newDrain++
		}
	}
	m.mu.Unlock()

	// Dial replacements before dropping anything (make-before-break).
	for i := 0; i < newDrain; i++ {
		m.addReplacement(ctx)
	}

	m.mu.Lock()
	kept := m.links[:0]
	dropped := 0
	for _, ml := range m.links {
		if ml.draining && (ml.users.Load() == 0 || now.Sub(ml.drainSince) > maxDrain) {
			ml.link.Close()
			dropped++
			continue
		}
		kept = append(kept, ml)
	}
	m.links = kept
	m.mu.Unlock()
	if dropped > 0 {
		m.log("mtcp: retired %d drained link(s), now %d", dropped, m.count())
	}
}

func (m *LinkManager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.links)
}

// Target is the link count the autopilot currently wants; the reverse exit reads
// it over the pool-control channel to size its own dial pool.
func (m *LinkManager) Target() int { return int(m.target.Load()) }

// PoolStats is a snapshot of the pattern for the live monitor.
type PoolStats struct {
	Links     int     // live links now
	Target    int     // links the autopilot wants
	Min, Max  int     // envelope
	Users     int     // active user connections
	MbitPerS  float64 // aggregate goodput, Mbit/s
	Phase     string  // calibrating | probing | steady | shrinking | scaling
	Saturated bool    // links are pushing against a limit
}

// Stats returns a consistent snapshot of the pool for monitoring.
func (m *LinkManager) Stats() PoolStats {
	m.mu.RLock()
	links := 0
	for _, ml := range m.links {
		if ml.link.Alive() {
			links++
		}
	}
	m.mu.RUnlock()
	return PoolStats{
		Links:     links,
		Target:    int(m.target.Load()),
		Min:       m.min,
		Max:       m.max,
		Users:     int(m.users.Load()),
		MbitPerS:  mbitps(float64(m.lastAgg.Load())),
		Phase:     apPhase(m.phase.Load()).String(),
		Saturated: m.lastSat.Load(),
	}
}

func (m *LinkManager) closeAll() {
	m.closing.Store(true)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ml := range m.links {
		ml.link.Close()
	}
	m.links = nil
}
