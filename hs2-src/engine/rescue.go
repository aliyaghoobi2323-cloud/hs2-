package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// A new connection on a link that went black before the exit answered it.
//
// The pool stops placing new connections on a link the moment its traffic
// is seen waiting on the path (managedLink.lagging), but the first sign
// comes half a second or more after the link goes black (a new stream's
// answer waits openAnswerWait, the oldest control ping lagWait). A
// connection that opened on the link before has nothing to tell it the link
// is gone: it waits for the link's TCP to give up (20 s), then fails — 12 in
// 2400 new connections while a link was black-holed, in a test with a new
// user every 100 ms (36 when an answer waited a second).
//
// Its stream does tell, though. The exit reads a new stream's kind byte at
// once, and smux v2 announces a stream's window at its first read, so on a
// working path every user stream is answered (smux Stream.Answered) about a
// round trip after it opened, whatever the panel does. So a user connection
// whose app has sent nothing yet, or one TLS ClientHello (most panels: VLESS
// or Trojan over TLS or REALITY), is looked at rescueWait after its open (and
// every rescueRecheck after that while it waits): a stream still unanswered
// on a link a new connection would now be steered off, while a link that
// does not lag is there to go to (LinkManager.stranded), and that has
// received nothing at all, nor had a waiting write go through, for as long
// as a working link stays silent after a send (linkQuiet) — gone, not just
// slow — moves: its stream is opened again on another link and those bytes
// are sent again there; the app notices nothing. A TLS server does nothing
// with a ClientHello but answer it, and a second copy is a separate
// handshake on a separate panel connection — safe even when the first copy
// reached the panel and only the way back died. In the gap-2 test (4 links,
// one black-holed both ways, only on the way out or only on the way back, a
// new user every 100 ms) every such user that opened on the dead link had
// its first byte 1.56 s after its open (RTT 300 ms: 1.81 s) instead of
// failing after 20 s.
//
// Anything else the app sent stays where it is, as before. It may already
// have done something at the panel, or still do: a segment lost on a live
// link arrives an RTO or two later, and on an idle link that looks just like
// a dead one here. Resetting the connection so its app reconnects at once
// was tried, and ran requests twice (5 in 540 users at 10% loss and a 300 ms
// round trip); such a user still waits for the link's TCP to give up.
//
// A slow link is left alone: its other streams still trickle in. On a pool
// a DPI box slows down every link lags in turn, and with lag alone as the
// signal, in the same test, a reset turned a slow answer into an error and
// a move put a user back at the end of another link's queue (its first byte
// 15–19 s after its open, against 12.6 s for the median user). So is a slow
// link busy with an upload of its own: it reads next to nothing, and a new
// stream there waits behind the upload, but its writes go through
// (linkMeter.sentAt); without that check, on a 400 kbit/s link carrying an
// upload, connections moved off it that were only waiting their turn.
// A connection moves at most once, and moves are rate-limited for the whole
// pool (rescueBurst, rescueRate), so a misjudged signal can never send a
// pool's handshakes twice wholesale.

// rescueWaitMin: how long a new stream waits for its answer before its link
// is judged (rescueWait); rescueRTTs round trips of the link if that is
// longer — a far but working path answers only after one. It is past
// openAnswerWait, so a link that stayed silent since the open lags by then.
// rescueRecheck: a stream still unanswered on a link that looks fine is
// judged again this often, until rescueWatchMax after its open (by then a
// dead link's session has ended). (Variables only so the tests can shorten
// them.)
var (
	rescueWaitMin = 1500 * time.Millisecond
	rescueRecheck = 500 * time.Millisecond
)

const (
	rescueRTTs     = 3
	rescueWatchMax = 20 * time.Second
	// rescueKeep: what the app sends before the answer is kept for a move
	// up to this much (a ClientHello is 0.3–2 KiB); past it the connection
	// stays where it is.
	rescueKeep = 16 << 10
	// rescueBurst, rescueRate: the moves a pool makes at most at once, and
	// per second after that.
	rescueBurst = 32
	rescueRate  = 8
	// rescueLogEvery: at most one rescue line this often.
	rescueLogEvery = 30 * time.Second
)

// rescueWait is how long a new stream on l waits for its answer before l is
// judged.
func rescueWait(l Link) time.Duration {
	d := rescueWaitMin
	if mtr := linkMeterOf(l); mtr != nil {
		if rtt := rescueRTTs * time.Duration(mtr.rttMicros.Load()) * time.Microsecond; rtt > d {
			d = min(rtt, lagQuiet)
		}
	}
	return d
}

// answerer is a stream that knows whether the exit has answered it (smux
// Stream.Answered, through countedStream).
type answerer interface{ Answered() bool }

// answered reports whether the exit has answered st; a stream that cannot
// tell counts as answered (it is never rescued).
func answered(st stream) bool {
	a, ok := st.(answerer)
	return !ok || a.Answered()
}

// streamOn is a user connection's stream, the link it is on, and the
// connection's slot there.
type streamOn struct {
	st      stream
	link    Link
	release func()
}

// rescuable is a user connection's stream that can move to another link
// until the exit answers it. relayStream sees one stream: Read and Write go
// to the stream the connection is on, and the end of a stream it moved off
// is not its end (GetDieCh).
type rescuable struct {
	ctx  context.Context
	lm   *LinkManager
	port int

	cur atomic.Pointer[streamOn]
	// settled: the connection stays on the stream it is on — the exit
	// answered it, the app got data from it or sent what cannot be sent
	// again, it moved or ended, or the watch is over. Read and Write go
	// straight through then.
	settled atomic.Bool

	mu     sync.Mutex
	kept   []byte    // what the app has sent, while keep
	keep   bool      // kept is all the app has sent: it may move
	opened time.Time // when the current stream opened
	timer  *time.Timer
	closed bool
	moved  chan struct{} // closed when the connection moves (move)
}

// newRescuable wraps the stream st just opened on link for a user connection
// to port, and starts watching it for an answer.
func newRescuable(ctx context.Context, lm *LinkManager, port int, st stream, link Link, release func()) *rescuable {
	r := &rescuable{ctx: ctx, lm: lm, port: port, opened: time.Now()}
	r.cur.Store(&streamOn{st: st, link: link, release: release})
	if _, ok := st.(answerer); !ok || link == nil || lm == nil {
		r.settled.Store(true) // nothing to tell an answer by: a plain relay
		return r
	}
	r.mu.Lock()
	r.keep = true
	r.moved = make(chan struct{})
	r.timer = time.AfterFunc(rescueWait(link), r.check)
	r.mu.Unlock()
	return r
}

// on returns the stream the connection is on now.
func (r *rescuable) on() stream { return r.cur.Load().st }

func (r *rescuable) Read(p []byte) (int, error) {
	for {
		on := r.cur.Load()
		n, err := on.st.Read(p)
		if r.settled.Load() && r.cur.Load() == on {
			return n, err
		}
		r.mu.Lock()
		if r.cur.Load() != on {
			r.mu.Unlock()
			continue // it moved meanwhile: what the old stream gave is not the app's
		}
		if n > 0 {
			r.settleLocked() // the app has its answer: it stays
		}
		r.mu.Unlock()
		return n, err
	}
}

func (r *rescuable) Write(p []byte) (int, error) {
	if r.settled.Load() {
		return r.cur.Load().st.Write(p)
	}
	r.mu.Lock()
	on := r.cur.Load()
	kept := false
	switch {
	case !r.keep:
	case answered(on.st), len(r.kept)+len(p) > rescueKeep:
		r.settleLocked()
	default:
		r.kept = append(r.kept, p...)
		if kept = replayable(r.kept); !kept {
			r.settleLocked() // it could do something at the panel twice: it stays
		}
	}
	r.mu.Unlock()
	if kept {
		return r.writeKept(on, p)
	}
	n, err := on.st.Write(p)
	if err != nil && r.cur.Load() != on {
		return len(p), nil // it moved: p went out on the new stream with the rest
	}
	return n, err
}

// writeKept writes p, which kept holds too, to the stream on, but stops
// waiting once the connection moves: p then went out on the new stream with
// the rest, while a write on a link that went black waits for its session's
// writer until the session ends (closing the old stream does not end it), and
// the app's next bytes would wait behind it. The old stream gets a copy of p:
// its queued frame may still point at it after this returns.
func (r *rescuable) writeKept(on *streamOn, p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	b := append([]byte(nil), p...)
	done := make(chan result, 1)
	go func() {
		n, err := on.st.Write(b)
		done <- result{n, err}
	}()
	select {
	case res := <-done:
		if res.err != nil && r.cur.Load() != on {
			return len(p), nil
		}
		return res.n, res.err
	case <-r.moved:
		return len(p), nil
	}
}

// Close closes the stream the connection is on and ends the watch.
func (r *rescuable) Close() error {
	r.mu.Lock()
	r.closed = true
	r.settleLocked()
	on := r.cur.Load()
	r.mu.Unlock()
	return on.st.Close()
}

// release gives back the connection's slot on the link it ended on.
func (r *rescuable) release() {
	r.mu.Lock()
	r.closed = true
	r.settleLocked()
	on := r.cur.Load()
	r.mu.Unlock()
	on.release()
}

// GetDieCh is the current stream's: relayStream asks again when it closes. A
// move under way decides first which stream that is (the old one may end
// while the move is sending the app's bytes on the new one).
func (r *rescuable) GetDieCh() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return dieChOf(r.cur.Load().st)
}

// settleLocked keeps the connection where it is for good. Caller holds r.mu.
func (r *rescuable) settleLocked() {
	r.settled.Store(true)
	r.keep, r.kept = false, nil
	if r.timer != nil {
		r.timer.Stop()
	}
}

// check is the watch: it judges the connection's stream (see the top of the
// file) and looks again later if it cannot tell yet.
func (r *rescuable) check() {
	r.mu.Lock()
	on := r.cur.Load()
	if r.closed || r.settled.Load() {
		r.mu.Unlock()
		return
	}
	if answered(on.st) || time.Since(r.opened) >= rescueWatchMax {
		r.settleLocked()
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if !r.lm.stranded(on.link) || !linkQuiet(on.link) || !r.lm.rescue.take(time.Now()) {
		r.recheck()
		return
	}
	if st, link, release, ok := openStreamOn(r.ctx, r.lm, false, r.port, on.link); ok {
		if r.move(on, &streamOn{st: st, link: link, release: release}) {
			r.lm.noteRescue()
			return
		}
		release()
		go st.Close()
	}
	r.mu.Lock()
	r.settleLocked() // no other link took it, or it was answered meanwhile: it stays
	r.mu.Unlock()
}

// recheck looks at the connection's stream again rescueRecheck from now.
func (r *rescuable) recheck() {
	r.mu.Lock()
	if !r.closed && !r.settled.Load() {
		r.timer.Reset(rescueRecheck)
	}
	r.mu.Unlock()
}

// linkQuiet reports whether l is down, or has received nothing — not even a
// trickle of another stream's data, a keepalive or a control answer — for
// as long as a working link stays silent after a send (quietWait), while
// its session's reader waits on the socket, and the network has not taken a
// waiting write of it meanwhile (linkMeter.sentAt): gone, not just slow. A
// reader that is not reading (parked on a full receive buffer, wedge.go)
// reads nothing however alive the path is: this side holds the link up
// then, and the wedge guard frees it. A link busy with an upload of its own
// reads next to nothing while its new stream waits behind the upload — but
// its writes go through. A link without a meter cannot tell.
func linkQuiet(l Link) bool {
	if !l.Alive() {
		return true
	}
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	if g := mtr.guard; g != nil && !g.w.inRead.Load() {
		return false
	}
	now, quiet := ctrlNow(), quietWait(mtr)
	if s := mtr.sentAt.Load(); s != 0 && now-s < quiet {
		return false
	}
	rx := mtr.rxAt.Load()
	return rx == 0 || now-rx >= quiet
}

// move puts the connection on nw for good and sends there what the app has
// sent so far, and reports whether it did — it does not when the old stream
// was answered meanwhile, the app sent what cannot be sent again or the
// connection ended (all of which settle it), or nw failed.
func (r *rescuable) move(old, nw *streamOn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.settled.Load() || r.cur.Load() != old || answered(old.st) {
		return false
	}
	if len(r.kept) > 0 {
		wd, _ := nw.st.(interface{ SetWriteDeadline(time.Time) error })
		if wd != nil {
			wd.SetWriteDeadline(time.Now().Add(openTimeout))
		}
		if _, err := nw.st.Write(r.kept); err != nil {
			return false
		}
		if wd != nil {
			wd.SetWriteDeadline(time.Time{})
		}
	}
	r.cur.Store(nw)
	r.settleLocked() // a connection moves once
	close(r.moved)   // a write still waiting on the old stream returns (writeKept)
	old.release()
	go old.st.Close() // ends its reads at once; its FIN waits for the dead link's writer
	return true
}

// replayable reports whether what an app sent on a stream that got no answer
// can be sent again on another stream without the panel acting on it twice:
// nothing, or one TLS ClientHello (or its start).
func replayable(b []byte) bool {
	return len(b) == 0 || clientHelloPrefix(b)
}

// clientHelloPrefix reports whether b is one TLS ClientHello, or the start of
// one, and nothing else: whole TLS handshake record headers (content type
// 22, version 3.1 to 3.4, 1 to 2^14 bytes), the records carrying one
// handshake message of type client_hello with a TLS (3.x) version — in one
// record or split over several, as clients that fight censorship do — and
// no byte past the message's end.
func clientHelloPrefix(b []byte) bool {
	var msg []byte
	for len(b) > 0 {
		if len(b) < 5 || b[0] != 22 || b[1] != 3 || b[2] < 1 || b[2] > 4 {
			return false
		}
		n := int(b[3])<<8 | int(b[4])
		if n == 0 || n > 1<<14 {
			return false
		}
		b = b[5:]
		take := min(n, len(b))
		msg = append(msg, b[:take]...)
		b = b[take:]
	}
	if len(msg) == 0 || msg[0] != 1 {
		return false
	}
	if len(msg) >= 4 && len(msg) > 4+(int(msg[1])<<16|int(msg[2])<<8|int(msg[3])) {
		return false // more than the hello
	}
	return len(msg) < 5 || msg[4] == 3
}

// rescueState is a pool's move rate limit and log (LinkManager.rescue).
type rescueState struct {
	mu     sync.Mutex
	tokens float64
	at     time.Time
	moved  int // since the last line
	logAt  time.Time
	moves  atomic.Int64 // in all
}

// take spends one move from the rate limit, if one is left.
func (s *rescueState) take(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.IsZero() {
		s.tokens = rescueBurst
	} else if d := now.Sub(s.at); d > 0 {
		s.tokens = min(rescueBurst, s.tokens+d.Seconds()*rescueRate)
	}
	s.at = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// noteRescue counts a connection moved and logs, throttled.
func (m *LinkManager) noteRescue() {
	s := &m.rescue
	s.moves.Add(1)
	now := m.now()
	s.mu.Lock()
	s.moved++
	if !s.logAt.IsZero() && now.Sub(s.logAt) < rescueLogEvery {
		s.mu.Unlock()
		return
	}
	mv := s.moved
	s.logAt, s.moved = now, 0
	s.mu.Unlock()
	m.log("mtcp: %d new connection(s) got no answer from the exit on a link that went quiet: moved to another link (their app had sent nothing yet, or a TLS hello, sent again there)", mv)
}
