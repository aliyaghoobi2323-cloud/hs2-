package engine

import (
	"net"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// censor351 (step-3 F2) hook glue: read-only copies into the detector
// (package censor). Every call site is guarded by censor.Active(), so with
// HS2_CENSOR_DETECT=0 a site costs one atomic load and allocates nothing.
// Nothing here changes an error, a return value, a timing or a lock.

// censorDeath records a link's first read/write error, with TCP_INFO taken
// before our close (watchConn.fail runs before onErr closes the socket).
// Our own closes (retire, drain, spare, ZDU peerOld) fail with net.ErrClosed
// and are classed local by censor.DeathOf.
func censorDeath(w *watchConn, err error) {
	d := censor.Death{Class: censor.DeathOf(err)}
	if w.raw != nil {
		d.Local, d.Remote = censor.AddrOf(w.raw.LocalAddr()), censor.AddrOf(w.raw.RemoteAddr())
	}
	if !w.born.IsZero() {
		d.LifeMs = uint32(time.Since(w.born) / time.Millisecond)
	}
	if tc := censorTCPOf(w.raw); tc != nil {
		censorTCP(tc, &d)
	}
	censor.NoteDeath(d)
}

// censorTCPOf unwraps net.Conn wrappers (TLS) down to the kernel socket, as
// tlscarrier.Carrier.TCPConn does; bounded.
func censorTCPOf(c net.Conn) *net.TCPConn {
	type netConner interface{ NetConn() net.Conn }
	for i := 0; i < 8 && c != nil; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			return tc
		}
		nc, ok := c.(netConner)
		if !ok {
			return nil
		}
		c = nc.NetConn()
	}
	return nil
}

// censorLBSample copies one path reading (under the LB lock measure already
// holds, only when the detector is on); censorLBFlush sends them after the
// lock is released.
func censorLBSample(buf []censor.PathSample, local, remote string, r q2Reading, rateBps float64, cs *censorAcc, j int) []censor.PathSample {
	p := censor.PathSample{Local: censor.ParseAddr(local), Remote: censor.ParseAddr(remote),
		Loss: r.loss, QueueMs: r.queueMs, Silent: r.silent, PeerLoss: -1, RateBps: rateBps, PathLimited: -1}
	if cs != nil && j < len(cs.p) {
		a := &cs.p[j]
		if a.segsIn >= censorMinSegs {
			p.PeerLoss = float64(a.peerRt) / float64(a.segsIn)
		}
		p.BusyLinks = a.busy
		if a.chronoN > 0 {
			p.PathLimited = a.limited / float64(a.chronoN)
		}
	}
	return append(buf, p)
}

// censorMinSegs: received segments a path needs this reading before its
// PeerLoss counts (as lbMinSegs for our own loss).
const censorMinSegs = 50

// censorPrev is the per-link counter snapshot the detector's M7/O7 inputs
// diff (kept inside qPrev; filled only when the detector is on).
type censorPrev struct {
	peerRt, segsIn       uint64
	busyUs, rwndUs, sbUs uint64
	rcvd                 uint64
	peerOK, chrono       bool
}

func censorPrevFill(c *qPrev, st tcpStat, m *linkMeter) {
	c.cen = censorPrev{segsIn: uint64(st.segsIn), busyUs: st.busyUs, rwndUs: st.rwndUs, sbUs: st.sndbufUs,
		rcvd: st.bytesRcvd, chrono: st.chronoValid}
	if m != nil && m.peerSeen.Load() {
		c.cen.peerOK, c.cen.peerRt = true, m.peerRetrans.Load()
	}
}

// censorAcc sums one reading's M7/O7 inputs per path (only when on).
type censorAcc struct{ p []censorPath }

type censorPath struct {
	peerRt, segsIn uint64  // O7: the peer's retransmits vs the segments we received
	busy           uint16  // M7: links that moved >= qBusyBytes either way
	limited        float64 // M7: sum over busy links of the network-limited share
	chronoN        uint16  // busy links with chrono counters
}

func newCensorAcc(n int) *censorAcc { return &censorAcc{p: make([]censorPath, n)} }

func (a *censorAcc) add(j int, cur, prev *qPrev, dt time.Duration) {
	if j < 0 || j >= len(a.p) {
		return
	}
	c, o := &cur.cen, &prev.cen
	p := &a.p[j]
	if c.peerOK && o.peerOK && c.peerRt >= o.peerRt && c.segsIn >= o.segsIn {
		p.peerRt += c.peerRt - o.peerRt
		p.segsIn += c.segsIn - o.segsIn
	}
	if c.rcvd < o.rcvd {
		return
	}
	if cur.acked-prev.acked < qBusyBytes && c.rcvd-o.rcvd < qBusyBytes {
		return
	}
	if p.busy < 1<<15 {
		p.busy++
	}
	if c.chrono && o.chrono && c.busyUs >= o.busyUs && dt > 0 {
		b := float64(c.busyUs - o.busyUs)
		var lim float64
		if c.rwndUs >= o.rwndUs {
			lim += float64(c.rwndUs - o.rwndUs)
		}
		if c.sbUs >= o.sbUs {
			lim += float64(c.sbUs - o.sbUs)
		}
		share := (b - lim) / float64(dt/time.Microsecond)
		if share < 0 {
			share = 0
		} else if share > 1 {
			share = 1
		}
		p.limited += share
		p.chronoN++
	}
}

func censorLBFlush(buf []censor.PathSample) {
	for i := range buf {
		censor.NotePath(buf[i])
	}
}
