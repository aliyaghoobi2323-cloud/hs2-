package engine

import (
	"context"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// LinkManager maintains a pool of parallel TLS links to the same kharej
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
// The pool is sized by the autopilot (autopilot.go) between Min and Max. Its
// target counts SERVING links — the ones that take new connections. Shrinking
// marks surplus links RETIRING: they take no new connections and are closed
// once the connections on them have ended, so shrinking never cuts a connection
// that is still in use. Growing brings a retiring link back into service before
// it dials (or, in reverse, before the exit dials) a new one. Links that die are
// rebuilt in the background.

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

// Sizing signal and actuator constants (the controller's own clocks are in
// apTunables).
const (
	// A link is "pressed" in a direction when its sender spent at least
	// pressBlocked of the time waiting for the network while moving at least
	// pressMinBytes per tick, and less than rwndShareMax of that wait was the
	// receiver's window (a slow receiver is not something more links fix).
	pressBlocked  = 0.5
	pressMinBytes = 16 << 10
	rwndShareMax  = 0.5
	// statsStale: an exit record older than this no longer counts as pressure.
	// statsGap: records further apart than this are not compared (re-baseline).
	statsStale = 6 * time.Second
	statsGap   = 7 * time.Second

	// drainIdleDefault: a connection on a RETIRING link that has not moved a
	// byte for this long is closed (FIN), so a retiring link held only by
	// forgotten idle connections can finish. It is above xray's default
	// connIdle (300 s), so normally the panel has already closed them.
	drainIdleDefault = 310 * time.Second
	idleReclaimMax   = 16 // idle connections closed per retiring link per tick
	maxClosesPerTick = 2  // empty retiring links closed per tick

	heldLogFirst = 15 * time.Minute // "retiring link held by N" first log
	heldLogEvery = time.Hour        // ... and then

	// Reverse churn guard: an exit whose own min_links is above the edge's
	// target redials every link the edge retires. churnTrips links arriving
	// surplus within churnReArrive of a retire-close, inside churnWindow, stop
	// retire-closes for churnHold. (A surplus link lives bornSpareGrace before
	// it can be closed, so one close/redial cycle takes ~30 s.)
	churnReArrive = 10 * time.Second
	churnWindow   = 3 * time.Minute // > churnTrips cycles of bornSpareGrace
	churnTrips    = 3
	churnHold     = 10 * time.Minute

	dialFailLogEvery = 30 * time.Second
	rwndHintEvery    = 10 * time.Minute
)

type LinkManager struct {
	dialer  LinkDialer
	accept  bool // reverse edge: links are injected via AddLink, never dialed
	min     int
	max     int
	perLink int // concurrently active flows per link the floor sizes for
	log     func(string, ...any)
	clock   func() time.Time // nil: time.Now (tests inject a fake clock)

	mu       sync.RWMutex
	links    []*managedLink
	linkSeq  int          // monotonic id source, so ids never collide after reaps
	users    atomic.Int32 // total open user connections across the pool
	closing  atomic.Bool
	scaleCtx context.Context

	// OnLink, if set, is called (in its own goroutine) for every new link.
	OnLink func(Link)

	// The sizing brain and what feeds it. ap, sample, dec, lastSampleAt,
	// targetDropAt and the log-throttle times are touched only by the pool's
	// own goroutine (Run/runAccept); target and stats are atomic so the
	// pool-control sender and the status reporter can read them.
	ap           *autopilot
	sample       apSample   // the last sampleHealth measurement
	dec          apDecision // the last decision
	lastSampleAt time.Time
	target       atomic.Int32 // SERVING links wanted (published to the reverse exit)
	pin          atomic.Int32 // > 0: overrides the autopilot (tests only)
	// targetDropAt is when the target last went down; the reverse edge closes a
	// retiring link only once the exit has had time to learn the lower target,
	// so it retires that slot instead of redialing it.
	targetDropAt time.Time
	stats        atomic.Pointer[PoolStats]
	exitStats    string
	dialFailAt   time.Time
	rwndHintAt   time.Time
	reclaimLogAt time.Time
	reclaimed    atomic.Int64 // idle connections closed on retiring links, not yet logged

	// drainIdle: idle-connection reclaim on retiring links, ns (0 = never).
	drainIdle atomic.Int64

	// Reverse close guards, under mu.
	retireCloses []time.Time // edge retire-closes in the churn window
	reArrivals   []time.Time // surplus arrivals shortly after a retire-close
	noCloseUntil time.Time   // churn guard: no retire-closes before this
	growable     bool        // last sample: the peer can add links
	shortLived   shortLivedLinks
	noPoolLogged bool
}

// rawStreamOpener is implemented by links that can open a stream which does
// not count as user load (e.g. the TUN side channel).
type rawStreamOpener interface {
	OpenRawStream() (*smux.Stream, error)
}

// idleReclaimer is implemented by links whose idle user streams can be closed.
type idleReclaimer interface {
	idleStreams(now time.Time, idle time.Duration, max int) []idleCand
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

	// Sampler state, written by the pool goroutine under LinkManager.mu (Pick
	// reads the routing fields under the same lock).
	mtr             *linkMeter // nil for links without metering (never soft-degrades)
	prevRd          uint64     // download byte counter at the last sample
	prevWr          uint64     // upload byte counter at the last sample
	prevRetrans     uint64     // local (upload) TCP retransmit counter at last sample
	prevPeerRetrans uint64     // peer (download) TCP retransmit counter at last sample
	prevBlocked     int64      // upload writer-blocked ns at the last sample
	prevBusy        uint64     // TCP_INFO busy time at the last sample
	prevRwnd        uint64     // TCP_INFO rwnd-limited time at the last sample
	goodput         float64    // EWMA bytes/sec, both directions (diagnostic)
	lowStreak       int        // consecutive high-loss samples (either direction)
	sampled         bool       // prev* are valid (skips the first delta)
	degraded        bool       // soft-bad: excluded from new-user routing
	draining        bool       // being retired after its replacement is up
	drainSince      time.Time

	// Sizing.
	retiring     bool      // takes no new users; closed once empty
	retireSince  time.Time //
	servingSince time.Time // when it (re)entered service — probe attribution
	upHist       uint8     // last 3 raw upload-pressure samples
	dnHist       uint8     // last 3 raw download-pressure samples (exit records)
	pressed      bool      // serving and its sender is blocked by the path
	suspect      bool      // nothing received for suspectAfter (see serving)
	rates        [5]float64
	doms         [3]float64
	nSamples     int
	rate         float64 // bytes/s this tick, both directions
	rate10       float64 // mean rate over the last 5 ticks
	sustained    float64 // min over 3 ticks of the dominant direction's rate
	flowing      int     // user streams really moving data
	open         int     // user streams open
	recent       int     // user streams that moved a byte within drainIdle
	lastByte     time.Time
	picks        int                 // connections placed since the last sample
	pickHist     [pickWindow - 1]int // ... and in the samples before
	lastRec      statsRec
	haveRec      bool
	lastRecAt    time.Time
	poolRefused  atomic.Bool // the exit refused kindPool on this link (older exit)
	bornSpare    bool        // reverse: arrived while the pool already had its target
	lastRx       time.Time   // when the link last received anything (keepalives included)
	rxSeen       bool        // it has received something at all
	reclaiming   atomic.Bool // an idle-reclaim goroutine is running for it
	heldLogAt    time.Time
}

// serving reports whether the link takes new users. Caller holds m.mu.
func (ml *managedLink) serving() bool {
	return !ml.retiring && !ml.degraded && !ml.draining && ml.link.Alive() && !ml.suspect
}

// bornSpareGrace: a reverse link that arrives while the pool already has its
// target may be the exit replacing a serving link that died without this side
// noticing yet (smux notices within its 24 s keepalive timeout). It is kept
// this long before it can be closed, so it can take over. (A variable only so
// the real-socket tests can shorten it.)
var bornSpareGrace = 30 * time.Second

// suspectAfter: a link that has received nothing at all — not even the
// other side's smux keepalive (every 4–8 s) or a control reply (every 3 s) —
// for this long has probably died on the far side first (a NAT rebinding in
// front of the exit, say); this side's TCP only gives up after 20 s or more.
// A suspect link takes no new users and does not count as serving, so the
// exit's replacement is used at once; it is serving again the moment
// anything arrives.
const suspectAfter = 12 * time.Second

// newManaged wraps a Link with a managed entry, capturing its meter (if any).
func (m *LinkManager) newManaged(l Link, id int, now time.Time) *managedLink {
	return &managedLink{link: l, id: id, born: now, servingSince: now, mtr: linkMeterOf(l)}
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
		ap: newAutopilot(min, max, perLink), growable: true}
	m.drainIdle.Store(int64(drainIdleDefault))
	m.target.Store(int32(warmSize(min, max)))
	return m
}

// SetDrainIdle sets how long a connection on a retiring link may sit idle
// before it is closed; 0 disables the reclaim.
func (m *LinkManager) SetDrainIdle(d time.Duration) {
	if d < 0 {
		d = 0
	}
	m.drainIdle.Store(int64(d))
}

func (m *LinkManager) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
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

// closeJitter spaces closes (50–250 ms) so a shrink is not a burst of FINs.
func closeJitter() time.Duration {
	return 50*time.Millisecond + time.Duration(rand.IntN(200))*time.Millisecond
}

// AddLink injects an externally acquired link into the pool. It is used by the
// reverse edge, which does not dial links but accepts them from the peer that
// dials in; the pool, load-balancing and reaping are otherwise identical. from
// names the peer for the log. It returns the link's id. A link that arrives
// while the pool already has its target of serving links is born retiring: the
// exit dialed more than the edge wants (a restart, or its own minimum).
func (m *LinkManager) AddLink(l Link, from string) int {
	now := m.now()
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	ml := m.newManaged(l, id, now)
	if S, _ := m.countsLocked(); S >= int(m.target.Load()) {
		ml.retiring, ml.retireSince, ml.bornSpare = true, now, true
	}
	spare := ml.retiring
	tripped := spare && m.noteSurplusArrivalLocked(now)
	m.links = append(m.links, ml)
	n := m.aliveLocked()
	S, _ := m.countsLocked()
	m.mu.Unlock()
	if spare {
		m.log("mtcp: reverse link %d up from %s (now %d) — spare: the pattern needs %d serving; it takes no connections and closes unless needed",
			id, from, n, S)
	} else {
		m.log("mtcp: reverse link %d up from %s (now %d)", id, from, n)
	}
	if tripped {
		m.log("mtcp: the exit redials links this server retires (check the exit's min_links) — keeping %d up for %s", n, fmtDur(churnHold))
	}
	if m.OnLink != nil {
		go m.OnLink(l)
	}
	return id
}

// noteSurplusArrivalLocked records a born-retiring arrival and reports whether
// it trips the churn guard. Caller holds m.mu.
func (m *LinkManager) noteSurplusArrivalLocked(now time.Time) bool {
	recent := false
	kept := m.retireCloses[:0]
	for _, c := range m.retireCloses {
		if now.Sub(c) <= churnWindow {
			kept = append(kept, c)
			if now.Sub(c) <= churnReArrive {
				recent = true
			}
		}
	}
	m.retireCloses = kept
	if !recent {
		return false
	}
	ra := m.reArrivals[:0]
	for _, a := range m.reArrivals {
		if now.Sub(a) <= churnWindow {
			ra = append(ra, a)
		}
	}
	m.reArrivals = append(ra, now)
	if len(m.reArrivals) >= churnTrips && !now.Before(m.noCloseUntil) {
		m.noCloseUntil = now.Add(churnHold)
		m.reArrivals = nil
		return true
	}
	return false
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
		return // already retired or reaped, or the whole pool is shutting down
	}
	m.log("mtcp: reverse link %d from %s down: %s (now %d)", id, from, linkDownReason(l), n)
}

// markPoolRefused records that the exit refused the pool-control stream on
// link l (an exit older than pool control).
func (m *LinkManager) markPoolRefused(l Link) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if ml.link == l {
			ml.poolRefused.Store(true)
		}
	}
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

// countsLocked returns the serving and retiring link counts. Caller holds m.mu.
func (m *LinkManager) countsLocked() (serving, retiring int) {
	for _, ml := range m.links {
		switch {
		case !ml.link.Alive() || ml.degraded || ml.draining || ml.suspect:
		case ml.retiring:
			retiring++
		default:
			serving++
		}
	}
	return
}

// linkDownReason asks a link why it stopped, when it can tell.
func linkDownReason(l Link) string {
	if r, ok := l.(interface{ downReason() string }); ok {
		return r.downReason()
	}
	return "closed"
}

// Run brings the pool up and then maintains it: rebuilds dead links, heals
// soft-bad ones and sizes the pool, until ctx ends. In accept mode (reverse
// edge) it never dials: it sizes the pool through the exit (runAccept).
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
	// toward what the traffic needs. This costs only a one-time startup ramp.
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
			m.reap()
			m.sampleHealth() // per-link throughput, pressure, loss, active flows
			m.heal(ctx)      // replace soft-bad links make-before-break
			m.autoscale(ctx) // decide the serving count and move the pool to it
			m.drainTick()    // close retiring links once their connections end
			m.publishStats()
		}
	}
}

// decideTarget runs one autopilot tick on the last sample, publishes the
// result for the reverse exit and the live monitor, logs the controller's note
// if it has one, and returns the number of serving links wanted. Both the
// direct edge (which dials to match) and the reverse edge (which sends it to
// the exit) size the pattern here, by exactly the same logic.
func (m *LinkManager) decideTarget() int {
	if p := int(m.pin.Load()); p > 0 {
		m.setTarget(p)
		m.dec = apDecision{target: p, phase: apSteady, reason: "pinned"}
		return p
	}
	s := m.sample
	if s.now.IsZero() {
		s = apSample{now: m.now(), growable: true}
	}
	d := m.ap.decide(s)
	m.setTarget(d.target)
	m.dec = d
	if d.note != "" {
		m.log("mtcp: %s", d.note)
	}
	return d.target
}

// setTarget publishes a new desired link count and remembers when it went down.
func (m *LinkManager) setTarget(n int) {
	if old := int(m.target.Swap(int32(n))); n < old {
		m.targetDropAt = m.now()
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

// autoscale sizes the DIRECT pool: decide the serving count, then move the
// pool to it.
func (m *LinkManager) autoscale(ctx context.Context) {
	m.reconcile(ctx, m.decideTarget())
}

// reconcile moves the pool to T serving links. Growing first brings retiring
// links back into service (the busiest first: they are already up, cost
// nothing, and their connections stay put), then — direct mode only — dials the
// rest, at most a quarter of T per tick. Shrinking marks the surplus retiring,
// choosing the links that will empty soonest (fewest active, then recently
// active, then open connections). Marking is free and reversible; a retiring
// link is closed by drainTick only once it is empty. In reverse the exit dials
// any shortfall itself when it learns the target.
func (m *LinkManager) reconcile(ctx context.Context, T int) {
	now := m.now()
	m.mu.Lock()
	var serving, retiring []*managedLink
	for _, ml := range m.links {
		switch {
		case !ml.link.Alive() || ml.degraded || ml.draining || ml.suspect:
		case ml.retiring:
			retiring = append(retiring, ml)
		default:
			serving = append(serving, ml)
		}
	}
	var back, gone []string
	if len(serving) < T && len(retiring) > 0 {
		sort.Slice(retiring, func(i, j int) bool {
			a, b := retiring[i], retiring[j]
			if a.open != b.open {
				return a.open > b.open
			}
			return a.lastByte.After(b.lastByte)
		})
		for len(serving) < T && len(retiring) > 0 {
			ml := retiring[0]
			retiring = retiring[1:]
			ml.retiring, ml.servingSince, ml.heldLogAt, ml.bornSpare = false, now, time.Time{}, false
			serving = append(serving, ml)
			back = append(back, fmt.Sprint(ml.id))
		}
	}
	if len(serving) > T {
		sort.Slice(serving, func(i, j int) bool {
			a, b := serving[i], serving[j]
			switch {
			case a.flowing != b.flowing:
				return a.flowing < b.flowing
			case a.recent != b.recent:
				return a.recent < b.recent
			case a.open != b.open:
				return a.open < b.open
			}
			return a.rate10 < b.rate10
		})
		for _, ml := range serving[:len(serving)-T] {
			ml.retiring, ml.retireSince, ml.pressed = true, now, false
			retiring = append(retiring, ml)
			gone = append(gone, fmt.Sprint(ml.id))
		}
		serving = serving[len(serving)-T:]
	}
	S, R, up := len(serving), len(retiring), m.aliveLocked()
	m.mu.Unlock()
	if len(back) > 0 {
		m.log("mtcp: link(s) %s back in service (%d up: %d serving, %d retiring)", strings.Join(back, ","), up, S, R)
	}
	if len(gone) > 0 {
		m.log("mtcp: link(s) %s retiring — no new connections; each closes once its connections end (%d up: %d serving, %d retiring)",
			strings.Join(gone, ","), up, S, R)
	}
	if m.accept || S >= T || m.dialer == nil {
		return
	}
	step := (T + 3) / 4
	if step < 1 {
		step = 1
	}
	n := T - S
	if n > step {
		n = step
	}
	dialed := 0
	var lastErr error
	for i := 0; i < n && m.count() < m.max; i++ {
		if i > 0 && !sleepCtx(ctx, jitterGap()) {
			return
		}
		if err := m.dialLink(ctx); err != nil {
			lastErr = err
			break
		}
		dialed++
	}
	if dialed > 0 {
		m.log("mtcp: dialed %d link(s) — %d up, %d of %d serving", dialed, up+dialed, S+dialed, T)
	}
	if lastErr != nil && now.Sub(m.dialFailAt) >= dialFailLogEvery {
		m.dialFailAt = now
		m.log("mtcp: want %d serving links, only %d up — dials failing (peer down or path blocked): %v", T, S+dialed, lastErr)
	}
}

// drainTick closes retiring links that have emptied and reclaims long-idle
// connections on retiring links. An empty link is taken out of the pool under
// the same lock Pick uses, so no new user can land on it, and closed outside
// the lock with a little jitter. In reverse the close also waits until the
// exit knows the lower target (so it retires the slot instead of redialing),
// until the link is old enough to have learned it, and never while the exit is
// found to redial what is retired or cannot take pool control at all.
func (m *LinkManager) drainTick() {
	now := m.now()
	drainIdle := time.Duration(m.drainIdle.Load())
	m.mu.Lock()
	guard := true
	if m.accept {
		ok := 0 // links whose pool control has had time to be refused and was not
		for _, ml := range m.links {
			if ml.link.Alive() && !ml.poolRefused.Load() && now.Sub(ml.born) >= retireAfterDrop {
				ok++
			}
		}
		guard = now.Sub(m.targetDropAt) >= retireAfterDrop && ok > 0 && !now.Before(m.noCloseUntil)
	}
	var closing, reclaim []*managedLink
	var held []string
	kept := m.links[:0]
	for _, ml := range m.links {
		if !ml.retiring || !ml.link.Alive() {
			kept = append(kept, ml)
			continue
		}
		if guard && len(closing) < maxClosesPerTick && ml.users.Load() == 0 && ml.link.Active() == 0 &&
			(!m.accept || now.Sub(ml.born) >= retireAfterDrop) &&
			(!ml.bornSpare || now.Sub(ml.born) >= bornSpareGrace) {
			closing = append(closing, ml)
			continue
		}
		kept = append(kept, ml)
		if drainIdle > 0 && ml.open > 0 && ml.reclaiming.CompareAndSwap(false, true) {
			reclaim = append(reclaim, ml)
		}
		if age := now.Sub(ml.retireSince); age >= heldLogFirst && (ml.heldLogAt.IsZero() || now.Sub(ml.heldLogAt) >= heldLogEvery) {
			ml.heldLogAt = now
			if n := max(ml.open, int(ml.users.Load())); n > 0 {
				held = append(held, fmt.Sprintf("link %d retiring %s: held by %d open connection(s), %d active", ml.id, fmtDur(age), n, ml.flowing))
			} else {
				held = append(held, fmt.Sprintf("link %d retiring %s: empty but kept up — the exit would redial it (no pool control, or its min_links is above this server's target)", ml.id, fmtDur(age)))
			}
		}
	}
	m.links = kept
	if m.accept {
		kept := m.retireCloses[:0]
		for _, c := range m.retireCloses {
			if now.Sub(c) <= churnWindow {
				kept = append(kept, c)
			}
		}
		m.retireCloses = kept
		for range closing {
			m.retireCloses = append(m.retireCloses, now)
		}
	}
	up := m.aliveLocked()
	S, R := m.countsLocked()
	m.mu.Unlock()

	what := "link"
	if m.accept {
		what = "reverse link"
	}
	for i, ml := range closing {
		left := len(closing) - i - 1 // closed in this batch after this one
		m.log("mtcp: %s %d retired: its connections ended (%s after retiring) — now %d up (%d serving, %d retiring)",
			what, ml.id, fmtDur(now.Sub(ml.retireSince)), up+left, S, R+left)
		go func(l Link) {
			time.Sleep(closeJitter())
			l.Close() // DropLink then finds nothing and stays quiet
		}(ml.link)
	}
	for _, ml := range reclaim {
		go m.reclaimIdle(ml, now, drainIdle)
	}
	for _, h := range held {
		m.log("mtcp: %s", h)
	}
	if n := m.reclaimed.Load(); n > 0 && now.Sub(m.reclaimLogAt) >= time.Minute {
		m.reclaimed.Add(-n)
		m.reclaimLogAt = now
		m.log("mtcp: closed %d connection(s) idle for over %s on retiring links", n, fmtDur(drainIdle))
	}
}

// reclaimIdle closes, with FIN, the connections on a retiring link that have
// not moved a byte for drainIdle. A connection that moves anything between
// being chosen and being closed is spared. Closing a stream can block, so this
// runs in its own goroutine, one per link at a time.
func (m *LinkManager) reclaimIdle(ml *managedLink, now time.Time, idle time.Duration) {
	defer ml.reclaiming.Store(false)
	ir, ok := ml.link.(idleReclaimer)
	if !ok {
		return
	}
	for i, c := range ir.idleStreams(now, idle, idleReclaimMax) {
		if i > 0 {
			time.Sleep(closeJitter())
		}
		if c.cs.bytes.Load() != c.snap {
			continue
		}
		c.cs.Close()
		m.reclaimed.Add(1)
	}
}

// runAccept maintains the reverse-edge pool. It never dials: it runs the same
// autopilot and publishes the serving count for the exit (which dials) over the
// pool-control channel (exit_pool.go / openPoolCtl), retires surplus links
// exactly like the direct pool, and routes users away from soft-bad links and
// drops them — the exit then redials a fresh one to keep its count.
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
			m.reconcile(ctx, m.decideTarget())
			m.drainTick()
			m.sweepReverse()
			m.publishStats()
		}
	}
}

// sweepReverse drops dead links and drains degraded ones on the reverse edge.
func (m *LinkManager) sweepReverse() {
	now := m.now()
	m.mu.Lock()
	var logs []string
	var closing []Link
	alive := m.links[:0]
	for _, ml := range m.links {
		if !ml.link.Alive() {
			closing = append(closing, ml.link)
			logs = append(logs, fmt.Sprintf("reverse link %d down: %s", ml.id, linkDownReason(ml.link)))
			continue
		}
		if ml.degraded && !ml.draining {
			ml.draining = true
			ml.drainSince = now
			logs = append(logs, fmt.Sprintf("reverse link %d degraded — draining (peer will redial)", ml.id))
		}
		if ml.draining && (ml.users.Load() == 0 || now.Sub(ml.drainSince) > maxDrain) {
			closing = append(closing, ml.link) // the exit redials to restore its count
			continue
		}
		alive = append(alive, ml)
	}
	m.links = alive
	m.mu.Unlock()
	for _, l := range closing {
		l.Close()
	}
	for _, s := range logs {
		m.log("mtcp: %s", s)
	}
}

// addLink dials one link for the initial fill.
func (m *LinkManager) addLink(ctx context.Context) {
	if m.count() >= m.max {
		return
	}
	if err := m.dialLink(ctx); err != nil {
		m.log("mtcp: link dial failed: %v", err)
	}
}

// dialLink dials one serving link and adds it to the pool.
func (m *LinkManager) dialLink(ctx context.Context) error {
	l, err := m.dialer.DialLink(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.links = append(m.links, m.newManaged(l, id, m.now()))
	m.mu.Unlock()
	if m.OnLink != nil {
		go m.OnLink(l)
	}
	return nil
}

// addReplacement dials a fresh link for make-before-break healing. Unlike
// reconcile it bypasses the max cap: it is a temporary over-provision so a
// degraded link's users keep full capacity until they drain onto the new link,
// after which heal drops the degraded one and the pool returns to size.
func (m *LinkManager) addReplacement(ctx context.Context) {
	l, err := m.dialer.DialLink(ctx)
	if err != nil {
		m.log("mtcp: replacement dial failed: %v", err)
		return
	}
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.links = append(m.links, m.newManaged(l, id, m.now()))
	m.mu.Unlock()
	if m.OnLink != nil {
		go m.OnLink(l)
	}
	m.log("mtcp: dialed replacement link %d (make-before-break)", id)
}

// reap takes links that died out of the pool and logs each with its reason.
// The pool is refilled to its serving target by reconcile in the same tick
// (a retiring link is brought back first), so idle connections no longer
// decide how many links are redialed.
func (m *LinkManager) reap() {
	m.mu.Lock()
	alive := m.links[:0]
	var dead []*managedLink
	for _, ml := range m.links {
		if ml.link.Alive() {
			alive = append(alive, ml)
		} else {
			dead = append(dead, ml)
		}
	}
	m.links = alive
	m.mu.Unlock()
	now := m.now()
	for _, ml := range dead {
		ml.link.Close()
		m.log("mtcp: link %d down: %s", ml.id, linkDownReason(ml.link))
		if hint := m.shortLived.note(now.Sub(ml.born)); hint != "" {
			m.log("mtcp: %s", hint)
		}
	}
}

// shortLivedLinks notices a path that lets a TCP link come up and then kills
// it (the Iran border does this to TCP after ~10 KB): several links in a row
// dead within shortLinkLife. It says so once, with the way out, instead of an
// endless column of "link down: timed out".
type shortLivedLinks struct {
	run    int
	hinted bool
}

const (
	shortLinkLife = 20 * time.Second
	shortLinkRun  = 3
)

// note takes one dead link's lifetime and returns the hint when it is due.
func (s *shortLivedLinks) note(life time.Duration) string {
	if life >= shortLinkLife {
		s.run, s.hinted = 0, false
		return ""
	}
	s.run++
	if s.run < shortLinkRun || s.hinted {
		return ""
	}
	s.hinted = true
	return fmt.Sprintf("%d links in a row died within %s of coming up — this path lets TCP start and then kills it (common on the Iran border after ~10 KB); the tun over icmp transport (tun → icmp) does not use TCP on the wire", s.run, shortLinkLife)
}

// pickKey orders candidate links for a new user connection; smaller is better.
// An unpressed link comes first; then the one with the fewest flows actually
// moving data (counting connections placed since the last sample, so a burst
// in one tick still spreads); then the fewest open connections, which spreads
// idle ones and limits how many one link's death resets. A fresh link has key
// zero and takes the next flows. The simulator uses the same ordering.
type pickKey struct {
	pressed bool
	load    int
	users   int
}

func (a pickKey) less(b pickKey) bool {
	if a.pressed != b.pressed {
		return !a.pressed
	}
	if a.load != b.load {
		return a.load < b.load
	}
	return a.users < b.users
}

// newPickKey builds a link's key. Pressure is measured a few seconds late
// (2 of 3 samples), so an unpressed link that has already taken half of
// perLink new connections within that window counts as pressed until its
// measurement catches up: a burst spreads over every link instead of piling
// onto the few that looked free. recentPicks covers the last pickWindow
// samples, picks only the current one.
func newPickKey(pressed bool, flowing, picks, recentPicks, users, perLink int) pickKey {
	return pickKey{pressed: pressed || recentPicks >= max(1, perLink/2), load: flowing + picks, users: users}
}

// pickWindow is how many samples of placements count toward the burst cap.
const pickWindow = 3

func (ml *managedLink) recentPicks() int {
	n := ml.picks
	for _, p := range ml.pickHist {
		n += p
	}
	return n
}

// Pick returns the best link for a NEW user connection, and a release func to
// call when that user disconnects. Choosing and counting happen under one
// lock, so N connections arriving together land on N different links. Ties
// are broken at random so load does not pile onto the oldest link.
func (m *LinkManager) Pick() (Link, func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chosen := m.pickLocked()
	if chosen == nil {
		return nil, func() {}, false
	}
	chosen.users.Add(1)
	chosen.picks++
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

// pickLocked chooses among serving links first; if there are none, a healthy
// retiring link (better than refusing the user); and only if every link is
// degraded or draining, one of those. Caller holds m.mu.
func (m *LinkManager) pickLocked() *managedLink {
	for tier := 0; tier < 3; tier++ {
		var chosen *managedLink
		var best pickKey
		ties := 0
		for _, ml := range m.links {
			if !ml.link.Alive() {
				continue
			}
			switch tier {
			case 0:
				if ml.retiring || ml.degraded || ml.draining || ml.suspect {
					continue
				}
			case 1:
				if ml.degraded || ml.draining || ml.suspect {
					continue
				}
			}
			k := newPickKey(ml.pressed, ml.flowing, ml.picks, ml.recentPicks(), int(ml.users.Load()), m.perLink)
			switch {
			case chosen == nil || k.less(best):
				chosen, best, ties = ml, k, 1
			case !best.less(k):
				ties++
				if rand.IntN(ties) == 0 {
					chosen = ml
				}
			}
		}
		if chosen != nil {
			return chosen
		}
	}
	return nil
}

// linkObs is what sampleHealth reads from one link outside the pool lock.
type linkObs struct {
	alive      bool
	rd, wr     uint64
	blocked    int64
	ts         tcpStat
	tsOK       bool
	peerRT     uint64
	peerSeen   bool
	rec        *statsRec
	statsState int32
	fs         flowSnap
}

// sampleHealth measures every link once per tick and builds the autopilot's
// sample:
//
//   - throughput per link and in total;
//   - active flows (per-stream rate EWMA) and open connections;
//   - pressure: whether the link's sender is blocked by the path — upload from
//     this side's own writer and TCP_INFO, download from the exit's records
//     (kindStats), each sticky over 2 of 3 samples;
//   - loss: a link actively moving data and retransmitting more than lossFrac
//     of its packets for degradeStreak samples is flagged degraded. The
//     activity gate (activeBytes) keeps an idle link from being condemned.
//
// Socket reads and per-stream bookkeeping happen outside the pool lock; the
// results are applied under it.
func (m *LinkManager) sampleHealth() {
	now := m.now()
	dt := healthTick
	if !m.lastSampleAt.IsZero() {
		if d := now.Sub(m.lastSampleAt); d > time.Millisecond {
			dt = d
		} else {
			dt = time.Millisecond
		}
	}
	m.lastSampleAt = now
	recentWin := time.Duration(m.drainIdle.Load())
	if recentWin <= 0 {
		recentWin = drainIdleDefault
	}

	m.mu.RLock()
	snap := append([]*managedLink(nil), m.links...)
	m.mu.RUnlock()
	obs := make(map[*managedLink]*linkObs, len(snap))
	for _, ml := range snap {
		o := &linkObs{alive: ml.link.Alive()}
		obs[ml] = o
		if !o.alive {
			continue
		}
		if fs, ok := ml.link.(flowSource); ok {
			o.fs = fs.flowStats(now, dt, recentWin)
		} else {
			o.fs.open = int(ml.link.Active())
		}
		if ml.mtr != nil {
			o.rd, o.wr, o.blocked = ml.mtr.rdBytes.Load(), ml.mtr.wrBytes.Load(), ml.mtr.wrBlocked.Load()
			o.ts, o.tsOK = linkTCPStatsOf(ml.link)
			o.peerRT, o.peerSeen = ml.mtr.peerRetrans.Load(), ml.mtr.peerSeen.Load()
			o.rec = ml.mtr.peer.Load()
			o.statsState = ml.mtr.statsState.Load()
		}
	}

	var logs []string
	rwndHint := false
	secs := dt.Seconds()
	perTick := func(b uint64) float64 { return float64(b) * healthTick.Seconds() / secs }
	s := apSample{now: now, open: int(m.users.Load())}
	nOK, nOld, poolOK, aged := 0, 0, 0, 0

	m.mu.Lock()
	for _, ml := range m.links {
		o := obs[ml]
		if o == nil { // arrived after the snapshot: counted, measured next tick
			if ml.link.Alive() {
				s.links = append(s.links, apLink{id: ml.id, serving: ml.serving(), retiring: ml.retiring, servingSince: ml.servingSince})
			}
			continue
		}
		if !o.alive {
			continue
		}
		copy(ml.pickHist[1:], ml.pickHist[:len(ml.pickHist)-1])
		ml.pickHist[0], ml.picks = ml.picks, 0
		ml.flowing, ml.open, ml.recent = o.fs.flowing, o.fs.open, o.fs.recent
		if o.fs.last.After(ml.lastByte) {
			ml.lastByte = o.fs.last
		}
		if now.Sub(ml.born) >= retireAfterDrop { // old enough to have been refused
			aged++
			if !ml.poolRefused.Load() {
				poolOK++
			}
		}
		if ml.mtr != nil {
			switch o.statsState {
			case statsOK:
				nOK++
			case statsUnsupported:
				nOld++
			}
		}
		if ml.mtr != nil {
			if o.rd != ml.prevRd || !ml.sampled && o.rd > 0 {
				ml.lastRx, ml.rxSeen = now, true
			}
			wasSuspect := ml.suspect
			ml.suspect = ml.rxSeen && now.Sub(ml.lastRx) >= suspectAfter
			if ml.suspect && !wasSuspect {
				logs = append(logs, fmt.Sprintf("link %d: nothing received for %s — not used for new connections until it answers", ml.id, fmtDur(now.Sub(ml.lastRx))))
			}
		}
		if ml.mtr == nil || !ml.sampled {
			if ml.mtr != nil {
				ml.prevRd, ml.prevWr, ml.prevBlocked = o.rd, o.wr, o.blocked
				ml.prevRetrans, ml.prevBusy, ml.prevRwnd = o.ts.retrans, o.ts.busyUs, o.ts.rwndUs
				ml.prevPeerRetrans, ml.sampled = o.peerRT, true
			}
			ml.pressed = false
			s.links = append(s.links, ml.apLink())
			s.flowing += ml.flowing
			continue
		}
		dRd := o.rd - ml.prevRd
		dWr := o.wr - ml.prevWr
		dUp := o.ts.retrans - ml.prevRetrans
		dDown := o.peerRT - ml.prevPeerRetrans
		dBlocked := o.blocked - ml.prevBlocked
		upRwnd := 0.0
		if o.tsOK && o.ts.chronoValid && o.ts.busyUs > ml.prevBusy && o.ts.rwndUs >= ml.prevRwnd {
			upRwnd = float64(o.ts.rwndUs-ml.prevRwnd) / float64(o.ts.busyUs-ml.prevBusy)
		}
		ml.prevRd, ml.prevWr, ml.prevBlocked = o.rd, o.wr, o.blocked
		ml.prevRetrans, ml.prevPeerRetrans = o.ts.retrans, o.peerRT
		ml.prevBusy, ml.prevRwnd = o.ts.busyUs, o.ts.rwndUs

		// Throughput.
		ml.rate = float64(dRd+dWr) / secs
		dom := float64(max(dRd, dWr)) / secs
		ml.rates[ml.nSamples%len(ml.rates)] = ml.rate
		ml.doms[ml.nSamples%len(ml.doms)] = dom
		ml.nSamples++
		ml.rate10 = mean(ml.rates[:min(ml.nSamples, len(ml.rates))])
		ml.sustained = 0
		if ml.nSamples >= len(ml.doms) {
			ml.sustained = ml.doms[0]
			for _, d := range ml.doms[1:] {
				ml.sustained = min(ml.sustained, d)
			}
		}
		if ml.goodput == 0 {
			ml.goodput = ml.rate
		} else {
			ml.goodput = gpAlpha*ml.rate + (1-gpAlpha)*ml.goodput
		}

		// Upload pressure: this side's writer waited for the network.
		rawUp := perTick(dWr) >= pressMinBytes && float64(dBlocked)/float64(dt) >= pressBlocked && upRwnd < rwndShareMax
		ml.upHist = (ml.upHist<<1 | b2u(rawUp)) & 7
		// Download pressure: the exit's writer waited for the network.
		if ml.consumeRecord(o.rec, now) {
			rwndHint = true
		}
		up := bits.OnesCount8(ml.upHist) >= 2
		dn := o.statsState == statsOK && bits.OnesCount8(ml.dnHist) >= 2 && now.Sub(ml.lastRecAt) <= statsStale
		ml.pressed = ml.serving() && (up || dn)
		// Ask the exit for a fresh record only while the link moves data, so
		// an idle link carries no extra periodic beat.
		if perTick(dRd+dWr) >= pressMinBytes {
			pollStats(ml.mtr)
		}

		// Loss. A direction is "bad" when it is actively moving data and
		// retransmitting more than lossFrac of its packets. Upload uses local
		// TCP_INFO; download uses the exit's retransmits from the control channel,
		// so a link bad only on the download path is caught too.
		if !ml.degraded && !ml.draining {
			bad := false
			if o.tsOK && dWr >= activeBytes {
				if pkts := float64(dWr) / mss; pkts > 0 && float64(dUp)/pkts > lossFrac {
					bad = true
				}
			}
			if o.peerSeen && dRd >= activeBytes {
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
				ml.degraded, ml.pressed = true, false
				logs = append(logs, fmt.Sprintf("link %d degraded (up-loss +%d/%dKB, down-loss +%d/%dKB, rtt %dms) — draining",
					ml.id, dUp, dWr>>10, dDown, dRd>>10, ml.mtr.rttMicros.Load()/1000))
			}
		}
		s.G += ml.rate
		s.flowing += ml.flowing
		s.links = append(s.links, ml.apLink())
	}
	// Growable unless every link old enough to have been refused pool control
	// was refused (a pre-pool-control exit); while all links are fresh, as at
	// start, assume it can.
	s.growable = !m.accept || aged == 0 || poolOK > 0
	noPool := m.growable && !s.growable && !m.noPoolLogged
	if noPool {
		m.noPoolLogged = true
	}
	m.growable = s.growable
	m.mu.Unlock()
	m.sample = s

	switch {
	case nOK+nOld == 0:
		m.exitStats = ""
	case nOld == 0:
		m.exitStats = "ok"
	case nOK == 0:
		m.exitStats = "older exit: download pressure unknown"
	default:
		m.exitStats = "partial"
	}
	for _, l := range logs {
		m.log("mtcp: %s", l)
	}
	if noPool {
		m.log("mtcp: the exit has no pool control (older hs2) — the link count is fixed by its rev_links until it is upgraded")
	}
	if rwndHint && now.Sub(m.rwndHintAt) >= rwndHintEvery {
		m.rwndHintAt = now
		m.log("mtcp: downloads are limited by this server's receive side (a slow reader or small tcp_rmem), not the path — more links would not help")
	}
}

// consumeRecord turns a fresh exit record into one download-pressure sample:
// over the time between it and the previous record, did the exit's writer wait
// for the network while moving real data, and not because this side's receive
// window was full? A gap longer than statsGap re-baselines. It reports whether
// the sample would have been pressure but for the receive window (a tuning
// hint). Caller holds m.mu.
func (ml *managedLink) consumeRecord(rec *statsRec, now time.Time) (rwndHint bool) {
	if rec == nil || (ml.haveRec && rec.seq == ml.lastRec.seq) {
		return false
	}
	p, had := ml.lastRec, ml.haveRec
	ml.lastRec, ml.haveRec = *rec, true
	if !had {
		return false
	}
	if rec.mono <= p.mono || rec.tx < p.tx || rec.txBlocked < p.txBlocked {
		ml.dnHist = 0
		return false
	}
	dt := time.Duration(rec.mono - p.mono)
	if dt > statsGap {
		ml.dnHist = 0
		return false
	}
	perTick := float64(rec.tx-p.tx) * healthTick.Seconds() / dt.Seconds()
	blk := float64(rec.txBlocked-p.txBlocked) / float64(dt)
	rwnd := 0.0
	if rec.chrono() && p.chrono() && rec.busy > p.busy && rec.rwnd >= p.rwnd {
		rwnd = float64(rec.rwnd-p.rwnd) / float64(rec.busy-p.busy)
	}
	limited := perTick >= pressMinBytes && blk >= pressBlocked
	raw := limited && rwnd < rwndShareMax
	ml.dnHist = (ml.dnHist<<1 | b2u(raw)) & 7
	ml.lastRecAt = rec.at
	if ml.lastRecAt.IsZero() {
		ml.lastRecAt = now
	}
	return limited && !raw
}

func (ml *managedLink) apLink() apLink {
	return apLink{
		id: ml.id, serving: ml.serving(), retiring: ml.retiring && ml.link.Alive(),
		servingSince: ml.servingSince, pressed: ml.pressed,
		rate: ml.rate, rate10: ml.rate10, sustained: ml.sustained,
		flowing: ml.flowing, open: ml.open,
	}
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// heal performs make-before-break on degraded links: for each newly-degraded
// serving link it first brings a retiring link back into service, and only if
// there is none dials a replacement — so capacity never dips — then drops any
// draining link that has emptied or overstayed maxDrain. Existing users on a
// draining link are never yanked — they finish or reconnect onto a fresh link.
func (m *LinkManager) heal(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	need := 0
	for _, ml := range m.links {
		if ml.degraded && !ml.draining {
			if !ml.retiring {
				need++
			}
			ml.draining, ml.retiring, ml.drainSince = true, false, now
		}
	}
	var back []string
	for need > 0 {
		var best *managedLink
		for _, ml := range m.links {
			if ml.retiring && ml.link.Alive() && !ml.degraded && (best == nil || ml.open > best.open) {
				best = ml
			}
		}
		if best == nil {
			break
		}
		best.retiring, best.servingSince, best.heldLogAt = false, now, time.Time{}
		back = append(back, fmt.Sprint(best.id))
		need--
	}
	m.mu.Unlock()
	if len(back) > 0 {
		m.log("mtcp: link(s) %s back in service to replace a degraded link", strings.Join(back, ","))
	}
	// Dial replacements before dropping anything (make-before-break).
	for i := 0; i < need && m.dialer != nil; i++ {
		m.addReplacement(ctx)
	}

	m.mu.Lock()
	kept := m.links[:0]
	var drop []Link
	for _, ml := range m.links {
		if ml.draining && (ml.users.Load() == 0 || now.Sub(ml.drainSince) > maxDrain) {
			drop = append(drop, ml.link)
			continue
		}
		kept = append(kept, ml)
	}
	m.links = kept
	n := len(m.links)
	m.mu.Unlock()
	for _, l := range drop {
		l.Close()
	}
	if len(drop) > 0 {
		m.log("mtcp: retired %d drained link(s), now %d", len(drop), n)
	}
}

func (m *LinkManager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.links)
}

// Target is the number of serving links the autopilot currently wants; the
// reverse exit reads it over the pool-control channel to size its dial pool.
func (m *LinkManager) Target() int { return int(m.target.Load()) }

// PoolStats is a snapshot of the pattern for the live monitor. Fields are only
// ever added, so older readers keep working.
type PoolStats struct {
	Links      int     // live links now (serving + retiring + draining)
	Target     int     // serving links the autopilot wants
	Min, Max   int     // envelope
	Users      int     // open user connections
	MbitPerS   float64 // aggregate throughput, Mbit/s
	Phase      string  // steady | scaling | probing | holding | shrinking (exit: following | listening)
	Saturated  bool    // some serving link is at its limit (= Pressed > 0)
	Serving    int     // links taking new connections
	Retiring   int     // links closing once their connections end
	HeldBy     int     // open connections still on retiring links
	HeldActive int     // ... of which actively moving data
	Flowing    int     // connections actively moving data
	Pressed    int     // serving links at their limit
	CapMbit    float64 // measured per-link limit, Mbit/s (0 = none seen)
	PeakMbit   float64 // last minute's peak throughput, Mbit/s
	Reason     string  // why the pattern is this size, with the numbers
	NextProbeS int     // seconds until growth is tried again (holding), else 0
	ExitStats  string  // "ok" | "partial" | "older exit: ..." | "" (unknown yet)

	// Datagram pools (dgtun): what this side's carriers see on what they SEND
	// (the peer reports its loss) and what they receive.
	Datagram      bool
	LossPct       float64 // wire loss of what this side sends, pool-wide (rate-weighted)
	MaxLossPct    float64 // the worst active carrier's
	ParityPct     float64 // FEC parity per data byte, mean over active carriers
	FECAtCeiling  int     // active carriers whose parity is at its maximum
	FECRecovered  uint64  // received data rebuilt from parity (live carriers)
	FECLost       uint64  // received data lost for good (live carriers)
	PacerDropped  uint64  // datagrams the carriers' pacers dropped (live carriers)
	RxDropped     uint64  // received datagrams dropped for a full carrier queue (live carriers)
	TunDrops      uint64  // tunnel packets dropped for a full carrier queue (since start)
	TunRead       uint64  // packets read from the tun (to send)
	SentPkts      uint64  // ... handed to a carrier and sent
	RecvPkts      uint64  // packets received from carriers
	TunWritten    uint64  // ... written to the tun
	DropNoCarrier uint64  // tun packets dropped: no live carrier
	DropQueueFull uint64  // tun packets dropped: the carrier's queue was full
	DropAged      uint64  // tun packets dropped: waited > 50 ms in a carrier's queue
	Carriers      string  // per carrier: id:state:sent/loss%
	// TCP reorder buffer before the TUN (live carriers): segments held behind a
	// gap, gaps that filled while held, and gaps given up after the hold.
	ReorderHeld, ReorderFilled, ReorderTimedOut uint64
	Policed                                     bool    // the pool is held under a policer cap (being tested or confirmed)
	PoliceConfirm                               bool    // the cap stretched the loss episodes: a confirmed policer
	PoliceCapMbit                               float64 // that cap, Mbit/s (wire: data + parity)
}

// publishStats stores the monitor snapshot at the end of a pool tick.
func (m *LinkManager) publishStats() {
	now := m.now()
	st := PoolStats{
		Min: m.min, Max: m.max,
		MbitPerS: mbitps(m.sample.G), Phase: m.dec.phase.String(), Reason: m.dec.reason,
		Flowing: m.sample.flowing, CapMbit: mbitps(m.ap.cCap), PeakMbit: mbitps(m.ap.gPeak),
		ExitStats: m.exitStats,
	}
	if m.pin.Load() == 0 && m.ap.next.After(now) && m.dec.phase == apHolding {
		st.NextProbeS = int(m.ap.next.Sub(now).Seconds() + 0.5)
	}
	m.stats.Store(&st)
}

// Stats returns a snapshot of the pool for monitoring: the counts are read
// live, the measurements come from the last pool tick.
func (m *LinkManager) Stats() PoolStats {
	st := PoolStats{Min: m.min, Max: m.max, Phase: apSteady.String(),
		Reason: fmt.Sprintf("starting — bringing up the first %d links", warmSize(m.min, m.max))}
	if p := m.stats.Load(); p != nil {
		st = *p
	}
	m.mu.RLock()
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		st.Links++
		switch {
		case ml.degraded || ml.draining:
		case ml.retiring:
			st.Retiring++
			st.HeldBy += ml.open
			st.HeldActive += ml.flowing
		default:
			st.Serving++
			if ml.pressed {
				st.Pressed++
			}
		}
	}
	m.mu.RUnlock()
	st.Target = int(m.target.Load())
	st.Users = int(m.users.Load())
	st.Saturated = st.Pressed > 0
	return st
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
