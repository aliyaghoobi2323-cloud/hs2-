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

// ZDU (proposals/zdu350.md) slot S7: UDP sockets in the handover.
//
// With HS2_ZDU=1 every UDP socket the engine binds on a user-facing address
// (Iran user ports with "udp": true, the dg ports, the dg tun port, the
// forward_ports proxy) goes through listenPacketUDP and is a registered
// zduPC. The old process hands the fd over as "udp:ADDR" (SCM_RIGHTS dups
// the socket: one kernel socket, one receive queue, the same port, so the
// new process needs no bind and nothing is refused). After GO the old
// process stops reading it (ZDUPauseAccept: the deadline kick, the socket
// stays open) and the new process takes every datagram; the old process
// keeps WRITING the replies of its own flows on the same socket while they
// drain (same source address and port, so the client sees no change). A
// revert in probation resumes the reads (ZDUResumeAccept).
//
// Key off: listenPacketUDP is net.ListenPacket after one atomic load.

type zduPC struct {
	net.PacketConn
	name   string
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
	closed bool
}

var zduPCs struct {
	mu      sync.Mutex
	m       map[string]*zduPC
	adopted map[string]*os.File
}

// ZDUAdoptedUDP / ZDUColdUDP count UDP sockets built on a received fd and
// fresh binds with the key on.
var (
	ZDUAdoptedUDP atomic.Int64
	ZDUColdUDP    atomic.Int64
)

// ZDUPCName is the hand-off FD name of the UDP socket on addr.
func ZDUPCName(addr string) string { return "udp:" + addr }

// listenPacketUDP binds a user-facing UDP socket (adoptable, pausable with
// the key on).
func listenPacketUDP(addr string) (net.PacketConn, error) {
	if !zduOn.Load() {
		return net.ListenPacket("udp", addr)
	}
	return zduListenPacket(addr)
}

// ZDUAdoptPacketConn records a UDP fd received from the old process (name
// "udp:ADDR"); the next listenPacketUDP on ADDR uses it.
func ZDUAdoptPacketConn(name string, f *os.File) {
	zduPCs.mu.Lock()
	defer zduPCs.mu.Unlock()
	if zduPCs.adopted == nil {
		zduPCs.adopted = map[string]*os.File{}
	}
	if old := zduPCs.adopted[name]; old != nil {
		old.Close()
	}
	zduPCs.adopted[name] = f
}

func zduDropAdoptedUDP() (n int) {
	zduPCs.mu.Lock()
	defer zduPCs.mu.Unlock()
	for k, f := range zduPCs.adopted {
		f.Close()
		delete(zduPCs.adopted, k)
		n++
	}
	return n
}

func zduListenPacket(addr string) (net.PacketConn, error) {
	name := ZDUPCName(addr)
	zduPCs.mu.Lock()
	f := zduPCs.adopted[name]
	delete(zduPCs.adopted, name)
	zduPCs.mu.Unlock()
	var pc net.PacketConn
	if f != nil {
		c, err := net.FilePacketConn(f)
		f.Close() // FilePacketConn dups it
		if err != nil {
			// Review F3: never cold-bind a handed-over address.
			return nil, fmt.Errorf("zdu: handed-off udp socket %s: %w", name, err)
		}
		pc = c
		ZDUAdoptedUDP.Add(1)
	}
	if pc == nil {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return nil, err
		}
		pc = c
		ZDUColdUDP.Add(1)
	}
	z := &zduPC{PacketConn: pc, name: name}
	z.cond = sync.NewCond(&z.mu)
	zduPCs.mu.Lock()
	z.paused = zduHold.Load() // review issue 4: no read before GO (zdu_gate.go)
	if zduPCs.m == nil {
		zduPCs.m = map[string]*zduPC{}
	}
	zduPCs.m[name] = z
	zduPCs.mu.Unlock()
	return z, nil
}

// ReadFrom waits while paused (the old process after GO: the socket's queue
// belongs to the new process). The pause kick's timeout is never returned.
func (z *zduPC) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		z.mu.Lock()
		for z.paused && !z.closed {
			z.cond.Wait()
		}
		z.mu.Unlock()
		n, a, err := z.PacketConn.ReadFrom(p)
		if err == nil {
			return n, a, nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			z.mu.Lock()
			cl := z.closed
			z.mu.Unlock()
			if !cl {
				continue // the pause kick (or a resume that raced it); the engine's UDP loops set no deadline of their own
			}
		}
		return n, a, err
	}
}

func (z *zduPC) Close() error {
	z.mu.Lock()
	z.closed = true
	z.cond.Broadcast()
	z.mu.Unlock()
	zduPCs.mu.Lock()
	if zduPCs.m[z.name] == z {
		delete(zduPCs.m, z.name)
	}
	zduPCs.mu.Unlock()
	return z.PacketConn.Close()
}

func (z *zduPC) setPaused(p bool) {
	z.mu.Lock()
	z.paused = p
	z.cond.Broadcast()
	z.mu.Unlock()
	if p {
		z.PacketConn.SetReadDeadline(time.Unix(1, 0)) // kick a blocked ReadFrom out
	} else {
		z.PacketConn.SetReadDeadline(time.Time{})
	}
}

func zduAllPC() []*zduPC {
	zduPCs.mu.Lock()
	defer zduPCs.mu.Unlock()
	out := make([]*zduPC, 0, len(zduPCs.m))
	for _, z := range zduPCs.m {
		out = append(out, z)
	}
	return out
}

// zduPacketFiles dups every registered UDP socket for the hand-off.
func zduPacketFiles() (names []string, files []*os.File, err error) {
	for _, z := range zduAllPC() {
		fc, ok := z.PacketConn.(interface{ File() (*os.File, error) })
		if !ok {
			continue
		}
		f, e := fc.File()
		if e != nil {
			for _, g := range files {
				g.Close()
			}
			return nil, nil, fmt.Errorf("zdu: dup %s: %w", z.name, e)
		}
		names = append(names, z.name)
		files = append(files, f)
	}
	return names, files, nil
}
