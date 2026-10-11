package smux

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestShaper(t *testing.T) {
	w1 := writeRequest{seq: 1}
	w2 := writeRequest{seq: 2}
	w3 := writeRequest{seq: 3}
	w4 := writeRequest{seq: 4}
	w5 := writeRequest{seq: 5}

	var reqs shaperHeap
	heap.Push(&reqs, w5)
	heap.Push(&reqs, w4)
	heap.Push(&reqs, w3)
	heap.Push(&reqs, w2)
	heap.Push(&reqs, w1)

	for len(reqs) > 0 {
		w := heap.Pop(&reqs).(writeRequest)
		t.Log("sid:", w.frame.sid, "seq:", w.seq)
	}
}

func TestShaper2(t *testing.T) {
	w1 := writeRequest{class: CLSDATA, seq: 1} // stream 0
	w2 := writeRequest{class: CLSDATA, seq: 2}
	w3 := writeRequest{class: CLSDATA, seq: 3}
	w4 := writeRequest{class: CLSDATA, seq: 4}
	w5 := writeRequest{class: CLSDATA, seq: 5}
	w6 := writeRequest{class: CLSCTRL, seq: 6, frame: Frame{sid: 10}} // ctrl 1
	w7 := writeRequest{class: CLSCTRL, seq: 7, frame: Frame{sid: 11}} // ctrl 2

	var reqs shaperHeap
	heap.Push(&reqs, w6)
	heap.Push(&reqs, w5)
	heap.Push(&reqs, w4)
	heap.Push(&reqs, w3)
	heap.Push(&reqs, w2)
	heap.Push(&reqs, w1)
	heap.Push(&reqs, w7)

	for len(reqs) > 0 {
		w := heap.Pop(&reqs).(writeRequest)
		t.Log("sid:", w.frame.sid, "seq:", w.seq)
	}
}

// shaperSim drives the shaper's heap and fair queue as shaperLoop does: push
// queues a frame (tagging a data frame), pop hands the first one to the
// writer.
type shaperSim struct {
	reqs shaperHeap
	fq   fairQueue
	seq  uint32
}

var simData = make([]byte, 1<<16)

func (s *shaperSim) push(class CLASSID, sid uint32, n int) {
	s.seq++
	r := writeRequest{class: class, frame: Frame{sid: sid, data: simData[:n]}, seq: s.seq}
	if class == CLSDATA {
		s.fq.tag(&r)
	}
	heap.Push(&s.reqs, r)
}

func (s *shaperSim) pop() writeRequest {
	r := heap.Pop(&s.reqs).(writeRequest)
	s.fq.sent(&r)
	return r
}

// Streams that always have a frame waiting share the writer by bytes (on
// the wire: a frame's header counts), not by frames: one sending datagrams of
// 1200 B or requests of 300 B next to downloads in 16 KiB frames gets as
// many bytes as each of them (by frames it got 1/14 and 1/53 of their share).
func TestShaperSharesByBytes(t *testing.T) {
	size := map[uint32]int{1: 16384, 3: 16384, 5: 16384, 7: 16384, 9: 1200, 11: 300}
	var s shaperSim
	for sid, n := range size {
		s.push(CLSDATA, sid, n) // each writer waits for its frame before the next
	}
	got := map[uint32]int{}
	for i := 0; i < 20000; i++ {
		r := s.pop()
		got[r.frame.sid] += headerSize + len(r.frame.data)
		s.push(CLSDATA, r.frame.sid, size[r.frame.sid])
	}
	least, most := -1, 0
	for _, n := range got {
		if least < 0 || n < least {
			least = n
		}
		if n > most {
			most = n
		}
	}
	if most-least > 2*(16384+headerSize) {
		t.Fatalf("bytes per stream %v: spread %d, want within two frames", got, most-least)
	}
}

// A frame of a stream that has sent nothing lately goes next, ahead of the
// frames the busy streams queued earlier, and a control frame (a window
// update) ahead of it; a stream that keeps sending small frames is served
// at its share, not ahead of everyone.
func TestShaperSparseFrameGoesNext(t *testing.T) {
	var s shaperSim
	for sid := uint32(1); sid <= 7; sid += 2 {
		s.push(CLSDATA, sid, 16384)
	}
	for i := 0; i < 10; i++ { // a few rounds of the bulk streams
		r := s.pop()
		s.push(CLSDATA, r.frame.sid, 16384)
	}
	s.push(CLSDATA, 21, 16) // a ping
	if r := s.pop(); r.frame.sid != 21 {
		t.Fatalf("after the ping came, stream %d went first", r.frame.sid)
	}
	s.push(CLSDATA, 23, 2048) // a reply
	s.push(CLSCTRL, 25, 8)    // a window update
	if r := s.pop(); r.class != CLSCTRL {
		t.Fatalf("a data frame of stream %d went before the window update", r.frame.sid)
	}
	if r := s.pop(); r.frame.sid != 23 {
		t.Fatalf("after the reply came, stream %d went first", r.frame.sid)
	}
	// stream 27 sends small frames back to back: it gets its share by
	// bytes over the next rounds, no more
	got := map[uint32]int{}
	s.push(CLSDATA, 27, 1000)
	for i := 0; i < 400; i++ {
		r := s.pop()
		got[r.frame.sid] += len(r.frame.data)
		if r.frame.sid == 27 {
			s.push(CLSDATA, 27, 1000)
		} else {
			s.push(CLSDATA, r.frame.sid, 16384)
		}
	}
	if got[27] > got[1]+2*16384 || got[27] < got[1]-2*16384 {
		t.Fatalf("bytes: small-frame stream %d, a bulk stream %d", got[27], got[1])
	}
}

// One stream's frames leave in the order they were queued, however many of
// them wait at once and whatever their sizes.
func TestShaperKeepsStreamOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var s shaperSim
	queued := map[uint32][]uint32{} // per stream, the seqs it queued, in order
	for i := 0; i < 50000; i++ {
		if len(s.reqs) == 0 || rng.Intn(3) != 0 {
			sid := uint32(rng.Intn(12))
			class := CLSDATA
			if rng.Intn(10) == 0 {
				class = CLSCTRL
			}
			s.push(class, sid, 1+rng.Intn(16384))
			if class == CLSDATA {
				queued[sid] = append(queued[sid], s.seq)
			}
			continue
		}
		r := s.pop()
		if r.class != CLSDATA {
			continue
		}
		q := queued[r.frame.sid]
		if q[0] != r.seq {
			t.Fatalf("stream %d: frame %d left before frame %d", r.frame.sid, r.seq, q[0])
		}
		queued[r.frame.sid] = q[1:]
	}
}

// The fair queue forgets streams that have gone idle (or closed): what it
// keeps is bounded by the streams sending lately, not by every stream the
// session ever had.
func TestShaperForgetsIdleStreams(t *testing.T) {
	var s shaperSim
	for sid := uint32(1); sid < 100000; sid++ {
		s.push(CLSDATA, sid, 100)
		s.push(CLSDATA, 0, 16384) // one stream that never stops
		s.pop()
		s.pop()
		if n := len(s.fq.finish); n > 200 {
			t.Fatalf("after %d streams the fair queue holds %d", sid, n)
		}
	}
}

// pipeSession is a client session over net.Pipe whose peer reads a frame
// only when the test asks: until then the session's writer is stuck in the
// frame it is writing, and what is queued behind it waits in the shaper.
type pipeSession struct {
	t    *testing.T
	sess *Session
	peer net.Conn
}

type pipeFrame struct {
	cmd byte
	sid uint32
	n   int
}

func newPipeSession(t *testing.T) *pipeSession {
	c1, c2 := net.Pipe()
	config := DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	config.MaxFrameSize = 4096
	session, err := Client(c1, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); c2.Close() })
	return &pipeSession{t, session, c2}
}

// read lets the session's writer write one frame and returns it.
func (p *pipeSession) read() pipeFrame {
	p.t.Helper()
	var hdr rawHeader
	p.peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(p.peer, hdr[:]); err != nil {
		p.t.Fatal(err)
	}
	body := make([]byte, hdr.Length())
	if _, err := io.ReadFull(p.peer, body); err != nil {
		p.t.Fatal(err)
	}
	return pipeFrame{hdr.Cmd(), hdr.StreamID(), len(body)}
}

func (p *pipeSession) open() *Stream {
	p.t.Helper()
	got := make(chan *Stream, 1)
	go func() {
		s, _ := p.sess.OpenStream()
		got <- s
	}()
	if f := p.read(); f.cmd != cmdSYN {
		p.t.Fatalf("got cmd %d, want a SYN", f.cmd)
	}
	return <-got
}

// queued waits until n frames in all have been handed to the shaper.
func (p *pipeSession) queued(n uint32) {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadUint32(&p.sess.requestID) < n {
		if time.Now().After(deadline) {
			p.t.Fatalf("%d frames queued, want %d", atomic.LoadUint32(&p.sess.requestID), n)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // from queueFrame into the heap
}

// On a session whose writer is held up by a busy link, a window update (of a
// stream that is sending too) and a small frame of an idle stream leave
// right after the frame being written, ahead of the data frames the bulk
// streams queued before them.
func TestShaperUpdateAndSparseFrameGoFirst(t *testing.T) {
	p := newPipeSession(t)
	var bulk []*Stream
	for i := 0; i < 4; i++ {
		bulk = append(bulk, p.open())
	}
	reader, idle := p.open(), p.open()
	for _, s := range append(bulk, reader) {
		go s.Write(make([]byte, 64<<10)) // 4 KiB frames, one at a time
	}
	p.queued(6 + 5) // the SYNs, then one frame in the writer and one of each other stream
	// data for reader: its first read queues a window update
	psh := make([]byte, headerSize+100)
	psh[0], psh[1] = byte(p.sess.config.Version), cmdPSH
	binary.LittleEndian.PutUint16(psh[2:], 100)
	binary.LittleEndian.PutUint32(psh[4:], reader.ID())
	if _, err := p.peer.Write(psh); err != nil {
		t.Fatal(err)
	}
	go reader.Read(make([]byte, 100))
	p.queued(6 + 5 + 1)
	go idle.Write(make([]byte, 16))
	p.queued(6 + 5 + 2)

	if f := p.read(); f.cmd != cmdPSH || f.n != 4096 {
		t.Fatalf("first frame %+v, want the bulk frame being written", f)
	}
	if f := p.read(); f.cmd != cmdUPD || f.sid != reader.ID() {
		t.Fatalf("second frame %+v, want the window update of stream %d", f, reader.ID())
	}
	if f := p.read(); f.cmd != cmdPSH || f.sid != idle.ID() || f.n != 16 {
		t.Fatalf("third frame %+v, want the 16 bytes of stream %d", f, idle.ID())
	}
}

// The session's shaper tags each data frame as it comes: a stream whose frame
// is being written, writing another at once, queues it behind the first
// frames of the others even though it came before one of them.
func TestShaperTagsInSession(t *testing.T) {
	p := newPipeSession(t)
	x, y, z := p.open(), p.open(), p.open()
	go x.Write(make([]byte, 4096))
	p.queued(3 + 1) // the SYNs, then x's frame, in the writer
	go y.Write(make([]byte, 4096))
	p.queued(3 + 2)
	go x.Write(make([]byte, 4096)) // a second writer on x
	p.queued(3 + 3)
	go z.Write(make([]byte, 4096))
	p.queued(3 + 4)
	for i, want := range []uint32{x.ID(), y.ID(), z.ID(), x.ID()} {
		if f := p.read(); f.cmd != cmdPSH || f.sid != want {
			t.Fatalf("frame %d %+v, want one of stream %d", i, f, want)
		}
	}
}

// A stream that starts sending next to one that has sent for a while gets
// its share from then on, not the bytes the other sent before it came: the
// shaper's virtual time moves with what it hands to the writer.
func TestShaperNewStreamDoesNotStarveOld(t *testing.T) {
	p := newPipeSession(t)
	old, late := p.open(), p.open()
	go old.Write(make([]byte, 64<<10))
	for i := 0; i < 8; i++ {
		if f := p.read(); f.sid != old.ID() {
			t.Fatalf("frame %d %+v, want one of stream %d", i, f, old.ID())
		}
	}
	p.queued(2 + 9) // the SYNs and nine frames: the ninth in the writer
	// two writers: the late stream always has a frame queued
	for i := 0; i < 2; i++ {
		go late.Write(make([]byte, 32<<10))
	}
	p.queued(2 + 9 + 2)
	got := 0
	for i := 0; i < 6; i++ {
		time.Sleep(20 * time.Millisecond) // the writers queue their next frames meanwhile
		if p.read().sid == old.ID() {
			got++
		}
	}
	if got < 3 {
		t.Fatalf("the stream sending before got %d of the 6 frames after another started", got)
	}
}

// Many streams writing at once, in frames of every size, each get their own
// bytes through intact and in order.
func TestFairQueueStreamsIntact(t *testing.T) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	config.MaxFrameSize = 4096
	client, err := Client(c1, config)
	if err != nil {
		t.Fatal(err)
	}
	server, err := Server(c2, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	const streams, total = 16, 1 << 20
	pattern := func(sid uint32, i int) byte { return byte(i*7 + int(sid)) }
	errs := make(chan error, 2*streams)
	go func() {
		for {
			st, err := server.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 5000)
				for got := 0; got < total; {
					st.SetReadDeadline(time.Now().Add(20 * time.Second))
					n, err := st.Read(buf)
					for k := 0; k < n; k++ {
						if buf[k] != pattern(st.ID(), got+k) {
							errs <- fmt.Errorf("stream %d: byte %d is wrong", st.ID(), got+k)
							return
						}
					}
					got += n
					if err != nil {
						errs <- fmt.Errorf("stream %d: %v after %d bytes", st.ID(), err, got)
						return
					}
				}
				errs <- nil
			}()
		}
	}()
	for i := 0; i < streams; i++ {
		st, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewSource(int64(i)))
		small := i%4 == 0 // small frames
		go func() {
			for sent := 0; sent < total; {
				n := 1 + rng.Intn(20000)
				if small {
					n = 1 + rng.Intn(300)
				}
				if n > total-sent {
					n = total - sent
				}
				b := make([]byte, n)
				for k := range b {
					b[k] = pattern(st.ID(), sent+k)
				}
				if _, err := st.Write(b); err != nil {
					errs <- err
					return
				}
				sent += n
			}
		}()
	}
	for i := 0; i < streams; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
