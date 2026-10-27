package engine

import (
	"errors"
	"fmt"
	"log"
	"math"
	"math/bits"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// C3, the host-queue guard (REDESIGN §3.3): the link socket's unsent queue
// is bounded in time, not bytes (P3), and the writer sleeps in Go's
// netpoller, not in a poll(2) loop. It takes sendGate's place (sendgate.go).
//
//   - Each hostqTick the low-water mark (TCP_NOTSENT_LOWAT) is 2 ms of the
//     link's rate, at least 4 KiB and two segments, at most 256 KiB
//     (Lowat), written only when it moved by hostqMove or more. The writer
//     writes as it does today and keeps no more than one frame past the
//     mark: it takes its next frame from C2 only once its write returned.
//   - Linux checks the mark only when a write starts a new buffer (it
//     appends to the unsent tail up to the size goal unchecked), so the
//     mark bounds the queue to about twice itself: fine at high rates, 65 ms
//     at 1 Mbit/s with the 4 KiB floor. So below hostqGateOn (on) and until
//     past hostqGateOff (off), the gate: before each write the socket's
//     unsent bytes are read (SIOCOUTQNSD, one cheap ioctl) and a write
//     goes only while the room under cap = max(2 ms of the rate, two
//     segments) takes it, cap never more than half of it for a large
//     frame; the mark is then max(cap/2, MSS), so the netpoller wakes the
//     writer with room for half a cap at least (when the socket polls
//     writable, under half the mark unsent on Linux 5.10 on, under the mark
//     before). A rate that swings around 25 Mbit/s does not flip it.
//   - A kernel without TCP_NOTSENT_LOWAT or SIOCOUTQNSD keeps today's
//     behaviour (the fixed mark of tlscarrier.NotSentLowat and sendGate),
//     with one log line for the process.
//
// The rate comes from C4-lite's reading of the same socket each hostqTick
// (sendCapLink.guard, sendcap.go): the least of the largest delivery rate
// of the last 2 s and the latest sample that was not app-limited, so a
// drop shows at its first sample, where the pacing cap keeps the largest
// of 2 s (decision T2; with the 2 s max alone, a tenfold drop held the old
// mark and cap for 2 s, past the 20 ms bound); the link meter's written
// rate until the first sample not app-limited (R3: C4's). Before the first
// update the gate is on at its floor (an unknown rate is low; the first
// reading corrects it).

// TuneHQG 0 (cmd/hs2 applyTuning: HS2_TUNE_HQG=0) keeps the fixed
// low-water mark and sendGate instead of C3.
var TuneHQG = 1

// HostQOff says whether C3 is off (HS2_TUNE_HQG=0).
func HostQOff() bool { return TuneHQG == 0 }

// TuneHQGGate 0 (cmd/hs2 applyTuning: HS2_TUNE_HQG_GATE=0; a lab knob of
// angle (b), docs/architecture/wip/r1/angle-b.fa.md) keeps C3's mark and
// drops its write gate: with the gate, below 20-30 Mbit/s a link socket is
// woken with less than a segment unsent, so Linux marks its every delivery
// sample app-limited, its BBR never leaves STARTUP (pacing at 2.89 times the
// path, the round trip at three times the floor) and C4-lite, which waits
// for a sample not app-limited, never caps it (rig, 8 links of 25 Mbit/s:
// 33% of the segments resent with the gate, 3.7% without C3).
var TuneHQGGate = 1

// TuneHQGSegs > 0 (HS2_TUNE_HQG_SEGS; lab, angle (b)) keeps the gate with
// a cap of at least that many segments and the mark at the cap, so the
// writer is woken while a segment or more is still unsent (the gate's
// floor is two segments and its mark half the cap, §3.3).
var TuneHQGSegs = 0

const (
	// hostqTick: the mark and the gate follow the rate this often.
	hostqTick = 500 * time.Millisecond
	// hostqTime: the unsent queue's bound, in time of the link's rate.
	hostqTime = 2 * time.Millisecond
	// hostqLowatMin, hostqLowatMax: the mark's floor (and two segments),
	// and its ceiling.
	hostqLowatMin = 4 << 10
	hostqLowatMax = 256 << 10
	// hostqGateOn, hostqGateOff: the gate comes on below the one and goes
	// off above the other, bytes/s (20 and 30 Mbit/s).
	hostqGateOn  = 20e6 / 8
	hostqGateOff = 30e6 / 8
	// hostqMove: the mark is written only when it moved by this share.
	hostqMove = 0.25
	// hostqMSS: the segment when the socket's is not known.
	hostqMSS = 1448
)

// Lowat is the low-water mark for a link of rate bytes/s and segment mss:
// clamp(2 ms of the rate, max(2·mss, 4 KiB), 256 KiB).
func Lowat(rate float64, mss int) int {
	if mss <= 0 {
		mss = hostqMSS
	}
	lo := float64(max(2*mss, hostqLowatMin))
	v := rate / float64(time.Second/hostqTime)
	if !(v > lo) {
		v = lo
	}
	return int(math.Min(v, hostqLowatMax))
}

// hostGate is the gate's state (pure: the rate in, the decision out).
type hostGate struct {
	off   bool // past hostqGateOff, not yet back under hostqGateOn
	limit int  // the latest cap
}

// Update takes the link's rate (bytes/s; 0: unknown) and segment, and says
// whether the gate is on, its cap (the most the socket may hold unsent)
// and the socket's low-water mark.
func (g *hostGate) Update(rate float64, mss int) (on bool, cap, lowat int) {
	if mss <= 0 {
		mss = hostqMSS
	}
	switch {
	case !g.off && rate > hostqGateOff:
		g.off = true
	case g.off && rate < hostqGateOn:
		g.off = false
	}
	cap = max(int(rate/float64(time.Second/hostqTime)), 2*mss)
	g.limit = cap
	if g.off {
		return false, cap, Lowat(rate, mss)
	}
	return true, cap, max(cap/2, mss)
}

// Room is how much may be written with notsent bytes unsent: cap − notsent,
// at least 0.
func (g *hostGate) Room(notsent int) int { return max(g.limit-notsent, 0) }

// hostSys is what the guard asks of the link socket (hostq_linux.go; a
// synthetic socket in the tests).
type hostSys interface {
	setLowat(n int) error  // TCP_NOTSENT_LOWAT
	notsent() (int, error) // SIOCOUTQNSD
	// waitBelow sleeps in the netpoller until less than n bytes are unsent
	// or the socket polls writable, at once if so already.
	waitBelow(n int) error
}

// coreLog, when set, takes R1's cores' lines (a test); else the tunnel's
// log (the window core's, setWcoreLog), else the standard one.
var coreLog atomic.Pointer[func(string, ...any)]

func coreLogf(format string, a ...any) {
	if f := coreLog.Load(); f != nil {
		(*f)(format, a...)
	} else if f := wcores.logf.Load(); f != nil {
		(*f)(format, a...)
	} else {
		log.Printf(format, a...)
	}
}

// hostqNoSys: the line for a kernel without the guard's calls is written.
var hostqNoSys atomic.Bool

// hostqHist: the notsent histogram's buckets (by bit length).
const hostqHist = 24

// hostQ is one link's guard: Write is the link writer's, Update the
// runner's (each hostqTick).
type hostQ struct {
	conn  net.Conn
	sys   hostSys
	gate  hostGate     // Update's
	lowat atomic.Int64 // the mark written (0: none yet)
	on    atomic.Bool
	cap   atomic.Int64
	// fb: the kernel lacks a call of the guard: today's send gate
	fb atomic.Pointer[sendGate]
	// the status line's: notsent at each gated write, and the time spent
	// waiting at the gate
	hist    [hostqHist]atomic.Uint32
	blocked atomic.Int64 // ns
	// waiting: a write waits at the gate (hostQConn.Close)
	waiting atomic.Bool
}

// newHostQ guards c, the link's conn, through sys (nil: none, as off Linux:
// today's send gate). The gate is on at its floor until the first Update.
func newHostQ(c net.Conn, sys hostSys) *hostQ {
	h := &hostQ{conn: c, sys: sys}
	if sys == nil {
		h.fallback(nil)
		return h
	}
	h.Update(0, 0)
	return h
}

// Update moves the mark and the gate to the link's rate (bytes/s; 0:
// unknown) and segment (0: unknown).
func (h *hostQ) Update(rate float64, mss int) {
	if h.fb.Load() != nil {
		return
	}
	on, cap, lowat := h.gate.Update(rate, mss)
	switch {
	case TuneHQGGate == 0:
		on, lowat = false, Lowat(rate, mss)
	case on && TuneHQGSegs > 0:
		// lab: the gate's cap at least that many segments, and the mark at
		// the cap (the socket polls writable under half the mark): the
		// writer is woken with a segment or more still unsent
		seg := mss
		if seg <= 0 {
			seg = hostqMSS
		}
		cap = max(cap, TuneHQGSegs*seg)
		lowat = cap
	}
	if w := int(h.lowat.Load()); w == 0 || math.Abs(float64(lowat-w)) >= hostqMove*float64(w) {
		if err := h.sys.setLowat(lowat); err != nil {
			if hostqUnsupported(err) {
				h.fallback(err)
			}
			return // (any other error: the socket is gone, its writer finds out)
		}
		h.lowat.Store(int64(lowat))
	}
	h.cap.Store(int64(cap))
	h.on.Store(on)
}

// Write writes p, a frame, whole: with the gate on, once the socket has
// room for it (for at most half the cap of a larger one).
func (h *hostQ) Write(p []byte) (int, error) {
	if fb := h.fb.Load(); fb != nil {
		return fb.Write(p)
	}
	if h.on.Load() {
		c := int(h.cap.Load())
		need := min(len(p), c/2)
		ns, err := h.sys.notsent()
		if err == nil {
			h.hist[min(bits.Len(uint(ns)), hostqHist-1)].Add(1)
			if c-ns < need {
				t0 := ctrlNow()
				h.waiting.Store(true)
				err = h.sys.waitBelow(c - need + 1)
				h.waiting.Store(false)
				h.blocked.Add(ctrlNow() - t0)
			}
		}
		if err != nil && hostqUnsupported(err) {
			h.fallback(err)
			return h.fb.Load().Write(p)
		}
		// (any other error: the socket is gone, and the write says so)
	}
	return h.conn.Write(p)
}

// hostQConn is the link's conn with C3's guard on its writes: what the
// shaper writes its records to, in the send gate's place (newSessionCaps).
type hostQConn struct {
	net.Conn // the link's TLS conn
	hq       *hostQ
	tcp      *net.TCPConn // its socket (nil: none)
}

func (c *hostQConn) Write(p []byte) (int, error) { return c.hq.Write(p) }

// Close closes the link's conn. A write waiting at the gate (in the
// netpoller, on the socket's write lock) would hold the TLS conn's
// close_notify behind it until the socket drains: as sendGate.Close does,
// the socket is closed under it first.
func (c *hostQConn) Close() error {
	if fb := c.hq.fb.Load(); fb != nil {
		return fb.Close()
	}
	if c.hq.waiting.Load() && c.tcp != nil {
		c.tcp.Close()
	}
	return c.Conn.Close()
}

// hostqUnsupported says whether err is a kernel's "no such option" or "no
// such ioctl" (a kernel without the guard's calls), not a socket's own
// failure (closed: the writer's error to report).
func hostqUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOPROTOOPT) || errors.Is(err, syscall.ENOTTY) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EINVAL)
}

// took feeds the fallback send gate its rate (meteredConn's tally).
func (h *hostQ) took(n int, d time.Duration) {
	if fb := h.fb.Load(); fb != nil {
		fb.took(n, d)
	}
}

// fallback turns the guard into today's send gate over the fixed mark
// (err: the call the kernel refused; nil: no socket to guard).
func (h *hostQ) fallback(err error) {
	if !h.fb.CompareAndSwap(nil, newSendGate(h.conn)) {
		return
	}
	h.on.Store(false)
	if err == nil {
		return
	}
	if h.lowat.Load() != 0 && tlscarrier.NotSentLowat > 0 {
		h.sys.setLowat(tlscarrier.NotSentLowat) // (best effort: the mark as before)
	}
	if hostqNoSys.CompareAndSwap(false, true) {
		coreLogf("hostq: the kernel lacks TCP_NOTSENT_LOWAT or SIOCOUTQNSD (%v): link sockets keep the fixed low-water mark (%d B) and the old send gate", err, tlscarrier.NotSentLowat)
	}
}

// line is the guard's status line (§3.3) over the last elapsed, and starts
// the next one's tallies.
func (h *hostQ) line(n int, elapsed time.Duration) string {
	gate := "off"
	if h.on.Load() {
		gate = "on"
	}
	var cnt [hostqHist]uint32
	tot := uint32(0)
	for i := range h.hist {
		cnt[i] = h.hist[i].Swap(0)
		tot += cnt[i]
	}
	p95, acc := 0, uint32(0)
	for i, c := range cnt {
		acc += c
		if tot > 0 && acc*100 >= tot*95 {
			p95 = (1 << i) - 1 // the bucket's top
			break
		}
	}
	ratio := 0.0
	if elapsed > 0 {
		ratio = float64(h.blocked.Swap(0)) / float64(elapsed)
	}
	return fmt.Sprintf("hostq link %d: lowat %.1f KiB, gate %s (cap %d B), notsent p95 %d B, blocked ratio %.2f",
		n, float64(h.lowat.Load())/1024, gate, h.cap.Load(), p95, ratio)
}
