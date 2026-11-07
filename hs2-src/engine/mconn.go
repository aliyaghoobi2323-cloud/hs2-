package engine

import (
	"cmp"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// mconn: one user connection carried by several sub-streams ("subs"), each a
// smux stream on its own link to the same peer (HS2_MCONN=1).
//
// Every byte of a direction has an offset, so the subs are interchangeable:
//
//   - light mode: each chunk is written to every live light sub (two). Each sub
//     has its own bounded queue and writer goroutine, so one link's TCP loss
//     recovery or queueing delays only that copy; the receiver delivers
//     whichever copy arrives first and drops the other.
//   - bulk mode: once a direction becomes heavy the connection moves to one
//     sub on a bulk link (opened by the Iran side, announced with BULK on it)
//     and stops duplicating; the light subs are retired a few seconds later.
//
// Records, on every sub, in both directions:
//
//	DATA     0x01 | off u64 | len u16 | payload   bytes [off, off+len)
//	FIN      0x02 | off u64                       this direction ends at off
//	BULK     0x03     "send to me only on this sub" (Iran); echoed by Kharej
//	WANTBULK 0x04     Kharej asks Iran to open a bulk sub
//	RST      0x05     abort the connection
//
// There is no retransmission layer: a byte lives on as long as one live sub
// still holds (or has written) a copy. When a sub dies holding the only copy
// of some bytes — or may have (see mHoleSafe) — the connection is reset, the
// same outcome as today's single stream on a dying link.
//
// Goroutines per mConn: one manager, plus a reader and a writer per sub, plus a
// short-lived one per sub being opened. All of them end once the mConn ended
// (GetDieCh closed) and the opener calls in flight returned.

// mPlane is the kind of link a sub is opened on.
type mPlane int

const (
	planeLight mPlane = iota
	planeBulk
)

// mSub is one sub-stream (a smux stream with the header already written).
type mSub interface{ io.ReadWriteCloser }

// mOpener opens a new sub on a link of the given plane, not on any of the
// links in `avoid` (opaque link keys from earlier calls). Iran side only.
type mOpener func(p mPlane, avoid []any) (sub mSub, linkKey any, err error)

// Record kinds and sizes.
const (
	mRecData     = 0x01
	mRecFin      = 0x02
	mRecBulk     = 0x03
	mRecWantBulk = 0x04
	mRecRst      = 0x05

	mDataHdr = 1 + 8 + 2 // kind, offset, length
	mFinLen  = 1 + 8     // kind, offset

	// mRecMax: a whole DATA record fits one smux frame (SmuxFrameSize), so a
	// record is never split into two frames on the link.
	mRecMax = 16 << 10
	// mChunkMax: the payload of one DATA record (and one pooled chunk).
	mChunkMax = mRecMax - mDataHdr
)

// Thresholds.
const (
	// mSubQueue bounds the bytes waiting in one sub's send queue. A stalled
	// sub fills it (and then misses copies) instead of blocking Write; when
	// every sending sub is full, Write blocks (backpressure).
	mSubQueue = 256 << 10
	// mRecvBuf bounds the receiver: the in-order bytes Read has not taken
	// yet, and separately the out-of-order bytes waiting for a gap. Beyond
	// it the subs' readers stop reading (backpressure into smux).
	mRecvBuf = 4 << 20

	// Heavy detection, per sending direction. A relay reads up to 32 KiB
	// only when data is waiting in its socket, so several big writes close
	// together mean the source is ahead of us — a bulk transfer, not a
	// request/response exchange. A sustained rate caps what duplication
	// costs the light links.
	mHeavyBigWrite  = 16 << 10    // a write at least this big counts as "big"
	mHeavyBigWrites = 3           // this many big writes within mHeavyWindow
	mHeavyRate      = 1_000_000   // or more than this many bytes (8 Mbit/s)
	mHeavyWindow    = time.Second // ... within this window
	// mHeavyBigMin: the big writes count only with at least this many bytes
	// in the window (4 Mbit/s): a call or a game that fell behind for a
	// moment also reads a few big chunks, and moving it to a bulk link puts
	// it behind the very downloads it was kept away from.
	mHeavyBigMin = 512 << 10
	// mDupMaxRate: a light connection is sent on two subs only while it moves
	// at most this many bytes a window (384 kbit/s): pings, games, requests,
	// chat. Above it (a call, a stream) it stays on the light links with one
	// copy, so duplication never loads them (in the lab, duplicating every
	// light connection took a 2 Mbit/s call from ~135 ms to ~1 s).
	mDupMaxRate = 48 << 10

	// mRetireAfter: Iran closes the light subs once the peer's BULK echo is
	// this old and no record arrived on them for this long (records the peer
	// wrote on them before it switched have long arrived) — the receiver
	// takes their EOF without FIN.
	mRetireAfter = 3 * time.Second
	mRetirePoll  = 500 * time.Millisecond

	// mHoleSafe: a light sub that missed a copy (its queue was full, or it
	// joined after data had been sent) within this long is not trusted to
	// hold everything the other sub may have lost in flight; if the other
	// one dies meanwhile the connection is reset rather than left hanging
	// on a gap no one will fill.
	mHoleSafe = 5 * time.Second

	// mReopenBackoff: wait after a failed open before trying again.
	mReopenBackoff = 2 * time.Second

	// mCloseLinger: after Close, the queued bytes and the FIN get this long
	// to be written before the subs are closed (with an RST if they could
	// not all be written).
	mCloseLinger = 5 * time.Second
	// mRSTWait: how long an RST may take to be written before its sub is
	// closed anyway.
	mRSTWait = time.Second

	// mGapTimeout: missing bytes the receiver has waited for this long
	// (while not held back by its own app) are lost; the connection resets
	// instead of hanging.
	mGapTimeout = 30 * time.Second
	mGapPoll    = time.Second

	// mDeadKeys: recently dead light links avoided by a replacement open.
	mDeadKeys = 4
)

var (
	errMConnReset    = errors.New("mconn: reset by peer")
	errMConnAborted  = errors.New("mconn: aborted")
	errMConnBulkDied = errors.New("mconn: bulk sub died")
	errMConnSubsDied = errors.New("mconn: every sub died")
	errMConnLost     = errors.New("mconn: a sub died holding the only copy of some bytes")
	errMConnProto    = errors.New("mconn: bad record")
	errMConnGap      = errors.New("mconn: missing bytes never arrived")
	errMConnClosed   = io.ErrClosedPipe
)

// mchunk is one record on the send side (shared by every sub queue that holds
// it, reference counted) or one received payload. Pooled.
type mchunk struct {
	buf  [mRecMax]byte
	kind byte
	off  uint64
	n    int // payload bytes (DATA)

	refs   atomic.Int32 // send side: queues and writers holding it
	copies int          // send side, under mConn.mu: subs holding or having written it
}

var mchunks = sync.Pool{New: func() any { return new(mchunk) }}

func getChunk(kind byte) *mchunk {
	c := mchunks.Get().(*mchunk)
	c.kind, c.off, c.n, c.copies = kind, 0, 0, 0
	c.refs.Store(0)
	c.buf[0] = kind
	return c
}

func putChunk(c *mchunk) { mchunks.Put(c) }

// unref drops one send-side reference; the last one returns the chunk.
func (c *mchunk) unref() {
	if c.refs.Add(-1) == 0 {
		putChunk(c)
	}
}

// seq: the chunk carries this direction's byte stream (DATA or FIN), as
// opposed to a per-sub control record.
func (c *mchunk) seq() bool { return c.kind == mRecData || c.kind == mRecFin }

// qsize is what the chunk counts against a sub's queue bound.
func (c *mchunk) qsize() int { return c.n }

func (c *mchunk) setOff(off uint64) {
	c.off = off
	binary.BigEndian.PutUint64(c.buf[1:9], off)
}

func (c *mchunk) wire() []byte {
	switch c.kind {
	case mRecData:
		return c.buf[:mDataHdr+c.n]
	case mRecFin:
		return c.buf[:mFinLen]
	}
	return c.buf[:1]
}

// msub is one sub of an mConn.
type msub struct {
	m   *mConn
	rw  mSub
	key any

	bulk     atomic.Bool  // this is the connection's bulk sub
	lastRecv atomic.Int64 // UnixNano of the last record read from it

	// under m.mu
	q          []*mchunk
	qBytes     int
	cur        *mchunk // being written now
	dead       bool
	retired    bool // closed on purpose (a light sub after the switch)
	closeAfter bool // close once the queue (an RST) is written
	lastHole   time.Time

	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	hdr [mDataHdr]byte // reader only
}

func (s *msub) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.rw.Close()
	})
}

// push queues c on s (under m.mu).
func (s *msub) push(c *mchunk) {
	c.refs.Add(1)
	if c.seq() {
		c.copies++
	}
	s.q = append(s.q, c)
	s.qBytes += c.qsize()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// fits: s has room for n more bytes (under m.mu).
func (s *msub) fits(n int) bool { return s.qBytes == 0 || s.qBytes+n <= mSubQueue }

// rchunk is a received payload slice b at offset off (of b[0]).
type rchunk struct {
	c   *mchunk
	off uint64
	b   []byte
}

// mrecv is the receive side: reassembly of the peer's direction by offset.
type mrecv struct {
	mu   sync.Mutex
	cond sync.Cond

	next       uint64   // offset of the first byte not yet in ready
	ready      []rchunk // contiguous bytes below next that Read has not taken
	head       int
	readyBytes int
	ooo        []rchunk // out-of-order, sorted by off, non-overlapping, all > next
	oooBytes   int

	fin    bool
	finOff uint64

	closed bool  // local Close: Read fails, arrivals are dropped
	dead   bool  // the mConn ended: arrivals are dropped
	err    error // why it ended (Read returns it once ready is drained)

	lastProgress time.Time // gap watch: last delivery (or app-held) moment
}

// mConn is one user connection over several subs. It implements
// io.ReadWriteCloser and GetDieCh, as relayStream expects of a stream.
type mConn struct {
	id   uint64
	iran bool
	open mOpener

	mu       sync.Mutex
	sendCond sync.Cond // Write waits here for queue room (L = &mu)

	subs    []*msub // live subs in join order
	mode    mPlane
	bulk    *msub
	sendOff uint64
	wclosed bool // FIN queued
	closing bool // Close called; lingering until queues drain
	ended   bool
	err     error
	peerFin bool

	// heavy detection (this side's sending direction)
	hvStart time.Time
	hvBig   int
	hvBytes int
	heavy   bool
	// duplication gate (this side's sending direction): bytes in the current
	// window, and whether this direction is over mDupMaxRate (one copy then)
	dupStart time.Time
	dupBytes int
	dupOff   bool

	wantBulk     bool // Iran: switch to bulk (own heavy, or WANTBULK)
	bulkOpening  bool
	lightOpening bool
	bulkRetryAt  time.Time
	lightRetryAt time.Time
	echoAt       time.Time // Iran: the peer echoed BULK
	retired      bool
	lingerAt     time.Time
	deadKeys     []any

	primary atomic.Pointer[msub] // the first light sub (for the "saved" counter)

	r mrecv

	kick      chan struct{} // wakes the manager
	die       chan struct{}
	closeOnce sync.Once
}

func newMConn(id uint64, iran bool, open mOpener) *mConn {
	m := &mConn{id: id, iran: iran, open: open,
		kick: make(chan struct{}, 1), die: make(chan struct{})}
	m.sendCond.L = &m.mu
	m.r.cond.L = &m.r.mu
	m.r.lastProgress = time.Now()
	obs.mconnOpen.Add(1)
	return m
}

// newMConnIran: Iran side; opens its first two light subs with open (the
// first before returning, the second right after, in the background).
func newMConnIran(id uint64, open mOpener) (*mConn, error) {
	sub, key, err := open(planeLight, nil)
	if err != nil {
		return nil, err
	}
	m := newMConn(id, true, open)
	m.mu.Lock()
	s := m.addSubLocked(sub, key, false)
	m.mu.Unlock()
	m.startSub(s)
	m.kickMgr() // opens the second light sub
	go m.manage()
	return m, nil
}

// newMConnExit: Kharej side; subs arrive with attach.
func newMConnExit(id uint64) *mConn {
	m := newMConn(id, false, nil)
	go m.manage()
	return m
}

// attach adds a sub that arrived for this connection (Kharej). The role (0:
// first, 1: join) matters only to the caller; a sub is a light sub until a
// BULK record arrives on it.
func (m *mConn) attach(sub mSub, role byte) {
	_ = role
	m.mu.Lock()
	if m.ended || m.closing {
		m.mu.Unlock()
		sub.Close()
		return
	}
	s := m.addSubLocked(sub, nil, false)
	m.sendCond.Broadcast()
	m.mu.Unlock()
	m.startSub(s)
}

// GetDieCh is closed when the connection fails or ends.
func (m *mConn) GetDieCh() <-chan struct{} { return m.die }

func (m *mConn) addSubLocked(rw mSub, key any, bulk bool) *msub {
	s := &msub{m: m, rw: rw, key: key, kick: make(chan struct{}, 1), done: make(chan struct{})}
	s.bulk.Store(bulk)
	s.lastRecv.Store(time.Now().UnixNano())
	if m.sendOff > 0 && !bulk {
		s.lastHole = time.Now() // it lacks what was sent before it joined
	}
	m.subs = append(m.subs, s)
	m.setPrimaryLocked()
	return s
}

func (m *mConn) removeSubLocked(s *msub) {
	if i := slices.Index(m.subs, s); i >= 0 {
		m.subs = slices.Delete(m.subs, i, i+1)
	}
	m.setPrimaryLocked()
}

func (m *mConn) setPrimaryLocked() {
	for _, s := range m.subs {
		if !s.bulk.Load() && !s.retired {
			m.primary.Store(s)
			return
		}
	}
	m.primary.Store(nil)
}

func (m *mConn) startSub(s *msub) {
	go s.readLoop()
	go s.writeLoop()
}

func (m *mConn) kickMgr() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// ---- send side ----

func (m *mConn) writeErrLocked() error {
	if m.ended {
		if m.err != nil && m.err != errMConnClosed {
			return m.err
		}
		return io.ErrClosedPipe
	}
	if m.wclosed {
		return io.ErrClosedPipe
	}
	return nil
}

// Write queues p; it returns once every chunk of p is queued on at least one
// sending sub.
func (m *mConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	if err := m.writeErrLocked(); err != nil {
		m.mu.Unlock()
		return 0, err
	}
	m.noteWriteLocked(len(p))
	m.mu.Unlock()
	written := 0
	for len(p) > 0 {
		c := getChunk(mRecData)
		n := copy(c.buf[mDataHdr:], p)
		c.n = n
		binary.BigEndian.PutUint16(c.buf[9:11], uint16(n))
		m.mu.Lock()
		err := m.sendLocked(c)
		m.mu.Unlock()
		if err != nil {
			putChunk(c) // never queued
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// sendLocked queues a DATA chunk on the sending subs, waiting while none has
// room. It assigns the chunk its offset.
func (m *mConn) sendLocked(c *mchunk) error {
	for {
		if err := m.writeErrLocked(); err != nil {
			return err
		}
		if m.mode == planeBulk {
			if b := m.bulk; b.fits(c.n) {
				c.setOff(m.sendOff)
				m.sendOff += uint64(c.n)
				b.push(c)
				return nil
			}
			m.sendCond.Wait()
			continue
		}
		fit := 0
		for _, s := range m.subs {
			if s.retired {
				continue
			}
			if s.fits(c.n) {
				fit++
			}
		}
		if fit == 0 {
			m.sendCond.Wait() // all full, or no sub yet (Kharej before attach)
			continue
		}
		c.setOff(m.sendOff)
		m.sendOff += uint64(c.n)
		// one copy when this direction is over mDupMaxRate: on the sub with
		// the shortest queue; the others miss it (lastHole, as a full queue)
		var one *msub
		if m.dupOff {
			for _, s := range m.subs {
				if !s.retired && s.fits(c.n) && (one == nil || s.qBytes < one.qBytes) {
					one = s
				}
			}
			fit = 1
		}
		var now time.Time
		for _, s := range m.subs {
			if s.retired {
				continue
			}
			if s.fits(c.n) && (one == nil || s == one) {
				s.push(c)
				continue
			}
			if now.IsZero() {
				now = time.Now()
			}
			s.lastHole = now
			obs.mconnMissed.Add(1)
		}
		if fit > 1 {
			obs.mconnDupBytes.Add(uint64(c.n * (fit - 1)))
		}
		return nil
	}
}

// pushAllLocked queues a record on every sending sub regardless of room
// (records that must not be lost: FIN, WANTBULK). Returns how many took it.
func (m *mConn) pushAllLocked(c *mchunk) int {
	n := 0
	if m.mode == planeBulk {
		m.bulk.push(c)
		return 1
	}
	for _, s := range m.subs {
		if !s.retired {
			s.push(c)
			n++
		}
	}
	return n
}

// noteWriteLocked runs heavy detection for this side's sending direction.
func (m *mConn) noteWriteLocked(n int) {
	now := time.Now()
	if w := now.Sub(m.dupStart); w > mHeavyWindow {
		// the window that just ended decides the next one (a gap of more
		// than two windows is a quiet connection: duplicate again)
		m.dupOff = m.dupBytes > mDupMaxRate && w < 2*mHeavyWindow
		m.dupStart, m.dupBytes = now, 0
	}
	m.dupBytes += n
	if m.dupBytes > mDupMaxRate {
		m.dupOff = true
	}
	if m.heavy || m.mode != planeLight {
		return
	}
	if now.Sub(m.hvStart) > mHeavyWindow {
		m.hvStart, m.hvBig, m.hvBytes = now, 0, 0
	}
	m.hvBytes += n
	if n >= mHeavyBigWrite {
		m.hvBig++
	}
	if (m.hvBig < mHeavyBigWrites || m.hvBytes < mHeavyBigMin) && m.hvBytes <= mHeavyRate {
		return
	}
	m.heavy = true
	if m.iran {
		m.wantBulk = true
		m.kickMgr()
		return
	}
	// Kharej cannot open subs: ask Iran for a bulk sub.
	c := getChunk(mRecWantBulk)
	if m.pushAllLocked(c) == 0 {
		putChunk(c)
	}
}

// CloseWrite ends this direction (FIN after the bytes already written); the
// other direction keeps flowing.
func (m *mConn) CloseWrite() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended {
		return m.writeErrLocked()
	}
	m.finLocked()
	return nil
}

func (m *mConn) finLocked() {
	if m.wclosed {
		return
	}
	m.wclosed = true
	c := getChunk(mRecFin)
	c.setOff(m.sendOff)
	if m.pushAllLocked(c) == 0 {
		putChunk(c)
	}
	m.sendCond.Broadcast()
}

// Close ends both directions: the bytes already written and a FIN are still
// sent (for up to mCloseLinger), the peer's bytes are no longer read.
func (m *mConn) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		if !m.ended {
			m.closing = true
			m.lingerAt = time.Now().Add(mCloseLinger)
			m.finLocked()
		}
		m.sendCond.Broadcast()
		m.mu.Unlock()
		r := &m.r
		r.mu.Lock()
		r.closed = true
		r.dropLocked()
		r.cond.Broadcast()
		r.mu.Unlock()
		m.kickMgr()
	})
	return nil
}

// abort resets the connection at once (RST to the peer).
func (m *mConn) abort() { m.end(errMConnAborted, true) }

func (s *msub) writeLoop() {
	m := s.m
	for {
		m.mu.Lock()
		for len(s.q) == 0 && !s.dead {
			if s.closeAfter {
				m.mu.Unlock()
				s.close()
				m.subDied(s, nil)
				return
			}
			m.mu.Unlock()
			select {
			case <-s.kick:
			case <-s.done:
			}
			m.mu.Lock()
		}
		if s.dead {
			m.mu.Unlock()
			return
		}
		c := s.q[0]
		s.q[0] = nil
		s.q = s.q[1:]
		s.qBytes -= c.qsize()
		s.cur = c
		m.sendCond.Broadcast()
		m.mu.Unlock()

		_, err := s.rw.Write(c.wire())

		m.mu.Lock()
		s.cur = nil
		idle := len(s.q) == 0 && m.closing
		m.mu.Unlock()
		c.unref()
		if idle {
			m.kickMgr()
		}
		if err != nil {
			m.subDied(s, err)
			return
		}
	}
}

// switchLocked makes b the only sending sub: everything still waiting on (or
// being written to) the light subs is queued on b too, in offset order and
// once, after a BULK record. Both sides use it (Iran on a new bulk sub,
// Kharej on the sub BULK arrived on, where BULK is the echo).
func (m *mConn) switchLocked(b *msub) {
	var seqs []*mchunk
	var ctrl []*mchunk
	for _, c := range b.q {
		if c.seq() {
			seqs = append(seqs, c)
		} else {
			ctrl = append(ctrl, c)
		}
	}
	b.q = nil
	for _, s := range m.subs {
		if s == b {
			continue
		}
		for _, c := range s.q {
			if c.seq() {
				seqs = append(seqs, c) // the queue's reference moves to b
			} else {
				c.unref()
			}
		}
		s.q, s.qBytes = nil, 0
		if c := s.cur; c != nil && c.seq() {
			c.refs.Add(1) // may not get through on s: send it again on b
			c.copies++
			seqs = append(seqs, c)
		}
	}
	slices.SortStableFunc(seqs, func(x, y *mchunk) int { return cmp.Compare(x.off, y.off) })
	bc := getChunk(mRecBulk)
	bc.refs.Store(1)
	q := append(ctrl, bc)
	qb := 0
	var prev *mchunk
	for _, c := range seqs {
		if c == prev || c == b.cur {
			c.copies--
			c.unref()
			continue
		}
		prev = c
		q = append(q, c)
		qb += c.qsize()
	}
	b.q, b.qBytes = q, qb
	select {
	case b.kick <- struct{}{}:
	default:
	}
	b.bulk.Store(true)
	m.mode, m.bulk = planeBulk, b
	m.setPrimaryLocked()
	m.sendCond.Broadcast()
	obs.mconnBulk.Add(1)
}

// ---- receive side ----

func (s *msub) readLoop() {
	m := s.m
	var err error
loop:
	for {
		if _, err = io.ReadFull(s.rw, s.hdr[:1]); err != nil {
			break
		}
		s.lastRecv.Store(time.Now().UnixNano())
		switch s.hdr[0] {
		case mRecData:
			if _, err = io.ReadFull(s.rw, s.hdr[1:mDataHdr]); err != nil {
				break loop
			}
			off := binary.BigEndian.Uint64(s.hdr[1:9])
			n := int(binary.BigEndian.Uint16(s.hdr[9:11]))
			if n == 0 || n > mChunkMax {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
			c := getChunk(mRecData)
			if _, err = io.ReadFull(s.rw, c.buf[:n]); err != nil {
				putChunk(c)
				break loop
			}
			m.deliver(s, off, c, n)
		case mRecFin:
			if _, err = io.ReadFull(s.rw, s.hdr[1:mFinLen]); err != nil {
				break loop
			}
			m.recvFin(binary.BigEndian.Uint64(s.hdr[1:9]))
		case mRecBulk:
			m.onBulk(s)
		case mRecWantBulk:
			m.onWantBulk()
		case mRecRst:
			err = errMConnReset
			m.end(err, false)
			break loop
		default:
			err = errMConnProto
			m.end(err, true)
			break loop
		}
	}
	m.subDied(s, err)
}

func (r *mrecv) dropLocked() {
	for i := r.head; i < len(r.ready); i++ {
		putChunk(r.ready[i].c)
	}
	for _, e := range r.ooo {
		putChunk(e.c)
	}
	clear(r.ready)
	clear(r.ooo)
	r.ready, r.head, r.readyBytes = r.ready[:0], 0, 0
	r.ooo, r.oooBytes = r.ooo[:0], 0
}

// deliver hands one received DATA payload (c.buf[:n] at off) to reassembly,
// waiting while the buffer it would go to is full. It owns c.
func (m *mConn) deliver(s *msub, off uint64, c *mchunk, n int) {
	r := &m.r
	r.mu.Lock()
	for {
		if r.closed || r.dead {
			r.mu.Unlock()
			putChunk(c)
			return
		}
		end := off + uint64(n)
		if r.fin && end > r.finOff { // nothing exists past FIN
			if off >= r.finOff {
				r.mu.Unlock()
				putChunk(c)
				return
			}
			n = int(r.finOff - off)
			end = r.finOff
		}
		if end <= r.next || r.coveredLocked(off, end) {
			r.mu.Unlock()
			putChunk(c) // the other copy came first
			return
		}
		if off <= r.next {
			if r.readyBytes >= mRecvBuf {
				r.cond.Wait() // the app is behind: hold the sub
				continue
			}
			r.appendReadyLocked(rchunk{c: c, off: r.next, b: c.buf[r.next-off : n]})
			r.pullLocked()
			r.lastProgress = time.Now()
			r.cond.Broadcast()
			r.mu.Unlock()
			m.noteSaved(s)
			return
		}
		if r.oooBytes >= mRecvBuf {
			r.cond.Wait() // too far ahead of the gap: hold this sub
			continue
		}
		wasEmpty := len(r.ooo) == 0
		if wasEmpty {
			r.lastProgress = time.Now()
		}
		r.insertLocked(rchunk{c: c, off: off, b: c.buf[:n]})
		r.mu.Unlock()
		m.noteSaved(s)
		if wasEmpty {
			m.kickMgr() // watch the gap
		}
		return
	}
}

// noteSaved counts a chunk that arrived first on a light sub other than the
// first one: the second copy beat the first.
func (m *mConn) noteSaved(s *msub) {
	if !s.bulk.Load() && m.primary.Load() != s {
		obs.mconnSaved.Add(1)
	}
}

// coveredLocked: [off,end) is entirely inside one out-of-order entry.
func (r *mrecv) coveredLocked(off, end uint64) bool {
	i := sort.Search(len(r.ooo), func(i int) bool { return r.ooo[i].off > off })
	if i == 0 {
		return false
	}
	p := r.ooo[i-1]
	return p.off+uint64(len(p.b)) >= end
}

func (r *mrecv) appendReadyLocked(e rchunk) {
	if r.head > 64 && r.head*2 > len(r.ready) {
		k := copy(r.ready, r.ready[r.head:])
		clear(r.ready[k:])
		r.ready, r.head = r.ready[:k], 0
	}
	r.ready = append(r.ready, e)
	r.readyBytes += len(e.b)
	r.next = e.off + uint64(len(e.b))
}

// pullLocked moves out-of-order entries that became contiguous into ready.
func (r *mrecv) pullLocked() {
	k := 0
	for ; k < len(r.ooo); k++ {
		e := r.ooo[k]
		if e.off > r.next {
			break
		}
		r.oooBytes -= len(e.b)
		end := e.off + uint64(len(e.b))
		if end <= r.next {
			putChunk(e.c)
			continue
		}
		e.b = e.b[r.next-e.off:]
		e.off = r.next
		r.appendReadyLocked(e)
	}
	if k > 0 {
		n := copy(r.ooo, r.ooo[k:])
		clear(r.ooo[n:])
		r.ooo = r.ooo[:n]
	}
}

// insertLocked adds an out-of-order entry (e.off > next), trimming overlaps
// so the entries stay disjoint.
func (r *mrecv) insertLocked(e rchunk) {
	i := sort.Search(len(r.ooo), func(i int) bool { return r.ooo[i].off > e.off })
	if i > 0 {
		p := r.ooo[i-1]
		if pend := p.off + uint64(len(p.b)); pend > e.off {
			e.b = e.b[pend-e.off:] // coveredLocked ruled out pend >= end
			e.off = pend
		}
	}
	end := e.off + uint64(len(e.b))
	j := i
	for j < len(r.ooo) && r.ooo[j].off+uint64(len(r.ooo[j].b)) <= end {
		r.oooBytes -= len(r.ooo[j].b)
		putChunk(r.ooo[j].c)
		j++
	}
	if j < len(r.ooo) && r.ooo[j].off < end {
		e.b = e.b[:r.ooo[j].off-e.off]
	}
	if len(e.b) == 0 { // the neighbours already hold all of it
		r.ooo = slices.Delete(r.ooo, i, j)
		putChunk(e.c)
		return
	}
	r.oooBytes += len(e.b)
	r.ooo = slices.Replace(r.ooo, i, j, e)
}

func (m *mConn) recvFin(off uint64) {
	r := &m.r
	r.mu.Lock()
	if !r.fin && off >= r.next {
		r.fin, r.finOff = true, off
		r.lastProgress = time.Now()
		r.cond.Broadcast()
	}
	missing := r.fin && r.next < r.finOff
	r.mu.Unlock()
	m.mu.Lock()
	m.peerFin = true
	m.mu.Unlock()
	if missing {
		m.kickMgr()
	}
}

// Read returns the peer's bytes in order; io.EOF after its FIN.
func (m *mConn) Read(p []byte) (int, error) {
	r := &m.r
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.readyBytes == 0 {
		switch {
		case r.closed:
			return 0, io.ErrClosedPipe
		case r.fin && r.next >= r.finOff:
			return 0, io.EOF
		case r.err != nil:
			return 0, r.err
		}
		r.cond.Wait()
	}
	n := 0
	for n < len(p) && r.head < len(r.ready) {
		e := &r.ready[r.head]
		k := copy(p[n:], e.b)
		n += k
		e.b = e.b[k:]
		if len(e.b) == 0 {
			putChunk(e.c)
			*e = rchunk{}
			r.head++
		}
	}
	r.readyBytes -= n
	if r.head == len(r.ready) {
		r.ready, r.head = r.ready[:0], 0
	}
	r.cond.Broadcast() // room for a held reader
	return n, nil
}

func (m *mConn) onBulk(s *msub) {
	m.mu.Lock()
	if m.ended || m.closing {
		m.mu.Unlock()
		return
	}
	if m.iran {
		if s == m.bulk && m.echoAt.IsZero() {
			m.echoAt = time.Now() // both sides send only on the bulk sub now
			m.mu.Unlock()
			m.kickMgr()
			return
		}
		m.mu.Unlock()
		return
	}
	if m.mode == planeBulk && m.bulk == s {
		m.mu.Unlock()
		return
	}
	m.switchLocked(s) // queues the echo first
	m.mu.Unlock()
}

func (m *mConn) onWantBulk() {
	if !m.iran {
		return
	}
	m.mu.Lock()
	want := m.mode == planeLight && !m.wantBulk
	m.wantBulk = true
	m.mu.Unlock()
	if want {
		m.kickMgr()
	}
}

// ---- sub death and the end ----

func (m *mConn) subDied(s *msub, cause error) {
	_ = cause
	s.close()
	m.mu.Lock()
	if s.dead {
		m.mu.Unlock()
		return
	}
	s.dead = true
	m.removeSubLocked(s)
	lost := false
	for _, c := range s.q {
		if c.seq() {
			if c.copies--; c.copies <= 0 {
				lost = true
			}
		}
		c.unref()
	}
	s.q, s.qBytes = nil, 0
	if c := s.cur; c != nil && c.seq() {
		if c.copies--; c.copies <= 0 {
			lost = true // the writer still owns its reference
		}
	}
	m.sendCond.Broadcast()
	if m.ended || s.retired {
		m.mu.Unlock()
		return
	}
	var endErr error
	rst := false
	switch {
	case m.mode == planeBulk && s == m.bulk:
		endErr, rst = errMConnBulkDied, true
	case len(m.subs) == 0:
		endErr = errMConnSubsDied
	case m.mode == planeBulk:
		// a light sub of a switched connection: nothing rides on it now
	case lost || m.holeRecentLocked():
		endErr, rst = errMConnLost, true
	default:
		if !m.peerFin && !m.closing {
			obs.mconnSubSurvived.Add(1)
		}
		if s.key != nil {
			m.deadKeys = append(m.deadKeys, s.key)
			if len(m.deadKeys) > mDeadKeys {
				m.deadKeys = m.deadKeys[1:]
			}
		}
	}
	m.mu.Unlock()
	if endErr != nil {
		m.end(endErr, rst)
		return
	}
	m.kickMgr()
}

// holeRecentLocked: a live sub missed a copy within mHoleSafe.
func (m *mConn) holeRecentLocked() bool {
	now := time.Now()
	for _, s := range m.subs {
		if !s.lastHole.IsZero() && now.Sub(s.lastHole) < mHoleSafe {
			return true
		}
	}
	return false
}

// end finishes the connection once: subs are closed (after an RST when rst),
// Read returns what it holds and then err, Write fails, the die channel
// closes.
func (m *mConn) end(err error, rst bool) {
	m.mu.Lock()
	if m.ended {
		m.mu.Unlock()
		return
	}
	m.ended, m.err = true, err
	var closeNow []*msub
	for _, s := range m.subs {
		if !rst || s.dead {
			closeNow = append(closeNow, s)
			continue
		}
		for _, c := range s.q {
			c.unref()
		}
		s.q, s.qBytes = nil, 0
		s.push(getChunk(mRecRst))
		s.closeAfter = true
		time.AfterFunc(mRSTWait, s.close)
	}
	m.sendCond.Broadcast()
	m.mu.Unlock()
	for _, s := range closeNow {
		s.close()
	}
	r := &m.r
	r.mu.Lock()
	r.dead = true
	if r.err == nil {
		r.err = err
	}
	finished := r.fin
	r.dropOOOLocked()
	r.cond.Broadcast()
	r.mu.Unlock()
	close(m.die)
	obs.mconnOpen.Add(-1)
	if err != errMConnClosed && !finished {
		obs.mconnFailed.Add(1)
	}
}

// dropOOOLocked frees the out-of-order bytes (no gap will be filled now);
// ready bytes stay for Read.
func (r *mrecv) dropOOOLocked() {
	for _, e := range r.ooo {
		putChunk(e.c)
	}
	clear(r.ooo)
	r.ooo, r.oooBytes = r.ooo[:0], 0
}

// ---- manager ----

func (m *mConn) manage() {
	t := time.NewTimer(time.Hour)
	t.Stop()
	defer t.Stop()
	for {
		select {
		case <-m.die:
			return
		case <-m.kick:
		case <-t.C:
		}
		if d := m.tick(time.Now()); d > 0 {
			t.Reset(d)
		} else {
			t.Stop()
		}
	}
}

// tick does the timed and deferred work; it returns when to look again (0:
// only when kicked).
func (m *mConn) tick(now time.Time) time.Duration {
	var wake time.Duration
	at := func(d time.Duration) {
		if d <= 0 {
			d = time.Millisecond
		}
		if wake == 0 || d < wake {
			wake = d
		}
	}
	if stalled, pending := m.gapCheck(now); stalled {
		m.end(errMConnGap, true)
		return 0
	} else if pending {
		at(mGapPoll)
	}

	m.mu.Lock()
	if m.ended {
		m.mu.Unlock()
		return 0
	}
	if m.closing {
		idle := m.idleLocked()
		switch {
		case len(m.subs) == 0 || idle:
			m.mu.Unlock()
			m.end(errMConnClosed, false)
			return 0
		case !now.Before(m.lingerAt):
			m.mu.Unlock()
			m.end(errMConnClosed, true) // bytes left unsent: tell the peer
			return 0
		}
		at(m.lingerAt.Sub(now))
		m.mu.Unlock()
		return wake
	}
	var retire []*msub
	if m.iran {
		if m.mode == planeLight {
			if m.wantBulk && !m.bulkOpening {
				if now.Before(m.bulkRetryAt) {
					at(m.bulkRetryAt.Sub(now))
				} else {
					m.bulkOpening = true
					go m.openBulk()
				}
			}
			if !m.wantBulk && !m.peerFin && !m.lightOpening && len(m.subs) < 2 {
				if now.Before(m.lightRetryAt) {
					at(m.lightRetryAt.Sub(now))
				} else {
					m.lightOpening = true
					avoid := make([]any, 0, len(m.subs)+len(m.deadKeys))
					for _, s := range m.subs {
						avoid = append(avoid, s.key)
					}
					avoid = append(avoid, m.deadKeys...)
					go m.openLight(avoid)
				}
			}
		} else if !m.retired && !m.echoAt.IsZero() {
			if m.canRetireLocked(now) {
				m.retired = true
				for _, s := range m.subs {
					if s != m.bulk {
						s.retired = true
						retire = append(retire, s)
					}
				}
				m.setPrimaryLocked()
			} else {
				at(mRetirePoll)
			}
		}
	}
	m.mu.Unlock()
	for _, s := range retire {
		s.close()
	}
	return wake
}

// idleLocked: nothing waits on or is being written to any live sub.
func (m *mConn) idleLocked() bool {
	for _, s := range m.subs {
		if len(s.q) > 0 || s.cur != nil {
			return false
		}
	}
	return true
}

// canRetireLocked: the light subs carry nothing any more (Iran, bulk mode).
func (m *mConn) canRetireLocked(now time.Time) bool {
	if now.Sub(m.echoAt) < mRetireAfter {
		return false
	}
	quiet := now.Add(-mRetireAfter).UnixNano()
	for _, s := range m.subs {
		if s == m.bulk {
			continue
		}
		if len(s.q) > 0 || s.cur != nil || s.lastRecv.Load() > quiet {
			return false
		}
	}
	r := &m.r
	r.mu.Lock()
	gap := len(r.ooo) > 0
	r.mu.Unlock()
	return !gap
}

// gapCheck: whether bytes are missing (pending) and have been for too long
// while the app was keeping up (stalled).
func (m *mConn) gapCheck(now time.Time) (stalled, pending bool) {
	r := &m.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.dead {
		return false, false
	}
	pending = len(r.ooo) > 0 || (r.fin && r.next < r.finOff)
	if !pending || r.readyBytes >= mRecvBuf {
		r.lastProgress = now
		return false, pending
	}
	return now.Sub(r.lastProgress) > mGapTimeout, true
}

func (m *mConn) openLight(avoid []any) {
	sub, key, err := m.open(planeLight, avoid)
	m.mu.Lock()
	m.lightOpening = false
	if err != nil {
		m.lightRetryAt = time.Now().Add(mReopenBackoff)
		m.deadKeys = nil // maybe only those links were left
		m.mu.Unlock()
		m.kickMgr()
		return
	}
	if m.ended || m.closing || m.mode != planeLight || m.wantBulk {
		m.mu.Unlock()
		sub.Close()
		m.kickMgr()
		return
	}
	s := m.addSubLocked(sub, key, false)
	m.sendCond.Broadcast()
	m.mu.Unlock()
	m.startSub(s)
}

func (m *mConn) openBulk() {
	sub, key, err := m.open(planeBulk, nil)
	m.mu.Lock()
	m.bulkOpening = false
	if err != nil {
		m.bulkRetryAt = time.Now().Add(mReopenBackoff)
		m.mu.Unlock()
		m.kickMgr()
		return
	}
	if m.ended || m.closing || m.mode != planeLight {
		m.mu.Unlock()
		sub.Close()
		return
	}
	s := m.addSubLocked(sub, key, true)
	m.switchLocked(s) // BULK first, then what the light subs still held
	m.mu.Unlock()
	m.startSub(s)
}
