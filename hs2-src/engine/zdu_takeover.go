package engine

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ZDU (proposals/zdu350.md) slot S6: the engine half of the takeover.
//
// With HS2_ZDU=1 every TCP listener the engine opens (listenReuseRcvBuf: the
// Iran user ports, the Kharej link listener, the dg ports, the LB) goes
// through zduListen:
//   - in a new process that adopted fds from the old one (ZDUAdoptListener),
//     a listener of the same address is built on the received socket (same
//     accept queue: nothing is refused while both processes hold it);
//   - every listener is registered as a pausable zduLn, so the old process
//     can hand the fds over (ZDUListenerFiles), stop accepting after GO
//     (ZDUPauseAccept) and resume when the new process fails its probation
//     (ZDUResumeAccept): the socket never closes, so the revert needs no bind.
//
// Key off: listenReuseRcvBuf never calls into this file (one atomic load).

type zduLn struct {
	*net.TCPListener
	name   string
	link   bool // a link listener (ListenLink): accepts in a new process before GO (zdu_gate.go)
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
	closed bool
	// dropped (review M2): this process closed its copy of the socket at the
	// commit (zduDropLinkLns); Accept waits like paused until Close.
	dropped bool
	rcvbuf  int // b428: a rebind after a revert sets the same buffer
}

// zduLinkPaused (review M2): link accept is paused in this process (old
// process, handover); a link listener created meanwhile (an LB reload in the
// warm wait) starts paused too.
var zduLinkPaused atomic.Bool

// zduOldPaused (rc9): this process handed its users over (old process after
// GO, until a revert resumes it). A user port still waiting on EADDRINUSE
// (listen_retry.go) does not bind meanwhile: the port belongs to the new
// process. A socket registered in that state starts paused.
var zduOldPaused atomic.Bool

var zduLns struct {
	mu      sync.Mutex
	m       map[string]*zduLn   // "tcp:"+addr -> registered listener
	adopted map[string]*os.File // "tcp:"+addr -> fd received from the old process
}

// Counters (the takeover log lines read them).
var (
	ZDUAdoptedListeners atomic.Int64 // listeners built on a received fd
	ZDUColdListeners    atomic.Int64 // key on, no fd for the address: fresh bind
	ZDUPauses           atomic.Int64
	ZDUResumes          atomic.Int64
	// ZDULateAccepts: connections an Accept took after the pause (the
	// accept(2) raced the pause kick); served by this process.
	ZDULateAccepts atomic.Int64
)

// zduAcceptHook (tests only): runs after a successful accept, before the
// late-accept check.
var zduAcceptHook atomic.Pointer[func(*zduLn)]

// ZDULnName is the hand-off FD name of the TCP listener on addr.
func ZDULnName(addr string) string { return "tcp:" + addr }

// ZDUAdoptListener records a listener fd received from the old process
// (handoff FD name "tcp:ADDR"); the next listen on ADDR uses it.
func ZDUAdoptListener(name string, f *os.File) {
	zduLns.mu.Lock()
	defer zduLns.mu.Unlock()
	if zduLns.adopted == nil {
		zduLns.adopted = map[string]*os.File{}
	}
	if old := zduLns.adopted[name]; old != nil {
		old.Close()
	}
	zduLns.adopted[name] = f
}

// ZDUDropAdopted closes every adopted fd no listener used (a leftover means
// the new config no longer binds that address).
func ZDUDropAdopted() (n int) {
	return len(ZDUDropAdoptedNames())
}

// ZDUDropAdoptedNames is ZDUDropAdopted returning the hand-off names it
// closed (b428: the new process sends them to the old one as UNUSED).
func ZDUDropAdoptedNames() (names []string) {
	zduLns.mu.Lock()
	for k, f := range zduLns.adopted {
		f.Close()
		delete(zduLns.adopted, k)
		names = append(names, k)
	}
	zduLns.mu.Unlock()
	return append(names, zduDropAdoptedUDP()...)
}

func zduListen(addr string, rcvbuf int) (net.Listener, error) {
	return zduListenKind(addr, rcvbuf, false)
}

func zduListenKind(addr string, rcvbuf int, link bool) (net.Listener, error) {
	name := ZDULnName(addr)
	zduLns.mu.Lock()
	f := zduLns.adopted[name]
	delete(zduLns.adopted, name)
	zduLns.mu.Unlock()
	var ln net.Listener
	if f != nil {
		l, err := net.FileListener(f)
		f.Close() // FileListener dups it
		if err != nil {
			// Review F3: a handed-over address is never cold-bound (the old
			// process still owns the port): fail the start instead.
			return nil, fmt.Errorf("zdu: handed-off listener %s: %w", name, err)
		}
		ln = l
		ZDUAdoptedListeners.Add(1)
	}
	if ln == nil {
		l, err := listenReuseRcvBufCold(addr, rcvbuf)
		if err != nil {
			return nil, err
		}
		ln = l
		ZDUColdListeners.Add(1)
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return ln, nil
	}
	z := &zduLn{TCPListener: tl, name: name, link: link, rcvbuf: rcvbuf}
	z.cond = sync.NewCond(&z.mu)
	zduLns.mu.Lock()
	// Review issue 4: a new process holds its user listeners until GO (the
	// shared accept queue stays with the old process, which still accepts).
	z.paused = (!link && (zduHold.Load() || zduOldPaused.Load())) || (link && zduLinkPaused.Load())
	if zduLns.m == nil {
		zduLns.m = map[string]*zduLn{}
	}
	zduLns.m[name] = z
	zduLns.mu.Unlock()
	return z, nil
}

// Accept waits while the listener is paused (the old process after GO): the
// shared accept queue then belongs to the new process alone. A connection
// taken just before the pause kick is served here (the old process drains
// what it has).
//
// Gate fix (late accept): the pause cannot be made atomic with the accept
// syscall through package net. An Accept already past its deadline check
// (pollDesc.prepareRead) when setPaused sets the past deadline still runs one
// accept(2) and can take a connection that arrived after the pause. That
// connection is in this process now (it cannot go back to the kernel queue),
// so it is served here exactly like one taken just before the pause: the old
// process drains it. At most one per Accept caller per pause (the next Accept
// waits). ZDULateAccepts counts them (zdu counters line).
func (z *zduLn) Accept() (net.Conn, error) {
	for {
		z.mu.Lock()
		for (z.paused || z.dropped) && !z.closed {
			z.cond.Wait()
		}
		tl := z.TCPListener // b428: a revert may rebind a dropped listener
		z.mu.Unlock()
		c, err := tl.Accept()
		if err == nil {
			if h := zduAcceptHook.Load(); h != nil {
				(*h)(z)
			}
			z.mu.Lock()
			late := z.paused || z.dropped
			z.mu.Unlock()
			if late {
				ZDULateAccepts.Add(1)
			}
			return c, nil
		}
		z.mu.Lock()
		dr, cl := z.dropped, z.closed
		z.mu.Unlock()
		if dr && !cl {
			continue // our copy was dropped at the commit: wait for Close
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			z.mu.Lock()
			cl := z.closed
			z.mu.Unlock()
			if !cl {
				continue // the pause kick (or a resume that raced it)
			}
		}
		return nil, err
	}
}

func (z *zduLn) Close() error {
	z.mu.Lock()
	z.closed = true
	z.cond.Broadcast()
	tl := z.TCPListener
	z.mu.Unlock()
	zduLns.mu.Lock()
	if zduLns.m[z.name] == z {
		delete(zduLns.m, z.name)
	}
	zduLns.mu.Unlock()
	return tl.Close()
}

func (z *zduLn) setPaused(p bool) {
	z.mu.Lock()
	z.paused = p
	z.cond.Broadcast()
	tl := z.TCPListener
	z.mu.Unlock()
	if p {
		tl.SetDeadline(time.Unix(1, 0)) // kick a blocked Accept out
	} else {
		tl.SetDeadline(time.Time{})
	}
}

// drop closes this process's copy of the listening socket but keeps the
// wrapper (Accept waits until Close): a socket the new process also holds
// stays open there; one it does not (its config dropped the IP) is gone, so
// dials to it are refused instead of queueing in a paused backlog.
func (z *zduLn) drop() {
	z.mu.Lock()
	if z.dropped || z.closed {
		z.mu.Unlock()
		return
	}
	z.dropped = true
	z.cond.Broadcast()
	tl := z.TCPListener
	z.mu.Unlock()
	tl.Close()
}

func zduAll() []*zduLn {
	zduLns.mu.Lock()
	defer zduLns.mu.Unlock()
	out := make([]*zduLn, 0, len(zduLns.m))
	for _, z := range zduLns.m {
		out = append(out, z)
	}
	return out
}

// ZDUPauseAccept stops accept on every registered listener (the sockets stay
// open: they are the revert). Returns how many.
func ZDUPauseAccept() int {
	zduOldPaused.Store(true)
	ls := zduAll()
	for _, z := range ls {
		z.setPaused(true)
	}
	ps := zduAllPC() // S7: UDP sockets stop reading too
	for _, z := range ps {
		z.setPaused(true)
	}
	ZDUPauses.Add(1)
	return len(ls) + len(ps)
}

// ZDUResumeAccept re-opens accept on every registered listener (the revert).
func ZDUResumeAccept() int {
	zduOldPaused.Store(false)
	ls := zduAll()
	for _, z := range ls {
		z.setPaused(false)
	}
	ps := zduAllPC()
	for _, z := range ps {
		z.setPaused(false)
	}
	ZDUResumes.Add(1)
	return len(ls) + len(ps)
}

// ZDUListenerFiles dups every registered listener's fd for the hand-off
// (name "tcp:ADDR"). The caller sends and closes them.
func ZDUListenerFiles() (names []string, files []*os.File, err error) {
	for _, z := range zduAll() {
		z.mu.Lock()
		tl, dr := z.TCPListener, z.dropped || z.closed
		z.mu.Unlock()
		if dr {
			continue // b428: a dropped copy is not ours to hand over
		}
		f, e := tl.File()
		if e != nil {
			for _, g := range files {
				g.Close()
			}
			return nil, nil, fmt.Errorf("zdu: dup %s: %w", z.name, e)
		}
		names = append(names, z.name)
		files = append(files, f)
	}
	// S7: the UDP sockets ("udp:ADDR") go with the listeners.
	un, uf, err := zduPacketFiles()
	if err != nil {
		for _, g := range files {
			g.Close()
		}
		return nil, nil, err
	}
	return append(names, un...), append(files, uf...), nil
}

// LinkManagers created with the key on: the drain report sums them.
var zduLMs struct {
	mu sync.Mutex
	l  []*LinkManager
}

func zduRegisterLM(m *LinkManager) {
	if !zduOn.Load() {
		return
	}
	zduLMs.mu.Lock()
	zduLMs.l = append(zduLMs.l, m)
	zduLMs.mu.Unlock()
}

// ZDUDrainStats is this process's DRAINSTAT: live links and open user
// connections across its link pools.
func ZDUDrainStats() (links, users int) {
	zduLMs.mu.Lock()
	ls := append([]*LinkManager(nil), zduLMs.l...)
	zduLMs.mu.Unlock()
	for _, m := range ls {
		if m.closing.Load() {
			continue
		}
		links += m.alive()
		users += int(m.users.Load())
	}
	// S7: the exit (Kharej) has no LinkManager; its links and the user
	// connections riding them are in the exit's live link set. Without this
	// a Kharej old process reported users 0 and left right after probation,
	// cutting every connection it still carried.
	l, u := zduExitPeers.Load().zduDrain()
	return links + l, users + u
}

// zduDrain: the exit's live links and the user connections on them.
func (p *linkPeers) zduDrain() (links, users int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for m := range p.m {
		links++
		users += int(m.users.Load())
	}
	return links, users
}

// ZDUForeignDrainAll feeds the old process's DRAINSTAT links to every link
// pool of this (new, Iran edge) process: the foreign-drain term of S4.
func ZDUForeignDrainAll(links int) {
	zduLMs.mu.Lock()
	ls := append([]*LinkManager(nil), zduLMs.l...)
	zduLMs.mu.Unlock()
	for _, m := range ls {
		m.SetZDUForeignDrain(links)
	}
}

// ZDUPools is the number of link pools created with the key on.
func ZDUPools() int {
	zduLMs.mu.Lock()
	n := len(zduLMs.l)
	zduLMs.mu.Unlock()
	if zduExitPeers.Load() != nil {
		n++ // S7: the exit's link set (Kharej) is a pool for the warm wait
	}
	return n
}

// tcpListenerOf is the *net.TCPListener under ln (a zduLn wraps one).
func tcpListenerOf(ln net.Listener) *net.TCPListener {
	switch l := ln.(type) {
	case *zduLn:
		return l.TCPListener
	case *net.TCPListener:
		return l
	}
	return nil
}
