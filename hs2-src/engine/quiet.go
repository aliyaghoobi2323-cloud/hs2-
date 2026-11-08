package engine

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// HS2_QUIET_IDLE=1: an idle link is almost silent, like a browser's idle
// HTTPS connection, instead of beating every few seconds (TCP keepalive 3 s,
// smux keepalive 4–8 s each way, control ping 9–15 s, TUN keepalive ~10 s
// each way). Only links with NO users go quiet; a link with users behaves
// exactly as without the key, so black-hole detection on it (suspect 12 s,
// stuck, the loss rule, the TUN 12 s rule, TCP_USER_TIMEOUT 20 s, the 24 s
// keepalive timeout) is as fast as before.
//
// Both servers must agree, so a side goes quiet only towards a peer that
// announced capQuietIdle over kindInfo (it runs the key too, so its own
// silence limits allow it); towards an older peer nothing changes.
//
// Who is idle. The edge decides: a link is idle on the edge when it carries
// no user stream and no TUN packet for quietL3Recent. The edge tells the exit
// in every control ping (the top bit of the ping's sequence number; an older
// exit only echoes it) and pings at once whenever that changes. The exit goes
// quiet only while the edge said idle AND it has no user of its own; it
// loosens its silence limits only while the edge said idle — so whenever the
// edge has users, both sides keep today's cadence and limits. A newly built
// link counts as busy for quietL3Recent (both sides have exchanged kindInfo
// by then).
//
// On a quiet idle link, per direction: the control ping / pong every 20–40 s,
// an smux keepalive only if nothing else was written for 35–50 s, a TUN
// keepalive every 45–65 s, and TCP keepalives after 45 s (tlscarrier). The
// silence limits are then quietSilent (80 s) for the session and TUN
// watchers and the edge's suspect check, and quietL3ReadDead for the TUN
// reader. When a link turns busy again its watchers measure silence from that
// moment, so the first user does not find a link "silent for 30 s".
//
// The session watchdog replaces smux's own keepalive timeout (which cannot
// change during a session): today's 24 s while the link is busy or the peer
// is older, quietSilent while it is idle.
var quietIdle = os.Getenv("HS2_QUIET_IDLE") == "1"

// capQuietIdle (kindInfo caps): this side runs HS2_QUIET_IDLE — its silence
// limits allow the other side to go quiet on idle links. Only sent with the
// key on, so the kindInfo bytes are unchanged without it.
const capQuietIdle = 1 << 7

func quietCap() uint8 {
	if quietIdle {
		return capQuietIdle
	}
	return 0
}

// quietIdlePeer reports whether the other server announced capQuietIdle.
func (pi *peerInfo) quietIdlePeer() bool { return pi != nil && pi.V2 && pi.Caps&capQuietIdle != 0 }

// Quiet cadences and limits. Variables so tests can shorten them.
var (
	quietPingMin, quietPingMax = 20 * time.Second, 40 * time.Second // edge control ping
	quietNOPMin, quietNOPMax   = 35 * time.Second, 50 * time.Second // smux keepalive, if nothing else went
	quietL3Min, quietL3Max     = 45 * time.Second, 65 * time.Second // TUN keepalive
	quietSilent                = 80 * time.Second                   // silence limit of an idle link
	quietL3ReadDead            = 100 * time.Second                  // TUN reader deadline, quiet peer
	quietL3Recent              = 30 * time.Second                   // a TUN packet keeps a link busy this long
	quietSessTimeout           = 24 * time.Second                   // today's smux KeepAliveTimeout
	quietWatchTick             = 2 * time.Second
)

// quietSmuxBackstop is smux's own keepalive timeout with the key on: the
// session watchdog does the real work; this only catches a watchdog that is
// gone.
const quietSmuxBackstop = 10 * time.Minute

// ctrlIdleBit is the top bit of a control ping's sequence: the edge is idle.
const ctrlIdleBit = uint64(1) << 63

func between(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(rand.Int64N(int64(hi-lo)))
}

// quietState is one link's quiet-idle state (linkMeter.quiet; nil without the
// key).
type quietState struct {
	edge bool
	mtr  *linkMeter
	// users reports whether the link has user connections (edge: user
	// streams; exit: user connections). nil: unknown, counted as users.
	users atomic.Pointer[func() bool]
	// l3At: ctrlNow when a TUN packet last went over the link either way.
	l3At atomic.Int64
	// peerIdle (exit): the edge's last control ping said it is idle.
	peerIdle atomic.Bool

	// The NOP filter's state; only smux's send loop touches it.
	lastWrite time.Time
	nopGap    time.Duration
}

func newQuietState(mtr *linkMeter, edge bool) *quietState {
	q := &quietState{edge: edge, mtr: mtr, lastWrite: time.Now(), nopGap: between(quietNOPMin, quietNOPMax)}
	q.l3At.Store(ctrlNow()) // a new link is busy at first
	return q
}

func (q *quietState) setUsers(f func() bool) { q.users.Store(&f) }

func (q *quietState) hasUsers() bool {
	f := q.users.Load()
	return f == nil || (*f)()
}

func (q *quietState) peerQuiet() bool { return q.mtr != nil && q.mtr.peerInfo.Load().quietIdlePeer() }

// markL3 notes a TUN packet over the link.
func (q *quietState) markL3() {
	if q != nil {
		q.l3At.Store(ctrlNow())
	}
}

func (q *quietState) l3Recent() bool {
	return time.Duration(ctrlNow()-q.l3At.Load()) < quietL3Recent
}

// edgeIdle: the edge's own view (what its pings announce).
func (q *quietState) edgeIdle() bool { return !q.hasUsers() && !q.l3Recent() }

// sendQuiet: this side may stretch its keepalives on the link now.
func (q *quietState) sendQuiet() bool {
	if q == nil || !q.peerQuiet() {
		return false
	}
	if q.edge {
		return q.edgeIdle()
	}
	return q.peerIdle.Load() && !q.hasUsers() && !q.l3Recent()
}

// lenient: this side's silence limits may be the idle ones now — the other
// side may be quiet. The edge's sending follows only its own idleness, so the
// exit loosens only while the edge said idle.
func (q *quietState) lenient() bool {
	if q == nil || !q.peerQuiet() || q.l3Recent() {
		return false
	}
	if q.edge {
		return !q.hasUsers()
	}
	return q.peerIdle.Load()
}

// quietWatch is one silence watcher's view of lenient/strict changes: after a
// switch to strict, silence counts from the switch.
type quietWatch struct {
	was    bool
	strict time.Time
}

// ref returns the time silence is measured from: last, or the moment the
// link turned strict if that is later.
func (w *quietWatch) ref(lenient bool, last, now time.Time) time.Time {
	if w.was && !lenient {
		w.strict = now
	}
	w.was = lenient
	if w.strict.After(last) {
		return w.strict
	}
	return last
}

// quietConn sits between smux and the link (only with the key): it drops
// smux's keepalive (NOP) frames while the link is quiet, unless nothing else
// was written for the NOP gap. smux writes one whole frame per Write from one
// goroutine (its send loop), so a NOP is exactly an 8-byte write with
// cmd = NOP and stream id 0.
type quietConn struct {
	net.Conn
	q *quietState
}

const smuxCmdNOP = 3 // smux's cmdNOP

func isSmuxNOP(p []byte) bool {
	return len(p) == 8 && p[1] == smuxCmdNOP && p[2] == 0 && p[3] == 0 && p[4] == 0 && p[5] == 0 && p[6] == 0 && p[7] == 0
}

func (c *quietConn) Write(p []byte) (int, error) {
	now := time.Now()
	if isSmuxNOP(p) && c.q.sendQuiet() {
		if now.Sub(c.q.lastWrite) < c.q.nopGap {
			return len(p), nil // not sent: the link said something recently enough
		}
		c.q.nopGap = between(quietNOPMin, quietNOPMax)
	}
	n, err := c.Conn.Write(p)
	c.q.lastWrite = now
	return n, err
}

// watch is the session watchdog. It is smux's keepalive timeout, rule for
// rule: time is cut into windows of the limit from the session's start, and
// the session closes at the end of a window in which nothing was read — so
// silence is caught after one to two limits, as smux does (24–48 s on a busy
// link). A reader not in a read (smux parked on a full receive buffer) is
// not silence, as in smux. Only the window length differs: quietSilent while
// the link is quiet-idle, and a switch back to busy starts a fresh window.
func (q *quietState) watch(sess *smux.Session, w *watchConn) {
	t := time.NewTicker(quietWatchTick)
	defer t.Stop()
	last := w.rdCalls.Load()
	winStart, seen, wasLax := time.Now(), false, false
	who := "the exit"
	if !q.edge {
		who = "the edge"
	}
	for {
		select {
		case <-sess.CloseChan():
			return
		case now := <-t.C:
			if n := w.rdCalls.Load(); n != last || !w.inRead.Load() {
				last, seen = n, true
			}
			lax := q.lenient()
			if wasLax && !lax { // busy again: a whole busy window from now
				winStart, seen = now, false
			}
			wasLax = lax
			limit := quietSessTimeout
			if lax {
				limit = quietSilent
			}
			if now.Sub(winStart) >= limit {
				if !seen {
					w.expire(fmt.Sprintf("no data from %s for %s (keepalive timeout — path stalled)", who, limit))
					return
				}
				winStart, seen = now, false
			}
		}
	}
}

// keepaliveTimeoutWords is the busy-link keepalive timeout, for the log.
func keepaliveTimeoutWords() string {
	if quietIdle {
		return quietSessTimeout.String()
	}
	return newSmuxConfig().KeepAliveTimeout.String()
}

// quietOf is a link meter's quiet state (nil: none, or the key is off).
func quietOf(m *linkMeter) *quietState {
	if m == nil {
		return nil
	}
	return m.quiet
}
