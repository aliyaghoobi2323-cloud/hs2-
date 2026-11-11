package engine

import (
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// A link listener hands every accepted TCP conn to its front's AcceptFront
// in a goroutine of its own. That code parses bytes from anyone on the
// internet (a scanner, a DPI probe, a broken client): a ClientHello, a record,
// the auth. A panic there used to end the whole process — every user of the
// tunnel dropped by one bad conn. guardAccept recovers such a panic, closes
// that one conn and counts it; the log line (with the stack) is limited to
// one per panicLogEvery so a flood of them cannot fill the disk.
//
// Only the handshake is guarded: the carrier is handed to onTunnel after
// AcceptFront has returned, so a panic in the tunnel's own code (which may hold
// shared locks: recovering it could leave the process wedged) is not hidden.

var (
	panicsRecovered atomic.Int64
	panicLogAt      atomic.Int64 // monoNano of the last line
	panicLogEvery   = 10 * time.Second
)

// notePanic counts a recovered panic and logs it, rate limited.
func notePanic(logf func(string, ...any), where string, r any) {
	n := panicsRecovered.Add(1)
	if logf == nil {
		return
	}
	now := monoNano(time.Now())
	last := panicLogAt.Load()
	if last != 0 && now-last < int64(panicLogEvery) || !panicLogAt.CompareAndSwap(last, now) {
		return
	}
	st := debug.Stack()
	if len(st) > 4096 {
		st = st[:4096]
	}
	logf("BUG: recovered a panic in %s (%d recovered since start; this conn closed, the tunnel goes on): %v\n%s", where, n, r, st)
}

// guardAccept runs acc.AcceptFront on conn with a recover around the
// handshake, then calls onTunnel with the carrier, if any, outside it.
func guardAccept(ctx context.Context, acc tlscarrier.Acceptor, conn net.Conn, logf func(string, ...any), onTunnel func(*tlscarrier.Carrier)) {
	var car *tlscarrier.Carrier
	func() {
		defer func() {
			if r := recover(); r != nil {
				if car != nil {
					car.Close()
					car = nil
				}
				conn.Close()
				notePanic(logf, fmt.Sprintf("the link front's accept (from %s)", conn.RemoteAddr()), r)
			}
		}()
		acc.AcceptFront(ctx, conn, func(c *tlscarrier.Carrier) { car = c })
	}()
	if car != nil {
		onTunnel(car)
	}
}
