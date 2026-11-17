package engine

import (
	"io"
	"weak"
)

// mem68: a long-lived connection must not keep a dead link alive.
//
// A connection remembers the links its last subs died on (deadKeys) so a
// replacement open avoids them. On Iran those keys are the links themselves
// (*mtcpLink), and a link holds its whole stack: the smux session, the TLS
// carrier with its read and record buffers, the shaped conn's frame buffers.
// The soak (idle users that stay for hours, 30% of the links killed every
// 3 min) showed it: smux Session objects grew by ~45 after every kill on
// both sides with HS2_MCONN=lite and stayed flat with HS2_MCONN=off, while
// the live sessions stayed ~155 — each dead link pinned by the deadKeys (and,
// through the dead sub, the call-score slots) of the connections that had a
// sub on it, i.e. until the last of those users left.
//
// A dead link is only worth avoiding while it is still around, so deadKeys
// holds it weakly: once nothing else references it, it is collected and its
// entry simply drops out of the avoid list.

// deadLinkKey is a deadKeys entry for an *mtcpLink, held weakly.
type deadLinkKey struct{ w weak.Pointer[mtcpLink] }

// noteDeadKeyLocked (under m.mu) remembers key, the link a sub just died on.
func (m *mConn) noteDeadKeyLocked(key any) {
	if key == nil {
		return
	}
	if l, ok := key.(*mtcpLink); ok {
		key = deadLinkKey{w: weak.Make(l)}
	}
	if len(m.deadKeys) >= mDeadKeys {
		// shift in place: re-slicing [1:] would leave the oldest entry
		// referenced by the backing array
		n := copy(m.deadKeys, m.deadKeys[1:])
		m.deadKeys[n] = nil
		m.deadKeys = m.deadKeys[:n]
	}
	m.deadKeys = append(m.deadKeys, key)
}

// deadAvoidLocked (under m.mu) returns the remembered dead links still
// alive, as avoid entries (their IDs, mconn_avoidkey.go). It allocates only when an
// entry is weak.
func (m *mConn) deadAvoidLocked() []any {
	weakSeen := false
	for _, k := range m.deadKeys {
		if _, ok := k.(deadLinkKey); ok {
			weakSeen = true
			break
		}
	}
	if !weakSeen {
		return m.deadKeys
	}
	out := make([]any, 0, len(m.deadKeys))
	for _, k := range m.deadKeys {
		if dk, ok := k.(deadLinkKey); ok {
			if l := dk.w.Value(); l != nil {
				out = append(out, l.avoidID()) // mem70: an ID, not the link
			}
			continue
		}
		out = append(out, k)
	}
	return out
}

// forgetScoreSubLocked (under m.mu) clears s from the call-score slots
// (HS2_MCONN_CALL): a dead sub must not stay referenced there until the next
// rescoring, which an idle connection may never do.
func (m *mConn) forgetScoreSubLocked(s *msub) {
	for i := range m.scoreSub {
		if m.scoreSub[i] == s {
			m.scoreSub[i] = nil
		}
	}
}

// ---- the raw first sub (mconn_raw.go) ----------------------------------------
//
// The root of the growth: m.raw1 stays set for the connection's whole life
// (it also marks the connection as raw), so once the first sub died on a
// killed link while the upgraded connection lived on, the dead sub kept its
// stream, and the stream its link (Iran: countedStream.link, key) or session
// (exit), with every buffer under them. The soak's pin scan: right after one
// kill, 990 of ~6850 live connections on each side held a dead raw1 — exactly
// the sessions above the live ones.
//
// mem69, the second holder (heap dump of the exit after two kills: 88 of 94
// dead sessions): the exit's raw1 also kept onClose, a closure over its
// exitConn → linkMeter (.guard → sessGuard → Session, .tcp); it is dropped
// with the stream.
//
// Once raw1 is dead, closed, its loops gone and neither direction raw any
// more, nothing uses its stream or key again: they are replaced by a closed
// stand-in and nil. The raw paths read m.raw1 under m.rmu (reader) and m.wmu
// (writer), so the swap holds both (order rmu → wmu → mu, as theirs); the
// unlocked interrupt of the old deadline path is why it needs
// HS2_MCONN_DEADLINE_FIX (on in production).

// deadSubRW stands in for a released sub's stream.
type deadSubRW struct{}

func (deadSubRW) Read([]byte) (int, error)  { return 0, io.ErrClosedPipe }
func (deadSubRW) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (deadSubRW) Close() error              { return nil }

func (m *mConn) subLoopEnter(s *msub) {
	connLoops.Add(1)
	m.mu.Lock()
	s.loops++
	m.mu.Unlock()
}

func (m *mConn) subLoopExit(s *msub) {
	m.mu.Lock()
	s.loops--
	last := s.loops == 0 && s == m.raw1
	m.mu.Unlock()
	if last {
		m.releaseRaw1()
	}
	connLoops.Add(-1)
}

// releaseRaw1 drops what a dead raw first sub holds (see above); a no-op
// until every condition holds.
func (m *mConn) releaseRaw1() {
	if !mconnDeadlineFix.Load() || m.rawR.Load() || m.rawW.Load() {
		return
	}
	m.rmu.Lock()
	m.wmu.Lock()
	m.mu.Lock()
	if s := m.raw1; s != nil && s.dead && s.loops == 0 && s.rwClosed.Load() &&
		!m.rawR.Load() && !m.rawW.Load() {
		if _, done := s.rw.(deadSubRW); !done {
			// mem69: onClose has run (rwClosed is set after it); on the
			// exit its closure holds the sub's exitConn, whose link meter
			// holds the session guard and so the dead session.
			s.rw, s.key, s.onClose = deadSubRW{}, nil, nil
			obs.mconnRaw1Released.Add(1)
		}
	}
	m.mu.Unlock()
	m.wmu.Unlock()
	m.rmu.Unlock()
}
