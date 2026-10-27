package smux

import (
	"encoding/binary"
	"errors"
)

// A session over a pair of connections (hs2 wire level 2; decision T16: an
// hs2 link is a pair of TCP connections, candidate β1). QUIET carries the
// control frames (SYN, FIN, UPD, NOP, MOV) and the latency lane, BULK the
// bulk lane. Each connection has its own writer, with a scheduler of its own
// (Config.Sched, one per connection), and its own reader: a stall of one (a
// retransmission hole, BBR's PROBE_RTT, a full socket) holds none of the
// other's frames.
//
// A stream's data starts on QUIET. When its owner says it is bulk
// (RequestMove), its writer moves it once, at its next frame boundary: it
// queues a MOV (movMove) on QUIET with the count of data bytes it has queued
// on QUIET so far, and every later data frame of the stream goes on BULK.
// The receiver keeps the stream's bytes in order: a BULK frame of a stream
// that has not switched yet is held, within the stream's window, until the
// MOV has come and the stream's QUIET bytes up to its count have; then the
// held frames are delivered in order, and the stream reads BULK alone. A
// stream of self-delimiting frames (WriteFlowAsync: the TUN's) splits
// instead (movSplit): from the same point on, frames from either connection
// are delivered as they come.
//
// A FIN carries the count of data bytes the stream sent in all: a FIN that
// overtakes the stream's data (it goes in QUIET's control lane; the data may
// be queued behind it, or on BULK) ends the stream only once that much has
// been delivered. A MOV may overtake the stream's last QUIET frames the same
// way: the switch waits for them.
//
// BULK data of a stream whose SYN has not come yet (QUIET behind a hole) is
// held for the session (early) until it does. A pair's SYNs leave in stream
// id order (OpenStreamSyn), so an unknown stream of the peer's above its
// latest SYN is one still to come, and one below it is gone (its frames are
// dropped, as on one connection).
//
// The QUIET reader waits for buffer room only MaxReceiveBuffer past BULK's
// (recvLoop): BULK's backlog, which slow readers leave in the buffer, never
// stops the control frames and the latency lane, and the MOV a held frame
// waits for always comes. Held bytes are counted in the buffer only once
// delivered.

const (
	// a MOV's kind
	movMove  byte = 0
	movSplit byte = 1

	// a stream's path (Stream.path, Stream.rxPath)
	pathQuiet uint32 = 0
	pathMoved uint32 = 1
	pathSplit uint32 = 2

	// PathBulk, OR'ed into WriteFlowAsync's lane, queues the frame on a
	// pair's BULK connection (ignored on one connection).
	PathBulk uint8 = 0x80
)

// errPairProto is a frame of a pair that breaks its rules: data on QUIET
// past a stream's move, a second MOV, more held than a window holds.
var errPairProto = errors.New("smux: pair protocol violation")

// earlyBuf is the BULK data of a stream whose SYN has not come.
type earlyBuf struct {
	bufs [][]byte
	n    int
}

// Paired says whether the session runs over a pair of connections.
func (s *Session) Paired() bool { return s.bconn != nil }

// peerOpened says whether sid is one the peer opens (its parity).
func (s *Session) peerOpened(sid uint32) bool {
	if s.client {
		return sid%2 == 0
	}
	return sid%2 == 1
}

// heldMax: what a stream may hold for its switch, or for its SYN, at most
// (a window and a frame).
func (s *Session) heldMax() int { return s.config.MaxStreamBuffer + s.config.MaxFrameSize }

// holdEarly keeps buf, BULK data of the peer's stream sid whose SYN has not
// come yet (under streamLock).
func (s *Session) holdEarly(sid uint32, buf []byte) error {
	if s.early == nil {
		s.early = map[uint32]*earlyBuf{}
	}
	e := s.early[sid]
	if e == nil {
		e = &earlyBuf{}
		s.early[sid] = e
	}
	e.bufs = append(e.bufs, buf)
	e.n += len(buf)
	s.earlyN += len(buf)
	if e.n > s.heldMax() || s.earlyN > 2*s.config.MaxReceiveBuffer {
		return errPairProto
	}
	return nil
}

// adoptEarly hands a new stream what BULK brought for it before its SYN:
// held until its MOV (under streamLock).
func (s *Session) adoptEarly(st *Stream) {
	e := s.early[st.id]
	if e == nil {
		return
	}
	delete(s.early, st.id)
	s.earlyN -= e.n
	st.bufferLock.Lock()
	st.held, st.heldN = e.bufs, e.n
	st.bufferLock.Unlock()
}

// dropEarlyOf drops what BULK brought for sid, a stream that will not open
// (under streamLock).
func (s *Session) dropEarlyOf(sid uint32) {
	if e := s.early[sid]; e != nil {
		delete(s.early, sid)
		s.earlyN -= e.n
		for _, b := range e.bufs {
			defaultAllocator.Put(b)
		}
	}
}

// dropEarly drops all held for SYNs not come (the session closes; under
// streamLock).
func (s *Session) dropEarly() {
	for sid := range s.early {
		s.dropEarlyOf(sid)
	}
}

// finFrame is the FIN of stream sid, which sent off data bytes in all: a
// pair's says so.
func (s *Session) finFrame(sid, off uint32) Frame {
	f := newFrame(byte(s.config.Version), cmdFIN, sid)
	if s.bconn != nil {
		var p [szFinOff]byte
		binary.LittleEndian.PutUint32(p[:], off)
		f.data = p[:]
	}
	return f
}

// RequestMove asks for the stream's data to go on the pair's BULK
// connection from its writer's next frame on, once (a stream that has moved
// or split, or one of a session that is no pair, stays where it is).
func (s *Stream) RequestMove() {
	if s.sess.bconn != nil && s.path.Load() == pathQuiet {
		s.moveReq.Store(true)
	}
}

// Moved says whether the stream's data goes on the pair's BULK connection
// now (its writer moved it).
func (s *Stream) Moved() bool { return s.path.Load() == pathMoved }

// Split says whether the stream's frames go on either connection of the
// pair by their lane (WriteFlowAsync with PathBulk).
func (s *Stream) Split() bool { return s.path.Load() == pathSplit }

// pathOf says whether the stream's next data frame goes on BULK; its
// writer moves it there first when its owner asked (RequestMove).
func (s *Stream) pathOf() (bool, error) {
	if s.sess.bconn == nil {
		return false, nil
	}
	switch s.path.Load() {
	case pathMoved:
		return true, nil
	case pathSplit:
		return false, nil // a byte write of a split stream: its frames stand alone
	}
	if !s.moveReq.Load() {
		return false, nil
	}
	if err := s.sendMov(movMove); err != nil {
		return false, err
	}
	return true, nil
}

// sendMov queues the stream's MOV of kind on QUIET (its control lane: it
// may overtake the stream's last QUIET frames, the receiver waits for them)
// and switches the stream's path. Its writer's (one at a time).
func (s *Stream) sendMov(kind byte) error {
	p := make([]byte, szCmdMOV)
	p[0] = kind
	binary.LittleEndian.PutUint32(p[1:], s.qSent.Load())
	f := newFrame(byte(s.sess.config.Version), cmdMOV, s.id)
	f.data = p
	if _, err := s.sess.queueSched(f, CLSCTRL, 0, LaneCtrl, false); err != nil {
		return err
	}
	if kind == movSplit {
		s.path.Store(pathSplit)
	} else {
		s.path.Store(pathMoved)
	}
	return nil
}

// rxPair takes buf, a data frame of the stream from QUIET or BULK (bulk),
// in the stream's order: into its buffers, or held until its switch. It
// returns the bytes delivered (to count in the session's buffer). Under
// the session's streamLock.
func (s *Stream) rxPair(buf []byte, bulk bool) (int, error) {
	s.markAnswered()
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	n := 0
	switch {
	case !bulk:
		if s.rxPath == pathMoved || s.movSet && s.movKind == movMove && int32(s.rxQuiet+uint32(len(buf))-s.movOff) > 0 {
			return 0, errPairProto // QUIET data past the stream's move
		}
		s.pushLocked(buf)
		s.rxQuiet += uint32(len(buf))
		s.rxTot += uint32(len(buf))
		n = len(buf) + s.switchLocked()
	case s.rxPath != pathQuiet:
		s.pushLocked(buf)
		s.rxTot += uint32(len(buf))
		n = len(buf)
	default:
		s.held = append(s.held, buf)
		s.heldN += len(buf)
		if s.heldN > s.sess.heldMax() {
			return 0, errPairProto
		}
	}
	s.finLocked()
	return n, nil
}

// mov takes the stream's MOV (kind, the data bytes the peer sent on QUIET
// before it) and switches once those have come; it returns the bytes
// delivered (held ones let go). Under the session's streamLock.
func (s *Stream) mov(kind byte, off uint32) (int, error) {
	s.markAnswered()
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	if s.movSet || s.rxPath != pathQuiet || kind > movSplit {
		return 0, errPairProto
	}
	if kind == movMove && int32(s.rxQuiet-off) > 0 {
		return 0, errPairProto // more came on QUIET than it says
	}
	s.movSet, s.movKind, s.movOff = true, kind, off
	n := s.switchLocked()
	s.finLocked()
	return n, nil
}

// switchLocked switches the stream to its MOV's path once its QUIET bytes
// up to the MOV's count have come, and delivers what BULK brought meanwhile,
// in order; it returns those bytes (under bufferLock).
func (s *Stream) switchLocked() int {
	if !s.movSet || int32(s.rxQuiet-s.movOff) < 0 {
		return 0
	}
	s.movSet = false
	s.rxPath = pathMoved
	if s.movKind == movSplit {
		s.rxPath = pathSplit
	}
	n := 0
	for _, b := range s.held {
		s.pushLocked(b)
		n += len(b)
	}
	s.rxTot += uint32(n)
	s.held, s.heldN = nil, 0
	return n
}

// finAt takes the stream's FIN, sent after off data bytes in all: it ends
// the stream once those have been delivered (under the session's
// streamLock).
func (s *Stream) finAt(off uint32) {
	s.markAnswered()
	s.bufferLock.Lock()
	s.finSet, s.finOff = true, off
	s.finLocked()
	s.bufferLock.Unlock()
}

// finLocked ends the stream if its FIN came and all the data it follows has
// been delivered (under bufferLock).
func (s *Stream) finLocked() {
	if s.finSet && int32(s.rxTot-s.finOff) >= 0 {
		s.finSet = false
		s.fin()
	}
}

// pairStats is what a pair session holds for its switches and SYNs (tests).
func (s *Session) pairStats() (early int, held int) {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	for _, st := range s.streams {
		st.bufferLock.Lock()
		held += st.heldN
		st.bufferLock.Unlock()
	}
	return s.earlyN, held
}

