package engine

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// A packet-level model of one hs2 link for the window core (§9.1 of
// docs/architecture/WINDOW_CORE.fa.md), in 1 ms steps, with no sockets and
// no sleeping:
//
//   - the path: a bottleneck of a given rate (changes at given times), a
//     tail-drop queue of a fixed size in bytes, one-way delay each way,
//     random loss after the bottleneck, an optional per-connection policer
//     (a token bucket on the link's connection only) and an optional outside
//     BBR flow sharing the bottleneck;
//   - the sender (the exit for a download): a BBRv1-like TCP (windowed max
//     bandwidth over 10 rounds, min_rtt over 10 s with PROBE_RTT, STARTUP
//     and DRAIN with full-pipe detection on rounds that were not
//     app-limited, the 1.25/0.75/1×6 pacing cycle, inflight ≤ cwnd =
//     2·BDP_est, packet conservation in recovery, RACK-style loss detection
//     (with cfg.rack Linux's reordering window too) and a tail timer, resends
//     counted), fed by an smux writer that sends
//     each stream only as far as its window credit and puts control frames
//     (pongs, short replies) first;
//   - the receiver (the edge): in-order delivery (data behind a hole waits
//     for its resend; Rcv_ooopack counts what arrived behind one), the
//     session's frame demux with its buffer (parks), per-stream readers at
//     a set pace, the smux update logic (adaptive window, step, pool,
//     smux.FitCore), the window turnaround samples, and the receiver
//     socket's own srtt/min_rtt, sampled only on what it sends (updates,
//     pings) so the bias of §3.3 appears: its ACK enters the download queue
//     when the update reaches the sender;
//   - the runner: every 100 ms, a link whose turn has come is snapshotted
//     (wcLinkSnap), turned into wcIn by wcBuilder and stepped, and the
//     ceilings, turnRef, qBrake and growOK go to the streams.
//
// The core under test is the real one. N4 (3·pace·min_rtt), fin
// ((1+m)·pace·min_rtt + step) and base (fixed 2 MiB) run in the same model
// to show that it reproduces what the rig measured before it is trusted.

// ---- configuration and results -----------------------------------------------

const (
	wmMSS       = 1448
	wmNeed      = 2 << 20
	wmMaxWin    = 16 << 20
	wmMinWin    = 64 << 10
	wmLagTarget = 128 << 10
	wmFrame     = 16 << 10
	wmBucket    = 8 << 20
	wmInitWin   = 256 << 10
	wmLowat     = 64 << 10 // the exit's unsent bytes in its socket (sendGate)
	wmReadChunk = 32 << 10
	wmHitGap    = 2 // ms: a cycle is window-limited if its data paused this long at the limit
	wmMbit      = 1e6 / 8
)

// What the real rig showed and the model takes over (z5-core-calibration.fa.md
// §3, §6):
//
//   - wmEff: a path carries at most this share of its nominal rate as
//     payload (TCP/IP, TLS and smux headers and the rig's emulator: 261 of
//     300 Mbit/s): each packet takes 1/wmEff of its payload of the
//     bottleneck and of its queue.
//   - wmStartPace: a sender held by its window never leaves BBR's STARTUP (its
//     rounds are app-limited), and STARTUP's pacing only rises: the rig's
//     window-limited senders paced at 3.9 times the path's rate, not 2.89
//     (its bandwidth samples run past the path: ACKs and deliveries come in
//     clumps). Each window update's credit, and after a PROBE_RTT the whole
//     window, goes out at that pace.
//   - Linux keeps BBR's min_rtt until a strictly lower sample comes (in µs),
//     so a sender whose queue is never quite empty enters PROBE_RTT every
//     ~10 s whatever the receiver does (the core's refresh does not stop it).
//     The model's samples are whole ms of a constant path: after the first,
//     none is strictly lower, and PROBE_RTT comes every 10 s as on the rig.
const (
	wmEff       = 0.87
	wmStartPace = 3.9
)

// wmWire is what a packet of n payload bytes takes of the bottleneck.
func wmWire(n int) int64 { return int64(math.Ceil(float64(n) / wmEff)) }

type wmMode uint8

const (
	wmBase  wmMode = iota // the 2 MiB window as the rig measured it (before the cover)
	wmN4                  // round 1: 3·pace·min_rtt, step w/2
	wmFin                 // round 2 "delay": (1+m(q))·pace·min_rtt + step, fine step
	wmCore                // the window core (on smux's windows: the cover past need, smux.CoverNext)
	wmCover               // smux's windows without the core: the need, or its cover while the window holds the stream back
)

func (m wmMode) String() string {
	return [...]string{"base", "n4", "fin", "core", "cover"}[m]
}

type wmStreamCfg struct {
	read  float64       // reader pace, bytes/s (0: reads what it has at once)
	src   float64       // source pace at the sender, bytes/s (0: unlimited)
	start time.Duration // when its transfer begins
}

type wmChange struct {
	at   time.Duration
	rate float64 // bytes/s
}

type wmCfg struct {
	name      string
	rate      float64       // bottleneck, bytes/s
	rtt       time.Duration // base round trip
	queue     time.Duration // tail-drop queue, in time at the first rate
	loss      float64       // random loss per packet
	changes   []wmChange
	policer   float64       // per-connection policer, bytes/s (0: none)
	compete   time.Duration // an outside BBR flow starts then (0: none)
	streams   []wmStreamCfg // default: one fast download
	mode      wmMode
	stage     int
	dur, warm time.Duration // default 30 s and 6 s
	seed      uint64
	pool      int64 // window pool, bytes (0: 64 MiB; < 0: none)
	ackAgg    int   // ms the receiver's ACKs are held and sent together (0: each ms)
	// stall, stallMax: the rig's emulator is a Go program on a loaded VM
	// whose delivery goroutines sleep until each packet is due; now and
	// then one wakes late and delivers what is overdue at once (each
	// direction, a stall starts with probability stall per ms and lasts up
	// to stallMax ms)
	stall     float64
	stallMax  int
	hitTruth  bool // hit from the sender's truth instead of the receiver's pause
	apProbing func(t time.Duration) bool
	trace     bool
	// red team: exit runs the core as the exit's (uploads: no record of the
	// sender, no control ping), and only with WcoreUpload (the lab knob):
	// without it the exit has no core, as the runner's; lossAt changes the
	// path's random loss at given times
	exit   bool
	lossAt []wmChange
	// oldExit: the edge's core with an exit whose record has no TCP_INFO
	// in it (a 64-byte record: no q_snd, no resend share): it stays OFF
	oldExit bool
	// reorder, reorderMs: a packet of the link's connection is held this
	// many ms past its turn with this probability (multipath, link
	// aggregation): what comes after it meanwhile arrives behind a hole
	reorder   float64
	reorderMs int
	// rack: the senders find their losses as Linux's RACK does
	// (tcp_recovery.c) instead of taking every packet sent before the newest
	// (s)acked one for lost at once (wmFlow.rackMark): once a packet came in
	// after one sent later (reordering seen), a packet is lost only when it is
	// still out a reordering window (min_rtt/4, more steps of it after DSACKs)
	// past the round trip of the latest sent one that came. A packet the path
	// holds less than that is not resent: it shows only at the receiver, as
	// what arrived behind its hole (Rcv_ooopack), and not in the exit's
	// record. Without it the model's sender resends every reordered packet
	// and the record shows reordering as loss (z5-red1-core-notes §7).
	rack bool
	// reoField: the link's connection is reordered as the real Iran↔abroad
	// path reordered a direct TCP flow (wmReo): bursty episodes, more of them
	// the faster the connection runs, in which half its packets come
	// reorderMs (3 if 0) late; reoScale multiplies the episodes' share of the
	// time (0: 1)
	reoField bool
	reoScale float64
	// red team, round 2: rttAt changes the path's round trip at given times
	// (rate: the new round trip in ms; a route change: the sockets' min_rtt
	// keep the old floor, as Linux's for 5 minutes); polAt changes the
	// policer's rate (0: none); recEvery is the cadence of the exit's records
	// (0: 2 s); recOff: no record comes within these spans (the stats stream
	// stalled and is reopening; the next one covers the gap); recZero: the
	// records within these spans have no TCP_INFO in them (all of the
	// extended fields 0, as serveStats sends when tcpStats fails)
	rttAt []wmChange
	polAt []wmChange
	// paceCap: the exit caps its socket's pacing as the lab's WcorePaceCap
	// (wcPaceRate) at this many times the largest rate it delivered in a
	// runner wake of the last wcPaceSpan, or the windows granted per min_rtt
	// while its round trip shows no queue; lifted while it sends next to
	// nothing (0: no cap)
	paceCap float64
	// netOff: the exit's link socket runs without the pacing net
	// (pacenet.go), as before it (the rig's calibration runs); the net runs
	// on the exit's socket only (the downloads' sender), as PaceNet says
	netOff   bool
	recEvery time.Duration
	recOff   [][2]time.Duration
	recZero  [][2]time.Duration
	// check sees every step of the core (the invariant tests)
	check func(t time.Duration, c *wcore, in wcIn, o wcOut)
	// bwKeep: the sender keeps its max_bw through app-limited phases, as
	// Linux does (bbr_update_bw: an app-limited sample under the max is left
	// out, and the minmax filter expires the max only as a new sample comes).
	// Without it the max expires after 10 rounds whatever came, and the next
	// app-limited sample (PROBE_RTT's four packets) is max_bw: a
	// window-limited sender in PROBE_BW then paces at next to nothing for
	// seconds (red team, H2: what the covers cost next to it)
	bwKeep bool
}

type wmResult struct {
	cfg        wmCfg
	mbit       float64 // goodput of the link's downloads over [warm, dur]
	reach      float64 // what the path and the windows allow, Mbit/s
	resend     float64 // resent share of the exit's data segments over [warm, dur]
	resendAll  float64 // ... over the whole run
	selfDrop   float64 // tail and policer drops of the link's connection, share of its packets [warm, dur]
	randDrop   float64
	qMean, q99 float64 // bottleneck queue, ms, over [warm, dur]
	iP50, iP99 float64 // interactive replies on the same link, ms
	t90        time.Duration
	drains     [3]int // queue, loss, rate (whole run)
	holdDrains int    // drains that started from HOLD
	probes     [2]int // ok, failed
	peakB      uint64
	lastState  wcState
	compMbit   float64
	probeRTTs  int // the sender's PROBE_RTTs over the run
	// back90: with changes of the path's rate, from the last one until a
	// second's pace is back at 90% of the pace before the first (as the
	// rig's analysis measures it; -1: never)
	back90 time.Duration
	trace  []string
	// rtxSec: the sender's cumulative (resent, sent) segments at each second
	rtxSec [][2]uint64
	// netN: the times the pacing net engaged; netMs: ms it held the pace
	netN  int
	netMs int64
	// ooo: the share of the link's segments that arrived behind a hole
	// (Rcv_ooopack / Data_segs_in at the receiver) over [warm, dur];
	// oooHalf: the receiver's cumulative (behind a hole, in) segments at
	// each 500 ms, as the field's samples (wmReoBins); spurious: segments the
	// sender resent whose first copy arrived after all (a DSACK each)
	ooo      float64
	oooHalf  [][2]uint64
	spurious uint64
	// cuts: the core's halvings for loss between evaluations (fastStop;
	// counted in drains[1] too), each a line "packets arrived behind holes on
	// a clean path" of the runner
	cuts int
	// reoEps: the field's reordering episodes that started (cfg.reoField)
	reoEps int
	// updMiB: window updates per MiB read, of the streams that read past
	// the need (each past its first update; smux sends one per step, a MiB
	// at the need)
	updMiB float64
}

func (r wmResult) String() string {
	return fmt.Sprintf("%-34s %4s %6.1f Mbit (reach %5.0f) resend %5.2f%% (all %5.2f%%) drop self %5.2f%% rand %5.2f%% q %5.1f/%5.1f ms i %5.0f/%5.0f t90 %5.1fs drains %v/%d probes %v peakB %.1f MiB",
		r.cfg.name, r.cfg.mode, r.mbit, r.reach, 100*r.resend, 100*r.resendAll, 100*r.selfDrop, 100*r.randDrop,
		r.qMean, r.q99, r.iP50, r.iP99, r.t90.Seconds(), r.drains, r.holdDrains, r.probes, float64(r.peakB)/(1<<20))
}

// ---- packets and queues ---------------------------------------------------------

type wmPkt struct {
	f       *wmFlow
	seq     uint64
	n       int
	xid     uint64 // transmission order
	sentAt  int64
	dlv     uint64 // the flow's delivered bytes when it was sent
	dlvAt   int64
	firstAt int64
	arrive  int64
	appLim  bool
	rtx     bool
	sacked  bool
	lost    bool
	inNet   bool // in the queue or on the line: not to be reused yet
	// rnd: lost to the path's random loss. A packet of k MSS stands for k
	// segments of which one was lost (it is dropped with 1-(1-p)^k, so loss
	// events come as often as on the real path): its resend counts as one
	// segment, else the resend share would read k× the path's loss
	rnd bool
	// spur (cfg.rack): it was marked lost and came after all; the sender
	// took its DSACK
	spur bool
}

type wmQ[T any] struct {
	buf  []T
	head int
}

func (q *wmQ[T]) push(v T) { q.buf = append(q.buf, v) }
func (q *wmQ[T]) len() int { return len(q.buf) - q.head }
func (q *wmQ[T]) peek() T  { return q.buf[q.head] }
func (q *wmQ[T]) pop() T {
	v := q.buf[q.head]
	var zero T
	q.buf[q.head] = zero
	q.head++
	if q.head > 1024 && q.head*2 > len(q.buf) {
		n := copy(q.buf, q.buf[q.head:])
		clear(q.buf[n:])
		q.buf = q.buf[:n]
		q.head = 0
	}
	return v
}

// ---- the sender: BBRv1-like TCP ----------------------------------------------------

const (
	bbrStartup = iota
	bbrDrain
	bbrProbeBW
	bbrProbeRTT
)

var bbrCycle = [8]float64{1.25, 0.75, 1, 1, 1, 1, 1, 1}

const bbrHighGain = 2.885

type wmFlow struct {
	m    *wmModel
	main bool
	unit int

	sndNxt, writeSeq uint64 // writeSeq: what the app wrote (∞ for the competitor)
	xid              uint64
	out              wmQ[*wmPkt] // sent, not yet sacked or marked lost, in send order
	rtxQ             wmQ[*wmPkt]
	inflight         int64
	cwnd             int64
	pacing           float64 // bytes/ms
	credit           float64
	delivered        uint64
	dlvAt            int64
	firstTx          int64
	appLimUntil      uint64
	maxAckXid        uint64
	lastAckAt        int64
	lastSendAt       int64
	inRecovery       bool
	recoveryXid      uint64
	srtt, rttvar     float64 // ms
	minRTT           int64   // tcp min_rtt, ms (0: none)
	retrans, segsOut uint64  // segments
	rtxBytes         uint64
	// rateDlv: tcpi_delivery_rate (tcp_rate_gen): the latest rate sample
	// that was not app-limited, or an app-limited one past it, bytes/ms
	rateDlv float64

	// BBR
	mode             int
	bw               [10]float64 // per-round max, bytes/ms
	bwRound          [10]uint64
	round            uint64
	nextRoundDlv     uint64
	roundStart       bool
	fullBW           float64
	fullCnt          int
	full             bool
	bMinRTT          int64 // ms
	bMinUs           int64 // the same in µs, as Linux keeps it (see wmStartPace)
	bMinStamp        int64
	probeRTTDone     int64
	probeRTTRoundOK  bool
	cycleIdx         int
	cycleStamp       int64
	pacingGain       float64
	cwndGain         float64
	priorCwnd        int64
	pktConservation  bool
	ackBatch         []*wmPkt // arrived at its receiver, not yet sent back
	gotBytes, gotAll uint64   // competitor: bytes its sink received
	probeRTTs        int      // PROBE_RTTs entered
	bwLast           float64  // cfg.bwKeep: the latest max_bw

	// RACK (cfg.rack, rackMark): rackXid and rackRTT are the latest sent
	// packet that was (s)acked and its round trip (rack.mstamp, rack.rtt_us);
	// reordSeen: a packet came in after one sent later (tp->reord_seen);
	// reoSteps: the reordering window in steps of min_rtt/4, one more for a
	// DSACK at most once a round trip (reoDlv: delivered at the latest),
	// back to one after reoPersist recoveries with none; dsack: a DSACK
	// came with the acks being taken; rackLost: what the reordering timer
	// marked lost since the latest ack (BBR answers it there)
	rack                 bool
	rackXid              uint64
	rackRTT              int64
	reordSeen            bool
	reoSteps, reoPersist int
	reoDlv               uint64
	dsack                bool
	rackLost             int64
	spurious             uint64
}

func newFlow(m *wmModel, main bool, unit int) *wmFlow {
	f := &wmFlow{m: m, main: main, unit: unit, cwnd: 10 * wmMSS, pacingGain: bbrHighGain, cwndGain: bbrHighGain,
		rack: m.cfg.rack, reoSteps: 1}
	f.pacing = wmStartPace * float64(10*wmMSS) / float64(m.rtt)
	if !main {
		f.writeSeq = math.MaxUint64 / 2
	}
	return f
}

func (f *wmFlow) segs(n int) uint64 { return uint64((n + wmMSS - 1) / wmMSS) }

func (f *wmFlow) maxBW() float64 {
	b := 0.0
	for i, v := range f.bw {
		if f.bwRound[i]+10 > f.round && v > b {
			b = v
		}
	}
	if f.m.cfg.bwKeep {
		if b > 0 {
			f.bwLast = b
		} else {
			b = f.bwLast
		}
	}
	return b
}

func (f *wmFlow) bdp(gain float64) float64 {
	if f.bMinRTT <= 0 {
		return float64(f.cwnd)
	}
	return gain * f.maxBW() * float64(f.bMinRTT)
}

// send moves what pacing, cwnd and the app allow this ms into the path.
func (f *wmFlow) send() {
	m := f.m
	t := m.t
	pace := f.pacing
	if f.main && m.pcCap > 0 {
		pace = math.Min(pace, m.pcCap)
	}
	f.credit += pace
	cap := math.Max(2*float64(f.unit), pace)
	if f.credit > cap {
		f.credit = cap
	}
	for f.credit > 0 {
		if f.inflight+int64(f.unit) > f.cwnd {
			return // cwnd-limited
		}
		var p *wmPkt
		if f.rtxQ.len() > 0 {
			old := f.rtxQ.pop()
			p = m.newPkt()
			p.seq, p.n, p.rtx = old.seq, old.n, true
			if old.rnd {
				f.retrans++
			} else {
				f.retrans += f.segs(p.n)
			}
			f.rtxBytes += uint64(p.n)
		} else {
			if f.main && f.writeSeq-f.sndNxt < wmLowat {
				m.fill(f)
			}
			avail := f.writeSeq - f.sndNxt
			if avail == 0 {
				return
			}
			p = m.newPkt()
			p.seq, p.n = f.sndNxt, int(min(avail, uint64(f.unit)))
			f.sndNxt += uint64(p.n)
		}
		if f.inflight == 0 {
			f.firstTx, f.dlvAt = t, t
		}
		f.xid++
		p.f, p.xid, p.sentAt = f, f.xid, t
		p.dlv, p.dlvAt, p.firstAt, p.appLim = f.delivered, f.dlvAt, f.firstTx, f.appLimUntil != 0
		f.inflight += int64(p.n)
		f.out.push(p)
		f.lastSendAt = t
		f.segsOut += f.segs(p.n)
		f.credit -= float64(p.n)
		m.enqueue(p)
	}
}

func (f *wmFlow) rttSample(r float64) {
	if f.srtt == 0 {
		f.srtt, f.rttvar = r, r/2
	} else {
		f.rttvar += 0.25 * (math.Abs(r-f.srtt) - f.rttvar)
		f.srtt += 0.125 * (r - f.srtt)
	}
	if f.minRTT == 0 || int64(r) < f.minRTT {
		f.minRTT = int64(r)
	}
}

// onAcks takes a batch of (s)acked packets back at the sender.
func (f *wmFlow) onAcks(pkts []*wmPkt) {
	m := f.m
	t := m.t
	var newest *wmPkt
	acked := int64(0)
	prior := f.maxAckXid
	for _, p := range pkts {
		if p.sacked || p.lost {
			if f.rack && p.lost && !p.rtx && !p.spur {
				// (RACK) its first copy came after all: what came in
				// behind it was reordering, and its resend will come in
				// twice (a DSACK; the model takes it now, not a round trip
				// on with the resend)
				p.spur = true
				f.reordSeen, f.dsack = true, true
				f.spurious += f.segs(p.n)
			}
			continue
		}
		if f.rack && !p.rtx && p.xid < prior {
			f.reordSeen = true // it came in after one sent later
		}
		p.sacked = true
		f.inflight -= int64(p.n)
		f.delivered += uint64(p.n)
		acked += int64(p.n)
		if newest == nil || p.xid > newest.xid {
			newest = p
		}
	}
	if newest == nil {
		return
	}
	priorInflight := f.inflight + acked
	f.dlvAt, f.lastAckAt = t, t
	f.firstTx = newest.sentAt
	if newest.xid > f.maxAckXid {
		f.maxAckXid = newest.xid
	}
	if f.appLimUntil != 0 && f.delivered > f.appLimUntil {
		f.appLimUntil = 0
	}
	rtt := int64(-1)
	if !newest.rtx {
		rtt = t - newest.sentAt
		f.rttSample(float64(rtt))
	}
	// rate sample (tcp_rate_gen): over the longer of the send and ack
	// intervals, discarded when shorter than min_rtt
	ival := max(t-newest.dlvAt, newest.sentAt-newest.firstAt)
	bwOK := ival > 0 && (f.minRTT == 0 || ival >= f.minRTT)
	bw := 0.0
	if bwOK {
		bw = float64(f.delivered-newest.dlv) / float64(ival)
	}
	priorDlv, appLim := newest.dlv, newest.appLim
	if bwOK && (!appLim || bw >= f.rateDlv) {
		f.rateDlv = bw
	}
	if f.rack {
		// (tcp_rack_advance: a resend's round trip shorter than min_rtt
		// may be its first copy's, and does not count)
		if newest.xid > f.rackXid && (!newest.rtx || t-newest.sentAt >= f.minRTT) {
			f.rackXid, f.rackRTT = newest.xid, t-newest.sentAt
		}
		f.reoUpdate(priorDlv)
		lost := f.rackMark() + f.rackLost
		f.rackLost = 0
		f.bbr(t, bw, bwOK, appLim, priorDlv, rtt, acked, lost, priorInflight)
		return
	}
	// RACK: what was sent before the newest acked packet and is not acked
	// is lost (the path keeps order)
	lost := int64(0)
	for f.out.len() > 0 {
		p := f.out.peek()
		if p.xid > f.maxAckXid {
			break
		}
		f.out.pop()
		if p.sacked {
			m.freePkt(p)
			continue
		}
		p.lost = true
		f.inflight -= int64(p.n)
		lost += int64(p.n)
		f.rtxQ.push(p)
	}
	f.bbr(t, bw, bwOK, appLim, priorDlv, rtt, acked, lost, priorInflight)
}

// rackMark is RACK's loss detection (tcp_rack_detect_loss, cfg.rack), from
// the oldest packet out: one sent before the latest sent packet that came in
// (rackXid) is lost once it has been out for that one's round trip and the
// reordering window. The first that has not been is the oldest still in time,
// and so is every packet after it (sent later). It returns the bytes it
// marked lost.
func (f *wmFlow) rackMark() int64 {
	t := f.m.t
	wnd := f.reoWnd()
	lost := int64(0)
	for f.out.len() > 0 {
		p := f.out.peek()
		if p.sacked {
			f.out.pop()
			f.m.freePkt(p)
			continue
		}
		if p.xid >= f.rackXid || t-p.sentAt < f.rackRTT+wnd {
			break
		}
		f.out.pop()
		p.lost = true
		f.inflight -= int64(p.n)
		lost += int64(p.n)
		f.rtxQ.push(p)
	}
	return lost
}

// reoWnd is RACK's reordering window (tcp_rack_reo_wnd): none until
// reordering was seen (Linux waits min_rtt/4 then too, but neither in
// recovery nor once the dupthresh's three segments were sacked past a hole,
// and a packet of the model holds k segments: past three within the ms),
// then min_rtt/4 a step, at most srtt.
func (f *wmFlow) reoWnd() int64 {
	if !f.reordSeen {
		return 0
	}
	return min(f.minRTT/4*int64(f.reoSteps), int64(f.srtt))
}

// reoUpdate is tcp_rack_update_reo_wnd, at each ack: a DSACK takes the
// reordering window a step up, at most once a round trip (priorDlv:
// delivered when the newest packet acked was sent), and holds it for 16
// recoveries (counted down as each starts, in bbr); past those with no
// DSACK it is one step again.
func (f *wmFlow) reoUpdate(priorDlv uint64) {
	if f.dsack && priorDlv < f.reoDlv {
		f.dsack = false // a round trip has not passed since the last step
	}
	if f.dsack {
		f.reoSteps = min(f.reoSteps+1, 255)
		f.reoDlv, f.reoPersist = f.delivered, 16
		f.dsack = false
	} else if f.reoPersist == 0 {
		f.reoSteps = 1
	}
}

// tailTimer marks the oldest outstanding packet lost when nothing was
// acked past it for long (a tail loss: no later packet to reveal it). With
// cfg.rack it first runs RACK's reordering timer (tcp_rack_reo_timeout): what
// it marks lost the sender's BBR answers at the next ack.
func (f *wmFlow) tailTimer() {
	if f.rack {
		f.rackLost += f.rackMark()
	}
	if f.out.len() == 0 || f.srtt == 0 {
		return
	}
	p := f.out.peek()
	if p.sacked {
		return
	}
	if float64(f.m.t-p.sentAt) > 2*f.srtt+4*f.rttvar+10 {
		f.out.pop()
		p.lost = true
		f.inflight -= int64(p.n)
		f.rtxQ.push(p)
		if !f.inRecovery {
			f.inRecovery, f.recoveryXid = true, f.xid
			f.priorCwnd = max(f.priorCwnd, f.cwnd)
		}
		// an RTO: Linux's bbr_set_state(TCP_CA_Loss) restarts the
		// full-pipe detection of STARTUP
		f.fullBW = 0
	}
}

func (f *wmFlow) bbr(t int64, bw float64, bwOK, appLim bool, priorDlv uint64, rtt, acked, lost, priorInflight int64) {
	m := f.m
	f.roundStart = false
	if priorDlv >= f.nextRoundDlv {
		f.nextRoundDlv = f.delivered
		f.round++
		f.roundStart = true
		f.pktConservation = false
	}
	if lost > 0 && !f.inRecovery {
		f.inRecovery, f.recoveryXid = true, f.xid
		f.priorCwnd = f.cwnd
		f.pktConservation = true
		f.nextRoundDlv = f.delivered
		f.cwnd = f.inflight + acked
		if f.rack && f.reoPersist > 0 {
			f.reoPersist--
		}
	}
	if f.inRecovery && f.maxAckXid >= f.recoveryXid {
		f.inRecovery = false
		f.pktConservation = false
		f.cwnd = max(f.cwnd, f.priorCwnd)
	}
	if bwOK && (!appLim || bw >= f.maxBW()) {
		i := f.round % 10
		if f.bwRound[i] != f.round {
			f.bwRound[i], f.bw[i] = f.round, 0
		}
		if bw > f.bw[i] {
			f.bw[i] = bw
		}
	}
	if f.mode == bbrProbeBW && f.bMinRTT > 0 {
		g := bbrCycle[f.cycleIdx]
		full := t-f.cycleStamp > f.bMinRTT
		next := full
		if g > 1 {
			next = full && (lost > 0 || float64(priorInflight) >= f.bdp(g))
		} else if g < 1 {
			next = full || float64(priorInflight) <= f.bdp(1)
		}
		if next {
			f.cycleIdx = (f.cycleIdx + 1) % 8
			f.cycleStamp = t
			f.pacingGain = bbrCycle[f.cycleIdx]
		}
	}
	if !f.full && f.roundStart && !appLim {
		if b := f.maxBW(); b >= 1.25*f.fullBW {
			f.fullBW, f.fullCnt = b, 0
		} else if f.fullCnt++; f.fullCnt >= 3 {
			f.full = true
		}
	}
	if f.mode == bbrStartup && f.full {
		f.mode = bbrDrain
		f.pacingGain, f.cwndGain = 1/bbrHighGain, bbrHighGain
	}
	if f.mode == bbrDrain && float64(f.inflight) <= f.bdp(1) {
		f.enterProbeBW(t)
	}
	expired := t > f.bMinStamp+10000
	// Linux keeps min_rtt in µs and takes a strictly lower sample (see
	// wmStartPace): the model's whole-ms samples get where in their ms the
	// packet went and came back, so on an empty queue a sample beats the
	// floor now and then (ever more rarely as the floor ages), and under a
	// standing queue never
	if rtt >= 0 {
		us := rtt*1000 - int64(m.rnd.IntN(1000))
		if f.bMinRTT == 0 || us < f.bMinUs || expired {
			f.bMinUs, f.bMinStamp = max(us, 1000), t
			f.bMinRTT = (f.bMinUs + 999) / 1000
		}
	}
	if expired && f.mode != bbrProbeRTT && f.bMinRTT > 0 {
		f.probeRTTs++
		f.mode = bbrProbeRTT
		f.pacingGain, f.cwndGain = 1, 1
		f.priorCwnd = max(f.priorCwnd, f.cwnd)
		f.probeRTTDone = 0
	}
	if f.mode == bbrProbeRTT {
		// Linux's bbr_update_min_rtt marks PROBE_RTT app-limited
		// (tp->app_limited = delivered + in flight): its small samples
		// neither enter the bandwidth filter nor count toward a full pipe.
		// Without it a window-limited sender whose samples had all been
		// app-limited for 10 rounds took a PROBE_RTT sample as max_bw,
		// "filled" the pipe within PROBE_RTT and paced at ~0 in ProbeBW
		// (z12: 2 of 3 seeds of 300/150 q200 with a steady HOLD)
		f.appLimUntil = max(f.delivered+uint64(f.inflight), 1)
		if f.probeRTTDone == 0 && f.inflight <= 4*wmMSS {
			f.probeRTTDone = t + 200
			f.probeRTTRoundOK = false
			f.nextRoundDlv = f.delivered
		} else if f.probeRTTDone != 0 {
			if f.roundStart {
				f.probeRTTRoundOK = true
			}
			if f.probeRTTRoundOK && t > f.probeRTTDone {
				f.bMinStamp = t
				f.cwnd = max(f.cwnd, f.priorCwnd)
				if f.full {
					f.enterProbeBW(t)
				} else {
					f.mode = bbrStartup
					f.pacingGain, f.cwndGain = bbrHighGain, bbrHighGain
				}
			}
		}
	}
	if b := f.maxBW(); b > 0 {
		g := f.pacingGain
		if f.mode == bbrStartup {
			g = wmStartPace // (see wmStartPace)
		}
		r := g * b * 0.99
		if f.full || r > f.pacing {
			f.pacing = r
		}
	}
	target := int64(f.bdp(f.cwndGain)) + 3*int64(f.unit)
	if f.bMinRTT <= 0 || f.maxBW() == 0 {
		target = f.cwnd + acked
	}
	switch {
	case f.pktConservation:
		f.cwnd = max(f.cwnd, f.inflight+acked)
	case f.full:
		f.cwnd = min(f.cwnd+acked, target)
	case f.cwnd < target || f.delivered < 10*wmMSS:
		f.cwnd += acked
	}
	f.cwnd = max(f.cwnd, 4*wmMSS)
	if f.mode == bbrProbeRTT {
		f.cwnd = min(f.cwnd, 4*wmMSS)
	}
	_ = m
}

func (f *wmFlow) enterProbeBW(t int64) {
	f.mode = bbrProbeBW
	f.cwndGain = 2
	f.cycleIdx = (8 - f.m.rnd.IntN(7)) % 8
	f.cycleStamp = t
	f.pacingGain = bbrCycle[f.cycleIdx]
}

// ---- the link's streams ----------------------------------------------------------

// wmSnd is a stream at the sender: its credit from the receiver's updates.
type wmSnd struct {
	limit   uint64 // consumed + window of the latest update to arrive
	written uint64
	src     float64
	start   int64
	blocked bool // wanted to send past its credit since the last update came
	hitS    uint32
}

type wmTurn struct {
	tUpd  int64
	lPrev uint64
	aAt   int64
}

// wmRcvStream is a stream at the receiver: smux's receive side.
type wmRcvStream struct {
	id        uint32
	read      float64
	start     int64
	readCred  float64
	unread    int64
	numRead   uint64
	rcvWin    uint32
	incr      uint64
	annCons   uint64
	annWin    uint32
	limHigh   uint64 // the highest limit announced so far (smux: streamCore.limHigh)
	minUnread int64
	adapted   bool
	firstAt   int64
	firstRead uint64
	lastParks uint32
	claim     int64
	ring      [4]wmTurn
	ringN     int
	ringHead  int
	hitCycle  bool
	pause     int64 // ms: the longest pause at a limit since the latest update (smux streamCore.pause)
	covered   bool  // smux streamCore.covered
	miss      uint8
	held      uint8 // smux streamCore.held (CoverHold)
	updN      uint32
	hitN      uint32
	lagN      uint32
	poolShort uint32
	snapHit   uint32 // hitN at the previous core snapshot
	turnMin   time.Duration
	turnLast  time.Duration
	lastTurn  time.Duration
	updAt     int64
	updRead   uint64
	paceRead  uint64
	paceDur   int64
	ceil      uint32
	// the runner's hit gap (wcLink.noteRates): its rate smoothed over the
	// wakes, and wcHitFrames of its frames' time at it, in ms
	rateRead uint64
	rateE    float64
	hitGap   int64
}

type wmFrameRec struct {
	end  uint64 // TCP sequence just past it
	n    int
	kind uint8 // 0 data, 1 pong, 2 reply
	sid  int
	t0   int64
}

type wmRev struct {
	at   int64
	kind uint8 // 0 acks, 1 update, 2 ping, 3 request
	f    *wmFlow
	pkts []*wmPkt
	sid  int
	cons uint64
	win  uint32
	sent int64
}

// ---- the field's reordering (cfg.reoField) ------------------------------------------

// The real Iran↔abroad path (72 ms) reordered a direct TCP flow (BBR) as its
// receiver's Rcv_ooopack / Data_segs_in read, sampled every 0.5 s, while the
// sender resent next to nothing (3, 13 and 26 segments in 15 s: Linux's
// RACK waited the reordering out):
//
//	300 Mbit/s:   2.0% overall; 5 of 29 samples had any; p90 6.6%, max 38%
//	600 Mbit/s:   6.1% overall; 13 of 28 had any; p90 22%, max 35%
//	1.7 Gbit/s:  14.7% overall; p50 20%, p90 42%, max 49%
//	a few Mbit/s: 0 to 0.5%
//
// It comes in episodes, more of them the faster the flow runs, and no sample
// read more than half of its packets behind holes. wmReo has the path, within
// an episode, send each packet of the link's connection by a branch reorderMs
// longer with probability wmReoHalf (a link aggregate or a balancer spraying
// over two routes): what goes the short way while one is held arrives behind
// its hole, so a sample within an episode reads half of its packets behind
// holes (at a few Mbit/s fewer: few packets pass one in reorderMs), and one
// the episode covered in part that share of it. The episodes take a share of
// the time that grows with the connection's own rate (its packets through
// the bottleneck, a 100 ms average: the field's flows were alone on the
// path), wmReoTab, each of an exponential length; TestWcoreS2ReorderProfile
// holds the profile to the field's samples.
//
// The reorderMs of the field is not known: RACK waits min_rtt/4 (18 ms on
// the 72 ms path) and the sender resent none, so it was under that; 3 ms by
// default (a route through another box of the same site).
const (
	wmReoHalf = 0.5
	wmReoMs   = 3
)

// wmReoTab: the episodes' share of the time and their mean length (ms) at
// the connection's rate (Mbit/s), linear in between, the last row's past it.
var wmReoTab = []struct{ mbit, duty, len float64 }{
	{0, 0, 120}, {300, 0.040, 120}, {600, 0.122, 140}, {1700, 0.294, 200},
}

// wmReoAt is wmReoTab at r Mbit/s.
func wmReoAt(r float64) (duty, l float64) {
	t := wmReoTab
	if r >= t[len(t)-1].mbit {
		return t[len(t)-1].duty, t[len(t)-1].len
	}
	for i := 1; i < len(t); i++ {
		if r < t[i].mbit {
			a, b := t[i-1], t[i]
			x := (math.Max(r, 0) - a.mbit) / (b.mbit - a.mbit)
			return a.duty + x*(b.duty-a.duty), a.len + x*(b.len-a.len)
		}
	}
	return 0, t[0].len
}

// wmReo is the field's reordering of one link's connection. It draws from
// its own generator, so it leaves the model's draws as they were.
type wmReo struct {
	rnd   *rand.Rand
	ms    int64   // how late a held packet comes
	scale float64 // times the episodes' share of the time
	rate  float64 // the connection's rate through the bottleneck, Mbit/s (100 ms average)
	bytes int     // its bytes this ms
	on    bool
	until int64 // the episode ends then (ms)
	eps   int   // episodes started
}

func newWmReo(seed uint64, ms int, scale float64) *wmReo {
	if ms <= 0 {
		ms = wmReoMs
	}
	if scale <= 0 {
		scale = 1
	}
	return &wmReo{rnd: rand.New(rand.NewPCG(seed^0x7e0de12, seed+0x9e3779b97f4a7c15)), ms: int64(ms), scale: scale}
}

// hold takes a packet of n bytes of the connection off the bottleneck and
// says how many ms late it comes (0: on time).
func (r *wmReo) hold(n int) int64 {
	r.bytes += n
	if r.on && r.rnd.Float64() < wmReoHalf {
		return r.ms
	}
	return 0
}

// tick ends the ms at t: the rate, and an episode's end or start (a two-state
// process whose episodes last l ms on average and take duty of the time: one
// starts with probability duty/(l·(1−duty)) a ms between them).
func (r *wmReo) tick(t int64) {
	r.rate += (float64(r.bytes)*8/1000 - r.rate) / 100
	r.bytes = 0
	if r.on {
		if t >= r.until {
			r.on = false
		}
		return
	}
	duty, l := wmReoAt(r.rate)
	duty = math.Min(duty*r.scale, 0.9)
	if duty > 0 && r.rnd.Float64() < duty/(l*(1-duty)) {
		r.on, r.until = true, t+1+int64(r.rnd.ExpFloat64()*l)
		r.eps++
	}
}

// ---- the model ---------------------------------------------------------------------

type wmModel struct {
	cfg  wmCfg
	rnd  *rand.Rand
	t    int64
	end  int64
	d    int64 // one-way delay, ms
	rtt  int64
	rate float64 // bytes/ms
	unit int

	// path
	q          wmQ[*wmPkt]
	qBytes     int64
	qCap       int64
	serveCred  float64
	line       wmQ[*wmPkt]
	late       []*wmPkt // reordered: held past their turn (cfg.reorder)
	due        []*wmPkt // arrive's own
	lossUnit   float64
	tokens     float64
	free       []*wmPkt
	slab       []wmPkt
	tailDrops  uint64 // main flow, segments
	polDrops   uint64
	randDrops  uint64
	rev        wmQ[wmRev]
	main, comp *wmFlow
	fwdWake    int64 // the forward delivery stalls until then
	revWake    int64
	// bn is the model whose path (queue, line, random loss, stalls) this
	// link's packets take: itself, or with several links on one bottleneck
	// (wcore_shared_test.go) the first link's, which holds them all in links
	bn    *wmModel
	links []*wmModel
	// recPhase: the exit's records of this link come this many ms past
	// each record span (each link's stats poll runs on its own ticker)
	recPhase int64
	// reo: the field's reordering of this link's connection (cfg.reoField)
	reo *wmReo

	// the link's receiving end
	rcvNxt    uint64
	ooo       [][2]uint64
	segsIn    uint64
	oooPkts   uint64
	frames    wmQ[wmFrameRec]
	rs        []*wmRcvStream
	ss        []*wmSnd
	rr        int
	ctrlQ     []wmFrameRec // pongs and replies waiting for the writer
	unreadSum int64
	claimSum  int64
	parks     uint32
	parked    bool
	rcvd      uint64
	poolFree  int64
	pool      bool
	rSrtt     float64
	rMinRTT   float64
	pongs     []time.Duration
	pongHist  []time.Duration
	ctrlMin   time.Duration
	sndRec    struct {
		at               int64
		srtt, minRTT     time.Duration
		retrans, segsOut uint64
		pendAt           int64
		pSrtt, pMin      time.Duration
		pRetrans, pSegs  uint64
	}

	// the core
	core     *wcore
	bld      wcBuilder
	coreNext int64
	turnRef  time.Duration
	qBrake   time.Duration
	growOK   bool
	noCover  bool // smux Session.SetCover(false)
	epoch    time.Time
	lastSt   wcState

	// metrics
	readMs   []uint64 // bytes read per 100 ms
	qHist    []uint64 // ms of queue, histogram
	qSum, qN float64
	inter    []float64
	at0      struct {
		segs, rtx, tail, pol, rnd uint64
		read                      uint64
		ooo, in                   uint64
	}
	res     wmResult
	ackHold []*wmPkt

	// the exit's pacing cap (cfg.paceCap, or the pacing net's): bytes/ms,
	// 0 none
	pcCap  float64
	pcDlv  []wcRate
	pcLast uint64
	netOn  bool
	net    wcNet
}

func (m *wmModel) newPkt() *wmPkt {
	if n := len(m.free); n > 0 {
		p := m.free[n-1]
		m.free = m.free[:n-1]
		*p = wmPkt{}
		return p
	}
	if len(m.slab) == 0 {
		m.slab = make([]wmPkt, 4096)
	}
	p := &m.slab[0]
	m.slab = m.slab[1:]
	return p
}

func (m *wmModel) freePkt(p *wmPkt) { m.free = append(m.free, p) }

func newModel(cfg wmCfg) *wmModel {
	if cfg.dur == 0 {
		cfg.dur = 30 * time.Second
	}
	if cfg.warm == 0 {
		cfg.warm = 6 * time.Second
	}
	if len(cfg.streams) == 0 {
		cfg.streams = []wmStreamCfg{{}}
	}
	if cfg.stage == 0 {
		cfg.stage = 1
	}
	m := &wmModel{cfg: cfg, rnd: rand.New(rand.NewPCG(cfg.seed, cfg.seed^0x5bd1e995)),
		end: cfg.dur.Milliseconds(), rtt: cfg.rtt.Milliseconds(), rate: cfg.rate / 1000,
		epoch: time.Unix(1_700_000_000, 0), growOK: true}
	m.d = m.rtt / 2
	m.bn, m.links = m, []*wmModel{m}
	maxRate := cfg.rate
	for _, c := range cfg.changes {
		maxRate = math.Max(maxRate, c.rate)
	}
	// a packet of k MSS keeps the model under ~25 packets per ms
	k := max(1, int(math.Ceil(maxRate/1000/(25*wmMSS))))
	m.unit = k * wmMSS
	m.lossUnit = 1 - math.Pow(1-cfg.loss, float64(k))
	m.qCap = int64(cfg.rate * cfg.queue.Seconds())
	m.tokens = cfg.policer * 0.02
	m.main = newFlow(m, true, m.unit)
	if cfg.compete > 0 {
		m.comp = newFlow(m, false, m.unit)
	}
	if cfg.reoField {
		m.reo = newWmReo(cfg.seed, cfg.reorderMs, cfg.reoScale)
	}
	m.pool = cfg.pool >= 0 && cfg.mode != wmBase
	m.poolFree = cfg.pool
	if cfg.pool == 0 {
		m.poolFree = 64 << 20
	}
	for i, s := range cfg.streams {
		m.rs = append(m.rs, &wmRcvStream{id: uint32(2*i + 1), read: s.read, start: s.start.Milliseconds(),
			rcvWin: wmInitWin, annWin: wmInitWin, limHigh: wmInitWin, minUnread: math.MaxInt64})
		m.ss = append(m.ss, &wmSnd{limit: wmInitWin, src: s.src, start: s.start.Milliseconds()})
	}
	// (the exit's end gets a core only with the lab knob WcoreUpload, as the
	// runner's newWcLink: without it the exit variant is base)
	if cfg.mode == wmCore && wcCoreOn(cfg.exit) {
		side := wcEdge
		if cfg.exit {
			side = wcExit
		}
		m.core = newWcore(wcConfig{side: side, stage: cfg.stage, need: wmNeed, maxWin: wmMaxWin, minWin: wmMinWin,
			noCover: !smuxCoverOn(cfg.exit), coverSeen: SmuxCoverGain > 0}, m.rnd.Float64)
		m.noCover = true // (smux Config.CoverWait: the core lets them)
	}
	// the pacing net on the exit's socket (the lab's always-on cap, when
	// set, in its place as in the runner)
	m.netOn = !cfg.netOff && !cfg.exit && PaceNet != 0 && cfg.paceCap <= 0
	m.readMs = make([]uint64, m.end/100+1)
	m.qHist = make([]uint64, 4001)
	m.res.cfg = cfg
	return m
}

// fill writes frames into the exit's socket up to wmLowat unsent bytes:
// control frames first, then the streams round-robin, each as far as its
// credit and its source allow.
func (m *wmModel) fill(f *wmFlow) {
	// tcp_rate_check_app_limited, as the app writes (tcp_sendmsg): less than
	// a packet unsent (what is in the qdisc of a TUN path leaves within
	// microseconds), not cwnd-limited and no lost packet waiting for its
	// resend. The exit's writer writes whenever its socket has less than
	// wmLowat unsent (TCP_NOTSENT_LOWAT), so a sender that has credit never
	// finds its queue empty: only one that waited for a window update does
	// (when the model refilled only at the last packet, every refill marked
	// the sender app-limited and it never left STARTUP, unlike the rig's,
	// whose base window past the knee paced at 1.2 times the path)
	empty := f.writeSeq-f.sndNxt < wmMSS && f.inflight+int64(f.unit) <= f.cwnd && f.rtxQ.len() == 0
	from := f.writeSeq
	defer func() {
		if empty && f.writeSeq > from {
			f.appLimUntil = max(f.delivered+uint64(f.inflight), 1)
		}
	}()
	for f.writeSeq-f.sndNxt < wmLowat {
		if len(m.ctrlQ) > 0 {
			c := m.ctrlQ[0]
			m.ctrlQ = m.ctrlQ[1:]
			f.writeSeq += uint64(c.n)
			c.end = f.writeSeq
			m.frames.push(c)
			continue
		}
		wrote := false
		for k := range m.ss {
			i := (m.rr + k) % len(m.ss)
			s := m.ss[i]
			if m.t < s.start {
				continue
			}
			credit := int64(s.limit) - int64(s.written)
			src := int64(wmFrame)
			if s.src > 0 {
				src = int64(s.src*float64(m.t-s.start)/1000) - int64(s.written)
			}
			if credit <= 0 && src > 0 {
				s.blocked = true
			}
			n := min(credit, src, wmFrame)
			if n <= 0 {
				continue
			}
			s.written += uint64(n)
			f.writeSeq += uint64(n)
			m.frames.push(wmFrameRec{end: f.writeSeq, n: int(n), sid: i})
			m.rr = i + 1
			wrote = true
			break
		}
		if !wrote {
			return
		}
	}
}

func (m *wmModel) enqueue(p *wmPkt) {
	segs := p.f.segs(p.n)
	if p.f.main && m.cfg.policer > 0 {
		if m.tokens < float64(p.n) {
			m.polDrops += segs
			return
		}
		m.tokens -= float64(p.n)
	}
	b := m.bn // (the shared bottleneck's, with several links)
	if b.qBytes+wmWire(p.n) > b.qCap {
		if p.f.main {
			m.tailDrops += segs
		}
		return
	}
	p.inNet = true
	b.q.push(p)
	b.qBytes += wmWire(p.n)
}

func (m *wmModel) serve() {
	m.serveCred += m.rate
	for m.q.len() > 0 && m.serveCred >= float64(wmWire(m.q.peek().n)) {
		p := m.q.pop()
		m.qBytes -= wmWire(p.n)
		m.serveCred -= float64(wmWire(p.n))
		if m.cfg.loss > 0 && m.rnd.Float64() < m.lossUnit {
			p.inNet = false
			p.rnd = true
			if p.f.main {
				p.f.m.randDrops++
			}
			continue
		}
		p.arrive = m.t + m.d
		if lm := p.f.m; p.f.main && lm.reo != nil {
			// (its own link's episodes: with several links on one
			// bottleneck each connection runs at its own rate)
			if d := lm.reo.hold(p.n); d > 0 {
				p.arrive += d
				m.late = append(m.late, p)
				continue
			}
		}
		if m.cfg.reorder > 0 && p.f.main && m.rnd.Float64() < m.cfg.reorder {
			p.arrive += int64(m.cfg.reorderMs)
			m.late = append(m.late, p)
			continue
		}
		m.line.push(p)
	}
	if m.q.len() == 0 && m.serveCred > m.rate {
		m.serveCred = m.rate
	}
	for _, l := range m.links {
		if l.reo != nil {
			l.reo.tick(m.t)
		}
	}
}

// arrive takes what reaches the receiving ends this ms.
func (m *wmModel) arrive() {
	if m.cfg.stall > 0 {
		if m.t >= m.fwdWake && m.rnd.Float64() < m.cfg.stall {
			m.fwdWake = m.t + 1 + int64(m.rnd.IntN(m.cfg.stallMax))
		}
		if m.t < m.fwdWake {
			return
		}
	}
	due := m.due[:0]
	defer func() { clear(due); m.due = due[:0] }()
	for m.line.len() > 0 && m.line.peek().arrive <= m.t {
		due = append(due, m.line.pop())
	}
	if len(m.late) > 0 {
		k := 0
		for _, p := range m.late {
			if p.arrive <= m.t {
				due = append(due, p)
			} else {
				m.late[k] = p
				k++
			}
		}
		m.late = m.late[:k]
	}
	for _, p := range due {
		p.inNet = false
		f := p.f
		// (with RACK a first copy marked lost is acked too: the sender
		// learns that it was reordering, onAcks)
		if !p.lost || f.rack && !p.rtx {
			f.ackBatch = append(f.ackBatch, p)
		}
		if !f.main {
			f.gotAll += uint64(p.n)
			continue
		}
		lm := f.m // the link it belongs to (m itself, but on a shared bottleneck)
		segs := f.segs(p.n)
		lm.segsIn += segs
		s, e := p.seq, p.seq+uint64(p.n)
		switch {
		case e <= lm.rcvNxt:
		case s > lm.rcvNxt:
			lm.oooPkts += segs
			lm.oooAdd(s, e)
		default:
			lm.rcvNxt = e
			for len(lm.ooo) > 0 && lm.ooo[0][0] <= lm.rcvNxt {
				lm.rcvNxt = max(lm.rcvNxt, lm.ooo[0][1])
				lm.ooo = lm.ooo[1:]
			}
		}
	}
	for _, l := range m.links {
		for _, f := range []*wmFlow{l.main, l.comp} {
			if f == nil || len(f.ackBatch) == 0 {
				continue
			}
			if m.cfg.ackAgg > 1 && m.t%int64(m.cfg.ackAgg) != 0 {
				continue
			}
			l.rev.push(wmRev{at: m.t + l.d, kind: 0, f: f, pkts: f.ackBatch})
			f.ackBatch = nil
		}
	}
}

func (m *wmModel) oooAdd(s, e uint64) {
	n := len(m.ooo)
	if n > 0 && m.ooo[n-1][1] == s {
		m.ooo[n-1][1] = e
		return
	}
	if n == 0 || m.ooo[n-1][1] < s {
		m.ooo = append(m.ooo, [2]uint64{s, e})
		return
	}
	i := sort.Search(n, func(i int) bool { return m.ooo[i][0] > s })
	m.ooo = append(m.ooo, [2]uint64{})
	copy(m.ooo[i+1:], m.ooo[i:])
	m.ooo[i] = [2]uint64{s, e}
	// merge neighbours
	out := m.ooo[:0]
	for _, r := range m.ooo {
		if k := len(out); k > 0 && out[k-1][1] >= r[0] {
			out[k-1][1] = max(out[k-1][1], r[1])
			continue
		}
		out = append(out, r)
	}
	m.ooo = out
}

// demux hands the frames that are wholly in order to the session.
func (m *wmModel) demux() {
	for m.frames.len() > 0 {
		r := m.frames.peek()
		if r.end > m.rcvNxt {
			return
		}
		if r.kind == 0 {
			if m.unreadSum+int64(r.n) > wmBucket+m.claimSum {
				if !m.parked {
					m.parks++
					m.parked = true
				}
				return
			}
			m.parked = false
			st := m.rs[r.sid]
			m.push(st, int64(r.n))
			if st.read == 0 && m.t >= st.start {
				// a reader that keeps up takes each frame as the
				// session's reader pushes it (its own goroutine)
				m.consumed(r.sid, st, int64(r.n))
			}
		} else {
			d := time.Duration(m.t-r.t0) * time.Millisecond
			if r.kind == 1 {
				m.pongs = append(m.pongs, d)
				m.pongHist = append(m.pongHist, d)
				if len(m.pongHist) > 8 {
					m.pongHist = m.pongHist[1:]
				}
				m.ctrlMin = m.pongHist[0]
				for _, v := range m.pongHist {
					m.ctrlMin = min(m.ctrlMin, v)
				}
			} else if m.t >= m.cfg.warm.Milliseconds() {
				m.inter = append(m.inter, float64(m.t-r.t0))
			}
		}
		m.rcvd += uint64(r.n)
		m.frames.pop()
	}
}

// push is smux's pushBytes: the turnaround samples and the hit test.
func (m *wmModel) push(st *wmRcvStream, n int64) {
	st.unread += n
	m.unreadSum += n
	got := st.numRead + uint64(st.unread)
	for st.ringN > 0 {
		e := &st.ring[st.ringHead]
		if e.aAt == 0 && got+wmFrame >= e.lPrev {
			e.aAt = m.t
		}
		if got <= e.lPrev {
			break
		}
		s := time.Duration(m.t-e.tUpd) * time.Millisecond
		if smux.CoverPause(s, st.lastTurn) {
			st.pause = max(st.pause, m.t-e.aAt) // (smux turned: the cover's pause)
		}
		st.turnLast, st.lastTurn = s, s
		if st.turnMin == 0 || s < st.turnMin {
			st.turnMin = s
		}
		if m.t-e.aAt >= max(wmHitGap, st.hitGap) {
			st.hitCycle = true
		}
		st.ringHead = (st.ringHead + 1) % len(st.ring)
		st.ringN--
	}
}

// read runs the readers.
func (m *wmModel) read() {
	for i, st := range m.rs {
		if m.t < st.start || st.unread == 0 {
			if st.read > 0 {
				st.readCred = math.Min(st.readCred+st.read/1000, st.read/50+wmReadChunk)
			}
			continue
		}
		budget := st.unread
		if st.read > 0 {
			st.readCred = math.Min(st.readCred+st.read/1000, st.read/50+wmReadChunk)
			budget = min(budget, int64(st.readCred))
			st.readCred -= float64(budget)
		}
		for budget > 0 {
			c := min(budget, wmReadChunk)
			budget -= c
			m.consumed(i, st, c)
		}
	}
}

func (m *wmModel) consumed(i int, st *wmRcvStream, n int64) {
	st.numRead += uint64(n)
	st.incr += uint64(n)
	st.unread -= n
	m.unreadSum -= n
	if st.unread < st.minUnread {
		st.minUnread = st.unread
	}
	if b := m.t / 100; b < int64(len(m.readMs)) {
		m.readMs[b] += uint64(n)
	}
	first := st.numRead == uint64(n)
	step := uint64(st.rcvWin / 2)
	if m.cfg.mode == wmFin || m.cfg.mode == wmCore || m.cfg.mode == wmCover {
		step = uint64(smux.StepOf(wmNeed, st.rcvWin))
	}
	if !first && st.incr < step {
		return
	}
	m.update(i, st, first)
}

// update sends a window update, resizing the window first (adaptWindow and
// the mode's own rule).
func (m *wmModel) update(i int, st *wmRcvStream, first bool) {
	old := st.rcvWin
	if !first {
		parked := m.parks != st.lastParks
		var w uint32
		switch m.cfg.mode {
		case wmBase:
			w = m.adapt(st, parked, wmNeed)
		case wmN4, wmFin:
			w = m.adapt(st, parked, wmMaxWin)
			if w > wmNeed {
				lim := uint32(wmNeed)
				if nd := m.paceNeed(st, old); nd > uint64(lim) {
					lim = uint32(min(nd, wmMaxWin))
				}
				keep := old / 2
				if m.cfg.mode == wmFin {
					keep = old - smux.StepOf(wmNeed, old)
				}
				lim = max(lim, keep)
				w = min(w, lim)
			}
		case wmCore:
			// (adaptWindow runs to MaxStreamBuffer, and FitCore holds it to
			// the ceiling or the stream's own window: its cover only with
			// the covers on, HS2_TUNE_SMUX_COVER)
			capW := uint32(wmNeed)
			if m.pool && smuxCoverOn(m.cfg.exit) {
				capW = wmCoverWin
			}
			w = m.adapt(st, parked, max(capW, st.ceil))
			turnOK := m.turnRef == 0 || st.lastTurn <= m.turnRef+m.qBrake
			own := m.cover(st, old, w, m.growOK, turnOK)
			w = smux.FitCore(old, w, st.ceil, own, wmMinWin, uint32(min(st.unread, math.MaxUint32)), m.growOK, turnOK)
		case wmCover:
			w = m.adapt(st, parked, wmCoverWin)
			own := m.cover(st, old, w, true, true)
			w = smux.FitCore(old, w, 0, own, wmMinWin, uint32(min(st.unread, math.MaxUint32)), true, true)
		}
		m.setWindow(st, w)
	}
	st.pause = 0 // (smux's announced)
	st.lastParks = m.parks
	if !first && st.minUnread > wmLagTarget/4 {
		st.lagN++
	}
	st.updN++
	if m.cfg.hitTruth {
		st.hitCycle = false
	} else if st.hitCycle {
		st.hitN++
		st.hitCycle = false
	}
	// as smux's announced: a turnaround starts only with an update that
	// raises the highest limit the peer had, measured from that limit (with
	// the previous update's limit, an update after a window came down timed
	// data the peer had been allowed long before: turnarounds of half a round
	// trip, and turnBase with them)
	if lim := st.numRead + uint64(st.rcvWin); lim > st.limHigh {
		if st.ringN < len(st.ring) {
			st.ring[(st.ringHead+st.ringN)%len(st.ring)] = wmTurn{tUpd: m.t, lPrev: st.limHigh}
			st.ringN++
		}
		st.limHigh = lim
	}
	st.annCons, st.annWin = st.numRead, st.rcvWin
	st.incr = 0
	st.minUnread = st.unread
	if first {
		st.minUnread = math.MaxInt64
		st.firstAt, st.firstRead = m.t, st.numRead
	}
	st.paceRead, st.paceDur = st.numRead-st.updRead, m.t-st.updAt
	st.updAt, st.updRead = m.t, st.numRead
	m.rev.push(wmRev{at: m.t + m.d, kind: 1, sid: i, cons: st.numRead, win: st.rcvWin, sent: m.t})
}

// adapt is smux's adaptWindow, simplified: shrink by a standing backlog over
// the lag target, double while the reader ran dry and the session did not
// park, and at the first resize jump to what the reader took in a second.
func (m *wmModel) adapt(st *wmRcvStream, parked bool, maxW uint32) uint32 {
	w := st.rcvWin
	standing := st.minUnread
	firstResize := !st.adapted
	st.adapted = true
	switch {
	case standing > wmLagTarget && standing != math.MaxInt64:
		ex := standing - wmLagTarget
		if ex >= int64(w)-wmMinWin {
			w = wmMinWin
		} else {
			w -= uint32(ex)
		}
	case 4*min(standing, wmLagTarget) < wmLagTarget && !parked:
		w2 := uint64(w) * 2
		if firstResize {
			dt := max(m.t-st.firstAt, 50)
			if j := (st.numRead - st.firstRead) * 1000 / uint64(dt); j > w2 {
				w2 = j
			}
		}
		w = uint32(min(w2, uint64(maxW)))
	}
	return w
}

// wmCoverWin is smux's cover of the need (smux.Cover).
var wmCoverWin = smux.Cover(wmNeed, wmMaxWin)

// cover is smux's fitCore before FitCore: the stream's cover after this
// update (smux.RanUp and smux.CoverNext on the longest pause at a limit
// since the previous update), and its own window with it (Stream.ownWin).
// No pool or the exit's end (SmuxCover: the edge's only): no cover. The
// core took the covers away: none, and the one it took comes back if it
// lets covers again within coverMiss updates (smux.CoverHold: a cut taken
// back, wcore.go undoCut).
func (m *wmModel) cover(st *wmRcvStream, old, want uint32, growOK, turnOK bool) uint32 {
	if !m.pool || !smuxCoverOn(m.cfg.exit) {
		st.covered, st.miss, st.held = false, 0, 0
		return wmNeed
	}
	st.covered, st.held, _ = smux.CoverHold(st.covered, st.held, !m.noCover)
	if m.noCover {
		st.miss = 0
		return wmNeed
	}
	ms := func(v int64) time.Duration { return time.Duration(v) * time.Millisecond }
	ran := st.updAt > 0 && smux.RanUp(ms(st.pause), ms(max(wmHitGap, st.hitGap)), ms(m.t-st.updAt), st.numRead-st.updRead, wmFrame)
	if ran && !st.covered && !smux.CoverGains(SmuxCoverGain, ms(st.pause), ms(m.t-st.updAt)) {
		ran = false
	}
	st.covered, st.miss = smux.CoverNext(st.covered, st.miss, old, want, wmNeed, ran, growOK, turnOK)
	if st.covered {
		return wmCoverWin
	}
	return wmNeed
}

// paceNeed is N4's and fin's needWindow.
func (m *wmModel) paceNeed(st *wmRcvStream, old uint32) uint64 {
	rtt := m.rMinRTT
	if rtt <= 0 || st.updAt == 0 {
		return 0
	}
	dt := float64(m.t - st.updAt)
	read := float64(st.numRead - st.updRead)
	if m.cfg.mode == wmFin {
		dt += float64(st.paceDur)
		read += float64(st.paceRead)
	}
	dt = math.Max(dt, rtt/2)
	if m.cfg.mode == wmN4 {
		return uint64(3 * read * rtt / dt)
	}
	q := m.rSrtt - m.rMinRTT
	lo := math.Max(rtt/32, 2)
	hi := math.Max(rtt/16, lo+2)
	mg := 0.1
	switch {
	case q >= hi:
		mg = 0
	case q > lo:
		mg = 0.1 * (hi - q) / (hi - lo)
	}
	return uint64((1+mg)*read*rtt/dt) + uint64(smux.StepOf(wmNeed, old))
}

// setWindow is N4's: past need only as far as the pool covers it, within
// what it has left for the link (smux WindowPool.FreeFor: the link holds no
// more than the pool has left for the others).
func (m *wmModel) setWindow(st *wmRcvStream, w uint32) {
	if m.pool && w > wmNeed {
		if want := int64(w) - wmNeed - st.claim; want > 0 {
			got := min(want, max((m.poolFree-m.claimSum)/2, 0))
			m.poolFree -= got
			st.claim += got
			m.claimSum += got
			if got < want {
				w -= uint32(want - got)
				st.poolShort++
			}
		}
	}
	keep := max(int64(w), st.unread) - wmNeed
	keep = max(keep, 0)
	if st.claim > keep {
		m.poolFree += st.claim - keep
		m.claimSum -= st.claim - keep
		st.claim = keep
	}
	st.rcvWin = w
}

// atSender takes what reaches the exit this ms on the way back.
func (m *wmModel) atSender() {
	if m.cfg.stall > 0 {
		if m.t >= m.revWake && m.rnd.Float64() < m.cfg.stall {
			m.revWake = m.t + 1 + int64(m.rnd.IntN(m.cfg.stallMax))
		}
		if m.t < m.revWake {
			return
		}
	}
	for m.rev.len() > 0 && m.rev.peek().at <= m.t {
		e := m.rev.pop()
		switch e.kind {
		case 0:
			e.f.onAcks(e.pkts)
			continue
		case 1:
			s := m.ss[e.sid]
			s.limit = e.cons + uint64(e.win)
			if s.blocked {
				s.hitS++
				if m.cfg.hitTruth {
					m.rs[e.sid].hitN++
				}
			}
			s.blocked = false
		case 2:
			m.ctrlQ = append(m.ctrlQ, wmFrameRec{n: 64, kind: 1, t0: e.sent})
		case 3:
			m.ctrlQ = append(m.ctrlQ, wmFrameRec{n: 1024, kind: 2, t0: e.sent})
		}
		// the edge's segment is acked with the next data the exit sends:
		// its round trip has the download queue as it is now
		r := float64(m.t-e.sent) + float64(m.bn.qBytes)/m.bn.rate + float64(m.d)
		if m.rSrtt == 0 {
			m.rSrtt = r
		} else {
			m.rSrtt += 0.125 * (r - m.rSrtt)
		}
		if m.rMinRTT == 0 || r < m.rMinRTT {
			m.rMinRTT = r
		}
	}
}

// runCore is the runner: every 100 ms, the link is stepped when its turn
// has come.
func (m *wmModel) runCore() {
	if m.core == nil || m.t%100 != 0 || m.t < m.coreNext {
		return
	}
	now := m.epoch.Add(time.Duration(m.t) * time.Millisecond)
	s := wcLinkSnap{now: now, minRTT: time.Duration(m.rMinRTT * float64(time.Millisecond)),
		srtt: time.Duration(m.rSrtt * float64(time.Millisecond)), segsIn: m.segsIn, ooo: m.oooPkts, oooOK: true,
		rcvd: m.rcvd, parks: m.parks, poolFree: -1, ctrlMin: m.ctrlMin, pongs: m.pongs}
	if m.pool {
		s.poolFree = max((m.poolFree-m.claimSum)/2, 0) // (smux WindowPool.FreeFor)
	}
	m.pongs = nil
	if r := &m.sndRec; r.at > 0 {
		s.sndAt = m.epoch.Add(time.Duration(r.at) * time.Millisecond)
		s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = r.srtt, r.minRTT, r.retrans, r.segsOut
	}
	if m.cfg.apProbing != nil {
		s.apProbing = m.cfg.apProbing(time.Duration(m.t) * time.Millisecond)
	}
	if m.cfg.oldExit {
		s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = 0, 0, 0, 0
	}
	// the exit's stats handshake, a round trip after the link came up
	// (runStats, before the first record): what its records carry
	if !m.cfg.exit && m.t >= 2*m.d {
		s.sndExt = statsExtSender
		if m.cfg.oldExit {
			s.sndExt = statsExtNone
		}
	}
	if m.cfg.exit {
		// the exit's runner: no record of the sender, no control ping
		s.sndAt, s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = time.Time{}, 0, 0, 0, 0
		s.ctrlMin, s.pongs, s.apProbing = 0, nil, false
	}
	// Session.CoreSnapshot: the streams at need/4 or more, or with a fresh
	// hit; the turnaround samples are swapped out
	for _, st := range m.rs {
		s.poolShort += st.poolShort
		if st.rcvWin >= wmNeed/4 || st.hitN != st.snapHit {
			s.streams = append(s.streams, wcStreamSnap{id: st.id, readTot: st.numRead, updN: st.updN, hitN: st.hitN,
				lagN: st.lagN, turnMin: st.turnMin, turnLast: st.turnLast, win: st.rcvWin})
		}
		st.turnMin, st.turnLast, st.snapHit = 0, 0, st.hitN
	}
	// the runner's hit gap of each stream (wcLink.noteRates)
	for _, st := range m.rs {
		r := float64(st.numRead-st.rateRead) / 0.1
		st.rateRead = st.numRead
		if st.rateE == 0 {
			st.rateE = r
		} else {
			st.rateE += 0.3 * (r - st.rateE)
		}
		g := wcHitGapMax
		if st.rateE > 0 {
			g = min(wcHitGapMax, time.Duration(wcHitFrames*wmFrame/st.rateE*float64(time.Second)))
		}
		st.hitGap = int64((g + time.Millisecond - 1) / time.Millisecond)
	}
	in := m.bld.build(s)
	pub, drainLoss := m.core.bPub, m.core.cnt.drainLoss
	o := m.core.step(in)
	if m.cfg.check != nil {
		m.cfg.check(time.Duration(m.t)*time.Millisecond, m.core, in, o)
	}
	// the runner clears the ceiling of a stream the snapshot left out
	for _, st := range m.rs {
		st.ceil = 0
		for _, c := range o.ceils {
			if c.id == st.id {
				st.ceil = c.ceil
			}
		}
	}
	m.turnRef, m.qBrake, m.growOK, m.noCover = o.turnRef, o.qBrake, o.growOK, o.noCover
	m.coreNext = o.next.Sub(m.epoch).Milliseconds()
	for _, e := range o.events {
		if e.kind == wcEvDrain && m.lastSt == wcHold {
			m.res.holdDrains++
		}
	}
	if m.cfg.trace {
		c, lm := m.core, m.core.last
		var ss strings.Builder
		for i, x := range in.streams {
			if i > 1 {
				break
			}
			fmt.Fprintf(&ss, "[r%.0f u%d h%d l%d t%d/%d w%.1f]", float64(x.read)/lm.dt/wmMbit, x.upd, x.hit, x.lag, x.turnMin.Milliseconds(), x.turnLast.Milliseconds(), float64(x.win)/(1<<20))
		}
		f := m.main
		m.res.trace = append(m.res.trace, fmt.Sprintf("%5.1fs %-7s B%5.2f/%5.2f cap%5.2f nF%d RF%4.0f RL%4.0f hit%.2f qt%3d(ok%v) qo%3d qs%3d snd%.3f ooo%.3f | tau%3d qref%3d brake%3d grow%v | bbr%d pace%4.0f q%3.0f %s",
			float64(m.t)/1000, o.state, float64(o.B)/(1<<20), float64(c.B)/(1<<20), float64(c.bCap)/(1<<20), len(lm.f), lm.rF/wmMbit, lm.rLink/wmMbit, lm.hit, lm.qTurn.Milliseconds(), lm.qOK, lm.qOwn.Milliseconds(), lm.qSnd.Milliseconds(), lm.snd, lm.ooo,
			c.tau.Milliseconds(), c.qRef.Milliseconds(), o.qBrake.Milliseconds(), o.growOK, f.mode, f.pacing*1000/wmMbit, float64(m.bn.qBytes)/m.bn.rate, ss.String()))
		if ro := c.roLevel(in.now); ro > 0 || c.cnt.undo > 0 {
			// (the path's reordering level and the cuts for holes taken back)
			m.res.trace[len(m.res.trace)-1] += fmt.Sprintf(" ro%.2f undo%d", ro, c.cnt.undo)
		}
		for _, e := range o.events {
			m.res.trace = append(m.res.trace, "        event "+e.String())
		}
		if m.core.cnt.drainLoss > drainLoss && !wcHasDrain(o.events) && m.core.B < pub {
			// (as the runner logs a halving between evaluations, wcore.cut)
			m.res.trace = append(m.res.trace, fmt.Sprintf("        event cut from=%d b=%d (packets arrived behind holes on a clean path)", pub, m.core.B))
		}
	}
	m.lastSt = o.state
}

func ceilsMiB(c []wcCeil) string {
	var b strings.Builder
	for i, v := range c {
		if i > 3 {
			b.WriteString("…")
			break
		}
		fmt.Fprintf(&b, "%.1f ", float64(v.ceil)/(1<<20))
	}
	return b.String()
}

func winsMiB(rs []*wmRcvStream) string {
	var b strings.Builder
	for i, st := range rs {
		if i > 3 {
			b.WriteString("…")
			break
		}
		fmt.Fprintf(&b, "%.1f ", float64(st.rcvWin)/(1<<20))
	}
	return b.String()
}

func (m *wmModel) periodic() {
	t := m.t
	if t%3000 == 1500 {
		m.rev.push(wmRev{at: t + m.d, kind: 2, sent: t})
	}
	if t%100 == 37 {
		m.rev.push(wmRev{at: t + m.d, kind: 3, sent: t})
	}
	r := &m.sndRec
	// the edge polls the exit's record each health tick (2 s), the first one
	// 2 s after the link came up (TestZ5Rig's pollStats)
	every := int64(2000)
	if m.cfg.recEvery > 0 {
		every = m.cfg.recEvery.Milliseconds()
	}
	in := func(spans [][2]time.Duration) bool {
		for _, s := range spans {
			if t >= s[0].Milliseconds() && t < s[1].Milliseconds() {
				return true
			}
		}
		return false
	}
	if (t-m.recPhase)%every == 0 && t > m.recPhase && !in(m.cfg.recOff) {
		f := m.main
		r.pendAt = t + m.d
		r.pSrtt = time.Duration(f.srtt * float64(time.Millisecond))
		r.pMin = time.Duration(f.minRTT) * time.Millisecond
		r.pRetrans, r.pSegs = f.retrans, f.segsOut
		if in(m.cfg.recZero) {
			r.pSrtt, r.pMin, r.pRetrans, r.pSegs = 0, 0, 0, 0
		}
	}
	if r.pendAt > 0 && t >= r.pendAt {
		r.at, r.srtt, r.minRTT, r.retrans, r.segsOut = t, r.pSrtt, r.pMin, r.pRetrans, r.pSegs
		r.pendAt = 0
	}
	if m.cfg.policer > 0 {
		m.tokens = math.Min(m.tokens+m.cfg.policer/1000, math.Max(m.cfg.policer*0.02, 64<<10))
	}
	if m.cfg.paceCap > 0 && t%100 == 0 {
		m.paceCapStep()
	}
	if m.netOn && t%100 == 0 {
		m.netStep()
	}
}

// netStep is the exit's runner wake for the pacing net (wcLink.paceNet), on
// the model's sender: its TCP_INFO as Linux reports it (the pacing rate
// within the cap, the delivery rate of tcp_rate_gen).
func (m *wmModel) netStep() {
	f := m.main
	pace := f.pacing
	if m.pcCap > 0 {
		pace = math.Min(pace, m.pcCap)
		m.res.netMs += 100
	}
	now := m.epoch.Add(time.Duration(m.t) * time.Millisecond)
	o := m.net.step(wcNetIn{now: now, retrans: f.retrans, segsOut: f.segsOut, acked: f.delivered, dlv: f.rateDlv * 1000, pace: pace * 1000,
		srtt: time.Duration(f.srtt * float64(time.Millisecond)), minRTT: time.Duration(f.minRTT) * time.Millisecond})
	switch o.act {
	case wcNetEngage:
		m.res.netN++
		m.pcCap = float64(o.cap) / 1000
	case wcNetMoveCap:
		m.pcCap = float64(o.cap) / 1000
	case wcNetRelease:
		m.pcCap = 0
	}
	if m.cfg.trace && o.act != wcNetNone {
		m.res.trace = append(m.res.trace, fmt.Sprintf("        net %5.1fs act%d cap %.0f Mbit share %.1f%% ratio %.1f dlv %.0f idle %v",
			float64(m.t)/1000, o.act, float64(o.cap)/wmMbit, 100*o.share, o.ratio, o.dlv/wmMbit, o.idle))
	}
}

// paceCapStep is the exit's runner wake for the pacing cap (wcLink.paceCap,
// wcPaceRate), on the model's sender: its delivery rate as what it delivered
// since the previous wake.
func (m *wmModel) paceCapStep() {
	f := m.main
	acked := f.delivered - m.pcLast
	m.pcLast = f.delivered
	now := m.epoch.Add(time.Duration(m.t) * time.Millisecond)
	if acked < wcPaceIdle {
		m.pcCap, m.pcDlv = 0, m.pcDlv[:0]
		return
	}
	m.pcDlv = append(m.pcDlv, wcRate{now, float64(acked) * 10}) // bytes/s
	cut := 0
	for cut < len(m.pcDlv)-1 && now.Sub(m.pcDlv[cut].at) > wcPaceSpan {
		cut++
	}
	m.pcDlv = m.pcDlv[cut:]
	r := 0.0
	for _, x := range m.pcDlv {
		r = math.Max(r, x.r)
	}
	win := uint64(0)
	for _, st := range m.rs {
		if m.t >= st.start {
			win += uint64(st.annWin)
		}
	}
	// (wcPaceRate with the model's own factor)
	v := m.cfg.paceCap * r
	minRTT, srtt := time.Duration(f.minRTT)*time.Millisecond, time.Duration(f.srtt*float64(time.Millisecond))
	if minRTT > 0 && srtt > 0 && srtt-minRTT <= max(wcDqMin, minRTT/10) {
		v = math.Max(v, float64(win)/minRTT.Seconds())
	}
	m.pcCap = math.Max(v, wcPaceFloor) / 1000
}

func (m *wmModel) run() wmResult {
	warm := m.cfg.warm.Milliseconds()
	ci, li, ri, pi := 0, 0, 0, 0
	for m.t = 0; m.t < m.end; m.t++ {
		for li < len(m.cfg.lossAt) && m.t >= m.cfg.lossAt[li].at.Milliseconds() {
			m.cfg.loss = m.cfg.lossAt[li].rate
			m.lossUnit = 1 - math.Pow(1-m.cfg.loss, float64(m.unit/wmMSS))
			li++
		}
		for ri < len(m.cfg.rttAt) && m.t >= m.cfg.rttAt[ri].at.Milliseconds() {
			// (packets on the line keep their arrival; the line and the
			// way back are FIFO: a shorter delay does not reorder them)
			m.d = int64(m.cfg.rttAt[ri].rate) / 2
			ri++
		}
		for pi < len(m.cfg.polAt) && m.t >= m.cfg.polAt[pi].at.Milliseconds() {
			if m.cfg.policer <= 0 && m.cfg.polAt[pi].rate > 0 {
				m.tokens = m.cfg.polAt[pi].rate * 0.02
			}
			m.cfg.policer = m.cfg.polAt[pi].rate
			pi++
		}
		if m.t%1000 == 0 {
			m.res.rtxSec = append(m.res.rtxSec, [2]uint64{m.main.retrans, m.main.segsOut})
		}
		if m.t%500 == 0 {
			m.res.oooHalf = append(m.res.oooHalf, [2]uint64{m.oooPkts, m.segsIn})
		}
		for ci < len(m.cfg.changes) && m.t >= m.cfg.changes[ci].at.Milliseconds() {
			// (the rig's emulator sets the queue to the same time at the
			// new rate)
			m.rate = m.cfg.changes[ci].rate / 1000
			m.qCap = int64(m.cfg.changes[ci].rate * m.cfg.queue.Seconds())
			ci++
		}
		if m.t == warm {
			m.at0.segs, m.at0.rtx = m.main.segsOut, m.main.retrans
			m.at0.tail, m.at0.pol, m.at0.rnd = m.tailDrops, m.polDrops, m.randDrops
			m.at0.ooo, m.at0.in = m.oooPkts, m.segsIn
			if m.comp != nil {
				m.at0.read = m.comp.gotAll
			}
		}
		m.atSender()
		m.main.tailTimer()
		m.main.send()
		if m.comp != nil && m.t >= m.cfg.compete.Milliseconds() {
			m.comp.tailTimer()
			m.comp.send()
		}
		m.serve()
		m.arrive()
		m.demux()
		m.read()
		m.demux()
		m.periodic()
		m.runCore()
		if m.cfg.trace && m.core == nil && m.t%100 == 99 {
			f := m.main
			m.res.trace = append(m.res.trace, fmt.Sprintf("%5.1fs %4.0f Mbit bbr%d full%v pace%5.0f cwnd%6.0fK infl%6.0fK minrtt%d q%3.0f rtx%d wins %s",
				float64(m.t)/1000, float64(m.readMs[m.t/100])*10/wmMbit, f.mode, f.full, f.pacing*1000/wmMbit, float64(f.cwnd)/1024, float64(f.inflight)/1024,
				f.bMinUs, float64(m.qBytes)/m.rate, f.retrans, winsMiB(m.rs)))
		}
		if m.t >= warm {
			qms := float64(m.qBytes) / m.rate
			m.qSum += qms
			m.qN++
			m.qHist[min(int(qms), len(m.qHist)-1)]++
		}
	}
	return m.result()
}

func (m *wmModel) result() wmResult {
	r := &m.res
	cfg := m.cfg
	warm := cfg.warm.Milliseconds()
	f := m.main
	var read uint64
	for b := warm / 100; b < int64(len(m.readMs)); b++ {
		read += m.readMs[b]
	}
	secs := float64(m.end-warm) / 1000
	r.mbit = float64(read) / secs / wmMbit
	if segs := f.segsOut - m.at0.segs; segs > 0 {
		r.resend = float64(f.retrans-m.at0.rtx) / float64(segs)
		r.selfDrop = float64(m.tailDrops+m.polDrops-m.at0.tail-m.at0.pol) / float64(segs)
		r.randDrop = float64(m.randDrops-m.at0.rnd) / float64(segs)
	}
	if f.segsOut > 0 {
		r.resendAll = float64(f.retrans) / float64(f.segsOut)
	}
	if in := m.segsIn - m.at0.in; in > 0 {
		r.ooo = float64(m.oooPkts-m.at0.ooo) / float64(in)
	}
	r.spurious = f.spurious
	if m.reo != nil {
		r.reoEps = m.reo.eps
	}
	if m.qN > 0 {
		r.qMean = m.qSum / m.qN
		r.q99 = histPct(m.qHist, 0.99)
	}
	if len(m.inter) > 0 {
		sort.Float64s(m.inter)
		r.iP50 = m.inter[len(m.inter)/2]
		r.iP99 = m.inter[min(len(m.inter)-1, len(m.inter)*99/100)]
	}
	r.reach = m.reach(cfg.rate)
	r.t90 = m.t90(0, r.reach)
	if m.core != nil {
		c := m.core.cnt
		r.drains = [3]int{c.drainQ, c.drainLoss, c.drainRate}
		r.probes = [2]int{c.probeOK, c.probeFail}
		r.peakB = c.peakB
		r.lastState = m.core.state
		r.cuts = m.core.cuts
	}
	if m.comp != nil {
		r.compMbit = float64(m.comp.gotAll-m.at0.read) / secs / wmMbit
	}
	r.probeRTTs = f.probeRTTs
	var upd, got uint64
	for _, st := range m.rs {
		if st.numRead > wmNeed && st.updN > 1 {
			upd += uint64(st.updN - 1)
			got += st.numRead
		}
	}
	if got > 0 {
		r.updMiB = float64(upd) / (float64(got) / (1 << 20))
	}
	if len(cfg.changes) > 0 {
		c0 := cfg.changes[0].at.Milliseconds()
		var pre uint64
		for b := warm / 100; b < c0/100 && b < int64(len(m.readMs)); b++ {
			pre += m.readMs[b]
		}
		preMbit := float64(pre) / (float64(c0-warm) / 1000) / wmMbit
		r.back90 = m.t90(cfg.changes[len(cfg.changes)-1].at.Milliseconds(), preMbit)
	}
	return *r
}

// reach is what the link can carry at rate: the path (or the policer), and
// what the streams' largest windows let through in a round trip.
func (m *wmModel) reach(rate float64) float64 {
	cfg := m.cfg
	r := rate * wmEff
	if cfg.policer > 0 {
		r = math.Min(r, cfg.policer)
	}
	w := float64(wmMaxWin - smux.StepOf(wmNeed, wmMaxWin)/2)
	switch cfg.mode {
	case wmBase:
		w = float64(wmNeed - wmNeed/4)
	case wmCover:
		w = float64(wmCoverWin - smux.StepOf(wmNeed, wmCoverWin)/2)
	}
	win := 0.0
	for _, s := range cfg.streams {
		if s.read > 0 {
			win += s.read
		} else if s.src > 0 {
			win += s.src
		} else {
			win += w / cfg.rtt.Seconds()
		}
	}
	return math.Min(r, win) / wmMbit
}

// t90 is how long after from the reading pace (1 s average) first reached
// 90% of target Mbit/s (-1: never).
func (m *wmModel) t90(from int64, target float64) time.Duration {
	sum := uint64(0)
	b0 := from / 100
	for b := b0; b < int64(len(m.readMs)); b++ {
		sum += m.readMs[b]
		if b-b0 >= 10 {
			sum -= m.readMs[b-10]
		}
		if b-b0 >= 9 && float64(sum)/wmMbit >= 0.9*target {
			return time.Duration(b+1-b0) * 100 * time.Millisecond
		}
	}
	return -1
}

func histPct(h []uint64, p float64) float64 {
	tot := uint64(0)
	for _, v := range h {
		tot += v
	}
	want := uint64(p * float64(tot))
	acc := uint64(0)
	for i, v := range h {
		acc += v
		if acc >= want && acc > 0 {
			return float64(i)
		}
	}
	return 0
}

func wmRun(cfg wmCfg) wmResult { return newModel(cfg).run() }

// wmRunN runs cfg with n seeds from cfg.seed on and keeps each measure's
// median. The model is chaotic as the rig is (a loss a millisecond earlier
// or later sends a run another way): one seed is one draw, like one run of
// the rig, and the rig's tables are medians of three.
func wmRunN(cfg wmCfg, n int) wmResult {
	if n <= 1 {
		return wmRun(cfg)
	}
	rs := make([]wmResult, n)
	for i := range rs {
		c := cfg
		c.seed = cfg.seed + uint64(i)
		rs[i] = wmRun(c)
	}
	medF := func(get func(*wmResult) float64) float64 {
		v := make([]float64, n)
		for i := range rs {
			v[i] = get(&rs[i])
		}
		sort.Float64s(v)
		return v[n/2]
	}
	medD := func(get func(*wmResult) time.Duration) time.Duration {
		v := medF(func(r *wmResult) float64 {
			if d := get(r); d >= 0 {
				return float64(d)
			}
			return math.Inf(1) // never
		})
		if math.IsInf(v, 1) {
			return -1
		}
		return time.Duration(v)
	}
	out := rs[0]
	out.mbit = medF(func(r *wmResult) float64 { return r.mbit })
	out.resend = medF(func(r *wmResult) float64 { return r.resend })
	out.resendAll = medF(func(r *wmResult) float64 { return r.resendAll })
	out.selfDrop = medF(func(r *wmResult) float64 { return r.selfDrop })
	out.randDrop = medF(func(r *wmResult) float64 { return r.randDrop })
	out.qMean = medF(func(r *wmResult) float64 { return r.qMean })
	out.q99 = medF(func(r *wmResult) float64 { return r.q99 })
	out.iP50 = medF(func(r *wmResult) float64 { return r.iP50 })
	out.iP99 = medF(func(r *wmResult) float64 { return r.iP99 })
	out.compMbit = medF(func(r *wmResult) float64 { return r.compMbit })
	out.t90 = medD(func(r *wmResult) time.Duration { return r.t90 })
	out.back90 = medD(func(r *wmResult) time.Duration { return r.back90 })
	for k := range out.drains {
		out.drains[k] = int(medF(func(r *wmResult) float64 { return float64(r.drains[k]) }))
	}
	for k := range out.probes {
		out.probes[k] = int(medF(func(r *wmResult) float64 { return float64(r.probes[k]) }))
	}
	out.holdDrains = int(medF(func(r *wmResult) float64 { return float64(r.holdDrains) }))
	out.probeRTTs = int(medF(func(r *wmResult) float64 { return float64(r.probeRTTs) }))
	out.peakB = uint64(medF(func(r *wmResult) float64 { return float64(r.peakB) }))
	out.netN = int(medF(func(r *wmResult) float64 { return float64(r.netN) }))
	out.netMs = int64(medF(func(r *wmResult) float64 { return float64(r.netMs) }))
	out.ooo = medF(func(r *wmResult) float64 { return r.ooo })
	out.spurious = uint64(medF(func(r *wmResult) float64 { return float64(r.spurious) }))
	out.cuts = int(medF(func(r *wmResult) float64 { return float64(r.cuts) }))
	out.updMiB = medF(func(r *wmResult) float64 { return r.updMiB })
	return out
}

// ---- validation: the model reproduces the rig ----------------------------------------

func wmPath(rateMbit float64, rtt, queue time.Duration) wmCfg {
	return wmCfg{name: fmt.Sprintf("%.0f/%d q%d", rateMbit, rtt.Milliseconds(), queue.Milliseconds()),
		rate: rateMbit * wmMbit, rtt: rtt, queue: queue, seed: 1}
}

func (c wmCfg) with(mode wmMode) wmCfg { c.mode = mode; return c }

// TestWcoreModelReproducesRig: the model must show the failures the rig
// measured before it is trusted with the core (§9.1), each as the median of
// five seeds. N4 (3·pace·min_rtt) on 300/150 behind a 50 ms queue: the
// window-limited sender stays in BBR's STARTUP, sends each window update's
// credit at 3.9 times the path's rate, and the queue overflows for as long as
// it runs (rig: 15.5% resent, interactive p99 889 ms against 167); fin
// ((1+m)·pace·min_rtt + step) under 0.1% random loss on 1000/100 stays far
// below N4 (rig: 97.5 against 293–308, base 77); the 2 MiB window lands near
// what the rig measured (70.8–77 on 300/150, 110–122 on 1000/100).
func TestWcoreModelReproducesRig(t *testing.T) {
	const seeds = 5
	// (the rig measured these without the exit's pacing net)
	p300 := wmPath(300, 150*time.Millisecond, 50*time.Millisecond)
	l1000 := wmPath(1000, 100*time.Millisecond, 50*time.Millisecond)
	l1000.loss, l1000.name = 0.001, "1000/100 q50 l0.1"
	p1000 := wmPath(1000, 100*time.Millisecond, 50*time.Millisecond)
	p300.netOff, l1000.netOff, p1000.netOff = true, true, true
	base3, n43 := wmRunN(p300.with(wmBase), seeds), wmRunN(p300.with(wmN4), seeds)
	fin1, n41, base1 := wmRunN(l1000.with(wmFin), seeds), wmRunN(l1000.with(wmN4), seeds), wmRunN(l1000.with(wmBase), seeds)
	b1000 := wmRunN(p1000.with(wmBase), seeds)
	for _, r := range []wmResult{base3, n43, base1, n41, fin1, b1000} {
		t.Log(r)
	}
	if n43.resend < 0.08 || n43.resend > 0.35 || n43.iP99 < 2*base3.iP99 {
		t.Errorf("N4 on 300/150 q50: resent %.1f%%, interactive p99 %.0f ms (base %.0f): the model lost N4's overflow",
			100*n43.resend, n43.iP99, base3.iP99)
	}
	if fin1.mbit > 0.6*n41.mbit || n41.mbit < 2*base1.mbit {
		t.Errorf("0.1%% loss on 1000/100: fin %.0f, n4 %.0f, base %.0f Mbit/s: the model lost fin's collapse", fin1.mbit, n41.mbit, base1.mbit)
	}
	if base3.mbit < 60 || base3.mbit > 115 {
		t.Errorf("base on 300/150: %.0f Mbit/s, want ~70–110", base3.mbit)
	}
	if b1000.mbit < 80 || b1000.mbit > 160 {
		t.Errorf("base on 1000/100: %.0f Mbit/s, want ~100–150", b1000.mbit)
	}
}

// ---- the scenario table (§9.1) and the acceptance targets (§9.4) ------------

type wmKind uint8

const (
	wmGain    wmKind = iota // one download: the gain over base, its cost
	wmLossy                 // random loss: ≥ 60% of N4's gain at base + 0.5 points resent
	wmMulti                 // several downloads on one link: resent ≤ base
	wmSlow                  // a slow reader next to the download
	wmChangeK               // 300 → 50 → 300: 90% of the new rate in ≤ 10 s
	wmPolice                // a per-connection policer
	wmCompete               // an outside BBR flow on the same bottleneck
)

type wmCell struct {
	cfg  wmCfg
	kind wmKind
}

func wmMS(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// wmCells is the table: the default run keeps a representative subset (each
// cell three model runs of 30–60 s, well under a second of CPU each), all
// of it with HS2_WCORE_MODEL_FULL=1.
func wmCells(full bool) []wmCell {
	var cs []wmCell
	add := func(rate float64, rtt, q int, loss float64, k wmKind, mod func(*wmCfg)) {
		c := wmPath(rate, wmMS(rtt), wmMS(q))
		c.loss = loss / 100
		if loss > 0 {
			c.name += fmt.Sprintf(" l%g", loss)
		}
		if mod != nil {
			mod(&c)
		}
		cs = append(cs, wmCell{c, k})
	}
	if full {
		for _, rate := range []float64{100, 300, 1000} {
			for _, rtt := range []int{50, 100, 150, 300, 500} {
				for _, q := range []int{50, 200} {
					add(rate, rtt, q, 0, wmGain, nil)
				}
				for _, loss := range []float64{0.1, 1, 3} {
					add(rate, rtt, 50, loss, wmLossy, nil)
				}
			}
		}
	} else {
		for _, p := range [][2]int{{300, 150}, {1000, 100}, {1000, 150}, {300, 100}, {100, 100}, {100, 50}, {1000, 500}} {
			add(float64(p[0]), p[1], 50, 0, wmGain, nil)
		}
		add(300, 150, 200, 0, wmGain, nil)
		add(1000, 100, 50, 0.1, wmLossy, nil)
		add(300, 150, 50, 0.1, wmLossy, nil)
		add(1000, 100, 50, 1, wmLossy, nil)
	}
	multi := func(n int) func(*wmCfg) {
		return func(c *wmCfg) {
			c.streams = make([]wmStreamCfg, n)
			c.name += fmt.Sprintf(" x%d", n)
		}
	}
	add(300, 150, 50, 0, wmMulti, multi(4))
	add(1000, 100, 50, 0, wmMulti, multi(4))
	add(300, 150, 50, 0, wmMulti, multi(16))
	add(300, 150, 50, 0, wmSlow, func(c *wmCfg) {
		c.streams = []wmStreamCfg{{}, {read: 2 * wmMbit}}
		c.name += " +slow reader"
	})
	add(300, 150, 50, 0, wmChangeK, func(c *wmCfg) {
		// as the rig: 50 Mbit/s from 18 s for 12 s (the queue 50 ms at
		// each rate), measured to 42 s
		c.changes = []wmChange{{18 * time.Second, 50 * wmMbit}, {30 * time.Second, 300 * wmMbit}}
		c.dur = 42 * time.Second
		c.name += " 300>50>300"
	})
	add(1000, 100, 50, 0, wmPolice, func(c *wmCfg) {
		c.policer = 300 * wmMbit
		c.name += " policer 300"
	})
	add(300, 150, 200, 0, wmCompete, func(c *wmCfg) {
		c.compete = 15 * time.Second
		c.name += " +bbr at 15s"
	})
	if full {
		add(1000, 150, 50, 0, wmMulti, multi(16))
		add(100, 100, 50, 0, wmChangeK, func(c *wmCfg) {
			c.changes = []wmChange{{20 * time.Second, 20 * wmMbit}, {40 * time.Second, 100 * wmMbit}}
			c.dur = 60 * time.Second
			c.name += " 100>20>100"
		})
		add(300, 150, 50, 0.1, wmMulti, multi(4))
	}
	return cs
}

// wmVerdict is a cell's three runs against the targets.
type wmVerdict struct {
	cell           wmCell
	base, n4, core wmResult
	fails          []string
	cpu            time.Duration
}

// wmRace: built with -race (wcore_race_test.go).
var wmRace bool

// wmSeeds: each cell of the table is the median of this many seeds.
const wmSeeds = 3

func wmJudge(c wmCell) wmVerdict {
	v := wmVerdict{cell: c}
	v.base = wmRunN(c.cfg.with(wmBase), wmSeeds)
	v.n4 = wmRunN(c.cfg.with(wmN4), wmSeeds)
	t1 := time.Now()
	v.core = wmRunN(c.cfg.with(wmCore), wmSeeds)
	v.cpu = time.Since(t1) / wmSeeds
	b, n, k := v.base, v.n4, v.core
	fail := func(f string, a ...any) { v.fails = append(v.fails, fmt.Sprintf(f, a...)) }
	gainN4 := n.mbit - b.mbit
	share := 0.8
	if c.kind == wmLossy {
		share = 0.6
	}
	// where the 2 MiB window already overflows the path, N4's gain comes
	// with its resends: there the target is "not slower than base" (§9.4,
	// 100/100)
	over := b.selfDrop > 0.01
	switch {
	case c.kind == wmChangeK || c.kind == wmCompete:
	case gainN4 > 0.1*b.mbit && c.kind != wmMulti && !over:
		if k.mbit-b.mbit < share*gainN4 {
			fail("gain %.0f%% of N4's", 100*(k.mbit-b.mbit)/gainN4)
		}
	default:
		if k.mbit < 0.95*b.mbit {
			fail("slower than base (%.0f < %.0f)", k.mbit, b.mbit)
		}
	}
	// resent: + 0.5 point where base is clean (and under random loss), + 3
	// points elsewhere; never more than base with several downloads
	lim := b.resend + 0.03
	if (b.resend-b.randDrop < 0.005 || c.kind == wmLossy) && c.kind != wmChangeK {
		lim = b.resend + 0.005
	}
	if c.kind == wmMulti {
		lim = b.resend + 0.002
	}
	if k.resend > lim {
		fail("resent %.2f%% (base %.2f%%)", 100*k.resend, 100*b.resend)
	}
	// the interactive user next to it: p50 and p99 within 1.3× base; where
	// base itself overflows, no worse than base
	if over {
		if k.iP50 > 1.05*b.iP50+10 || k.iP99 > 1.05*b.iP99+10 {
			fail("interactive %.0f/%.0f ms (base %.0f/%.0f)", k.iP50, k.iP99, b.iP50, b.iP99)
		}
	} else if c.kind != wmChangeK && c.kind != wmCompete && (k.iP50 > 1.3*b.iP50 || k.iP99 > 1.3*b.iP99) {
		fail("interactive %.0f/%.0f ms (base %.0f/%.0f)", k.iP50, k.iP99, b.iP50, b.iP99)
	}
	// the queue the core keeps: a standing qTarget (15 ms) plus what probes
	// and the sender's bursts add
	if !over && c.kind != wmChangeK && c.kind != wmCompete && (k.qMean > 20 || k.q99 > math.Max(45, b.q99)) {
		fail("queue %.0f/%.0f ms", k.qMean, k.q99)
	}
	// a static, clean path: no drain once HOLD was reached
	if c.cfg.loss == 0 && len(c.cfg.changes) == 0 && c.cfg.compete == 0 && c.cfg.policer == 0 && !over && k.holdDrains > 0 {
		fail("%d drain(s) from HOLD", k.holdDrains)
	}
	if c.kind == wmChangeK && (k.back90 < 0 || k.back90 > 10*time.Second) {
		// (as the rig's analysis: 90% of the pace before the drop)
		fail("back to 90%% of the rate before in %v", k.back90)
	}
	if v.cpu > time.Second && !wmRace {
		fail("model run took %v", v.cpu)
	}
	v.core = k
	return v
}

func (v wmVerdict) String() string {
	ok := "ok"
	if len(v.fails) > 0 {
		ok = strings.Join(v.fails, "; ")
	}
	t := v.core.t90
	if len(v.cell.cfg.changes) > 0 {
		t = v.core.back90
	}
	return fmt.Sprintf("%-28s base %6.1f %5.2f%% i%4.0f/%4.0f | n4 %6.1f %5.2f%% | core %6.1f %5.2f%% q%3.0f/%3.0f i%4.0f/%4.0f t90 %4.1fs d%v/%d p%v B%4.1f | %s",
		v.cell.cfg.name, v.base.mbit, 100*v.base.resend, v.base.iP50, v.base.iP99, v.n4.mbit, 100*v.n4.resend, v.core.mbit, 100*v.core.resend,
		v.core.qMean, v.core.q99, v.core.iP50, v.core.iP99, t.Seconds(), v.core.drains, v.core.holdDrains, v.core.probes, float64(v.core.peakB)/(1<<20), ok)
}

// wmKindOf names the target a failure message is about.
func wmKindOf(msg string) string {
	switch {
	case strings.HasPrefix(msg, "gain"):
		return "gain"
	case strings.HasPrefix(msg, "slower"):
		return "slower"
	case strings.HasPrefix(msg, "resent"):
		return "resent"
	case strings.HasPrefix(msg, "interactive"):
		return "interactive"
	case strings.HasPrefix(msg, "queue"):
		return "queue"
	case strings.Contains(msg, "drain(s) from HOLD"):
		return "drain"
	case strings.HasPrefix(msg, "back to"):
		return "t90"
	}
	return "other" // the run's CPU time: never expected
}

// wmKnown are the targets the core is known to miss in the model, per
// cell of the default table, and why (core-NOTES.md has the traces). A
// cell that misses a target not listed for it fails the test; a listed
// cell that meets every target is reported, to be taken off the list.
var wmKnown = map[string]wmKnownCell{
	"300/150 q50": {[]string{"resent"},
		"the sender's PROBE_RTT every ~10 s (Linux takes a new min_rtt only from a strictly lower sample), then its whole window at 3.9 times the path's rate: past ~2.5 MB of window the 50 ms queue (0.33 BDP) overflows whatever the receiver does (the rig: a loss every 10–11 s at 5–6 MiB). The exit's pacing net (pacenet.go) holds that pace once it loses (151 → 244 Mbit/s, 1.65 → 0.24% resent, against the model before it). With the covers (smux Cover, the floor: wcore.go cover) it no longer drains from HOLD, and resends 0.51% against base's 0 (0.24% before them; the target is 0.5)"},
	"1000/150 q50": {[]string{"resent", "interactive"},
		"a burst after PROBE_RTT overflows the queue (a cut); 0.26 to 0.54% resent against base's 0 over the 30 s table's seeds, with the exit's pacing net or without (0.37% either way in the 40 s grid). With the covers the core gets 637 Mbit/s (616 before) and no drain from HOLD, and the user next to it a p99 of 438 ms against base's 304 (373 before)"},
	"300/100 q50": {[]string{"interactive", "gain"},
		"the bursts after PROBE_RTT (one loss holds the user behind its hole). With the covers the windows without the core carry 168 Mbit/s (base 84) in bursts the exit's record reads as 14 ms of queue (the need's read 9): past Δq, harm() keeps the core OFF until the sender's PROBE_RTT clears them, 12 s later, so it gets 182 Mbit/s against 219 before (59% of N4's gain, the target 80%; 199 with the need as floor)"},
	"100/100 q50": {[]string{"interactive"},
		"with the exit's pacing net the 2 MiB windows of base no longer overflow this path (base 66.8 Mbit/s with 7.1% resent without it, 83.5 with 0.2%), so the core leaves OFF. Its 1.25 MB BDP lies between the need less a step and the need: a cover there adds no rate (84 against 83 Mbit/s) and its sender's bursts after PROBE_RTT (the whole 3 MiB window at 3.9 times the path) take the user's p99 to 408 ms against base's 266 (the windows without the core alone: 400)"},
	"300/150 q200": {[]string{"queue"},
		"the 200 ms queue lets STARTUP and the probes build a mean queue past 20 ms before the turnarounds show it"},
	"1000/100 q50 l0.1": {[]string{"interactive", "gain"},
		"every hole holds the user's reply for a round trip, and 3–4 times the rate is 3–4 times the holes (p50 1.3–1.5×; the rig's N4 too): one link shared with the download; the multi-link cell has new users land elsewhere. With the covers (in OFF too) the window-limited sender no longer waits for credit every round and leaves BBR's STARTUP (pacing 3.9 times the path) for PROBE_BW at ~4.7 s, where it stayed in STARTUP to 13 s before; the core's later steps of B turn into rate only at BBR's probing pace: 160 Mbit/s against 241 (42% of N4's gain, the target 60%), the windows without the core 109 against base's 83"},
	"300/150 q50 l0.1": {[]string{"gain", "interactive"},
		"each recovered hole releases its credit at once and the sender sends it at 3.9 times the path's rate: on the 50 ms queue those bursts overflow near the knee, so HOLD stays under it (48% of N4's gain without the exit's pacing net). The net takes N4 from 129 to 158 Mbit/s (its bursts held once they lose: 25% of that gain now) and costs the core one seed of three: a tail loss's resends set it off for 10 s in the core's STARTUP, which ends lower (87 → 76 Mbit/s median). With the covers 97 Mbit/s (44%), and the user's p50 next to it 225 ms against base's 169 (1.33×; 194 before them, at 76 Mbit/s)"},
	"300/150 q50 x4": {[]string{"interactive"},
		"four downloads fill the path at base already (255 Mbit/s); their covers (smux Cover) add no rate, and the bursts of 3 MiB windows after each sender's PROBE_RTT take the user's p99 to ~495 ms against base's 326 (the windows without the core alone: 491)"},
	"1000/100 q50 x4": {[]string{"interactive"},
		"the bursts of four 3 MiB windows after their senders' PROBE_RTT (smux Cover, in OFF and the core's floor): the user's p99 301 ms against base's 219 (1.37×; 249 before the covers), at 757–779 Mbit/s as before"},
	"300/150 q50 300>50>300": {[]string{"interactive"},
		"the loss of the drop to 50 Mbit/s holds the user behind its holes (p99 1.1 s against base's 0.7 s; the exit's pacing net takes the core's resends 10.4 → 2.9% and base's 8.9 → 1.9%)"},
	"1000/100 q50 policer 300": {[]string{"gain", "interactive"},
		"with the exit's pacing net base no longer runs past the policer (2.7 → 0.3% resent, 95 → 100 Mbit/s), and the gain target applies: the core 101 → 149 Mbit/s with 0.46% resent, 38% of N4's gain (N4 resends 10%); with the covers 187 Mbit/s with 0.04% (67%; the windows without the core 177), and the user's p99 377 ms against base's 288 (1.31×)"},
}

// wmKnownNoCover is wmKnown as it was before smux's covers (93d0faa): with
// the covers off (HS2_TUNE_SMUX_COVER=0) the model is base's to the byte
// (TestWcoreModelCoverOff), and so are the core's misses.
var wmKnownNoCover = map[string]wmKnownCell{
	"300/150 q50": {[]string{"drain"},
		"the sender's PROBE_RTT every ~10 s (Linux takes a new min_rtt only from a strictly lower sample), then its whole window at 3.9 times the path's rate: past ~2.5 MB of window the 50 ms queue (0.33 BDP) overflows whatever the receiver does (the rig: a loss every 10–11 s at 5–6 MiB). The exit's pacing net (pacenet.go) holds that pace once it loses (151 → 244 Mbit/s, 1.65 → 0.24% resent, against the model before it); a burst before it holds (or after a lift) still overflows the queue in HOLD in one of three seeds (a cut)"},
	"1000/150 q50": {[]string{"drain", "resent"},
		"a burst after PROBE_RTT overflows the queue in HOLD (a cut); 0.26 to 0.54% resent against base's 0 over the 30 s table's seeds, with the exit's pacing net or without (0.37% either way in the 40 s grid)"},
	"300/100 q50": {[]string{"interactive"},
		"the bursts after PROBE_RTT (one loss holds the user behind its hole)"},
	"100/100 q50": {[]string{"drain"},
		"with the exit's pacing net the 2 MiB windows of base no longer overflow this path (base 66.8 Mbit/s with 7.1% resent without it, 83.5 with 0.2%), so the core leaves OFF (do no harm passes), and drains from HOLD at the sender's PROBE_RTT bursts before the net holds its pace: 84 Mbit/s with 0.22% resent, base's"},
	"300/150 q200": {[]string{"queue"},
		"the 200 ms queue lets STARTUP and the probes build a mean queue past 20 ms before the turnarounds show it"},
	"1000/100 q50 l0.1": {[]string{"interactive"},
		"every hole holds the user's reply for a round trip, and 3–4 times the rate is 3–4 times the holes (p50 1.3–1.5×; the rig's N4 too): one link shared with the download; the multi-link cell has new users land elsewhere"},
	"300/150 q50 l0.1": {[]string{"gain"},
		"each recovered hole releases its credit at once and the sender sends it at 3.9 times the path's rate: on the 50 ms queue those bursts overflow near the knee, so HOLD stays under it (48% of N4's gain without the exit's pacing net). The net takes N4 from 129 to 158 Mbit/s (its bursts held once they lose: 25% of that gain now) and costs the core one seed of three: a tail loss's resends set it off for 10 s in the core's STARTUP, which ends lower (87 → 76 Mbit/s median)"},
	"300/150 q50 +slow reader": {[]string{"resent", "interactive", "gain", "drain"},
		"a probe's loss while the slow stream's window holds a hole; with the exit's pacing net N4 no longer overflows the queue (180 → 250 Mbit/s, 3.3 → 0% resent), and the gain target is against that (the core 209 → 211 Mbit/s, 75% of it); one seed of three drains from HOLD at a PROBE_RTT burst"},
	"300/150 q50 300>50>300": {[]string{"interactive"},
		"the loss of the drop to 50 Mbit/s holds the user behind its holes (p99 1.1 s against base's 0.7 s; the exit's pacing net takes the core's resends 10.4 → 2.9% and base's 8.9 → 1.9%)"},
	"1000/100 q50 policer 300": {[]string{"gain"},
		"with the exit's pacing net base no longer runs past the policer (2.7 → 0.3% resent, 95 → 100 Mbit/s), and the gain target applies: the core 101 → 149 Mbit/s with 0.46% resent, 38% of N4's gain (N4 resends 10%)"},
}

type wmKnownCell struct {
	kinds []string
	why   string
}

// wmKnownNow is wmKnown, or wmKnownNoCover with the covers off.
func wmKnownNow() map[string]wmKnownCell {
	if !smuxCoverOn(false) {
		return wmKnownNoCover
	}
	return wmKnown
}

// wmKnownRoadmap are the cells of the full table where the core misses the
// roadmap's own limit (resent ≤ base + 3 points): 50 ms queues of at most
// ~600 KB-2 MB on long round trips, where BBR's STARTUP pacing (raised by
// the core's first growth, never lowered while the sender is window-limited)
// sends each half-window update's credit faster than the path and the
// queue overflows at the floor too. Stage 2's smaller steps or paced credit
// at the exit are what helps there; stage 1 cannot. Since z12 (the model's
// sender as the rig's: STARTUP pacing 3.9×, PROBE_RTT every ~10 s and the
// whole window after it) also 100/500 q200 and 300/300 q50, where the
// window the path's BDP needs is 2.5 to 6 times the queue: the burst after
// each PROBE_RTT overflows it. The exit's pacing cap (WcorePaceCap, a lab
// experiment) is what removes that burst on the rig.
var wmKnownRoadmap = map[string]bool{
	"100/100 q50 l1": true, "100/150 q50 l0.1": true, "100/300 q50 l0.1": true, "100/500 q50": true,
	"100/500 q50 l0.1": true, "300/300 q50 l0.1": true, "300/500 q50": true,
	"100/500 q200": true, "300/300 q50": true,
}

// TestWcoreModelTable runs base, N4 and the core on each cell and judges the
// core against the targets of §9.4 (wmJudge). The default table is a gate:
// a miss not in wmKnown fails. HS2_WCORE_MODEL_FULL=1 adds the full matrix
// (rates × round trips × queues × loss); its other cells are a report, held
// only to the roadmap's resend limit (with wmKnownRoadmap).
func TestWcoreModelTable(t *testing.T) {
	full := os.Getenv("HS2_WCORE_MODEL_FULL") == "1"
	gate := map[string]bool{}
	for _, c := range wmCells(false) {
		gate[c.cfg.name] = true
	}
	cells := wmCells(full)
	bad, seen := 0, map[string]bool{}
	knownNow := wmKnownNow()
	for _, c := range cells {
		v := wmJudge(c)
		t.Log(v)
		name := c.cfg.name
		if seen[name] {
			t.Fatalf("two cells named %q", name)
		}
		seen[name] = true
		if len(v.fails) > 0 {
			bad++
		}
		if lim := v.base.resend + 0.03; v.core.resend > lim && !wmKnownRoadmap[name] {
			t.Errorf("%s: resent %.2f%% past the roadmap's base + 3 points (%.2f%%)", name, 100*v.core.resend, 100*lim)
		}
		if !gate[name] {
			continue
		}
		known := knownNow[name]
		missed := map[string]bool{}
		for _, f := range v.fails {
			missed[wmKindOf(f)] = true
			if !slices.Contains(known.kinds, wmKindOf(f)) {
				t.Errorf("%s: %s", name, f)
			}
		}
		for _, k := range known.kinds {
			if !missed[k] {
				t.Logf("%s now meets its %s target: take it off wmKnown", name, k)
			}
		}
		if len(v.fails) == 0 && len(known.kinds) > 0 {
			t.Logf("%s now meets every target: take it off wmKnown", name)
		}
	}
	for name := range knownNow {
		if !seen[name] {
			t.Errorf("wmKnown names %q, which is no cell", name)
		}
	}
	for name := range wmKnownRoadmap {
		if full && !seen[name] {
			t.Errorf("wmKnownRoadmap names %q, which is no cell", name)
		}
	}
	t.Logf("%d of %d cells meet every target", len(cells)-bad, len(cells))
}

// TestWcoreModelCover runs smux's windows without the core (wmCover: the
// need, or its cover while the window holds a stream back; a mode the
// engine no longer runs: an end with no core gives no cover, smuxCoverOn)
// next to base
// (the 2 MiB window as the rig measured it, before the cover) and, on the
// default table's cells, holds the cover to:
//
//   - a single download on a clean path that base leaves at under 60% of
//     what the path carries (its window holds it): at least 1.4 times base
//     (the need in flight where base had about the need less a step);
//   - nowhere under 95% of base;
//   - resent no more than base + 0.5 points (+3 across a drop of the
//     path's rate, as the table's targets);
//   - window updates (wmUpdMiBMax): no more than 1.05 per MiB read, as
//     base (one per step, a MiB at the need), or than base's own where its
//     windows came down -- the cover and the core alike.
//
// HS2_WCORE_MODEL_FULL=1 adds the full table as a report: there, under 1%
// and 3% random loss, the cover runs 4 to 20% under base (100/50, 300/100,
// 1000/150 at 1%; 300/300 at 3%), and on 100/500 at 0.1% it resends 2.0%
// against base's 0.6%. With the covers off (HS2_TUNE_SMUX_COVER=0) there is
// nothing to hold (TestWcoreModelCoverOff holds the model to base's).
func TestWcoreModelCover(t *testing.T) {
	if !smuxCoverOn(false) {
		t.Skip("the covers are off (HS2_TUNE_SMUX_COVER=0)")
	}
	full := os.Getenv("HS2_WCORE_MODEL_FULL") == "1"
	gate := map[string]bool{}
	for _, c := range wmCells(false) {
		gate[c.cfg.name] = true
	}
	held, gained := 0, 0
	updMax := [3]float64{} // base, cover, core: the most updates per MiB of a cell
	for _, c := range wmCells(full) {
		b, k := wmRunN(c.cfg.with(wmBase), wmSeeds), wmRunN(c.cfg.with(wmCover), wmSeeds)
		o := wmRunN(c.cfg.with(wmCore), wmSeeds)
		t.Logf("%-28s base %6.1f %5.2f%% q%3.0f/%3.0f i%4.0f/%4.0f | cover %6.1f %5.2f%% q%3.0f/%3.0f i%4.0f/%4.0f | %.2fx | upd/MiB %.3f %.3f core %.3f",
			c.cfg.name, b.mbit, 100*b.resend, b.qMean, b.q99, b.iP50, b.iP99, k.mbit, 100*k.resend, k.qMean, k.q99, k.iP50, k.iP99, k.mbit/b.mbit,
			b.updMiB, k.updMiB, o.updMiB)
		for i, v := range []float64{b.updMiB, k.updMiB, o.updMiB} {
			updMax[i] = math.Max(updMax[i], v)
		}
		updLim := math.Max(wmUpdMiBMax, wmUpdMiBMax*b.updMiB)
		if k.updMiB > updLim {
			t.Errorf("%s: the cover sends %.3f window updates per MiB, base %.3f", c.cfg.name, k.updMiB, b.updMiB)
		}
		if o.updMiB > updLim {
			t.Errorf("%s: the core sends %.3f window updates per MiB, base %.3f", c.cfg.name, o.updMiB, b.updMiB)
		}
		isHeld := c.kind == wmGain && len(c.cfg.streams) == 0 && b.mbit < 0.6*c.cfg.rate*wmEff/wmMbit
		if isHeld {
			held++
			if k.mbit >= 1.4*b.mbit {
				gained++
			}
		}
		if !gate[c.cfg.name] {
			continue
		}
		if isHeld && k.mbit < 1.4*b.mbit {
			t.Errorf("%s: cover %.1f Mbit/s, base %.1f: want 1.4 times base where its window holds it", c.cfg.name, k.mbit, b.mbit)
		}
		if k.mbit < 0.95*b.mbit {
			t.Errorf("%s: cover %.1f Mbit/s, slower than base %.1f", c.cfg.name, k.mbit, b.mbit)
		}
		lim := b.resend + 0.005
		if c.kind == wmChangeK {
			lim = b.resend + 0.03
		}
		if k.resend > lim {
			t.Errorf("%s: cover resent %.2f%%, base %.2f%%", c.cfg.name, 100*k.resend, 100*b.resend)
		}
	}
	t.Logf("the cover at 1.4 times base or more in %d of %d cells where base's window holds a single download", gained, held)
	t.Logf("window updates per MiB, the most of a cell: base %.3f, cover %.3f, core %.3f", updMax[0], updMax[1], updMax[2])
}

// wmUpdMiBMax is as many window updates per MiB read as the covers and the
// core may send (TestWcoreModelCover; smux's TestCoverHalfWindowLock): base's
// one per step of a MiB at the need, and 5% for the rest.
const wmUpdMiBMax = 1.05

// TestWcoreModelCoverOff: with the covers off (HS2_TUNE_SMUX_COVER=0, the
// covers alone; HS2_TUNE_WCORE=0 takes the core and its covers) the model is
// base's (93d0faa: the reordering fix, before the covers) to the byte:
// every run of base, N4, fin and the core, each seed of each cell of the
// default table
// (HS2_WCORE_MODEL_FULL=1: of the full one), gives what
// testdata/wcore_model_nocover.txt holds for it, made on 93d0faa by the same
// wmRunLine (a hash of a cell's runs per mode). HS2_WCORE_MODEL_GOLDEN=1
// writes the file anew from this tree: only for a change to the core meant
// to show with the covers off too, and said so in its commit.
func TestWcoreModelCoverOff(t *testing.T) {
	defer func(v int) { SmuxCover = v }(SmuxCover)
	SmuxCover = 0
	const golden = "testdata/wcore_model_nocover.txt"
	full := os.Getenv("HS2_WCORE_MODEL_FULL") == "1"
	write := os.Getenv("HS2_WCORE_MODEL_GOLDEN") == "1"
	want := map[string]string{}
	if !write {
		b, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if i := strings.LastIndexByte(l, ' '); i > 0 {
				want[l[:i]] = l[i+1:]
			}
		}
	}
	var out []string
	n := 0
	for _, c := range wmCells(full || write) {
		for _, mode := range []wmMode{wmBase, wmN4, wmFin, wmCore} {
			key, got := wmRunLine(c.cfg.with(mode))
			out = append(out, key+" "+got)
			if write {
				continue
			}
			n++
			if w, ok := want[key]; !ok {
				t.Errorf("%s: not in %s", key, golden)
			} else if got != w {
				t.Errorf("%s: %s with the covers off, base %s", key, got, w)
			}
		}
	}
	if write {
		if err := os.WriteFile(golden, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d runs to %s", len(out), golden)
		return
	}
	t.Logf("%d cells and modes as base's to the byte", n)
}

// wmRunLine runs cfg on each seed of wmRunN and hashes what each run gave
// (every field of wmResult base had, each float to its last bit, and the
// sender's resent and sent segments of each second); key names the cell and
// the mode.
func wmRunLine(cfg wmCfg) (key, sum string) {
	h := fnv.New64a()
	for i := 0; i < wmSeeds; i++ {
		c := cfg
		c.seed = cfg.seed + uint64(i)
		r := wmRun(c)
		fmt.Fprintf(h, "%x %x %x %x %x %x %x %x %x %x|%d %d %v %d %v %d %v %x %d %d %d %d|",
			r.mbit, r.reach, r.resend, r.resendAll, r.selfDrop, r.randDrop, r.qMean, r.q99, r.iP50, r.iP99,
			r.t90, r.back90, r.drains, r.holdDrains, r.probes, r.peakB, r.lastState, r.compMbit, r.probeRTTs, r.netN, r.netMs, len(r.trace))
		for _, s := range r.rtxSec {
			fmt.Fprintf(h, "%d,%d ", s[0], s[1])
		}
	}
	return strings.ReplaceAll(cfg.name, " ", "_") + "|" + cfg.mode.String(), fmt.Sprintf("%016x", h.Sum64())
}

// The model takes HS2_TUNE_SMUX_COVER as cmd/hs2 does (SmuxCover), so that
// HS2_TUNE_SMUX_COVER=0 HS2_WCORE_MODEL_FULL=1 go test -run TestWcoreModel
// ./engine prints base's model (93d0faa) to the byte: the roll-back's check
// (TestWcoreModelCoverOff holds it).
func init() {
	if v, err := strconv.Atoi(os.Getenv("HS2_TUNE_SMUX_COVER")); err == nil {
		SmuxCover = v
	}
	if v, err := strconv.Atoi(os.Getenv("HS2_TUNE_SMUX_COVER_GAIN")); err == nil {
		SmuxCoverGain = v
	}
}

// TestWcoreModelCoverGain: the lab knob SmuxCoverGain 5 (a cover only where
// it gains a quarter of the rate or more, and the core's floor the cover
// only once its link held one in OFF). On paths whose BDP lies between the
// need less a step and the need the user next to the download keeps base's
// p99 within 20 ms (the judge's bar; with the covers as they are 100/100
// 408 ms against base's 301, 150/72 231 against 148), and on large paths
// the cover's gain stays (1.4 times base or more). Not on 300/50 behind
// 50 ms: the cover gains a quarter there, and its p99 stays 178–206 ms
// against base's 114.
func TestWcoreModelCoverGain(t *testing.T) {
	if !smuxCoverOn(false) {
		t.Skip("the covers are off (HS2_TUNE_SMUX_COVER=0)")
	}
	defer func(v int) { SmuxCoverGain = v }(SmuxCoverGain)
	SmuxCoverGain = 5
	for _, x := range []struct {
		rate   float64
		rtt, q int
		mid    bool
	}{{100, 100, 50, true}, {150, 72, 50, true}, {1000, 72, 50, false}, {300, 72, 50, false}} {
		c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
		c.dur = 40 * time.Second
		b, k := wmRunN(c.with(wmBase), wmSeeds), wmRunN(c.with(wmCore), wmSeeds)
		msg := fmt.Sprintf("%s: base %.1f Mbit p99 %.0f ms; core with CoverGain 5 %.1f Mbit p99 %.0f ms", c.name, b.mbit, b.iP99, k.mbit, k.iP99)
		switch {
		case x.mid && k.iP99 > b.iP99+20:
			t.Errorf("the user's p99 past base's + 20 ms: %s", msg)
		case !x.mid && k.mbit < 1.4*b.mbit:
			t.Errorf("the cover's gain lost: %s", msg)
		default:
			t.Log(msg)
		}
	}
}

func TestWcoreModelTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_MODEL_TRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_MODEL_TRACE=rate/rtt/queue[/loss[/mode[/streams]]]")
	}
	var rate, rtt, q, loss float64
	mode := "core"
	parts := strings.Split(spec, "/")
	fmt.Sscan(parts[0], &rate)
	fmt.Sscan(parts[1], &rtt)
	fmt.Sscan(parts[2], &q)
	if len(parts) > 3 {
		fmt.Sscan(parts[3], &loss)
	}
	if len(parts) > 4 {
		mode = parts[4]
	}
	streams := 1
	if len(parts) > 5 {
		fmt.Sscan(parts[5], &streams)
	}
	c := wmPath(rate, time.Duration(rtt)*time.Millisecond, time.Duration(q)*time.Millisecond)
	c.loss = loss / 100
	c.trace = true
	if streams > 1 {
		c.streams = make([]wmStreamCfg, streams)
	}
	for i, n := range []string{"base", "n4", "fin", "core", "cover"} {
		if n == mode {
			c.mode = wmMode(i)
		}
	}
	r := wmRun(c)
	for _, l := range r.trace {
		t.Log(l)
	}
	t.Log(r)
}

// TestWcoreModelInvariants runs the core on random paths (fixed seeds) and
// checks at every step:
//   - B never above n·MaxStreamBuffer, nor above n·need plus the window pool
//     (the pool covers what is past need); no ceiling above
//     MaxStreamBuffer, and the published ceilings within B;
//   - every probe is over within one evaluation period (its settle and
//     measure parts, each after the ramp and ending on a runner wake), and
//     a failed one leaves B at or below the budget it probed from;
//   - on paths where the windows cannot make a queue (n·MaxStreamBuffer of
//     window drains in under half the round trip, and the tail-drop queue
//     holds all of it), random loss never takes the core to DRAIN.
//
// HS2_WCORE_MODEL_FULL=1 runs more seeds.
func TestWcoreModelInvariants(t *testing.T) {
	seeds := 12
	if os.Getenv("HS2_WCORE_MODEL_FULL") == "1" {
		seeds = 60
	}
	for seed := 1; seed <= seeds; seed++ {
		r := rand.New(rand.NewPCG(uint64(seed), 0x5eed))
		pick := func(v ...float64) float64 { return v[r.IntN(len(v))] }
		quiet := seed%3 == 0 // a path the windows cannot queue on
		var c wmCfg
		n := 1 + r.IntN(4)
		if quiet {
			rtt := time.Duration(pick(100, 200, 300, 500)) * time.Millisecond
			n = 1 + r.IntN(2)
			rate := 2 * float64(n) * wmMaxWin / rtt.Seconds() / wmMbit
			c = wmPath(rate, rtt, time.Duration(float64(n)*wmMaxWin/(rate*wmMbit)*2*float64(time.Second)))
			c.loss = pick(0.001, 0.003, 0.01, 0.02)
		} else {
			c = wmPath(pick(20, 50, 100, 300, 1000), time.Duration(pick(20, 50, 100, 150, 300, 500))*time.Millisecond,
				time.Duration(pick(10, 20, 50, 100, 300))*time.Millisecond)
			c.loss = pick(0, 0, 0.001, 0.01)
			if r.IntN(4) == 0 {
				c.changes = []wmChange{{time.Duration(5+r.IntN(10)) * time.Second, c.rate * pick(0.1, 0.3, 3)}}
			}
			if r.IntN(6) == 0 {
				c.policer = c.rate * pick(0.2, 0.5)
			}
			if r.IntN(3) == 0 {
				c.pool = int64(pick(1, 4, 8)) << 20
			}
		}
		c.streams = make([]wmStreamCfg, n)
		for i := range c.streams {
			if !quiet && r.IntN(3) == 0 {
				c.streams[i].read = c.rate * pick(0.02, 0.1, 0.5)
			}
			if !quiet && r.IntN(4) == 0 {
				c.streams[i].start = time.Duration(r.IntN(8)) * time.Second
			}
		}
		c.mode, c.seed, c.dur = wmCore, uint64(seed), 20*time.Second
		c.name = fmt.Sprintf("seed %d: %s x%d l%g", seed, c.name, n, 100*c.loss)
		pool := c.pool
		if pool == 0 {
			pool = 64 << 20
		}
		var (
			errs     []string
			inProbe  bool
			probeAt  time.Duration
			probeLim time.Duration
			probeB   uint64
			fails    int
			prev     = wcOff
			holdB    uint64
			drains   int
		)
		fail := func(f string, a ...any) {
			if len(errs) < 5 {
				errs = append(errs, fmt.Sprintf(f, a...))
			}
		}
		c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
			ns := uint64(len(in.streams))
			if o.B > ns*wmMaxWin || k.B > ns*wmMaxWin {
				fail("%v: B %d/%d above %d streams × MaxStreamBuffer", at, o.B, k.B, ns)
			}
			if o.B > ns*wmNeed+uint64(pool) {
				fail("%v: B %d above n·need + pool %d", at, o.B, ns*wmNeed+uint64(pool))
			}
			sum := uint64(0)
			for _, x := range o.ceils {
				if x.ceil > wmMaxWin {
					fail("%v: ceiling %d above MaxStreamBuffer", at, x.ceil)
				}
				sum += uint64(x.ceil)
			}
			if sum > o.B {
				fail("%v: ceilings %d above B %d", at, sum, o.B)
			}
			switch {
			case o.state == wcProbe && prev != wcProbe:
				s, mz := k.interval()
				inProbe, probeAt, fails = true, at, k.cnt.probeFail
				// the hold budget it probed from (HOLD's estimate at that
				// step: within the margin of the one in force before)
				probeB = max(k.probeFrom, uint64(len(k.last.f))*wmNeed)
				if holdB > 0 && float64(k.probeFrom) > 1.3*float64(holdB) {
					fail("%v: a probe from %d while HOLD held %d", at, k.probeFrom, holdB)
				}
				// its settle and measure parts, each after the ramp, each
				// ending on a runner wake
				probeLim = s + mz + 2*k.rampLeft() + 2*wcRampTick
			case inProbe && o.state != wcProbe:
				inProbe = false
				if at-probeAt > probeLim {
					fail("%v: a probe from %v ran %v (period %v)", at, probeAt, at-probeAt, probeLim)
				}
				if k.cnt.probeFail > fails && k.B > probeB {
					fail("%v: a failed probe left B %d above %d", at, k.B, probeB)
				}
			case inProbe && at-probeAt > probeLim+100*time.Millisecond:
				fail("%v: a probe from %v still runs (period %v)", at, probeAt, probeLim)
				inProbe = false
			}
			if o.state == wcHold {
				holdB = k.B
			}
			for _, e := range o.events {
				if e.kind == wcEvDrain {
					drains++ // (the refresh is no drain: it emits none)
				}
			}
			prev = o.state
		}
		res := wmRun(c)
		if quiet && drains > 0 {
			fail("random loss without a queue: %d drain(s) %v", drains, res.drains)
		}
		if len(errs) > 0 {
			t.Errorf("%s\n\t%s\n\t%s", c.name, res, strings.Join(errs, "\n\t"))
		} else {
			t.Logf("%s: %.0f Mbit, resent %.2f%%, drains %v", c.name, res.mbit, 100*res.resend, res.drains)
		}
	}
}
