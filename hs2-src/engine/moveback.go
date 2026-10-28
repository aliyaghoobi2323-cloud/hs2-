package engine

import (
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The move back from a BULK link to a QUIET one (R1 round 3, the judge:
// priority 5; REDESIGN §10 item 63(2): "back to the light class after 2 s of
// silence"; decision T20 placed it in R2).
//
// A connection moves to a BULK link once, for its bulk behaviour, and
// stayed there for good. A connection that carries more than one thing —
// xray's XMUX or gRPC: a game and a download, or a game after a download,
// in one TCP connection — then kept its light part on the BULK link after
// the download ended: in the field capture 43% of the light activity came
// after the connection's first bulk spell (round 3's xray report), and an
// XMUX connection lives 30 to 50 minutes.
//
// With MoveBack, a connection on a BULK link that has been light for
// moveBackLight (5 s: less than C1's light marks, 64 KiB in 2 s and 16 KiB
// in 200 ms, every tick, and no word of a backlog from the exit) moves back
// to a QUIET link, at least moveBackHold (10 s) after its latest move: the
// hysteresis. There it is judged again as a new connection (the exit's
// hint, the upload rule) and may move on. smux's group moves at most
// moveBackMax times (Config.GroupMoves), each once the stream before is let
// go at both ends.
//
// Both ends must allow it (tlscarrier.CapMoveBack, HS2_TUNE_MOVE_BACK=1 at
// both: opt-in, a lab switch in R1).

// moveBackLight, moveBackHold: the light run and the least time since the
// latest move (vars for the tests); moveBackMax: smux's GroupMoves.
var (
	moveBackLight = 5 * time.Second
	moveBackHold  = 10 * time.Second
)

const moveBackMax = 8

// MoveBackOn says whether this end moves connections back (the opt-in
// switch, HS2_TUNE_MOVE_BACK=1; its caps bit is tlscarrier's).
func MoveBackOn() bool { return TuneMoveBack != 0 && !QuietOff() }

// TuneMoveBack 1 (HS2_TUNE_MOVE_BACK=1, lab): the move back.
var TuneMoveBack = 0

// moveBackRule is a moved connection's light run.
type moveBackRule struct {
	r       moveRule // its 2 s of bytes per tick (the move rule's ring)
	run     time.Duration
	lastAt  int64
	movedAt int64
}

// tick takes the connection's counters at now and says whether it has been
// light long enough to move back.
func (b *moveBackRule) tick(tx, rx uint64, now int64, hinted bool) bool {
	b.r.tick(tx, rx, 0, now, false, false)
	if b.lastAt == 0 {
		b.lastAt = now
		return false
	}
	dt := time.Duration(now - b.lastAt)
	b.lastAt = now
	last2 := uint64(b.r.tx[b.r.pos]) + uint64(b.r.rx[b.r.pos])
	prev := (b.r.pos + moveSlots - 1) % moveSlots
	last2 += uint64(b.r.tx[prev]) + uint64(b.r.rx[prev])
	if hinted || b.r.sumTx >= classLightSpan || b.r.sumRx >= classLightSpan || last2 >= classLightBurst {
		b.run = 0
		return false
	}
	b.run += dt
	return b.run >= moveBackLight && time.Duration(now-b.movedAt) >= moveBackHold
}

// watchBack starts the move-back rule of gs, just moved to the BULK link
// bl (when both of its links allow more moves).
func (q *quietSet) watchBack(gs *groupStream, bl *mtcpLink) {
	if !MoveBackOn() || bl.caps&tlscarrier.CapMoveBack == 0 || gs.ql == nil || gs.ql.caps&tlscarrier.CapMoveBack == 0 {
		return
	}
	gs.back = moveBackRule{movedAt: ctrlNow()}
	q.gmu.Lock()
	q.onBulk = append(q.onBulk, gs)
	q.gmu.Unlock()
}

// backTick ticks the move-back rule of the connections on BULK links and
// moves back the due ones: those it moved back (for the mover's list).
func (q *quietSet) backTick(now int64) []*groupStream {
	q.gmu.Lock()
	list := q.onBulk
	q.gmu.Unlock()
	if len(list) == 0 {
		return nil
	}
	keep := list[:0:0]
	var back []*groupStream
	for _, s := range list {
		s.mu.Lock()
		gone := s.closed || s.bcs == nil
		s.mu.Unlock()
		if gone {
			continue
		}
		hinted := s.hintAt.Swap(0) != 0
		if s.back.tick(s.g.TxBytes(), s.g.RxBytes(), now, hinted) && s.g.Retired() && q.moveBack(s) {
			back = append(back, s)
			continue
		}
		keep = append(keep, s)
	}
	q.gmu.Lock()
	q.onBulk = append(keep, q.onBulk[len(list):]...)
	q.gmu.Unlock()
	return back
}

// moveBack moves gs from its BULK link to a QUIET one the set picks: true
// when it moved (the mover's list takes it again), false when no QUIET link
// took it now or its group refused (tried again at a later tick).
func (q *quietSet) moveBack(gs *groupStream) bool {
	ql, rel, ok := q.pick(nil)
	if !ok {
		return false
	}
	s1, err := gs.g.Move(ql.link.sess, gs.tag)
	if err != nil {
		rel()
		return false
	}
	gs.mu.Lock()
	bcs, brel := gs.bcs, gs.brel
	gs.qcs, gs.qrel, gs.qown, gs.ql = ql.link.track(s1), rel, true, ql.link
	gs.bcs, gs.brel = nil, nil
	closed := gs.closed
	qcs := gs.qcs
	gs.mu.Unlock()
	if closed {
		qcs.untrack()
		rel()
	}
	bcs.untrack()
	if brel != nil {
		brel()
	}
	gs.rule = moveRule{}
	gs.hintAt.Store(0)
	q.movedBack.Add(1)
	return true
}
