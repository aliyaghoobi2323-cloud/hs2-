package engine

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// After the wedge guard releases a link's stalled relays (wedge.go), does the
// link move again at once? On the loopback (64 KiB MSS) the other side's TCP
// sometimes sat in retransmission-timeout backoff for seconds after the
// release, so the link's other users kept waiting. This only MEASURES whether
// that happens in production; it changes nothing.
//
// A wedge stops the direction peer → us: our smux reader stopped taking the
// socket's data, so the peer's sender met a zero window. Forward progress is
// therefore new data arriving from the peer (tcpi_bytes_received grows). After
// a release the peer always has some to send — at least the FIN frames its own
// relays answer the reset streams with — so "nothing arrives" means the peer's
// sender is stuck, not that the link is idle. Our own bytes_acked is not used:
// the FIN frames we send for the reset streams are acked at once and would
// hide the stall. Our socket's backoff, CA state and rto are recorded too (it
// is in backoff when OUR sending is the stuck part).
const (
	releaseWatchFor  = 10 * time.Second      // how long one watch lasts at most
	releaseWatchStep = 50 * time.Millisecond // TCP_INFO reads while watching
	releaseStallMin  = time.Second           // no progress for this long: a stall
	releaseLogEvery  = wedgeLogEvery         // the stall line's rate limit
	tcpCALoss        = 4                     // TCP_CA_Loss (include/net/tcp.h)
)

// releaseWatch is what one watch saw.
type releaseWatch struct {
	waited     time.Duration // release → first new data from the peer (or the whole watch)
	resumed    bool          // data arrived before the watch ended
	backoff0   uint8         // our socket's backoff at the release
	maxBackoff uint8         // the highest backoff seen while watching
	loss       bool          // our socket was in the Loss CA state at some read
	maxRtoMs   uint32        // the highest rto seen, ms
}

// watchRelease watches a link's socket after a release: read returns its
// TCP_INFO, sleep waits one step, now is the clock. It returns false when
// TCP_INFO cannot be read (non-Linux, a bare session, a closed socket).
func watchRelease(read func() (tcpStat, bool), sleep func(time.Duration), now func() time.Time) (releaseWatch, bool) {
	var r releaseWatch
	st, ok := read()
	if !ok {
		return r, false
	}
	start := now()
	base := st.bytesRcvd
	r.backoff0 = st.backoff
	note := func(st tcpStat) {
		if st.backoff > r.maxBackoff {
			r.maxBackoff = st.backoff
		}
		if st.caState == tcpCALoss {
			r.loss = true
		}
		if ms := st.rtoUs / 1000; ms > r.maxRtoMs {
			r.maxRtoMs = ms
		}
	}
	note(st)
	for {
		sleep(releaseWatchStep)
		st, ok := read()
		r.waited = now().Sub(start)
		if !ok { // the link died: what it waited until then stands
			return r, true
		}
		note(st)
		if st.bytesRcvd != base {
			r.resumed = true
			return r, true
		}
		if r.waited >= releaseWatchFor {
			return r, true
		}
	}
}

// stalled reports whether the watch counts as a post-release stall.
func (r releaseWatch) stalled() bool { return r.waited >= releaseStallMin }

// releaseLog rate-limits the stall line (process-wide).
var releaseLog struct {
	mu      sync.Mutex
	at      time.Time
	dropped int
}

// noteRelease counts one release (a guard look that reset stuck relays on a
// wedged link) and starts the link's watch unless one is already running.
func (g *sessGuard) noteRelease() {
	obs.wedgeReleases.Add(1)
	if g.tcp == nil || !g.watching.CompareAndSwap(false, true) {
		return
	}
	tc := g.tcp
	go func() {
		defer g.watching.Store(false)
		r, ok := watchRelease(func() (tcpStat, bool) { return tcpStats(tc) }, time.Sleep, time.Now)
		if !ok || !r.stalled() {
			return
		}
		ms := r.waited.Milliseconds()
		obs.postReleaseStall.Add(1)
		for {
			old := obs.postReleaseStallMaxMs.Load()
			if ms <= old || obs.postReleaseStallMaxMs.CompareAndSwap(old, ms) {
				break
			}
		}
		g.logStall(r, ms)
	}()
}

func (g *sessGuard) logStall(r releaseWatch, ms int64) {
	lp := guards.logf.Load()
	if lp == nil {
		return
	}
	releaseLog.mu.Lock()
	now := time.Now()
	if now.Sub(releaseLog.at) < releaseLogEvery {
		releaseLog.dropped++
		releaseLog.mu.Unlock()
		return
	}
	more := releaseLog.dropped
	releaseLog.at, releaseLog.dropped = now, 0
	releaseLog.mu.Unlock()

	line := fmt.Sprintf("mtcp: link %s: %d ms without progress after releasing a stalled relay (rto %d ms, backoff %d)",
		g.linkName(), ms, r.maxRtoMs, r.maxBackoff)
	if r.backoff0 > 0 || r.loss {
		line += fmt.Sprintf(" — this side's TCP was in backoff/loss (backoff %d at the release, loss state %v)", r.backoff0, r.loss)
	}
	if !r.resumed {
		line += fmt.Sprintf(" — nothing arrived from the other side in %s", fmtDur(r.waited))
	}
	if more > 0 {
		line += fmt.Sprintf(" (+%d more since the last such line)", more)
	}
	(*lp)("%s", line)
}

// linkName is the link's number on the edge (its link manager sets it), else
// the socket's addresses.
func (g *sessGuard) linkName() string {
	if id := g.linkID.Load(); id > 0 {
		return fmt.Sprint(id - 1)
	}
	if g.tcp != nil {
		return g.tcp.LocalAddr().String() + ">" + g.tcp.RemoteAddr().String()
	}
	return "?"
}

// setGuardLinkID names a managed link's guard for the stall line.
func setGuardLinkID(mtr *linkMeter, id int) {
	if mtr != nil && mtr.guard != nil {
		mtr.guard.linkID.Store(int32(id) + 1)
	}
}

// tcpOfConn finds the kernel socket beneath a link's TLS connection (nil when
// there is none, as in tests over net.Pipe).
func tcpOfConn(c net.Conn) *net.TCPConn {
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
