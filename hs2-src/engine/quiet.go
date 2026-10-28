package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// R1 round 2, candidate β2 (docs/architecture/wip/r1/decisions.fa.md T16 to
// T18; REDESIGN §3.5 C5): QUIET links at the pool level.
//
// The round-2 diagnosis measured that on one TCP connection with BBRv1 a
// light flow cannot stay within RTT_floor + 20 ms next to bulk (PROBE_RTT
// stalls the link's writer every 10 s; behind fq_codel nearly every probe
// waits behind a resent hole), while on a TCP connection of its own it does.
// So the edge's pool keeps a few QUIET links (QuietCount: 1, or 2 for
// redundancy, whatever the pool's size) next to its BULK ones:
//
//   - every user connection opens on a QUIET link as a group stream (smux
//     group.go: its frames numbered), and so do the TUN's new flows;
//   - a connection that shows bulk behaviour in either direction — at least
//     moveBytes over 2 s and a send backlog for moveRun (§3.5) — moves once
//     to a BULK link the pool's placement picks (pickLocked), with the
//     group's move stream: the receiver keeps the bytes in order across the
//     two links, the move stream's window bounding the reorder;
//   - the links' control streams and the latency lane of every user stay on
//     QUIET; BULK links carry what moved (and their own control).
//
// QUIET links count toward max_links: a pool of max_links N runs
// max(1, N - QuietCount(N)) BULK links, so N = 1 runs one QUIET and one BULK
// link (two TCP connections), N = 8 two QUIET and six BULK.
//
// The link's class rides its caps (tlscarrier.CapQuiet, level 2): the edge
// offers it on a link it wants QUIET (dialing, or as the reverse server in
// its proof), the exit on every link, so both ends know each link's class
// from its handshake. With a level 1 peer no link has CapGroup: the edge
// keeps no QUIET link and everything runs as before. HS2_TUNE_QUIET=0 does
// the same (and keeps this side at level 1's caps).

// TuneQuiet 0 (HS2_TUNE_QUIET=0) turns β2 off: no QUIET links, no group
// streams, C4-lite on every link (the caps lose CapGroup too).
var TuneQuiet = 1

// QuietOff says whether β2 is off.
func QuietOff() bool { return TuneQuiet == 0 }

// TuneEarlyDone 0 (HS2_TUNE_EARLYDONE=0): a stream's writer waits for each
// frame to be written before it queues the next (the P10 (b) fix off).
var TuneEarlyDone = 1

// EarlyDoneOff says whether the (b) fix is off.
func EarlyDoneOff() bool { return TuneEarlyDone == 0 }

// QuietCC is the congestion control of a QUIET link's sockets, at both
// ends (HS2_TUNE_QUIET_CC; "": the links' own, BBR): its traffic is light,
// and BBR's PROBE_RTT, its window at 4 segments for 200 ms each 10 s, held
// the probes of a QUIET link with 198 probe users (each a padded record
// each 200 ms) for hundreds of ms on the rig.
var QuietCC = "cubic"

// QuietRec 1 (HS2_TUNE_QUIET_REC=1, lab; the R1 round-2 judge): a QUIET
// link's records follow an interactive HTTPS connection's sizes
// (obfs.NewInteractiveLengthSampler), not bulk HTTPS's: each small message
// is padded to ~390 B on average instead of ~990 B. The other end decodes
// any size, so each end's switch is its own sending's.
var QuietRec = 0

// QuietCount is how many QUIET links a pool of maxLinks keeps: QuietN when
// set; with QuietExtra (the default) one up to quietOneMax links (decision
// T19: max_links + 1 connections, within the population of dpi_eval2's
// model, 1 to 6), two past it; within max_links one, and a second for
// redundancy once the pool may have four links or more.
//
// Why two past quietOneMax (γ2's measured need, R1 round 3 M1): a QUIET link
// carries every light user's messages, each a padded record (~8 Mbit/s a
// direction with 200 probe users), and on fq_codel it is one flow among the
// bulk ones: with N=64 and 17 bulk flows its share was 100/18 = 5.6 Mbit/s,
// CoDel dropped its packets (12 resent) and the probe's p99 went to +75 ms
// (one_vs_16, against +12.5 for round 2's final with two QUIET links). Two
// halve each one's rate. Past quietOneMax a pool already has more
// connections than the model's population, so the second costs no tell
// the first did not.
func QuietCount(maxLinks int) int {
	switch {
	case QuietN > 0:
		return QuietN
	case QuietExtra != 0 && maxLinks <= quietOneMax:
		return 1
	case QuietExtra != 0:
		return 2
	case maxLinks >= 4:
		return 2
	}
	return 1
}

// quietOneMax: the most links a pool of one QUIET link has (QuietExtra).
const quietOneMax = 5

// QuietN (HS2_TUNE_QUIET_N, lab): the QUIET links of every pool, when
// above 0. Both ends must set it alike (the exit dials the reverse pool's
// links: QuietTotal).
var QuietN = 0

// QuietBatch 0 (HS2_TUNE_QUIET_BATCH=0, lab: the measure of γ2's batching):
// a QUIET link's writer flushes the shaper after each frame (smux
// Config.FlushEach), so each small message is a padded record of its own.
// 1, the default: a record takes every frame queued by the time it is
// written, up to the record's sampled length, and nothing waits for more
// (the shaper's carry; R1 round 3, γ2). Each end's switch is its own
// sending's.
var QuietBatch = 1

// QuietLazy 0 (HS2_TUNE_QUIET_LAZY=0, lab): with QuietBatch, a QUIET link's
// writer that ran out of frames writes the shaper's tail at once. 1, the
// default: while the socket has bytes unsent, the tail waits for them to go
// (they are ahead of it whatever it does) and takes what comes meanwhile
// (shape.go's lazy): more messages to a record, and no wait of its own
// past shapeLazyCheck.
var QuietLazy = 1

// QuietThin 0 (HS2_TUNE_QUIET_THIN=0): a QUIET link's socket keeps Linux's
// exponential back-off of its retransmission timeout. 1, the default (the
// R1 round-3 lead's note): TCP_THIN_LINEAR_TIMEOUTS, so a thin stream (under
// four segments in flight, a QUIET link's usual state) resends a lost
// message after one RTO each time, its first six times, not after 2, 4, 8
// of them; a light user waits one RTO for a lost probe, not seconds.
var QuietThin = 1

// QuietCoalesce (HS2_TUNE_QUIET_COAL, µs; 0: none): a busy QUIET link's
// writer holds the tail of its light frames this long (a uniform draw in
// [0.5, 1.5) of it with QuietCoalJitter) from the first of them for more to
// share its padded record (smux Config.Coalesce), while it wrote at least
// QuietCoalesceMin light frames over the last second. Candidate gamma1 of
// round 3 measured it: 4 ms took the QUIET records 3.5 times fewer (41069
// to 11738 in 23 s with 200 users) and P10 (a) from 0.879 to 0.957-0.962,
// for +4 to +6 ms on the probe (the bound +20); gamma2's batching without a
// hold took 10-17 % fewer. The judge merged it onto gamma2 with a jittered
// hold: gamma1's fixed one spaced the QUIET packets 4.26 ms apart at the
// median (its DPI capture), a timing mark of its own. The one-way delay
// messages and the TUN's frames are urgent (no hold after them).
var QuietCoalesce = 4000

// QuietCoalesceMin: the light frames a second of a QUIET link's writer past
// which it holds (fewer: a padded record costs little, the wait all).
var QuietCoalesceMin = 200

// QuietCoalJitter 0 (HS2_TUNE_QUIET_COAL_JITTER=0, lab): the hold is
// QuietCoalesce itself (gamma1's).
var QuietCoalJitter = 1

// QuietCoalTun 0 (HS2_TUNE_QUIET_COAL_TUN=0, lab): a TUN frame does not end
// the hold (its ping then waits it too).
var QuietCoalTun = 1

// QuietExtra 1 (HS2_TUNE_QUIET_EXTRA; the R1 round-2 judge, the default
// since round 3 by decision T19): the QUIET links come next to max_links
// BULK ones, not within max_links. Within them, a pool of N links under a
// policer on each connection (a per-flow limit on the path) carries bulk on
// N − 2 (N ≥ 4; N − 1 below): on the rig, N=8 under a 10 Mbit/s policer
// each, a lone user's 16 connections got 55 Mbit/s against 72 for the base
// binary and 74 for β1 (whose QUIET connection is a link's second); with
// the QUIET links outside, 73.1. The cost is one connection more than
// max_links (QuietCount). Both ends set it alike (the exit dials the
// reverse pool's links: QuietTotal); 0: within max_links, as β2.
var QuietExtra = 1

// QuietTotal is the most links a pool of maxLinks holds with its QUIET ones
// (they count toward max_links, but a pool of one link still has a BULK
// one: one more then; QuietExtra: maxLinks BULK ones and the QUIET ones).
func QuietTotal(maxLinks int) int {
	if QuietExtra != 0 {
		return maxLinks + QuietCount(maxLinks)
	}
	return max(maxLinks, 1+QuietCount(maxLinks))
}

const (
	// moveBytes, moveSpan: a connection moves once it moved at least this
	// much over the last 2 s in one direction (§3.5) ...
	moveBytes = 64 << 10
	moveSlots = 20 // of moveTick each
	moveTick  = 100 * time.Millisecond
	// ... while that direction kept a send backlog moveRun ticks in a row
	// (200 ms): this end's writer was inside Write at least moveBusy of each
	// tick (upload); for what this end receives (download) it cannot see the
	// exit's backlog, so the stand-in is data arriving, at least moveRxMin a
	// tick, a tick longer (moveRxRun)
	moveRun   = 2
	moveBusy  = 0.5
	moveRxRun = 3
	moveRxMin = 4 << 10
	// quietOfferHold: a reverse edge's CapQuiet offer counts toward its
	// QUIET links this long, while the link it was made to comes up
	quietOfferHold = 15 * time.Second
)

// quietLink is one QUIET link of the edge's pool.
type quietLink struct {
	link  *mtcpLink
	id    int
	born  time.Time
	users atomic.Int32
}

// quietSet is the edge's QUIET links, and the mover of its group streams.
type quietSet struct {
	m    *LinkManager
	want int
	// state: 0 not known yet, 1 on (a link has CapGroup), -1 off (a link
	// without it: a level 1 peer, or the other server's switch)
	state atomic.Int32

	mu      sync.Mutex
	links   []*quietLink
	seq     int
	dialing int
	offers  []int64 // reverse: CapQuiet offered at these ctrlNow, not yet arrived
	logAt   time.Time

	// the group streams not moved yet (the mover's), by id too (the exit's
	// move hints, hint.go), and the tallies
	gmu    sync.Mutex
	groups []*groupStream
	byID   map[uint32]*groupStream
	hinted atomic.Int64 // hints heard for a group not moved yet
	// hintOn: the QUIET links whose kindHint stream is up (hint.go), under
	// mu: their groups move for a download on the exit's word alone
	hintOn   map[*mtcpLink]int
	opened   atomic.Int64 // group connections opened on a QUIET link
	fallback atomic.Int64 // user connections opened on a BULK link (no QUIET link up)
	moved    atomic.Int64 // group connections moved to a BULK link
	moveFail atomic.Int64 // moves that found no BULK link (tried again later)
}

// enableQuiet gives the pool its QUIET links (the edge, β2 on): maxLinks is
// its max_links, of which QuietCount go to QUIET links.
func (m *LinkManager) enableQuiet() *quietSet {
	if QuietOff() {
		return nil
	}
	q := &quietSet{m: m, want: QuietCount(m.max)}
	m.quiet = q
	if cd, ok := m.dialer.(classDialer); ok {
		m.dialer = bulkDialer{cd} // the pool's own links are BULK
	}
	bm := m.bulkMax()
	m.target.Store(int32(warmSize(min(m.min, bm), bm)))
	m.ap.max, m.ap.min = bm, min(m.ap.min, bm)
	m.ap.T = min(m.ap.T, bm)
	f := q.line
	placeLine.Store(&f)
	return q
}

// active says whether the pool keeps QUIET links now.
func (q *quietSet) active() bool { return q != nil && q.state.Load() >= 0 && !QuietOff() }

// noteCaps learns from a new link's caps whether the other server has
// group streams; a link without them turns the QUIET links off.
func (q *quietSet) noteCaps(caps uint32) {
	if q == nil {
		return
	}
	if caps&tlscarrier.CapGroup != 0 {
		q.state.CompareAndSwap(0, 1)
		return
	}
	if q.state.Swap(-1) != -1 {
		q.m.log("mtcp: the other server has no group streams (an older hs2, or HS2_TUNE_QUIET=0 there): no QUIET links, every link carries everything as before")
		q.m.notifyCtl()
	}
}

// bulkMax is the most BULK links the pool runs: max_links less its QUIET
// links, at least one.
func (m *LinkManager) bulkMax() int {
	if m.quiet.active() && QuietExtra == 0 {
		return max(1, m.max-m.quiet.want)
	}
	return m.max
}

// quietWant is how many QUIET links the pool keeps now (0: none).
func (m *LinkManager) quietWant() int {
	if m.quiet.active() {
		return m.quiet.want
	}
	return 0
}

// alive is the QUIET links up now.
func (q *quietSet) aliveLocked() int {
	n := 0
	for _, ql := range q.links {
		if ql.link.Alive() {
			n++
		}
	}
	return n
}

func (q *quietSet) count() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.aliveLocked()
}

// has says whether l is one of the QUIET links.
func (q *quietSet) has(l Link) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.ContainsFunc(q.links, func(ql *quietLink) bool { return Link(ql.link) == l })
}

// add takes a new QUIET link into the set.
func (q *quietSet) add(l *mtcpLink, from string) {
	q.mu.Lock()
	ql := &quietLink{link: l, id: q.seq, born: q.m.now()}
	q.seq++
	q.links = append(q.links, ql)
	n := q.aliveLocked()
	q.mu.Unlock()
	if l.mtr != nil && l.mtr.wc != nil {
		l.mtr.wc.setName(fmt.Sprintf("quiet link %d", ql.id))
	}
	q.m.upLog.log("mtcp: QUIET link q%d up%s (now %d QUIET)", ql.id, from, n)
	if q.m.OnLink != nil {
		go q.m.OnLink(l)
	}
}

// drop takes l out of the set (a reverse link that ended), with why.
func (q *quietSet) drop(l Link) {
	q.mu.Lock()
	id := -1
	q.links = slices.DeleteFunc(q.links, func(ql *quietLink) bool {
		if Link(ql.link) == l {
			id = ql.id
			return true
		}
		return false
	})
	n := q.aliveLocked()
	q.mu.Unlock()
	l.Close()
	if id >= 0 && !q.m.closing.Load() {
		q.m.downLog.log("mtcp: QUIET link q%d down: %s (now %d QUIET)%s", id, linkDownReason(l), n, linkStallNote(l))
	}
}

// reap takes the dead QUIET links out and logs each.
func (q *quietSet) reap() {
	q.mu.Lock()
	var dead []*quietLink
	q.links = slices.DeleteFunc(q.links, func(ql *quietLink) bool {
		if !ql.link.Alive() {
			dead = append(dead, ql)
			return true
		}
		return false
	})
	n := q.aliveLocked()
	q.mu.Unlock()
	for _, ql := range dead {
		ql.link.Close()
		q.m.downLog.log("mtcp: QUIET link q%d down: %s (now %d QUIET)%s", ql.id, linkDownReason(ql.link), n, linkStallNote(ql.link))
	}
}

// tickIf is tick for a pool that may have no QUIET links (nil).
func (q *quietSet) tickIf(ctx context.Context) {
	if q != nil {
		q.tick(ctx)
	}
}

// tick keeps the direct edge's QUIET links up: the dead ones out, one dial
// at a time through the pool's gate while fewer than want are up.
func (q *quietSet) tick(ctx context.Context) {
	q.reap()
	if !q.active() || q.m.dialer == nil || q.m.refuse.holding(q.m.now()) > 0 {
		return
	}
	qd, ok := q.m.dialer.(classDialer)
	if !ok {
		return
	}
	q.mu.Lock()
	need := q.want - q.aliveLocked() - q.dialing
	if need > 0 {
		q.dialing++
	}
	q.mu.Unlock()
	if need <= 0 {
		return
	}
	q.m.dialWG.Add(1)
	go func() {
		defer q.m.dialWG.Done()
		defer func() {
			q.mu.Lock()
			q.dialing--
			q.mu.Unlock()
		}()
		release, ok := q.m.gate.acquireIf(ctx, func() bool { return q.active() })
		if !ok {
			return
		}
		defer release()
		l, err := qd.DialClass(ctx, tlscarrier.CapQuiet)
		if err != nil {
			q.mu.Lock()
			logIt := q.m.now().Sub(q.logAt) >= dialFailLogEvery
			if logIt {
				q.logAt = q.m.now()
			}
			q.mu.Unlock()
			if logIt {
				q.m.log("mtcp: a QUIET link's dial failed: %v", err)
			}
			return
		}
		if ctx.Err() != nil || q.m.closing.Load() {
			l.Close()
			return
		}
		if !q.place(l, "") {
			// the other server has no classes (an older hs2): a link like
			// any other, in the pool
			q.m.adopt(l)
		}
	}()
}

// adopt takes a link dialed as QUIET that came up without the class into
// the pool, as a dial of its own would (queueDial).
func (m *LinkManager) adopt(l Link) {
	m.mu.Lock()
	id := m.linkSeq
	m.linkSeq++
	m.noteArrivalLocked(m.now())
	m.links = append(m.links, m.newManaged(l, id, m.now()))
	m.mu.Unlock()
	m.dialedN.Add(1)
	if m.OnLink != nil {
		go m.OnLink(l)
	}
}

// place puts a link dialed or accepted as QUIET where its caps say: the set
// if it is QUIET, else the pool (the other server is older: noteCaps).
func (q *quietSet) place(l Link, from string) bool {
	ml, ok := l.(*mtcpLink)
	if !ok {
		return false
	}
	q.noteCaps(ml.caps)
	if !ml.quiet {
		return false
	}
	q.add(ml, from)
	return true
}

// offer is the reverse edge's Server.OfferCaps: the class it wants the
// link to be — QUIET while fewer QUIET links are up or on their way than
// wanted, else BULK (none while the pool keeps no classes).
func (q *quietSet) offer() uint32 {
	if !q.active() {
		return 0
	}
	now := ctrlNow()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.offers = slices.DeleteFunc(q.offers, func(at int64) bool { return now-at > int64(quietOfferHold) })
	if q.aliveLocked()+len(q.offers) >= q.want {
		return tlscarrier.CapBulk
	}
	q.offers = append(q.offers, now)
	return tlscarrier.CapQuiet
}

// arrived: a reverse link offered CapQuiet came up QUIET.
func (q *quietSet) arrived() {
	q.mu.Lock()
	if len(q.offers) > 0 {
		q.offers = q.offers[1:]
	}
	q.mu.Unlock()
}

// pick chooses a QUIET link for a new connection (never one in avoid): one
// that is not lagging first, then the fewest connections. release gives
// its slot back (once).
func (q *quietSet) pick(avoid []Link) (*quietLink, func(), bool) {
	if !q.active() {
		return nil, nil, false
	}
	now := ctrlNow()
	q.mu.Lock()
	defer q.mu.Unlock()
	var best *quietLink
	bestLag := true
	for _, ql := range q.links {
		if !ql.link.Alive() || slices.Contains(avoid, Link(ql.link)) {
			continue
		}
		lag := meterLagging(ql.link.mtr, now)
		if best == nil || bestLag && !lag || lag == bestLag && ql.users.Load() < best.users.Load() {
			best, bestLag = ql, lag
		}
	}
	if best == nil {
		return nil, nil, false
	}
	best.users.Add(1)
	q.m.users.Add(1)
	var once sync.Once
	return best, func() {
		once.Do(func() {
			best.users.Add(-1)
			q.m.users.Add(-1)
		})
	}, true
}

// meterLagging is managedLink.lagging's reading of a link's meter: its
// traffic waits on the path right now.
func meterLagging(mtr *linkMeter, now int64) bool {
	if mtr == nil {
		return false
	}
	if w := mtr.ctrlWait.Load(); w != 0 && now-w >= int64(lagWait) {
		return true
	}
	if mtr.ctrlSlow.Load() {
		return true
	}
	if w := mtr.wrStart.Load(); w != 0 && now-w >= int64(lagWait) {
		return true
	}
	r := mtr.rxAt.Load()
	return r != 0 && now-r >= int64(lagQuiet)
}

// pickOpen is the link a new user connection opens on: a QUIET one while
// the pool keeps them (never one in tried), else the pool's pick, which may
// wait for a link (pickWait).
func pickOpen(ctx context.Context, lm *LinkManager, hold bool, tried ...Link) (Link, func(), bool) {
	if ql, rel, ok := lm.quiet.pick(tried); ok {
		return ql.link, rel, true
	}
	if lm.quiet.active() {
		lm.quiet.fallback.Add(1)
	}
	return pickWait(ctx, lm, hold, tried...)
}

// pickOpenExcept is pickOpen that never waits (pickExcept).
func pickOpenExcept(lm *LinkManager, tried []Link, hold bool) (Link, func(), bool) {
	if ql, rel, ok := lm.quiet.pick(tried); ok {
		return ql.link, rel, true
	}
	return lm.pickExcept(tried, hold)
}

// ---- group streams at the edge ----

// edgeGroupIDs is the edge's live group ids: a new one never takes one of
// them (§4.2).
var edgeGroupIDs sync.Map

// newGroupID is a fresh group id: random, never 0, not live here.
func newGroupID() uint32 {
	for {
		id := rand.Uint32()
		if id == 0 {
			continue
		}
		if _, taken := edgeGroupIDs.LoadOrStore(id, true); !taken {
			return id
		}
	}
}

// groupStream is a user connection the edge opened on a QUIET link as a
// group (smux.Group): it reads and writes the group, counts its bytes with
// the link it is on, and moves once to a BULK link (quietSet.move).
type groupStream struct {
	g   *smux.Group
	tag uint32
	q   *quietSet

	mu     sync.Mutex
	qcs    *countedStream // its main stream, counted on the QUIET link (until the move)
	bcs    *countedStream // its move stream, counted on the BULK link
	qrel   func()         // its slot on the QUIET link (the connection's release too)
	brel   func()         // its slot on the BULK link
	closed bool

	rule moveRule // the mover's
	// noMove: it will not move (the group refused: it ended, or moved)
	noMove bool
	// ql: the QUIET link it opened on; while that link's kindHint stream is
	// up (quietSet.hintOn) it moves for a download on the exit's word, not
	// on data arriving (hint.go); hintAt: when the exit's hint came
	// (ctrlNow; 0: none)
	ql     *mtcpLink
	hintAt atomic.Int64
}

// openGroupStream opens a user connection of user tag on the QUIET link l
// as a group; rel is its slot on l.
func openGroupStream(q *quietSet, l *mtcpLink, tag uint32, rel func()) (stream, error) {
	id := newGroupID()
	g, err := l.sess.OpenGroup(tag, 0, id)
	if err != nil {
		edgeGroupIDs.Delete(id)
		return nil, err
	}
	gs := &groupStream{g: g, tag: tag, q: q, qrel: rel, qcs: l.track(g.Main()), ql: l}
	if q != nil {
		q.opened.Add(1)
		q.gmu.Lock()
		q.groups = append(q.groups, gs)
		if q.byID == nil {
			q.byID = map[uint32]*groupStream{}
		}
		q.byID[id] = gs
		q.gmu.Unlock()
	}
	return gs, nil
}

// cur is the counted stream the connection is on now.
func (gs *groupStream) cur() *countedStream {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.bcs != nil {
		return gs.bcs
	}
	return gs.qcs
}

func (gs *groupStream) Read(p []byte) (int, error) {
	n, err := gs.g.Read(p)
	if n > 0 {
		gs.cur().bytes.Add(uint64(n))
	}
	return n, err
}

func (gs *groupStream) Write(p []byte) (int, error) {
	n, err := gs.g.Write(p)
	if n > 0 {
		gs.cur().bytes.Add(uint64(n))
	}
	return n, err
}

// Close ends the connection: the group's streams (its main one lingers
// until the exit's FIN after a move: smux group.go), its counts on both
// links, and its BULK slot (the QUIET one is the relay's to release).
func (gs *groupStream) Close() error {
	gs.mu.Lock()
	first := !gs.closed
	gs.closed = true
	qcs, bcs, brel := gs.qcs, gs.bcs, gs.brel
	gs.mu.Unlock()
	err := gs.g.Close()
	if first {
		qcs.untrack()
		if bcs != nil {
			bcs.untrack()
		}
		if brel != nil {
			brel()
		}
		edgeGroupIDs.Delete(gs.g.ID())
	}
	return err
}

func (gs *groupStream) Answered() bool                     { return gs.g.Answered() }
func (gs *groupStream) GetDieCh() <-chan struct{}          { return gs.g.GetDieCh() }
func (gs *groupStream) SetWriteDeadline(t time.Time) error { return gs.g.SetWriteDeadline(t) }
func (gs *groupStream) SetReadDeadline(t time.Time) error  { return gs.g.SetReadDeadline(t) }

// guard is the wedge guard of the link the connection is on now.
func (gs *groupStream) guard() *sessGuard {
	if cs := gs.cur(); cs != nil && cs.link != nil && cs.link.mtr != nil {
		return cs.link.mtr.guard
	}
	return nil
}

var _ io.ReadWriteCloser = (*groupStream)(nil)

// moveRule is one group connection's move rule, as the mover ticks it
// (§3.5): per direction the bytes of the last 2 s, and how long it has
// kept a send backlog.
type moveRule struct {
	ticked             bool
	at                 int64 // the latest tick (ctrlNow / moveTick)
	lastTx, lastRx     uint64
	lastBusy           time.Duration
	lastAt             int64
	tx, rx             [moveSlots]uint32
	sumTx, sumRx       uint64
	pos                int
	busyRun, rxRun     int
	lastDir            string
	firstBulkAt, dueAt int64
}

// tick takes the connection's counters at now (ctrlNow) and says whether it
// is due to move, and in which direction it showed bulk behaviour. txBusy:
// busy is its writer's time inside Write (a stream); else (a TUN flow, no
// writer to watch) its sending direction goes by the bytes rule, as the
// receiving one. rxRule false: the receiving direction does not move it
// (the exit's move hint does, hint.go).
func (r *moveRule) tick(tx, rx uint64, busy time.Duration, now int64, txBusy, rxRule bool) (due bool, dir string) {
	k := now / int64(moveTick)
	if !r.ticked {
		r.ticked, r.at, r.lastTx, r.lastRx, r.lastBusy, r.lastAt = true, k, tx, rx, busy, now
		return false, ""
	}
	steps := k - r.at
	if steps <= 0 {
		return false, ""
	}
	r.at = k
	dt := time.Duration(now - r.lastAt)
	dtx, drx, dbusy := tx-r.lastTx, rx-r.lastRx, busy-r.lastBusy
	r.lastTx, r.lastRx, r.lastBusy, r.lastAt = tx, rx, busy, now
	// age the slots by the ticks since the latest
	for i := int64(0); i < min(steps, moveSlots); i++ {
		r.pos = (r.pos + 1) % moveSlots
		r.sumTx -= uint64(r.tx[r.pos])
		r.sumRx -= uint64(r.rx[r.pos])
		r.tx[r.pos], r.rx[r.pos] = 0, 0
	}
	r.tx[r.pos] = uint32(min(dtx, 1<<31))
	r.rx[r.pos] = uint32(min(drx, 1<<31))
	r.sumTx += uint64(r.tx[r.pos])
	r.sumRx += uint64(r.rx[r.pos])
	txRun := moveRun
	if !txBusy {
		txRun = moveRxRun
	}
	switch {
	case steps != 1:
		r.busyRun = 0
	case txBusy && dt > 0 && float64(dbusy) >= moveBusy*float64(dt) && dtx > 0,
		!txBusy && dtx >= moveRxMin:
		r.busyRun++
	default:
		r.busyRun = 0
	}
	if steps == 1 && drx >= moveRxMin {
		r.rxRun++
	} else {
		r.rxRun = 0
	}
	switch {
	case r.sumTx >= moveBytes && r.busyRun >= txRun:
		return true, "up"
	case rxRule && r.sumRx >= moveBytes && r.rxRun >= moveRxRun:
		return true, "down"
	}
	return false, ""
}

// runMover ticks the move rule of every group connection not moved yet, and
// moves those that are due, until ctx ends.
func (q *quietSet) runMover(ctx context.Context) {
	t := time.NewTicker(moveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		q.moverTick(ctrlNow())
	}
}

// moverTick is one look at the group connections.
func (q *quietSet) moverTick(now int64) {
	q.gmu.Lock()
	gs := q.groups
	q.gmu.Unlock()
	keep := gs[:0:0]
	for _, s := range gs {
		s.mu.Lock()
		gone := s.closed || s.noMove
		s.mu.Unlock()
		if gone {
			continue
		}
		if s.g.Moved() != nil {
			continue
		}
		due, dir := s.rule.tick(s.g.TxBytes(), s.g.RxBytes(), s.g.WriteBusy(), now, true, !q.hinting(s.ql))
		if !due && s.hintAt.Load() != 0 {
			due, dir = true, "down" // the exit's word: a send backlog there (hint.go)
		}
		if due && q.move(s, dir) {
			continue
		}
		keep = append(keep, s)
	}
	q.gmu.Lock()
	q.groups = append(keep, q.groups[len(gs):]...) // with those opened meanwhile
	if len(q.byID) > len(q.groups) {
		// forget the ids of the groups the mover is done with
		live := make(map[uint32]*groupStream, len(q.groups))
		for _, s := range q.groups {
			live[s.g.ID()] = s
		}
		q.byID = live
	}
	q.gmu.Unlock()
}

// hintUp and hintDown: link's kindHint stream came up, ended (hint.go).
func (q *quietSet) hintUp(l *mtcpLink) {
	q.mu.Lock()
	if q.hintOn == nil {
		q.hintOn = map[*mtcpLink]int{}
	}
	q.hintOn[l]++
	q.mu.Unlock()
}

func (q *quietSet) hintDown(l *mtcpLink) {
	q.mu.Lock()
	if q.hintOn[l]--; q.hintOn[l] <= 0 {
		delete(q.hintOn, l)
	}
	q.mu.Unlock()
}

// hinting says whether the exit's move hints come over link's stream now.
func (q *quietSet) hinting(l *mtcpLink) bool {
	if l == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.hintOn[l] > 0
}

// noteHint takes the exit's move hint for group id (hint.go): the mover
// moves it at its next tick (an id it does not know, or one done with, is
// let go).
func (q *quietSet) noteHint(id uint32) {
	q.gmu.Lock()
	gs := q.byID[id]
	q.gmu.Unlock()
	if gs == nil {
		return
	}
	if gs.hintAt.CompareAndSwap(0, ctrlNow()) {
		q.hinted.Add(1)
	}
}

// move moves gs to a BULK link the pool picks: the group's move stream
// there, the connection counted there, its QUIET slot given back. True: the
// mover is done with gs (it moved, or its group cannot move: it ended or
// moved already); false: no BULK link took it now (tried again at the
// mover's next tick).
func (q *quietSet) move(gs *groupStream, dir string) bool {
	link, rel, ok := q.m.pickExcept(nil, false)
	if !ok {
		q.moveFail.Add(1)
		return false
	}
	ml, isM := link.(*mtcpLink)
	if !isM || !ml.bulkClass() {
		rel()
		q.moveFail.Add(1)
		return false
	}
	s1, err := gs.g.Move(ml.sess, gs.tag)
	if err != nil {
		rel()
		if err == smux.ErrNoMove {
			gs.mu.Lock()
			gs.noMove = true
			gs.mu.Unlock()
			return true // it ended (or moved): not tried again
		}
		q.moveFail.Add(1)
		return false
	}
	gs.mu.Lock()
	gs.bcs, gs.brel = ml.track(s1), rel
	qcs, qrel, closed := gs.qcs, gs.qrel, gs.closed
	gs.mu.Unlock()
	if closed {
		// ended while it moved: Close missed the BULK slot and count
		gs.bcs.untrack()
		rel()
	}
	qcs.untrack()
	if qrel != nil {
		qrel()
	}
	gs.rule.lastDir = dir
	q.moved.Add(1)
	return true
}

// line is the edge's placement status line (§3.5, in part): the QUIET and
// BULK links, and what opened and moved.
func (q *quietSet) line() string {
	q.gmu.Lock()
	waiting := len(q.groups)
	q.gmu.Unlock()
	st := "on"
	switch q.state.Load() {
	case 0:
		st = "waiting for the first link"
	case -1:
		st = "off (the other server has no group streams)"
	}
	return fmt.Sprintf("place: %s; quiet %d of %d links, bulk %d links; opened on quiet %d, on bulk (no quiet up) %d; moved to bulk %d (not yet %d, no bulk link %d); exit hints %d",
		st, q.count(), q.want, q.m.count(), q.opened.Load(), q.fallback.Load(), q.moved.Load(), waiting, q.moveFail.Load(), q.hinted.Load())
}

// placeLine is the status line of the process's placement (nil: none).
var placeLine atomic.Pointer[func() string]

// quietLinksOf is the QUIET links, by id (tests, status).
func (q *quietSet) quietLinks() []*quietLink {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := slices.Clone(q.links)
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}
