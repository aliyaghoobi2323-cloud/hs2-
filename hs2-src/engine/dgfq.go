package engine

import (
	"os"
	"sync"
	"time"
)

// A carrier's send queue: deficit round robin over its flows, with the flows
// that have nothing queued served first — fq_codel's new-flow list, without
// CoDel — so a game's, a call's, a DNS query's or a ping's packet never waits
// behind a download on the same carrier, and downloads share the carrier
// evenly. It replaces one FIFO per carrier, where an interactive packet that
// shared a carrier with a download waited behind up to dgSojourn (50 ms) of
// it in this queue and up to the pacer's 20 ms after it: on a busy pool,
// where every carrier carries some download, that was ping under load.
//
// What a packet waits for now:
//
//   - here, a sparse flow — one with nothing queued that has sent under
//     fqSparseRate lately — is served before every flow that has a backlog
//     (one quantum of it, then it queues with the others), and the flows with
//     a backlog take turns by bytes. (fq_codel gives the head start to any
//     flow that was idle, and keeps one that just emptied on its old list
//     until its turn comes round: a game's next packet, 16 ms later, then
//     waited a whole round behind the downloads. Here a flow that emptied
//     gets the head start again as soon as its next packet comes — but only
//     while it stays under the rate, so a heavy flow paced just below the
//     carrier's rate cannot keep it and starve the downloads.)
//   - in the carrier's pacer, a packet from a flow served that way goes in
//     the pacer's fast lane (after FEC parity, before the data queue: see
//     udpcarrier's SendUrgent) — but only if none of the flow's packets went
//     the ordinary way for fqUrgentGap, so a flow's packets never overtake
//     each other (a TCP flow that bursts takes the ordinary lane, and its
//     next lone packets wait until its ordinary ones have left);
//   - when the queue is full, the packet dropped is the head of the flow with
//     the most queued (fq_codel's choice), not the newcomer: the download that
//     filled the queue loses, not the ping that arrived last.
//
// The sojourn bound (dgSojourn), the queue's size (dgQueueLen) and what
// counts as pressure are unchanged. HS2_DG_FQ=0 puts every packet in one flow
// and the fast lane off: the FIFO of before.

const (
	// fqQuantum: bytes a flow sends per turn (and the head start a flow with
	// nothing queued gets) — one full packet at the tunnel's MTU.
	fqQuantum = 1500
	// fqUrgentGap: a flow that sent a packet the ordinary way this recently
	// sends none through the pacer's fast lane — longer than its ordinary
	// packets can wait there (the pacer holds ~20 ms at its rate), so the fast
	// one can never overtake them.
	fqUrgentGap = 100 * time.Millisecond
	// fqForget: a flow with nothing queued keeps its record (when it last
	// went the ordinary way) this long, then the record is dropped.
	fqForget = 2 * time.Second
	// fqSparseRate / fqSparseBurst: a flow is sparse — served ahead of the
	// backlog — while it sends under this rate (bytes/s), with this much
	// burst: a game, a call's audio, DNS, a ping, a remote shell, the first
	// packets of any new connection. Video and downloads are not.
	fqSparseRate  = 256e3 / 8
	fqSparseBurst = 8 << 10
)

// dgFQOff turns the fair queue off (HS2_DG_FQ=0): one FIFO per carrier.
var dgFQOff = os.Getenv("HS2_DG_FQ") == "0"

const (
	fqNone = iota
	fqNew
	fqOld
)

// fqFlow is one flow's queue in a carrier's scheduler.
type fqFlow struct {
	id         uint32
	q          []qpkt // FIFO: q[head:]
	head       int
	bytes      int
	credit     int
	list       int8 // fqNone, fqNew, fqOld
	next, prev *fqFlow
	slowAt     time.Time // last packet sent the ordinary way (not urgent)
	idleAt     time.Time // when its queue last emptied
	// sparse: a token bucket at fqSparseRate (bytes), spent by everything the
	// flow sends; tokAt is when it was last filled.
	tok   float64
	tokAt time.Time
}

// sparseFor fills the flow's bucket to now and reports whether a packet of n
// bytes still finds it under fqSparseRate.
func (f *fqFlow) sparseFor(n int, now time.Time) bool {
	if f.tokAt.IsZero() {
		f.tok = fqSparseBurst
	} else if d := now.Sub(f.tokAt).Seconds(); d > 0 {
		f.tok = min(fqSparseBurst, f.tok+d*fqSparseRate)
	}
	f.tokAt = now
	return f.tok >= float64(n)
}

func (f *fqFlow) len() int { return len(f.q) - f.head }

func (f *fqFlow) push(p qpkt) {
	if f.head > 0 && f.head == len(f.q) {
		f.q, f.head = f.q[:0], 0
	}
	f.q = append(f.q, p)
	f.bytes += len(*p.b)
}

func (f *fqFlow) pop() qpkt {
	p := f.q[f.head]
	f.q[f.head] = qpkt{}
	f.head++
	f.bytes -= len(*p.b)
	if f.head == len(f.q) {
		f.q, f.head = f.q[:0], 0
	}
	return p
}

// fqList is an intrusive doubly linked list of flows.
type fqList struct{ head, tail *fqFlow }

func (l *fqList) pushBack(f *fqFlow) {
	f.next, f.prev = nil, l.tail
	if l.tail != nil {
		l.tail.next = f
	} else {
		l.head = f
	}
	l.tail = f
}

func (l *fqList) remove(f *fqFlow) {
	if f.prev != nil {
		f.prev.next = f.next
	} else {
		l.head = f.next
	}
	if f.next != nil {
		f.next.prev = f.prev
	} else {
		l.tail = f.prev
	}
	f.next, f.prev = nil, nil
}

// fqSched is one carrier's send queue (see above). Safe for concurrent use;
// one writer pops.
type fqSched struct {
	mu         sync.Mutex
	flows      map[uint32]*fqFlow
	newL, oldL fqList
	n          int // packets queued
	limit      int
	off        bool          // HS2_DG_FQ=0: every packet in flow 0, never urgent
	wake       chan struct{} // 1-buffered: something was queued
	sweptAt    time.Time
}

func newFQSched(limit int) *fqSched {
	return &fqSched{flows: map[uint32]*fqFlow{}, limit: limit, off: dgFQOff, wake: make(chan struct{}, 1)}
}

// push queues a packet of flow. When the queue is full the head of the flow
// with the most queued is dropped and returned (to recycle and count) — the
// newcomer itself when its flow is that one.
func (s *fqSched) push(p qpkt, flow uint32) (dropped *qpkt) {
	if s.off {
		flow = 0
	}
	s.mu.Lock()
	f := s.flows[flow]
	if f == nil {
		f = &fqFlow{id: flow}
		s.flows[flow] = f
	}
	if s.n >= s.limit && s.off {
		s.mu.Unlock()
		return &p // the FIFO of before: the newcomer is dropped
	}
	if s.n >= s.limit {
		fat := f
		for _, g := range s.flows {
			if g.len() > fat.len() {
				fat = g
			}
		}
		if fat == f && f.len() == 0 {
			s.mu.Unlock()
			return &p // nothing queued anywhere fatter: the newcomer goes
		}
		if fat.len() > 0 {
			d := fat.pop()
			s.n--
			dropped = &d
			if fat.len() == 0 {
				fat.idleAt = p.t
			}
		}
	}
	f.push(p)
	s.n++
	switch {
	case f.list == fqNone && f.sparseFor(len(*p.b), p.t):
		f.credit = fqQuantum
		f.list = fqNew
		s.newL.pushBack(f)
	case f.list == fqNone:
		f.credit = fqQuantum // a heavy flow back after a pause: its turn with the others
		f.list = fqOld
		s.oldL.pushBack(f)
	case f.list == fqOld && f.len() == 1 && f.sparseFor(len(*p.b), p.t):
		// It had emptied and waits on the old list for its turn to be
		// removed: a sparse flow's next packet gets the head start now.
		s.oldL.remove(f)
		f.credit = fqQuantum
		f.list = fqNew
		s.newL.pushBack(f)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return dropped
}

// pop takes the next packet to send: the flows with nothing queued first,
// then the others in turn by bytes. urgent says the packet may take the
// pacer's fast lane (it came off the new-flow list, and its flow sent nothing
// the ordinary way for fqUrgentGap). ok is false when nothing is queued.
func (s *fqSched) pop(now time.Time) (p qpkt, urgent, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(now)
	for {
		l, fromNew := &s.newL, true
		if l.head == nil {
			l, fromNew = &s.oldL, false
		}
		f := l.head
		if f == nil {
			return qpkt{}, false, false
		}
		if f.credit <= 0 {
			f.credit += fqQuantum
			l.remove(f)
			f.list = fqOld
			s.oldL.pushBack(f)
			continue
		}
		if f.len() == 0 {
			l.remove(f)
			if fromNew {
				// A flow that emptied on the new list goes to the old one
				// once (fq_codel), so it cannot keep the head start by
				// sending one packet at a time faster than it is served.
				f.list = fqOld
				s.oldL.pushBack(f)
			} else {
				f.list = fqNone
			}
			continue
		}
		p = f.pop()
		s.n--
		f.credit -= len(*p.b)
		f.tok -= float64(len(*p.b)) // all it sends counts against its rate
		if f.len() == 0 {
			f.idleAt = now
		}
		urgent = fromNew && !s.off && now.Sub(f.slowAt) >= fqUrgentGap
		if !urgent {
			f.slowAt = now
		}
		return p, urgent, true
	}
}

// sweep drops the records of flows idle for fqForget (under s.mu, at most
// once a second).
func (s *fqSched) sweep(now time.Time) {
	if now.Sub(s.sweptAt) < time.Second {
		return
	}
	s.sweptAt = now
	for id, f := range s.flows {
		if f.list == fqNone && f.len() == 0 && now.Sub(f.idleAt) >= fqForget && now.Sub(f.slowAt) >= fqForget {
			delete(s.flows, id)
		}
	}
}

// queued is the number of packets waiting.
func (s *fqSched) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// drain empties the queue, handing each packet to put (carrier closing).
func (s *fqSched) drain(put func(qpkt)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.flows {
		for f.len() > 0 {
			put(f.pop())
		}
	}
	s.flows = map[uint32]*fqFlow{}
	s.newL, s.oldL, s.n = fqList{}, fqList{}, 0
}
