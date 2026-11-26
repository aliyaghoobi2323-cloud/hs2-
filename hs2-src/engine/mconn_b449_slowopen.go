package engine

import (
	"sync/atomic"
	"time"
)

// b449e slow open (Iran, HS2_B449_SLOWOPEN_MS, default 1000; 0 = off): lab3
// b449c n2 (2026-11-20), 2 heavy download errors: the exit's striped
// connection lost its last sub (ss -K) while Iran's other sub rode a
// blackholed link; Iran's light open (m.open, mconn_b312_open.go) picked a
// link that was dark too and blocked in OpenStream/the header write for 4+ s
// ("tick held ... light=true" every second), the exit's grace (3 s) ran out
// with 387 chunks orphaned: errMConnSubsDied there, "reset by peer" to the
// user. A light open still pending after the slow-open time gets one second
// open in parallel, avoiding the live and dead subs' links; whichever answers
// first carries the connection (a late one joins as a spare, retired when
// idle like any second lite sub). One extra open per pending open, so a dark
// link costs at most 2 opens per connection.
var b449SlowOpen atomic.Int64 // ns; 0 = off

// b449eCapWait (HS2_B449E_CAPWAIT, default on; rc14b lab1 2026-11-24): the slow-open
// clock skips the time the open waited for a b457 pick-cap slot. With the cap on
// by default (rc14), every light rejoin at a ZDU drain end queued > 1 s behind
// thousands of others, so the slow open fired a second open for ~4.6k
// connections (won 2-118), which queued too: join fails +6k, cap queue peak
// +4.8k, Iran +0.4 core in [2,4) s after the exit. A wait in the queue is not a
// dark link (the case the slow open is for), and a second open would wait in
// the same queue. The clock restarts when the wait ends (the pick, OpenStream
// and header write after it are timed as before).
var b449eCapWait atomic.Bool

func init() {
	b449SlowOpen.Store(int64(time.Duration(b449EnvMs0("HS2_B449_SLOWOPEN_MS", 1000)) * time.Millisecond))
	b449eCapWait.Store(envBoolDefault("HS2_B449E_CAPWAIT", true))
}

var b449SlowOpenStat struct {
	opens   atomic.Uint64 // second opens started
	won     atomic.Uint64 // the second open joined while the first still waited
	capHeld atomic.Uint64 // CAPWAIT: ticks that held the second open while the first waited for a cap slot
}

// b449SlowOpenTickLocked (Iran, tick, under m.mu): d = when to look again.
func (m *mConn) b449SlowOpenTickLocked(now time.Time) time.Duration {
	so := time.Duration(b449SlowOpen.Load())
	if so <= 0 || !m.iran || m.open == nil || !m.lightOpening || m.lightOpen2 || m.lightOpenT0.IsZero() ||
		m.ended || m.closing || m.handshaking || m.mode != planeLight {
		return 0
	}
	el := now.Sub(m.lightOpenT0)
	if b449eCapWait.Load() { // CAPWAIT: time in the b457 pick-cap queue is not a slow open
		if m.b449capW.Load() > 0 {
			b449SlowOpenStat.capHeld.Add(1)
			return so
		}
		if ce := m.b449capEnd.Load(); ce != 0 {
			if s := time.Duration(monoNano(now) - ce); s < el {
				el = s
			}
		}
	}
	if w := so - el; w > 0 {
		return w
	}
	if l485CallOpen.Load() { // l485 CALLOPEN (mconn_l485_callopen.go): one second open per pending open
		if m.l485SoT0.Equal(m.lightOpenT0) {
			l485Stat.soDup.Add(1)
			return 0
		}
		if m.l485FreshCapLocked(now) {
			return 200 * time.Millisecond
		}
		m.l485SoT0 = m.lightOpenT0
	}
	m.lightOpen2 = true
	b449SlowOpenStat.opens.Add(1)
	avoid := make([]any, 0, len(m.subs)+len(m.deadKeys))
	for _, s := range m.subs {
		avoid = append(avoid, avoidKey(s.key))
	}
	avoid = append(avoid, m.deadAvoidLocked()...)
	avoid = l485Live(avoid) // l485 CALLOPEN
	go m.openLight2(avoid)
	return 0
}

// openLight2: the slow open's second open (openLight's tail, its own flag).
func (m *mConn) openLight2(avoid []any) {
	sub, key, err := m.open(planeLight, avoid)
	m.mu.Lock()
	m.lightOpen2 = false
	if err != nil {
		m.mu.Unlock()
		return
	}
	if m.ended || m.closing || m.mode != planeLight || m.wantBulk {
		m.mu.Unlock()
		sub.Close()
		m.kickMgr()
		return
	}
	if m.lightOpening {
		b449SlowOpenStat.won.Add(1)
	}
	s := m.addSubLocked(sub, key, false)
	m.sendCond.Broadcast()
	m.mu.Unlock()
	m.startSub(s)
}

// b449CapWait (CAPWAIT, called by b457PickAcquireDG when a pick of m's edge
// has to wait for a slot): marks the wait; the returned func ends it.
func (m *mConn) b449CapWait() func() {
	m.b449capW.Add(1)
	return func() {
		m.b449capEnd.Store(monoNano(time.Now()))
		m.b449capW.Add(-1)
	}
}
