package engine

import (
	"errors"
	"math/bits"
	"net"
	"sync/atomic"
	"syscall"
)

// Observability counters: process-wide, lock-free, written on the data path
// and read only by the status writer. Nothing in the tunnel reads them to
// decide anything, so they cannot change its behaviour — they exist to measure
// what each later change does on a real server:
//
//   - the length shaper: records per write and the bytes it adds (padding and
//     its own header) on top of the data;
//   - the control ping's round trip, which travels behind each link's own
//     traffic and so is what that link's users wait;
//   - how long a user connection waits for its stream (pick + open + header);
//   - the bytes queued on UDP user flows, and the datagrams dropped there;
//   - the exit's dials to its target, and why they fail.
//
// Latencies and queue sizes go into an obsHist: log-spaced buckets, four per
// power of two, so a percentile read back is within ~12% of the true value.

const obsHistBuckets = 4 * 64

type obsHist struct{ b [obsHistBuckets]atomic.Uint64 }

func obsBucket(v uint64) int {
	if v < 4 {
		return int(v)
	}
	e := bits.Len64(v) - 1 // v in [2^e, 2^(e+1))
	return e*4 + int((v>>(e-2))&3)
}

// obsBucketMid is a representative value of bucket i (its midpoint).
func obsBucketMid(i int) uint64 {
	if i < 4 {
		return uint64(i)
	}
	e, sub := i/4, uint64(i%4)
	lo := uint64(1)<<e + sub<<(e-2)
	return lo + uint64(1)<<(e-2)/2
}

func (h *obsHist) observe(v uint64) { h.b[obsBucket(v)].Add(1) }

func (h *obsHist) snapshot() (s ObsHistSnap) {
	for i := range h.b {
		s[i] = h.b[i].Load()
	}
	return s
}

// ObsHistSnap is a copy of an obsHist's buckets at one moment.
type ObsHistSnap [obsHistBuckets]uint64

// Sub returns the observations made between old and s.
func (s ObsHistSnap) Sub(old ObsHistSnap) (d ObsHistSnap) {
	for i := range s {
		d[i] = s[i] - old[i]
	}
	return d
}

// Count is the number of observations.
func (s ObsHistSnap) Count() (n uint64) {
	for _, c := range s {
		n += c
	}
	return n
}

// Quantile returns the value at quantile q (0..1); ok is false when empty.
func (s ObsHistSnap) Quantile(q float64) (v uint64, ok bool) {
	n := s.Count()
	if n == 0 {
		return 0, false
	}
	rank := uint64(q*float64(n-1)) + 1
	var acc uint64
	for i, c := range s {
		if acc += c; acc >= rank {
			return obsBucketMid(i), true
		}
	}
	return obsBucketMid(obsHistBuckets - 1), true
}

var obs struct {
	shapeWrites, shapeRecords, shapeData, shapePad atomic.Uint64

	ctrlRTT obsHist // µs
	openLat obsHist // µs
	openErr atomic.Uint64

	udpQueued obsHist // bytes waiting on the flow when a datagram is queued
	udpDrops  atomic.Uint64

	exitDials, exitNoAddr, exitRefused, exitTimeout, exitOther atomic.Uint64
	exitRotated                                                atomic.Uint64 // dials that got through from another 127.0.0.x (exitdial.go)

	// HS2_L3_DUP (l3_dup.go): packets sent over two links, second copies
	// dropped by the receiver, and packets whose second copy came first.
	l3DupSent, l3DupDropped, l3DupSaved atomic.Uint64
}

// noteShapeWrite records one shapedConn.Write: its records, data and padding.
func noteShapeWrite(records, data, pad int) {
	obs.shapeWrites.Add(1)
	obs.shapeRecords.Add(uint64(records))
	obs.shapeData.Add(uint64(data))
	obs.shapePad.Add(uint64(pad))
}

// noteExitDial records one dial of the exit to its target and, if it failed,
// why: no local address/port left (EADDRNOTAVAIL — the ephemeral ports to that
// target are used up), refused, timed out, or anything else.
func noteExitDial(err error) {
	obs.exitDials.Add(1)
	if err == nil {
		return
	}
	var ne net.Error
	switch {
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		obs.exitNoAddr.Add(1)
	case errors.Is(err, syscall.ECONNREFUSED):
		obs.exitRefused.Add(1)
	case errors.As(err, &ne) && ne.Timeout():
		obs.exitTimeout.Add(1)
	default:
		obs.exitOther.Add(1)
	}
}

// ObsSnap is a copy of every counter at one moment. Counters only grow; a
// reader takes two snapshots and looks at the difference.
type ObsSnap struct {
	ShapeWrites, ShapeRecords, ShapeData, ShapePad uint64

	CtrlRTT, OpenLat, UDPQueued ObsHistSnap
	OpenErr, UDPDrops           uint64

	ExitDials, ExitNoAddr, ExitRefused, ExitTimeout, ExitOther uint64
	ExitRotated                                                uint64
	L3DupSent, L3DupDropped, L3DupSaved                        uint64
}

// Observed returns the counters now.
func Observed() ObsSnap {
	return ObsSnap{
		ShapeWrites: obs.shapeWrites.Load(), ShapeRecords: obs.shapeRecords.Load(),
		ShapeData: obs.shapeData.Load(), ShapePad: obs.shapePad.Load(),
		CtrlRTT: obs.ctrlRTT.snapshot(), OpenLat: obs.openLat.snapshot(), UDPQueued: obs.udpQueued.snapshot(),
		OpenErr: obs.openErr.Load(), UDPDrops: obs.udpDrops.Load(),
		ExitDials: obs.exitDials.Load(), ExitNoAddr: obs.exitNoAddr.Load(), ExitRefused: obs.exitRefused.Load(),
		ExitTimeout: obs.exitTimeout.Load(), ExitOther: obs.exitOther.Load(), ExitRotated: obs.exitRotated.Load(),
		L3DupSent: obs.l3DupSent.Load(), L3DupDropped: obs.l3DupDropped.Load(), L3DupSaved: obs.l3DupSaved.Load(),
	}
}

// Sub returns what happened between old and s.
func (s ObsSnap) Sub(old ObsSnap) ObsSnap {
	return ObsSnap{
		ShapeWrites: s.ShapeWrites - old.ShapeWrites, ShapeRecords: s.ShapeRecords - old.ShapeRecords,
		ShapeData: s.ShapeData - old.ShapeData, ShapePad: s.ShapePad - old.ShapePad,
		CtrlRTT: s.CtrlRTT.Sub(old.CtrlRTT), OpenLat: s.OpenLat.Sub(old.OpenLat), UDPQueued: s.UDPQueued.Sub(old.UDPQueued),
		OpenErr: s.OpenErr - old.OpenErr, UDPDrops: s.UDPDrops - old.UDPDrops,
		ExitDials: s.ExitDials - old.ExitDials, ExitNoAddr: s.ExitNoAddr - old.ExitNoAddr, ExitRefused: s.ExitRefused - old.ExitRefused,
		ExitTimeout: s.ExitTimeout - old.ExitTimeout, ExitOther: s.ExitOther - old.ExitOther, ExitRotated: s.ExitRotated - old.ExitRotated,
		L3DupSent: s.L3DupSent - old.L3DupSent, L3DupDropped: s.L3DupDropped - old.L3DupDropped, L3DupSaved: s.L3DupSaved - old.L3DupSaved,
	}
}
