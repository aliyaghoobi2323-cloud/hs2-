package smux

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// A group (hs2 wire level 2, REDESIGN §3.5, §4.2, §4.7): one logical
// connection carried by a main stream (role RoleMain) and, once it has
// moved, by a stream of another session (role RoleMove, C5's one-time move
// from a QUIET link to a BULK one). Every data frame of a group's streams
// starts with its sequence number, per direction: each end numbers the
// frames it sends from 0, whichever stream they go on, and the receiver
// hands the bytes on in that order.
//
// The opener (the edge) moves: it opens the move stream on the other
// session with the next number it sends (its SYN's seq base), and its next
// frame goes there. The other end (the exit) attaches the move stream when
// it accepts it, and its next frame goes there too. Each receiver reads the
// main stream until the move stream's frames are next; frames of the move
// stream that arrive first wait in that stream's own buffer, which its
// window bounds (no memory of its own: the reorder buffer is the stream
// window, §3.5).
//
// A further move (Config.GroupMoves past 1; R1 round 3, the judge: hs2's
// move back to a QUIET link) goes the same way once the stream before is
// let go at this end (Retired): the move stream becomes the group's main
// one (shift), and the next move stream follows it as the first followed
// the main one. Since the opener lets a stream go only after the
// attacher's FIN on it, which the attacher sends once it let it go, the
// attacher has shifted, or shifts as it attaches, by the time the next move
// stream reaches it.
//
// The main stream is let go once neither end needs it: the attacher closes
// it once it writes on the move stream and has read every frame before the
// move stream's base; the opener once it writes on the move stream and has
// read the main stream up to the attacher's FIN. The opener never sends a
// FIN on the main stream before the attacher's (a FIN there with no move
// stream known means the connection ended): when it closes the connection
// after a move, the main stream lingers until the attacher's FIN arrives,
// at most groupLinger.

// groupLinger: a moved connection's main stream waits at most this long for
// the attacher's FIN once its opener closed the connection.
var groupLinger = 30 * time.Second

var (
	// ErrNoMove: the group cannot move (or take a move stream): it moved
	// already, it is closed, or this end does not move it.
	ErrNoMove = errors.New("smux: the group cannot move")
	// ErrGroupHole: a stream of the group ended with frames before the
	// other stream's missing: the connection lost data.
	ErrGroupHole = errors.New("smux: a group stream ended before the frames the other one waits for")
	// errSwitched: the group's writer moved off this stream (internal).
	errSwitched = errors.New("smux: the group's writer moved")
)

// Group is a logical connection over a main stream and, after a move, a
// second one.
type Group struct {
	id     uint32
	opener bool

	mu     sync.Mutex
	subs   [2]*Stream
	base1  uint32 // the move stream's seq base (its opener's first number on it)
	cur    int    // the stream this end writes on
	txNext uint32 // the number of this end's next frame
	flight *Stream
	// txLeft: this end writes nothing more on the main stream (moved, and
	// its last frame there is copied: a FIN after it leaves after it)
	txLeft  bool
	sw      chan struct{} // closed when cur changes (then replaced)
	attach  chan struct{} // closed when the move stream is there
	rxNext  uint32        // the number of the next frame to hand on
	closed  bool
	retired bool // the main stream is let go
	closeCh chan struct{}
	movedAt time.Time
	moves   int // the moves done (GroupMoves)
	// txBase, rxBase: the data bytes of the streams let go before the main
	// one now (a shift), so TxBytes and RxBytes keep counting up
	txBase, rxBase uint64

	wmu, rmu      sync.Mutex
	readDeadline  atomic.Value
	writeDeadline atomic.Value
	wake          chan struct{} // a deadline changed

	// the writer's time inside Write (WriteBusy)
	busy   atomic.Int64
	busyAt atomic.Int64 // monoNow when the current Write began (0: none)
}

func newGroup(main *Stream, opener bool) *Group {
	id, _, _ := main.GroupInfo()
	g := &Group{id: id, opener: opener, sw: make(chan struct{}), attach: make(chan struct{}),
		closeCh: make(chan struct{}), wake: make(chan struct{}, 1)}
	g.subs[0] = main
	main.grp = g
	return g
}

// OpenGroup opens a logical connection of group id (≠ 0) on the session:
// its main stream, whose SYN carries the user's tag and the class hint
// (EncodeSynGroup). The session must have a frame scheduler.
func (s *Session) OpenGroup(userTag uint32, classHint byte, id uint32) (*Group, error) {
	if s.sch == nil {
		return nil, ErrNoSched
	}
	if id == 0 {
		return nil, ErrBadSyn
	}
	st, err := s.OpenStreamSyn(EncodeSynGroup(userTag, classHint, RoleMain, id, 0))
	if err != nil {
		return nil, err
	}
	return newGroup(st, true), nil
}

// AcceptGroup is the logical connection of a main stream the peer opened
// (its SYN names a group, role RoleMain); nil for any other stream.
func AcceptGroup(st *Stream) *Group {
	id, role, _ := st.GroupInfo()
	if id == 0 || role != RoleMain || st.grp != nil || st.sess.sch == nil {
		return nil
	}
	return newGroup(st, false)
}

// ID is the group's id.
func (g *Group) ID() uint32 { return g.id }

// Main is the group's main stream.
func (g *Group) Main() *Stream { return g.subs[0] }

// Moved is the move stream (nil: not moved).
func (g *Group) Moved() *Stream {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.subs[1]
}

// Current is the stream this end writes on.
func (g *Group) Current() *Stream {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.subs[g.cur]
}

// Retired says whether the main stream was let go (after a move).
func (g *Group) Retired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.retired
}

// Move moves the opener's side of the group to a new stream on sess (its
// SYN: role RoleMove, the same group, the user's tag, the bulk hint, and the
// number of the next frame this end sends): the next frame goes there. A
// group moves once. The SYN is queued, not waited for: the frames after it
// on sess go out after it.
func (g *Group) Move(sess *Session, userTag uint32) (*Stream, error) {
	if sess.sch == nil {
		return nil, ErrNoSched
	}
	g.mu.Lock()
	if !g.opener || g.closed || !g.mayMoveLocked(sess.config) {
		g.mu.Unlock()
		return nil, ErrNoMove
	}
	base := g.txNext
	s1, err := sess.openStreamSyn(EncodeSynGroup(userTag, 2, RoleMove, g.id, base), false)
	if err != nil {
		g.mu.Unlock()
		return nil, err
	}
	g.shiftLocked()
	g.switchLocked(s1, base)
	g.mu.Unlock()
	g.checkRetire()
	return s1, nil
}

// mayMoveLocked says whether the group may move (or take a move stream)
// now under cfg's GroupMoves: never moved; or moved fewer than GroupMoves
// times, the stream before let go at this end.
func (g *Group) mayMoveLocked(cfg *Config) bool {
	if g.subs[1] == nil {
		return true
	}
	return cfg.GroupMoves > 1 && g.moves < cfg.GroupMoves && g.retired
}

// shiftLocked makes the move stream the group's main one before a further
// move (its stream before is let go): nothing when it never moved.
func (g *Group) shiftLocked() {
	if g.subs[1] == nil {
		return
	}
	g.txBase += g.subs[0].TxBytes()
	g.rxBase += g.subs[0].RxBytes()
	g.subs[0], g.subs[1] = g.subs[1], nil
	g.base1, g.cur = 0, 0
	g.txLeft, g.retired = false, false
	g.attach = make(chan struct{})
}

// Moves is how many times the group moved.
func (g *Group) Moves() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.moves
}

// Attach takes the move stream st of the group (accepted on another
// session): this end's next frame goes on it, and its frames are read once
// the main stream's are.
func (g *Group) Attach(st *Stream) error {
	id, role, base := st.GroupInfo()
	if id != g.id || role != RoleMove {
		return ErrNoMove
	}
	g.mu.Lock()
	if g.opener || g.closed || st.grp != nil || !g.mayMoveLocked(st.sess.config) {
		g.mu.Unlock()
		return ErrNoMove
	}
	g.shiftLocked()
	g.switchLocked(st, base)
	g.mu.Unlock()
	g.checkRetire()
	return nil
}

// switchLocked makes s1 the move stream and this end's stream to write on.
func (g *Group) switchLocked(s1 *Stream, base uint32) {
	s1.grp = g
	g.subs[1], g.base1, g.cur = s1, base, 1
	g.movedAt = time.Now()
	g.moves++
	close(g.sw)
	g.sw = make(chan struct{})
	close(g.attach)
	if g.flight != g.subs[0] {
		g.txLeft = true
	}
}

// MovedAt is when the group moved (zero: it has not).
func (g *Group) MovedAt() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.movedAt
}

// nextSeq numbers a frame the writer cuts for s: false once this end
// writes on another stream (the writer goes there).
func (g *Group) nextSeq(s *Stream) (uint32, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.subs[g.cur] != s {
		return 0, false
	}
	seq := g.txNext
	g.txNext++
	g.flight = s
	return seq, true
}

// frameDone: the frame numbered for s is copied by its session's writer
// (or will not go).
func (g *Group) frameDone(s *Stream) {
	g.mu.Lock()
	if g.flight == s {
		g.flight = nil
	}
	left := g.subs[1] != nil && s == g.subs[0] && g.cur == 1 && !g.txLeft
	if left {
		g.txLeft = true
	}
	g.mu.Unlock()
	if left {
		g.checkRetire()
	}
}

// lose: a numbered frame did not go out (its writer gave up on it): the
// peer would wait for it for good, so the connection ends.
func (g *Group) lose() { go g.Close() }

// checkRetire lets the main stream go once neither end needs it (see the
// top of the file).
func (g *Group) checkRetire() {
	g.mu.Lock()
	if g.retired || g.closed || g.subs[1] == nil || !g.txLeft {
		g.mu.Unlock()
		return
	}
	s0 := g.subs[0]
	fin, dead, empty := s0.endState()
	ok := false
	if g.opener {
		ok = (fin || dead) && empty
	} else {
		ok = int32(g.rxNext-g.base1) >= 0 && empty
	}
	if !ok {
		g.mu.Unlock()
		return
	}
	g.retired = true
	g.mu.Unlock()
	go s0.Close()
}

// endState says whether the stream got its FIN, whether it is closed (by
// its session or locally), and whether it holds nothing unread.
func (s *Stream) endState() (fin, dead, empty bool) {
	select {
	case <-s.chFinEvent:
		fin = true
	default:
	}
	select {
	case <-s.die:
		dead = true
	default:
	}
	s.bufferLock.Lock()
	empty = len(s.buffers) == 0
	s.bufferLock.Unlock()
	return
}

// headSeq is the number of the stream's next frame to read (false: none).
func (s *Stream) headSeq() (uint32, bool) {
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	if len(s.buffers) == 0 {
		return 0, false
	}
	return s.seqs[0], true
}

// readSeq reads from the stream's next frame if its number is want (ok):
// n bytes, and done once the frame is all read. A frame numbered before
// want (a copy of one already handed on) is read off and dropped first,
// counted as read (§4.7).
func (s *Stream) readSeq(b []byte, want uint32) (n int, done, ok bool, err error) {
	var notify bool
	read := 0
	s.bufferLock.Lock()
	for len(s.buffers) > 0 && int32(s.seqs[0]-want) < 0 {
		k := len(s.buffers[0])
		read += k
		s.unread -= k
		s.popLocked()
	}
	if len(s.buffers) > 0 && s.seqs[0] == want {
		ok = true
		n = copy(b, s.buffers[0])
		s.buffers[0] = s.buffers[0][n:]
		if len(s.buffers[0]) == 0 {
			s.popLocked()
			done = true
		}
		s.unread -= n
		read += n
	}
	if read > 0 {
		notify = s.consumed(uint32(read))
	}
	s.bufferLock.Unlock()
	if read > 0 {
		s.sess.returnTokens(read)
		s.sess.returnSpare()
		if notify {
			err = s.sendWindowUpdate()
		}
	}
	return n, done, ok, err
}

// Read hands on the group's bytes in their order, from whichever stream
// carries the next frame.
func (g *Group) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	g.rmu.Lock()
	defer g.rmu.Unlock()
	for {
		n, err, wait := g.tryRead(b)
		if !wait {
			return n, err
		}
		if err := g.waitRead(); err != nil {
			return 0, err
		}
	}
}

func (g *Group) tryRead(b []byte) (n int, err error, wait bool) {
	g.checkRetire()
	g.mu.Lock()
	s0, s1, want, closed, retired, base1 := g.subs[0], g.subs[1], g.rxNext, g.closed, g.retired, g.base1
	g.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe, false
	}
	for _, s := range [2]*Stream{s0, s1} {
		if s == nil || s == s0 && retired {
			continue
		}
		n, done, ok, err := s.readSeq(b, want)
		if !ok {
			continue
		}
		if done {
			g.mu.Lock()
			g.rxNext++
			g.mu.Unlock()
		}
		return n, err, false
	}
	// nothing to hand on now
	fin0, dead0, empty0 := s0.endState()
	end0 := retired || (fin0 || dead0) && empty0
	if !end0 {
		return 0, nil, true
	}
	if s1 == nil {
		if fin0 {
			return 0, io.EOF, false
		}
		return 0, io.ErrClosedPipe, false
	}
	// the main stream brings nothing more: the move stream's next frame
	// must be the one wanted
	if h, ok := s1.headSeq(); ok && h != want || !g.opener && int32(base1-want) > 0 {
		return 0, ErrGroupHole, false
	}
	fin1, dead1, empty1 := s1.endState()
	if !((fin1 || dead1) && empty1) {
		return 0, nil, true
	}
	if fin1 && (fin0 || retired) {
		return 0, io.EOF, false
	}
	return 0, io.ErrClosedPipe, false
}

// waitRead waits for something on either stream: data, a FIN, its end, the
// move stream's arrival, a deadline change.
func (g *Group) waitRead() error {
	var deadline <-chan time.Time
	if d, ok := g.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	g.mu.Lock()
	s0, s1, att, retired := g.subs[0], g.subs[1], g.attach, g.retired
	g.mu.Unlock()
	var r0, f0, d0, r1, f1, d1, a <-chan struct{}
	if !retired {
		r0, f0, d0 = s0.chReadEvent, openCh(s0.chFinEvent), openCh(s0.die)
	}
	if s1 != nil {
		r1, f1, d1 = s1.chReadEvent, openCh(s1.chFinEvent), openCh(s1.die)
	} else {
		a = att
	}
	select {
	case <-r0:
	case <-f0:
	case <-d0:
	case <-r1:
	case <-f1:
	case <-d1:
	case <-a:
	case <-g.wake:
	case <-g.closeCh:
		return io.ErrClosedPipe
	case <-deadline:
		return ErrTimeout
	}
	return nil
}

// openCh is c, or nil once it is closed (a select on it would spin).
func openCh(c chan struct{}) <-chan struct{} {
	select {
	case <-c:
		return nil
	default:
		return c
	}
}

// Write sends b in frames numbered in order, on the stream this end writes
// on: a move takes effect at the next frame.
func (g *Group) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	g.wmu.Lock()
	defer g.wmu.Unlock()
	g.busyAt.Store(monoNow())
	defer func() {
		at := g.busyAt.Swap(0)
		g.busy.Add(monoNow() - at)
	}()
	var deadline <-chan time.Time
	if d, ok := g.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	sent := 0
	for {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return sent, io.ErrClosedPipe
		}
		s, sw := g.subs[g.cur], g.sw
		g.mu.Unlock()
		n, err := s.writeFrames(b[sent:], s.sess.config.Version == 2, deadline, g, sw)
		sent += n
		if err == errSwitched {
			continue
		}
		return sent, err
	}
}

// WriteBusy is how long the group's writer has spent inside Write so far
// (the one under way included): a writer with more to send than goes out
// is in Write all the time (its send backlog, C5's move rule).
func (g *Group) WriteBusy() time.Duration {
	b := g.busy.Load()
	if at := g.busyAt.Load(); at != 0 {
		b += monoNow() - at
	}
	return time.Duration(b)
}

// TxBytes and RxBytes are the data bytes the group's streams queued to send
// and received.
func (g *Group) TxBytes() uint64 {
	g.mu.Lock()
	s0, s1, n := g.subs[0], g.subs[1], g.txBase
	g.mu.Unlock()
	n += s0.TxBytes()
	if s1 != nil {
		n += s1.TxBytes()
	}
	return n
}

func (g *Group) RxBytes() uint64 {
	g.mu.Lock()
	s0, s1, n := g.subs[0], g.subs[1], g.rxBase
	g.mu.Unlock()
	n += s0.RxBytes()
	if s1 != nil {
		n += s1.RxBytes()
	}
	return n
}

// Close ends the connection: the attacher closes both streams; the opener
// closes the move stream and leaves the main one to linger until the
// attacher's FIN (see the top of the file).
func (g *Group) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return io.ErrClosedPipe
	}
	g.closed = true
	close(g.closeCh)
	s0, s1, retired := g.subs[0], g.subs[1], g.retired
	g.mu.Unlock()
	if s1 == nil {
		return s0.Close()
	}
	if !retired {
		if g.opener {
			go linger(s0)
		} else {
			go s0.Close()
		}
	}
	return s1.Close()
}

// linger closes a moved connection's main stream once the attacher's FIN
// arrived, its session ended, or groupLinger passed.
func linger(s0 *Stream) {
	t := time.NewTimer(groupLinger)
	defer t.Stop()
	select {
	case <-s0.chFinEvent:
	case <-s0.die:
	case <-t.C:
	}
	s0.Close()
}

// GetDieCh is the die channel of the stream this end writes on: a relay
// that sees it close asks again, and a move it finds there is not the
// connection's end.
func (g *Group) GetDieCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.subs[g.cur].die
}

// Answered reports whether the peer has sent anything on the group yet.
func (g *Group) Answered() bool {
	g.mu.Lock()
	s0, s1 := g.subs[0], g.subs[1]
	g.mu.Unlock()
	return s0.Answered() || s1 != nil && s1.Answered()
}

func (g *Group) poke() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// SetReadDeadline, SetWriteDeadline and SetDeadline are net.Conn's.
func (g *Group) SetReadDeadline(t time.Time) error {
	g.readDeadline.Store(t)
	g.poke()
	return nil
}

func (g *Group) SetWriteDeadline(t time.Time) error {
	g.writeDeadline.Store(t)
	return nil
}

func (g *Group) SetDeadline(t time.Time) error {
	g.SetReadDeadline(t)
	return g.SetWriteDeadline(t)
}

// Syn is the user tag and class hint of the group's main stream.
func (g *Group) Syn() (uint32, byte) { return g.subs[0].Syn() }
