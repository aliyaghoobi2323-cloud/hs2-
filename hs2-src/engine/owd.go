package engine

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// α's own-direction queue delay (R1 round 2, the judge's fix 2 to β2): the
// one-way delay of small timestamped messages the two ends of each BULK
// link trade on that link (stream kindOWD, which the edge opens on each
// BULK link).
//
// α read srtt − min_rtt of its BULK socket. On a socket that carries both
// directions that holds both directions' queues: the acks of a download
// wait behind the upload's data (the same five-tuple, so the same fq_codel
// flow queue, or the same FIFO). The judge's rig cell of a 100 Mbit/s
// download beside a 20 Mbit/s upload held the download at 17 of 100
// Mbit/s: α at the exit cut for the upload's queue (β1's α: 9). A message
// riding the link's own data in one direction waits in that direction's
// queue only, the one this end's sending makes:
//
//   - each end stamps a message with its monotonic clock (ctrlNow) every
//     owdTickOf(rate) while the link is busy; the receiver keeps the least
//     raw delay (its clock less the stamp: the clocks' offset and the path)
//     of each second over owdWin as the base, and the least of the readings
//     of the last owdLook less the base is the queue the other end's
//     sending meets in this direction (qIn): the least, so a message held
//     behind a lost segment for a round trip is no queue;
//   - each message carries the sender's qIn, so each end learns the queue
//     of its own sending direction (qOut) a message later; α reads that in
//     place of srtt − min_rtt.
//
// The messages ride the link's own frames (the shaper's carry puts them in
// the records of the data going that way); a direction with no data pads
// each to a record (~1 KB), so the beat follows the link's rate (1% of it
// at most, owdTickOf). A first version traded them on the QUIET link: on
// fq_codel a QUIET link sees no BULK queue, so α never cut and CoDel
// dropped 4% of a BULK link's segments (one_vs_16, N=2, on the rig).
//
// Wire (kindOWD stream, after the one kind byte), one message each way per
// beat: [ver:1 = 1][0:3][stampNs:8 big-endian][qInUs:4 big-endian;
// 0xFFFFFFFF none]. Only level-2 peers have BULK links, so only they see
// the kind; HS2_TUNE_OWD=0 keeps α on srtt − min_rtt (the stream is still
// served).

const (
	kindOWD    byte = 10
	owdMsgLen       = 16
	owdTickMin      = 25 * time.Millisecond
	owdTickMax      = time.Second
	// owdWin: the seconds the least raw delay is kept over (the base)
	owdWin = 30
	// owdLook: the span the queue is the least reading of (at least two
	// beats): a standing queue raises every reading, a hole one or two
	owdLook = 200 * time.Millisecond
	// owdRecBits: the bits one message costs when it has a record of its
	// own (a shaped record, ~991 B on average), for the beat's 1% rule
	owdRecBits = 8 * 1000
	owdNone    = 0xFFFFFFFF
	// owdFresh: a queue heard longer ago than this is none
	owdFresh = 3 * owdTickMax
)

// TuneOWD 0 (HS2_TUNE_OWD=0): α reads srtt − min_rtt (β2's).
var TuneOWD = 1

// OWDOff says whether α's one-way delay is off.
func OWDOff() bool { return TuneOWD == 0 }

// owdBase is the least raw one-way delay of each of the last owdWin
// seconds (a ring): the base the queue is read against.
type owdBase struct {
	mins [owdWin]int64
	have [owdWin]bool
	sec  int64
	init bool
}

// add notes raw (ns) at second sec and returns the queue part of it (raw
// less the least of the span, never negative).
func (b *owdBase) add(sec, raw int64) int64 {
	if !b.init || sec-b.sec >= owdWin || sec < b.sec {
		b.have = [owdWin]bool{}
		b.sec, b.init = sec, true
	}
	for s := b.sec + 1; s <= sec; s++ {
		b.have[s%owdWin] = false
	}
	b.sec = sec
	k := sec % owdWin
	if !b.have[k] || raw < b.mins[k] {
		b.mins[k], b.have[k] = raw, true
	}
	lo := raw
	for i := range b.mins {
		if b.have[i] && b.mins[i] < lo {
			lo = b.mins[i]
		}
	}
	return raw - lo
}

// owdLook is the readings of the last owdLook (ns at, queue ns), a ring.
type owdRecent struct {
	at, q [64]int64
	n, h  int
}

func (r *owdRecent) add(at, q int64) int64 {
	for r.n > 0 && time.Duration(at-r.at[r.h]) > owdLook {
		r.h, r.n = (r.h+1)%len(r.at), r.n-1
	}
	if r.n == len(r.at) {
		r.h, r.n = (r.h+1)%len(r.at), r.n-1
	}
	k := (r.h + r.n) % len(r.at)
	r.at[k], r.q[k] = at, q
	r.n++
	lo := q
	for i := 0; i < r.n; i++ {
		lo = min(lo, r.q[(r.h+i)%len(r.at)])
	}
	return lo
}

// owdLink is one link's two one-way queue delays (ns) and when each was
// last heard (ctrlNow; 0: never), with the link's rate for the beat.
type owdLink struct {
	mu     sync.Mutex
	base   owdBase
	recent owdRecent

	qIn, qInAt   atomic.Int64
	qOut, qOutAt atomic.Int64
	rate, rateAt atomic.Int64 // float64 bits, bytes/s; ctrlNow
	msgs         atomic.Int64
}

// noteRate is the link's delivery rate (bytes/s) for the beat.
func (o *owdLink) noteRate(r float64) {
	if o == nil {
		return
	}
	o.rate.Store(int64(math.Float64bits(r)))
	o.rateAt.Store(ctrlNow())
}

// tick is the beat now: owdTickOf the link's rate, owdTickMax when unknown.
func (o *owdLink) tick() time.Duration {
	if at := o.rateAt.Load(); at == 0 || time.Duration(ctrlNow()-at) > owdFresh {
		return owdTickMax
	}
	return owdTickOf(math.Float64frombits(uint64(o.rate.Load())))
}

// owdTickOf is the beat for a link rate r (bytes/s; 0: unknown): one
// message per beat costs at most 1% of r, within [owdTickMin, owdTickMax].
func owdTickOf(r float64) time.Duration {
	if r <= 0 {
		return owdTickMax
	}
	t := time.Duration(float64(owdRecBits) / (0.01 * r * 8) * float64(time.Second))
	return min(max(t, owdTickMin), owdTickMax)
}

// queue is the queue delay of this end's sending direction on the link,
// as the other end last measured it (false: none heard lately, OWD off).
func (o *owdLink) queue() (time.Duration, bool) {
	if o == nil || OWDOff() {
		return 0, false
	}
	at := o.qOutAt.Load()
	if at == 0 || time.Duration(ctrlNow()-at) > owdFresh {
		return 0, false
	}
	return time.Duration(o.qOut.Load()), true
}

// recv takes a message heard at now.
func (o *owdLink) recv(now int64, b []byte) bool {
	stamp, qOut, have, ok := owdDecode(b)
	if !ok {
		return false
	}
	o.mu.Lock()
	q := o.base.add(now/int64(time.Second), now-stamp)
	q = o.recent.add(now, q)
	o.mu.Unlock()
	o.qIn.Store(q)
	o.qInAt.Store(now)
	if have {
		o.qOut.Store(int64(qOut))
		o.qOutAt.Store(now)
	}
	o.msgs.Add(1)
	return true
}

func owdEncode(b []byte, stamp int64, qIn int64, have bool) {
	b[0], b[1], b[2], b[3] = 1, 0, 0, 0
	binary.BigEndian.PutUint64(b[4:12], uint64(stamp))
	q := uint32(owdNone)
	if have {
		q = uint32(min(max(qIn/1000, 0), owdNone-1))
	}
	binary.BigEndian.PutUint32(b[12:16], q)
}

func owdDecode(b []byte) (stamp int64, qOut time.Duration, have, ok bool) {
	if len(b) < owdMsgLen || b[0] != 1 {
		return 0, 0, false, false
	}
	stamp = int64(binary.BigEndian.Uint64(b[4:12]))
	q := binary.BigEndian.Uint32(b[12:16])
	if q == owdNone {
		return stamp, 0, false, true
	}
	return stamp, time.Duration(q) * time.Microsecond, true, true
}

// owdLoop trades the messages of link o on st (the kind byte already
// written or read) until it fails or ctx ends; both ends run it.
func owdLoop(ctx context.Context, st io.ReadWriteCloser, o *owdLink) {
	defer st.Close()
	var once sync.Once
	stop := make(chan struct{})
	end := func() { once.Do(func() { close(stop); st.Close() }) }
	go func() {
		defer end()
		var b [owdMsgLen]byte
		for {
			if _, err := io.ReadFull(st, b[:]); err != nil {
				return
			}
			if !o.recv(ctrlNow(), b[:]) {
				return
			}
		}
	}()
	var b [owdMsgLen]byte
	for {
		t := time.NewTimer(o.tick())
		select {
		case <-ctx.Done():
			t.Stop()
			end()
			return
		case <-stop:
			t.Stop()
			return
		case <-t.C:
		}
		at := o.qInAt.Load()
		fresh := at != 0 && time.Duration(ctrlNow()-at) <= owdFresh
		owdEncode(b[:], ctrlNow(), o.qIn.Load(), fresh)
		if _, err := st.Write(b[:]); err != nil {
			end()
			return
		}
	}
}

// openOWD runs the edge's end of kindOWD on a BULK link (others: none).
func openOWD(ctx context.Context, l Link) {
	ml, ok := l.(*mtcpLink)
	if !ok || !ml.bulk || OWDOff() || ml.mtr == nil {
		return
	}
	st, err := ml.OpenRawStream()
	if err != nil {
		return
	}
	if _, err := st.Write([]byte{kindOWD}); err != nil {
		st.Close()
		return
	}
	owdLoop(ctx, st, &ml.mtr.owd)
}
