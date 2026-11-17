package engine

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// A stuck reader must not stall — or kill — a whole link.
//
// Each link's smux session holds at most SmuxSessionBuffer (8 MiB) of data its
// streams' readers have not taken yet; when that bucket is empty smux stops
// reading the link's socket until a reader catches up. Four users whose apps
// stop reading (a frozen client, a panel that hangs) each fill their 2 MiB
// stream window and so empty the bucket: then EVERY stream on the link stops —
// the other users' traffic, the control and info streams, the TUN — and after
// 20 s of zero window the other server's TCP gives up on the link
// (TCP_USER_TIMEOUT) and every user on it is reset. They reconnect, land on
// other links, and the stuck ones do it again: a cascade.
//
// The guard ends it where it starts. The session's one reader (smux recvLoop)
// is watched without a syscall: watchConn counts its Read calls, so "not in a
// Read, and no Read since the last look" means recvLoop is parked on the empty
// bucket — or parked with only a trickle of reads since the last look (a slow
// reader returning a few tokens at a time must not hide the stall). Each user
// relay marks its writes to the local app, so "a write in progress and none
// completed for stuckFor" means that app has stopped reading. When a link has
// been starved for wedgeLooks looks (>= 4 s) the stuck relays are ended:
// their local socket is reset (which ends the relay and frees the kernel's
// queue at once), closing the stream returns its tokens to the bucket, and
// the link's other users keep flowing — well before the other side's 20 s.
// A slow app that is still reading is never one: its writes complete.
//
// While either server's kernel TCP memory is above its pressure mark
// (mempressure.go) the stuck relays are ended without waiting for their link
// to starve: one stalled download per link never empties a bucket, but
// twenty of them hold enough kernel buffers to squeeze every socket on a
// 2 GB server.

const (
	// wedgeLooks: consecutive looks a session's reader must be parked before
	// the link counts as wedged (the first look proves ~0 s, each next +tick).
	wedgeLooks = 3
	// starveCalls: a reader that is out of Read now and made fewer Read calls
	// than this since the last look counts as parked (a link moving real data
	// makes thousands; a trickle of returned tokens a few hundred at most).
	starveCalls = 2048
	// stuckFor: a relay is stuck once it is in a local write and no local
	// write has completed for this long (a reader taking >= ~6 KiB/s never is).
	stuckFor = 6 * time.Second
	// wedgeLogEvery throttles the guard's summary lines.
	wedgeLogEvery = 30 * time.Second
)

// guardTick is how often the guard looks (a variable only so tests can drive
// looks directly; production never changes it).
var guardTick = 2 * time.Second

// relayDieGrace: after a stream's session dies, its relay gets this long to
// hand the bytes it still holds to the local app before both ends are closed
// (a local app that is not reading would otherwise pin the stream's buffers,
// and the relay's goroutines, for good). Atomic only so tests can shorten it.
var relayDieGrace atomic.Int64

func init() { relayDieGrace.Store(int64(5 * time.Second)) }

// sessGuard watches one smux session.
type sessGuard struct {
	w      *watchConn
	manual atomic.Bool // tests drive this guard's looks themselves
	sess   atomic.Pointer[smux.Session]

	mu        sync.Mutex
	relays    map[*relayWatch]struct{}
	lastCalls uint64
	parked    int // consecutive looks the reader was parked
	// parkedAt is when (ctrlNow) a look last found the reader parked: the
	// link's own users, not its path, were holding it up (the stuck check
	// leaves such a link to the guard; stuck.go).
	parkedAt atomic.Int64
	quietLog time.Time
	// squeezed counts the relays ended for memory pressure, not a full
	// bucket (for the log line).
	squeezed atomic.Int64

	// The post-release watch (wedge_watch.go): the link's kernel socket (nil:
	// none), its number on the edge + 1 (0: unknown), and whether a watch runs.
	tcp      *net.TCPConn
	linkID   atomic.Int32
	watching atomic.Bool
}

// relayWatch is one relay's local-write state as the guard sees it.
type relayWatch struct {
	wseq atomic.Uint64 // +1 entering a local Write, +1 leaving it: odd = in a write
	kill func()        // closes the local side, which ends the relay

	// guard-only (under sessGuard.mu)
	lastSeq  uint64
	progress time.Time // the last look at which a local write had completed
}

// watchedWriter marks writes to the local app for the guard.
type watchedWriter struct {
	w  io.Writer
	rw *relayWatch
}

func (x watchedWriter) Write(p []byte) (int, error) {
	x.rw.wseq.Add(1)
	n, err := x.w.Write(p)
	x.rw.wseq.Add(1)
	return n, err
}

func (g *sessGuard) add(rw *relayWatch) {
	g.mu.Lock()
	if g.relays == nil {
		g.relays = map[*relayWatch]struct{}{}
	}
	g.relays[rw] = struct{}{}
	g.mu.Unlock()
}

func (g *sessGuard) remove(rw *relayWatch) {
	g.mu.Lock()
	delete(g.relays, rw)
	g.mu.Unlock()
}

// look is one guard observation. It returns how many stuck relays it ended,
// and whether the reader is wedged with nothing stuck to release.
func (g *sessGuard) look() (killed int, wedgedEmpty bool) {
	return g.lookAt(time.Now())
}

func (g *sessGuard) lookAt(now time.Time) (killed int, wedgedEmpty bool) {
	g.mu.Lock()
	calls := g.w.rdCalls.Load()
	if !g.w.inRead.Load() && calls-g.lastCalls < starveCalls {
		g.parked++
		g.parkedAt.Store(ctrlNow())
	} else {
		g.parked = 0
	}
	g.lastCalls = calls
	var stuck []*relayWatch
	for rw := range g.relays {
		s := rw.wseq.Load()
		if s != rw.lastSeq || rw.progress.IsZero() {
			rw.progress = now // a write completed (or started) since the last look
		}
		rw.lastSeq = s
		if s&1 == 1 && now.Sub(rw.progress) >= stuckFor {
			stuck = append(stuck, rw)
		}
	}
	wedged := g.parked >= wedgeLooks
	squeezed := !wedged && memPressure() && len(stuck) > 0
	if (wedged || squeezed) && len(stuck) > 0 {
		for _, rw := range stuck {
			delete(g.relays, rw)
		}
		if wedged {
			g.parked = 0 // give the freed bucket a fresh window
		}
	}
	g.mu.Unlock()
	if squeezed {
		g.squeezed.Add(int64(len(stuck)))
		for _, rw := range stuck {
			go rw.kill()
		}
		return len(stuck), false
	}
	if !wedged {
		return 0, false
	}
	for _, rw := range stuck {
		go rw.kill() // a close can block briefly; never stall the guard
	}
	if len(stuck) > 0 {
		g.noteRelease() // counts it and watches the link (wedge_watch.go)
	}
	return len(stuck), len(stuck) == 0
}

// guardSet is the process-wide registry; one goroutine looks at every session.
type guardSet struct {
	mu   sync.Mutex
	all  map[*sessGuard]struct{}
	once sync.Once
	logf atomic.Pointer[func(string, ...any)]

	// summary state (run goroutine only)
	killed   int
	links    map[*sessGuard]bool // distinct links with a release since the last line
	logAt    time.Time
	emptyLog time.Time

	squeezedN    int // relays ended for memory pressure since the last line
	squeezeLogAt time.Time
}

var guards guardSet

// setGuardLog sets where the guard's summary lines go (the tunnel's log).
func setGuardLog(logf func(string, ...any)) {
	if logf != nil {
		guards.logf.Store(&logf)
	}
}

func (gs *guardSet) add(g *sessGuard) {
	gs.once.Do(func() { go gs.run() })
	gs.mu.Lock()
	if gs.all == nil {
		gs.all = map[*sessGuard]struct{}{}
	}
	gs.all[g] = struct{}{}
	gs.mu.Unlock()
}

func (gs *guardSet) run() {
	t := time.NewTicker(guardTick)
	defer t.Stop()
	for now := range t.C {
		gs.lookAll(now)
	}
}

// lookAll looks at every live session once, drops ended ones, and logs a
// throttled summary.
func (gs *guardSet) lookAll(now time.Time) {
	gs.mu.Lock()
	list := make([]*sessGuard, 0, len(gs.all))
	for g := range gs.all {
		if s := g.sess.Load(); s != nil && s.IsClosed() {
			delete(gs.all, g)
			continue
		}
		if g.manual.Load() {
			continue
		}
		list = append(list, g)
	}
	gs.mu.Unlock()
	empty := 0
	for _, g := range list {
		k, e := g.look()
		if n := g.squeezed.Swap(0); n > 0 {
			gs.squeezedN += int(n)
			k -= int(n)
		}
		if k > 0 {
			gs.killed += k
			if gs.links == nil {
				gs.links = map[*sessGuard]bool{}
			}
			gs.links[g] = true
		}
		if e {
			empty++
		}
	}
	lp := gs.logf.Load()
	if lp == nil {
		return
	}
	if gs.squeezedN > 0 && now.Sub(gs.squeezeLogAt) >= wedgeLogEvery {
		(*lp)("mtcp: reset %d connection(s) whose app had taken nothing for %s while kernel TCP memory was above its pressure mark (here or on the other server) — their buffers squeezed every socket",
			gs.squeezedN, fmtDur(stuckFor))
		gs.squeezedN, gs.squeezeLogAt = 0, now
	}
	if gs.killed > 0 && now.Sub(gs.logAt) >= wedgeLogEvery {
		(*lp)("mtcp: reset %d connection(s) on %d link(s) whose app had taken nothing for %s while the link's receive buffer was full — the links' other connections keep flowing",
			gs.killed, len(gs.links), fmtDur(stuckFor))
		gs.killed, gs.links, gs.logAt = 0, nil, now
	}
	if empty > 0 && now.Sub(gs.emptyLog) >= 10*time.Minute {
		gs.emptyLog = now
		(*lp)("mtcp: %s", fmt.Sprintf("%d link(s) stopped reading for several seconds with no stuck connection to release (UDP/TUN backlog or a slow panel dial)", empty))
	}
}

// guardOf returns the guard of a stream's link (nil when it has none).
func guardOf(st io.ReadWriteCloser) *sessGuard {
	if cs, ok := st.(*countedStream); ok && cs.link != nil && cs.link.mtr != nil {
		return cs.link.mtr.guard
	}
	return nil
}

// relayStream relays a user connection over a smux stream. Like relay it
// returns when either direction ends, closing both. In addition the stream→
// local direction is watched by the link's guard g (nil: unguarded), and when
// the stream's session dies the relay ends after relayDieGrace even if the
// local app neither reads nor writes.
//
// fx170: when the stream ended abnormally before its peer's whole data
// reached local — a read error other than io.EOF (a reset, every sub died, a
// dead link), or the stream dead before its EOF was relayed — local is closed
// with an RST (SetLinger(0)), not a FIN: a cut download (edge) or upload
// (exit) is never handed to the app as a complete one. reset reports it. And
// when local ends with an error (it was reset), a stream that can (an mConn)
// is aborted, so the far side resets its end too rather than taking a FIN.
func relayStream(local, st io.ReadWriteCloser, g *sessGuard) (reset bool) {
	return relayStreamHC(local, st, g, hcOff)
}

// relayStreamHC is relayStream; hc (hc183, halfclose.go): the peer announced
// half-close, so an EOF on one direction ends only that direction (a
// CloseWrite on the other end) and the relay goes on until the other
// direction ends too (or halfCloseIdle without progress: a reset).
func relayStreamHC(local, st io.ReadWriteCloser, g *sessGuard, mode hcMode) (reset bool) {
	hc := mode != hcOff && halfCloseOn.Load()
	if hc {
		if h, ok := st.(halfOpener); ok {
			h.SetHalfOpen(true) // smux: writes outlive the peer's FIN
		}
	}
	var dst io.Writer = local
	if g != nil {
		rw := &relayWatch{kill: func() {
			// A reset, not a FIN: the app took nothing for seconds, and a
			// FIN would leave its unread data queued in the kernel for
			// minutes; a reset frees it at once.
			if tc, ok := local.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			local.Close()
		}}
		g.add(rw)
		defer g.remove(rw)
		dst = watchedWriter{w: local, rw: rw}
	}
	var die <-chan struct{}
	if d, ok := st.(interface{ GetDieCh() <-chan struct{} }); ok {
		die = d.GetDieCh()
	}
	down := &relayEnd{r: st}
	up := &relayEnd{r: local}
	done := make(chan struct{}, 2)
	downDone := make(chan struct{})
	// hc183: set (before done) when a direction ended with EOF and its
	// CloseWrite took: the relay then waits for the other direction
	var downHalf, upHalf atomic.Bool
	go func() {
		relayCopy(dst, down)
		if hc && down.state.Load() == relayEndEOF && halfGoneNow(st) {
			// hc213: the peer dropped the stream (closed it without a
			// CloseWrite: its user reset, its relay cut): the EOF is a cut,
			// not a FIN — local is reset below, not half-closed
			down.state.Store(relayEndErr)
			halfCloseAborts.Add(1)
		}
		if hc && down.state.Load() == relayEndEOF {
			if cw, ok := local.(closeWriter); ok && cw.CloseWrite() == nil {
				downHalf.Store(true)
			}
		}
		close(downDone)
		done <- struct{}{}
	}()
	go func() {
		relayCopy(st, up)
		if hc && up.state.Load() == relayEndEOF {
			if cw, ok := st.(closeWriter); ok && cw.CloseWrite() == nil {
				upHalf.Store(true)
			}
		}
		done <- struct{}{}
	}()
	got := 0
	select {
	case <-done:
		got = 1
	case <-die:
	}
	half, idle := false, false
	if got == 1 && (downHalf.Load() || upHalf.Load()) && halfOpenAdmit() {
		// hc183: one direction is over and half-closed; wait for the other
		// one, the stream's death, or halfCloseIdle without progress (over
		// halfCloseMax: the plain close at once, as before hc183)
		half = true
		halfCloses.Add(1)
		limit := mode.idle()
		t := time.NewTicker(time.Duration(halfClosePoll.Load()))
		last, quiet := down.n.Load()+up.n.Load(), time.Now()
		var gone <-chan struct{}
		if downHalf.Load() {
			// hc183b: the peer's FIN was read; only this side writes on.
			// The peer dropping the stream ends it (no reader is left)
			gone = halfGone(st)
		}
	wait:
		for {
			select {
			case <-done:
				got = 2
				break wait
			case <-die:
				break wait
			case <-gone:
				if v, ok := st.(interface{ goneValid() bool }); ok && !v.goneValid() {
					gone = nil // records carry it now: their own end counts
					continue
				}
				select {
				case <-done: // ended on its own meanwhile
					got = 2
				default:
					halfCloseGone.Add(1)
				}
				break wait
			case now := <-t.C:
				if n := down.n.Load() + up.n.Load(); n != last {
					last, quiet = n, now
				} else if now.Sub(quiet) >= limit {
					idle = true
					halfCloseIdles.Add(1)
					break wait
				}
			}
		}
		t.Stop()
		halfOpenNow.Add(-1)
	}
	dead := false
	if die != nil {
		select {
		case <-die:
			dead = true
		default:
		}
	}
	if dead && down.state.Load() == relayEndNone {
		// The stream ended under us (its link died, or it was closed). The
		// stream→local copier still drains what the stream holds (on a
		// stream closed here it ends at once); give it a moment, then close
		// whatever is left. fx170: wait for that copier itself — the other
		// one ends at once on a dead stream, and its end said nothing about
		// whether the download was all relayed.
		t := time.NewTimer(time.Duration(relayDieGrace.Load()))
		select {
		case <-downDone:
		case <-t.C:
		}
		t.Stop()
	}
	cut := down.state.Load() == relayEndErr || down.state.Load() == relayEndNone && dead
	if c, ok := st.(interface{ ClosedHere() bool }); ok && cut && c.ClosedHere() {
		// closed on purpose on this side (an idle connection reclaimed
		// from a retiring link, an evacuation): a FIN, as before
		cut = false
	}
	closedHere := false
	if c, ok := st.(interface{ ClosedHere() bool }); ok {
		closedHere = c.ClosedHere()
	}
	// hc183: half-closed, the remaining direction did not end with EOF (cut,
	// a failed write, the stream's death, halfCloseIdle): the side still
	// waiting is reset, and so is the far end
	halfCut := half && !closedHere && (idle || down.state.Load() != relayEndEOF || up.state.Load() != relayEndEOF)
	if cut || halfCut {
		if lc, ok := local.(interface{ SetLinger(int) error }); ok {
			lc.SetLinger(0)
		}
		reset = true
	}
	if up.state.Load() == relayEndErr || halfCut {
		if a, ok := st.(interface{ abort() }); ok {
			a.abort()
		}
	}
	local.Close()
	st.Close()
	for ; got < 2; got++ {
		<-done
	}
	return reset
}

// relayEnd states: how a relay direction's reads ended so far.
const (
	relayEndNone = iota // still reading (or it ended on a write)
	relayEndEOF         // io.EOF: the peer's whole data was read and written
	relayEndErr         // any other read error: the data was cut
)

// relayEnd is a relay direction's reader; it records how the reads ended.
// io.EOF with bytes is split (the bytes now, the EOF on the next read), so
// relayEndEOF means every byte was also written on.
type relayEnd struct {
	r     io.Reader
	state atomic.Int32
	n     atomic.Int64 // hc183: bytes read (the half-closed relay's progress)
}

func (e *relayEnd) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if n > 0 {
		e.n.Add(int64(n))
	}
	switch {
	case err == nil:
	case err == io.EOF:
		if n > 0 {
			return n, nil // the EOF again on the next read
		}
		e.state.Store(relayEndEOF)
	default:
		e.state.Store(relayEndErr)
	}
	return n, err
}

// fx170 counters: user connections (edge) reset by relayStream because the
// download ended abnormally — mConn and one-stream paths — and targets (exit,
// one-stream path) reset because the upload did.
var (
	mconnUserRst   atomic.Uint64
	streamLocalRst atomic.Uint64
)
