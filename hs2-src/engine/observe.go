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

// ctrlLinkBusy reports a link that carries a download now: a user stream at a
// download's rate (heavyN, as the sampler last stored it) or a stripe sub
// (stripeN) — the signals the TUN lane (HS2_L3_LANE) keeps off.
func ctrlLinkBusy(m *linkMeter) bool {
	return m != nil && (m.heavyN.Load() > 0 || m.stripeN.Load() > 0)
}

// observeCtrlRTT records one control round trip (µs): in the whole histogram
// and in the light or busy one. A measurement only; nothing reads it back.
func observeCtrlRTT(us uint64, busy bool) {
	obs.ctrlRTT.observe(us)
	if busy {
		obs.ctrlRTTBusy.observe(us)
	} else {
		obs.ctrlRTTLight.observe(us)
	}
}

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
	// The same round trips split by the link they ran on (observeCtrlRTT):
	// light — no download-rate user stream and no stripe sub on it; busy —
	// one of those, so the pong waits behind the download's data.
	ctrlRTTLight, ctrlRTTBusy obsHist // µs
	openLat                   obsHist // µs
	openErr                   atomic.Uint64

	udpQueued obsHist // bytes waiting on the flow when a datagram is queued
	udpDrops  atomic.Uint64

	exitDials, exitNoAddr, exitRefused, exitTimeout, exitOther atomic.Uint64
	exitRotated                                                atomic.Uint64 // dials that got through from another 127.0.0.x (exitdial.go)

	// HS2_L3_DUP (l3_dup.go): packets sent over two links, second copies
	// dropped by the receiver, and packets whose second copy came first.
	l3DupSent, l3DupDropped, l3DupSaved atomic.Uint64

	// HS2_DUP_PATHS (dup_paths.go): second copies (TUN packets; mconn light
	// subs) placed on another path than the first copy, or on the same one.
	l3DupPathOther, l3DupPathSame, mconnPathOther, mconnPathSame atomic.Uint64

	// HS2_MCONN (mconn.go): connections open now; payload bytes sent on a
	// second light sub; chunks whose copy on a non-first light sub arrived
	// first; switches to a bulk sub; light sub deaths the connection
	// survived; copies a full sub queue missed; connections that failed.
	// Gauges by sending state: two copies, one sub, heavy; the moves to
	// heavy by rule (a standing queue, the rate, or a fast start); and the
	// primary switches of a light connection whose primary sub was stuck.
	mconnOpen                                  atomic.Int64
	mconnDupBytes, mconnSaved, mconnBulk       atomic.Uint64
	mconnSubSurvived, mconnMissed, mconnFailed atomic.Uint64
	mconnDual, mconnSingle, mconnHeavy         atomic.Int64
	mconnHeavyQueue, mconnHeavyRate            atomic.Uint64
	mconnHeavyFast, mconnPrimarySwitch         atomic.Uint64
	// HS2_MCONN=lite: lite connections open, those on two subs now, the
	// upgrades to two subs, and the upgrades refused by the duplication cap.
	mconnLiteOpen, mconnLiteDual      atomic.Int64
	mconnLiteUpgrade, mconnLiteCapped atomic.Uint64

	// Link health: loss candidates kept by HS2_LOSS_RELATIVE (within
	// lossMargin of the judged links' median), and degraded links refused a
	// second probation by HS2_PROBATION_ONCE.
	lossRelKept, probationOnceDenied atomic.Uint64

	// Wedge guard (wedge.go, wedge_watch.go): releases of a link's stalled
	// relays, those after which nothing arrived from the other side for
	// releaseStallMin or more, and the longest such wait (ms).
	wedgeReleases, postReleaseStall atomic.Uint64
	postReleaseStallMaxMs           atomic.Int64
}

// noteShapeWrite records one shapedConn.Write: its records, data and padding.
func noteShapeWrite(records, data, pad int) {
	obs.shapeWrites.Add(1)
	obs.shapeRecords.Add(uint64(records))
	obs.shapeData.Add(uint64(data))
	obs.shapePad.Add(uint64(pad))
}

// noteProbationOnceDenied counts a degraded link refused a second probation
// (HS2_PROBATION_ONCE; sampleHealth, where obs is a local name).
func noteProbationOnceDenied() { obs.probationOnceDenied.Add(1) }

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
	CtrlRTTLight, CtrlRTTBusy   ObsHistSnap // CtrlRTT split (observeCtrlRTT)
	OpenErr, UDPDrops           uint64

	ExitDials, ExitNoAddr, ExitRefused, ExitTimeout, ExitOther   uint64
	ExitRotated                                                  uint64
	L3DupSent, L3DupDropped, L3DupSaved                          uint64
	L3DupPathOther, L3DupPathSame, MConnPathOther, MConnPathSame uint64

	MConnOpen                                  int64 // a level, not a count (Sub keeps the newer)
	MConnDupBytes, MConnSaved, MConnBulk       uint64
	MConnSubSurvived, MConnMissed, MConnFailed uint64
	MConnDual, MConnSingle, MConnHeavy         int64 // levels
	MConnHeavyQueue, MConnHeavyRate            uint64
	MConnHeavyFast, MConnPrimarySwitch         uint64
	MConnLiteOpen, MConnLiteDual               int64 // levels
	MConnLiteUpgrade, MConnLiteCapped          uint64
	LossRelKept, ProbationOnceDenied           uint64
	WedgeReleases, PostReleaseStall            uint64
	PostReleaseStallMaxMs                      int64 // a maximum, not a count (Sub keeps the newer)
}

// Observed returns the counters now.
func Observed() ObsSnap {
	return ObsSnap{
		ShapeWrites: obs.shapeWrites.Load(), ShapeRecords: obs.shapeRecords.Load(),
		ShapeData: obs.shapeData.Load(), ShapePad: obs.shapePad.Load(),
		CtrlRTTLight: obs.ctrlRTTLight.snapshot(), CtrlRTTBusy: obs.ctrlRTTBusy.snapshot(),
		CtrlRTT: obs.ctrlRTT.snapshot(), OpenLat: obs.openLat.snapshot(), UDPQueued: obs.udpQueued.snapshot(),
		OpenErr: obs.openErr.Load(), UDPDrops: obs.udpDrops.Load(),
		ExitDials: obs.exitDials.Load(), ExitNoAddr: obs.exitNoAddr.Load(), ExitRefused: obs.exitRefused.Load(),
		ExitTimeout: obs.exitTimeout.Load(), ExitOther: obs.exitOther.Load(), ExitRotated: obs.exitRotated.Load(),
		L3DupSent: obs.l3DupSent.Load(), L3DupDropped: obs.l3DupDropped.Load(), L3DupSaved: obs.l3DupSaved.Load(),
		L3DupPathOther: obs.l3DupPathOther.Load(), L3DupPathSame: obs.l3DupPathSame.Load(),
		MConnPathOther: obs.mconnPathOther.Load(), MConnPathSame: obs.mconnPathSame.Load(),
		MConnOpen: obs.mconnOpen.Load(), MConnDupBytes: obs.mconnDupBytes.Load(), MConnSaved: obs.mconnSaved.Load(),
		MConnBulk: obs.mconnBulk.Load(), MConnSubSurvived: obs.mconnSubSurvived.Load(),
		MConnMissed: obs.mconnMissed.Load(), MConnFailed: obs.mconnFailed.Load(),
		MConnDual: obs.mconnDual.Load(), MConnSingle: obs.mconnSingle.Load(), MConnHeavy: obs.mconnHeavy.Load(),
		MConnHeavyQueue: obs.mconnHeavyQueue.Load(), MConnHeavyRate: obs.mconnHeavyRate.Load(),
		MConnHeavyFast: obs.mconnHeavyFast.Load(), MConnPrimarySwitch: obs.mconnPrimarySwitch.Load(),
		MConnLiteOpen: obs.mconnLiteOpen.Load(), MConnLiteDual: obs.mconnLiteDual.Load(),
		MConnLiteUpgrade: obs.mconnLiteUpgrade.Load(), MConnLiteCapped: obs.mconnLiteCapped.Load(),
		LossRelKept: obs.lossRelKept.Load(), ProbationOnceDenied: obs.probationOnceDenied.Load(),
		WedgeReleases: obs.wedgeReleases.Load(), PostReleaseStall: obs.postReleaseStall.Load(),
		PostReleaseStallMaxMs: obs.postReleaseStallMaxMs.Load(),
	}
}

// Sub returns what happened between old and s.
func (s ObsSnap) Sub(old ObsSnap) ObsSnap {
	return ObsSnap{
		ShapeWrites: s.ShapeWrites - old.ShapeWrites, ShapeRecords: s.ShapeRecords - old.ShapeRecords,
		ShapeData: s.ShapeData - old.ShapeData, ShapePad: s.ShapePad - old.ShapePad,
		CtrlRTTLight: s.CtrlRTTLight.Sub(old.CtrlRTTLight), CtrlRTTBusy: s.CtrlRTTBusy.Sub(old.CtrlRTTBusy),
		CtrlRTT: s.CtrlRTT.Sub(old.CtrlRTT), OpenLat: s.OpenLat.Sub(old.OpenLat), UDPQueued: s.UDPQueued.Sub(old.UDPQueued),
		OpenErr: s.OpenErr - old.OpenErr, UDPDrops: s.UDPDrops - old.UDPDrops,
		ExitDials: s.ExitDials - old.ExitDials, ExitNoAddr: s.ExitNoAddr - old.ExitNoAddr, ExitRefused: s.ExitRefused - old.ExitRefused,
		ExitTimeout: s.ExitTimeout - old.ExitTimeout, ExitOther: s.ExitOther - old.ExitOther, ExitRotated: s.ExitRotated - old.ExitRotated,
		L3DupSent: s.L3DupSent - old.L3DupSent, L3DupDropped: s.L3DupDropped - old.L3DupDropped, L3DupSaved: s.L3DupSaved - old.L3DupSaved,
		L3DupPathOther: s.L3DupPathOther - old.L3DupPathOther, L3DupPathSame: s.L3DupPathSame - old.L3DupPathSame,
		MConnPathOther: s.MConnPathOther - old.MConnPathOther, MConnPathSame: s.MConnPathSame - old.MConnPathSame,
		MConnOpen: s.MConnOpen, MConnDupBytes: s.MConnDupBytes - old.MConnDupBytes, MConnSaved: s.MConnSaved - old.MConnSaved,
		MConnBulk: s.MConnBulk - old.MConnBulk, MConnSubSurvived: s.MConnSubSurvived - old.MConnSubSurvived,
		MConnMissed: s.MConnMissed - old.MConnMissed, MConnFailed: s.MConnFailed - old.MConnFailed,
		MConnDual: s.MConnDual, MConnSingle: s.MConnSingle, MConnHeavy: s.MConnHeavy,
		MConnHeavyQueue: s.MConnHeavyQueue - old.MConnHeavyQueue, MConnHeavyRate: s.MConnHeavyRate - old.MConnHeavyRate,
		MConnHeavyFast: s.MConnHeavyFast - old.MConnHeavyFast, MConnPrimarySwitch: s.MConnPrimarySwitch - old.MConnPrimarySwitch,
		MConnLiteOpen: s.MConnLiteOpen, MConnLiteDual: s.MConnLiteDual,
		MConnLiteUpgrade: s.MConnLiteUpgrade - old.MConnLiteUpgrade, MConnLiteCapped: s.MConnLiteCapped - old.MConnLiteCapped,
		LossRelKept: s.LossRelKept - old.LossRelKept, ProbationOnceDenied: s.ProbationOnceDenied - old.ProbationOnceDenied,
		WedgeReleases: s.WedgeReleases - old.WedgeReleases, PostReleaseStall: s.PostReleaseStall - old.PostReleaseStall,
		PostReleaseStallMaxMs: s.PostReleaseStallMaxMs,
	}
}
