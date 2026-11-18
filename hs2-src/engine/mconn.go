package engine

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
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
//	UPGRADE  0x06 | off u64   raw first sub: my raw bytes on it end at off (mconn_raw.go)
//	UPACK    0x07     raw first sub: Iran knows the peer's boundary
//	CLOSE-REQ  0x0a             lite: Iran wants to close this idle sub (mconn_closehs.go)
//	CLOSE-ACK  0x0b | end u64   Kharej: my bytes on this sub end at end; nothing more on it
//	CLOSE-DONE 0x0c             Iran: all read, the sub closes
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

// mSendState is what a connection's sending direction does right now, for
// the obs gauges (one connection is in exactly one state until it ends).
type mSendState uint8

const (
	mStateDual   mSendState = iota // light mode, every chunk on two subs
	mStateSingle                   // light mode over mDupMaxRate: one sub only
	mStateHeavy                    // heavy: on a bulk sub, or waiting for one
	mStateEnded
)

func (st mSendState) gauge() *atomic.Int64 {
	switch st {
	case mStateDual:
		return &obs.mconnDual
	case mStateSingle:
		return &obs.mconnSingle
	case mStateHeavy:
		return &obs.mconnHeavy
	}
	return nil
}

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
	mRecUpgrade  = 0x06 // raw first sub (lite): see mconn_raw.go
	mRecUpAck    = 0x07

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
	// every sending sub is full, Write blocks (backpressure). v3's queue
	// rules (heavy by queue, stuck primary) are tuned to this size.
	mSubQueue = 256 << 10
	// mSubQueueLite: lite (HS2_MCONN=lite) keeps two chunks. smux's stream
	// window already pipelines a link; anything queued here is extra delay
	// in front of it — 256 KiB was ~1 s for a 2 Mbit/s call on a slowed link
	// (the call regression in every lab run).
	mSubQueueLite = 32 << 10
	// mRecvBuf bounds the receiver: the in-order bytes Read has not taken
	// yet, and separately the out-of-order bytes waiting for a gap. Beyond
	// it the subs' readers stop reading (backpressure into smux).
	mRecvBuf = 4 << 20

	// Heavy detection, per sending direction. A direction is heavy when its
	// source keeps getting ahead of the link: the send queue of the sub
	// carrying it (bytes waiting plus the record being written) stays above
	// mHeavyQueue for mHeavyQueueFor without a break. A relay only has bytes
	// to queue when its socket had them, so a standing queue means the
	// source outruns the light link — a download at any rate (a 3 Mbit/s one
	// on a loaded link too), while a call's or a game's queue drains between
	// frames even when it reads a few big chunks after a hiccup. A sustained
	// rate (more than mHeavyRate a window) is the second way: a source a fast
	// link still keeps up with, but that would load it.
	mHeavyQueue    = 64 << 10 // queue above this ...
	mHeavyQueueFor = time.Second
	mHeavyRate     = 1_000_000   // or more than this many bytes (8 Mbit/s)
	mHeavyWindow   = time.Second // ... within this window
	// Fast start: a direction that has already sent mFastStartBytes within
	// mFastStartWindow of its first write is a download (or a reel) from its
	// first moment, and is heavy at once — it skips the mHeavyQueueFor wait
	// of the queue rule and the light-mode start of a connection that will
	// move anyway (the move itself is cheapest while little is queued). A
	// steady 2 Mbit/s call sends ~75 KB in the window; a 3 Mbit/s one ~115.
	mFastStartBytes  = 256 << 10
	mFastStartWindow = 300 * time.Millisecond
	// Primary switch (light mode, one copy): when the primary sub's queue has
	// stayed above mSwitchQueue for mSwitchQueueFor with no record written on
	// it meanwhile (its link is stuck in loss recovery, not merely slow) and
	// another light sub has at most mSwitchOtherMax queued, that sub becomes
	// the primary: the next chunks go on it, and what the stuck sub holds is
	// sent on it too (the receiver delivers in offset order, so a gap left
	// on the stuck link would hold everything after it; the stuck sub still
	// writes its copies when released, dropped as duplicates). At most one
	// switch per mSwitchMinGap, so the subs never flip-flop.
	mSwitchQueue    = 16 << 10
	mSwitchQueueFor = 150 * time.Millisecond
	mSwitchOtherMax = 8 << 10
	mSwitchMinGap   = time.Second
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
	// mEndSubGrace (mem66): after the end, a sub left open for a reader
	// (the raw first sub) is closed after this long at the latest.
	mEndSubGrace = 3 * time.Second

	// mGapTimeout: missing bytes the receiver has waited for this long
	// (while not held back by its own app) are lost; the connection resets
	// instead of hanging.
	mGapTimeout = 30 * time.Second
	mGapPoll    = time.Second

	// mDeadKeys: recently dead light links avoided by a replacement open.
	mDeadKeys = 4

	// lite (HS2_MCONN=lite, adaptive redundancy): a connection starts on one
	// sub (where the old path would put its one stream) and gets a second,
	// duplicating, sub only once it is mLiteAfter old and both directions
	// moved at most mDupMaxRate in the last window (sampled every mLitePoll
	// by the Iran manager); above mDupMaxRate it sends on the first sub only
	// and never moves to bulk. A second sub idle both ways (no chunk queued
	// on it, none received on it) for mLiteIdle is closed by Iran. New upgrades stop while the duplicated
	// bytes of the last mLiteCapWindow seconds exceed mLiteCapPct of all
	// bytes the process's mconn connections sent.
	mLiteAfter     = time.Second
	mLitePoll      = time.Second
	mLiteCapPct    = 5
	mLiteCapWindow = 5
)

// mLiteIdle: see the lite constants above (a variable so tests can shorten
// it). It must stay over mHoleSafe: by then the sub left has had time to
// take a copy of everything, and its death costs the peer nothing.
var mLiteIdle = 10 * time.Second

// liteBudget is the process-wide duplication budget of lite connections: per
// second buckets (by unix second, 8 slots) of all bytes sent and of the
// duplicate copies among them. Lock-free; a bucket's reset on rollover may
// drop a few bytes of a concurrent adder, which the 5% rule tolerates.
type liteBudget struct {
	slots [8]struct {
		sec      atomic.Int64
		all, dup atomic.Uint64
	}
}

var liteBud liteBudget

func (b *liteBudget) add(now int64, all, dup int) {
	s := &b.slots[now&7]
	if old := s.sec.Load(); old != now && s.sec.CompareAndSwap(old, now) {
		s.all.Store(0)
		s.dup.Store(0)
	}
	s.all.Add(uint64(all))
	s.dup.Add(uint64(dup))
}

// capped: duplicates exceed mLiteCapPct of all bytes over the last
// mLiteCapWindow seconds (an idle process is never capped).
func (b *liteBudget) capped(now int64) bool {
	var all, dup uint64
	for i := range b.slots {
		s := &b.slots[i]
		if sec := s.sec.Load(); sec > now-int64(mLiteCapWindow) && sec <= now {
			all += s.all.Load()
			dup += s.dup.Load()
		}
	}
	return all > 0 && dup*100 > all*mLiteCapPct
}

var (
	errMConnReset    = errors.New("mconn: reset by peer")
	errMConnAborted  = errors.New("mconn: aborted")
	errMConnBulkDied = errors.New("mconn: bulk sub died")
	errMConnSubsDied = errors.New("mconn: every sub died")
	// errMConnPeerDone (up56): every sub ended after the peer's FIN - the far
	// end closed normally (its server closed before the bytes in flight
	// arrived), not a dead path; the log said "every sub died" for both.
	errMConnPeerDone = errors.New("mconn: peer closed (every sub ended after its FIN)")
	errMConnLost     = errors.New("mconn: a sub died holding the only copy of some bytes")
	errMConnProto    = errors.New("mconn: bad record")
	errMConnGap      = errors.New("mconn: missing bytes never arrived")
	errMConnClosed   = io.ErrClosedPipe
)

// mchunk is one record on the send side (shared by every sub queue that holds
// it, reference counted) or one received payload. Pooled, in size classes.
type mchunk struct {
	buf  []byte // len is the chunk's size class (mChunkClasses)
	kind byte
	off  uint64
	n    int // payload bytes (DATA)

	refs   atomic.Int32 // send side: queues and writers holding it
	copies int          // send side, under mConn.mu: subs holding or having written it
}

// mChunkClasses: the chunk sizes. A chunk used to be one 16 KB buffer
// whatever it carried, and a chunk is held as long as its bytes wait (in a
// sub's send queue, a writer's batch, the receiver's ready or out-of-order
// list). The bounds there count payload bytes, so small records were the
// costly case: a 100-byte write queued on a lite sub's 32 KB queue held
// ~16.4 KB (18 KB with the allocator's size class), and the Iran side of a
// busy server (3000 users writing small messages, link kills that make every
// queue stand) held hundreds of MB of mostly empty chunks; their peak set the
// heap goal and the RSS grew with each new peak (mem55). A chunk now takes
// the smallest class that fits its record.
var mChunkClasses = [...]int{512, 2 << 10, 8 << 10, mRecMax}

var mchunkPools [len(mChunkClasses)]sync.Pool

func init() {
	for i := range mchunkPools {
		size := mChunkClasses[i]
		mchunkPools[i].New = func() any { return &mchunk{buf: make([]byte, size)} }
	}
}

// chunkClass: the index of the smallest class holding size bytes.
func chunkClass(size int) int {
	for i, c := range mChunkClasses {
		if size <= c {
			return i
		}
	}
	return len(mChunkClasses) - 1
}

// getChunk: a chunk for a control record (FIN-sized at most), or a full one
// for DATA (Write and the readers size theirs with getChunkN).
func getChunk(kind byte) *mchunk {
	if kind == mRecData {
		return getChunkN(kind, mRecMax)
	}
	return getChunkN(kind, mFinLen)
}

// getChunkN: a chunk whose buf holds at least size bytes (at most mRecMax).
func getChunkN(kind byte, size int) *mchunk {
	c := mchunkPools[chunkClass(size)].Get().(*mchunk)
	c.kind, c.off, c.n, c.copies = kind, 0, 0, 0
	c.refs.Store(0)
	c.buf[0] = kind
	return c
}

func putChunk(c *mchunk) {
	if c != nil { // nil: a compacted receive entry (compactLocked)
		mchunkPools[chunkClass(len(c.buf))].Put(c)
	}
}

// mCompactMax: a received record this small that has to wait (the app or the
// gap is behind) is copied out of its 16 KB chunk. The receive limits
// (mRecvBuf, oooMax) count payload bytes, so without it a reader that stops
// while the other end writes small messages (a backgrounded phone, a push
// channel, a destination that does not read an upload) held one 16 KB chunk
// per message: 100 B records made 4 MB of payload pin ~650 MB.
const mCompactMax = 4 << 10

// compactRecv returns the entry for payload b of chunk c: c itself, or (b
// small and others already waiting) a right-sized copy with c returned.
func compactRecv(c *mchunk, off uint64, b []byte, waiting bool) rchunk {
	if !waiting || len(b) > mCompactMax {
		return rchunk{c: c, off: off, b: b}
	}
	nb := make([]byte, len(b))
	copy(nb, b)
	putChunk(c)
	return rchunk{off: off, b: nb}
}

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
	case mRecFin, mRecUpgrade, mRecCloseAck, mRecXS:
		return c.buf[:mFinLen]
	case mRecRescue, mRecRescueAck, mRecRescueNak, mRecRRescue, mRecRRescueAck, mRecRRescueNak, mRecRstRsc, mRecRstRscAck:
		return c.buf[:mRescueLen]
	}
	return c.buf[:1]
}

// msub is one sub of an mConn.
type msub struct {
	m   *mConn
	rw  mSub
	key any
	idx int // join order, for the trace
	// dcPre: this tick's darkcall link evidence, read with m.mu released
	// (cs228, mconn_cs228.go); under m.mu
	dcPre *dcPreRead
	// b296: dcSubMeter(s) taken once at addSubLocked (s.rw is fixed while live);
	// the darkcall timer reads it instead of 3 pointer hops per sub per tick
	dcMtr *linkMeter

	bulk     atomic.Bool  // this is the connection's bulk sub
	lastRecv atomic.Int64 // UnixNano of the last record read from it

	// under m.mu
	q          []*mchunk
	qBytes     int
	cur        *mchunk   // being written now
	curMore    []*mchunk // HS2_MCONN_BATCH: the records written with cur in one batch
	batch      []byte    // HS2_MCONN_BATCH: the batch buffer (writer only)
	dead       bool
	retired    bool // closed on purpose (a light sub after the switch)
	rrAband    bool // er97: abandoned by a records rescue (its bytes are resent; its death ends nothing)
	closeAfter bool // close once the queue (an RST) is written
	lastHole   time.Time
	lastPush   time.Time     // lite: when a chunk was last queued on it (idle rule)
	gated      bool          // raw upgrade: queue but do not write yet (mconn_raw.go)
	closeHS    bool          // in the close handshake (mconn_closehs.go)
	closeDone  bool          // the handshake is over (Iran: ACK read; Kharej: DONE read)
	txEnd      uint64        // end offset of the last byte placed on it (CLOSE-ACK)
	stripe     bool          // carries the stripe (HS2_MCONN_STRIPE, mconn_stripe.go)
	stJoinAt   time.Time     // dl240 (exit): when it joined the stripe (HS2_DL240_YOUNG, mconn_dl240.go)
	dl242      dl242Sub      // dl242 (exit): young-sub first-chunk rescue (HS2_DL242_RESCUE, mconn_dl242.go)
	stPushed   time.Time     // the stripe rule last put a chunk on it (HS2_MCONN_STRIPE_UP)
	lastWarm   time.Time     // Iran: the last keep-warm STRIPE record (HS2_MCONN_STRIPE_SHARE)
	sent       []sentRec     // HS2_MCONN_STRIPE_RESEND: chunks it wrote recently (mconn_stripe_resend.go)
	subGapT    time.Time     // r68fix4: newest chunk it wrote without a kept copy, not yet delivered (mconn_sub_resend.go)
	noTgt      bool          // b271 Q5: died with no fresh resend target (mconn_sub_resend.go)
	subSrtt    time.Duration // r68fix4: its link's srtt at the last keep tick (0: unknown)
	ks         ksSub         // HS2_MCONN_STRIPE_KEEPSTALL: ack progress (mconn_stripe_keepstall.go)
	dc         dcSub         // HS2_MCONN_DARKCALL: link evidence and the in-flight cap (mconn_darkcall.go)
	missEnd    uint64        // cs221: end offset of the newest DATA chunk it was not given (mconn_cs221.go)
	unprovEnd  uint64        // cs223: end offset of the newest DATA chunk it let go without proof of delivery (mconn_cs223.go)
	gaveTo     *msub         // cs223: given up, its chunks were queued on gaveTo ...
	gaveEnd    uint64        // ... up to this end offset
	rsLo, rsHi uint64        // cs223: the range the last darkcall switch resent onto it
	dcLeft     bool          // cs224: a darkcall switch left it and it has stayed dark since
	b270ResAt  time.Time     // b270: last txDark resend look (mconn_b270.go)
	b270End    uint64        // b270: end of the chunks a txDark resend already covered
	b270Trig   bool          // b270: half dead at the last visit
	wrLo, wrHi uint64        // b262: the newest contiguous DATA range its writer handed to its stream (mconn_b262.go)
	// under m.r.mu (hs119, HS2_MCONN_STRIPE_KEEPSTALL): bytes this sub put
	// past every reorder limit for the hole at r.next == hxNext
	hxNext  uint64
	hxBytes int

	// stMtr (exit): the link meter whose stripeN this stripe sub raised
	// (released in close).
	stMtr atomic.Pointer[linkMeter]

	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	onClose   func() // after close (Kharej's raw first sub: its link counters)

	hdr   [mDataHdr]byte // reader only
	rxEnd uint64         // reader (then under m.mu): end offset of the last byte read on it

	tr       mtraceSub // HS2_MCONN_TRACE per-sub counters (under m.r.mu)
	callLate bool      // HS2_LB_CALL_LATE: its call copy is late, no pair copy on it (under m.mu)

	loops    int         // mem68: its readLoop/writeLoop running (under m.mu)
	rwClosed atomic.Bool // mem68: close has finished closing rw
	u268Idx  atomic.Bool // in ul268's link index (mconn_ul268_scale.go)
}

func (s *msub) close() {
	s.closeOnce.Do(func() {
		obs.msubsLive.Add(-1)
		ul268IdxDel(s) // ul268 link index
		close(s.done)
		if mtr := s.stMtr.Swap(nil); mtr != nil {
			mtr.stripeN.Add(-1)
		}
		s.rw.Close()
		if s.onClose != nil {
			s.onClose()
		}
		s.rwClosed.Store(true)
	})
}

// push queues c on s (under m.mu).
func (s *msub) push(c *mchunk) {
	c.refs.Add(1)
	if c.seq() {
		c.copies++
		if e := c.off + uint64(c.n); e > s.txEnd {
			s.txEnd = e
		}
	}
	s.q = append(s.q, c)
	s.qBytes += c.qsize()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// fits: s has room for n more bytes (under m.mu).
// coldLocked (lite): nothing queued on s and nothing received on it since
// before t — Iran's idle rule (mLiteIdle, both ways) may close it soon.
func (s *msub) coldLocked(t time.Time) bool {
	return s.qBytes == 0 && s.cur == nil && s.lastPush.Before(t) && s.lastRecv.Load() < monoNano(t)
}

func (s *msub) fits(n int) bool {
	max := mSubQueue
	if s.m.lite {
		max = mSubQueueLite
	}
	if s.dc.full && s.dc.fullFresh() { // HS2_MCONN_DARKCALL in-flight cap (mconn_darkcall.go)
		return false
	}
	return s.qBytes == 0 || s.qBytes+n <= max
}

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
	oooMax     int // out-of-order bound (0: mRecvBuf; a stripe raises it, mconn_stripe.go)
	// HS2_MCONN_STRIPE_UP (mconn_stripe_up.go): the bound may grow on demand
	// (oooGrow); oooExtra is the room taken from the process's extra,
	// oooSlot an exit receiver's stripe slot.
	oooGrow  bool
	oooExtra int
	oooSlot  bool

	fin    bool
	finOff uint64

	closed bool  // local Close: Read fails, arrivals are dropped
	dead   bool  // the mConn ended: arrivals are dropped
	err    error // why it ended (Read returns it once ready is drained)

	lastProgress time.Time // gap watch: last delivery (or app-held) moment
	hxNext       uint64    // hs119: the hole hxBytes counts for (HS2_MCONN_STRIPE_KEEPSTALL)
	hxBytes      int       // ... bytes all subs put past every limit for it
	ksX          int       // hs133: out-of-order bytes over the normal bound, charged to holeRoomBytes
}

// mConn is one user connection over several subs. It implements
// io.ReadWriteCloser and GetDieCh, as relayStream expects of a stream.
type mConn struct {
	id   uint64
	iran bool
	// ul268 vs darkcall (mconn_ul268_scale.go), under mu
	u268At time.Time // ul268 started a data-dark rescue of this conn
	dcSwAt time.Time // darkcall switched this conn's primary
	// b296 (mconn_b296.go): the subs' write sequence and the darkcall
	// timer's idle proof (seq+1; 0: none), side by side for the timer's scan
	dcWr     atomic.Uint64
	dcIdleAt atomic.Uint64

	open mOpener

	dl243Res [dl243ConnMax]time.Time // dl243: this conn's recent rescues (under mu)

	mu       sync.Mutex
	sendCond sync.Cond // Write waits here for queue room (L = &mu)

	subs      []*msub // live subs in join order
	mode      mPlane
	bulk      *msub
	sendOff   uint64
	keptBytes int       // HS2_MCONN_STRIPE_RESEND: bytes kept in the subs' sent lists
	subScoped bool      // r68fix3: went over HS2_MCONN_SUB_RESEND_SCOPE_KB (counted once)
	ksNext    time.Time // HS2_MCONN_STRIPE_KEEPSTALL: no ack-stall round before
	ksTicking bool      // ... the manager polls the ack-stall rule
	ksIdle    bool      // mb174: the manager's slow keep poll is armed (keepIdleLocked)
	ksSwept   bool      // fx212: in the shared keep sweeper (subSweepConns) instead of the manager's poll
	ksLookAt  time.Time // hs128 (lazy): the writers' last suspect look

	swKept atomic.Bool // sw235: the swept conn kept a chunk or opened a gap since the sweeper last found it quiet

	// HS2_MCONN_STRIPE_GRACE_MS (mconn_stripe_resend.go): the last sub died;
	// what it held waits here for the next sub until graceUntil
	orphan       []*mchunk
	graceUntil   time.Time
	graceRetryAt time.Time
	stallLogAt   time.Time // HS2_MCONN_STRIPE_LOG: the last stall line
	wclosed      bool      // FIN queued
	closing      bool      // Close called; lingering until queues drain
	ended        bool
	err          error
	peerFin      bool

	// heavy detection (this side's sending direction): the rate window, and
	// since when the carrying sub's queue has been above mHeavyQueue
	hvStart    time.Time
	hvBytes    int
	qHighSince time.Time
	heavy      bool
	state      mSendState // what the obs gauges count this connection as
	// fast start: the first write's moment and the bytes written since,
	// until mFastStartWindow has passed (startDone)
	startAt    time.Time
	startBytes int
	startDone  bool
	// primary switch: the sub chosen over the join order (nil: the first
	// light sub), since when the primary's queue has been high with nothing
	// written, and the last switch
	prefPrimary *msub
	pStuckSince time.Time
	lastPSwitch time.Time
	dc          dcConn    // HS2_MCONN_DARKCALL (mconn_darkcall.go), under mu
	missLost    bool      // cs221: a chunk went out by the bulk or stripe rule (subs' missEnd not kept)
	b262RepAt   time.Time // b262 replace: the last stale-backlog sub closed (mconn_b262rep.go)
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

	// lite (HS2_MCONN=lite): the mode, whether the connection counts as dual
	// in the gauge now, and the Iran manager's last rate sample (when, and
	// the send and receive offsets then) for the upgrade rule.
	lite       bool
	liteDual   bool
	liteSample time.Time
	liteSent   uint64
	liteRecvd  uint64

	tr    *mtrace   // HS2_MCONN_TRACE (nil when off)
	trRx  *mtraceRx // per-sub trace (mconn_trace_subs.go; under r.mu)
	nsubs int

	rawState  // lite with capMConnRaw: the first sub carries raw bytes until an upgrade
	callState // HS2_MCONN_CALL (mconn_call.go)

	stripeState // HS2_MCONN_STRIPE (mconn_stripe.go)
	rescueState // HS2_MCONN_RESCUE (mconn_rescue.go)
	zsState     // zs291 (mconn_zs291.go)

	r mrecv

	kick      chan struct{} // wakes the manager
	die       chan struct{}
	closeOnce sync.Once
}

func newMConn(id uint64, iran bool, open mOpener) *mConn {
	m := &mConn{id: id, iran: iran, open: open,
		kick: make(chan struct{}, 1), die: make(chan struct{})}
	m.sendCond.L = &m.mu
	if iran && !ul268Iran.Load() {
		ul268Iran.Store(true) // ul268: this process acts on the TCP_INFO poll
	}
	memTrack(m, &memObjMConns)
	m.zsBorn = time.Now()
	memRegMConn(m)
	m.r.cond.L = &m.r.mu
	m.r.lastProgress = time.Now()
	m.tr = newMTrace(id, iran)
	if m.tr != nil {
		m.trRx = &mtraceRx{}
	}
	obs.mconnOpen.Add(1)
	obs.mconnDual.Add(1) // m.state
	if mconnLite.Load() {
		m.lite = true
		m.liteSample = m.r.lastProgress
		obs.mconnLiteOpen.Add(1)
	}
	return m
}

// refreshLiteDualLocked keeps the lite "dual now" gauge: two live light subs.
func (m *mConn) refreshLiteDualLocked() {
	if !m.lite {
		return
	}
	n := 0
	for _, s := range m.subs {
		if !s.retired {
			n++
		}
	}
	dual := n >= 2 && !m.ended
	if dual != m.liteDual {
		m.liteDual = dual
		if dual {
			obs.mconnLiteDual.Add(1)
		} else {
			obs.mconnLiteDual.Add(-1)
		}
	}
}

// liteWantDualLocked (Iran, lite, one sub): samples both directions' rates
// every mLitePoll; true once the connection is mLiteAfter old and the last
// window moved at most mDupMaxRate each way. recvd is the receive offset now.
func (m *mConn) liteWantDualLocked(now time.Time, recvd uint64) bool {
	w := now.Sub(m.liteSample)
	if w < mLiteAfter {
		return false
	}
	sent, got := m.sendOff-m.liteSent, recvd-m.liteRecvd
	m.liteSample, m.liteSent, m.liteRecvd = now, m.sendOff, recvd
	m.upLo = float64(min(sent, got)) / w.Seconds() // st246 (mconn_st244.go)
	lim := uint64(mDupMaxRate) * uint64(w) / uint64(time.Second)
	if sent <= lim && got <= lim {
		return m.smallWantLocked()
	}
	return m.callWantDualLocked(sent, got, w) // HS2_MCONN_CALL (mconn_call.go)
}

// setStateLocked moves the connection between the obs gauges.
func (m *mConn) setStateLocked(st mSendState) {
	if st == m.state {
		return
	}
	if g := m.state.gauge(); g != nil {
		g.Add(-1)
	}
	if g := st.gauge(); g != nil {
		g.Add(1)
	}
	m.state = st
}

// refreshStateLocked derives the gauge state from heavy and dupOff.
func (m *mConn) refreshStateLocked() {
	if m.state == mStateEnded {
		return
	}
	switch {
	case m.heavy:
		m.setStateLocked(mStateHeavy)
	case m.dupOff:
		m.setStateLocked(mStateSingle)
	default:
		m.setStateLocked(mStateDual)
	}
}

// newMConnIran: Iran side; opens its first light sub with open before
// returning and the second right after, in the background (lite: only once
// the connection proves light, see tick).
func newMConnIran(id uint64, open mOpener) (*mConn, error) {
	sub, key, err := open(planeLight, nil)
	if err != nil {
		return nil, err
	}
	if r, ok := sub.(mconnRawSub); ok && r.mconnRaw() && rawCapable(sub) {
		m := newMConnIranRaw(id, open, sub, key)
		if rs, ok := sub.(mconnRescueSub); ok && rs.mconnRescue() {
			m.rescueEnable() // HS2_MCONN_RESCUE (mconn_rescue.go)
			if rr, ok := sub.(mconnRescueRecSub); ok && rr.mconnRescueRec() {
				m.mu.Lock()
				m.rrPeer = true // er97 (mconn_rescue_rec.go)
				m.mu.Unlock()
			}
		}
		return m, nil
	}
	m := newMConn(id, true, open)
	m.mu.Lock()
	s := m.addSubLocked(sub, key, false)
	m.mu.Unlock()
	m.startSub(s)
	m.kickMgr() // opens the second light sub
	m.goManage()
	return m, nil
}

// newMConnExit: Kharej side; subs arrive with attach.
func newMConnExit(id uint64) *mConn {
	m := newMConn(id, false, nil)
	m.goManage()
	return m
}

// attach adds a sub that arrived for this connection (Kharej). The role (0:
// first, 1: join) matters only to the caller; a sub is a light sub until a
// BULK record arrives on it.
func (m *mConn) attach(sub mSub, role byte) {
	if role == mconnRoleFirstRaw || role == mconnRoleFirstRawRescue {
		m.attachRaw(sub, nil)
		if role == mconnRoleFirstRawRescue {
			m.rescueEnable()
		}
		return
	}
	m.mu.Lock()
	if m.ended || m.closing {
		if m.rscCap { // er94: never silent
			rescueStat.joinEnded.Add(1)
			rtrace("%x exit join for an ended conn (ended=%v closing=%v)", m.id, m.ended, m.closing)
		}
		m.mu.Unlock()
		sub.Close()
		return
	}
	if m.rscCap {
		rescueStat.joins.Add(1) // er92 diagnostic (mconn_rescue.go)
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
	obs.msubsLive.Add(1)
	memTrack(s, &memObjMSubs)
	memRegMSub(s)
	s.bulk.Store(bulk)
	now := time.Now()
	s.lastRecv.Store(monoNano(now))
	s.lastPush = now
	if m.sendOff > 0 && !bulk {
		s.lastHole = now // it lacks what was sent before it joined
		s.missEnd = m.sendOff
	}
	s.idx = m.nsubs
	m.nsubs++
	m.subs = append(m.subs, s)
	s.dcMtr = dcSubMeter(s)
	m.dcWrBump() // b296: a new sub ends the darkcall idle proof
	if m.iran && ul268On.Load() && !bulk && m.mode != planeBulk {
		ul268IdxAdd(s) // ul268: dark-link visits find it (mconn_ul268_scale.go); a bulk conn is no rescue candidate
	}
	if !bulk {
		m.graceFlushLocked(s) // HS2_MCONN_STRIPE_GRACE_MS
	}
	m.setPrimaryLocked()
	m.refreshLiteDualLocked()
	m.tr.ev("sub+ %d bulk=%v key=%p sendOff=%d subs=%d", s.idx, bulk, key, m.sendOff, len(m.subs))
	return s
}

func (m *mConn) removeSubLocked(s *msub) {
	m.zsNoteRemoveLocked()
	if i := slices.Index(m.subs, s); i >= 0 {
		m.subs = slices.Delete(m.subs, i, i+1)
	}
	ul268IdxDel(s)
	m.setPrimaryLocked()
	m.refreshLiteDualLocked()
}

func (m *mConn) setPrimaryLocked() {
	if pp := m.prefPrimary; pp != nil {
		if !pp.retired && !pp.bulk.Load() && slices.Contains(m.subs, pp) {
			m.primary.Store(pp)
			return
		}
		m.prefPrimary = nil
	}
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
	written := 0
	if m.rawW.Load() {
		n, err, done := m.writeRaw(p)
		if done {
			return n, err
		}
		// er91: a rescue cut the raw write after n bytes (mconn_rescue.go)
		written, p = n, p[n:]
		if len(p) == 0 {
			return written, nil
		}
	}
	m.mu.Lock()
	if err := m.writeErrLocked(); err != nil {
		m.mu.Unlock()
		return written, err
	}
	m.noteWriteLocked(len(p))
	m.mu.Unlock()
	for len(p) > 0 {
		c := getChunkN(mRecData, mDataHdr+min(len(p), mChunkMax))
		n := copy(c.buf[mDataHdr:], p)
		c.n = n
		binary.BigEndian.PutUint16(c.buf[9:11], uint16(n))
		var t0 time.Time
		if m.tr != nil {
			t0 = time.Now()
		}
		m.mu.Lock()
		err := m.sendLocked(c)
		if err == nil && m.rscCap {
			m.rrKeepLocked(p[:n]) // er97 (mconn_rescue_rec.go)
		}
		m.mu.Unlock()
		if m.tr != nil {
			m.tr.noteWait(time.Since(t0))
		}
		if err != nil {
			putChunk(c) // never queued
			return written, err
		}
		written += n
		p = p[n:]
	}
	if m.tr != nil {
		m.tr.sent.Add(int64(written))
		m.mu.Lock()
		q, subs := m.sendQueueLocked(), m.traceSubsLocked()
		m.mu.Unlock()
		m.tr.report(q, subs)
	}
	return written, nil
}

// sendLocked queues a DATA chunk on the sending subs, waiting while none has
// room. It assigns the chunk its offset.
func (m *mConn) sendLocked(c *mchunk) error {
	var dw b262Wait // b262: this send's darkwait episode
	for {
		if err := m.writeErrLocked(); err != nil {
			return err
		}
		if m.mode == planeBulk {
			if b := m.bulk; b.fits(c.n) {
				c.setOff(m.sendOff)
				m.sendOff += uint64(c.n)
				b.push(c)
				m.missLost = true // cs221
				return nil
			}
			m.sendCond.Wait()
			continue
		}
		if m.stripeN() {
			switch m.stripeSendLocked(c) {
			case stripeSent:
				return nil
			case stripeWait:
				m.sendCond.Wait()
				continue
			}
		}
		if m.dupOff {
			// One copy when this direction is over mDupMaxRate, and always on
			// the primary sub: a connection rides one link, never two (spread
			// over two, the receiver waits for the slower link on every other
			// chunk, and the idle second sub — copyOnly on its link — would
			// carry half a download unseen). A full primary queue makes Write
			// wait (backpressure); the other sub misses the copy (lastHole).
			// A primary whose link is stuck is swapped for the other light
			// sub first (maybeSwitchPrimaryLocked; not in lite, where the
			// connection stays on its first link).
			if !m.lite {
				m.maybeSwitchPrimaryLocked()
			}
			p := m.primary.Load()
			if p == nil || !p.fits(c.n) {
				m.sendCond.Wait()
				continue
			}
			c.setOff(m.sendOff)
			m.sendOff += uint64(c.n)
			p.push(c)
			var now time.Time
			for _, s := range m.subs {
				if s != p && !s.retired {
					if now.IsZero() {
						now = time.Now()
					}
					s.lastHole = now
					s.noteMiss(m.sendOff) // cs221
				}
			}
			if m.lite {
				if now.IsZero() {
					now = time.Now()
				}
				p.lastPush = now
				liteBud.add(now.Unix(), c.n, 0)
			}
			m.noteQueueLocked()
			return nil
		}
		if ok, wait := m.callPairLocked(c); ok { // HS2_MCONN_CALL (mconn_call.go)
			m.noteQueueLocked()
			return nil
		} else if wait {
			m.sendCond.Wait()
			continue
		}
		fit, warm := 0, 0
		good, fitGood := 0, 0 // b262: subs not dark (and those with room)
		var cold time.Time    // lite: a sub idle both ways since before this is cold
		if m.lite {
			cold = time.Now().Add(-mLiteIdle / 2)
		}
		p := m.primary.Load()
		for _, s := range m.subs {
			if s.retired {
				continue
			}
			dark := b262DarkLocked(s)
			if !dark && !s.dead {
				good++
			}
			if s.fits(c.n) {
				fit++
				if !dark {
					fitGood++
				}
				if s == p || !m.lite || !s.coldLocked(cold) {
					warm++
				}
			}
		}
		if good > 0 && fitGood == 0 && !b262WaitOver(&dw, m, c.n) {
			// b262: the only copy would ride a dark sub (a switch just
			// left it, or its link sends nothing): wait for a live one
			b262Stat.darkWait.Add(1)
			m.sendCond.Wait()
			continue
		}
		m.b262WaitDone(&dw)
		if fit == 0 || warm == 0 {
			// all full, or no sub yet (Kharej before attach); lite: never
			// the only copy on a cold second sub — Iran may be closing it
			// as idle this very moment (it decides alone, by mLiteIdle),
			// and the copy would be lost with it: wait for a warm one
			m.sendCond.Wait()
			continue
		}
		c.setOff(m.sendOff)
		m.sendOff += uint64(c.n)
		var now time.Time
		for _, s := range m.subs {
			if s.retired {
				continue
			}
			if s.fits(c.n) {
				s.push(c)
				continue
			}
			if now.IsZero() {
				now = time.Now()
			}
			s.lastHole = now      // a full queue misses this copy
			s.noteMiss(m.sendOff) // cs221
			obs.mconnMissed.Add(1)
		}
		if fit > 1 {
			obs.mconnDupBytes.Add(uint64(c.n * (fit - 1)))
		}
		if m.lite {
			if now.IsZero() {
				now = time.Now()
			}
			for _, s := range m.subs {
				if !s.retired && s.lastHole != now {
					s.lastPush = now // it took this chunk (a miss stamped lastHole with now)
				}
			}
			m.budAddLocked(now, c.n, c.n*(fit-1))
		}
		m.noteQueueLocked()
		return nil
	}
}

// maybeSwitchPrimaryLocked runs the primary switch rule (mSwitchQueue) before
// a one-copy chunk is queued. The clock is read only while the queue is high.
func (m *mConn) maybeSwitchPrimaryLocked() {
	p := m.primary.Load()
	if p == nil || p.queueLocked() <= mSwitchQueue {
		m.pStuckSince = time.Time{}
		return
	}
	now := time.Now()
	if m.pStuckSince.IsZero() {
		m.pStuckSince = now
		return
	}
	if now.Sub(m.pStuckSince) < mSwitchQueueFor || now.Sub(m.lastPSwitch) < mSwitchMinGap {
		return
	}
	for _, s := range m.subs {
		if s == p || s.retired || s.bulk.Load() || s.queueLocked() > mSwitchOtherMax {
			continue
		}
		m.tr.ev("primary switch %d -> %d: sub %d stuck with %d queued for %v, sub %d has %d",
			p.idx, s.idx, p.idx, p.queueLocked(), now.Sub(m.pStuckSince).Round(time.Millisecond), s.idx, s.queueLocked())
		m.prefPrimary = s
		m.setPrimaryLocked()
		moved := 0
		if c := p.cur; c != nil && c.seq() {
			s.push(c)
			moved += c.n
		}
		for _, c := range p.q {
			if c.seq() {
				s.push(c)
				moved += c.n
			}
		}
		obs.mconnDupBytes.Add(uint64(moved))
		m.lastPSwitch = now
		m.pStuckSince = time.Time{}
		m.qHighSince = time.Time{} // the queue rule now watches the new primary
		obs.mconnPrimarySwitch.Add(1)
		return
	}
}

// sendQueueLocked: the bytes waiting on, or being written to, the sub this
// direction rides on — the primary while one copy is sent, else the least
// loaded of the duplicating subs (a stalled sub's full queue only means it
// misses copies; it is not the source getting ahead of the connection).
func (m *mConn) sendQueueLocked() int {
	if m.dupOff {
		if p := m.primary.Load(); p != nil {
			return p.queueLocked()
		}
		return 0
	}
	q := -1
	for _, s := range m.subs {
		if s.retired {
			continue
		}
		if n := s.queueLocked(); q < 0 || n < q {
			q = n
		}
	}
	return max(q, 0)
}

// queueLocked: bytes waiting on s plus the record being written (under m.mu).
func (s *msub) queueLocked() int {
	n := s.qBytes
	if c := s.cur; c != nil {
		n += c.qsize()
	}
	for _, c := range s.curMore {
		n += c.qsize()
	}
	return n
}

// noteQueueLocked runs the queue rule of heavy detection (mHeavyQueue) at a
// cheap moment: after a chunk was queued or one was written. The clock is
// read only while the queue is high.
func (m *mConn) noteQueueLocked() {
	if m.heavy || m.mode != planeLight || m.lite {
		return
	}
	if m.sendQueueLocked() <= mHeavyQueue {
		m.qHighSince = time.Time{}
		return
	}
	now := time.Now()
	if m.qHighSince.IsZero() {
		m.qHighSince = now
		return
	}
	if now.Sub(m.qHighSince) >= mHeavyQueueFor {
		m.tr.ev("heavy by queue: sendq=%d high for %v subq=[%s]", m.sendQueueLocked(), now.Sub(m.qHighSince).Round(time.Millisecond), m.traceSubsLocked())
		m.goHeavyLocked(&obs.mconnHeavyQueue)
	}
}

// goHeavyLocked marks this side's sending direction heavy (rule: the counter
// of the rule that fired) and starts the move to a bulk sub.
func (m *mConn) goHeavyLocked(rule *atomic.Uint64) {
	m.heavy = true
	rule.Add(1)
	m.refreshStateLocked()
	if m.iran {
		m.wantBulk = true
		m.kickMgr()
		return
	}
	// Kharej cannot open subs: ask Iran for a bulk sub.
	m.tr.ev("send WANTBULK")
	c := getChunk(mRecWantBulk)
	if m.pushAllLocked(c) == 0 {
		putChunk(c)
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
		was := m.dupOff
		m.dupOff = m.dupBytes > mDupMaxRate && w < 2*mHeavyWindow
		m.dupOff = m.callRollLocked(now, w, m.dupBytes, m.dupOff) // HS2_MCONN_CALL
		if was != m.dupOff && m.tr != nil {
			m.tr.ev("dup %v -> %v (window %v, %d bytes)", !was, !m.dupOff, w.Round(time.Millisecond), m.dupBytes)
		}
		m.dupStart, m.dupBytes = now, 0
	}
	m.dupBytes += n
	if m.dupBytes > m.dupMaxLocked() && !m.dupOff {
		m.dupOff = true
		m.tr.ev("dup on -> off (%d bytes in %v)", m.dupBytes, now.Sub(m.dupStart).Round(time.Millisecond))
	}
	if m.heavy || m.mode != planeLight {
		return
	}
	m.refreshStateLocked() // dual or single
	if m.lite {
		return // lite: never heavy, no bulk, no fast start
	}
	if now.Sub(m.hvStart) > mHeavyWindow {
		m.hvStart, m.hvBytes = now, 0
	}
	m.hvBytes += n
	if m.hvBytes > mHeavyRate {
		m.tr.ev("heavy by rate: %d bytes in %v", m.hvBytes, now.Sub(m.hvStart).Round(time.Millisecond))
		m.goHeavyLocked(&obs.mconnHeavyRate)
		return
	}
	if m.startDone {
		return
	}
	if m.startAt.IsZero() {
		m.startAt = now
	}
	if now.Sub(m.startAt) > mFastStartWindow {
		m.startDone = true
		return
	}
	m.startBytes += n
	if m.startBytes > mFastStartBytes {
		m.startDone = true
		m.tr.ev("heavy by fast start: %d bytes in %v", m.startBytes, now.Sub(m.startAt).Round(time.Millisecond))
		m.goHeavyLocked(&obs.mconnHeavyFast)
	}
}

// CloseWrite ends this direction (FIN after the bytes already written); the
// other direction keeps flowing.
func (m *mConn) CloseWrite() error {
	if m.rawW.Load() {
		// hc183: a raw first sub half-closes as its smux stream (only
		// toward a peer that announced half-close: SetHalfOpen)
		if done, err := m.rawCloseWrite(); done {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rawW.Load() {
		return errors.ErrUnsupported
	}
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
		m.rawClose()
		m.mu.Lock()
		// zs291: a connection that never had a sub (the exit's lost
		// reserve race: a raw first sub arriving after ul268's rescue join
		// made the conn) has nothing to linger for, and a raw one has no
		// manager to end it: end it now, or obs.mconnOpen keeps it forever.
		never := !m.ended && m.nsubs == 0
		if !m.ended {
			m.closing = true
			m.lingerAt = time.Now().Add(mCloseLinger)
			m.finLocked()
		}
		m.sendCond.Broadcast()
		m.mu.Unlock()
		if never {
			zsNeverEnded.Add(1)
			m.end(errMConnClosed, false)
		}
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

// failedEnd (fx168): the connection ended other than by Close and before
// the peer's FIN (the bytes read are not the peer's whole stream).
func (m *mConn) failedEnd() bool {
	m.mu.Lock()
	ended, err := m.ended, m.err
	m.mu.Unlock()
	if !ended || err == errMConnClosed {
		return false
	}
	r := &m.r
	r.mu.Lock()
	fin := r.fin
	r.mu.Unlock()
	return !fin
}

// abort resets the connection at once (RST to the peer).
func (m *mConn) abort() { m.end(errMConnAborted, true) }

func (s *msub) writeLoop() {
	m := s.m
	m.subLoopEnter(s)
	defer m.subLoopExit(s)
	defer func() { // mem66: a sub still referenced (raw1, bulk) keeps no batch buffer
		m.mu.Lock()
		b := s.batch
		s.batch = nil
		m.mu.Unlock()
		putMBatch(b) // gc295: the next sub's batches reuse it
	}()
	// gc295: the batch's chunk list (s.curMore, plus c for keepSentLocked)
	// reuses one backing array per writer instead of a new one per batch.
	// Only this goroutine sets s.curMore; others only read it under m.mu.
	var spare []*mchunk
	for {
		m.mu.Lock()
		for (len(s.q) == 0 || s.gated) && !s.dead {
			if s.closeAfter && len(s.q) == 0 {
				m.mu.Unlock()
				s.close()
				m.subDied(s, nil)
				return
			}
			m.mu.Unlock()
			select {
			case <-s.kick:
			case <-s.done:
				// Closed (a retired idle sub, or a dead one) with nothing
				// queued: done stays closed, so waiting again would return at
				// once and spin, taking m.mu each turn. In production that
				// was ~6 cores of busy loops once lite started retiring idle
				// second subs.
				return
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
		wire := c.wire()
		s.curMore = spare[:0] // empty: as before, nothing batched yet
		if mconnBatch.Load() && m.lite && len(s.q) > 0 {
			wire = s.batchLocked(wire)
		}
		m.sendCond.Broadcast()
		m.mu.Unlock()

		m.dcWrBump() // b296
		_, err := s.rw.Write(wire)
		m.dcWrBump()

		m.mu.Lock()
		s.cur = nil
		more := s.curMore
		s.curMore = nil
		// HS2_MCONN_STRIPE_RESEND: a record whose write FAILED is kept too
		// (the sub is about to die; subDied resends it). Released here, it
		// was the one chunk the resend missed: the receiver's unfilled gap
		// in the lab u8 (rel49, docs §11.9.5).
		if err == nil {
			s.noteWrittenLocked(c, more) // b262
		}
		kept := (err == nil || stripeResend.Load()) && m.stripeKeepOnLocked(s)
		all := append(more, c) // gc295: more's spare capacity (writeLoop's spare)
		if kept {
			m.keepSentLocked(s, time.Now(), all...) // HS2_MCONN_STRIPE_RESEND
		} else {
			for _, x := range all {
				if x != nil && x.seq() {
					s.noteUnprov(x.off + uint64(x.n)) // cs223: written, no kept copy
				}
			}
		}
		if s == m.primary.Load() {
			m.pStuckSince = time.Time{} // a record got through: not stuck
		}
		m.noteQueueLocked() // the queue fell: the rule's "without a break"
		idle := len(s.q) == 0 && m.closing
		m.mu.Unlock()
		if !kept {
			c.unref()
			for _, x := range more {
				x.unref()
			}
		}
		clear(all)
		spare = all[:0]
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
			b.noteMiss(c.off + uint64(c.n)) // cs221/cs223: moved off b unwritten
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
	m.tr.ev("switch to bulk sub %d: BULK + %d requeued bytes (%d records)", b.idx, qb, len(q)-len(ctrl)-1)
	m.mode, m.bulk = planeBulk, b
	for _, s := range m.subs {
		ul268IdxDel(s) // ul268: a bulk conn is neither raw- nor records-rescue eligible (planeBulk is final)
	}
	m.setPrimaryLocked()
	m.sendCond.Broadcast()
	obs.mconnBulk.Add(1)
}

// ---- receive side ----

// noteRx (reader): bytes up to end were read on s (CLOSE-ACK's check is made
// by the same reader).
func (s *msub) noteRx(end uint64) {
	if end > s.rxEnd {
		s.rxEnd = end
	}
}

func (s *msub) readLoop() {
	m := s.m
	m.subLoopEnter(s)
	defer m.subLoopExit(s)
	var err error
loop:
	for {
		if _, err = io.ReadFull(s.rw, s.hdr[:1]); err != nil {
			break
		}
		s.lastRecv.Store(monoNano(time.Now()))
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
			c := getChunkN(mRecData, n)
			if _, err = io.ReadFull(s.rw, c.buf[:n]); err != nil {
				putChunk(c)
				break loop
			}
			s.noteRx(off + uint64(n))
			m.deliver(s, off, c, n)
		case mRecFin:
			if _, err = io.ReadFull(s.rw, s.hdr[1:mFinLen]); err != nil {
				break loop
			}
			s.noteRx(binary.BigEndian.Uint64(s.hdr[1:9]))
			m.recvFin(binary.BigEndian.Uint64(s.hdr[1:9]))
		case mRecBulk:
			m.onBulk(s)
		case mRecWantBulk:
			m.onWantBulk()
		case mRecUpgrade:
			if _, err = io.ReadFull(s.rw, s.hdr[1:mFinLen]); err != nil {
				break loop
			}
			if !m.onUpgrade(s, binary.BigEndian.Uint64(s.hdr[1:9])) {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecUpAck:
			m.onUpAck()
		case mRecRescue, mRecRescueAck, mRecRescueNak: // er91 (mconn_rescue.go)
			var b [mRescueLen - 1]byte
			if _, err = io.ReadFull(s.rw, b[:]); err != nil {
				break loop
			}
			a, z := binary.BigEndian.Uint64(b[0:8]), binary.BigEndian.Uint64(b[8:16])
			ok := false
			switch s.hdr[0] {
			case mRecRescue:
				ok = m.onRescue(s, a, z)
			case mRecRescueAck:
				ok = m.onRescueAck(s, a, z)
			default:
				ok = m.onRescueNak(s, a, z) // er94
			}
			if !ok {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecRRescue, mRecRRescueAck, mRecRRescueNak: // er97 (mconn_rescue_rec.go)
			var b [mRescueLen - 1]byte
			if _, err = io.ReadFull(s.rw, b[:]); err != nil {
				break loop
			}
			if !m.rrReadRec(s, s.hdr[0], b[:]) {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecRstRsc, mRecRstRscAck: // rr155 (mconn_rescue_rst.go)
			var b [mRescueLen - 1]byte
			if _, err = io.ReadFull(s.rw, b[:]); err != nil {
				break loop
			}
			if !m.rstReadRec(s, s.hdr[0], b[:]) {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecCloseReq:
			if !m.onCloseReq(s) {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecCloseAck:
			if _, err = io.ReadFull(s.rw, s.hdr[1:mFinLen]); err != nil {
				break loop
			}
			if !m.onCloseAck(s, binary.BigEndian.Uint64(s.hdr[1:9])) {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecCloseDone:
			if !m.onCloseDone(s) {
				err = errMConnProto
				m.end(err, true)
				break loop
			}
		case mRecStripe:
			m.onStripe(s)
		case mRecXS: // xs261 (mconn_xs261.go): parsed even with the key off
			if _, err = io.ReadFull(s.rw, s.hdr[1:mFinLen]); err != nil {
				break loop
			}
			m.onXS(s, binary.BigEndian.Uint64(s.hdr[1:9]))
		case mRecRst:
			m.rstNoteReset() // rr156: a refused rescue-rst (mconn_rescue_rst.go)
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
	r.ksSyncLocked()
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
			m.traceDupLocked(s, off, end, n)
			r.mu.Unlock()
			putChunk(c) // the other copy came first
			return
		}
		if off <= r.next {
			if r.readyBytes >= mRecvBuf {
				r.cond.Wait() // the app is behind: hold the sub
				continue
			}
			if m.lite {
				liteBud.add(time.Now().Unix(), int(end-r.next), 0)
			}
			r.appendReadyLocked(compactRecv(c, r.next, c.buf[r.next-off:n], r.head < len(r.ready)))
			r.pullLocked()
			r.lastProgress = time.Now()
			r.cond.Broadcast()
			m.traceFirstLocked(s, end, n)
			m.traceRecvLocked(s, n)
			r.mu.Unlock()
			m.noteSaved(s)
			return
		}
		if r.oooBytes >= max(r.oooMax, mRecvBuf) {
			if r.oooGrow && r.growOOOLocked() { // HS2_MCONN_STRIPE_UP
				continue
			}
			// HS2_MCONN_STRIPE_KEEPSTALL: the hole rooms only past the normal
			// growth (hs123: ahead of it they kept oooMax from growing in
			// quiet runs — gap_room 300..3000 chunks per quiet download)
			// hs133: only toward a peer that resends (capExtMConnKeepStall)
			// and within holeRoomConnMax / holeRoomGlobalMax
			if now := time.Now(); !r.holeRoomOKLocked(s, now, n) || !r.gapRoomLocked(now, n) && !r.gapFillLocked(now, end) && !r.holeSubRoomLocked(s, now, n) {
				r.cond.Wait() // too far ahead of the gap: hold this sub
				continue
			}
		}
		wasEmpty := len(r.ooo) == 0
		if wasEmpty {
			r.lastProgress = time.Now()
		}
		if m.lite {
			liteBud.add(time.Now().Unix(), n, 0)
		}
		r.insertLocked(compactRecv(c, off, c.buf[:n], len(r.ooo) > 0))
		m.traceFirstLocked(s, end, n)
		m.traceRecvLocked(s, n)
		r.mu.Unlock()
		m.noteSaved(s)
		if wasEmpty {
			m.kickMgr() // watch the gap
		}
		return
	}
}

// traceRecvLocked (under m.r.mu, trace on): counts delivered bytes, follows
// the reassembly gap's age and writes the periodic report.
func (m *mConn) traceRecvLocked(s *msub, n int) {
	t := m.tr
	if t == nil {
		return
	}
	r := &m.r
	t.recvd.Add(int64(n))
	now := time.Now()
	if len(r.ooo) == 0 {
		if !t.gapSince.IsZero() {
			if age := now.Sub(t.gapSince); age > mTraceSlow {
				t.ev("gap filled after %v by sub %d (next=%d ready=%d)", age.Round(time.Millisecond), s.idx, r.next, r.readyBytes)
			}
			t.gapSince = time.Time{}
		}
	} else if t.gapSince.IsZero() {
		t.gapSince = now
	} else {
		t.noteGap(now.Sub(t.gapSince))
	}
	t.report(-1, fmt.Sprintf("recv next=%d ooo=%d/%d ready=%d", r.next, len(r.ooo), r.oooBytes, r.readyBytes))
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
	r.ksSyncLocked()
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
		r.ksSyncLocked()
		return
	}
	r.oooBytes += len(e.b)
	r.ooo = slices.Replace(r.ooo, i, j, e)
	r.ksSyncLocked()
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
	if m.rawR.Load() {
		if n, err, done := m.readRaw(p); done {
			return n, err
		}
	}
	r := &m.r
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.readyBytes == 0 {
		switch {
		case m.rawR.Load():
			// er94: a RESCUE-NAK put the reader back on the first sub
			r.mu.Unlock()
			n, err := m.Read(p)
			r.mu.Lock()
			return n, err
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
	m.tr.ev("recv BULK on sub %d (mode bulk=%v)", s.idx, m.mode == planeBulk)
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
	want := m.mode == planeLight && !m.wantBulk && !m.lite
	m.tr.ev("recv WANTBULK (new=%v)", want)
	if m.lite { // lite never moves to bulk
		m.mu.Unlock()
		return
	}
	m.wantBulk = true
	m.mu.Unlock()
	if want {
		m.kickMgr()
	}
}

// ---- sub death and the end ----

func (m *mConn) subDied(s *msub, cause error) {
	s.close()
	m.subGone(s, cause)
}

// subGone: subDied after the close (HS2_MCONN_STRIPE_KEEPSTALL runs the
// close in its own goroutine: on a dark path it blocks for seconds).
func (m *mConn) subGone(s *msub, cause error) {
	_ = cause
	m.mu.Lock()
	if s.dead {
		m.mu.Unlock()
		return
	}
	s.dead = true
	m.rescueSubDiedLocked(s, cause) // er94 (mconn_rescue.go)
	m.rrSubDiedLocked(s)            // er97 (mconn_rescue_rec.go)
	m.removeSubLocked(s)
	m.forgetScoreSubLocked(s) // mem68: a dead sub's score slot must not pin it
	if s.rrAband {
		// er97: a records rescue resent what it held: nothing is lost here
		m.abandonSentLocked(s) // fx212 (H2): its kept chunks too
		for _, c := range s.q {
			if c.seq() {
				c.copies--
			}
			c.unref()
		}
		s.q, s.qBytes = nil, 0
		if c := s.cur; c != nil && c.seq() {
			c.copies--
		}
		for _, c := range s.curMore {
			if c.seq() {
				c.copies--
			}
		}
		m.sendCond.Broadcast()
		m.tr.ev("sub- %d abandoned by rescue-rec cause=%v subs=%d", s.idx, cause, len(m.subs))
		m.mu.Unlock()
		return
	}
	resent := (!m.closedCleanLocked(s) || m.callUncleanLocked(s)) && m.stripeResendLocked(s) // HS2_MCONN_STRIPE_RESEND; r68fix: a given-up call sub (mconn_call_resend.go)
	if !resent && len(s.sent) > 0 {
		m.dropSentLocked(s, len(s.sent))
	}
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
	for _, c := range s.curMore { // a batch in flight (HS2_MCONN_BATCH): the writer owns them too
		if c.seq() {
			if c.copies--; c.copies <= 0 {
				lost = true
			}
		}
	}
	m.sendCond.Broadcast()
	m.tr.ev("sub- %d cause=%v retired=%v bulk=%v lost=%v subs=%d", s.idx, cause, s.retired, s.bulk.Load(), lost, len(m.subs))
	if stripeLog && m.stN > 0 {
		stripeLogf("stripe %x: sub- %d stripe=%v cause=%v retired=%v lost=%v holeRecent=%v quiet=%v iran=%v subs=%d", m.id&0xffffff, s.idx, s.stripe, cause, s.retired, lost, m.holeRecentLocked(), m.stripeQuietLocked(s), m.iran, len(m.subs))
	}
	if m.ended || m.closedCleanLocked(s) {
		m.mu.Unlock()
		return
	}
	if m.handshaking {
		// A sub died before both sides agreed on the raw boundaries: neither
		// side can tell what the other reads on the first sub now. End like
		// the old path's stream on a dead link (no RST: it might be read raw).
		m.mu.Unlock()
		m.end(errMConnSubsDied, false)
		return
	}
	var endErr error
	rst := false
	switch {
	case m.mode == planeBulk && s == m.bulk:
		endErr, rst = errMConnBulkDied, true
	case (len(m.subs) == 0 || m.onlyClosingLocked()) && resent && !m.graceUntil.IsZero():
		// HS2_MCONN_STRIPE_GRACE_MS: wait for a new sub (Iran opens one)
		m.noteDeadKeyLocked(s.key)
		m.graceOpenLocked(time.Now())
	case (len(m.subs) == 0 || m.onlyClosingLocked()) && !m.peerFin && m.rstAllDiedLocked(s):
		// rr156: every sub died: Iran rescues on a fresh link, the exit holds (mconn_rescue_rst.go)
		m.noteDeadKeyLocked(s.key)
	case len(m.subs) == 0 || m.onlyClosingLocked():
		endErr = errMConnSubsDied
		if m.peerFin {
			endErr = errMConnPeerDone
		}
	case m.mode == planeBulk:
		// a light sub of a switched connection: nothing rides on it now
	case !resent && (lost || m.holeRecentLocked() && !m.stripeQuietLocked(s)):
		if m.rstTryLocked(s) { // rr155: HS2_MCONN_RESCUE_RST (mconn_rescue_rst.go)
			m.noteDeadKeyLocked(s.key)
			break
		}
		endErr, rst = errMConnLost, true
		subLostResets.Add(1) // r68fix2 (mconn_sub_resend.go)
		subResendLog()
	default:
		if !m.peerFin && !m.closing {
			obs.mconnSubSurvived.Add(1)
		}
		m.noteDeadKeyLocked(s.key)
	}
	m.zsNoteBranchLocked(zsBranchName(len(m.subs), endErr, m.rstHold, !m.graceUntil.IsZero()), cause)
	if s.noTgt {
		subNoTargetOutcome(endErr) // b271 Q5 (mconn_sub_resend.go)
	}
	m.mu.Unlock()
	if endErr != nil {
		m.end(endErr, rst)
		return
	}
	m.kickMgr()
}

// holeRecentLocked: every live sub missed a copy (or joined) within mHoleSafe,
// so none of them is trusted to hold what the dead sub may have lost in
// flight. One live sub without a recent hole took every copy the dead one
// did, and the connection goes on. (Checking "any" live sub reset a healthy
// connection whenever a replacement sub, which joins with a hole by
// definition, was attached by the peer before it saw the dead sub's EOF:
// TestMConnSubDeath failed on that race.)
func (m *mConn) holeRecentLocked() bool {
	now := time.Now()
	live := 0
	for _, s := range m.subs {
		if s.retired {
			continue
		}
		live++
		if s.lastHole.IsZero() || now.Sub(s.lastHole) >= mHoleSafe {
			return false
		}
	}
	return live > 0
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
	if m.iran && m.rscPhase == 1 { // er94: ended while waiting for the ACK
		rescueStat.endWait.Add(1)
		rtrace("%x iran ended waiting for the ACK after %v: %v", m.id, time.Since(m.rscT0).Round(time.Millisecond), err)
	}
	m.tr.ev("end err=%v rst=%v sent=%d", err, rst, m.sendOff)
	if stripeLog && m.stN > 0 && err != errMConnClosed {
		stripeLogf("stripe %x: end err=%v rst=%v iran=%v sent=%d subs=%d", m.id&0xffffff, err, rst, m.iran, m.sendOff, len(m.subs))
	}
	m.setStateLocked(mStateEnded)
	m.subSweepDropLocked()              // fx247 (audit246 L4): out of the sweeper now, not at its next gc round
	m.releaseSentLocked()               // HS2_MCONN_STRIPE_RESEND (and the grace's orphans)
	m.rrEndLocked()                     // er97 (mconn_rescue_rec.go)
	notice := m.rstEndNoticeLocked(err) // rr157 (mconn_rescue_rst.go): before the subs go
	m.rstEndLocked()                    // rr155 (mconn_rescue_rst.go)
	if m.lite {
		m.refreshLiteDualLocked()
		obs.mconnLiteOpen.Add(-1)
	}
	var closeNow []*msub
	for _, s := range m.subs {
		if s == m.raw1 && m.rawR.Load() {
			// The raw reader still drains it (closed by Close or the
			// reader). mem66: not forever — a reader that never comes back
			// (the user gone, the peer never sending FIN) left the stream
			// in its session's map with every byte the peer sent after.
			time.AfterFunc(mEndSubGrace, s.close)
			continue
		}
		if !rst || s.dead || s.gated {
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
	if notice != nil {
		go notice() // rr157: Iran's end notice
	}
	r := &m.r
	r.mu.Lock()
	r.dead = true
	if r.err == nil {
		r.err = err
	}
	finished := r.fin
	r.dropOOOLocked()
	r.releaseOOOLocked(r.oooMax) // HS2_MCONN_STRIPE_UP: the grown room goes back
	r.oooGrow = false
	r.cond.Broadcast()
	r.mu.Unlock()
	close(m.die)
	m.rawEnded()
	m.stripeEnded()
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
	r.ksSyncLocked()
}

// ---- manager ----

// connLoops counts the running manage loops and sub read/write loops of
// every mConn. Only tests read it: they wait for it to reach 0 before they
// restore the package switches those loops read (b278).
var connLoops atomic.Int64

// goManage starts m's manage loop (go m.manage(), counted in connLoops).
func (m *mConn) goManage() {
	connLoops.Add(1)
	go func() {
		defer connLoops.Add(-1)
		m.manage()
	}()
}

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
	m.traceSubsTick(now)
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
	m.stripeStallLogLocked(now)
	if keepStallOn.Load() {
		m.keepStallStatsLog(now) // hs115: both sides (the receiver's gap room / fill too)
	}
	if stalled, d := m.ackStallTickLocked(now); d > 0 { // HS2_MCONN_STRIPE_KEEPSTALL
		at(d)
		for _, x := range stalled {
			go x.close() // hs113 lab U1: a close on the dark path took 7.6 s
			go m.subGone(x, errStripeAckStall)
		}
	}
	if over, d := m.graceTickLocked(now); over {
		m.mu.Unlock()
		m.end(errMConnSubsDied, false)
		return 0
	} else if d > 0 {
		at(d)
	}
	var retire []*msub
	if m.iran && m.lite {
		// lite: one sub until the connection proves light (then a second,
		// duplicating one on another link); a second sub idle for mLiteIdle
		// is closed again. Nothing else: no bulk, no replacement by itself.
		live := make([]*msub, 0, 2)
		for _, s := range m.subs {
			if !s.retired {
				live = append(live, s)
			}
		}
		if m.stK > 0 {
			r := &m.r
			r.mu.Lock()
			recvd := r.next
			r.mu.Unlock()
			if ev := m.stripeEvacLocked(now); ev != nil {
				go m.subDied(ev, errStripeEvac) // HS2_MCONN_STRIPE_EVAC (mconn_stripe_evac.go): the resend moves its bytes
			}
			if ev := m.ul268StripeEvacLocked(now); ev != nil {
				go m.subDied(ev, errStripeEvac) // ul268 (mconn_ul268.go): a sub on a data-dark link
			}
			if m.stripeTickLocked(now, m.sendOff, recvd) {
				go m.openStripe(m.stripeAvoidLocked(), false)
			}
			m.stripeIdleLocked()
			if !m.peerFin && !m.wclosed {
				at(mLitePoll)
			}
		}
		m.rescueCensusLocked(live) // diagnostics (lb_rescue_cut.go)
		m.rescueCensusLocked(live) // diagnostics (lb_rescue_cut.go)
		switch {
		case m.peerFin || m.wclosed:
		case len(live) == 1 && !m.lightOpening && !m.stOpening:
			r := &m.r
			r.mu.Lock()
			recvd := r.next
			r.mu.Unlock()
			if m.liteWantDualLocked(now, recvd) || m.b274RepairWantLocked(now) { // HS2_B274_REPAIR (mconn_b274.go)
				// HS2_LB_RESCUE (lb_rescue.go): a light connection whose one
				// sub is on a lowered path gets its copy past the budget.
				if m.upgradeCappedLocked(now) && !m.rescueSingleLocked(live[0], now) && !m.callRescueSingleLocked(live[0], now) {
					obs.mconnLiteCapped.Add(1)
					m.tr.ev("lite: upgrade refused (duplication cap)")
				} else if now.Before(m.lightRetryAt) {
					at(m.lightRetryAt.Sub(now))
				} else {
					m.lightOpening = true
					obs.mconnLiteUpgrade.Add(1)
					m.tr.ev("lite: upgrade to dual (sent=%d recvd=%d)", m.sendOff, recvd)
					avoid := make([]any, 0, 1+len(m.deadKeys))
					avoid = append(avoid, avoidKey(live[0].key))
					avoid = m.callAvoidLocked(append(avoid, m.deadAvoidLocked()...)) // HS2_MCONN_CALL_AVOID
					avoid = m.rescueAvoidLocked(avoid, live[0].key)                  // HS2_LB_RESCUE_CALLFREE
					avoid = m.callRescueAvoidLocked(avoid, now)                      // HS2_LB_CALL_RESCUE
					go m.openLight(avoid)
				}
			}
			at(mLitePoll)
		case len(live) >= 2:
			p := m.primary.Load()
			hs := false
			if m.rescueCutDropLocked(live, now) { // HS2_LB_RESCUE_CUT (lb_rescue_cut.go)
				hs = true
				p = m.primary.Load()
			} else if c := m.rescueDualLocked(live, p, now); c != nil { // HS2_LB_RESCUE (lb_rescue.go)
				m.closeReqLocked(c)
				hs = true
			}
			if m.callRescueLocked(live, now) { // HS2_LB_CALL_RESCUE (lb_call_rescue.go)
				hs = true
			}
			if g, d := m.callStuckLocked(live, now); g || d > 0 { // HS2_LB_CALL_STUCK (lb_call_stuck.go)
				hs = hs || g
				if d > 0 {
					at(d)
				}
			}
			if g, d := m.callLateLocked(live, now); g || d > 0 { // HS2_LB_CALL_LATE (lb_call_late.go)
				hs = hs || g
				if d > 0 {
					at(d)
				}
			}
			if lbRescue.Load() {
				// look again in a second: a path may be lowered any moment
				// (waking only while some link is marked would leave a dual
				// connection asleep up to mLiteIdle after the marking)
				at(mLitePoll)
			}
			for _, s := range live {
				if s == p || s.retired || len(s.q) > 0 || s.cur != nil {
					continue
				}
				if m.handshaking {
					// a raw upgrade not agreed yet (mconn_raw.go): a sub
					// closed now ends the connection (subDied), and the
					// handshake can take seconds when the first sub's path
					// is slow (the peer's raw write in flight there must
					// end first) — lab, HS2_LB_RESCUE_RAW: 14-30 crowd
					// connections a run ended at mLiteIdle this way
					at(mLitePoll)
					continue
				}
				if m.stripeWarmKeepLocked(s, now) { // HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go)
					continue
				}
				// Idle both ways: the peer decides alone what it sends on
				// this sub (one copy only when its other sub's queue is
				// full, or after its own primary switch), so a sub idle
				// only by our pushes may hold the peer's only copy of some
				// bytes. Closing it then lost them: the peer reset the
				// connection (lab, 2% loss: downloads cut ~11 s in, at
				// mLiteIdle after the upgrade).
				last := s.lastPush
				if r := monoTime(s.lastRecv.Load()); r.After(last) {
					last = r
				}
				if idle := now.Sub(last); idle >= mLiteIdle {
					if mconnCloseHS.Load() && subCloseHS(s.rw) {
						m.closeReqLocked(s) // closed once the peer's CLOSE-ACK is read
						hs = true
						continue
					}
					s.retired = true
					retire = append(retire, s)
					m.tr.ev("lite: close idle second sub %d (idle %v)", s.idx, idle.Round(time.Second))
				} else {
					at(mLiteIdle - idle)
				}
			}
			if len(retire) > 0 || hs {
				m.setPrimaryLocked()
				m.refreshLiteDualLocked()
			}
			if d := m.callMoreSubsLocked(live); d > 0 { // HS2_MCONN_CALL_SUBS (mconn_call.go)
				at(d)
			}
		}
	} else if m.iran {
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
						avoid = append(avoid, avoidKey(s.key))
					}
					avoid = append(avoid, m.deadAvoidLocked()...)
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
						m.tr.ev("retire light sub %d", s.idx)
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
	quiet := monoNano(now.Add(-mRetireAfter))
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
	if keepStallOn.Load() && len(r.ooo) > 0 && now.Sub(r.lastProgress) > gapRoomAfter {
		r.cond.Broadcast() // HS2_MCONN_STRIPE_KEEPSTALL: readers held on a full reorder buffer look at the gap room
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
