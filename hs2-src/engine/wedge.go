package engine

import (
	"fmt"
	"io"
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
// bucket. Each user relay marks its writes to the local app, so "the same write
// still in progress since the last looks" means that app is not reading. When
// a link has been parked for wedgeLooks looks (>= 4 s) the relays stuck for
// stuckLooks looks (>= 4 s) are ended: closing their local socket ends the
// relay, closing its stream returns the stream's tokens to the bucket, and the
// link's other users keep flowing — well before the other side's 20 s. Only
// connections whose app took nothing for seconds while holding the buffer are
// closed; a slow app still reading is never one (its writes complete).

const (
	// wedgeLooks: consecutive looks a session's reader must be parked before
	// the link counts as wedged (the first look proves ~0 s, each next +tick).
	wedgeLooks = 3
	// stuckLooks: consecutive looks at which one relay write is still the
	// same in-progress write (each proves another tick of blocking).
	stuckLooks = 2
	// wedgeLogEvery throttles the guard's summary lines.
	wedgeLogEvery = 30 * time.Second
)

// guardTick is how often the guard looks (a variable only so tests can drive
// looks directly; production never changes it).
var guardTick = 2 * time.Second

// relayDieGrace: after a stream's session dies, its relay gets this long to
// hand the bytes it still holds to the local app before both ends are closed
// (a local app that is not reading would otherwise pin the stream's buffers,
// and the relay's goroutines, for good). A variable only for tests.
var relayDieGrace = 5 * time.Second

// sessGuard watches one smux session.
type sessGuard struct {
	w    *watchConn
	sess atomic.Pointer[smux.Session]

	mu        sync.Mutex
	relays    map[*relayWatch]struct{}
	lastCalls uint64
	parked    int // consecutive looks the reader was parked
	quietLog  time.Time
}

// relayWatch is one relay's local-write state as the guard sees it.
type relayWatch struct {
	wseq atomic.Uint64 // +1 entering a local Write, +1 leaving it: odd = in a write
	kill func()        // closes the local side, which ends the relay

	// guard-only (under sessGuard.mu)
	lastSeq uint64
	looks   int
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
	g.mu.Lock()
	calls := g.w.rdCalls.Load()
	if !g.w.inRead.Load() && calls == g.lastCalls {
		g.parked++
	} else {
		g.parked = 0
	}
	g.lastCalls = calls
	var stuck []*relayWatch
	for rw := range g.relays {
		s := rw.wseq.Load()
		if s&1 == 1 && s == rw.lastSeq {
			rw.looks++
		} else {
			rw.looks = 0
		}
		rw.lastSeq = s
		if rw.looks >= stuckLooks {
			stuck = append(stuck, rw)
		}
	}
	wedged := g.parked >= wedgeLooks
	if wedged && len(stuck) > 0 {
		for _, rw := range stuck {
			delete(g.relays, rw)
		}
		g.parked = 0 // give the freed bucket a fresh window
	}
	g.mu.Unlock()
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
	links    int
	logAt    time.Time
	emptyLog time.Time
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
		list = append(list, g)
	}
	gs.mu.Unlock()
	empty := 0
	for _, g := range list {
		k, e := g.look()
		if k > 0 {
			gs.killed += k
			gs.links++
		}
		if e {
			empty++
		}
	}
	lp := gs.logf.Load()
	if lp == nil {
		return
	}
	if gs.killed > 0 && now.Sub(gs.logAt) >= wedgeLogEvery {
		(*lp)("mtcp: closed %d connection(s) on %d link(s) whose app had stopped reading for 4s+ while holding the link's receive buffer — the links' other connections keep flowing",
			gs.killed, gs.links)
		gs.killed, gs.links, gs.logAt = 0, 0, now
	}
	if empty > 0 && now.Sub(gs.emptyLog) >= 10*time.Minute {
		gs.emptyLog = now
		(*lp)("mtcp: %s", fmt.Sprintf("%d link(s) stopped reading for 4s+ with no stuck connection to release (UDP/TUN backlog or a slow panel dial)", empty))
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
		rw := &relayWatch{kill: func() { local.Close() }}
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
		t := time.NewTimer(relayDieGrace)
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
