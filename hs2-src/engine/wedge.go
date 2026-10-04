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
func relayStream(local, st io.ReadWriteCloser, g *sessGuard) {
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
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		bp := copyBufs.Get().(*[]byte)
		io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *bp)
		copyBufs.Put(bp)
		done <- struct{}{}
	}
	go cp(dst, st)
	go cp(st, local)
	got := 0
	select {
	case <-done:
		got = 1
	case <-die:
		// The stream ended under us (its link died, or it was closed). The
		// stream→local copier still drains what the stream holds; give it a
		// moment, then close whatever is left.
		t := time.NewTimer(time.Duration(relayDieGrace.Load()))
		select {
		case <-done:
			got = 1
		case <-t.C:
		}
		t.Stop()
	}
	local.Close()
	st.Close()
	for ; got < 2; got++ {
		<-done
	}
}
