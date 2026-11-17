package engine

import (
	"os"
	"sync/atomic"
	"time"
)

// r68fix: HS2_MCONN_CALL_RESEND (default on, "0" = the old rule; each side
// alone) — a call's chunks are kept until TCP acked them and a dead sub's
// unacked ones are queued again on a live sub, as HS2_MCONN_STRIPE_RESEND
// does for a striped download.
//
// Why (rel68 soak, lab3 r68ft B1 minute 14, reproduced on lab1 r68fix C*):
// a call with both subs on a path that goes dark gets a fresh sub from the
// stuck-call trigger (lb_call_stuck.go), and its old subs are given up with
// a CLOSE-REQ — sent on those very subs, so on the dark path it never
// arrives. The peer keeps them as live subs; 10-20 s later their sockets time
// out, and the peer's subGone finds the only sub left joined with a hole
// (holeRecent) or the dead sub holding the last copy of a chunk: errMConnLost,
// an RST — the call is cut (the probe's call then gives no sample for the
// rest of the run). On this side, a given-up sub dying counted as a clean
// close (closedCleanLocked), so the chunks only it carried were never sent
// again and the peer's reassembly reset the call 30 s later ("missing bytes
// never arrived"; rel67b too). The bytes are still here (a call is ~2 Mbit/s,
// a few hundred KB in flight at most): sending them again on the fresh sub
// (the receiver drops what it already has, by offset, as for any copy) keeps
// the call alive.
var mconnCallResend atomic.Bool

func init() { mconnCallResend.Store(os.Getenv("HS2_MCONN_CALL_RESEND") != "0") }

// callResendStat: sub deaths a call survived by the resend.
var callResendStat atomic.Uint64

// callKeepLocked (under m.mu): m is a lite call (call-like traffic seen on
// this side) whose chunks are kept for a resend.
func (m *mConn) callKeepLocked() bool {
	return mconnCallResend.Load() && stripeResend.Load() && m.lite && m.stN == 0 && !m.callLikeAt.IsZero()
}

// callUncleanLocked (under m.mu): s was given up (CLOSE-REQ) but its close
// handshake never completed — the peer did not confirm it read all s carried.
func (m *mConn) callUncleanLocked(s *msub) bool {
	if !m.iran || !s.closeHS || s.closeDone {
		return false
	}
	if !fx212SubResend.Load() || m.callKeepLocked() {
		return m.anyKeepLocked() // r68fix2: any kept conn
	}
	// fx212 (an210 Q1 #2): a sub-resend connection's given-up sub resends
	// only if it may hold undelivered bytes (subUndeliveredLocked): Iran
	// retires lite subs as clean, and r68fix2 made every one of them resend
	// duplicates of delivered bytes (lab K1: 43,122 vs Kharej 2,381).
	return m.anyKeepLocked() && m.subUndeliveredLocked(s, time.Now())
}
