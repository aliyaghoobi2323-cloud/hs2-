package smux

import (
	"io"
	"sync/atomic"
	"time"
)

// A stream's writes with a frame scheduler (Config.Sched, sched.go).

// writeSched is Write with a frame scheduler: writeV2's (window: the peer's
// window bounds what goes out, protocol version 2), or version 1's, with
// each frame cut for its lane as it goes: at most Config.LaneFrame in the
// latency lane, the session's bulk frame in the bulk lane (frameFor), as
// Config.FlowInfo says at that frame; on a pair, on the connection the
// stream's data is on (pathOf: it moves at a frame boundary).
//
// With Config.EarlyDone a bulk-lane frame does not hold its writer until it
// is written: the writer cuts and queues the next one meanwhile and waits for
// the one before it, so the stream has a frame queued when its turn comes
// back (one queued and one being written at most). A Write returns with its
// last such frame still queued (holding a copy of the caller's bytes); the
// next Write, or Close, waits for it.
func (s *Stream) writeSched(b []byte, window bool) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}
	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	s.busyIn()
	defer s.busyOut()
	// the caller's b is its own again on return: a frame of it still queued
	// (EarlyDone) goes out with a copy
	defer s.detachPend()
	sess := s.sess
	sent := 0
	frame := newFrame(byte(sess.config.Version), cmdPSH, s.id)
	for {
		var bts []byte
		if !window {
			bts, b = b, nil
		} else {
			// per stream sliding window control, as writeV2
			inflight := int32(atomic.LoadUint32(&s.numWritten) - atomic.LoadUint32(&s.peerConsumed))
			if inflight < 0 {
				return 0, ErrConsumed
			}
			if win := int32(atomic.LoadUint32(&s.peerWindow)) - inflight; win > 0 {
				if win > int32(len(b)) {
					bts, b = b, nil
				} else {
					bts, b = b[:win], b[win:]
				}
			}
		}
		for len(bts) > 0 {
			tag, lane := s.flowInfo()
			bulk, err := s.pathOf()
			if err != nil {
				return sent, err
			}
			sz := sess.frameFor(lane, bulk)
			if sz > len(bts) {
				sz = len(bts)
			}
			frame.data = bts[:sz]
			bts = bts[sz:]
			s.txBytes.Add(uint64(sz))
			// counted before it can go out: the peer's update for it never
			// finds more consumed than written
			s.sending(sz, bulk)
			req, err := sess.queueSched(frame, CLSDATA, tag, lane, bulk)
			if err != nil {
				s.unsent(sz, bulk)
				return sent, err
			}
			// (EarlyDone) the frame before this one is written first
			if err := s.waitPend(deadline); err != nil {
				s.setPend(req, sz, bulk) // still queued: waited for at the next Write or Close
				return sent, err
			}
			if lane == LaneBulk && sess.config.EarlyDone {
				s.setPend(req, sz, bulk)
			} else if _, err := sess.frameWritten(req, deadline); err != nil {
				if req.sf.sc.gone(req.sf) {
					s.unsent(sz, bulk) // it left its user's FIFO unsent: not in the window
				}
				return sent, err
			}
			sent += sz
		}
		if len(b) == 0 {
			return sent, nil
		}
		// wait until stream closes, window changes or deadline reached
		select {
		case <-s.chFinEvent: // if fin arrived, future window update is impossible
			return 0, io.EOF
		case <-s.die:
			return sent, io.ErrClosedPipe
		case <-deadline:
			return sent, ErrTimeout
		case <-sess.chSocketWriteError:
			return sent, sess.socketWriteError.Load().(error)
		case <-s.chUpdate:
		}
	}
}

// flowInfo is the user and the lane of the stream's next data frame:
// Config.FlowInfo's, else the SYN's tag in the bulk lane.
func (s *Stream) flowInfo() (uint32, uint8) {
	if f := s.sess.config.FlowInfo; f != nil {
		return f(s)
	}
	tag, _ := s.Syn()
	return tag, LaneBulk
}

// TxBytes is how many data bytes the stream has queued to send so far, with
// a frame scheduler (Config.Sched; 0 without one).
func (s *Stream) TxBytes() uint64 { return s.txBytes.Load() }

// SetFlowCtx keeps v, the session owner's state for the stream (what its
// Config.FlowInfo reads back with FlowCtx). Always of one type.
func (s *Stream) SetFlowCtx(v interface{}) { s.fctx.Store(v) }

// FlowCtx is what SetFlowCtx kept (nil: nothing).
func (s *Stream) FlowCtx() interface{} { return s.fctx.Load() }

// WriteFlowAsync queues p as one data frame of the stream, scheduled as the
// flow `flow` of user `tag` in `lane` (Config.Sched; ErrNoSched without
// one), and returns at once: done is called by the session's writer once
// the frame is written (nil) or will not be (the error), and p is the
// caller's again only then. A frame sent so is outside its user's
// Config.UserFrames (its caller bounds what it queues). With protocol
// version 2, ErrWouldBlock: the peer's window has no room for p now
// (WindowRoom; WindowNotify says when it may have).
//
// Frames of one flow leave in the order they were queued; frames of
// different flows of the stream may not, so each must stand on its own in
// the stream's bytes (hs2's TUN frames do).
//
// On a pair (pair.go), lane|PathBulk queues the frame on BULK: the stream
// splits (its frames are delivered from either connection as they come)
// before the first. A flow that moves to BULK may see its frames on QUIET
// overtaken by its first ones on BULK, once.
func (s *Stream) WriteFlowAsync(p []byte, tag uint32, flow uint64, lane uint8, done func(error)) error {
	sess := s.sess
	if sess.sch == nil {
		return ErrNoSched
	}
	bulk := lane&PathBulk != 0 && sess.bconn != nil
	lane &^= PathBulk
	if len(p) == 0 {
		return nil
	}
	if len(p) > 65535 {
		return ErrFrameSize
	}
	select {
	case <-s.die:
		return io.ErrClosedPipe
	case <-s.chFinEvent:
		return io.EOF
	case <-sess.die:
		return io.ErrClosedPipe
	case <-sess.chSocketWriteError:
		return sess.socketWriteError.Load().(error)
	default:
	}
	if sess.config.Version == 2 && s.WindowRoom() < len(p) {
		return ErrWouldBlock
	}
	if bulk && s.path.Load() == pathQuiet {
		if err := s.sendMov(movSplit); err != nil {
			return err
		}
	}
	frame := newFrame(byte(sess.config.Version), cmdPSH, s.id)
	frame.data = p
	sc := sess.schedOf(bulk)
	sf := &schedFrame{sc: sc, tag: tag, flow: flow, lane: lane, size: headerSize + len(p), done: done}
	sf.req = writeRequest{class: CLSDATA, frame: frame}
	// counted before it can go out: the peer's update for it never finds
	// more consumed than written
	s.sending(len(p), bulk)
	if err := sc.queue(sf, false); err != nil {
		s.unsent(len(p), bulk)
		return err
	}
	s.txBytes.Add(uint64(len(p)))
	return nil
}

// sending counts n data bytes about to be queued (on BULK: bulk): in the
// window, and on a pair's QUIET in what its MOV will say.
func (s *Stream) sending(n int, bulk bool) {
	atomic.AddUint32(&s.numWritten, uint32(n))
	if s.sess.bconn != nil && !bulk {
		s.qSent.Add(uint32(n))
	}
}

// unsent takes back sending's count of n bytes that did not go out.
func (s *Stream) unsent(n int, bulk bool) {
	if n <= 0 {
		return
	}
	atomic.AddUint32(&s.numWritten, ^uint32(n-1))
	if s.sess.bconn != nil && !bulk {
		s.qSent.Add(^uint32(n - 1))
	}
}

// pendFrame is a stream's bulk frame still to be written (EarlyDone).
type pendFrame struct {
	req  writeRequest
	sz   int
	bulk bool
}

func (s *Stream) setPend(req writeRequest, sz int, bulk bool) {
	s.pendMu.Lock()
	s.pend = &pendFrame{req, sz, bulk}
	s.pendMu.Unlock()
}

// waitPend waits for the stream's frame still to be written (EarlyDone; at
// once: none).
func (s *Stream) waitPend(deadline <-chan time.Time) error {
	s.pendMu.Lock()
	p := s.pend
	s.pend = nil
	s.pendMu.Unlock()
	if p == nil {
		return nil
	}
	_, err := s.sess.frameWritten(p.req, deadline)
	if err != nil && p.req.sf.sc.gone(p.req.sf) {
		s.unsent(p.sz, p.bulk)
	}
	return err
}

// detachPend leaves the frame still to be written a copy of its data, if it
// has not been read yet: the bytes are the caller's again (frameData).
func (s *Stream) detachPend() {
	s.pendMu.Lock()
	p := s.pend
	s.pendMu.Unlock()
	if p != nil {
		p.req.giveUp()
	}
}

// busyIn, busyOut: the stream's writer is in a Write with data to send
// (WriteBusy).
func (s *Stream) busyIn() { s.wrIn.Store(monoNow()) }

func (s *Stream) busyOut() {
	if in := s.wrIn.Swap(0); in != 0 {
		s.wrBusy.Add(monoNow() - in)
	}
}

// WriteBusy is how long, in all, the stream's writer has had data to send:
// in its Write calls, the one in progress included. Its owner reads a
// stream's send backlog from it (RequestMove).
func (s *Stream) WriteBusy() time.Duration {
	b := s.wrBusy.Load()
	if in := s.wrIn.Load(); in != 0 {
		b += monoNow() - in
	}
	return time.Duration(b)
}

// WindowRoom is how many more data bytes the peer's window takes now
// (protocol version 2).
func (s *Stream) WindowRoom() int {
	inflight := int32(atomic.LoadUint32(&s.numWritten) - atomic.LoadUint32(&s.peerConsumed))
	if inflight < 0 {
		inflight = 0
	}
	win := int32(atomic.LoadUint32(&s.peerWindow)) - inflight
	if win < 0 {
		return 0
	}
	return int(win)
}

// WindowNotify is signalled when the peer updates the stream's window (one
// pending signal at most; Write waits on it too).
func (s *Stream) WindowNotify() <-chan struct{} { return s.chUpdate }

// Session is the session the stream belongs to.
func (s *Stream) Session() *Session { return s.sess }
