package engine

import (
	"os"
	"sync/atomic"
	"time"
)

// ul257: an upload whose raw first sub is stuck grows its stripe at once
// (HS2_UL257_STUCK, Iran, default on; 0 turns it off).
//
// Lab3 dr257 (10 x 1 Gbit paths, 74 ms, 0.1-0.8 % loss, 1000 crowd users,
// alternating 12 s single-stream iperf3): about one upload in 15 sat at
// 0-30 Mbit/s for 2-5 s ([102, 0, 33, 0, 69, 95, 361, 504 ...]). Its trace
// (HS2_MCONN_TRACE): the raw first sub's link was in loss recovery / RTO on
// the 0.8 % path (cwnd 512 -> 4, notsent 50 KB, 10 MB in 5 s), and the
// growth rule could not leave it: the early sample needs 30 Mbit/s, and with
// another upload striping (stripeFastSolo) the normal rule needs
// stripeHotTicks (4) consecutive samples at stripeSubRate — each 0 Mbit/s
// second of the stuck link reset the count. The stripe grew at 5.0 s (one
// 31 Mbit/s sample); 1-2 s after the second sub joined the upload ran at
// 360-590 Mbit/s while the first link still showed cwnd 4.
//
// The signal the rate cannot give is demand: the user's bytes wait in
// mConn.Write. With the key, a raw upload sample in which the raw writer sat
// blocked in the first sub's Write for at least ul257BlockPct of the window,
// moving under stripeFastRate, after at least ul257MinSent raw bytes, opens
// the second sub now (the usual upgrade; one sub, no doubling chain). A
// connection the user feeds slowly (a call, a page, an app-limited upload)
// never blocks in Write, so it never takes this path; a stuck link that the
// user does not wait on stays as before. Only n == 1 (the raw first sub):
// from the second sub on the usual rules apply. Full samples only: an early
// (HS2_MCONN_STRIPE_FAST_EARLY_MS) 200 ms window is skipped, since the B2
// run showed a 20 Mbit/s paced upload's first burst blocking its writer for
// most of one (a false trigger).
var ul257Stuck atomic.Bool

func init() { ul257Stuck.Store(os.Getenv("HS2_UL257_STUCK") != "0") }

const (
	ul257BlockPct = 60      // % of the sample window the raw writer was blocked
	ul257MinSent  = 1 << 20 // raw bytes written before the rule applies
)

// ul257Stat: uploads whose stripe the rule started (logs/tests).
var ul257Stat atomic.Uint64

// ul257Log: the "ul257 stuck raw upload" lines, at most 10 a minute (b264).
var ul257Log = logBudget{per: 10}

// ul257WriteStart / ul257WriteEnd (writeRaw, under wmu): the raw writer's
// blocked time. A write still blocked counts up to now (rawBlockedLocked),
// so a link stuck for the whole window shows as blocked.
func (m *mConn) ul257WriteStart() int64 {
	t := monoNano(time.Now())
	m.rawWIn.Store(t)
	return t
}

func (m *mConn) ul257WriteEnd(t0 int64) {
	m.rawWIn.Store(0)
	if d := monoNano(time.Now()) - t0; d > 0 {
		m.rawWBlk.Add(d)
	}
}

// rawBlockedLocked (under m.mu): the raw writer's blocked time since the
// last call.
func (m *mConn) rawBlockedLocked(now time.Time) time.Duration {
	tot := m.rawWBlk.Load()
	if s := m.rawWIn.Load(); s > 0 {
		if d := monoNano(now) - s; d > 0 {
			tot += d
		}
	}
	d := tot - m.rawWBlkSnap
	m.rawWBlkSnap = tot
	return time.Duration(max(d, 0))
}

// ul257SampleStartLocked (stripeTickLocked's first sample, under m.mu): the
// blocked-time snapshot starts with the first window (b264 audit: without it
// the first full sample also counted every blocked ns since the connection
// began, before that window, so a short window could read as >= 60 %
// blocked from older writes).
func (m *mConn) ul257SampleStartLocked(now time.Time) {
	if m.rawW.Load() {
		m.rawBlockedLocked(now)
	}
}

// ul257StuckLocked (stripeTickLocked, under m.mu): this sample is a stuck
// raw upload that should grow now.
func (m *mConn) ul257StuckLocked(up bool, n int, rate float64, w, blocked time.Duration) bool {
	if !ul257Stuck.Load() || !up || n != 1 || !m.rawW.Load() || w <= 0 || m.stEarlyW > 0 {
		return false
	}
	if m.rawSent.Load() < ul257MinSent || rate >= float64(stripeFastRate.Load()) {
		return false
	}
	return blocked*100 >= w*ul257BlockPct
}

// ul257Up: the sample counts as an upload for the stuck rule. A link that
// moves nothing at all (dr258: a 4 s blackout of the first sub's path) gives
// 0 both ways, and the rate rule's strict "sent > recvd" called it no upload,
// so the rule never fired while the writer sat blocked for 4 s; a tie is an
// upload here (the blocked raw writer is the demand; a download's raw writer
// does not block).
func ul257Up(sentD, recvdD uint64, dlGo bool) bool {
	return !dlGo && sentD >= recvdD
}
