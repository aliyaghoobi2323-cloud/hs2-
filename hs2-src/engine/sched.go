package engine

import (
	"time"
)

// C2, the link scheduler's pure core (REDESIGN §3.2): one per link and end,
// generic over what it queues (smux frames, TUN packets), so either place
// it is wired into can use it. Three lanes:
//
//  1. control (SYN/FIN/UPD/NOP, the link's status record): always first,
//     never behind data;
//  2. latency (light and unknown flows, C1): FIFO, each frame at most
//     LaneFrameMax, under each user's credit, a token bucket of
//     schedLaneRate with a burst of schedLaneBurst (against abuse only,
//     P10): a light frame of a user out of credit is queued as a bulk frame
//     of its own flow, in its user's ring, until the bucket refills;
//  3. bulk, two levels and work-conserving (P10): each active user u has a
//     floor F_u = min(D_u, C/N_active), D_u its demand over the last 200 ms
//     and C the link's rate, set each Tick; users under their floor are
//     served first (DRR among them, each user's flows in turn); past the
//     floors the link goes by flows (DRR over all of them), so a user of 16
//     flows takes 16 shares of the surplus and one of a single flow one,
//     never from anyone's floor.
//
// D_u is what the user would send if there were room: the bytes the link
// served while it had bulk queued (its share of the window, times C; a user
// kept waiting reads as wanting the whole link), and no less than twice
// what it pushed (a user whose queue the scheduler keeps empty, by serving
// it at once, asks for what it brings, with room to grow). Too high a
// demand costs no one: a floor not used is surplus within the same tick.
// Floors, credit and demand move only in Tick; Next is O(1) (no walk over
// users or flows), and a tick's walk is over the link's users.
//
// The quantum is BulkFrameSize of the link's rate (one bulk frame), or the
// frame at hand when larger: each turn serves at least one frame.
//
// Bounds the integration keeps (smux's one frame pending per stream, the
// 64 pending frames and the FIFO of waiting writers of a user, the TUN
// queues per flow and per user) are not the core's; Stats gives the most
// items a user has queued. A Sched is not safe for concurrent use: one
// writer loop owns it.

// Lane is a scheduler lane.
type Lane uint8

const (
	LaneCtrl Lane = iota
	LaneLatency
	LaneBulk
	// LaneRT and LaneRTSteady (R1 round 4, TuneRT, rtlane.go): confirmed
	// light flows (a game), then steady ones (a call), both ahead of the
	// latency lane's new flows, under each user's realtime credit
	LaneRT
	LaneRTSteady
)

// LaneFrameMax: a latency-lane frame is at most this; the writer splits a
// light flow's larger write (smux writeV2), and a larger one pushed to the
// lane is queued as bulk.
const LaneFrameMax = 2048

// TuneSched 0 (cmd/hs2 applyTuning: HS2_TUNE_SCHED=0) keeps the old heap
// and FIFO instead of C2.
var TuneSched = 1

// SchedOff says whether C2 is off (HS2_TUNE_SCHED=0).
func SchedOff() bool { return TuneSched == 0 }

const (
	// schedTick: floors, credit and demand move this often.
	schedTick = 100 * time.Millisecond
	// schedLaneRate, schedLaneBurst: each user's latency-lane credit,
	// bytes/s and bytes (16 KiB each tick).
	schedLaneRate  = 160 << 10
	schedLaneBurst = 32 << 10
	// schedDemandX: a user's demand is at least this many times what it
	// pushed over the window.
	schedDemandX = 2
	// bulkFrameTime: a bulk frame is this much of the link's rate, within
	// bulkFrameMin and bulkFrameMax: a light frame waits behind one at most
	// that long where the floor allows (P3).
	bulkFrameTime = 5 * time.Millisecond
	bulkFrameMin  = 1 << 10
	bulkFrameMax  = 16 << 10
	// schedSlab: flows and users are allocated this many at a time.
	schedSlab = 64
)

// BulkFrameSize is the bulk frame (and quantum) for a link of rate
// bytes/s: clamp(rate × 5 ms, 1 KiB, 16 KiB); an unknown rate (0, NaN)
// takes the floor.
func BulkFrameSize(rate float64) int {
	n := rate / float64(time.Second/bulkFrameTime)
	switch {
	case !(n > bulkFrameMin):
		return bulkFrameMin
	case n >= bulkFrameMax:
		return bulkFrameMax
	}
	return int(n)
}

// SchedStats is what a scheduler holds and did.
type SchedStats struct {
	Users, Active, Flows int       // user and flow records; users with bulk demand at the latest tick
	Pending              [4]int    // items queued per lane
	Served               [4]uint64 // bytes taken per lane
	// Demoted: latency-lane items queued as bulk (out of credit, behind
	// bulk of their own flow, or past LaneFrameMax).
	Demoted uint64
	// FloorUsers: users under their floor with bulk queued, now.
	FloorUsers int
	// MaxUser, MaxUserItems: the user with the most bulk items queued (the
	// fattest, first in an overflow) and how many.
	MaxUser      uint32
	MaxUserItems int
	Quantum      int     // the bulk quantum, bytes
	Rate         float64 // C at the latest tick, bytes/s
}

type schedNode[T any] struct {
	it   T
	size int
	f    *schedFlow[T] // nil: a control item
	next *schedNode[T]
}

type schedFIFO[T any] struct {
	head, tail *schedNode[T]
	n          int
}

func (q *schedFIFO[T]) push(n *schedNode[T]) {
	n.next = nil
	if q.tail == nil {
		q.head = n
	} else {
		q.tail.next = n
	}
	q.tail = n
	q.n++
}

func (q *schedFIFO[T]) pop() *schedNode[T] {
	n := q.head
	if n == nil {
		return nil
	}
	q.head = n.next
	if q.head == nil {
		q.tail = nil
	}
	q.n--
	return n
}

// schedFlow is one flow: its bulk items, and its places in its user's ring
// and in the link's (both while it has bulk items).
type schedFlow[T any] struct {
	id           uint64
	u            *schedUser[T]
	q            schedFIFO[T]
	lat          int  // its items in the latency lane
	rt           int  // its items in the realtime lanes
	rtSteady     bool // ... in LaneRTSteady's FIFO (else LaneRT's)
	uPrev, uNext *schedFlow[T]
	gPrev, gNext *schedFlow[T]
	gDef         int // its surplus deficit, bytes
	// idle: nothing queued since tick idleAt (freed a tick later); also the
	// free list's link
	idlePrev, idleNext *schedFlow[T]
	idleAt             int64
	inIdle             bool
}

// schedUser is one user tag on the link.
type schedUser[T any] struct {
	tag          uint32
	cur          *schedFlow[T] // its ring's next flow; nil: no bulk queued
	items        int           // bulk items queued
	flows        int           // flow records
	fPrev, fNext *schedUser[T] // the floor list
	inFloor      bool
	fDef         int     // its floor deficit, bytes
	demand       float64 // D_u at the latest tick, bytes/s
	floor        float64 // F_u at the latest tick, bytes/s
	floorLeft    float64 // what it may still take at its floor this tick, bytes
	bucket       float64 // latency-lane credit, bytes
	rtBucket     float64 // realtime-lane credit, bytes (LaneRT)
	// the demand's window: bulk bytes pushed, and the bytes the link served
	// while it had bulk queued, the tick before and this one; readyAt is
	// the clock when its bulk queue last became non-empty
	pushed       [2]float64
	ready        [2]uint64
	readyAt      uint64
	aPrev, aNext *schedUser[T] // all users
	// w: its weight at the latest tick (1, or 1/k with XShare: k the links
	// it is active on; 0 inactive); xe: its XShare entry
	w  float64
	xe *xshareEnt
}

// Sched is a link's scheduler.
type Sched[T any] struct {
	ctrl, lat schedFIFO[T]
	rt, rtS   schedFIFO[T] // LaneRT, LaneRTSteady
	users     map[uint32]*schedUser[T]
	flows     map[uint64]*schedFlow[T]
	all       *schedUser[T]
	// the floor list (circular) and its current user; the link's ring of
	// flows with bulk queued and its current flow; fNew, gNew: the current
	// one's turn has not begun (it gets its quantum when it does)
	floor      *schedUser[T]
	nFloor     int
	fNew       bool
	gCur       *schedFlow[T]
	gNew       bool
	idleHead   *schedFlow[T]
	idleTail   *schedFlow[T]
	bulkN      int
	quantum    int
	clock      uint64    // bytes taken, every lane
	clockAt    uint64    // ... at the latest tick
	served     [2]uint64 // bytes taken over the tick before and the latest
	dt         [2]time.Duration
	tick       int64
	lastNow    time.Duration
	ticked     bool
	rate       float64
	active     int
	servedLane [4]uint64
	demoted    uint64
	freeNode   *schedNode[T]
	freeFlow   *schedFlow[T]
	freeUser   *schedUser[T]
	// xs, xsLink: XShare's users and this link's id in it (nil: off)
	xs     *xshareSet
	xsLink int
}

// setXShare weighs each active user's floor by the links it is active on
// (xshare.go), this scheduler being link in set.
func (s *Sched[T]) setXShare(set *xshareSet, link int) { s.xs, s.xsLink = set, link }

// xsNote counts u active on this link at now and says on how many links it
// is (at least 1).
func (s *Sched[T]) xsNote(u *schedUser[T], now time.Duration) int {
	for range 2 {
		if u.xe == nil {
			u.xe = s.xs.ent(u.tag, int64(now))
		}
		if k := u.xe.note(s.xsLink, int64(now)); k > 0 {
			return k
		}
		u.xe = nil // swept meanwhile: the tag's entry again
	}
	return 1
}

// NewSched makes a link's scheduler.
func NewSched[T any]() *Sched[T] {
	return &Sched[T]{users: map[uint32]*schedUser[T]{}, flows: map[uint64]*schedFlow[T]{}, quantum: bulkFrameMin}
}

// Push queues it (size bytes) of a flow of a user in lane; a flow keeps its
// order whatever the lanes of its items, but control items go ahead of
// everything (the caller pushes none that must follow a flow's data).
func (s *Sched[T]) Push(it T, user uint32, flow uint64, lane Lane, size int) {
	n := s.node(it, size)
	if lane == LaneCtrl {
		s.ctrl.push(n)
		return
	}
	f := s.flowOf(user, flow)
	n.f = f
	u := f.u
	if lane == LaneRT || lane == LaneRTSteady {
		// ahead of the latency lane, while the flow has nothing queued in
		// it or in bulk (its order) and its user has realtime credit; else
		// as a latency-lane item. A flow's realtime items stay in one of
		// the two FIFOs while any is queued (its order again).
		if size <= rtFrameMax && f.q.n == 0 && f.lat == 0 && u.rtBucket >= float64(size) {
			u.rtBucket -= float64(size)
			if f.rt == 0 {
				f.rtSteady = lane == LaneRTSteady
			}
			f.rt++
			if f.rtSteady {
				s.rtS.push(n)
			} else {
				s.rt.push(n)
			}
			return
		}
		lane = LaneLatency
	}
	if lane == LaneLatency {
		if size <= LaneFrameMax && f.q.n == 0 && u.bucket >= float64(size) {
			u.bucket -= float64(size)
			f.lat++
			s.lat.push(n)
			return
		}
		s.demoted++
	}
	f.q.push(n)
	s.bulkN++
	u.pushed[1] += float64(size)
	if f.q.n == 1 {
		s.ringIn(f)
	}
	u.items++
	if u.items == 1 {
		u.readyAt = s.clock
		if u.floorLeft > 0 {
			s.floorIn(u)
		}
	}
}

// NextUrgent takes the next control or latency-lane item (false: none),
// leaving the bulk ones (a bulk frame waits at a BULK link's in-flight
// budget, budget.go: these go past it). O(1).
func (s *Sched[T]) NextUrgent() (it T, ok bool) {
	if n := s.ctrl.pop(); n != nil {
		return s.take(n, LaneCtrl), true
	}
	for _, q := range [2]*schedFIFO[T]{&s.rt, &s.rtS} {
		if n := q.pop(); n != nil {
			f := n.f
			f.rt--
			it = s.take(n, LaneRT)
			s.maybeIdle(f)
			return it, true
		}
	}
	if n := s.lat.pop(); n != nil {
		f := n.f
		f.lat--
		it = s.take(n, LaneLatency)
		s.maybeIdle(f)
		return it, true
	}
	return it, false
}

// Next takes the next item to write: control, then the latency lane, then
// the users under their floor, then the surplus by flows. O(1).
func (s *Sched[T]) Next() (it T, ok bool) {
	if it, ok := s.NextUrgent(); ok {
		return it, true
	}
	switch {
	case s.floor != nil:
		u := s.floor
		f := u.cur
		if s.fNew {
			u.fDef += max(s.quantum, f.q.head.size)
			s.fNew = false
		}
		if u.fDef < f.q.head.size {
			// its turn is over: the next user's begins
			u = u.fNext
			s.floor = u
			f = u.cur
			u.fDef += max(s.quantum, f.q.head.size)
		}
		size := f.q.head.size
		u.fDef -= size
		u.floorLeft -= float64(size)
		u.cur = f.uNext // its flows take turns
		it = s.takeBulk(f)
		if u.items == 0 || u.floorLeft <= 0 {
			s.floorOut(u)
		}
		return it, true
	case s.gCur != nil:
		f := s.gCur
		if s.gNew {
			f.gDef += max(s.quantum, f.q.head.size)
			s.gNew = false
		}
		if f.gDef < f.q.head.size {
			f = f.gNext
			s.gCur = f
			f.gDef += max(s.quantum, f.q.head.size)
		}
		f.gDef -= f.q.head.size
		return s.takeBulk(f), true
	}
	return it, false
}

// Tick moves the floors, the credit and the demand (each schedTick), C
// being linkRate (bytes/s; 0: what the link took over the window).
func (s *Sched[T]) Tick(now time.Duration, linkRate float64) {
	dt := schedTick
	if s.ticked {
		dt = max(now-s.lastNow, 0)
	}
	s.lastNow, s.ticked = now, true
	s.tick++
	s.served = [2]uint64{s.served[1], s.clock - s.clockAt}
	s.clockAt = s.clock
	s.dt = [2]time.Duration{s.dt[1], dt}
	win := (s.dt[0] + s.dt[1]).Seconds()
	took := float64(s.served[0] + s.served[1])
	c := linkRate
	if !(c > 0) && win > 0 {
		c = took / win
	}
	s.rate, s.quantum = c, BulkFrameSize(c)
	refill := schedLaneRate * dt.Seconds()
	rtRefill := float64(RTMax) / 8 * dt.Seconds()
	n := 0
	wsum := 0.0
	for u, next := s.all, (*schedUser[T])(nil); u != nil; u = next {
		next = u.aNext
		if u.items > 0 {
			u.ready[1] += s.clock - u.readyAt
			u.readyAt = s.clock
		}
		d := 0.0
		if win > 0 {
			d = schedDemandX * (u.pushed[0] + u.pushed[1]) / win
		}
		if took > 0 {
			d = max(d, float64(u.ready[0]+u.ready[1])/took*c)
		}
		u.demand = d
		u.w = 0
		if d > 0 || u.items > 0 {
			n++
			u.w = 1
			if s.xs != nil {
				u.w = 1 / float64(s.xsNote(u, now))
			}
			wsum += u.w
		} else if u.flows == 0 {
			// no flow left (freed below a tick before) and no demand
			s.dropUser(u)
			continue
		}
		u.pushed = [2]float64{u.pushed[1], 0}
		u.ready = [2]uint64{u.ready[1], 0}
		u.bucket = min(u.bucket+refill, schedLaneBurst)
		u.rtBucket = min(u.rtBucket+rtRefill, rtBurst)
	}
	s.active = n
	// each active user's share is C·w/Σw: C/N with every weight 1 (XShare
	// off, or every user on this link alone). The floor list keeps its order, its turn and its deficits from tick
	// to tick (rebuilt in a fixed order, the same user went first each tick
	// and the one after lost the frame that fell past it: 45% where 50% was
	// due, TestSchedNewHeavyUser at 10 Mbit/s); users that come in join last
	for u := s.all; u != nil; u = u.aNext {
		share := 0.0
		if wsum > 0 {
			share = c * u.w / wsum
		}
		u.floor = min(u.demand, share)
		u.floorLeft = u.floor * schedTick.Seconds()
		if u.items > 0 && u.floorLeft > 0 {
			s.floorIn(u)
		} else {
			s.floorOut(u)
		}
	}
	// free the flows idle since the tick before (their users go at the
	// next tick, once without demand)
	for f := s.idleHead; f != nil && f.idleAt < s.tick-1; f = s.idleHead {
		s.idleOut(f)
		delete(s.flows, f.id)
		f.u.flows--
		*f = schedFlow[T]{idleNext: s.freeFlow}
		s.freeFlow = f
	}
}

// Stats says what the scheduler holds and did (O(users)).
func (s *Sched[T]) Stats() SchedStats {
	st := SchedStats{Users: len(s.users), Active: s.active, Flows: len(s.flows),
		Pending: [4]int{s.ctrl.n, s.lat.n, s.bulkN, s.rt.n + s.rtS.n}, Served: s.servedLane, Demoted: s.demoted,
		FloorUsers: s.nFloor, Quantum: s.quantum, Rate: s.rate}
	for u := s.all; u != nil; u = u.aNext {
		if u.items > st.MaxUserItems {
			st.MaxUser, st.MaxUserItems = u.tag, u.items
		}
	}
	return st
}

// ---- internals ----

func (s *Sched[T]) node(it T, size int) *schedNode[T] {
	n := s.freeNode
	if n != nil {
		s.freeNode = n.next
	} else {
		n = new(schedNode[T])
	}
	n.it, n.size, n.f = it, size, nil
	return n
}

// take hands an item out and recycles its node.
func (s *Sched[T]) take(n *schedNode[T], lane Lane) T {
	s.clock += uint64(n.size)
	s.servedLane[lane] += uint64(n.size)
	it := n.it
	var zero T
	n.it, n.f, n.next = zero, nil, s.freeNode
	s.freeNode = n
	return it
}

// takeBulk takes the head of f's bulk queue.
func (s *Sched[T]) takeBulk(f *schedFlow[T]) T {
	u := f.u
	n := f.q.pop()
	s.bulkN--
	it := s.take(n, LaneBulk)
	u.items--
	if u.items == 0 {
		u.ready[1] += s.clock - u.readyAt
	}
	if f.q.n == 0 {
		s.ringOut(f)
		s.maybeIdle(f)
	}
	return it
}

func (s *Sched[T]) flowOf(user uint32, id uint64) *schedFlow[T] {
	if f := s.flows[id]; f != nil {
		if f.inIdle {
			s.idleOut(f)
		}
		return f
	}
	f := s.freeFlow
	if f == nil {
		slab := make([]schedFlow[T], schedSlab)
		for i := 1; i < len(slab); i++ {
			slab[i].idleNext = s.freeFlow
			s.freeFlow = &slab[i]
		}
		f = &slab[0]
	} else {
		s.freeFlow = f.idleNext
	}
	u := s.userOf(user)
	*f = schedFlow[T]{id: id, u: u}
	u.flows++
	s.flows[id] = f
	return f
}

func (s *Sched[T]) userOf(tag uint32) *schedUser[T] {
	if u := s.users[tag]; u != nil {
		return u
	}
	u := s.freeUser
	if u == nil {
		slab := make([]schedUser[T], schedSlab)
		for i := 1; i < len(slab); i++ {
			slab[i].aNext = s.freeUser
			s.freeUser = &slab[i]
		}
		u = &slab[0]
	} else {
		s.freeUser = u.aNext
	}
	*u = schedUser[T]{tag: tag, bucket: schedLaneBurst, rtBucket: rtBurst, aNext: s.all}
	if s.all != nil {
		s.all.aPrev = u
	}
	s.all = u
	s.users[tag] = u
	return u
}

func (s *Sched[T]) dropUser(u *schedUser[T]) {
	if u.aPrev != nil {
		u.aPrev.aNext = u.aNext
	} else {
		s.all = u.aNext
	}
	if u.aNext != nil {
		u.aNext.aPrev = u.aPrev
	}
	delete(s.users, u.tag)
	*u = schedUser[T]{aNext: s.freeUser}
	s.freeUser = u
}

// ringIn puts f, which now has bulk queued, last in its user's turn and in
// the link's.
func (s *Sched[T]) ringIn(f *schedFlow[T]) {
	u := f.u
	if c := u.cur; c == nil {
		u.cur, f.uPrev, f.uNext = f, f, f
	} else {
		f.uPrev, f.uNext = c.uPrev, c
		c.uPrev.uNext = f
		c.uPrev = f
	}
	f.gDef = 0
	if c := s.gCur; c == nil {
		s.gCur, f.gPrev, f.gNext, s.gNew = f, f, f, true
	} else {
		f.gPrev, f.gNext = c.gPrev, c
		c.gPrev.gNext = f
		c.gPrev = f
	}
}

// ringOut takes f, which has no bulk left, out of both rings.
func (s *Sched[T]) ringOut(f *schedFlow[T]) {
	u := f.u
	if f.uNext == f {
		u.cur = nil
	} else {
		f.uPrev.uNext, f.uNext.uPrev = f.uNext, f.uPrev
		if u.cur == f {
			u.cur = f.uNext
		}
	}
	if f.gNext == f {
		s.gCur = nil
	} else {
		f.gPrev.gNext, f.gNext.gPrev = f.gNext, f.gPrev
		if s.gCur == f {
			s.gCur, s.gNew = f.gNext, true
		}
	}
	f.uPrev, f.uNext, f.gPrev, f.gNext, f.gDef = nil, nil, nil, nil, 0
}

func (s *Sched[T]) floorIn(u *schedUser[T]) {
	if u.inFloor {
		return
	}
	u.inFloor, u.fDef = true, 0
	s.nFloor++
	if c := s.floor; c == nil {
		s.floor, u.fPrev, u.fNext, s.fNew = u, u, u, true
	} else {
		u.fPrev, u.fNext = c.fPrev, c
		c.fPrev.fNext = u
		c.fPrev = u
	}
}

func (s *Sched[T]) floorOut(u *schedUser[T]) {
	if !u.inFloor {
		return
	}
	s.nFloor--
	if u.fNext == u {
		s.floor = nil
	} else {
		u.fPrev.fNext, u.fNext.fPrev = u.fNext, u.fPrev
		if s.floor == u {
			s.floor, s.fNew = u.fNext, true
		}
	}
	u.inFloor, u.fPrev, u.fNext, u.fDef = false, nil, nil, 0
}

// maybeIdle puts f in the idle list once nothing of it is queued.
func (s *Sched[T]) maybeIdle(f *schedFlow[T]) {
	if f.q.n != 0 || f.lat != 0 || f.rt != 0 || f.inIdle {
		return
	}
	f.inIdle, f.idleAt, f.idleNext, f.idlePrev = true, s.tick, nil, s.idleTail
	if s.idleTail != nil {
		s.idleTail.idleNext = f
	} else {
		s.idleHead = f
	}
	s.idleTail = f
}

func (s *Sched[T]) idleOut(f *schedFlow[T]) {
	if f.idlePrev != nil {
		f.idlePrev.idleNext = f.idleNext
	} else {
		s.idleHead = f.idleNext
	}
	if f.idleNext != nil {
		f.idleNext.idlePrev = f.idlePrev
	} else {
		s.idleTail = f.idlePrev
	}
	f.inIdle, f.idlePrev, f.idleNext = false, nil, nil
}
