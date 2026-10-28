package engine

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// R1's send path, angle A (R1-PLAN task 15): the link scheduler (C2) inside
// smux. Each link's smux session gets an engine.Sched of its own through
// smux.Config.Sched (schedAdapter: smux knows the FrameSched interface, not
// this package), in place of its heap and shaper goroutine; the session's
// writer takes one frame at a time from it and writes it through the
// link's host-queue guard (C3, hostQConn), so it never holds more than one
// frame past the socket's low-water mark. What smux asks of the engine:
//
//   - FlowInfo (linkFlows.info): a stream's user tag (its SYN's) and lane
//     (C1's class, queueClass) as its writer cuts each data frame; the frame
//     carries LaneFrameMax in the latency lane, BulkFrameSize of the link's
//     rate in the bulk lane;
//   - UserMem (userMems): each user's queued bytes, user_mem_share's account;
//   - for the TUN, frames of any 5-tuple flow on the link's kindL3 stream
//     (smux.Stream.WriteFlowAsync, l3sched.go), so a ping goes in the
//     latency lane of the same scheduler as the user streams, and a TUN bulk
//     flow shares its user's floor with the user's port streams.
//
// A user has at most schedUserFrames data frames of its streams queued on a
// link; the writer of one more waits in that user's FIFO (backpressure,
// nothing dropped), and other users' writers and control frames never wait
// for it. Smux's 1024-request cap of the old heap is gone: control frames
// have their own lane, and the streams bound what is queued (one frame each).
//
// The cores' runner (coreSet) steps every link each 100 ms: C1's
// classifier over its streams, C4-lite's reading of its socket (the pacing
// cap on every link socket at both ends; the exit's pacing net hands its cap
// to it and the lesser holds) and, each hostqTick, C3's mark and gate.
//
// Kill switches (§9.1): HS2_TUNE_SCHED=0 keeps smux's heap and shaper loop,
// the TUN's FIFO, writeLoop, l3MaxSojourn and the per-packet rendezvous
// hash; HS2_TUNE_HQG=0 the fixed low-water mark and sendGate; each core's
// caps bit leaves the link's caps with its switch (tlscarrier.LocalAuthHdr),
// and a core runs on a link only when both ends have its bit.

// schedUserFrames: a user's data frames queued on a link at most (§3.2).
const schedUserFrames = 64

// laneData: a latency-lane frame's data at most, so the frame with smux's
// header is within LaneFrameMax (a larger one the scheduler queues as bulk).
const laneData = LaneFrameMax - smux.FrameHeaderSize

// schedAdapter is a link's Sched as smux's FrameSched.
type schedAdapter struct{ s *Sched[any] }

// schedLinkSeq numbers the links' schedulers (XShare's link ids).
var schedLinkSeq atomic.Int64

func newSchedAdapter() smux.FrameSched {
	s := NewSched[any]()
	if XShare != 0 {
		s.setXShare(&xshares, int(schedLinkSeq.Add(1))) // xshare.go
	}
	return &schedAdapter{s: s}
}

func (a *schedAdapter) Push(it any, user uint32, flow uint64, lane uint8, size int) {
	a.s.Push(it, user, flow, Lane(lane), size)
}

func (a *schedAdapter) Next() (any, bool) { return a.s.Next() }

// NextUrgent is smux's UrgentSched: what goes past a bulk frame waiting at
// the in-flight budget (budget.go).
func (a *schedAdapter) NextUrgent() (any, bool) { return a.s.NextUrgent() }

func (a *schedAdapter) Tick(now time.Duration, rate float64) { a.s.Tick(now, rate) }

// laneOf is the lane of a flow queued as class c (queueClass).
func laneOf(c Class) uint8 {
	if c == ClassBulk {
		return smux.LaneBulk
	}
	return smux.LaneLatency
}

// ---- C1 on a link's streams ----

// flowEnt is one stream's C1 state.
type flowEnt struct {
	st    *smux.Stream
	tag   uint32
	stats flowStats
	cls   flowClassifier // the runner's
	class atomic.Uint32  // class_tx at the latest tick
	// the flips counted into the link's tallies so far (the runner's)
	toBulk, toLight uint32
	// moved: a group connection's move stream (it came here for its bulk
	// behaviour): with MovedBulk its class stays bulk
	moved bool
	// rtAllow, rtBase: its realtime bytes a tick and its tx at the latest
	// tick (TuneRT, rtlane.go)
	rtAllow, rtBase atomic.Int64
}

// MovedBulk 0 (HS2_TUNE_MOVED_BULK=0): a connection that moved to a BULK
// link is bulk until it is quiet (C1's rule, round 2). 1, the default (R1
// round 3, the judge): it stays bulk there. 400 connections moved to one
// BULK link each carried 22 kbit/s, under C1's quiet mark (8 KiB in 2 s),
// so each was light again, its frames went in the latency lane, which the
// in-flight budget never holds: the budget never grew, its pacing cap (1.25
// of B over its round trip) held the link at 10 Mbit/s of 100, and each
// connection stayed slow, and light (the judge's smoke C2, 25 users of 16
// connections on one link). A connection on a BULK link is there for its
// backlog; its frames are bulk.
var MovedBulk = 1

// linkFlows is a link's streams as C1 sees them: smux's FlowInfo, and the
// runner's tick.
type linkFlows struct {
	mu       sync.Mutex
	bulkLink bool // a BULK link's (MovedBulk: its moved connections stay bulk)
	ents     []*flowEnt
	// the status line's (the runner's, read under mu): flows by class at the
	// latest tick, flips since the latest line and when that was
	light, bulk, unknown int
	toBulk, toLight      uint64
	lineAt               time.Duration
	// rt, rtSteady: flows with a realtime allowance at the latest tick, and
	// those of them steady, not light (TuneRT)
	rt, rtSteady int
}

// info is smux's FlowInfo: the stream's user tag (its SYN's) and the lane of
// its next data frame. A stream is known from its first data frame.
func (t *linkFlows) info(st *smux.Stream) (uint32, uint8) {
	e, _ := st.FlowCtx().(*flowEnt)
	if e == nil {
		e = t.add(st)
	}
	e.stats.tx.Store(st.TxBytes())
	if l := e.rtLane(); l != 0 {
		return e.tag, uint8(l) // rtlane.go
	}
	return e.tag, laneOf(queueClass(Class(e.class.Load()), &e.stats))
}

func (t *linkFlows) add(st *smux.Stream) *flowEnt {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, _ := st.FlowCtx().(*flowEnt); e != nil {
		return e
	}
	tag, _ := st.Syn()
	e := &flowEnt{st: st, tag: tag}
	if _, role, _ := st.GroupInfo(); role == smux.RoleMove {
		// a connection that moved here for its bulk behaviour (β2): bulk
		// from its first frame, until it is quiet (C1's rule)
		e.cls.d[0].cls = ClassBulk
		e.class.Store(uint32(ClassBulk))
		e.moved = MovedBulk != 0 && t.bulkLink
	}
	st.SetFlowCtx(e)
	t.ents = append(t.ents, e)
	return e
}

// tick judges every stream of the link (the runner, each classTick) and
// forgets the closed ones.
func (t *linkFlows) tick(now time.Duration) {
	t.mu.Lock()
	ents := t.ents
	t.mu.Unlock()
	keep := ents[:0:0]
	light, bulk, unknown, rt, rtSteady := 0, 0, 0, 0, 0
	var toBulk, toLight uint64
	for _, e := range ents {
		select {
		case <-e.st.GetDieCh():
			continue
		default:
		}
		keep = append(keep, e)
		tx := e.st.TxBytes()
		e.stats.tx.Store(tx)
		if TuneRT.Get() != 0 && TwoWay.Get() != 0 {
			e.stats.rx.Store(e.st.RxBytes()) // the other way (rtlane.go)
		}
		c, _, confirmed := e.cls.Tick(&e.stats, now)
		if e.moved {
			c = ClassBulk // moved for its backlog: bulk here (MovedBulk)
		}
		if TuneRT.Get() != 0 {
			a := rtAllowance(&e.cls, confirmed) // rtlane.go
			e.rtBase.Store(int64(tx))
			e.rtAllow.Store(a)
			if a > 0 && !e.moved {
				rt++
				if a != rtLightAllow {
					rtSteady++
				}
			}
		}
		e.class.Store(uint32(c))
		switch c {
		case ClassLight:
			light++
		case ClassBulk:
			bulk++
		default:
			unknown++
		}
		toBulk += uint64(e.cls.toBulk - e.toBulk)
		toLight += uint64(e.cls.toLight - e.toLight)
		e.toBulk, e.toLight = e.cls.toBulk, e.cls.toLight
	}
	t.mu.Lock()
	t.ents = append(keep, t.ents[len(ents):]...) // with those added meanwhile
	t.light, t.bulk, t.unknown = light, bulk, unknown
	t.rt, t.rtSteady = rt, rtSteady
	t.toBulk += toBulk
	t.toLight += toLight
	t.mu.Unlock()
}

// line is the link's classes line (§3.1) since the previous one.
func (t *linkFlows) line(now time.Duration) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	mins := 0.0
	if t.lineAt > 0 && now > t.lineAt {
		mins = (now - t.lineAt).Minutes()
	}
	b, l := 0.0, 0.0
	if mins > 0 {
		b, l = float64(t.toBulk)/mins, float64(t.toLight)/mins
	}
	t.toBulk, t.toLight, t.lineAt = 0, 0, now
	s := classLine(t.light, t.bulk, t.unknown, b, l)
	if TuneRT.Get() != 0 {
		s += fmt.Sprintf("; realtime %d (steady %d)", t.rt, t.rtSteady)
	}
	return s
}

// ---- user_mem_share ----

// UserMemShare is each user's floor of queue memory (user_mem_share, §3.2):
// what its tag has queued in the C2 queues of every link and in the TUN's
// flow queues. A floor, not a ceiling: past it a user borrows while the
// process is not under memory pressure (UserMemPool, the kernel's TCP
// memory); under pressure it gets no more past its share, its writers wait
// and its TUN packets are dropped, until it is back under it.
var (
	UserMemShare = 64 << 20
	UserMemPool  = 512 << 20
)

// userMemIdle: an account with nothing queued this long is forgotten.
const userMemIdle = 10 * time.Minute

type userMemAcct struct {
	n       atomic.Int64
	used    atomic.Int64 // ctrlNow of its last use
	refused atomic.Uint64
	capAt   atomic.Int64 // ctrlNow of its last refusal
}

// userMemCapHold: a user refused this recently counts as at its cap.
const userMemCapHold = time.Second

type userMemSet struct {
	mu      sync.Mutex
	m       map[uint32]*userMemAcct
	total   atomic.Int64
	sweepAt time.Duration
}

var userMems userMemSet

// acct is the account of tag.
func (s *userMemSet) acct(tag uint32) *userMemAcct {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[uint32]*userMemAcct{}
	}
	a := s.m[tag]
	if a == nil {
		a = &userMemAcct{}
		s.m[tag] = a
	}
	a.used.Store(ctrlNow())
	return a
}

// handle is smux's UserMem.
func (s *userMemSet) handle(tag uint32) func(int, bool) bool { return s.acct(tag).take }

// userMemPressure: the process's queues past UserMemPool, or this server's
// kernel TCP memory past its pressure mark.
func userMemPressure() bool {
	return userMems.total.Load() > int64(UserMemPool) || TCPMemPressure()
}

// take counts n bytes queued (n < 0: taken off). More past the share under
// pressure is refused unless forced (the user has nothing else queued
// there).
func (a *userMemAcct) take(n int, force bool) bool {
	if n > 0 && !force && a.n.Load()+int64(n) > int64(UserMemShare) && userMemPressure() {
		a.refused.Add(1)
		a.capAt.Store(ctrlNow())
		return false
	}
	a.n.Add(int64(n))
	userMems.total.Add(int64(n))
	if n > 0 {
		a.used.Store(ctrlNow())
	}
	return true
}

// atCap is how many users are at their memory cap: past their share now,
// or turned away within userMemCapHold.
func (s *userMemSet) atCap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, n := ctrlNow(), 0
	for _, a := range s.m {
		if a.n.Load() > int64(UserMemShare) || a.capAt.Load() != 0 && now-a.capAt.Load() < int64(userMemCapHold) {
			n++
		}
	}
	return n
}

// sweep forgets the accounts idle with nothing queued (the runner's).
func (s *userMemSet) sweep(now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tag, a := range s.m {
		if a.n.Load() == 0 && now-a.used.Load() > int64(userMemIdle) {
			delete(s.m, tag)
		}
	}
}

// ---- C3 under the shaper ----

// hostQConn is a link's conn under its host-queue guard (C3): writes go
// through the guard. Close ends a write waiting at the gate (sendGate's
// Close: a write that waits there holds the socket's write lock, and the
// TLS conn's close_notify would wait behind it for its 5 s deadline).
type hostQConn struct {
	net.Conn
	h                *hostQ
	tcp              *net.TCPConn
	waiting, closing atomic.Bool
}

func (c *hostQConn) Write(p []byte) (int, error) { return c.h.Write(p) }

func (c *hostQConn) Close() error {
	c.closing.Store(true)
	if fb := c.h.fb.Load(); fb != nil {
		return fb.Close()
	}
	if c.waiting.Load() && c.tcp != nil {
		c.tcp.Close()
	}
	return c.Conn.Close()
}

// gateSys is the link socket's hostSys, with the conn told when a write
// waits at the gate.
type gateSys struct {
	hostSys
	c *hostQConn
}

func (g gateSys) waitBelow(n int) error {
	g.c.waiting.Store(true)
	defer g.c.waiting.Store(false)
	if g.c.closing.Load() {
		return net.ErrClosed
	}
	return g.hostSys.waitBelow(n)
}

// ---- the cores' runner ----

// coreLink is one link's R1 cores, as the runner steps them and the status
// shows them.
type coreLink struct {
	id    int
	sess  *smux.Session
	mtr   *linkMeter   // nil: none
	sc    *sendCapLink // C4-lite's and C3's readings (nil: neither)
	hq    *hostQ       // nil: C3 off
	flows *linkFlows   // nil: C2 off
	shp   *shapedConn  // a QUIET link's shaper (its records' tallies; nil: not one)
	tun   atomic.Pointer[l3Link]
	// the status lines': when the hostq line was last made
	hqAt time.Duration
	// the runner's: the link's own event counters at its wake before
	// (noteEvents, events.go)
	seenDirty                        int64
	seenCuts, seenWrites, seenJoined uint64
}

// coreSet is the process's links with R1 cores; one goroutine steps them.
type coreSet struct {
	mu   sync.Mutex
	all  map[*smux.Session]*coreLink
	n    int
	once sync.Once
	tick int
}

var coreLinks coreSet

// coreTick: the runner's wake (C1's tick, C4-lite's).
const coreTick = 100 * time.Millisecond

func (cs *coreSet) add(l *coreLink) {
	cs.mu.Lock()
	if cs.all == nil {
		cs.all = map[*smux.Session]*coreLink{}
	}
	cs.n++
	l.id = cs.n
	cs.all[l.sess] = l
	cs.mu.Unlock()
	cs.once.Do(func() { go cs.run() })
}

// of is the cores of the link of sess (nil: none).
func (cs *coreSet) of(sess *smux.Session) *coreLink {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.all[sess]
}

// links is the live links, by id; the ended ones are dropped.
func (cs *coreSet) links() []*coreLink {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]*coreLink, 0, len(cs.all))
	for s, l := range cs.all {
		if s.IsClosed() {
			delete(cs.all, s)
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (cs *coreSet) run() {
	t := time.NewTicker(coreTick)
	defer t.Stop()
	for range t.C {
		cs.wake(time.Duration(ctrlNow()))
	}
}

// wake steps every live link once.
func (cs *coreSet) wake(now time.Duration) {
	for _, l := range cs.links() {
		l.step(now)
	}
	if cs.tick++; cs.tick%100 == 0 {
		userMems.sweep(int64(now))
	}
	if cs.tick%eventsEvery == 0 {
		r1ev.logMinute(coreLogf) // R1's events of the minute, one line (events.go)
	}
}

// step: C4-lite's reading of the link socket and C3's guard, and C1's tick.
func (l *coreLink) step(now time.Duration) {
	wrote := uint64(0)
	if l.mtr != nil {
		wrote = l.mtr.wrBytes.Load()
	}
	if sc := l.sc; sc != nil {
		if sc.sys != nil {
			sc.step(now, wrote)
		} else {
			// no socket to read (not TCP, or off Linux): C3 on the
			// written rate alone
			sc.mu.Lock()
			sc.guard(now, wrote, tcpStat{})
			sc.mu.Unlock()
		}
	}
	if l.flows != nil {
		l.flows.tick(now)
	}
	l.noteEvents(&r1ev)
}

// schedLine is the link's scheduler line (§3.2) since the previous one.
func (l *coreLink) schedLine() string {
	st, ok := l.sess.SchedStats()
	if !ok {
		return ""
	}
	var ss SchedStats
	l.sess.WithSched(func(fs smux.FrameSched) { ss = fs.(*schedAdapter).s.Stats() })
	var dl, db uint64
	if t := l.tun.Load(); t != nil && t.sq != nil {
		dl, db = t.sq.dropsLine()
	}
	return fmt.Sprintf("sched link %d: lane p50/p99 %.1f/%.1f ms, bulk frame %.1f KiB, users %d (active %d), drops light/bulk %d/%d, blocked-writers %d, users at memory cap %d",
		l.id, msOf(st.LaneP50), msOf(st.LaneP99), float64(st.BulkFrame)/1024, ss.Users, ss.Active, dl, db, st.Blocked, userMems.atCap())
}

// coreLinesMax: the links whose lines the status shows (the rest are
// counted).
const coreLinesMax = 8

// CoreLines is the R1 cores' status lines of this process's links (§3.1 to
// §3.4): per link its scheduler, classes, host-queue guard and pacing cap,
// for the first coreLinesMax links.
func CoreLines() []string {
	links := coreLinks.links()
	now := time.Duration(ctrlNow())
	var out []string
	if f := placeLine.Load(); f != nil {
		out = append(out, (*f)()) // the edge's QUIET and BULK links (β2)
	}
	exitHints.mu.Lock()
	hints := len(exitHints.chans) > 0
	exitHints.mu.Unlock()
	if hints || exitGroups.attached.Load()+exitGroups.refused.Load() > 0 {
		out = append(out, exitGroups.line())
	}
	if hints || exitHints.sent.Load() > 0 {
		out = append(out, exitHints.line())
	}
	out = append(out, r1ev.line()) // R1's events since start (events.go)
	for i, l := range links {
		if i == coreLinesMax {
			out = append(out, fmt.Sprintf("... and %d more link(s)", len(links)-coreLinesMax))
			break
		}
		if s := l.schedLine(); s != "" {
			out = append(out, s)
		}
		if l.flows != nil {
			out = append(out, l.flows.line(now))
		}
		if l.hq != nil {
			el := time.Duration(0)
			if l.hqAt > 0 {
				el = now - l.hqAt
			}
			l.hqAt = now
			out = append(out, l.hq.line(l.id, el))
		}
		if l.sc != nil && l.sc.sys != nil {
			s := l.sc.line(l.id)
			if l.mtr != nil {
				s += fmt.Sprintf("; owd guard dirty %d", l.mtr.owd.dirties.Load())
			}
			out = append(out, s)
			if s := l.sc.budgetLine(l.id); s != "" {
				out = append(out, s)
			}
		}
		if l.shp != nil {
			out = append(out, l.shp.recLine(l.id, now))
		}
	}
	return out
}
