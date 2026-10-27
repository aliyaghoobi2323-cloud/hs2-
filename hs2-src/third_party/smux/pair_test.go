package smux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// A pair session's tests (pair.go): two in-memory connections whose
// delivery each way a test can hold, so one connection's frames arrive
// long before the other's (a retransmission hole on QUIET, a slow BULK).

// pairDir is one direction of an in-memory connection: what is written is
// read in order, but only while it is not held.
type pairDir struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	hold   bool
	closed bool
}

func newPairDir() *pairDir {
	d := &pairDir{}
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *pairDir) write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, io.ErrClosedPipe
	}
	d.buf = append(d.buf, p...)
	d.cond.Broadcast()
	return len(p), nil
}

func (d *pairDir) read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for !d.closed && (len(d.buf) == 0 || d.hold) {
		d.cond.Wait()
	}
	if d.closed {
		return 0, io.EOF
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func (d *pairDir) setHold(h bool) {
	d.mu.Lock()
	d.hold = h
	d.cond.Broadcast()
	d.mu.Unlock()
}

func (d *pairDir) close() {
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// pairFrameRec is a frame a connection's end wrote: its command, stream and
// length.
type pairFrameRec struct {
	cmd byte
	sid uint32
	n   int
}

// pairEnd is one end of an in-memory connection; it records the frames it
// writes (a session writes one frame per Write).
type pairEnd struct {
	in, out *pairDir
	mu      sync.Mutex
	sent    []pairFrameRec
}

func (e *pairEnd) Read(p []byte) (int, error) { return e.in.read(p) }

func (e *pairEnd) Write(p []byte) (int, error) {
	if len(p) >= headerSize {
		h := rawHeader{}
		copy(h[:], p)
		e.mu.Lock()
		e.sent = append(e.sent, pairFrameRec{h.Cmd(), h.StreamID(), int(h.Length())})
		e.mu.Unlock()
	}
	return e.out.write(p)
}

func (e *pairEnd) Close() error { e.in.close(); e.out.close(); return nil }

func (e *pairEnd) frames() []pairFrameRec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pairFrameRec(nil), e.sent...)
}

// pairLink is a client session and a server session over two in-memory
// connections: q (QUIET) and b (BULK); c2s/s2c are their directions.
type pairLink struct {
	t              *testing.T
	cli, srv       *Session
	cq, cb, sq, sb *pairEnd
	qc2s, bc2s     *pairDir
	qs2c, bs2c     *pairDir
}

func pairConfig() *Config {
	c := schedConfig()
	c.MaxFrameSize = 4096
	c.MaxStreamBuffer = 1 << 20
	c.MaxReceiveBuffer = 4 << 20
	return c
}

func newPairLink(t *testing.T, c *Config) *pairLink {
	t.Helper()
	l := &pairLink{t: t, qc2s: newPairDir(), bc2s: newPairDir(), qs2c: newPairDir(), bs2c: newPairDir()}
	l.cq = &pairEnd{in: l.qs2c, out: l.qc2s}
	l.cb = &pairEnd{in: l.bs2c, out: l.bc2s}
	l.sq = &pairEnd{in: l.qc2s, out: l.qs2c}
	l.sb = &pairEnd{in: l.bc2s, out: l.bs2c}
	var err error
	if l.cli, err = ClientPair(l.cq, l.cb, c); err != nil {
		t.Fatal(err)
	}
	if l.srv, err = ServerPair(l.sq, l.sb, c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.cli.Close(); l.srv.Close() })
	return l
}

func pairData(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// readAll reads st to its end within d.
func readAll(t *testing.T, st *Stream, d time.Duration) []byte {
	t.Helper()
	st.SetReadDeadline(time.Now().Add(d))
	b, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("reading the stream: %v (after %d B)", err, len(b))
	}
	return b
}

func accept(t *testing.T, s *Session) *Stream {
	t.Helper()
	s.SetDeadline(time.Now().Add(5 * time.Second))
	st, err := s.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A stream that moves once, at a frame boundary: its bytes arrive whole and
// in order although its BULK frames arrive long before its last QUIET ones
// and its MOV (QUIET held: they wait in the stream, within its window);
// after the MOV no data frame of it is on QUIET, and before it none is on
// BULK.
func TestPairMoveKeepsOrder(t *testing.T) {
	l := newPairLink(t, pairConfig())
	st, err := l.cli.OpenStreamSyn(EncodeSyn(7, 0))
	if err != nil {
		t.Fatal(err)
	}
	head, tail := pairData(100<<10, 1), pairData(400<<10, 2)
	if _, err := st.Write(head); err != nil {
		t.Fatal(err)
	}
	peer := accept(t, l.srv)
	l.qc2s.setHold(true) // QUIET behind a hole from here: the MOV and what is left
	st.RequestMove()
	done := make(chan error, 1)
	go func() {
		_, err := st.Write(tail)
		if err == nil {
			err = st.Close()
		}
		done <- err
	}()
	// BULK's frames come and wait in the stream
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, held := l.srv.pairStats(); held > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no BULK frame held for the switch")
		}
		time.Sleep(time.Millisecond)
	}
	l.qc2s.setHold(false)
	got := readAll(t, peer, 10*time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(append([]byte(nil), head...), tail...)) {
		t.Fatalf("the stream's bytes differ: got %d B, want %d", len(got), len(head)+len(tail))
	}
	if !st.Moved() {
		t.Fatal("the stream did not move")
	}
	// the wire: QUIET has the head's data, the MOV, the FIN; BULK the tail's
	qd, bd, mov := 0, 0, -1
	for i, f := range l.cq.frames() {
		switch {
		case f.cmd == cmdMOV && f.sid == st.ID():
			mov = i
		case f.cmd == cmdPSH && f.sid == st.ID():
			if mov >= 0 {
				t.Fatalf("a data frame on QUIET after the MOV (frame %d)", i)
			}
			qd += f.n
		}
	}
	for _, f := range l.cb.frames() {
		switch {
		case f.cmd == cmdPSH && f.sid == st.ID():
			bd += f.n
		case f.cmd != cmdNOP:
			t.Fatalf("BULK carried cmd %d", f.cmd)
		}
	}
	if mov < 0 || qd != len(head) || bd != len(tail) {
		t.Fatalf("MOV at %d, QUIET data %d B (want %d), BULK %d B (want %d)", mov, qd, len(head), bd, len(tail))
	}
	if e, h := l.srv.pairStats(); e != 0 || h != 0 {
		t.Fatalf("still held: early %d, held %d", e, h)
	}
}

// A FIN that arrives before the data it follows (BULK held) ends the stream
// only after that data: the reader gets every byte, then EOF.
func TestPairFinAfterItsData(t *testing.T) {
	l := newPairLink(t, pairConfig())
	st, err := l.cli.OpenStreamSyn(EncodeSyn(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	st.RequestMove()
	peer := accept(t, l.srv)
	l.bc2s.setHold(true)
	data := pairData(200<<10, 3) // within the window: all of it is out before the FIN
	go func() {
		st.Write(data)
		st.Close()
	}()
	time.Sleep(200 * time.Millisecond) // the FIN is in
	peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if n, err := peer.Read(make([]byte, 10)); err != ErrTimeout {
		t.Fatalf("with its data held the stream read %d B, err %v (want nothing yet)", n, err)
	}
	l.bc2s.setHold(false)
	if got := readAll(t, peer, 10*time.Second); !bytes.Equal(got, data) {
		t.Fatalf("got %d B, want %d", len(got), len(data))
	}
}

// BULK data of a stream whose SYN is still on QUIET (held) waits for it,
// and the stream opens with it, in order.
func TestPairBulkBeforeSyn(t *testing.T) {
	l := newPairLink(t, pairConfig())
	l.qc2s.setHold(true)
	st, err := l.cli.OpenStreamSyn(EncodeSyn(1, 0)) // its SYN waits behind the hole
	if err != nil {
		t.Fatal(err)
	}
	st.RequestMove() // at its first frame: MOV after 0 bytes on QUIET
	data := pairData(200<<10, 4)
	go func() {
		st.Write(data)
		st.Close()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e, _ := l.srv.pairStats(); e > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no BULK data held for the SYN")
		}
		time.Sleep(time.Millisecond)
	}
	l.qc2s.setHold(false)
	peer := accept(t, l.srv)
	if got := readAll(t, peer, 10*time.Second); !bytes.Equal(got, data) {
		t.Fatalf("got %d B, want %d", len(got), len(data))
	}
}

// The QUIET connection carries only control frames and the latency lane:
// with a stream moved and writing bulk, a second stream's small frames and
// the control frames are QUIET's, and every bulk-lane frame is BULK's.
func TestPairQuietCarriesCtrlAndLane(t *testing.T) {
	c := pairConfig()
	c.LaneFrame = 1024
	light := map[uint32]bool{}
	var lmu sync.Mutex
	c.FlowInfo = func(st *Stream) (uint32, uint8) {
		lmu.Lock()
		defer lmu.Unlock()
		if light[st.ID()] {
			return 1, LaneLatency
		}
		return 1, LaneBulk
	}
	l := newPairLink(t, c)
	bulk, _ := l.cli.OpenStreamSyn(EncodeSyn(1, 0))
	bulk.RequestMove()
	lt, _ := l.cli.OpenStreamSyn(EncodeSyn(1, 0))
	lmu.Lock()
	light[lt.ID()] = true
	lmu.Unlock()
	pb, pl := accept(t, l.srv), accept(t, l.srv)
	go io.Copy(io.Discard, pb)
	go io.Copy(io.Discard, pl)
	go func() {
		for i := 0; i < 50; i++ {
			bulk.Write(make([]byte, 64<<10))
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := lt.Write(make([]byte, 300)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	lane := 0
	for _, f := range l.cq.frames() {
		if f.cmd == cmdPSH {
			if f.sid != lt.ID() {
				t.Fatalf("QUIET carried data of stream %d (the bulk one is %d)", f.sid, bulk.ID())
			}
			lane++
		}
	}
	nb := 0
	for _, f := range l.cb.frames() {
		switch {
		case f.cmd == cmdPSH && f.sid == bulk.ID():
			nb++
		case f.cmd != cmdNOP:
			t.Fatalf("BULK carried cmd %d of stream %d", f.cmd, f.sid)
		}
	}
	if lane != 20 || nb == 0 {
		t.Fatalf("QUIET: %d light frames (want 20); BULK: %d bulk frames", lane, nb)
	}
}

// Either connection's end ends the session: the pair is torn down together.
func TestPairTornDownTogether(t *testing.T) {
	for _, which := range []string{"quiet", "bulk"} {
		t.Run(which, func(t *testing.T) {
			l := newPairLink(t, pairConfig())
			st, _ := l.cli.OpenStreamSyn(EncodeSyn(1, 0))
			peer := accept(t, l.srv)
			if which == "quiet" {
				l.cq.Close()
			} else {
				l.cb.Close()
			}
			for _, s := range []*Session{l.cli, l.srv} {
				deadline := time.Now().Add(5 * time.Second)
				for {
					_, err := s.AcceptStream()
					if err != nil && !errors.Is(err, ErrTimeout) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the session lives on with one connection gone")
					}
				}
			}
			st.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := st.Read(make([]byte, 1)); err == nil || err == ErrTimeout {
				t.Fatalf("a stream read on: %v", err)
			}
			peer.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := peer.Read(make([]byte, 1)); err == nil || err == ErrTimeout {
				t.Fatalf("the peer's stream read on: %v", err)
			}
		})
	}
}

// Each connection is kept alive and watched on its own: a BULK that stops
// delivering (a black hole) closes the session at the keepalive timeout,
// whatever QUIET does; with both delivering it stays.
func TestPairKeepaliveEach(t *testing.T) {
	c := pairConfig()
	c.KeepAliveDisabled = false
	c.KeepAliveInterval = 30 * time.Millisecond
	c.KeepAliveTimeout = 300 * time.Millisecond
	l := newPairLink(t, c)
	time.Sleep(700 * time.Millisecond)
	if l.cli.IsClosed() || l.srv.IsClosed() {
		t.Fatal("an idle pair closed with both connections alive")
	}
	l.bs2c.setHold(true) // the client's BULK hears nothing more
	deadline := time.Now().Add(3 * time.Second)
	for !l.cli.IsClosed() {
		if time.Now().After(deadline) {
			t.Fatal("the session lived on with its BULK silent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A stream may not hold more for its switch than a window: a BULK flood
// before any MOV is a protocol error, not memory.
func TestPairHeldBounded(t *testing.T) {
	l := newPairLink(t, pairConfig())
	st, _ := l.cli.OpenStreamSyn(EncodeSyn(1, 0))
	accept(t, l.srv)
	hdr := make([]byte, headerSize)
	hdr[0], hdr[1] = byte(l.cli.config.Version), cmdPSH
	binary.LittleEndian.PutUint16(hdr[2:], 4096)
	binary.LittleEndian.PutUint32(hdr[4:], st.ID())
	frame := append(hdr, make([]byte, 4096)...)
	for i := 0; i < (l.cli.config.MaxStreamBuffer/4096)+4; i++ {
		l.bc2s.write(frame) // straight onto BULK, no MOV
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-l.srv.chProtoError:
			return
		default:
		}
		if time.Now().After(deadline) {
			_, held := l.srv.pairStats()
			t.Fatalf("no protocol error; %d B held", held)
		}
		time.Sleep(time.Millisecond)
	}
}

// A pair session needs a scheduler and both connections.
func TestPairNeedsSched(t *testing.T) {
	c := DefaultConfig()
	a, b := &pairEnd{in: newPairDir(), out: newPairDir()}, &pairEnd{in: newPairDir(), out: newPairDir()}
	if _, err := ClientPair(a, b, c); err == nil {
		t.Fatal("a pair without a scheduler")
	}
	if _, err := ClientPair(a, nil, pairConfig()); err == nil {
		t.Fatal("a pair without its BULK")
	}
}

// pairRxStream is a stream of a pair session with no connections, for
// driving its reassembly directly.
func pairRxStream() *Stream {
	s := &Session{config: pairConfig(), bconn: &pairEnd{}}
	s.streams = map[uint32]*Stream{}
	st := newStream(3, s.config.MaxFrameSize, s)
	return st
}

// pairEvent is one arrival at a stream: a data frame from QUIET or BULK, a
// MOV, or a FIN.
type pairEvent struct {
	kind byte // 'q', 'b', 'm', 'f'
	data []byte
	mk   byte
	off  uint32
}

// runPairEvents delivers evs to a fresh stream and checks it: the bytes it
// holds for its reader, in order, are want; it ends (FIN) only once all of
// them are in, and then for good.
func runPairEvents(t *testing.T, evs []pairEvent, want []byte, split bool) {
	st := pairRxStream()
	finned := func() bool {
		select {
		case <-st.chFinEvent:
			return true
		default:
			return false
		}
	}
	got := 0
	for i, e := range evs {
		var err error
		switch e.kind {
		case 'q':
			_, err = st.rxPair(e.data, false)
		case 'b':
			_, err = st.rxPair(e.data, true)
		case 'm':
			_, err = st.mov(e.mk, e.off)
		case 'f':
			st.finAt(e.off)
		}
		if err != nil {
			t.Fatalf("event %d (%c): %v", i, e.kind, err)
		}
		st.bufferLock.Lock()
		got = st.unread
		st.bufferLock.Unlock()
		if finned() && got != len(want) {
			t.Fatalf("event %d: ended with %d of %d B delivered", i, got, len(want))
		}
	}
	if !finned() {
		t.Fatalf("never ended: %d of %d B delivered, %d held", got, len(want), st.heldN)
	}
	var out []byte
	for _, b := range st.buffers {
		out = append(out, b...)
	}
	if split {
		// frames stand alone: the same frames, the QUIET head first
		if len(out) != len(want) {
			t.Fatalf("split: %d B, want %d", len(out), len(want))
		}
		return
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("delivered %d B out of order or wrong (want %d)", len(out), len(want))
	}
}

// pairScript makes a sender's arrivals at a stream from r: frames on QUIET,
// a move (or not) after some of them, frames on BULK, and its FIN; the MOV
// and the FIN go in QUIET's control lane, so each may overtake up to two
// data frames of QUIET still queued (one queued, one being written:
// EarlyDone) -- the order on each connection is kept, and the two
// connections interleave as r says.
func pairScript(r *rand.Rand, split bool) ([]pairEvent, []byte) {
	var all []byte
	var quiet, bulk []pairEvent
	nq := r.Intn(6)
	for i := 0; i < nq; i++ {
		d := pairData(1+r.Intn(3000), r.Int63())
		all = append(all, d...)
		quiet = append(quiet, pairEvent{kind: 'q', data: d})
	}
	move := split || r.Intn(4) > 0
	if move {
		k := movMove
		if split {
			k = movSplit
		}
		at := len(quiet) - r.Intn(imin(len(quiet), 2)+1) // overtaking the last ones
		m := pairEvent{kind: 'm', mk: k, off: uint32(len(all))}
		quiet = append(quiet[:at], append([]pairEvent{m}, quiet[at:]...)...)
		nb := r.Intn(8)
		for i := 0; i < nb; i++ {
			d := pairData(1+r.Intn(4096), r.Int63())
			all = append(all, d...)
			bulk = append(bulk, pairEvent{kind: 'b', data: d})
			if split && r.Intn(3) == 0 { // a split stream goes on with QUIET frames too
				d := pairData(1+r.Intn(1000), r.Int63())
				all = append(all, d...)
				quiet = append(quiet, pairEvent{kind: 'q', data: d})
			}
		}
	}
	// the FIN: after the MOV in QUIET's order, overtaking up to two data
	// frames of QUIET's tail
	lastData, movAt := -1, -1
	for i, e := range quiet {
		if e.kind == 'q' {
			lastData = i
		}
		if e.kind == 'm' {
			movAt = i
		}
	}
	at := len(quiet)
	if back := r.Intn(3); back > 0 && lastData >= 0 {
		at = imax(imax(lastData+1-back, movAt+1), 0)
	}
	f := pairEvent{kind: 'f', off: uint32(len(all))}
	quiet = append(quiet[:at], append([]pairEvent{f}, quiet[at:]...)...)
	var evs []pairEvent
	for len(quiet)+len(bulk) > 0 {
		if len(bulk) == 0 || len(quiet) > 0 && r.Intn(2) == 0 {
			evs, quiet = append(evs, quiet[0]), quiet[1:]
		} else {
			evs, bulk = append(evs, bulk[0]), bulk[1:]
		}
	}
	return evs, all
}

// The receiver's reassembly over many random interleavings (seeded).
func TestPairReassemblyRandom(t *testing.T) {
	for seed := int64(0); seed < 3000; seed++ {
		r := rand.New(rand.NewSource(seed))
		split := seed%5 == 4
		evs, all := pairScript(r, split)
		runPairEvents(t, evs, all, split)
	}
}

// FuzzPairReassembly: the same, the interleaving drawn from the fuzzer's
// seed.
func FuzzPairReassembly(f *testing.F) {
	for _, s := range []int64{1, 2, 3, 42, 1 << 40} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, seed int64, split bool) {
		evs, all := pairScript(rand.New(rand.NewSource(seed)), split)
		runPairEvents(t, evs, all, split)
	})
}

// Data on QUIET past a move, or a second MOV, is a protocol error.
func TestPairRxViolations(t *testing.T) {
	st := pairRxStream()
	st.rxPair(make([]byte, 10), false)
	if _, err := st.mov(movMove, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := st.rxPair(make([]byte, 1), false); err == nil {
		t.Fatal("QUIET data after the move was taken")
	}
	if _, err := st.mov(movMove, 10); err == nil {
		t.Fatal("a second MOV was taken")
	}
	st = pairRxStream()
	st.rxPair(make([]byte, 10), false)
	if _, err := st.mov(movMove, 5); err == nil {
		t.Fatal("a MOV short of what QUIET brought was taken")
	}
}

func imin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func imax(a, b int) int {
	if a > b {
		return a
	}
	return b
}
