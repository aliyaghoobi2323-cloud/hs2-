package engine

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// The TUN side channel of a stream-mode link with C2 (angle A; REDESIGN
// §3.2, "the TUN path"): in place of the link's FIFO of 256 packets, each
// 5-tuple flow (flowKey) has a queue of its own on the link, at most
// tunFlowPkts packets and tunFlowWait old, and each user at most
// tunUserPkts packets on the link. The flows go to the link's own smux
// scheduler, the one its user streams go to, through its kindL3 stream
// (smux.Stream.WriteFlowAsync): one frame of a flow with smux at a time,
// built when its turn comes from what the flow has queued (its packets
// coalesced up to the frame its lane takes: one latency-lane frame, or the
// link's bulk frame), each frame under the flow's user tag. So a ping (ICMP
// echo is always light) goes in the latency lane ahead of the TUN's bulk
// and the user streams' bulk, and a TUN bulk flow shares its user's floor
// with the user's port streams.
//
// Overflow drops a packet that is queued, never blocks the TUN's reader: a
// flow past its bound loses its oldest packet, a user past its bound the
// oldest of its longest flow, a link past tunLinkPkts the oldest of the
// longest flow of its fattest user (dgfq's choice: the flow that filled the
// queue loses, not the ping that arrived last); a packet older than
// tunFlowWait when its frame is built is dropped (the inner TCP reads the
// drops as congestion). Its bytes count with its user's user_mem_share,
// which under memory pressure turns away a user past its share.
//
// HS2_TUNE_SCHED=0 (or a link without the caps bit) keeps the FIFO,
// writeLoop and l3MaxSojourn.

const (
	tunFlowPkts = 64
	tunFlowWait = 20 * time.Millisecond
	tunUserPkts = 1024
	// tunLinkPkts: what one link holds of all its users (this design's
	// choice, about 6 MB of full packets): past it the fattest user loses.
	tunLinkPkts = 4096
	// tunFlowIdle: a flow with nothing queued this long is forgotten.
	tunFlowIdle = 2 * time.Second
	// tunFlowBase: a TUN flow's id in the link's scheduler is past every
	// stream id; tunKeepFlow is the keepalive's.
	tunFlowBase = 1 << 32
	tunKeepFlow = 1 << 33
)

type tunPkt struct {
	b  *[]byte
	at int64 // ctrlNow when queued
}

// tunFlow is one 5-tuple flow's queue on a link.
type tunFlow struct {
	key    uint32
	u      *tunUser
	sq     *tunSched
	q      []tunPkt // FIFO: q[head:]
	head   int
	busy   bool // a frame of it is with smux
	ready  bool // in the ready list
	pend   bool // frame holds a frame smux has not taken yet (the window was full)
	echo   bool // ICMP echo: always light
	stats  flowStats
	cls    flowClassifier
	class  Class
	usedAt int64
	next   *tunFlow // the ready list
	frame  []byte
	doneFn func(error)
}

func (f *tunFlow) n() int { return len(f.q) - f.head }

// lane is the lane of the flow's next frame.
func (f *tunFlow) lane() uint8 {
	if f.echo {
		return smux.LaneLatency
	}
	return laneOf(queueClass(f.class, &f.stats))
}

// tunUser is a user's flows on the link.
type tunUser struct {
	tag   uint32
	pkts  int
	flows map[*tunFlow]struct{}
	acct  *userMemAcct
}

// longest is the user's flow with the most queued.
func (u *tunUser) longest() *tunFlow {
	var best *tunFlow
	for f := range u.flows {
		if best == nil || f.n() > best.n() {
			best = f
		}
	}
	return best
}

// tunSched is a link's TUN flows; its run loop hands their frames to smux.
type tunSched struct {
	mu         sync.Mutex
	st         *smux.Stream
	pool       *sync.Pool     // packet buffers (the set's)
	setDrops   *atomic.Uint64 // the set's drop count (logDrops)
	flows      map[uint32]*tunFlow
	users      map[uint32]*tunUser
	pkts       int
	head, tail *tunFlow // flows ready for a frame, in turn
	closed     bool
	kick       chan struct{}
	// drops by the lane of their flow (light, bulk), and what the status
	// line saw of them
	drops    [2]atomic.Uint64
	dropSeen [2]uint64
	ka       []byte
	kaBusy   atomic.Bool
}

func newTunSched(st *smux.Stream) *tunSched {
	if QuietCoalTun != 0 && st != nil {
		st.SetUrgent(true) // no coalescing hold after a TUN frame (quiet.go)
	}
	return &tunSched{st: st, flows: map[uint32]*tunFlow{}, users: map[uint32]*tunUser{}, kick: make(chan struct{}, 1),
		pool: &sync.Pool{}, setDrops: new(atomic.Uint64)}
}

// attach takes the set's packet pool and drop count (l3Set.add).
func (sq *tunSched) attach(pool *sync.Pool, drops *atomic.Uint64) {
	sq.mu.Lock()
	sq.pool, sq.setDrops = pool, drops
	sq.mu.Unlock()
}

func (sq *tunSched) signal() {
	select {
	case sq.kick <- struct{}{}:
	default:
	}
}

// push queues a packet of the flow key of user tag (echo: an ICMP echo).
func (sq *tunSched) push(bp *[]byte, tag, key uint32, echo bool) {
	now := ctrlNow()
	n := len(*bp)
	sq.mu.Lock()
	if sq.closed {
		sq.mu.Unlock()
		sq.pool.Put(bp)
		sq.setDrops.Add(1)
		return
	}
	f := sq.flows[key]
	if f == nil {
		u := sq.users[tag]
		if u == nil {
			u = &tunUser{tag: tag, flows: map[*tunFlow]struct{}{}, acct: userMems.acct(tag)}
			sq.users[tag] = u
		}
		f = &tunFlow{key: key, u: u, sq: sq}
		f.doneFn = f.written
		u.flows[f] = struct{}{}
		sq.flows[key] = f
	}
	f.usedAt = now
	f.echo = f.echo || echo
	u := f.u
	if !u.acct.take(n, u.pkts == 0) {
		sq.count(f) // over its share under pressure: the newcomer goes
		sq.mu.Unlock()
		sq.pool.Put(bp)
		return
	}
	switch {
	case f.n() >= tunFlowPkts:
		sq.dropHead(f)
	case u.pkts >= tunUserPkts:
		sq.dropHead(u.longest())
	case sq.pkts >= tunLinkPkts:
		sq.dropHead(sq.fattest().longest())
	}
	if f.head > 0 && f.head == len(f.q) {
		f.q, f.head = f.q[:0], 0
	}
	f.q = append(f.q, tunPkt{bp, now})
	u.pkts++
	sq.pkts++
	f.stats.tx.Add(uint64(n))
	k := false
	if !f.busy && !f.ready {
		sq.readyIn(f, false)
		k = true
	}
	sq.mu.Unlock()
	if k {
		sq.signal()
	}
}

// fattest is the user with the most queued on the link (under mu).
func (sq *tunSched) fattest() *tunUser {
	var best *tunUser
	for _, u := range sq.users {
		if best == nil || u.pkts > best.pkts {
			best = u
		}
	}
	return best
}

// count counts a drop of f's.
func (sq *tunSched) count(f *tunFlow) {
	i := 0
	if f.lane() == smux.LaneBulk {
		i = 1
	}
	sq.drops[i].Add(1)
	sq.setDrops.Add(1)
}

// pop takes f's oldest packet off its queue (under mu).
func (sq *tunSched) pop(f *tunFlow) tunPkt {
	p := f.q[f.head]
	f.q[f.head] = tunPkt{}
	f.head++
	switch {
	case f.head == len(f.q):
		f.q, f.head = f.q[:0], 0
	case f.head >= 64 && 2*f.head >= len(f.q):
		k := copy(f.q, f.q[f.head:])
		clear(f.q[k:])
		f.q, f.head = f.q[:k], 0
	}
	f.u.pkts--
	sq.pkts--
	f.u.acct.take(-len(*p.b), false)
	return p
}

// dropHead drops f's oldest packet (under mu).
func (sq *tunSched) dropHead(f *tunFlow) {
	if f == nil || f.n() == 0 {
		return
	}
	p := sq.pop(f)
	sq.count(f)
	sq.pool.Put(p.b)
}

// readyIn puts f in the ready list, at its end or its front.
func (sq *tunSched) readyIn(f *tunFlow, front bool) {
	f.ready = true
	switch {
	case sq.head == nil:
		sq.head, sq.tail, f.next = f, f, nil
	case front:
		f.next, sq.head = sq.head, f
	default:
		f.next = nil
		sq.tail.next, sq.tail = f, f
	}
}

func (sq *tunSched) readyOut() *tunFlow {
	f := sq.head
	if f == nil {
		return nil
	}
	sq.head = f.next
	if sq.head == nil {
		sq.tail = nil
	}
	f.next, f.ready = nil, false
	return f
}

// limit is what a frame in lane carries at most.
func (sq *tunSched) limit(lane uint8) int {
	if lane == smux.LaneLatency {
		return laneData
	}
	if b := sq.st.Session().BulkFrame(); b > 0 {
		return b
	}
	return bulkFrameMin
}

// build makes f's next frame from its queue, up to lim bytes (one packet at
// least), dropping what waited past tunFlowWait (under mu).
func (sq *tunSched) build(f *tunFlow, lim int, now int64) []byte {
	b := f.frame[:0]
	for f.n() > 0 {
		p := f.q[f.head]
		if now-p.at > int64(tunFlowWait) {
			sq.dropHead(f)
			continue
		}
		if len(b) > 0 && len(b)+15+len(*p.b) > lim {
			break
		}
		b = tlscarrier.AppendTunFrame(b, core.TypeData, f.u.tag, f.key, *p.b)
		sq.pop(f)
		sq.pool.Put(p.b)
	}
	f.frame = b
	return b
}

// next is the next flow's frame for smux, and its lane (nil: none ready).
func (sq *tunSched) next() (*tunFlow, []byte, uint8) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	now := ctrlNow()
	for {
		f := sq.readyOut()
		if f == nil {
			return nil, nil, 0
		}
		lane := f.lane()
		if !f.pend && len(sq.build(f, sq.limit(lane), now)) == 0 {
			continue // all of it waited too long
		}
		f.busy, f.pend = true, false
		return f, f.frame, lane
	}
}

// written is smux's done for f's frame (the session's writer: brief).
func (f *tunFlow) written(err error) {
	sq := f.sq
	sq.mu.Lock()
	f.busy = false
	k := err != nil
	if !sq.closed && (f.n() > 0 || f.pend) && !f.ready {
		sq.readyIn(f, false)
		k = true
	}
	sq.mu.Unlock()
	if k {
		sq.signal()
	}
}

// held: smux had no window for f's frame; it goes first once it has.
func (sq *tunSched) held(f *tunFlow) {
	sq.mu.Lock()
	f.busy, f.pend = false, true
	if !f.ready {
		sq.readyIn(f, true)
	}
	sq.mu.Unlock()
}

// tick judges the flows (C1, each classTick) and forgets the idle ones.
func (sq *tunSched) tick(now int64) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	for key, f := range sq.flows {
		f.class, _, _ = f.cls.Tick(&f.stats, time.Duration(now))
		if f.n() == 0 && !f.busy && !f.ready && !f.pend && now-f.usedAt > int64(tunFlowIdle) {
			delete(sq.flows, key)
			delete(f.u.flows, f)
			if len(f.u.flows) == 0 {
				delete(sq.users, f.u.tag)
			}
		}
	}
}

// close: the link is gone; what is queued is dropped (its bytes leave its
// users' accounts).
func (sq *tunSched) close() {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	sq.closed = true
	for _, f := range sq.flows {
		for f.n() > 0 {
			sq.dropHead(f)
		}
	}
	sq.head, sq.tail = nil, nil
}

// dropsLine is the drops by lane since the previous call (the status line).
func (sq *tunSched) dropsLine() (light, bulk uint64) {
	l, b := sq.drops[0].Load(), sq.drops[1].Load()
	light, bulk = l-sq.dropSeen[0], b-sq.dropSeen[1]
	sq.dropSeen = [2]uint64{l, b}
	return light, bulk
}

// run hands the flows' frames to smux as their turns come, ticks C1, and
// sends the keepalive when the link has been idle (writeLoop's).
func (sq *tunSched) run(l *l3Link) {
	defer sq.close()
	tick := time.NewTicker(classTick)
	defer tick.Stop()
	idle := time.NewTimer(l.keepalive())
	defer idle.Stop()
	ka := false
	for {
		select {
		case <-l.done:
			return
		case <-sq.kick:
		case <-sq.st.WindowNotify():
		case <-tick.C:
			sq.tick(ctrlNow())
		case <-idle.C:
			ka = true
		}
		sent := false
		for {
			f, b, lane := sq.next()
			if f == nil {
				break
			}
			err := sq.st.WriteFlowAsync(b, f.u.tag, tunFlowBase|uint64(f.key), lane, f.doneFn)
			if errors.Is(err, smux.ErrWouldBlock) {
				sq.held(f)
				break
			}
			if err != nil {
				l.markDead()
				return
			}
			sent = true
		}
		if ka && !sent && sq.kaBusy.CompareAndSwap(false, true) {
			sq.ka = tlscarrier.AppendTunFrame(sq.ka[:0], core.TypePing, 0, 0, make([]byte, core.KeepalivePad()))
			err := sq.st.WriteFlowAsync(sq.ka, 0, tunKeepFlow, smux.LaneLatency, func(error) { sq.kaBusy.Store(false) })
			switch {
			case errors.Is(err, smux.ErrWouldBlock):
				sq.kaBusy.Store(false)
			case err != nil:
				l.markDead()
				return
			default:
				sent = true
			}
		}
		if sent || ka {
			ka = false
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(l.keepalive())
		}
	}
}

// isEcho says whether pkt is an ICMP or ICMPv6 echo request or reply
// (always light, §3.1).
func isEcho(pkt []byte) bool {
	switch {
	case len(pkt) >= 20 && pkt[0]>>4 == 4:
		ihl := int(pkt[0]&0x0f) * 4
		return pkt[9] == 1 && len(pkt) > ihl && (pkt[ihl] == 8 || pkt[ihl] == 0)
	case len(pkt) >= 40 && pkt[0]>>4 == 6:
		proto, off := ipv6Transport(pkt)
		return proto == 58 && off > 0 && len(pkt) > off && (pkt[off] == 128 || pkt[off] == 129)
	}
	return false
}
