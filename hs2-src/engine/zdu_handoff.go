package engine

import (
	"fmt"
	"net"
	"os"
)

// ZDU (proposals/zdu350.md) slot S5: what the engine needs from the hand-off
// channel (package handoff). The channel itself, its auth and the fd passing
// live in handoff; cmd/hs2 opens it only with HS2_ZDU=1.

// ZDUEnabled reports HS2_ZDU=1 (read once at start).
func ZDUEnabled() bool { return zduOn.Load() }

// ZDUInstance is this process's instance id (start ns, rand), the metadata a
// hand-off HELLO carries.
func ZDUInstance() (start uint64, rnd uint32) { return zduSelf.Start, zduSelf.Rand }

// ListenFromFile wraps a TCP listener fd received from the old process. The
// socket (and its accept queue) is shared: connections queued before or
// during the hand-off are accepted here. The caller keeps f (close it after,
// or keep it as the dup a revert needs).
func ListenFromFile(f *os.File) (net.Listener, error) {
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("zdu: listener %s: %w", f.Name(), err)
	}
	return l, nil
}

// PacketConnFromFile wraps a UDP socket fd received from the old process.
func PacketConnFromFile(f *os.File) (net.PacketConn, error) {
	c, err := net.FilePacketConn(f)
	if err != nil {
		return nil, fmt.Errorf("zdu: packet conn %s: %w", f.Name(), err)
	}
	return c, nil
}

// ZDUDrainSink is the DRAINSTAT consumer for an Iran LinkManager: the old
// process's live link count becomes the foreign-drain term of ctlTarget
// (0 = it is gone). handoff.Conn.FollowDrain calls it.
func (m *LinkManager) ZDUDrainSink() func(links int) {
	return m.SetZDUForeignDrain
}

// ZDUFollowParent (S7) makes this process's instance newer than the parent
// it took over from: Start = max(own start, parent+1). The instance orders
// the two generations on both sides (S3/S4); the wall clock alone would
// invert them after an NTP step back between the two starts. Called by the
// new process right after HELLO, before any link announces the instance.
// Returns the start in use.
func ZDUFollowParent(parent uint64) uint64 {
	zduSelf = zduAfter(zduSelf, parent)
	return zduSelf.Start
}

func zduAfter(self zduInst, parent uint64) zduInst {
	if parent != 0 && self.Start <= parent {
		self.Start = parent + 1
	}
	return self
}
