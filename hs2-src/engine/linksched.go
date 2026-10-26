package engine

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// linkSched is C2 wired into the engine, angle (b) of REDESIGN §5.1: "the
// scheduler in the engine, smux only a framer". One per link and end.
//
// The boundary (docs/architecture/wip/r1/angle-b.fa.md): smux builds every
// frame (a stream's data, SYN, FIN, UPD, the keepalive's NOP) and hands it
// to the link's linkSched (smux.FrameQueue: Push), never blocking; the
// stream's writer then waits for its frame's Done, smux's rule of one frame
// pending per stream. The link's single writer (run, in place of smux's
// sendLoop) takes the next item from the pure scheduler (Sched, sched.go)
// and writes it through the link's send chain (watchConn → meteredConn →
// shapedConn → C3's hostQ → TLS), taking the next one only once the write
// returned: no more than one frame past the socket's low-water mark. smux
// asks the frame size of a stream's next frame (FrameSize): LaneFrameMax
// for a light one, BulkFrameSize of the link's rate for a bulk one.
//
// What the engine adds around Sched:
//
//   - lanes: control frames in LaneCtrl (ahead of everything, no bound); a
//     stream's data by its class (C1, classifier.go: queueClass, from what
//     the stream queued here) in LaneLatency or LaneBulk, under its SYN's
//     user tag;
//   - the per-user bound of the stream path: at most lsUserPending frames of
//     a user in the scheduler; the writer of the next one waits in the
//     user's FIFO (its frame's Done comes later: backpressure, no drop) and
//     goes in as one of the user's frames leaves; no bound on the requests
//     as a whole (smux's 1024 of the old heap are gone);
//   - TUN (pushTun): the link's TUN stream (kindL3) carries packets of many
//     flows; each flow has its own queue here (tunFlowPkts packets and
//     tunFlowSojourn, tunUserPkts per user; past them the head of the
//     longest flow of that user goes), and one turn at a time in the SAME
//     scheduler as the link's streams, as a flow of its user: a light flow
//     (a ping: ICMP echo always) in the latency lane, ahead of the bulk.
//     Its frame is built when its turn comes (the packets that waited past
//     tunFlowSojourn dropped), counted against the TUN stream's window
//     (smux Stream.DataFrame);
//   - user_mem_share: what each user holds queued here (frames, packets) is
//     counted per user over the process (userMem); a user past its share
//     while memory is short is at its memory cap: its next stream frames
//     wait (one still goes) and its TUN flows drop from their longest;
//   - a bulk frame leaves its last partial record open for the next
//     frame's bytes (shapedConn.writeHold) instead of padding it, for
//     lsHoldWait at most when nothing is queued: smaller frames do not cost
//     the padding of more records (the records keep their sampled sizes).
//
// The runner (r1run.go) ticks it each schedTick: Sched's floors, credit and
// demand, the classes of the flows that queued, the TUN flows' upkeep.

const (
	// lsUserPending: a user's stream frames in the scheduler at most
	// (REDESIGN §3.2: 64 pending frames of the stream path per user).
	lsUserPending = 64
	// tunFlowPkts, tunFlowSojourn, tunUserPkts: a TUN flow's queue, in
	// packets and in time, and a user's (§3.2).
	tunFlowPkts    = 64
	tunFlowSojourn = 20 * time.Millisecond
	tunUserPkts    = 1024
	// tunFlowFor: a TUN flow's turn is in the scheduler under this id space
	// (a stream's is its sid).
	tunFlowFor = 1 << 32
	// lsFlowIdle, tunFlowIdle: a flow record with nothing queued for this
	// long is dropped (a stream's at its FIN, sooner).
	lsFlowIdle  = time.Minute
	tunFlowIdle = 10 * time.Second
	// lsSweep: the flow records are swept this often.
	lsSweep = time.Second
	// lsLaneRing: the latency lane's waits kept for the status line.
	lsLaneRing = 1024
	// lsHoldWait: a bulk frame's open last record (shapedConn.writeHold)
	// waits this long at most for the next frame's bytes, when nothing is
	// queued as its frame is written (a lone stream's next frame comes as
	// soon as its writer has its Done), then goes padded.
	lsHoldWait = time.Millisecond
)

// The process's share of memory per user (§3.2, user_mem_share): a floor,
// not a ceiling. A user holds more while memory is plentiful; only under
// pressure (the kernel's TCP memory, this server's or the peer's, or the
// scheduler queues of the process past UserMemPool) is a user past its share
// at its cap.
var (
	UserMemShare = 64 << 20
	UserMemPool  = 1 << 30
)

// userMem is a user's bytes queued in the process's link schedulers.
type userMem struct {
	tag   uint32
	bytes atomic.Int64
	refs  int // the links' user records (under umMu)
}

var (
	umMu    sync.Mutex
	umAll   = map[uint32]*userMem{}
	umTotal atomic.Int64
)

func userMemOf(tag uint32) *userMem {
	umMu.Lock()
	defer umMu.Unlock()
	m := umAll[tag]
	if m == nil {
		m = &userMem{tag: tag}
		umAll[tag] = m
	}
	m.refs++
	return m
}

func (m *userMem) release() {
	umMu.Lock()
	defer umMu.Unlock()
	if m.refs--; m.refs <= 0 {
		delete(umAll, m.tag)
		umTotal.Add(-m.bytes.Swap(0))
	}
}

func (m *userMem) add(n int) {
	m.bytes.Add(int64(n))
	umTotal.Add(int64(n))
}

// userMemShort says whether memory is short: a user past its share is then
// at its cap.
func userMemShort() bool { return memPressure() || umTotal.Load() > int64(UserMemPool) }

func (m *userMem) atCap() bool {
	return m.bytes.Load() > int64(UserMemShare) && userMemShort()
}

// usersAtMemCap counts the process's users at their memory cap.
func usersAtMemCap() int {
	if !userMemShort() {
		return 0
	}
	umMu.Lock()
	defer umMu.Unlock()
	n := 0
	for _, m := range umAll {
		if m.bytes.Load() > int64(UserMemShare) {
			n++
		}
	}
	return n
}

// lsClass is a flow's classification (C1): what it queued, its classifier.
type lsClass struct {
	stats  flowStats
	clf    flowClassifier
	cls    Class // class_tx at the latest tick
	tickAt int64 // ctrlNow of the latest tick (0: none)
	inAct  bool  // in the link's list of flows to tick
	lastAt int64 // ctrlNow when it last queued
}

// lsFlow is a stream of the link with data queued lately.
type lsFlow struct {
	lsClass
	sid uint32
	u   *lsUser
}

type tunPkt struct {
	b  *[]byte
	at int64 // ctrlNow when queued
}

// tunFlow is a TUN flow's queue on the link.
type tunFlow struct {
	lsClass
	id     uint32
	u      *lsUser
	q      []tunPkt
	head   int
	bytes  int  // packet bytes queued
	queued bool // its turn is in the scheduler
	echo   bool // ICMP or ICMPv6 echo: light whatever it sends
}

func (f *tunFlow) len() int { return len(f.q) - f.head }

func (f *tunFlow) pop() tunPkt {
	p := f.q[f.head]
	f.q[f.head] = tunPkt{}
	f.head++
	f.bytes -= len(*p.b)
	switch {
	case f.head == len(f.q):
		f.q, f.head = f.q[:0], 0
	case f.head >= 32 && 2*f.head >= len(f.q):
		n := copy(f.q, f.q[f.head:])
		clear(f.q[n:])
		f.q, f.head = f.q[:n], 0
	}
	return p
}

// lsUser is a user tag on the link.
type lsUser struct {
	tag     uint32
	um      *userMem
	pending int      // its stream frames in the scheduler
	wait    []lsItem // writers waiting for room (FIFO: wait[wHead:])
	wHead   int
	flows   int      // its lsFlow and tunFlow records
	tunPkts int      // its TUN packets queued
	fat     *tunFlow // its longest TUN flow, as last seen
	bytes   int      // what it holds queued on this link
}

func (u *lsUser) waiting() int { return len(u.wait) - u.wHead }

func (u *lsUser) mem(n int) {
	u.bytes += n
	u.um.add(n)
}

// lsItem is what the scheduler orders: a frame smux built, a TUN flow's
// turn (its frame built when taken), or a TUN keepalive.
type lsItem struct {
	f     *smux.OutFrame
	tf    *tunFlow
	ping  *l3Link
	u     *lsUser // a stream frame's user (its pending count)
	at    int64   // ctrlNow when queued
	light bool    // queued for the latency lane
	// a TUN flow's turn, taken: its frame's packets, its stream and link
	pkts int
	st   *smux.Stream
	tl   *l3Link
}

// lsTun is the link's TUN side channel (one kindL3 stream).
type lsTun struct {
	l     *l3Link
	st    *smux.Stream
	pool  *sync.Pool
	drops *atomic.Uint64 // the l3Set's
	flows map[uint32]*tunFlow
}

// frameWriter is the link's send chain as the writer uses it (watchConn).
type frameWriter interface {
	Write(p []byte) (int, error)
}

// holdWriter: a send chain that can leave a write's last partial record
// open for the next write (shapedConn.writeHold), and flush it.
type holdWriter interface {
	writeHold(p []byte, hold bool) (int, error)
	flushHeld() error
}

// linkSched is one link's (see above).
type linkSched struct {
	mu      sync.Mutex
	s       *Sched[lsItem]
	n       int // items in s
	users   map[uint32]*lsUser
	flows   map[uint32]*lsFlow
	act     []*lsClass // flows that queued since the latest tick
	tun     lsTun
	frame   int // the bulk frame (BulkFrameSize of the link's rate)
	closed  bool
	idle    bool // the writer waits for wake
	wake    chan struct{}
	sess    *smux.Session
	out     frameWriter
	hw      holdWriter // out's, nil: none
	sweepAt int64
	// the status line's
	lane       [lsLaneRing]int32 // latency-lane waits, µs (ring)
	laneN      int
	drops      [2]uint64 // TUN packets dropped: light, bulk
	blocked    uint64    // writers that waited for their user's room
	flips      [2]uint64 // class_tx flips: light→bulk, bulk→light
	frameAt    atomic.Int32
	statsUsers atomic.Int32
	statsAct   atomic.Int32
	statsWait  atomic.Int32
	statsCap   atomic.Int32
}

// newLinkSched is the scheduler of a link whose send chain is out.
func newLinkSched(out frameWriter) *linkSched {
	ls := &linkSched{s: NewSched[lsItem](), users: map[uint32]*lsUser{}, flows: map[uint32]*lsFlow{},
		frame: bulkFrameMin, wake: make(chan struct{}, 1), out: out}
	ls.tun.flows = map[uint32]*tunFlow{}
	ls.hw, _ = out.(holdWriter)
	ls.frameAt.Store(bulkFrameMin)
	return ls
}

// start runs the link's writer for sess (the session ls was given to).
func (ls *linkSched) start(sess *smux.Session) {
	ls.sess = sess
	go ls.run()
}

// Push takes a frame smux built (smux.FrameQueue).
func (ls *linkSched) Push(f *smux.OutFrame) {
	now := ctrlNow()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		f.Drop()
		return
	}
	if f.Control() {
		if f.Fin() {
			ls.forget(f.Sid())
		}
		ls.s.Push(lsItem{f: f, at: now}, 0, 0, LaneCtrl, f.Len())
		ls.n++
		ls.kick()
		return
	}
	user, _ := f.User()
	fl := ls.flowOf(f.Sid(), user, now)
	light := queueClass(fl.cls, &fl.stats) != ClassBulk
	fl.stats.Add(true, f.DataLen())
	ls.queued(&fl.lsClass, now)
	u := fl.u
	it := lsItem{f: f, u: u, at: now, light: light}
	u.mem(f.Len())
	if u.pending >= lsUserPending || u.pending > 0 && u.um.atCap() {
		u.wait = append(u.wait, it)
		ls.blocked++
		return
	}
	ls.admit(it)
	ls.kick()
}

// FrameSize is the largest next data frame of stream sid (smux.FrameQueue):
// LaneFrameMax while it is light (or not judged yet, within its first
// classNewBytes), the link's bulk frame once it is bulk.
func (ls *linkSched) FrameSize(sid uint32) int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if fl := ls.flows[sid]; fl != nil && queueClass(fl.cls, &fl.stats) == ClassBulk {
		return ls.frame
	}
	return LaneFrameMax
}

// admit puts a stream frame in the scheduler.
func (ls *linkSched) admit(it lsItem) {
	it.u.pending++
	lane := LaneBulk
	if it.light {
		lane = LaneLatency
	}
	ls.s.Push(it, it.u.tag, uint64(it.f.Sid()), lane, it.f.Len())
	ls.n++
}

// kick wakes the writer if it waits.
func (ls *linkSched) kick() {
	if ls.idle {
		ls.idle = false
		select {
		case ls.wake <- struct{}{}:
		default:
		}
	}
}

func (ls *linkSched) userOf(tag uint32) *lsUser {
	u := ls.users[tag]
	if u == nil {
		u = &lsUser{tag: tag, um: userMemOf(tag)}
		ls.users[tag] = u
	}
	return u
}

func (ls *linkSched) flowOf(sid, user uint32, now int64) *lsFlow {
	fl := ls.flows[sid]
	if fl == nil {
		fl = &lsFlow{sid: sid, u: ls.userOf(user)}
		fl.u.flows++
		ls.flows[sid] = fl
	}
	return fl
}

// forget drops the record of a stream that is closing (its FIN).
func (ls *linkSched) forget(sid uint32) {
	if fl := ls.flows[sid]; fl != nil {
		delete(ls.flows, sid)
		fl.u.flows--
	}
}

// queued notes that a flow queued at now: it is ticked at the next tick,
// and at once if its latest tick is a tick old or more (a flow back from a
// silence is judged on it before its frame takes a lane).
func (ls *linkSched) queued(c *lsClass, now int64) {
	if c.tickAt != 0 && time.Duration(now-c.tickAt) >= classTick {
		ls.classTick(c, now)
	}
	c.lastAt = now
	if !c.inAct {
		c.inAct = true
		ls.act = append(ls.act, c)
	}
}

func (ls *linkSched) classTick(c *lsClass, now int64) {
	was := c.cls
	c.cls, _, _ = c.clf.Tick(&c.stats, time.Duration(now))
	c.tickAt = now
	switch {
	case was == ClassLight && c.cls == ClassBulk:
		ls.flips[0]++
	case was == ClassBulk && c.cls == ClassLight:
		ls.flips[1]++
	}
}

// ---- the writer ----

// run is the link's single writer: the next item of the scheduler, written
// whole before the next is taken.
func (ls *linkSched) run() {
	var buf, tb []byte
	die := ls.sess.CloseChan()
	held := false
	holdT := time.NewTimer(lsHoldWait)
	holdT.Stop()
	for {
		ls.mu.Lock()
		it, ok := ls.next(&tb)
		if !ok {
			ls.idle = true
		}
		ls.mu.Unlock()
		if !ok {
			if held {
				// a stream's next frame comes as soon as its writer has
				// its Done: the open record waits for it that long at most
				holdT.Reset(lsHoldWait)
				select {
				case <-ls.wake:
					holdT.Stop()
					continue
				case <-die:
					ls.close()
					return
				case <-holdT.C:
				}
				held = false
				if err := ls.hw.flushHeld(); err != nil {
					ls.close()
					return
				}
			}
			select {
			case <-ls.wake:
			case <-die:
				ls.close()
				return
			}
			continue
		}
		f := it.f
		switch {
		case it.tf != nil:
			if f, ok = it.st.DataFrame(tb); !ok {
				ls.mu.Lock()
				ls.dropBuilt(it.pkts, it.light)
				ls.mu.Unlock()
				continue
			}
		case it.ping != nil:
			p := tlscarrier.AppendTunFrame(tb[:0], core.TypePing, 0, 0, make([]byte, core.KeepalivePad()))
			if f, ok = it.ping.st.DataFrame(p); !ok {
				continue
			}
		}
		buf = f.AppendTo(buf[:0])
		// a bulk frame leaves its last record open for the next frame's
		// bytes (for lsHoldWait at most, when none is queued); a light or
		// control one goes whole
		hold := ls.hw != nil && !it.light && !f.Control() && it.ping == nil
		var n int
		var err error
		if ls.hw != nil {
			n, err = ls.hw.writeHold(buf, hold)
			held = hold
		} else {
			n, err = ls.out.Write(buf)
		}
		f.Done(n, err)
		now := ctrlNow()
		if it.ping != nil {
			it.ping.lastTx.Store(now)
		} else if it.tl != nil {
			it.tl.lastTx.Store(now)
		}
		ls.mu.Lock()
		if it.u != nil {
			it.u.mem(-f.Len())
		}
		if it.light {
			ls.lane[ls.laneN%lsLaneRing] = int32(min((now-it.at)/1e3, 1<<31-1))
			ls.laneN++
		}
		ls.mu.Unlock()
		if err != nil {
			ls.close()
			return
		}
	}
}

// next takes the next item (the caller holds mu): a TUN flow's turn has its
// frame built into tb (none left: the next item).
func (ls *linkSched) next(tb *[]byte) (lsItem, bool) {
	for {
		it, ok := ls.s.Next()
		if !ok {
			return it, false
		}
		ls.n--
		switch {
		case it.u != nil:
			u := it.u
			u.pending--
			if u.waiting() > 0 && (u.pending == 0 || !u.um.atCap()) {
				w := u.wait[u.wHead]
				u.wait[u.wHead] = lsItem{}
				u.wHead++
				if u.wHead == len(u.wait) {
					u.wait, u.wHead = u.wait[:0], 0
				}
				ls.admit(w)
			}
		case it.tf != nil:
			at, pkts := ls.buildTun(it.tf, tb)
			if pkts == 0 {
				continue
			}
			it.at, it.pkts, it.st, it.tl = at, pkts, ls.tun.st, ls.tun.l
		}
		return it, true
	}
}

// close ends the writer: everything queued is let go.
func (ls *linkSched) close() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return
	}
	ls.closed = true
	for {
		it, ok := ls.s.Next()
		if !ok {
			break
		}
		if it.f != nil {
			it.f.Drop()
		}
		if it.u != nil {
			it.u.mem(-it.f.Len())
		}
	}
	ls.n = 0
	for _, u := range ls.users {
		for _, w := range u.wait[u.wHead:] {
			w.f.Drop()
			u.mem(-w.f.Len())
		}
		u.wait, u.wHead = nil, 0
	}
	ls.purgeTun()
	for tag, u := range ls.users {
		u.um.add(-u.bytes)
		u.bytes = 0
		u.um.release()
		delete(ls.users, tag)
	}
}

// ---- TUN ----

// attachTun makes l, the link's TUN stream, the one TUN packets of this
// link go on.
func (ls *linkSched) attachTun(l *l3Link, pool *sync.Pool, drops *atomic.Uint64) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.tun.l != nil && ls.tun.l != l {
		ls.purgeTun()
	}
	ls.tun.l, ls.tun.st, ls.tun.pool, ls.tun.drops = l, l.st, pool, drops
}

// detachTun: l, the link's TUN stream, is gone.
func (ls *linkSched) detachTun(l *l3Link) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.tun.l == l {
		ls.purgeTun()
		ls.tun.l, ls.tun.st = nil, nil
	}
}

// purgeTun drops every TUN packet queued (the caller holds mu).
func (ls *linkSched) purgeTun() {
	for id, f := range ls.tun.flows {
		for f.len() > 0 {
			ls.dropTun(f, f.cls != ClassBulk || f.echo)
		}
		f.queued = false
		f.u.flows--
		if f.u.fat == f {
			f.u.fat = nil
		}
		delete(ls.tun.flows, id)
	}
}

// pushTun queues a TUN packet of flow (user tag tag) on the link; false: the
// link takes none (no TUN stream), the caller drops it.
func (ls *linkSched) pushTun(l *l3Link, bp *[]byte, tag, flow uint32) bool {
	now := ctrlNow()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed || ls.tun.l != l || ls.tun.st == nil {
		return false
	}
	f := ls.tun.flows[flow]
	if f == nil {
		f = &tunFlow{id: flow, u: ls.userOf(tag), echo: tunEcho(*bp)}
		f.u.flows++
		ls.tun.flows[flow] = f
	}
	u := f.u
	ls.queued(&f.lsClass, now)
	f.stats.Add(true, len(*bp))
	switch {
	case f.len() >= tunFlowPkts:
		ls.dropTun(f, ls.tunLight(f))
	case u.tunPkts >= tunUserPkts || u.um.atCap() && u.tunPkts > 0:
		fat := u.fat
		if fat == nil || fat.len() < f.len() {
			fat = f
		}
		if fat.len() > 0 {
			ls.dropTun(fat, ls.tunLight(fat))
		}
	}
	f.q = append(f.q, tunPkt{bp, now})
	f.bytes += len(*bp)
	u.tunPkts++
	u.mem(len(*bp) + tunHdr)
	if u.fat == nil || u.fat.len() < f.len() {
		u.fat = f
	}
	if !f.queued {
		ls.tunTurn(f, now)
	}
	ls.kick()
	return true
}

const tunHdr = 15 // the TUN frame's descriptor (tlscarrier.AppendTunFrame)

func (ls *linkSched) tunLight(f *tunFlow) bool {
	return f.echo || queueClass(f.cls, &f.stats) != ClassBulk
}

// tunTurn puts f's turn in the scheduler: its frame is built when taken.
func (ls *linkSched) tunTurn(f *tunFlow, now int64) {
	light := ls.tunLight(f)
	lane, fmax := LaneBulk, ls.frame
	if light {
		lane, fmax = LaneLatency, LaneFrameMax
	}
	size := min(f.bytes+tunHdr*f.len(), fmax)
	ls.s.Push(lsItem{tf: f, at: now, light: light}, f.u.tag, tunFlowFor|uint64(f.id), lane, size)
	f.queued = true
	ls.n++
}

// buildTun builds f's frame into tb as its turn comes (the caller holds
// mu): its packets in order up to its lane's frame, and within what the
// TUN stream's window has left; those that waited past tunFlowSojourn are
// dropped. It gives the oldest packet's time and the packets in the frame;
// 0: nothing to send (all stale, the stream gone, or no window: its packets
// wait for the next packet or sweep).
func (ls *linkSched) buildTun(f *tunFlow, tb *[]byte) (at int64, pkts int) {
	f.queued = false
	light := ls.tunLight(f)
	st := ls.tun.st
	if st == nil || ls.tun.flows[f.id] != f {
		return 0, 0
	}
	now := ctrlNow()
	fmax := ls.frame
	if light {
		fmax = LaneFrameMax
	}
	room := st.Room()
	b := (*tb)[:0]
	for f.len() > 0 {
		p := f.q[f.head]
		if time.Duration(now-p.at) > tunFlowSojourn {
			ls.dropTun(f, light)
			continue
		}
		need := tunHdr + len(*p.b)
		if need > room-len(b) || len(b) > 0 && len(b)+need > fmax {
			break
		}
		if pkts == 0 {
			at = p.at
		}
		b = tlscarrier.AppendTunFrame(b, core.TypeData, f.u.tag, f.id, *p.b)
		f.pop()
		pkts++
		f.u.tunPkts--
		f.u.mem(-(len(*p.b) + tunHdr))
		ls.tun.pool.Put(p.b)
	}
	*tb = b
	if f.len() > 0 && pkts > 0 {
		ls.tunTurn(f, now)
	}
	return at, pkts
}

// dropBuilt: a frame of pkts packets built for a flow could not go (its
// stream ended or its window shrank meanwhile): they count as dropped.
func (ls *linkSched) dropBuilt(pkts int, light bool) {
	ls.drops[b2i(!light)] += uint64(pkts)
	if ls.tun.drops != nil {
		ls.tun.drops.Add(uint64(pkts))
	}
}

// dropTun drops the head of f's queue (the caller holds mu).
func (ls *linkSched) dropTun(f *tunFlow, light bool) {
	p := f.pop()
	f.u.tunPkts--
	f.u.mem(-(len(*p.b) + tunHdr))
	if ls.tun.pool != nil {
		ls.tun.pool.Put(p.b)
	}
	ls.drops[b2i(!light)]++
	if ls.tun.drops != nil {
		ls.tun.drops.Add(1)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pushTunPing queues l's keepalive in the control lane.
func (ls *linkSched) pushTunPing(l *l3Link) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed || ls.tun.l != l {
		return
	}
	ls.s.Push(lsItem{ping: l, at: ctrlNow()}, 0, 0, LaneCtrl, smux.HeaderSize+tunHdr+48)
	ls.n++
	ls.kick()
}

// tunEcho says whether a packet is an ICMP or ICMPv6 echo request or reply
// (always light, §3.1).
func tunEcho(pkt []byte) bool {
	switch {
	case len(pkt) >= 20 && pkt[0]>>4 == 4 && pkt[9] == 1:
		ihl := int(pkt[0]&0x0f) * 4
		return len(pkt) > ihl && (pkt[ihl] == 8 || pkt[ihl] == 0)
	case len(pkt) >= 40 && pkt[0]>>4 == 6:
		proto, off := ipv6Transport(pkt)
		if off == 0 {
			off = 40 // the walk gave up: a plain next header still reads
		}
		return proto == 58 && len(pkt) > off && (pkt[off] == 128 || pkt[off] == 129)
	}
	return false
}

// ---- the tick ----

// tick moves the scheduler each schedTick (the runner): Sched's floors,
// credit and demand at the link's rate (bytes/s; 0: the scheduler's own
// count), the classes of the flows that queued, and each lsSweep the flow
// records' upkeep.
func (ls *linkSched) tick(now int64, rate float64) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return
	}
	ls.s.Tick(time.Duration(now), rate)
	st := ls.s.Stats()
	ls.frame = st.Quantum
	ls.frameAt.Store(int32(st.Quantum))
	for _, c := range ls.act {
		c.inAct = false
		if c.tickAt != now {
			ls.classTick(c, now)
		}
	}
	clear(ls.act)
	ls.act = ls.act[:0]
	if time.Duration(now-ls.sweepAt) >= lsSweep {
		ls.sweepAt = now
		ls.sweep(now)
	}
	wait, atCap := 0, 0
	for _, u := range ls.users {
		wait += u.waiting()
		if u.bytes > 0 && u.um.atCap() {
			atCap++
		}
	}
	ls.statsUsers.Store(int32(st.Users))
	ls.statsAct.Store(int32(st.Active))
	ls.statsWait.Store(int32(wait))
	ls.statsCap.Store(int32(atCap))
}

// sweep drops the idle flow records and users, and gives the TUN flows
// left without a turn (their stream's window was full) one again.
func (ls *linkSched) sweep(now int64) {
	for sid, fl := range ls.flows {
		if time.Duration(now-fl.lastAt) >= lsFlowIdle {
			delete(ls.flows, sid)
			fl.u.flows--
		}
	}
	for id, f := range ls.tun.flows {
		for f.len() > 0 && time.Duration(now-f.q[f.head].at) > tunFlowSojourn {
			ls.dropTun(f, ls.tunLight(f))
		}
		switch {
		case f.len() > 0 && !f.queued:
			ls.tunTurn(f, now)
		case f.len() == 0 && !f.queued && time.Duration(now-f.lastAt) >= tunFlowIdle:
			delete(ls.tun.flows, id)
			f.u.flows--
			if f.u.fat == f {
				f.u.fat = nil
			}
		}
	}
	for tag, u := range ls.users {
		if u.flows == 0 && u.pending == 0 && u.waiting() == 0 && u.tunPkts == 0 {
			u.um.add(-u.bytes)
			u.um.release()
			delete(ls.users, tag)
		}
	}
	if ls.n > 0 {
		ls.kick()
	}
}

// ---- status ----

// lanePct is the latency lane's p50 and p99 wait over its latest frames
// (ms; ok false: none yet).
func (ls *linkSched) lanePct() (p50, p99 float64, ok bool) {
	ls.mu.Lock()
	n := min(ls.laneN, lsLaneRing)
	v := make([]int32, n)
	copy(v, ls.lane[:n])
	ls.mu.Unlock()
	if n == 0 {
		return 0, 0, false
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return float64(v[n/2]) / 1e3, float64(v[min(n*99/100, n-1)]) / 1e3, true
}

// line is the link's status line (§3.2).
func (ls *linkSched) line(n int) string {
	lane := "lane p50/p99 -/- ms"
	if p50, p99, ok := ls.lanePct(); ok {
		lane = fmt.Sprintf("lane p50/p99 %.1f/%.1f ms", p50, p99)
	}
	ls.mu.Lock()
	drops := ls.drops
	ls.mu.Unlock()
	return fmt.Sprintf("sched link %d: %s, bulk frame %.1f KiB, users %d (active %d), drops light/bulk %d/%d, blocked-writers %d, users at memory cap %d",
		n, lane, float64(ls.frameAt.Load())/1024, ls.statsUsers.Load(), ls.statsAct.Load(), drops[0], drops[1], ls.statsWait.Load(), ls.statsCap.Load())
}

// classes counts the link's flows by class_tx, and its flips so far.
func (ls *linkSched) classes() (light, bulk, unknown int, flips [2]uint64) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	count := func(c *lsClass, echo bool) {
		switch {
		case echo || c.cls == ClassLight:
			light++
		case c.cls == ClassBulk:
			bulk++
		default:
			unknown++
		}
	}
	for _, f := range ls.flows {
		count(&f.lsClass, false)
	}
	for _, f := range ls.tun.flows {
		count(&f.lsClass, f.echo)
	}
	return light, bulk, unknown, ls.flips
}
