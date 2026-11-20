package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// b312 (sc307/b305 scale tests): HS2_B312_OPENNOLOCK (default on; =0: the
// old path, every open holds e.mu from the pick to the install).
//
// mconnEdge.open held the edge's lock across the whole placement:
// pickPlaneCall (LinkManager.mu, call marks, cs229/cs286), its retry sleeps
// (mconnPickGap, up to mconnNextWait when no link fits), OpenStream (a SYN
// queued on the link's writer) and the header write. Everything else that
// takes e.mu waited behind it: the other plane's open (light, bulk and
// stripe opens run in their own goroutines), forget() from every sub's
// Close (the manager, the reader, the writer, the retire), and end(). At
// 3x-equivalent load that was ~3000 s of wait per 20 s on Iran.
//
// What e.mu protects: first (the first sub must be role 0 and is the only
// open whose role and pick depend on edge state), subs/opened (the install)
// and ended (no sub is installed after end). Once first is true it never
// goes back, and a join's role, pick and header depend only on the open's
// own arguments and the link. So with the key on, an open that finds first
// already true (and the edge not ended) drops the lock, picks, opens the
// stream and writes the header exactly as before, and then takes e.mu only
// to install: re-check ended; if the edge ended meanwhile, the stream is
// closed and the reservation released (rel) and the open fails as an open
// that came after end did (errMConnNoLink). That is the interleaving "the
// open installed, then end closed it" seen from the exit, so no new state is
// reachable. The first open, and an open that races it, keep the old locked
// path. The stripe open (one try, no waiting) is split the same way.
//
// Kept exact: one sub per successful open (installed once, opened++ once,
// the plane gauge +1 once), one rel() per failed pick, the pick itself
// (pickPlaneCall/pickStripeWhy are untouched, so light-before-heavy, call
// marks and the cs286 copy placement are the same), the copy mark on light
// joins, the mem70 wait bound (counted from the call), and Close/forget.
var b312OpenNoLock atomic.Bool

func init() { b312OpenNoLock.Store(envBoolDefault("HS2_B312_OPENNOLOCK", true)) }

// b312Open counts the paths (mem line " b312open: ...").
var b312Open struct {
	locked atomic.Int64 // opens on the old locked path (first sub, key off)
	nolock atomic.Int64 // opens that picked without e.mu
	ended  atomic.Int64 // ... whose edge ended before the install (sub closed, rel run)
}

// b312OpenLine: " b312open: nolock=N locked=N ended=N" on the mem line once
// an unlocked open ran.
func b312OpenLine() string {
	n := b312Open.nolock.Load()
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" b312open: nolock=%d locked=%d ended=%d", n, b312Open.locked.Load(), b312Open.ended.Load())
}

// openNoLock is open for a join sub (e.first already true) without e.mu.
// The loop is the old one's join branch line for line.
func (e *mconnEdge) openNoLock(p mPlane, avoid []any, fresh, rstJ bool, t0 time.Time) (mSub, any, error) {
	b312Open.nolock.Add(1)
	deadline := t0.Add(mconnNextWait)
	for tries := 0; !e.endedA.Load() && tries < 3; {
		l, rel, ok := e.lm.pickPlaneCall(p, avoid) // HS2_MCONN_CALL_AVOID (mconn_call.go)
		if !ok {
			if time.Now().After(deadline) || !sleepCtx(e.ctx, mconnPickGap) {
				break
			}
			continue
		}
		tries++ // a link can die between the pick and the open: try another
		st, err := l.OpenStream()
		if err == nil {
			autoWinOffForStripe(e.lm, st) // HS2_SMUX_AUTOWIN (smux_autowin.go)
			dl76Mark(st)
			role := mconnRoleJoin
			if fresh {
				role = mconnRoleRescueJoin
			}
			if rstJ && rstLinkOK(l) {
				role = mconnRoleRstJoin // rr156: the exit answers it, with an RST when it has no conn
			}
			if _, err = st.Write(mconnHeader(e.port, e.id, role)); err == nil {
				if p == planeLight {
					if cs, ok := st.(*countedStream); ok {
						cs.markCopy() // a copy of the first sub's bytes: not demand
					}
				}
				s := &mconnSub{stream: st, plane: p, release: rel, closeHS: peerCloseHS(l), edge: e}
				if !e.install(s) {
					st.Close()
					rel()
					return nil, nil, errMConnNoLink
				}
				e.zduPinTo(l)
				return s, l, nil
			}
			st.Close()
		}
		rel()
	}
	return nil, nil, errMConnNoLink
}

// install adds an opened sub under e.mu; false when the edge ended since
// the open dropped the lock (the caller closes the stream and runs rel).
func (e *mconnEdge) install(s *mconnSub) bool {
	e.mu.Lock()
	if e.ended {
		e.mu.Unlock()
		b312Open.ended.Add(1)
		return false
	}
	mconnPlaneGauge(s.plane).Add(1)
	e.subs = append(e.subs, s)
	e.opened++
	e.mu.Unlock()
	return true
}

// openStripeNoLock is openStripe without e.mu across the pick (the caller
// checked first && !ended under it).
func (e *mconnEdge) openStripeNoLock(avoid []any) (mSub, any, error) {
	b312Open.nolock.Add(1)
	l, rel, ok, held := e.lm.pickStripeWhy(avoid)
	if held {
		return nil, nil, errStripeSteerHeld // sd151: no retry spin
	}
	if !ok {
		return nil, nil, errMConnNoLink
	}
	st, err := l.OpenStream()
	if err == nil {
		autoWinOffForStripe(e.lm, st) // HS2_SMUX_AUTOWIN (smux_autowin.go)
		dl76Mark(st)
		if _, err = st.Write(mconnHeader(e.port, e.id, mconnRoleJoin)); err == nil {
			s := &mconnSub{stream: st, plane: planeStripe, release: rel, evacHS: peerCloseHS(l), edge: e}
			if e.install(s) {
				e.zduPinTo(l)
				return s, l, nil
			}
		}
		st.Close()
	}
	rel()
	return nil, nil, errMConnNoLink
}
