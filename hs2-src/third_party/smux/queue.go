package smux

import (
	"encoding/binary"
	"io"
	"sync/atomic"
)

// FrameQueue is a session's outbound frame scheduler when its owner, not
// the session, decides the order frames leave in (ServerQueued,
// ClientQueued; hs2's engine, the link scheduler of angle (b) in
// docs/architecture/REDESIGN.fa.md §5.1).
//
// Such a session runs neither shaperLoop nor sendLoop: smux is only a
// framer. Every frame it builds -- the streams' data, SYN, FIN, UPD and the
// keepalive's NOP -- is handed to the queue (Push), and the queue's single
// writer writes it to the session's conn when its turn comes (AppendTo,
// then one Write of the conn) and reports the write (Done). smux keeps the
// frame format, the streams' windows and the rule of one frame pending per
// stream: a stream's writer waits for each of its frames' Done before it
// builds the next, so the queue holds at most one data frame per stream
// writer. A writer that stops waiting (its deadline, the session's death)
// leaves the frame its data (OutFrame.AppendTo is still safe to call), as
// with the session's own writer.
//
// Push never blocks and may be called from any goroutine; FrameSize says
// how large the next data frame of a stream may be (0: the session's
// MaxFrameSize). The writer reports a failed write with Done, which fails
// the session's writers as a failed write of its own did. Frames still
// queued when the session closes are the queue's to drop: their writers
// have returned (the session died).
type FrameQueue interface {
	Push(f *OutFrame)
	FrameSize(sid uint32) int
}

// OutFrame is one frame a session handed its FrameQueue.
type OutFrame struct {
	req  writeRequest
	sess *Session
	user uint32 // the user tag of the stream's SYN (0: a session's frame)
	hint byte   // ... and its class hint
	data bool   // a stream's data (PSH), not a control frame
}

// HeaderSize is the size of a frame's header on the wire.
const HeaderSize = headerSize

// Sid is the frame's stream (0: the session's own, a NOP).
func (f *OutFrame) Sid() uint32 { return f.req.frame.sid }

// Control says whether the frame is a control frame (SYN, FIN, UPD, NOP):
// those go ahead of data, and none of them must follow a data frame still
// queued (a stream's SYN is written before its first data frame is built,
// and its FIN after its last one was written).
func (f *OutFrame) Control() bool { return !f.data }

// Fin says whether the frame closes its stream.
func (f *OutFrame) Fin() bool { return f.req.frame.cmd == cmdFIN }

// Len is the frame's size on the wire, header included.
func (f *OutFrame) Len() int { return headerSize + len(f.req.frame.data) }

// DataLen is the size of the frame's payload.
func (f *OutFrame) DataLen() int { return len(f.req.frame.data) }

// User is the user tag and the class hint of the SYN of the frame's stream
// (0, 0 for a session's frame).
func (f *OutFrame) User() (userTag uint32, classHint byte) { return f.user, f.hint }

// AppendTo appends the frame as it goes on the wire, header and data, to
// dst. The queue's writer calls it once, when the frame's turn has come.
func (f *OutFrame) AppendTo(dst []byte) []byte {
	var h [headerSize]byte
	h[0] = f.req.frame.ver
	h[1] = f.req.frame.cmd
	binary.LittleEndian.PutUint16(h[2:], uint16(len(f.req.frame.data)))
	binary.LittleEndian.PutUint32(h[4:], f.req.frame.sid)
	dst = append(dst, h[:]...)
	dst = append(dst, f.req.take()...)
	f.req.done()
	return dst
}

// Done reports the frame's write: written bytes of the conn's Write (its
// header included), err its error. An error fails the session's writers as
// a failed write of its own writer did.
func (f *OutFrame) Done(written int, err error) {
	if r := f.req.result; r != nil {
		n := written - headerSize
		if n < 0 {
			n = 0
		}
		r <- writeResult{n: n, err: err}
		close(r)
	}
	if err != nil && f.sess != nil {
		f.sess.notifyWriteError(err)
	}
}

// Failed reports a failed write of a frame whose writer the queue already
// told (Done before the write: the frame's data was in hand): it fails the
// session's writers as Done with an error does.
func (f *OutFrame) Failed(err error) {
	if err != nil && f.sess != nil {
		f.sess.notifyWriteError(err)
	}
}

// Drop: the queue let go of a frame it will not write (its session died).
func (f *OutFrame) Drop() {
	if r := f.req.result; r != nil {
		r <- writeResult{err: io.ErrClosedPipe}
		close(r)
	}
}

// ServerQueued is Server with a FrameQueue (see FrameQueue).
func ServerQueued(conn io.ReadWriteCloser, config *Config, q FrameQueue) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSessionQ(config, conn, false, q), nil
}

// ClientQueued is Client with a FrameQueue (see FrameQueue).
func ClientQueued(conn io.ReadWriteCloser, config *Config, q FrameQueue) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSessionQ(config, conn, true, q), nil
}

// Queue is the session's FrameQueue (nil: the session writes its own
// frames).
func (s *Session) Queue() FrameQueue { return s.queue }

// Session is the stream's session.
func (s *Stream) Session() *Session { return s.sess }

// Room is how many data bytes the stream may still send before the peer's
// window is full (protocol version 2; MaxFrameSize with version 1, which
// has no window).
func (s *Stream) Room() int {
	if s.sess.config.Version != 2 {
		return s.frameSize
	}
	inflight := int32(atomic.LoadUint32(&s.numWritten) - atomic.LoadUint32(&s.peerConsumed))
	if room := int(int32(atomic.LoadUint32(&s.peerWindow)) - inflight); room > 0 {
		return room
	}
	return 0
}

// DataFrame builds a data frame of the stream carrying p for its session's
// FrameQueue to schedule on its own (a packet side channel that keeps many
// frames of one stream queued at once, each its own flow): no writer waits
// for it, so Done only reports a failed write to the session. p must stay
// as it is until the frame is written. The frame is counted against the
// peer's window: false, and no frame, when p does not fit in what is left
// of it (Room), is empty, or is larger than a frame may be.
func (s *Stream) DataFrame(p []byte) (*OutFrame, bool) {
	if len(p) == 0 || len(p) > 65535 || s.sess.queue == nil || len(p) > s.Room() {
		return nil, false
	}
	select {
	case <-s.die:
		return nil, false
	default:
	}
	atomic.AddUint32(&s.numWritten, uint32(len(p)))
	f := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)
	f.data = p
	of := &OutFrame{req: writeRequest{class: CLSDATA, frame: f}, sess: s.sess, data: true}
	of.user, of.hint = s.Syn()
	return of, true
}

// nextFrameSize is the stream's next data frame's largest size: the
// queue's (FrameSize) when the session has one, within MaxFrameSize.
func (s *Stream) nextFrameSize() int {
	if q := s.sess.queue; q != nil {
		if n := q.FrameSize(s.id); n > 0 && n < s.frameSize {
			return n
		}
	}
	return s.frameSize
}
