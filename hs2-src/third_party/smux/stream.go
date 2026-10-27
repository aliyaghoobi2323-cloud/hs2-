package smux

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Stream implements net.Conn
type Stream struct {
	id   uint32
	sess *Session

	buffers [][]byte
	heads   [][]byte // slice heads kept for recycle

	bufferLock sync.Mutex
	frameSize  int

	// notify a read event
	chReadEvent chan struct{}

	// flag the stream has closed
	die     chan struct{}
	dieOnce sync.Once

	// FIN command
	chFinEvent   chan struct{}
	finEventOnce sync.Once

	// deadlines
	readDeadline  atomic.Value
	writeDeadline atomic.Value

	// per stream sliding window control
	numRead    uint32     // number of consumed bytes
	numWritten uint32     // count num of bytes written
	incr       uint32     // counting for sending
	rcvWin     uint32     // window announced in our last UPD (adaptive: under bufferLock)
	unread     int        // bytes in buffers not yet read (under bufferLock)
	minUnread  int        // adaptive: least unread left by a read since our last UPD
	lastParks  uint32     // adaptive: sess.parks at our last UPD
	updLost    bool       // our last UPD did not go out: resend at the next read (under bufferLock)
	lagSince   time.Time  // adaptive: since when unread has stayed over StreamLagTarget (zero: it has not; under bufferLock)
	lagRead    uint32     // adaptive: numRead at lagSince (under bufferLock)
	braking    bool       // adaptive: a braking UPD is on its way (under bufferLock)
	adapted    bool       // adaptive: adaptWindow has run (under bufferLock)
	firstAt    int64      // adaptive: monoNow of the first read (under bufferLock)
	firstRead  uint32     // adaptive: numRead after the first read (under bufferLock)
	idleAt     int64      // adaptive, before the first resize: monoNow of the read that left nothing unread (0: it did not; under bufferLock)
	restarted  bool       // adaptive: the pace of the first resize restarted after a wait (under bufferLock)
	settling   bool       // adaptive: after a brake, the backlog in flight before it is not yet down (under bufferLock)
	preBrake   uint32     // adaptive: rcvWin before the first brake of the latest run (0: none; under bufferLock)
	brakeAt    int64      // adaptive: monoNow of that first brake (under bufferLock)
	claim      int        // pool: what the window holds from the WindowPool (under bufferLock)
	claimDown  int64      // pool: monoNow from which the claim may come down to the window (0: not due; under bufferLock)
	readAt     int64      // pool: monoNow of the last read while it holds a claim (under bufferLock)
	updLock    sync.Mutex // one UPD at a time, so a later one never carries older values
	core       streamCore // the window core's (core.go)

	// answered is set (atomic) once the peer has sent anything for this
	// stream: data, a window update or its FIN (see Answered)
	answered uint32

	// UPD command
	peerConsumed uint32        // num of bytes the peer has consumed
	peerWindow   uint32        // peer window, initialized to 256KB, updated by peer
	chUpdate     chan struct{} // notify of remote data consuming and window update

	// the payload of the stream's SYN (EncodeSyn): set before the stream is
	// handed out, read-only after
	syn [SynPayloadLen]byte

	// with a frame scheduler (Config.Sched): the data bytes queued so far
	// (TxBytes), and what the session's owner keeps for the stream
	// (SetFlowCtx)
	txBytes atomic.Uint64
	fctx    atomic.Value

	// the stream's bulk frame still to be written (Config.EarlyDone), and
	// how long its writer has had data to send (WriteBusy)
	pendMu sync.Mutex
	pend   *pendFrame
	wrBusy atomic.Int64
	wrIn   atomic.Int64

	// a pair session (pair.go). Sending: where its data goes (pathQuiet,
	// pathMoved, pathSplit), its owner's ask to move it, and the data bytes
	// queued on QUIET so far
	path    atomic.Uint32
	moveReq atomic.Bool
	qSent   atomic.Uint32
	// receiving (under bufferLock): the path it switched to (pathQuiet:
	// not yet), the MOV it waits on, the BULK frames held until then, the
	// data bytes from QUIET and in all, and a FIN that came ahead of its
	// data
	rxPath  uint32
	movSet  bool
	movKind byte
	movOff  uint32
	held    [][]byte
	heldN   int
	rxQuiet uint32
	rxTot   uint32
	finSet  bool
	finOff  uint32
}

// newStream initiates a Stream struct
func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.chReadEvent = make(chan struct{}, 1)
	s.chUpdate = make(chan struct{}, 1)
	s.frameSize = frameSize
	s.sess = sess
	s.die = make(chan struct{})
	s.chFinEvent = make(chan struct{})
	s.peerWindow = initialPeerWindow   // set to initial window size
	s.core.limHigh = initialPeerWindow // what the peer may send before our first update
	s.rcvWin = uint32(sess.config.MaxStreamBuffer)
	if sess.config.MinStreamBuffer > 0 {
		s.rcvWin = startWindow(sess.config)
		s.lastParks = atomic.LoadUint32(&sess.parks)
	}
	return s
}

// startWindow is where an adaptive stream starts: the window the peer
// assumes before any update, within [MinStreamBuffer, MaxStreamBuffer]. It
// earns the rest by keeping up (at its first resize, at once, as far as its
// pace so far calls for: see adaptWindow), so a reader that is slow from the
// start never holds a full MaxStreamBuffer.
func startWindow(cfg *Config) uint32 {
	w := uint32(initialPeerWindow)
	if w > uint32(cfg.MaxStreamBuffer) {
		w = uint32(cfg.MaxStreamBuffer)
	}
	if w < uint32(cfg.MinStreamBuffer) {
		w = uint32(cfg.MinStreamBuffer)
	}
	return w
}

// ID returns the unique stream ID.
func (s *Stream) ID() uint32 {
	return s.id
}

// Syn returns the user tag and the class hint the stream's SYN carried (the
// ones it was opened with, on the opening side).
func (s *Stream) Syn() (userTag uint32, classHint byte) {
	return binary.BigEndian.Uint32(s.syn[0:]), s.syn[4] & 0x03
}

// Read implements net.Conn
func (s *Stream) Read(b []byte) (n int, err error) {
	for {
		n, err = s.tryRead(b)
		if err == ErrWouldBlock {
			if ew := s.waitRead(); ew != nil {
				return 0, ew
			}
		} else {
			return n, err
		}
	}
}

// tryRead is the nonblocking version of Read
func (s *Stream) tryRead(b []byte) (n int, err error) {
	if s.sess.config.Version == 2 {
		return s.tryReadv2(b)
	}

	if len(b) == 0 {
		return 0, nil
	}

	s.bufferLock.Lock()
	if len(s.buffers) > 0 {
		n = copy(b, s.buffers[0])
		s.buffers[0] = s.buffers[0][n:]
		if len(s.buffers[0]) == 0 {
			s.buffers[0] = nil
			s.buffers = s.buffers[1:]
			// full recycle
			defaultAllocator.Put(s.heads[0])
			s.heads = s.heads[1:]
		}
	}
	s.unread -= n
	s.bufferLock.Unlock()

	if n > 0 {
		s.sess.returnTokens(n)
		return n, nil
	}

	select {
	case <-s.die:
		return 0, io.EOF
	default:
		return 0, ErrWouldBlock
	}
}

func (s *Stream) tryReadv2(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}

	var notify bool
	s.bufferLock.Lock()
	if len(s.buffers) > 0 {
		n = copy(b, s.buffers[0])
		s.buffers[0] = s.buffers[0][n:]
		if len(s.buffers[0]) == 0 {
			s.buffers[0] = nil
			s.buffers = s.buffers[1:]
			// full recycle
			defaultAllocator.Put(s.heads[0])
			s.heads = s.heads[1:]
		}
	}
	s.unread -= n

	// in an ideal environment:
	// if more than half of buffer has consumed, send read ack to peer
	// based on round-trip time of ACK, continous flowing data
	// won't slow down because of waiting for ACK, as long as the
	// consumer keeps on reading data
	// s.numRead == n also notify window at the first read
	if n > 0 {
		notify = s.consumed(uint32(n))
	}
	s.bufferLock.Unlock()

	if n > 0 {
		s.sess.returnTokens(n)
		s.sess.returnSpare()
		if notify {
			err := s.sendWindowUpdate()
			return n, err
		} else {
			return n, nil
		}
	}

	select {
	case <-s.die:
		return 0, io.EOF
	default:
		return 0, ErrWouldBlock
	}
}

// WriteTo implements io.WriteTo
func (s *Stream) WriteTo(w io.Writer) (n int64, err error) {
	if s.sess.config.Version == 2 {
		return s.writeTov2(w)
	}

	for {
		var buf []byte
		s.bufferLock.Lock()
		if len(s.buffers) > 0 {
			buf = s.buffers[0]
			s.buffers = s.buffers[1:]
			s.heads = s.heads[1:]
		}
		s.unread -= len(buf)
		s.bufferLock.Unlock()

		if buf != nil {
			nw, ew := w.Write(buf)
			s.sess.returnTokens(len(buf))
			defaultAllocator.Put(buf)
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

func (s *Stream) writeTov2(w io.Writer) (n int64, err error) {
	for {
		var notify bool
		var buf []byte
		s.bufferLock.Lock()
		if len(s.buffers) > 0 {
			buf = s.buffers[0]
			s.buffers = s.buffers[1:]
			s.heads = s.heads[1:]
		}
		s.unread -= len(buf)
		if len(buf) > 0 {
			notify = s.consumed(uint32(len(buf)))
		}
		s.bufferLock.Unlock()

		if buf != nil {
			nw, ew := w.Write(buf)
			s.sess.returnTokens(len(buf))
			s.sess.returnSpare()
			defaultAllocator.Put(buf)
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}

			if notify {
				if err := s.sendWindowUpdate(); err != nil {
					return n, err
				}
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

// consumed accounts n bytes just read (n > 0) and reports whether a window
// update is due. The caller holds bufferLock and has already taken the n
// bytes off unread.
//
// An update is due at the first read and each time a step (updStep: half of
// the window announced in the last update, less past NeedStreamBuffer) has
// been read since. The peer may run at most rcvWin bytes past the consumed
// count of our last update; we always read a step of that and announce
// again, so the peer never waits for an update that is not coming, whatever
// the window does in between (a brake, see checkLag, announces a smaller
// window early and restarts that count).
//
// The first update of an adaptive stream announces the window the peer
// assumes anyway; the next one (adaptWindow) is the stream's first resize,
// judged on the reads after the first.
func (s *Stream) consumed(n uint32) (notify bool) {
	cfg := s.sess.config
	s.numRead += n
	s.core.readTot.Add(uint64(n))
	s.incr += n
	if s.unread < s.minUnread {
		s.minUnread = s.unread
	}
	if s.unread <= cfg.StreamLagTarget {
		s.lagSince = time.Time{}
	}
	first := s.numRead == n
	if cfg.MinStreamBuffer > 0 && !s.adapted {
		// a transfer that follows a small exchange and a wait (a TLS
		// handshake, a request) is paced from its own start: from the
		// first read it would count the wait as the reader's
		now := monoNow()
		if !first && !s.restarted && s.idleAt != 0 && now-s.idleAt >= int64(jumpFloor) &&
			s.numRead-n-s.firstRead < jumpRestartMax {
			s.firstAt, s.firstRead, s.restarted = now, s.numRead-n, true
		}
		s.idleAt = 0
		if s.unread == 0 {
			s.idleAt = now
		}
	}
	// a braked reader that has read all it held within brakeDrain of the
	// first brake only paused: it gets back the window it had, at once
	restore := s.preBrake > s.rcvWin && s.unread == 0 &&
		monoNow()-s.brakeAt < int64(brakeDrain)
	if restore {
		s.setWindow(cfg, s.coreCap(cfg, s.preBrake))
		s.settling, s.preBrake = false, 0
	}
	if s.claim > 0 {
		s.readAt = monoNow()
		s.reclaim(cfg) // the claim of a window that came down, once its hold is over
	}
	if s.incr < s.updStep(cfg) && !first && !s.updLost && !restore {
		return false
	}
	resend := s.updLost
	s.updLost = false
	s.incr = 0
	if cfg.MinStreamBuffer > 0 {
		parks := atomic.LoadUint32(&s.sess.parks)
		s.core.lagged = s.minUnread > cfg.StreamLagTarget/4 // (CoreSample.Lags)
		if !first && !resend && !restore {
			old := s.rcvWin
			s.adaptWindow(cfg, parks != s.lastParks)
			s.fitCore(cfg, old)
		}
		s.lastParks = parks
		s.minUnread = s.unread
		if first {
			// judge the first resize on the reads after this one: the
			// first read takes what came before the peer's first burst
			s.minUnread = int(^uint(0) >> 1)
			s.firstAt, s.firstRead = monoNow(), s.numRead
		}
	}
	return true
}

// adaptWindow resizes the adaptive receive window (Config.MinStreamBuffer >
// 0) at a window update, from the least backlog the reader left unread
// after any of its reads since the previous update: what it held on to the
// whole time, however the peer's bursts arrived.
//
// A backlog over StreamLagTarget is surplus: the window shrinks by the excess,
// down to MinStreamBuffer (by twice the excess it overshot: the least backlog
// reads high while data arrives in bursts, and a reader at a steady pace
// swung between starving and a full window). A backlog under a quarter of the
// target means the reader ran dry: the window doubles, up to MaxStreamBuffer
// -- unless the session's reader waited for buffer space since the previous
// update (parked): then the stream starved behind the other streams, not
// behind its own window, and growing it would only deepen the jam.
//
// The first resize of a reader that ran dry grows the window at once to what
// the reader took in jumpSpan at its pace since its first read, if that is
// more than twice the window: a transfer that starts fast gets the full
// window a round trip or two earlier than by doubling, while a reader that
// only kept up with a trickle (a slow link, or a connection still opening its
// congestion window) gets no more than its pace calls for.
func (s *Stream) adaptWindow(cfg *Config, parked bool) {
	standing := s.minUnread
	firstResize := !s.adapted
	s.adapted = true
	if s.settling {
		// judged only on backlogs of its own window: wait until what
		// was in flight before a brake has come down to it, then one
		// more update
		if standing < int(s.rcvWin) {
			s.settling = false
		}
		return
	}
	switch {
	case standing > cfg.StreamLagTarget:
		excess := uint64(standing - cfg.StreamLagTarget)
		if excess >= uint64(s.rcvWin)-uint64(cfg.MinStreamBuffer) {
			s.rcvWin = uint32(cfg.MinStreamBuffer)
		} else {
			s.rcvWin -= uint32(excess)
		}
	case 4*standing < cfg.StreamLagTarget && !parked:
		s.rcvWin *= 2
		if firstResize {
			// the first resize: a reader that kept up with the
			// window the peer started with gets at once what it
			// reads in jumpSpan at its pace so far (the full window
			// for a transfer that starts fast, as with the fixed
			// window), not just twice the window
			dt := monoNow() - s.firstAt
			if dt < int64(jumpFloor) {
				dt = int64(jumpFloor)
			}
			w := uint64(s.numRead-s.firstRead) * uint64(jumpSpan) / uint64(dt)
			if w > uint64(cfg.MaxStreamBuffer) {
				w = uint64(cfg.MaxStreamBuffer)
			}
			if w > uint64(s.rcvWin) {
				s.rcvWin = uint32(w)
			}
		}
		if s.rcvWin > uint32(cfg.MaxStreamBuffer) {
			s.rcvWin = uint32(cfg.MaxStreamBuffer)
		}
	}
}

// sendWindowUpdate announces the consumed count and window, by the read
// deadline (writeWindowUpdate).
func (s *Stream) sendWindowUpdate() error {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	return s.writeWindowUpdate(deadline)
}

// writeWindowUpdate announces the consumed count and window as they are when
// the update is queued, one update at a time: the reader and a brake
// (checkLag) may both send one, and the peer must never act on an older pair
// after a newer one. Only the queueing is serialized; the wait for the write
// is not.
//
// The update is queued as a control frame, ahead of every data frame waiting
// for the session's writer: the peer's writer of this stream may be waiting
// for exactly this window, and behind the data of the other streams it took
// a round of their frames to leave (a download crawled on a link busy with
// uploads, an upload on one busy with downloads, and a brake came late).
// There are no more of them than before: one per half a window read, or per
// frame received while a brake runs -- 16 bytes, though on an hs2 link each
// leaves as one length-shaped TLS record, about 1 KB.
func (s *Stream) writeWindowUpdate(deadline <-chan time.Time) error {
	s.updLock.Lock()
	s.bufferLock.Lock()
	consumed, window := s.numRead, s.rcvWin
	s.announced(window)
	s.bufferLock.Unlock()

	frame := newFrame(byte(s.sess.config.Version), cmdUPD, s.id)
	var hdr updHeader
	binary.LittleEndian.PutUint32(hdr[:], consumed)
	binary.LittleEndian.PutUint32(hdr[4:], window)
	frame.data = hdr[:]
	req, err := s.sess.queueFrame(frame, deadline, CLSCTRL)
	s.updLock.Unlock()
	if err == nil {
		_, err = s.sess.frameWritten(req, deadline)
	}
	if err != nil {
		// the peer may be waiting for exactly this update: announce
		// again at the next read rather than after another rcvWin/2
		s.bufferLock.Lock()
		s.updLost = true
		s.bufferLock.Unlock()
	}
	return err
}

// monoBase, monoNow: a monotonic clock in nanoseconds (never 0) for the
// adaptive window's stamps, which a step of the wall clock must not move.
var monoBase = time.Now()

func monoNow() int64 { return int64(time.Since(monoBase)) + 1 }

const (
	// jumpSpan, jumpFloor: the first resize of a reader that kept up
	// grows its window to what it reads in jumpSpan at its pace since its
	// first read, measured over jumpFloor at least (adaptWindow).
	jumpSpan  = time.Second
	jumpFloor = 50 * time.Millisecond
	// jumpRestartMax: a reader that has read less than this since its
	// first read and then waited jumpFloor or more with nothing unread
	// is paced from the read after the wait (consumed), once
	jumpRestartMax = 64 << 10
	// brakeLag is how long a stream's unread must stay over
	// StreamLagTarget before checkLag brakes it: a burst the reader takes
	// at once is not braked. It is short because a fast link delivers a
	// whole grant within milliseconds of the reader slowing down.
	brakeLag = 2 * time.Millisecond
	// brakeShare: checkLag brakes only while the other streams of the
	// session hold at least 1/brakeShare of MaxReceiveBuffer unread.
	brakeShare = 3
	// brakeDrain: checkLag brakes only a reader that, at its pace since
	// it fell behind, would take this long or more to read its backlog.
	brakeDrain = time.Second
	// brakeWait bounds how long a braking update waits to be queued and
	// written; one that cannot is resent at the next read (updLost)
	brakeWait = 100 * time.Millisecond
)

// checkLag runs in the session's reader after each frame pushed to an
// adaptive stream, free being what is left of the session's buffer.
//
// What the reader has granted the peer cannot be taken back by waiting: a
// window shrinks only at the next update, after another rcvWin/2 is read,
// and a reader that kept up until now (granted up to MaxStreamBuffer) and
// then slows down is seconds from it, while the peer fills the grant into
// the session's buffer. A handful of them can empty the buffer and stall
// every stream of the session at their pace.
//
// So when the unread backlog has stayed over StreamLagTarget for brakeLag
// while the other streams hold at least 1/brakeShare of the buffer, the
// peer may still send more than twice StreamLagTarget, and the reader, at
// its pace since it fell behind, would take brakeDrain or more to read what
// it holds, the window is halved at once (to no less than what the stream
// holds and MinStreamBuffer), with an update that leaves now; while the
// stream still lags so, the next frame halves it again, until the peer may
// send little more than the stream holds. The usual updates then take
// over. A reader that is slower than the link but keeps a pace its window
// is sized for (the usual updates did that) is left alone, and one that
// only paused loses at most half its window per frame it lets pile up.
// What was in flight still arrives, so this protects the buffer only as far
// as a round trip of data fits in it.
func (s *Stream) checkLag(free int) {
	cfg := s.sess.config
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	if s.unread <= cfg.StreamLagTarget {
		s.lagSince = time.Time{}
		return
	}
	now := time.Now()
	if s.lagSince.IsZero() {
		s.lagSince, s.lagRead = now, s.numRead
		return
	}
	others := cfg.MaxReceiveBuffer + int(atomic.LoadInt32(&s.sess.extra)) - free - s.unread
	lag := now.Sub(s.lagSince)
	if s.braking || int64(others)*brakeShare < int64(cfg.MaxReceiveBuffer) || lag < brakeLag {
		return
	}
	// what the peer may still send: the window of our last update past
	// its consumed count (numRead-incr), less what has arrived since
	if int64(s.rcvWin)-int64(s.incr)-int64(s.unread) <= int64(2*cfg.StreamLagTarget) {
		return // little to take back: the usual updates will do
	}
	// at the pace it has read since it fell behind, would the reader
	// take brakeDrain or longer to read what it holds?
	if float64(s.unread)*float64(lag) < float64(s.numRead-s.lagRead)*float64(brakeDrain) {
		return // a reader that keeps its pace: its window is sized for it
	}
	win := uint32(cfg.MinStreamBuffer)
	if uint32(s.unread) > win {
		win = uint32(s.unread)
	}
	if s.rcvWin/2 > win {
		win = s.rcvWin / 2 // more brakes follow while it still lags
	}
	if uint64(win)+uint64(s.incr) >= uint64(s.rcvWin) {
		// at the floor: an update now would only move the peer's limit
		// (numRead-incr+rcvWin) up, not down
		return
	}
	if s.preBrake == 0 || monoNow()-s.brakeAt >= int64(brakeDrain) {
		s.preBrake, s.brakeAt = s.rcvWin, monoNow() // the first brake of a run
	}
	s.setWindow(cfg, win)
	s.incr = 0
	s.minUnread = s.unread
	s.lastParks = atomic.LoadUint32(&s.sess.parks)
	// what was in flight still arrives: that backlog is not the
	// reader's doing (adaptWindow)
	s.settling = true
	s.braking = true
	atomic.AddUint32(&s.sess.brakes, 1)
	s.braked()
	go func() {
		select {
		case <-s.die:
		default:
			// not bound by the reader's deadline, but not for long
			// either: the reader's own update waits for this one
			t := time.NewTimer(brakeWait)
			s.writeWindowUpdate(t.C)
			t.Stop()
		}
		s.bufferLock.Lock()
		s.braking = false
		s.bufferLock.Unlock()
	}()
}

func (s *Stream) waitRead() error {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-s.chReadEvent:
		return nil
	case <-s.chFinEvent:
		// BUG(xtaci): Fix for https://github.com/xtaci/smux/issues/82
		s.bufferLock.Lock()
		defer s.bufferLock.Unlock()
		if len(s.buffers) > 0 {
			return nil
		}
		return io.EOF
	case <-s.sess.chSocketReadError:
		return s.sess.socketReadError.Load().(error)
	case <-s.sess.chProtoError:
		return s.sess.protoError.Load().(error)
	case <-deadline:
		return ErrTimeout
	case <-s.die:
		return io.ErrClosedPipe
	}

}

// Write implements net.Conn
//
// Note that the behavior when multiple goroutines write concurrently is not deterministic,
// frames may interleave in random way.
func (s *Stream) Write(b []byte) (n int, err error) {
	if s.sess.sch != nil {
		return s.writeSched(b, s.sess.config.Version == 2)
	}
	if s.sess.config.Version == 2 {
		return s.writeV2(b)
	}

	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	// check if stream has closed
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}

	// frame split and transmit
	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)
	bts := b
	for len(bts) > 0 {
		sz := len(bts)
		if sz > s.frameSize {
			sz = s.frameSize
		}
		frame.data = bts[:sz]
		bts = bts[sz:]
		n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
		s.numWritten++
		sent += n
		if err != nil {
			return sent, err
		}
	}

	return sent, nil
}

func (s *Stream) writeV2(b []byte) (n int, err error) {
	// check empty input
	if len(b) == 0 {
		return 0, nil
	}

	// check if stream has closed
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}

	// create write deadline timer
	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	// frame split and transmit process
	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)

	for {
		// per stream sliding window control
		// [.... [consumed... numWritten] ... win... ]
		// [.... [consumed...................+rmtwnd]]
		var bts []byte
		// note:
		// even if uint32 overflow, this math still works:
		// eg1: uint32(0) - uint32(math.MaxUint32) = 1
		// eg2: int32(uint32(0) - uint32(1)) = -1
		// security check for misbehavior
		inflight := int32(atomic.LoadUint32(&s.numWritten) - atomic.LoadUint32(&s.peerConsumed))
		if inflight < 0 {
			return 0, ErrConsumed
		}

		win := int32(atomic.LoadUint32(&s.peerWindow)) - inflight
		if win > 0 {
			if win > int32(len(b)) {
				bts = b
				b = nil
			} else {
				bts = b[:win]
				b = b[win:]
			}

			for len(bts) > 0 {
				sz := len(bts)
				if sz > s.frameSize {
					sz = s.frameSize
				}
				frame.data = bts[:sz]
				bts = bts[sz:]
				n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
				atomic.AddUint32(&s.numWritten, uint32(sz))
				sent += n
				if err != nil {
					return sent, err
				}
			}
		}

		// if there is any data remaining to be sent
		// wait until stream closes, window changes or deadline reached
		// this blocking behavior will inform upper layer to do flow control
		if len(b) > 0 {
			select {
			case <-s.chFinEvent: // if fin arrived, future window update is impossible
				return 0, io.EOF
			case <-s.die:
				return sent, io.ErrClosedPipe
			case <-deadline:
				return sent, ErrTimeout
			case <-s.sess.chSocketWriteError:
				return sent, s.sess.socketWriteError.Load().(error)
			case <-s.chUpdate:
				continue
			}
		} else {
			return sent, nil
		}
	}
}

// Close implements net.Conn
func (s *Stream) Close() error {
	var once bool
	var err error
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		if s.sess.bconn == nil {
			// (EarlyDone) its last data frame first: the FIN goes in the
			// control lane, ahead of what is queued (a pair's FIN says how
			// much data comes before it instead)
			s.waitPend(time.After(openCloseTimeout))
		}
		_, err = s.sess.writeFrame(s.sess.finFrame(s.id, atomic.LoadUint32(&s.numWritten)))
		s.sess.streamClosed(s.id)
		return err
	} else {
		if s.sess.IsClosed() {
			// its session closed it (sessionClose): its app lets go of it
			// now, and of what it holds with it -- the buffers and, with
			// a WindowPool, the claim and the spare that covered them
			// (pool.go)
			if n := s.recycleTokens(); n > 0 {
				s.sess.returnTokens(n)
			}
			s.sess.returnSpare()
		}
		return io.ErrClosedPipe
	}
}

// Answered reports whether the peer has sent anything for this stream yet:
// data, a window update or its FIN. A version 2 peer announces its window at
// its first read of the stream, so on a working path a stream the peer
// accepted and read from is answered about a round trip after its first
// frames went out -- whatever the peer then does with the data.
func (s *Stream) Answered() bool {
	return atomic.LoadUint32(&s.answered) != 0
}

// markAnswered records that the peer sent something for this stream (one
// atomic load per frame once it has).
func (s *Stream) markAnswered() {
	if atomic.LoadUint32(&s.answered) == 0 {
		atomic.StoreUint32(&s.answered, 1)
	}
}

// GetDieCh returns a readonly chan which can be readable
// when the stream is to be closed.
func (s *Stream) GetDieCh() <-chan struct{} {
	return s.die
}

// SetReadDeadline sets the read deadline as defined by
// net.Conn.SetReadDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.Store(t)
	s.notifyReadEvent()
	return nil
}

// SetWriteDeadline sets the write deadline as defined by
// net.Conn.SetWriteDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	return nil
}

// SetDeadline sets both read and write deadlines as defined by
// net.Conn.SetDeadline.
// A zero time value disables the deadlines.
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// session closes: the claim keeps what the stream holds unread past
// NeedStreamBuffer (it is still in memory, and may still be read) and lets
// go of the rest (reclaim)
func (s *Stream) sessionClose() {
	s.dieOnce.Do(func() { close(s.die) })
	s.bufferLock.Lock()
	s.reclaim(s.sess.config)
	s.bufferLock.Unlock()
}

// LocalAddr satisfies net.Conn interface
func (s *Stream) LocalAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Stream) RemoteAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// pushBytes append buf to buffers
func (s *Stream) pushBytes(buf []byte) (written int, err error) {
	s.markAnswered()
	s.bufferLock.Lock()
	s.pushLocked(buf)
	s.bufferLock.Unlock()
	return
}

// pushLocked is pushBytes under bufferLock.
func (s *Stream) pushLocked(buf []byte) {
	s.buffers = append(s.buffers, buf)
	s.heads = append(s.heads, buf)
	s.unread += len(buf)
	s.turned()
}

// recycleTokens transform remaining bytes to tokens(will truncate buffer),
// and lets go of the stream's claim on the WindowPool (a pair's held frames,
// never counted in the bucket, are dropped)
func (s *Stream) recycleTokens() (n int) {
	s.bufferLock.Lock()
	for k := range s.buffers {
		n += len(s.buffers[k])
		defaultAllocator.Put(s.heads[k])
	}
	s.buffers = nil
	s.heads = nil
	s.unread = 0
	for _, b := range s.held {
		defaultAllocator.Put(b)
	}
	s.held, s.heldN = nil, 0
	s.dropClaim()
	s.bufferLock.Unlock()
	return
}

// notify read event
func (s *Stream) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

// update command
func (s *Stream) update(consumed uint32, window uint32) {
	s.markAnswered()
	atomic.StoreUint32(&s.peerConsumed, consumed)
	atomic.StoreUint32(&s.peerWindow, window)
	select {
	case s.chUpdate <- struct{}{}:
	default:
	}
}

// mark this stream has been closed in protocol
func (s *Stream) fin() {
	s.markAnswered()
	s.finEventOnce.Do(func() {
		close(s.chFinEvent)
	})
}
