package engine

import (
	"encoding/binary"
	"os"
	"sync/atomic"
	"time"
)

// TUN interactive flows off links whose TCP makes no ack progress
// (HS2_B278_L3STALL, default on; both sides act on their own sends).
//
// lab3 data-dark (ab274 FA5, b277 FT3/FT4): the hs0 ping had 5-9 s gaps with
// no loss while calls did not stall. The lane (l3_lane.go) keeps an
// interactive flow on its link while the link is alive and light; a link
// whose path drops data (ACKs and keepalives still pass, so the session is
// not silent and the link never dies) holds every packet already handed to
// its TCP until RTO backoff gets one through, and every new packet joins that
// queue. The dup's second copy (l3_dup.go) is a rendezvous pick too, with no
// look at the socket, so it can be stuck the same way.
//
// With the key on, a link is "stuck" while its socket has data in flight and
// either the kernel is in RTO backoff or bytes_acked has not moved for
// max(b278StallMin, 3*srtt) (TCP_INFO, read at most every b278CheckEvery per
// link, only for links an interactive packet is about to use). A lane flow on
// a stuck link moves at once to the rendezvous pick among links that are not
// stuck, and the dup's second copy skips stuck links. Packets already in the
// stuck socket stay there (nothing can recall them); only the next ones move.
// Bulk flows: unchanged.
var b278On atomic.Bool

// b278Trace (HS2_B278_TRACE=1, lab only): one line per ICMP echo the TUN
// sends, with its seq and the path(s) and stuck state of the link(s) it went
// over.
var b278Trace = os.Getenv("HS2_B278_TRACE") == "1"

func init() { b278On.Store(envBoolDefault("HS2_B278_L3STALL", true)) }

var (
	b278StallMin   = 400 * time.Millisecond
	b278CheckEvery = int64(50 * time.Millisecond)
)

// b278Moved: lane flows moved off a stuck link; b278DupSkip: second copies
// that skipped a stuck link; b278Stuck: stuck verdicts (fresh reads).
// b278OtherPath: second copies moved off the first copy's path.
var b278Moved, b278DupSkip, b278Stuck, b278OtherPath atomic.Int64

// l3Stall is a link's ack-progress view (all atomics: pumpTun is the only
// writer in production, tests may read).
type l3Stall struct {
	checkAt atomic.Int64 // ctrlNow of the last TCP_INFO read
	progAt  atomic.Int64 // ctrlNow bytes_acked last moved (or nothing in flight)
	acked   atomic.Uint64
	stuck   atomic.Bool
}

// txStuck reports whether l's socket has data in flight with no ack
// progress (see above). A link without a socket view is never stuck.
func (l *l3Link) txStuck(now int64) bool {
	if l == nil || l.txStat == nil || !b278On.Load() {
		return false
	}
	s := &l.stall
	prev := s.checkAt.Load()
	if prev != 0 && now-prev < b278CheckEvery {
		return s.stuck.Load()
	}
	s.checkAt.Store(now)
	st, ok := l.txStat()
	if !ok {
		s.stuck.Store(false)
		return false
	}
	thr := max(int64(b278StallMin), 3*int64(st.rttUs)*int64(time.Microsecond))
	// A previous read older than the window did not watch the window: an
	// idle link (a 1 pps ping, a light link read once a second) with one
	// packet just sent would look like "no ack for seconds". Restart the
	// window then; RTO backoff still marks a really stuck socket at once.
	if st.unacked == 0 || st.bytesAcked != s.acked.Load() || s.progAt.Load() == 0 || now-prev >= thr {
		s.acked.Store(st.bytesAcked)
		s.progAt.Store(now)
	}
	stuck := st.unacked > 0 && (st.backoff > 0 || now-s.progAt.Load() >= thr)
	if stuck && !s.stuck.Load() {
		b278Stuck.Add(1)
	}
	s.stuck.Store(stuck)
	return stuck
}

// rendezvousNotStuck is rendezvous over the live links ok accepts (nil: all)
// that are not stuck. Only the winner's socket is read: a stuck winner is
// skipped and the pick repeated, at most b278Tries times (then nil).
func rendezvousNotStuck(links []*l3Link, h uint32, now int64, ok func(*l3Link) bool) *l3Link {
	var skip [b278Tries]*l3Link
	for try := 0; try < b278Tries; try++ {
		var best *l3Link
		var bestW uint32
	next:
		for _, l := range links {
			if !l.Alive() || ok != nil && !ok(l) {
				continue
			}
			for _, x := range skip[:try] {
				if x == l {
					continue next
				}
			}
			if w := mix32(h ^ l.id); best == nil || w > bestW {
				best, bestW = l, w
			}
		}
		if best == nil || !best.txStuck(now) {
			return best
		}
		skip[try] = best
	}
	return nil
}

const b278Tries = 6

// b278LanePick is the lane's pick when its flow has to (re)choose a link
// with the key on: light and off cut paths, then off cut paths, then any —
// each among links that are not stuck. nil: let the old chain decide.
func b278LanePick(links []*l3Link, h uint32, now int64) *l3Link {
	offCut := func(l *l3Link) bool { return !l3OnCut(l) }
	if l := rendezvousNotStuck(links, h, now, func(l *l3Link) bool { return offCut(l) && !l.isHeavy() }); l != nil {
		return l
	}
	if l := rendezvousNotStuck(links, h, now, offCut); l != nil {
		return l
	}
	return rendezvousNotStuck(links, h, now, nil)
}

// b278Second keeps the dup's second copy off a stuck link and off the first
// copy's path: two links of one path stall together when that path drops
// data (lab3 ab278 FZ3: both copies on the cut v6 path, 9.7 s ping gaps), so
// a second copy there saves nothing. Another path's not-stuck link (light
// first) when there is one; else as before, minus stuck links.
func b278Second(links []*l3Link, h uint32, first, second *l3Link, now int64) *l3Link {
	if second == nil || !b278On.Load() {
		return second
	}
	if first != nil && first.path != "" && second.path == first.path {
		other := func(l *l3Link) bool { return l.path != "" && l.path != first.path }
		if l := dupSecondWhere(links, h, first, func(l *l3Link) bool { return other(l) && !l.isHeavy() }); l != nil && !l.txStuck(now) {
			b278OtherPath.Add(1)
			return l
		}
		if l := dupSecondWhere(links, h, first, other); l != nil && !l.txStuck(now) {
			b278OtherPath.Add(1)
			return l
		}
	}
	if !second.txStuck(now) {
		return second
	}
	b278DupSkip.Add(1)
	var skip [b278Tries]*l3Link
	skip[0] = second
	for try := 1; try < b278Tries; try++ {
		l := dupSecondWhere(links, h, first, func(l *l3Link) bool {
			for _, x := range skip[:try] {
				if x == l {
					return false
				}
			}
			return true
		})
		if l == nil || !l.txStuck(now) {
			return l
		}
		skip[try] = l
	}
	return nil
}

// b278TraceICMP logs one ICMP echo (lab trace).
func (s *l3Set) b278TraceICMP(pkt []byte, first, second *l3Link, now int64) {
	lf := s.b278Log.Load()
	if lf == nil || len(pkt) < 28 || pkt[0]>>4 != 4 || pkt[9] != 1 {
		return
	}
	ihl := int(pkt[0]&15) * 4
	if len(pkt) < ihl+8 {
		return
	}
	typ := pkt[ihl]
	if typ != 0 && typ != 8 {
		return
	}
	seq := binary.BigEndian.Uint16(pkt[ihl+6:])
	p2, s2 := "-", false
	if second != nil {
		p2, s2 = second.path, second.stall.stuck.Load()
	}
	p1, s1 := "-", false
	if first != nil {
		p1, s1 = first.path, first.stall.stuck.Load()
	}
	(*lf)("b278 icmp type %d seq %d first %s stuck %v second %s stuck %v", typ, seq, p1, s1, p2, s2)
}
