package smux

import (
	"encoding/binary"
	"errors"
	"io"
	mathrand "math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The frame scheduler (Config.Sched): hs2's link scheduler (C2) in place of
// the shaper (shaper.go, shaperLoop).
//
// The session's frames go to a FrameSched its owner gives it (hs2's
// engine.Sched: control first, then the latency lane, then bulk by user and
// by flow), and the session's writer (sendLoopSched) takes them one at a
// time: it asks for the next frame only once the one before is written, so
// it never holds more than one frame past the socket (hs2's host-queue guard
// bounds the socket's own queue). There is no shaper goroutine and no bound
// on what is queued as a whole: a control frame is never held behind data,
// and a stream still has at most one data frame queued (its writer waits for
// each), so the streams bound what is queued.
//
// A user (the tag Config.FlowInfo says, else the one of the stream's SYN)
// has at most Config.UserFrames data frames queued at once: the writer of
// one more waits in that user's FIFO (backpressure, nothing dropped) and is
// let in, in turn, as the user's frames are taken; the writers of other
// users do not wait. Config.UserMem, when set, counts the bytes each user
// has queued and may refuse more to a user that already has a frame queued
// here: its writer then waits the same way.
//
// A data frame's lane and size are set as its stream's writer cuts it
// (writeV2Sched): a latency-lane frame carries at most Config.LaneFrame, a
// bulk one Config.BulkFrame of the rate the session wrote at over the last
// second. The writer ticks the scheduler each schedTick as it takes frames.

// The lanes of a FrameSched: hs2's C2 lanes (engine.Lane).
const (
	LaneCtrl    uint8 = 0 // SYN, FIN, UPD, NOP: first, never behind data
	LaneLatency uint8 = 1 // light flows, and new ones
	LaneBulk    uint8 = 2
)

// FrameSched orders the frames waiting for a session's writer
// (Config.Sched). The session calls it under its own lock, one call at a
// time, and only from its writer and the streams' writers.
type FrameSched interface {
	// Push queues it, a frame of size bytes of flow of user, in lane.
	Push(it interface{}, user uint32, flow uint64, lane uint8, size int)
	// Next takes the next frame to write (false: none queued).
	Next() (it interface{}, ok bool)
	// Tick moves the scheduler on to now; rate is what the link carried
	// (bytes/s, 0: unknown).
	Tick(now time.Duration, rate float64)
}

// UrgentSched is a FrameSched that can give its control and latency lanes'
// frames alone (Config.BulkGate: what the writer writes while a bulk frame
// waits at the gate).
type UrgentSched interface {
	// NextUrgent takes the next control or latency-lane frame (false: none
	// queued); bulk frames stay where they are.
	NextUrgent() (it interface{}, ok bool)
}

// FrameHeaderSize is a frame's header on the wire: the scheduler counts a
// frame's size with it (a latency-lane frame of LaneFrame data bytes is
// this much more to it).
const FrameHeaderSize = headerSize

var (
	// ErrNoSched: a frame scheduler's call on a session without one.
	ErrNoSched = errors.New("smux: the session has no frame scheduler")
	// ErrFrameSize: a frame of more than 65535 bytes.
	ErrFrameSize = errors.New("smux: frame too large")
)

const (
	// schedTick: the writer ticks the scheduler this often while it writes.
	schedTick = 100 * time.Millisecond
	// schedRateWin: the rate is what the session wrote over the last this
	// many ticks (a second).
	schedRateWin = 10
	// schedLatKeep: the latency lane's waits kept for SchedStats.
	schedLatKeep = 1024
	// schedFlushWait: after a bulk frame, a conn that holds back a tail
	// (flusher) is flushed only once nothing more came for this long; after
	// any other frame at once.
	schedFlushWait = time.Millisecond
)

// flusher is a conn that may hold back a short tail of what it was given
// (hs2's shaper packs it into the next record): the session's writer
// flushes it each time it runs out of frames.
type flusher interface{ Flush() error }

// coalescer is the session writer's hold of a tail of light frames
// (Config.Coalesce; not bulk ones: those wait schedFlushWait for the next
// as before). A light frame written while the session wrote at least
// CoalesceMin light frames over the last second starts a hold of
// Config.Coalesce (with CoalesceJitter, of a uniform draw in [0.5, 1.5) of
// it), from that frame: the frames written within it share the tail's
// record (hs2's shaper sends a record as soon as it is full, so the hold
// ends earlier when they fill it). An urgent frame (Stream.SetUrgent) ends
// the hold at once: its tail goes out as soon as the writer runs dry.
// (R1 round 3: candidate gamma1's; the jitter and the merge onto gamma2's
// writer are the judge's.)
type coalescer struct {
	win     time.Duration
	min     float64
	jitter  bool
	holding bool
	urgent  bool
	since   int64         // monoNow of the first frame of the hold
	cur     time.Duration // the current hold's length
	// the light frames of the last second, coalBuckets of coalBucket each
	// (ring), and the bucket of the latest
	n  [coalBuckets]uint32
	nb int64
}

const (
	coalBucket  = 100 * time.Millisecond
	coalBuckets = 10
)

// count notes a light frame at now and is the light frames of the last
// second.
func (c *coalescer) count(now int64) float64 {
	b := now / int64(coalBucket)
	switch d := b - c.nb; {
	case d >= coalBuckets || d < 0:
		c.n = [coalBuckets]uint32{}
	default:
		for k := int64(1); k <= d; k++ {
			c.n[(c.nb+k)%coalBuckets] = 0
		}
	}
	c.nb = b
	c.n[b%coalBuckets]++
	sum := 0.0
	for _, v := range c.n {
		sum += float64(v)
	}
	return sum
}

// wrote: a light frame (urgent: of an urgent stream) went into the conn.
func (c *coalescer) wrote(urgent bool, now int64) {
	r := c.count(now)
	if urgent {
		c.urgent = true
	}
	if !c.holding && r >= c.min {
		c.holding, c.since, c.cur = true, now, c.win
		if c.jitter && c.win > 0 {
			c.cur = c.win/2 + time.Duration(coalRand(int64(c.win)))
		}
	}
}

// left is how long the hold has left (0: none, or it ends now).
func (c *coalescer) left(now int64) time.Duration {
	if !c.holding || c.urgent {
		return 0
	}
	if l := time.Duration(c.since + int64(c.cur) - now); l > 0 {
		return l
	}
	return 0
}

// flushed: the writer flushed the conn.
func (c *coalescer) flushed() { c.holding, c.urgent = false, false }

// coalRand is a uniform draw in [0, n) (n > 0).
var coalRand = func(n int64) int64 { return mathrand.Int64N(n) }

// schedFrame is one frame for the scheduler, with its place in its user's
// FIFO while it waits for room.
type schedFrame struct {
	req   writeRequest
	u     *schedUser // the user it counts against (nil: control, or WriteFlowAsync's)
	tag   uint32
	flow  uint64
	lane  uint8
	size  int
	at    int64 // monoNow when it went into the scheduler
	state uint8
	next  *schedFrame // the user's FIFO
	mem   int         // bytes counted with the user's memory (UserMem)
	// done: WriteFlowAsync's, called by the writer once the frame is written
	// or will not be (nil: the writer of the frame waits on req.result)
	done func(error)
	// early: a data frame whose writer has its result once the session's
	// writer has copied it, not once it is written (Config.EarlyDone)
	early bool
	// urgent: a frame of an urgent stream (Stream.SetUrgent): no
	// coalescing hold after it (Config.Coalesce)
	urgent bool
}

const (
	sfWaiting uint8 = iota + 1 // in its user's FIFO
	sfQueued                   // in the scheduler
	sfTaken                    // taken by the session's writer
	sfGone                     // its writer gave up while it waited
)

// schedUser is a user with data frames queued, or writers waiting.
type schedUser struct {
	tag        uint32
	pending    int         // data frames in the scheduler
	head, tail *schedFrame // writers waiting for room, in order
	mem        func(n int, force bool) bool
	seen       uint64 // the tick of its latest frame (forget)
}

type rateMark struct {
	at    int64
	wrote uint64
}

// schedState is a session's scheduler with what the session keeps around
// it; mu covers all but the atomics.
type schedState struct {
	mu    sync.Mutex
	fs    FrameSched
	cfg   *Config
	users map[uint32]*schedUser
	wake  chan struct{} // the writer waits here when nothing is queued
	dead  bool
	// the ticks: the latest, and what the writer had written at the last
	// ones (ring)
	tickAt int64
	ticks  uint64
	marks  [schedRateWin + 1]rateMark
	mi     int
	wrote  atomic.Uint64 // bytes written, headers included (the writer's)
	bulk   atomic.Int32  // the bulk frame now
	rate   atomic.Uint64 // the rate of the latest tick, bytes/s
	queued int           // frames in the scheduler
	// SchedStats's, since its previous call
	lat     [schedLatKeep]uint32 // latency-lane waits, µs (ring)
	latN    uint64
	latSeen uint64
	blocked uint64 // writers that waited for room
	waiting int    // writers waiting now
}

func newSchedState(cfg *Config) *schedState {
	sc := &schedState{fs: cfg.Sched(), cfg: cfg, users: map[uint32]*schedUser{}, wake: make(chan struct{}, 1)}
	sc.bulk.Store(int32(sc.bulkOf(0)))
	return sc
}

// bulkOf is the bulk frame for a link of rate bytes/s, within
// MaxFrameSize.
func (sc *schedState) bulkOf(rate float64) int {
	n := sc.cfg.MaxFrameSize
	if f := sc.cfg.BulkFrame; f != nil {
		if b := f(rate); b > 0 && b < n {
			n = b
		}
	}
	return n
}

// user is the record of tag (made if it has none).
func (sc *schedState) user(tag uint32) *schedUser {
	u := sc.users[tag]
	if u == nil {
		u = &schedUser{tag: tag}
		if f := sc.cfg.UserMem; f != nil {
			u.mem = f(tag)
		}
		sc.users[tag] = u
	}
	u.seen = sc.ticks
	return u
}

// room says whether a data frame sf of u may go in now: the user is under
// UserFrames, and its memory takes the frame (always, with nothing of it
// queued here).
func (sc *schedState) room(u *schedUser, sf *schedFrame) bool {
	if n := sc.cfg.UserFrames; n > 0 && u.pending >= n {
		return false
	}
	if u.mem != nil {
		if !u.mem(sf.size, u.pending == 0) {
			return false
		}
		sf.mem = sf.size
	}
	return true
}

// push puts sf in the scheduler (under mu).
func (sc *schedState) push(sf *schedFrame, now int64) {
	sf.state, sf.at = sfQueued, now
	if sf.u != nil {
		sf.u.pending++
	}
	sc.queued++
	sc.fs.Push(sf, sf.tag, sf.flow, sf.lane, sf.size)
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// queue takes a frame from its writer: into the scheduler, or, a data frame
// of a user without room (bound: it counts against its user), into the
// user's FIFO.
func (sc *schedState) queue(sf *schedFrame, bound bool) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.dead {
		return io.ErrClosedPipe
	}
	now := monoNow()
	if !bound {
		sc.push(sf, now)
		return nil
	}
	u := sc.user(sf.tag)
	sf.u = u
	if u.head == nil && sc.room(u, sf) {
		sc.push(sf, now)
		return nil
	}
	sf.state = sfWaiting
	if u.tail == nil {
		u.head = sf
	} else {
		u.tail.next = sf
	}
	u.tail = sf
	sc.waiting++
	sc.blocked++
	return nil
}

// cancel: the writer of sf gave up on it; one still in its user's FIFO
// leaves it (one in the scheduler still goes out, see frameData).
func (sc *schedState) cancel(sf *schedFrame) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sf.state != sfWaiting {
		return
	}
	u := sf.u
	var prev *schedFrame
	for f := u.head; f != nil; prev, f = f, f.next {
		if f != sf {
			continue
		}
		if prev == nil {
			u.head = f.next
		} else {
			prev.next = f.next
		}
		if u.tail == f {
			u.tail = prev
		}
		break
	}
	sf.next, sf.state = nil, sfGone
	sc.waiting--
	sc.letIn(u, monoNow()) // the ones behind it may fit
}

// gone says whether sf left its user's FIFO without going out (its writer
// gave up, or the session died, while it waited).
func (sc *schedState) gone(sf *schedFrame) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sf.state == sfGone
}

// letIn lets u's waiting writers in, in order, while it has room.
func (sc *schedState) letIn(u *schedUser, now int64) {
	for u.head != nil && sc.room(u, u.head) {
		sf := u.head
		u.head = sf.next
		if u.head == nil {
			u.tail = nil
		}
		sf.next = nil
		sc.waiting--
		sc.push(sf, now)
	}
}

// forget drops the records of the users with nothing queued or waiting and
// no frame since the tick before (each tick: a user's record, and its
// memory account, outlive its frames, one at a time, of a busy stream).
func (sc *schedState) forget() {
	for tag, u := range sc.users {
		if u.pending == 0 && u.head == nil && u.seen+1 < sc.ticks {
			delete(sc.users, tag)
		}
	}
}

// take is the writer's: the next frame to write (nil: none queued), the
// scheduler ticked first when a tick is due.
func (sc *schedState) take() *schedFrame {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.dead {
		return nil
	}
	now := monoNow()
	if now-sc.tickAt >= int64(schedTick) {
		sc.tick(now)
	}
	it, ok := sc.fs.Next()
	if !ok {
		return nil
	}
	return sc.took(it.(*schedFrame), now)
}

// takeUrgent is the writer's while a bulk frame waits at the gate: the next
// control or latency-lane frame (nil: none queued, or the scheduler cannot
// tell its lanes apart).
func (sc *schedState) takeUrgent() *schedFrame {
	us, ok := sc.fs.(UrgentSched)
	if !ok {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.dead {
		return nil
	}
	it, ok := us.NextUrgent()
	if !ok {
		return nil
	}
	return sc.took(it.(*schedFrame), monoNow())
}

// took books sf, just taken from the scheduler at now (under mu).
func (sc *schedState) took(sf *schedFrame, now int64) *schedFrame {
	sf.state = sfTaken
	sc.queued--
	if sf.lane == LaneLatency || sf.lane > LaneBulk { // (hs2's realtime lanes: light frames too)
		sc.lat[sc.latN%schedLatKeep] = uint32((now - sf.at) / int64(time.Microsecond))
		sc.latN++
	}
	if u := sf.u; u != nil {
		u.pending--
		if sf.mem > 0 && u.mem != nil {
			u.mem(-sf.mem, false)
			sf.mem = 0
		}
		sc.letIn(u, now)
	}
	return sf
}

// tick moves the scheduler to now with the rate the session wrote at over
// the last second (under mu).
func (sc *schedState) tick(now int64) {
	sc.tickAt = now
	sc.ticks++
	sc.forget()
	sc.mi = (sc.mi + 1) % len(sc.marks)
	sc.marks[sc.mi] = rateMark{at: now, wrote: sc.wrote.Load()}
	// the oldest mark within the last second, else the newest before it
	// (after an idle spell: a rate near 0 over the gap)
	ref := -1
	for k := 1; k < len(sc.marks); k++ {
		i := (sc.mi - k + len(sc.marks)) % len(sc.marks)
		m := sc.marks[i]
		if m.at == 0 {
			break
		}
		ref = i
		if now-m.at >= int64(time.Second) {
			break
		}
	}
	rate := 0.0
	if ref >= 0 {
		m := sc.marks[ref]
		if dt := time.Duration(now - m.at).Seconds(); dt > 0 {
			rate = float64(sc.marks[sc.mi].wrote-m.wrote) / dt
		}
	}
	sc.rate.Store(uint64(rate))
	sc.fs.Tick(time.Duration(now), rate)
	sc.bulk.Store(int32(sc.bulkOf(rate)))
}

// drain: the session's writer is gone. What is queued will not be written:
// WriteFlowAsync's frames are told so, and every frame's bytes leave its
// user's memory. Frames queued from now on are refused.
func (sc *schedState) drain() {
	sc.mu.Lock()
	sc.dead = true
	var done []*schedFrame
	for {
		it, ok := sc.fs.Next()
		if !ok {
			break
		}
		sf := it.(*schedFrame)
		sf.state = sfTaken
		if u := sf.u; u != nil {
			u.pending--
			if sf.mem > 0 && u.mem != nil {
				u.mem(-sf.mem, false)
				sf.mem = 0
			}
		}
		if sf.done != nil {
			done = append(done, sf)
		}
	}
	for _, u := range sc.users {
		for f := u.head; f != nil; f = f.next {
			f.state = sfGone
		}
		u.head, u.tail = nil, nil
	}
	sc.users = map[uint32]*schedUser{}
	sc.queued, sc.waiting = 0, 0
	sc.mu.Unlock()
	for _, sf := range done {
		sf.done(io.ErrClosedPipe)
	}
}

// sendLoopSched is the session's writer with a scheduler (sendLoop's).
func (s *Session) sendLoopSched() {
	sc := s.sch
	defer sc.drain()
	var buf []byte
	var vec [][]byte
	bw, ok := s.conn.(interface {
		WriteBuffers(v [][]byte) (n int, err error)
	})
	if ok {
		buf = make([]byte, headerSize+seqLen)
		vec = make([][]byte, 2)
	} else {
		buf = make([]byte, (1<<16)+headerSize+seqLen)
	}
	fl, _ := s.conn.(flusher)
	gate := s.config.BulkGate
	var hold, gt *time.Timer
	defer func() {
		if gt != nil {
			gt.Stop()
		}
	}()
	held := false // the conn holds a tail of a bulk frame (fl)
	var co *coalescer
	if fl != nil && s.config.Coalesce > 0 {
		co = &coalescer{win: s.config.Coalesce, min: s.config.CoalesceMin, jitter: s.config.CoalesceJitter}
	}
	// write writes sf; false: the session's writing is over (the error told)
	write := func(sf *schedFrame) bool {
		req := sf.req
		buf[0] = req.frame.ver
		buf[1] = req.frame.cmd
		binary.LittleEndian.PutUint16(buf[2:], uint16(req.frame.wireLen()))
		binary.LittleEndian.PutUint32(buf[4:], req.frame.sid)
		hl := headerSize
		if req.frame.hasSeq {
			binary.BigEndian.PutUint32(buf[headerSize:], req.frame.seq)
			hl += seqLen
		}
		var n int
		var err error
		told := false // the frame's writer has its result (EarlyDone)
		data := req.take()
		if len(vec) > 0 {
			vec[0] = buf[:hl]
			vec[1] = data
			n, err = bw.WriteBuffers(vec)
			req.done()
		} else {
			copy(buf[hl:], data)
			req.done()
			if sf.early {
				// its data is copied and its place among the session's
				// frames fixed: its stream may queue the next one while this
				// one is written (a write error ends the session)
				req.result <- writeResult{n: len(data)}
				close(req.result)
				told = true
			}
			n, err = s.conn.Write(buf[:hl+len(data)])
		}
		if n > 0 {
			sc.wrote.Add(uint64(n))
		}
		held = fl != nil && sf.lane == LaneBulk
		if co != nil && sf.lane != LaneBulk {
			co.wrote(sf.urgent, monoNow())
		}
		n -= hl
		if n < 0 {
			n = 0
		}
		if sf.done != nil {
			sf.done(err)
		} else if !told {
			req.result <- writeResult{n: n, err: err}
			close(req.result)
		}
		if err == nil && fl != nil && s.config.FlushEach && sf.lane != LaneBulk {
			// (lab) no record carries two frames that are not bulk
			if err = fl.Flush(); err == nil {
				held = false
			}
		}
		if err != nil {
			s.notifyWriteError(err)
			return false
		}
		return true
	}
	// gated holds sf, a bulk frame, at the gate (BulkGate) until it may go,
	// writing the urgent lanes' frames that come meanwhile; false: the
	// session's writing is over (sf told so)
	gated := func(sf *schedFrame) bool {
		for {
			w := gate(sf.size)
			if w <= 0 {
				return true
			}
			if gt == nil {
				gt = time.NewTimer(w)
			} else {
				gt.Reset(w)
			}
			select {
			case <-gt.C:
				continue
			case <-sc.wake:
			case <-s.die:
			case <-s.chSocketWriteError:
			}
			if !gt.Stop() {
				select {
				case <-gt.C:
				default:
				}
			}
			select {
			case <-s.die:
				sf.abandon(io.ErrClosedPipe)
				return false
			case <-s.chSocketWriteError:
				sf.abandon(s.socketWriteError.Load().(error))
				return false
			default:
			}
			wrote := false
			for u := sc.takeUrgent(); u != nil; u = sc.takeUrgent() {
				if !write(u) {
					sf.abandon(io.ErrClosedPipe)
					return false
				}
				wrote = true
			}
			if wrote && fl != nil {
				// they are light: out at once, the bulk tail before them
				held = false
				if co != nil {
					co.flushed()
				}
				if err := fl.Flush(); err != nil {
					s.notifyWriteError(err)
					sf.abandon(err)
					return false
				}
			}
		}
	}
	for {
		sf := sc.take()
		if sf == nil {
			// a bulk stream queues its next frame as soon as its last is
			// written: the tail waits schedFlushWait for it before it goes
			// out padded (a link of one download flushed, and padded, every
			// frame); a tail of light frames (Coalesce) waits till its hold
			// from the first of them ends
			wait := time.Duration(0)
			if held {
				wait = schedFlushWait
			}
			if co != nil && co.holding {
				if lw := co.left(monoNow()); !held || lw < wait {
					wait = lw
				}
			}
			if wait > 0 {
				if hold == nil {
					hold = time.NewTimer(wait)
				} else {
					hold.Reset(wait)
				}
				select {
				case <-sc.wake:
					if !hold.Stop() {
						<-hold.C
					}
					continue
				case <-hold.C:
				case <-s.die:
					return
				case <-s.chSocketWriteError:
					return
				}
			}
			held = false
			if co != nil {
				co.flushed()
			}
			if fl != nil {
				if err := fl.Flush(); err != nil {
					s.notifyWriteError(err)
					return
				}
			}
			select {
			case <-sc.wake:
				continue
			case <-s.die:
				return
			case <-s.chSocketWriteError:
				return
			}
		}
		if gate != nil && sf.lane == LaneBulk && !gated(sf) {
			return
		}
		if !write(sf) {
			return
		}
	}
}

// abandon tells the writer of sf, a frame the session's writer took but
// will not write, that it is not going out (err).
func (sf *schedFrame) abandon(err error) {
	if sf.done != nil {
		sf.done(err)
		return
	}
	sf.req.result <- writeResult{err: err}
	close(sf.req.result)
}

// queueSched hands a frame to the scheduler: a control frame (class
// CLSCTRL) in the control lane, a data frame of user tag in lane, within
// the user's room.
func (s *Session) queueSched(f Frame, class CLASSID, tag uint32, lane uint8) (writeRequest, error) {
	req := writeRequest{class: class, frame: f, result: make(chan writeResult, 1)}
	if len(f.data) > 0 {
		req.data = &frameData{b: f.data}
	}
	select {
	case <-s.die:
		return req, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		return req, s.socketWriteError.Load().(error)
	default:
	}
	sf := &schedFrame{tag: tag, flow: uint64(f.sid), lane: lane, size: headerSize + f.wireLen(), urgent: f.urgent}
	if class != CLSDATA {
		sf.tag, sf.lane = 0, LaneCtrl
	} else {
		sf.early = s.config.EarlyDone
	}
	req.sf = sf
	sf.req = req
	if err := s.sch.queue(sf, class == CLSDATA); err != nil {
		return req, err
	}
	return req, nil
}

// frameFor is the most a data frame in lane carries now.
func (s *Session) frameFor(lane uint8) int {
	// a group's frame carries seqLen more on the wire, and the header's
	// length is 16 bits: MaxFrameSize may be 65535 (VerifyConfig), so a
	// full group frame wrapped and the peer read user bytes as frame
	// headers (the R1 red team's second pass, R2-5)
	n := min(s.config.MaxFrameSize, 0xFFFF-seqLen)
	lim := int(s.sch.bulk.Load())
	if lane == LaneLatency && s.config.LaneFrame > 0 {
		lim = s.config.LaneFrame
	}
	if lim > 0 && lim < n {
		n = lim
	}
	return n
}

// SchedStats is what a session's scheduler did since the previous call
// (Config.Sched).
type SchedStats struct {
	Rate      float64 // bytes/s the session wrote over the last second
	BulkFrame int     // the bulk frame now, bytes
	// LaneN latency-lane frames were taken; how long they waited, the
	// median and the 99th percentile (of the latest 1024)
	LaneN            uint64
	LaneP50, LaneP99 time.Duration
	Blocked          uint64 // writers that waited for their user's room
	Waiting          int    // writers waiting now
	Queued           int    // frames queued now
	// MostPending: the most data frames one user has queued now, and whose
	MostPending     int
	MostPendingUser uint32
}

// SchedStats says what the session's scheduler did since the previous call
// (false: the session has none).
func (s *Session) SchedStats() (SchedStats, bool) {
	sc := s.sch
	if sc == nil {
		return SchedStats{}, false
	}
	sc.mu.Lock()
	st := SchedStats{Rate: float64(sc.rate.Load()), BulkFrame: int(sc.bulk.Load()), Blocked: sc.blocked,
		Waiting: sc.waiting, Queued: sc.queued, LaneN: sc.latN - sc.latSeen}
	for _, u := range sc.users {
		if u.pending > st.MostPending {
			st.MostPending, st.MostPendingUser = u.pending, u.tag
		}
	}
	k := st.LaneN
	if k > schedLatKeep {
		k = schedLatKeep
	}
	w := make([]uint32, 0, k)
	for i := uint64(0); i < k; i++ {
		w = append(w, sc.lat[(sc.latN-1-i)%schedLatKeep])
	}
	sc.latSeen, sc.blocked = sc.latN, 0
	sc.mu.Unlock()
	if len(w) > 0 {
		sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
		st.LaneP50 = time.Duration(w[(len(w)-1)/2]) * time.Microsecond
		st.LaneP99 = time.Duration(w[(len(w)-1)*99/100]) * time.Microsecond
	}
	return st, true
}

// WithSched runs f with the session's scheduler, under the session's lock
// (false: the session has none). f must not call back into the session.
func (s *Session) WithSched(f func(FrameSched)) bool {
	sc := s.sch
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	f(sc.fs)
	return true
}

// BulkFrame is the session's bulk frame now (0: it has no scheduler).
func (s *Session) BulkFrame() int {
	if s.sch == nil {
		return 0
	}
	return int(s.sch.bulk.Load())
}

// HasSched says whether the session runs a frame scheduler (Config.Sched).
func (s *Session) HasSched() bool { return s.sch != nil }
