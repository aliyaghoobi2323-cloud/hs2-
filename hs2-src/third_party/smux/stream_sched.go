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
// Config.FlowInfo says at that frame.
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
			sz := sess.frameFor(lane)
			if sz > len(bts) {
				sz = len(bts)
			}
			frame.data = bts[:sz]
			bts = bts[sz:]
			s.txBytes.Add(uint64(sz))
			req, err := sess.queueSched(frame, CLSDATA, tag, lane)
			n := 0
			if err == nil {
				n, err = sess.frameWritten(req, deadline)
			}
			atomic.AddUint32(&s.numWritten, uint32(sz))
			sent += n
			if err != nil {
				return sent, err
			}
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
func (s *Stream) WriteFlowAsync(p []byte, tag uint32, flow uint64, lane uint8, done func(error)) error {
	sess := s.sess
	if sess.sch == nil {
		return ErrNoSched
	}
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
	frame := newFrame(byte(sess.config.Version), cmdPSH, s.id)
	frame.data = p
	sf := &schedFrame{tag: tag, flow: flow, lane: lane, size: headerSize + len(p), done: done}
	sf.req = writeRequest{class: CLSDATA, frame: frame}
	// counted before it can go out: the peer's update for it never finds
	// more consumed than written
	atomic.AddUint32(&s.numWritten, uint32(len(p)))
	if err := sess.sch.queue(sf, false); err != nil {
		atomic.AddUint32(&s.numWritten, ^uint32(len(p)-1))
		return err
	}
	s.txBytes.Add(uint64(len(p)))
	return nil
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
