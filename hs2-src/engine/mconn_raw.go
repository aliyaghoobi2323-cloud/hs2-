package engine

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Raw first sub (HS2_MCONN=lite, both sides announce capMConnRaw).
//
// A lite connection that never gets a second sub must cost what the old path
// costs: relayStream straight on one smux stream. So its first sub carries
// the user's bytes raw — no records, no queue, no reader or writer goroutine,
// no manager: mConn.Write and Read go straight to the stream (one uncontended
// mutex each). Only Iran's rate sample runs, as a timer (no goroutine).
//
// Upgrade (Iran decides, as in lite: mLiteAfter old and both ways under
// mDupMaxRate). Each direction switches the first sub from raw to records at
// a boundary offset, and the reader must know that offset before it reads
// past it, so a side writes no record on the first sub until the peer has
// confirmed it knows the boundary:
//
//  1. Iran opens the second sub (a join) and queues UPGRADE(Bi) on it, Bi =
//     the bytes it wrote raw on the first sub. From then on Iran writes
//     records: on the second sub at once, on the first one gated (queued,
//     not written).
//  2. Kharej reads UPGRADE(Bi) on the second sub: its raw reader of the first
//     sub stops at Bi (a blocked read is woken by a read deadline) and reads
//     records from there. It then queues UPGRADE(Bk) on the second sub, Bk =
//     its own raw bytes, and writes records (the first sub gated).
//  3. Iran reads UPGRADE(Bk): its raw reader stops at Bk; Kharej knows Bi, so
//     Iran's first sub opens, and Iran queues UPACK on both subs.
//  4. Kharej reads UPACK (either sub): Iran knows Bk, its first sub opens.
//
// Any sub dying during the handshake (between 1 and 3 for Iran, 2 and 4 for
// Kharej) ends the connection without an RST (an RST could be read as raw
// bytes): what the peer reads on the first sub is unknown then — the old
// path's outcome for a stream on a dead link. A failed open of the second sub
// changes nothing (the connection stays raw). Closing while still raw both
// ways is closing the stream, as on the old path.
//
// Before the first sub's reader switches, records from the second sub wait in
// reassembly (out of order: they start at the boundary).

type rawState struct {
	raw1       *msub       // the first sub of a raw connection (nil: not raw)
	rawW, rawR atomic.Bool // this side writes / reads the first sub raw
	wmu, rmu   sync.Mutex  // raw writer / raw reader (vs. the switch)
	rawSent    atomic.Int64
	rawRecvd   atomic.Int64
	rLimit     atomic.Int64 // the peer's boundary on the first sub (-1: unknown)
	rawBud     atomic.Int64 // raw bytes already added to liteBud

	// under mConn.mu
	handshaking bool // Iran: steps 1-3; Kharej: steps 2-4
	upgraded    bool // Kharej: UPGRADE seen
	rawTimer    *time.Timer
}

// mconnRawSub is a sub the opener marks as a raw first sub.
type mconnRawSub interface{ mconnRaw() bool }

type readDeadliner interface{ SetReadDeadline(time.Time) error }

// rawCapable: s can be a raw first sub (a blocked read can be woken).
func rawCapable(s any) bool {
	_, ok := s.(readDeadliner)
	return ok
}

// initRawLocked makes s the raw first sub of m (both directions raw).
func (m *mConn) initRawLocked(s *msub) {
	m.raw1 = s
	m.rLimit.Store(-1)
	m.rawW.Store(true)
	m.rawR.Store(true)
	if !m.lite { // the peer is lite (only lite opens raw subs)
		m.lite = true
		m.liteSample = time.Now()
		obs.mconnLiteOpen.Add(1)
	}
	m.setStateLocked(mStateSingle)
}

// newMConnIranRaw: Iran with a raw first sub: no goroutine until an upgrade.
func newMConnIranRaw(id uint64, open mOpener, sub mSub, key any) *mConn {
	m := newMConn(id, true, open)
	m.mu.Lock()
	s := m.addSubLocked(sub, key, false)
	m.initRawLocked(s)
	m.rawTimer = time.AfterFunc(mLitePoll, m.rawTick)
	m.mu.Unlock()
	return m
}

// newMConnExitRaw: Kharej for a raw first sub (attached with
// mconnRoleFirstRaw); no manager until an upgrade.
func newMConnExitRaw(id uint64) *mConn {
	m := newMConn(id, false, nil)
	m.rLimit.Store(-1)
	return m
}

// attachRaw adds the raw first sub (Kharej): no goroutines.
func (m *mConn) attachRaw(sub mSub, onClose func()) {
	m.mu.Lock()
	if m.ended || m.closing || m.raw1 != nil {
		m.mu.Unlock()
		sub.Close()
		if onClose != nil {
			onClose()
		}
		return
	}
	s := m.addSubLocked(sub, nil, false)
	s.onClose = onClose
	m.initRawLocked(s)
	m.sendCond.Broadcast()
	m.mu.Unlock()
}

// writeRaw: done=false when this direction is no longer raw.
func (m *mConn) writeRaw(p []byte) (int, error, bool) {
	m.wmu.Lock()
	if !m.rawW.Load() {
		m.wmu.Unlock()
		return 0, nil, false
	}
	n, err := m.raw1.rw.Write(p)
	m.rawSent.Add(int64(n))
	m.wmu.Unlock()
	if err != nil {
		m.end(err, false)
		return n, err, true
	}
	return n, nil, true
}

// readRaw: done=false when this direction is no longer raw (Read then takes
// reassembly).
func (m *mConn) readRaw(p []byte) (int, error, bool) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	s := m.raw1
	for {
		if !m.rawR.Load() {
			return 0, nil, false
		}
		got, lim := m.rawRecvd.Load(), m.rLimit.Load()
		want := p
		if lim >= 0 {
			if got >= lim {
				m.switchRead(uint64(lim))
				return 0, nil, false
			}
			if int64(len(want)) > lim-got {
				want = want[:lim-got]
			}
		}
		n, err := s.rw.Read(want)
		if n > 0 {
			m.rawRecvd.Add(int64(n))
			return n, nil, true
		}
		if err == nil {
			continue
		}
		if isTimeout(err) {
			// woken on purpose (a boundary came, or Close): clear the
			// deadline BEFORE looking at the state again (setReadLimit sets
			// the state before the deadline)
			if d, ok := s.rw.(readDeadliner); ok {
				d.SetReadDeadline(time.Time{})
			}
			continue
		}
		if !m.rawStop() {
			return 0, nil, false // Close took the reader off raw
		}
		if err == io.EOF {
			m.end(errMConnClosed, false)
			return 0, io.EOF, true
		}
		m.end(err, false)
		return 0, err, true
	}
}

// switchRead: the raw part of the peer's direction is all read (lim bytes);
// the first sub's records from here go through reassembly.
func (m *mConn) switchRead(lim uint64) {
	r := &m.r
	r.mu.Lock()
	if r.next < lim {
		r.next = lim
	}
	r.pullLocked()
	r.lastProgress = time.Now()
	r.cond.Broadcast()
	r.mu.Unlock()
	m.mu.Lock()
	m.rawR.Store(false)
	start := !m.ended && !m.raw1.dead
	m.mu.Unlock()
	m.tr.ev("raw: reader switched at %d", lim)
	if start {
		go m.raw1.readLoop()
	} else {
		m.raw1.close()
	}
}

// rawStop takes the reader off raw (true if it was): a blocked raw read is
// woken, and the first sub closed if the connection already ended (end left
// it to the reader).
func (m *mConn) rawStop() bool {
	m.mu.Lock()
	was := m.rawR.Swap(false)
	ended := m.ended
	m.mu.Unlock()
	if was {
		m.interruptRead()
		if ended {
			m.raw1.close()
		}
	}
	return was
}

func (m *mConn) interruptRead() {
	if d, ok := m.raw1.rw.(readDeadliner); ok {
		d.SetReadDeadline(time.Unix(1, 0))
	}
}

// setReadLimit: the peer's raw bytes on the first sub end at off.
func (m *mConn) setReadLimit(off uint64) {
	m.rLimit.Store(int64(off))
	m.interruptRead()
}

// rawClose (Close): raw both ways is closing the stream, as on the old path;
// otherwise the reader leaves raw and Close goes on as usual.
func (m *mConn) rawClose() {
	if m.raw1 == nil {
		return
	}
	m.mu.Lock()
	pure := m.rawW.Load() && !m.ended
	if pure {
		m.rawW.Store(false)
	}
	m.mu.Unlock()
	m.rawStop()
	if pure {
		m.end(errMConnClosed, false)
	}
}

// rawEnded (end): stop the sampling timer, count the last raw bytes.
func (m *mConn) rawEnded() {
	if m.raw1 == nil {
		return
	}
	m.mu.Lock()
	if m.rawTimer != nil {
		m.rawTimer.Stop()
	}
	m.mu.Unlock()
	m.rawBudget(time.Now())
}

// rawBudget adds the raw bytes moved since the last call (both ways) to the
// duplication budget's denominator.
func (m *mConn) rawBudget(now time.Time) {
	tot := m.rawSent.Load() + m.rawRecvd.Load()
	if d := tot - m.rawBud.Swap(tot); d > 0 {
		liteBud.add(now.Unix(), int(d), 0)
	}
}

// rawTick (Iran, timer): the lite upgrade rule while the first sub is raw.
func (m *mConn) rawTick() {
	now := time.Now()
	m.rawBudget(now)
	m.mu.Lock()
	if m.ended || m.closing || !m.rawW.Load() || m.lightOpening {
		m.mu.Unlock()
		return
	}
	m.sendOff = uint64(m.rawSent.Load())
	if m.liteWantDualLocked(now, uint64(m.rawRecvd.Load())) {
		switch {
		case liteBud.capped(now.Unix()):
			obs.mconnLiteCapped.Add(1)
			m.tr.ev("lite: upgrade refused (duplication cap)")
		case now.Before(m.lightRetryAt):
		default:
			m.lightOpening = true
			avoid := append([]any{m.raw1.key}, m.deadKeys...)
			m.mu.Unlock()
			obs.mconnLiteUpgrade.Add(1)
			m.upgradeRaw(avoid) // in the timer's own goroutine
			return
		}
	}
	m.rawTimer.Reset(mLitePoll)
	m.mu.Unlock()
}

// upgradeRaw (Iran): step 1.
func (m *mConn) upgradeRaw(avoid []any) {
	sub, key, err := m.open(planeLight, avoid)
	if err != nil {
		m.mu.Lock()
		m.lightOpening = false
		m.lightRetryAt = time.Now().Add(mReopenBackoff)
		m.deadKeys = nil
		if !m.ended {
			m.rawTimer.Reset(mLitePoll) // still raw: keep sampling
		}
		m.mu.Unlock()
		return
	}
	m.wmu.Lock() // a raw write in flight ends before the boundary is taken
	m.mu.Lock()
	m.lightOpening = false
	if m.ended || m.closing || !m.rawW.Load() {
		m.mu.Unlock()
		m.wmu.Unlock()
		sub.Close()
		return
	}
	bi := uint64(m.rawSent.Load())
	m.sendOff = bi
	s2 := m.addSubLocked(sub, key, false)
	c := getChunk(mRecUpgrade)
	c.setOff(bi)
	s2.push(c)
	m.raw1.gated = true
	m.handshaking = true
	m.rawW.Store(false)
	m.setStateLocked(mStateDual)
	m.tr.ev("raw: upgrade, UPGRADE(%d) on sub %d (recvd=%d)", bi, s2.idx, m.rawRecvd.Load())
	m.mu.Unlock()
	m.wmu.Unlock()
	go m.raw1.writeLoop()
	m.startSub(s2)
	go m.manage()
}

// onUpgrade: an UPGRADE record on s (false: a protocol error).
func (m *mConn) onUpgrade(s *msub, off uint64) bool {
	m.mu.Lock()
	if m.raw1 == nil || s == m.raw1 {
		m.mu.Unlock()
		return false
	}
	if m.iran { // step 3
		if !m.handshaking {
			m.mu.Unlock()
			return true
		}
		m.mu.Unlock()
		m.setReadLimit(off) // before UPACK lets the peer write records on the first sub
		m.mu.Lock()
		m.handshaking = false
		if m.raw1.gated {
			m.raw1.gated = false
			select {
			case m.raw1.kick <- struct{}{}:
			default:
			}
		}
		if !m.ended {
			if c := getChunk(mRecUpAck); m.pushAllLocked(c) == 0 {
				putChunk(c)
			}
		}
		m.tr.ev("raw: UPGRADE(%d) from the peer, first sub open", off)
		m.mu.Unlock()
		return true
	}
	if m.upgraded { // step 2
		m.mu.Unlock()
		return true
	}
	m.upgraded, m.handshaking = true, true
	m.mu.Unlock()
	m.setReadLimit(off) // before our UPGRADE tells Iran we know it
	go m.upgradeSendExit(s)
	return true
}

// upgradeSendExit (Kharej): the second half of step 2.
func (m *mConn) upgradeSendExit(s2 *msub) {
	m.wmu.Lock()
	m.mu.Lock()
	if m.ended || m.closing || !m.rawW.Load() || s2.dead {
		m.mu.Unlock()
		m.wmu.Unlock()
		return
	}
	bk := uint64(m.rawSent.Load())
	m.sendOff = bk
	c := getChunk(mRecUpgrade)
	c.setOff(bk)
	s2.push(c)
	m.raw1.gated = true
	m.rawW.Store(false)
	m.setStateLocked(mStateDual)
	m.tr.ev("raw: UPGRADE(%d) on sub %d", bk, s2.idx)
	m.mu.Unlock()
	m.wmu.Unlock()
	go m.raw1.writeLoop()
	go m.manage()
}

// onUpAck (Kharej): step 4.
func (m *mConn) onUpAck() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.iran || m.raw1 == nil || !m.handshaking || !m.raw1.gated {
		return
	}
	m.handshaking = false
	m.raw1.gated = false
	select {
	case m.raw1.kick <- struct{}{}:
	default:
	}
	m.tr.ev("raw: UPACK, first sub open")
}
