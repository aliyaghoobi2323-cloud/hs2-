package engine

import (
	"context"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// us200 (HS2_LINK_CC_REFRESH=1): give a link's kernel congestion control a
// fresh start once the link has gone light, so the next bulk transfer placed
// on it ramps like on a new link.
//
// Root cause (lab us200): a link socket that lost packets or hit a (spurious)
// retransmission timeout — a stall, or only the bufferbloat of a download in
// the other direction delaying its ACKs — leaves BBR's STARTUP for good
// (PROBE_BW), and while the link then carries only light traffic BBR's
// bandwidth estimate decays to that light rate (100-800 kbit/s, cwnd ~10,
// ssthresh 8). The next single upload placed on such a link was paced at that
// rate and grew only by PROBE_BW's 1.25 gain cycles: 3 s of zero throughput,
// then a slow ramp (u1 82 vs 130 Mbit/s right after a download, 16-30 after the
// stall pattern). A fresh link is still in STARTUP (light traffic is
// app-limited and never ends it) and ramps 2.89x per round trip.
//
// The fix re-initialises the socket's congestion control (setsockopt
// TCP_CONGESTION to "reno", then back to its own algorithm: the kernel only
// re-initialises on a change), which puts BBR back in STARTUP with its
// filters reset — the state of a fresh link. It is done only on a link that
// is light (under ccLightBytes acknowledged per health tick, nothing queued)
// for ccLightTicks ticks in a row, whose BBR has left STARTUP and whose
// bandwidth estimate is under ccRefreshBwMax: never on a link that moves
// bulk data, so no running transfer is disturbed, and light traffic is
// app-limited either way. Linux and BBR only; other algorithms are left alone.
var linkCCRefresh atomic.Bool

const (
	ccLightBytes   = 256 << 10         // per healthTick (2 s): ~1 Mbit/s
	ccLightTicks   = 2                 // ticks light in a row
	ccRefreshBwMax = 20e6 / 8          // bytes/s: BBR estimate under 20 Mbit/s
	ccBBRHighGain  = 2885*256/1000 + 1 // BBR's STARTUP pacing gain (BBR_UNIT 256)
	ccBBRUnit      = 256               // BBR_UNIT: PROBE_RTT runs pacing and cwnd gain 1

	// pk236 backoff. Prod rel69c (Iran, ~130 links): 1400-1600 refreshes a
	// minute, every light link about every 5 s - the fastest the rules
	// allow (2 light ticks). Iran's link sockets send only light data, and a
	// prod link's light traffic (many users' requests, small uploads) has
	// enough non-app-limited rounds for BBR to leave STARTUP again within
	// seconds (the lab's single test user did not: ~1/min per link), so the
	// "fresh" state did not stick and every link was re-inited in a loop.
	// Now a refresh that follows another one with no transfer in between
	// waits ccBackMin ticks, doubling to ccBackMax; a tick that moved
	// ccBulkBytes (a transfer: the us200 case, whose decay the refresh is
	// for) clears the backoff, so the first refresh after a transfer is as
	// prompt as before. The hl235 quiet re-init (ccIdleTick) is not backed
	// off: it must run inside BBR's 10 s min-RTT window to keep a quiet link
	// out of PROBE_RTT.
	ccBulkBytes = 2 << 20 // per healthTick: ~8 Mbit/s
	ccBackMin   = 4       // ticks (8 s)
	ccBackMax   = 30      // ticks (60 s)
)

// ccRefreshState: one link's watch (health goroutine only).
type ccRefreshState struct {
	acked   uint64
	light   int
	retrans uint64 // hl235: retransmitted segments at the last tick
	idle    int    // hl235: quiet ticks in a row (ccIdleTick)
	since   int    // pk236: ticks since the last light refresh (capped at ccBackMax)
	back    int    // pk236: ticks a next light refresh waits (0 after a transfer)
}

// hl235 (with HS2_LINK_CC_REFRESH; HS2_LINK_CC_IDLE=0 turns it off): a
// quiet link (under ccIdleBytes acknowledged per tick, nothing queued, no
// retransmission) with a cold estimate (under ccRefreshBwMax) gets the same
// re-initialisation every ccIdleTicks quiet ticks, and at once when it is
// found in PROBE_RTT (pacing and cwnd gain 1).
//
// Root cause (lab hl234 hS1-hS3 u1b, -5.7 %): the stripe's first sub went to
// the pool's set-aside link (set aside ~45 s before, never used). An idle
// link's only traffic is keepalives and pings whose ACKs come delayed, which
// never refresh BBR's 10 s min-RTT filter: it expires, BBR enters PROBE_RTT
// (cwnd 4) and, with no data to finish its round, stays there until the next
// keepalive (lab hl235 iD3 u1b: 10 link samples at cwnd 4, pacing_gain 1,
// cwnd_gain 1). The upload's first sub then ran at cwnd 4 for 200 ms + a
// round on each end: raw handshake 428-438 ms (B 92-182 ms; the exit's reply
// is a 16 KB padded record), first two seconds 134+394 Mbit/s against B's
// 444+482. The re-init restarts the filter's clock (bbr_init: min_rtt_stamp =
// now), leaves PROBE_RTT for STARTUP with cwnd untouched, and sends nothing.
// A warm link (estimate >= 20 Mbit/s) keeps its state: BBR restarts it at
// that rate after idle; a re-init would ramp it from scratch. A link with
// stuck in-flight data retransmits: never quiet.
var ccIdleOff = os.Getenv("HS2_LINK_CC_IDLE") == "0"

const (
	ccIdleTicks = 3       // 6 s of healthTick: under BBR's 10 s min-RTT window
	ccIdleBytes = 4 << 10 // acknowledged per tick: a control ping, not data
)

// ccInfo: what the kernel's congestion control reports.
type ccInfo struct {
	bbr      bool
	gain     uint32 // pacing gain, BBR_UNIT 256
	cwndGain uint32 // hl235: cwnd gain (pacing and cwnd gain 1: PROBE_RTT)
	bwBytes  uint64 // max-filtered bandwidth estimate, bytes/s
}

var ccRefreshStat struct {
	idle     atomic.Int64 // hl235: quiet links re-initialised (min-RTT filter kept fresh)
	probeRTT atomic.Int64 // hl235: of them, found in PROBE_RTT
	n        atomic.Int64 // refreshes done
	fail     atomic.Int64
	held     atomic.Int64 // pk236: light refresh due but waiting out the backoff (ticks)
	logAt    atomic.Int64
	lastN    atomic.Int64
}

// ccRefreshTick (health goroutine): one tick of the watch for a link.
// readCC / reinit are the socket's (stubs in tests).
func ccRefreshTick(st *ccRefreshState, ts tcpStat, readCC func() (ccInfo, bool), reinit func() bool) bool {
	prev := st.acked
	st.acked = ts.bytesAcked
	if st.since < ccBackMax {
		st.since++
	}
	if prev != 0 && ts.bytesAcked >= prev && ts.bytesAcked-prev >= ccBulkBytes {
		st.back = 0 // a transfer: the next light spell refreshes promptly
	}
	if ccIdleTick(st, ts, prev, readCC, reinit) {
		return true
	}
	if prev == 0 || ts.bytesAcked < prev || ts.bytesAcked-prev >= ccLightBytes || ts.notsent > 0 {
		st.light = 0
		return false
	}
	if st.light++; st.light < ccLightTicks {
		return false
	}
	ci, ok := readCC()
	if !ok || !ci.bbr || ci.gain == ccBBRHighGain || ci.gain == 0 || ci.bwBytes >= ccRefreshBwMax { // gain 0: re-initialised, no ACK yet
		return false
	}
	if st.since < st.back {
		ccRefreshStat.held.Add(1)
		return false
	}
	if !reinit() {
		ccRefreshStat.fail.Add(1)
		return false
	}
	ccRefreshStat.n.Add(1)
	st.light, st.since = 0, 0
	st.back = min(max(2*st.back, ccBackMin), ccBackMax)
	return true
}

// ccIdleTick (hl235): the idle half of the watch; true when it re-initialised.
func ccIdleTick(st *ccRefreshState, ts tcpStat, prevAcked uint64, readCC func() (ccInfo, bool), reinit func() bool) bool {
	prevRetrans := st.retrans
	st.retrans = ts.retrans
	if ccIdleOff || prevAcked == 0 || ts.bytesAcked < prevAcked || ts.bytesAcked-prevAcked >= ccIdleBytes ||
		ts.notsent > 0 || ts.retrans != prevRetrans {
		st.idle = 0
		return false
	}
	st.idle++
	ci, ok := readCC()
	if !ok || !ci.bbr || ci.bwBytes >= ccRefreshBwMax {
		// a warm link (estimate >= 20 Mbit/s) keeps it: BBR restarts it at
		// that rate after idle, a re-init would ramp it from scratch
		return false
	}
	probeRTT := ci.gain == ccBBRUnit && ci.cwndGain == ccBBRUnit
	if st.idle < ccIdleTicks && !probeRTT {
		return false
	}
	if !reinit() {
		ccRefreshStat.fail.Add(1)
		return false
	}
	st.idle = 0
	ccRefreshStat.idle.Add(1)
	if probeRTT {
		ccRefreshStat.probeRTT.Add(1)
	}
	st.light = 0
	return true
}

// ccRefreshLink: the watch for one pool link (sampleHealth, outside the lock).
func ccRefreshLink(ml *managedLink, ts tcpStat, ok bool) {
	if !linkCCRefresh.Load() || !ok {
		return
	}
	tc := linkTCPConn(ml.link)
	if tc == nil {
		return
	}
	ccRefreshTick(&ml.ccr, ts, func() (ccInfo, bool) { return tcpCCInfo(tc) }, func() bool { return tcpCCReinit(tc) })
	ccBackRetryAll() // fx247: links a failed reinit left on reno
	ccRefreshLogMaybe()
}

// linkTCPConn: a link's kernel socket, nil when it has none.
func linkTCPConn(l Link) *net.TCPConn {
	if ml, ok := l.(*mtcpLink); ok && ml.tls != nil {
		return ml.tls.TCPConn()
	}
	return nil
}

// ccRefreshLogMaybe: one line a minute while refreshes happen.
func ccRefreshLogMaybe() {
	now := monoNano(time.Now())
	at := ccRefreshStat.logAt.Load()
	if now-at < int64(time.Minute) || !ccRefreshStat.logAt.CompareAndSwap(at, now) {
		return
	}
	n, idle := ccRefreshStat.n.Load(), ccRefreshStat.idle.Load()
	if d := n + idle - ccRefreshStat.lastN.Swap(n+idle); d > 0 {
		log.Printf("link cc refresh: %d link(s) back to a fresh congestion state in the last minute (light total %d, quiet total %d of them in PROBE_RTT %d, failed %d, held by backoff %d)", d, n, idle, ccRefreshStat.probeRTT.Load(), ccRefreshStat.fail.Load(), ccRefreshStat.held.Load())
	}
}

// cc203: the exit (kharej) side's watch. Its link sockets carry the
// downloads and fall into the same state after an upload's bufferbloat or a
// stall (a single download then starts at the light rate). The exit has no
// LinkManager, so its links (direct: accepted, reverse: dialed) are tracked
// here while up and watched on the same 2 s tick with the same rules.
type ccExitWatch struct {
	mu    sync.Mutex
	links map[*net.TCPConn]*ccRefreshState
}

var ccExit = &ccExitWatch{links: map[*net.TCPConn]*ccRefreshState{}}

// track watches an exit link's socket while it is up (nothing when the key
// is off); the returned func forgets it.
func (w *ccExitWatch) track(tc *net.TCPConn) func() {
	if !linkCCRefresh.Load() || tc == nil {
		return func() {}
	}
	w.mu.Lock()
	w.links[tc] = &ccRefreshState{}
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.links, tc)
		w.mu.Unlock()
	}
}

// tick runs one watch tick over the tracked sockets (the syscalls outside
// the lock; a socket's state is touched only by the one run goroutine). It
// returns how many were refreshed.
func (w *ccExitWatch) tick(stats func(*net.TCPConn) (tcpStat, bool), readCC func(*net.TCPConn) (ccInfo, bool), reinit func(*net.TCPConn) bool) int {
	type one struct {
		tc *net.TCPConn
		st *ccRefreshState
	}
	w.mu.Lock()
	ls := make([]one, 0, len(w.links))
	for tc, st := range w.links {
		ls = append(ls, one{tc, st})
	}
	w.mu.Unlock()
	n := 0
	for _, l := range ls {
		ts, ok := stats(l.tc)
		if !ok {
			continue
		}
		tc := l.tc
		if ccRefreshTick(l.st, ts, func() (ccInfo, bool) { return readCC(tc) }, func() bool { return reinit(tc) }) {
			n++
		}
	}
	return n
}

// run: the exit's watch loop (started by the exit server when the key is on).
func (w *ccExitWatch) run(ctx context.Context) {
	t := time.NewTicker(healthTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !linkCCRefresh.Load() {
				continue
			}
			w.tick(tcpStats, tcpCCInfo, tcpCCReinit)
			ccBackRetryAll() // fx247
			ccRefreshLogMaybe()
		}
	}
}
