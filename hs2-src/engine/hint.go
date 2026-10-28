package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The exit's move hint (R1 round 3, the judge; REDESIGN §10 item 63(2): a
// connection moves to BULK on bulk behaviour "and a persistent send queue").
//
// The edge moves a group connection off its QUIET link (quiet.go). For what
// the edge sends (an upload) it sees its own writer wait. For what it
// receives (a download) round 2 had a stand-in: data arriving, at least
// moveRxMin a tick, three ticks in a row. That stand-in fails both ways,
// measured on the rig:
//
//   - when the QUIET link itself is slow, nothing arrives fast enough: under
//     1% loss (CUBIC on QUIET ~2 Mbit/s) one bulk connection of 17 moved and
//     the sum fell to 6-10 Mbit/s against 88 for the base; with 25 users of
//     16 connections each (425 on one QUIET link) no connection moved;
//   - an app-limited flow that sends steadily moves although it never waits:
//     a 1.5 Mbit/s video call moved in under half a second and then sat
//     behind BULK's own queue and BBR's PROBE_RTT (p99 +192 ms).
//
// The exit is the sender of a download and sees its backlog: its relay
// reads the panel's socket in 32 KiB chunks and writes them to the group,
// and a connection whose panel has more than the tunnel takes keeps that
// writer inside big writes. So the exit judges each of its group
// connections each moveTick (hintRule): a writer inside writes of at least
// hintWriteMin bytes for at least hintBusy of each of hintRun ticks in a row
// (300 ms) has a persistent send backlog, and the exit says so to the edge
// (kindHint), which moves it. A light flow never writes big chunks: a stall
// of the QUIET link (an RTO under loss) holds its small writes, not big
// ones; an app-limited flow writes what its app made, a frame at a time.
// With the hint the edge's stand-in is off for downloads (quietSet.move).
//
// Wire: the stream kindHint, which the edge opens on each QUIET link whose
// caps have CapHint (level 2, both ends), the exit writes 8-byte messages
// on: [ver:1 = 1][type:1 = 1 backlog][0:2][group id:4 big-endian]. Nothing
// goes the other way. HS2_TUNE_MOVE_HINT=0 takes the bit out (each end's
// own: with either end off the edge keeps round 2's stand-in).

// TuneMoveHint 0 (HS2_TUNE_MOVE_HINT=0): no move hint; the edge moves
// downloads by data arriving (round 2's stand-in).
var TuneMoveHint = 1

// MoveHintOff says whether the move hint is off.
func MoveHintOff() bool { return TuneMoveHint == 0 || QuietOff() }

const (
	kindHint    byte = 11
	hintMsgLen       = 8
	hintVer          = 1
	hintBacklog      = 1
	// hintWriteMin: a write this big is a chunk of a backlog (the relay's
	// buffer is 32 KiB; an app-limited flow's writes are its own frames)
	hintWriteMin = 8 << 10
	// hintBusy, hintRun: the writer inside big writes at least hintBusy of
	// each of hintRun moveTicks in a row (300 ms) is backlogged
	hintBusy = 0.5
	hintRun  = 3
	// hintResend: a hinted connection still on QUIET this long after is
	// hinted again (a lost or late hint)
	hintResend = time.Second
)

func hintEncode(b []byte, id uint32) {
	b[0], b[1], b[2], b[3] = hintVer, hintBacklog, 0, 0
	binary.BigEndian.PutUint32(b[4:8], id)
}

// hintDecode reads one message: the group id it names (ok false: not a
// message this end knows, or no id).
func hintDecode(b []byte) (id uint32, ok bool) {
	if len(b) < hintMsgLen || b[0] != hintVer || b[1] != hintBacklog {
		return 0, false
	}
	id = binary.BigEndian.Uint32(b[4:8])
	return id, id != 0
}

// bigWrites is what a writer spent inside writes of at least hintWriteMin
// bytes (ns), the one in progress included.
type bigWrites struct {
	busy atomic.Int64
	at   atomic.Int64 // monoNow-like (ctrlNow) when the current big write began (0: none)
}

// write runs w(p) and counts its time when p is big.
func (b *bigWrites) write(p []byte, w func([]byte) (int, error)) (int, error) {
	if len(p) < hintWriteMin {
		return w(p)
	}
	t0 := ctrlNow()
	b.at.Store(t0)
	n, err := w(p)
	if at := b.at.Swap(0); at != 0 {
		b.busy.Add(ctrlNow() - at)
	}
	return n, err
}

// total is the time so far, ns.
func (b *bigWrites) total(now int64) int64 {
	t := b.busy.Load()
	if at := b.at.Load(); at != 0 && now > at {
		t += now - at
	}
	return t
}

// hintRule is one connection's backlog rule at the exit.
type hintRule struct {
	ticked bool
	at     int64 // the latest tick (now / moveTick)
	last   int64 // big-write time at the latest tick
	lastAt int64
	run    int
	sentAt int64 // when it was last hinted (0: never)
}

// tick takes the big-write time so far (busy, ns) at now and says whether
// the connection is backlogged (hintRun ticks in a row past hintBusy).
func (r *hintRule) tick(busy, now int64) bool {
	k := now / int64(moveTick)
	if !r.ticked {
		r.ticked, r.at, r.last, r.lastAt = true, k, busy, now
		return false
	}
	steps := k - r.at
	if steps <= 0 {
		return false
	}
	r.at = k
	dt, db := now-r.lastAt, busy-r.last
	r.last, r.lastAt = busy, now
	if steps == 1 && dt > 0 && float64(db) >= hintBusy*float64(dt) {
		r.run++
	} else {
		r.run = 0
	}
	return r.run >= hintRun
}

// ---- the exit's end ----

// hintChan is the exit's end of one kindHint stream (one QUIET link's).
type hintChan struct {
	st  io.WriteCloser
	mtr *linkMeter
	wmu sync.Mutex
}

// exitHintSet is the exit's kindHint streams, and its tallies.
type exitHintSet struct {
	mu    sync.Mutex
	chans []*hintChan
	sent  atomic.Int64 // hints sent
	again atomic.Int64 // ... of them sent again
	fail  atomic.Int64 // hints no stream took
}

var exitHints exitHintSet

func (s *exitHintSet) add(c *hintChan) {
	s.mu.Lock()
	s.chans = append(s.chans, c)
	s.mu.Unlock()
}

func (s *exitHintSet) remove(c *hintChan) {
	s.mu.Lock()
	for i, x := range s.chans {
		if x == c {
			s.chans = append(s.chans[:i], s.chans[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

// send tells the edge that group id is backlogged, on the stream of the
// link it is on (mtr) or else any (the ids are the edge's, one per edge).
func (s *exitHintSet) send(id uint32, mtr *linkMeter) bool {
	s.mu.Lock()
	cs := make([]*hintChan, 0, len(s.chans))
	for _, c := range s.chans {
		if c.mtr == mtr {
			cs = append([]*hintChan{c}, cs...)
		} else {
			cs = append(cs, c)
		}
	}
	s.mu.Unlock()
	var b [hintMsgLen]byte
	hintEncode(b[:], id)
	for _, c := range cs {
		c.wmu.Lock()
		if d, ok := c.st.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		}
		_, err := c.st.Write(b[:])
		c.wmu.Unlock()
		if err == nil {
			return true
		}
	}
	return false
}

// serveHint is the exit's end of a kindHint stream the edge opened (its
// kind byte read): it is the link's hint channel until it ends.
func serveHint(st io.ReadWriteCloser, mtr *linkMeter) {
	if MoveHintOff() {
		st.Close()
		return
	}
	c := &hintChan{st: st, mtr: mtr}
	exitHints.add(c)
	defer exitHints.remove(c)
	io.Copy(io.Discard, st) // the edge writes nothing more: until it ends
	st.Close()
}

// runExitHints judges the exit's group connections each moveTick and hints
// the backlogged ones, until ctx ends.
func runExitHints(ctx context.Context) {
	if MoveHintOff() {
		return
	}
	t := time.NewTicker(moveTick)
	defer t.Stop()
	rules := map[*exitGroup]*hintRule{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		exitHintTick(rules, ctrlNow())
	}
}

// exitHintTick is one look at the exit's group connections (rules: their
// rules, kept by the caller).
func exitHintTick(rules map[*exitGroup]*hintRule, now int64) {
	live := exitGroups.live()
	seen := make(map[*exitGroup]bool, len(live))
	for _, eg := range live {
		seen[eg] = true
		if m := eg.mtr.Load(); eg.Moved() != nil && (m == nil || !m.quietLink) {
			delete(rules, eg) // on a BULK link now (moved back: judged again)
			continue
		}
		r := rules[eg]
		if r == nil {
			r = &hintRule{}
			rules[eg] = r
		}
		if !r.tick(eg.big.total(now), now) {
			continue
		}
		if r.sentAt != 0 && time.Duration(now-r.sentAt) < hintResend {
			continue
		}
		if exitHints.send(eg.ID(), eg.mtr.Load()) {
			if r.sentAt != 0 {
				exitHints.again.Add(1)
			}
			r.sentAt = now
			exitHints.sent.Add(1)
		} else {
			exitHints.fail.Add(1)
		}
	}
	for eg := range rules {
		if !seen[eg] {
			delete(rules, eg)
		}
	}
}

// line is the exit's hint status line.
func (s *exitHintSet) line() string {
	s.mu.Lock()
	n := len(s.chans)
	s.mu.Unlock()
	cap := ""
	if QuietCap > 0 {
		cap = fmt.Sprintf("; capped %d (moved %d, held to the end %d)", capStats.capped.Load(), capStats.moved.Load(), capStats.timeout.Load())
	}
	return fmt.Sprintf("move hints: %d streams; sent %d (again %d), none took %d%s", n, s.sent.Load(), s.again.Load(), s.fail.Load(), cap)
}

// ---- the edge's end ----

// openHint runs the edge's end of kindHint on a QUIET link whose caps have
// CapHint: the exit's hints go to q (the pool's QUIET set) until the stream
// or ctx ends.
func openHint(ctx context.Context, l Link, q *quietSet) {
	ml, ok := l.(*mtcpLink)
	if !ok || q == nil || MoveHintOff() || !ml.quiet || ml.caps&tlscarrier.CapHint == 0 {
		return
	}
	st, err := ml.OpenRawStream()
	if err != nil {
		return
	}
	defer st.Close()
	if _, err := st.Write([]byte{kindHint}); err != nil {
		return
	}
	q.hintUp(ml)
	defer q.hintDown(ml)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			st.Close()
		case <-done:
		}
	}()
	var b [hintMsgLen]byte
	for {
		if _, err := io.ReadFull(st, b[:]); err != nil {
			return
		}
		if id, ok := hintDecode(b[:]); ok {
			q.noteHint(id)
		}
	}
}
