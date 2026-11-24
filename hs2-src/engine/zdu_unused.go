package engine

import (
	"errors"
	"net"
	"strings"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/freebind"
)

// b428: moving a user port to another unit through a ZDU handover.
//
// The new process tells the old one (handoff UNUSED, right before READY)
// which handed-off user sockets it did not adopt because its config no longer
// binds them. At GO the old process closes its copies of exactly those
// (ZDUCloseUnused): dials to the port are no longer queued in a paused
// backlog nobody accepts, and another unit waiting on EADDRINUSE
// (listen_retry.go) binds the port at once. Connections already accepted on
// them keep draining. A revert in probation rebinds them (ZDURebindUnused); a
// bind that fails (another unit took the port) is reported to the caller,
// which logs it and keeps serving everything else.
//
// Only user sockets are closed: a link listener name is ignored (the commit
// drops those, ZDUDropLinkListeners).

// ZDUCloseUnused closes this process's copies of the named handed-off user
// sockets ("tcp:ADDR", "udp:ADDR"). It returns the names it closed.
func ZDUCloseUnused(names []string) (closed []string) {
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "tcp:"):
			zduLns.mu.Lock()
			z := zduLns.m[n]
			zduLns.mu.Unlock()
			if z == nil || z.link {
				continue
			}
			z.drop()
			closed = append(closed, n)
		case strings.HasPrefix(n, "udp:"):
			zduPCs.mu.Lock()
			z := zduPCs.m[n]
			zduPCs.mu.Unlock()
			if z != nil && z.dropUDP() {
				closed = append(closed, n)
			}
		}
	}
	return closed
}

// ZDURebindUnused binds again the sockets ZDUCloseUnused closed (the revert).
// failed maps a name to its bind error.
func ZDURebindUnused(names []string) (ok []string, failed map[string]error) {
	for _, n := range names {
		var err error
		switch {
		case strings.HasPrefix(n, "tcp:"):
			zduLns.mu.Lock()
			z := zduLns.m[n]
			zduLns.mu.Unlock()
			if z == nil {
				continue
			}
			err = z.rebind()
		case strings.HasPrefix(n, "udp:"):
			zduPCs.mu.Lock()
			z := zduPCs.m[n]
			zduPCs.mu.Unlock()
			if z == nil {
				continue
			}
			err = z.rebind()
		default:
			continue
		}
		if err != nil {
			if failed == nil {
				failed = map[string]error{}
			}
			failed[n] = err
			continue
		}
		ok = append(ok, n)
	}
	return ok, failed
}

var errNotTCP = errors.New("zdu: rebind: not a TCP listener")

// rebind binds the dropped listener's address again (cold) and makes it this
// wrapper's socket; Accept continues on it.
func (z *zduLn) rebind() error {
	z.mu.Lock()
	dr, cl := z.dropped, z.closed
	z.mu.Unlock()
	if !dr || cl {
		return nil
	}
	ln, err := listenReuseRcvBufCold(strings.TrimPrefix(z.name, "tcp:"), z.rcvbuf)
	if err != nil {
		return err
	}
	tl, isTCP := ln.(*net.TCPListener)
	if !isTCP {
		ln.Close()
		return errNotTCP
	}
	z.mu.Lock()
	if z.closed || !z.dropped {
		z.mu.Unlock()
		tl.Close()
		return nil
	}
	z.TCPListener = tl
	z.dropped = false
	paused := z.paused
	z.cond.Broadcast()
	z.mu.Unlock()
	if paused {
		z.setPaused(true) // the kick deadline on the new socket too
	}
	return nil
}

// dropUDP closes this process's copy of the UDP socket (b428, at GO). The
// replies of this process's own UDP flows on that port stop with it (a UDP
// flow has no handover; the client's next datagram reaches the new owner).
func (z *zduPC) dropUDP() bool {
	z.mu.Lock()
	if z.dropped || z.closed {
		z.mu.Unlock()
		return false
	}
	z.dropped = true
	z.cond.Broadcast()
	pc := z.PacketConn
	z.mu.Unlock()
	pc.Close()
	return true
}

func (z *zduPC) rebind() error {
	z.mu.Lock()
	dr, cl := z.dropped, z.closed
	z.mu.Unlock()
	if !dr || cl {
		return nil
	}
	pc, err := freebind.ListenPacket("udp", strings.TrimPrefix(z.name, "udp:")) // rc9: same bind as a cold start (b425)
	if err != nil {
		return err
	}
	z.mu.Lock()
	if z.closed || !z.dropped {
		z.mu.Unlock()
		pc.Close()
		return nil
	}
	z.PacketConn = pc
	z.dropped = false
	paused := z.paused
	z.cond.Broadcast()
	z.mu.Unlock()
	if paused {
		z.setPaused(true)
	}
	return nil
}
