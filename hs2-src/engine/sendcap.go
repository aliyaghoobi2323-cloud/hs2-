package engine

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// C4-lite, the pacing cap on every link socket at both ends (REDESIGN
// §3.4, R1): SO_MAX_PACING_RATE = 1.25 × the largest delivery rate of the
// last 2 s, never under 1 MiB/s, from the socket's first delivery-rate
// sample that is not app-limited, lifted when it goes idle. No in-flight
// budget, no probing, no drain (R3's C4 brings those).
//
// C2 and C3 bound only the host's queues; a byte that reached the link's
// TCP waits at the bottleneck behind all that TCP has in flight. A BBRv1
// sender held by windows never leaves STARTUP and paces at ~3.9 times the
// path (D3), filling the bottleneck's queue to its window: the probe on
// the same link waits there whatever C2 does. Capped at 1.25 times what the
// path delivered, that overshoot is cut at the source; it is the pacing
// net's cap (pacenet.go) made unconditional (not only past 2% resent) and
// symmetric (the edge too, for uploads).
//
// One writer for the cap: on the exit the window core and the pacing net
// stay on in R1; the net no longer sets the socket itself but hands its cap
// here (wcLink.netSet), and the lesser of the two is in force.
//
// "Idle" is the pacing net's quiet socket (pacenet.go): less than
// wcNetQuietBytes acknowledged and written and less than wcNetQuietSegs
// segments over the idle span, max(wcNetIdle, 2·srtt) up to wcNetIdleMax;
// a quiet socket is read once a wcNetQuiet (due), at once when its meter
// has wcNetQuietBytes written to it. The cap's factor is the net's
// (wcNetKOf, HS2_TUNE_PACENET_K), its floor wcPaceFloor, and it moves only
// by more than wcNetMove.

// TuneSendCap 0 (cmd/hs2 applyTuning: HS2_TUNE_SENDCAP=0) turns C4-lite's
// own cap off: the pacing net's alone, on the exit only, as before.
var TuneSendCap = 1

// SendCapOff says whether C4-lite is off (HS2_TUNE_SENDCAP=0).
func SendCapOff() bool { return TuneSendCap == 0 }

const (
	// capSpan: the cap follows the largest delivery rate of this span.
	capSpan = 2 * time.Second
	// capDlvN, capPtN: the readings kept for the span and for the idle span
	// (one each capGap at most: a reading each 100 ms keeps 3.2 s and 6.4 s).
	capDlvN = 32
	capPtN  = 64
	capGap  = 100 * time.Millisecond
)

// capSample is one reading of a link socket's TCP_INFO.
type capSample struct {
	Now        time.Duration
	Delivery   float64 // tcpi_delivery_rate, bytes/s
	AppLimited bool    // tcpi_delivery_rate_app_limited: that sample's
	// Acked: bytes acknowledged (tcpi_bytes_acked); Written: bytes written
	// to the socket (its link meter); both cumulative
	Acked, Written uint64
	Segs           uint32        // data segments sent (tcpi_data_segs_out), cumulative
	SRTT           time.Duration // the socket's srtt (0: unknown): the idle span
}

type capRate struct {
	at time.Duration
	r  float64
}

type capPt struct {
	at           time.Duration
	acked, wrote uint64
	segs         uint32
}

// sendCap is one link socket's C4-lite: pure (its readings in, the cap
// out).
type sendCap struct {
	dlv   [capDlvN]capRate // the delivery rates of the last capSpan (ring)
	dHead int
	dN    int
	pts   [capPtN]capPt // readings of the idle span (ring), one each capGap
	pHead int
	pN    int
	last  capPt // the latest reading
	n     uint64
	on    bool   // its own cap: from the first sample not app-limited to idleness
	own   uint64 // ... bytes/s
	pn    uint64 // the pacing net's (0: none)
	set   uint64 // the cap in force (0: none)
	quiet bool   // the latest reading found the socket quiet
	// full: the delivery rate of the latest sample that was not app-limited
	// (0: none yet), for C3 (hostRate)
	full float64
	// linked: off was set for the link as it came up (its caps and the
	// switch then: the cores' runner never reads the switch again); off: no
	// cap of its own on this link, as SendCapOff for the process
	linked, off bool
	// tallies (the status line)
	appLim, lifts uint64
}

// Update takes a reading and says the cap in force (0: none), whether to
// set it on the socket now, or to lift it.
func (c *sendCap) Update(s capSample) (rate uint64, apply, lift bool) {
	fresh := c.n > 0 && s.Acked > c.last.acked
	c.n++
	if s.AppLimited {
		c.appLim++
	} else if s.Delivery > 0 {
		c.full = s.Delivery
	}
	pt := capPt{at: s.Now, acked: s.Acked, wrote: s.Written, segs: s.Segs}
	c.last = pt
	// the delivery rates of the span (a reading within capGap of the latest
	// kept one joins it)
	if s.Delivery > 0 {
		if k := (c.dHead + c.dN - 1) % capDlvN; c.dN > 0 && s.Now-c.dlv[k].at < capGap/2 {
			c.dlv[k].r = math.Max(c.dlv[k].r, s.Delivery)
		} else {
			if c.dN == capDlvN {
				c.dHead, c.dN = (c.dHead+1)%capDlvN, c.dN-1
			}
			c.dlv[(c.dHead+c.dN)%capDlvN] = capRate{s.Now, s.Delivery}
			c.dN++
		}
	}
	for c.dN > 0 && s.Now-c.dlv[c.dHead].at > capSpan {
		c.dHead, c.dN = (c.dHead+1)%capDlvN, c.dN-1
	}
	// idle: next to nothing acknowledged, written or sent over the idle
	// span, read from a reading at least that old
	span := min(max(wcNetIdle, 2*s.SRTT), wcNetIdleMax)
	for c.pN > 1 && s.Now-c.pts[(c.pHead+1)%capPtN].at >= span {
		c.pHead, c.pN = (c.pHead+1)%capPtN, c.pN-1
	}
	idle := false
	if c.pN > 0 {
		if r := c.pts[c.pHead]; s.Now-r.at >= span {
			idle = s.Acked-r.acked < wcNetQuietBytes && s.Written-r.wrote < wcNetQuietBytes &&
				s.Segs-r.segs < uint32(wcNetQuietSegs)
		}
	}
	if k := (c.pHead + c.pN - 1) % capPtN; c.pN == 0 || s.Now-c.pts[k].at >= capGap {
		if c.pN == capPtN {
			c.pHead, c.pN = (c.pHead+1)%capPtN, c.pN-1
		}
		c.pts[(c.pHead+c.pN)%capPtN] = pt
		c.pN++
	}
	c.quiet = idle
	switch {
	case c.off || !c.linked && SendCapOff():
		c.on = false
	case c.on && idle:
		c.on = false
		c.lifts++
	case !c.on && !idle && fresh && !s.AppLimited && s.Delivery > 0:
		c.on = true
	}
	if c.on {
		c.own = uint64(math.Max(wcNetKOf()*c.maxDlv(), wcPaceFloor))
	}
	return c.decide()
}

// maxDlv is the largest delivery rate of the last capSpan.
func (c *sendCap) maxDlv() float64 {
	mx := 0.0
	for i := range c.dN {
		mx = math.Max(mx, c.dlv[(c.dHead+i)%capDlvN].r)
	}
	return mx
}

// hostRate is C3's rate from these readings (hostq.go; decision T2): the
// least of the largest delivery rate of the last capSpan and the latest
// sample that was not app-limited. A drop shows at its first sample (the
// mark and the gate follow at the next hostqTick), where the pacing cap
// keeps the largest of the span, not to collapse on a passing dip; a
// higher app-limited sample Linux kept raises neither. False before any
// sample that was not app-limited (C3 takes the link meter's written rate
// then).
func (c *sendCap) hostRate() (float64, bool) {
	if c.full <= 0 {
		return 0, false
	}
	if mx := c.maxDlv(); mx > 0 {
		return math.Min(mx, c.full), true
	}
	return c.full, true
}

// SetPacenetCap takes the pacing net's cap (bytes/s; 0: none): the lesser
// of the two is in force from the next decision.
func (c *sendCap) SetPacenetCap(rate uint64) { c.pn = rate }

// decide is the cap in force: its own while on, the net's, the lesser of
// the two; set when it moved by more than wcNetMove, lifted when none is.
func (c *sendCap) decide() (rate uint64, apply, lift bool) {
	want := uint64(0)
	if c.on {
		want = c.own
	}
	if c.pn > 0 && (want == 0 || c.pn < want) {
		want = c.pn
	}
	switch {
	case want == 0:
		lift = c.set != 0
		c.set = 0
		return 0, false, lift
	case c.set == 0 || math.Abs(float64(want)-float64(c.set)) > wcNetMove*float64(c.set):
		c.set = want
		return want, true, false
	}
	return c.set, false, false
}

// due says whether the runner reads the socket at this wake: always, but a
// quiet one only each wcNetQuiet or once wcNetQuietBytes were written to it
// (written: its link meter's count; 0 with none).
func (c *sendCap) due(now time.Duration, written uint64) bool {
	if !c.quiet || c.n == 0 {
		return true
	}
	return written-c.last.wrote >= wcNetQuietBytes || now-c.last.at >= wcNetQuiet-capGap/2
}

// capSys is what C4-lite's runner asks of a link socket (pacecap_linux.go;
// a synthetic one in the tests).
type capSys interface {
	info() (tcpStat, bool)      // TCP_INFO
	setPacing(rate uint64) bool // SO_MAX_PACING_RATE (0 lifts)
}

// sendcapNoDlv: the line for a kernel without tcpi_delivery_rate is written.
var sendcapNoDlv atomic.Bool

// sendCapLink is one link socket's C4-lite with its runner's state: step
// is the runner's wake (each 100 ms), SetPacenetCap the pacing net's
// hand-off (the exit's runner); the lock keeps the two to one cap. The
// same reading moves the link's host-queue guard (C3) each hostqTick.
type sendCapLink struct {
	mu   sync.Mutex
	sys  capSys
	core sendCap
	// hq: the link's C3 (nil: none); hqAt, hqWrote: its latest update and
	// what the link meter had written then (hqSet: one was made)
	hq      *hostQ
	hqAt    time.Duration
	hqWrote uint64
	hqSet   bool
	// blocked: how long the link's writer has waited on the socket so far,
	// at C3's gate or in the write (the link meter's wrBlocked, waits past
	// blockedMin; nil: unknown), and its value at the reading before
	blocked   func() int64
	blockSeen int64
}

// newSendCapLink: the C4-lite of a link socket (sys), and the C3 its
// readings move (hq; nil: none).
func newSendCapLink(sys capSys, hq *hostQ) *sendCapLink { return &sendCapLink{sys: sys, hq: hq} }

// step reads the socket when the core has it due (written: what its link
// meter counted written to it), sets or lifts its cap, and moves C3. A
// kernel without tcpi_delivery_rate gets none of C4-lite's (one line for
// the process), and C3 the link meter's written rate.
func (l *sendCapLink) step(now time.Duration, written uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.core.due(now, written) {
		return
	}
	ts, ok := l.sys.info()
	if !ok {
		return
	}
	app := ts.appLimited
	if l.blocked != nil {
		// a writer that waited on the socket since the reading before is no
		// app-limited sender, whatever the kernel's bit says (§3.3: C4 reads
		// C3's "blocked behind the socket" as that signal). C3's gate keeps
		// under a segment or two unsent, and Linux marks every sample of a
		// socket written to with less than a segment unsent app-limited: the
		// bit alone left C4-lite off on every gated link, and BBR, whose
		// app-limited samples never end its STARTUP, at 2.89 times its rate
		// (the rig's 8 links at 25 Mbit/s: 35% resent).
		b := l.blocked()
		if b > l.blockSeen {
			app = false
		}
		l.blockSeen = b
	}
	if ts.dlvOK {
		l.apply(l.core.Update(capSample{Now: now, Delivery: float64(ts.deliveryRate), AppLimited: app,
			Acked: ts.bytesAcked, Written: written, Segs: ts.segsOut, SRTT: usDur(uint64(ts.rtt))}))
	} else if sendcapNoDlv.CompareAndSwap(false, true) {
		coreLogf("sendcap: the kernel lacks tcpi_delivery_rate: no pacing cap of C4-lite on the link sockets (the pacing net's alone, as before)")
	}
	l.guard(now, written, ts)
}

// guard moves C3 each hostqTick: its rate is hostRate, or, until the first
// sample not app-limited (or on a kernel without the delivery rate), what
// the link meter had written to the socket since the update before (0, an
// unknown rate, at the first: the gate at its floor); its segment the
// socket's.
func (l *sendCapLink) guard(now time.Duration, written uint64, ts tcpStat) {
	if l.hq == nil || l.hqSet && now-l.hqAt < hostqTick {
		return
	}
	rate, ok := l.core.hostRate()
	if !ok || !ts.dlvOK {
		rate = 0
		if l.hqSet && now > l.hqAt {
			rate = float64(written-l.hqWrote) / (now - l.hqAt).Seconds()
		}
	}
	l.hq.Update(rate, int(ts.sndMSS))
	l.hqAt, l.hqWrote, l.hqSet = now, written, true
}

// SetPacenetCap takes the pacing net's cap (0: lifted) and sets the lesser
// of the two at once; false: the socket refused it.
func (l *sendCapLink) SetPacenetCap(rate uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.core.SetPacenetCap(rate)
	return l.apply(l.core.decide())
}

func (l *sendCapLink) apply(rate uint64, apply, lift bool) bool {
	switch {
	case apply:
		if !l.sys.setPacing(rate) {
			l.core.set = 0 // not capped after all: the next decision sets it again
			return false
		}
	case lift:
		l.sys.setPacing(0)
	}
	return true
}

// line is the link's status line (§3.4).
func (l *sendCapLink) line(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := &l.core
	share := 0.0
	if c.n > 0 {
		share = 100 * float64(c.appLim) / float64(c.n)
	}
	return fmt.Sprintf("sendcap link %d: cap %.0f Mbit/s (%g x max delivery 2s), app-limited %.0f%%, idle-lifted %d",
		n, float64(c.set)*8/1e6, wcNetKOf(), share, c.lifts)
}
