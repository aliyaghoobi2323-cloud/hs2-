package engine

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// b469n H6 wiring (HS2_LITE_H6=1, default off; design lab/b455/PROGRESS.txt
// "b469m F1m", receive core mconn_b469m_h6.go).
//
//   - Cap: the exit announces capExtLiteH6 (ext bit 17; the design's 1<<14
//     is capExtMConnXS) with the key. Iran with the key opens a lite conn's
//     first sub RAW toward such an exit even when a HEDGE key wants records,
//     and opens the copy sub with mconnRoleH6 (8). No cap or no key: no role
//     8 ever goes out (rc12 both ways unchanged).
//   - Iran: writeRaw copies raw bytes [0, h6N) into m.h6s; a goroutine opens
//     the role-8 sub (another link) and writes the copy as frames
//     [type 1][off 4][len 2][data], then an end frame (type 2) after h6N
//     bytes or h6Dur, and closes it. The sub is not an msub: no response,
//     no records, no manager state.
//   - Exit: a raw conn gets m.h6 (h6Recv) before its raw reader starts; a
//     role-8 sub joins its conn by id (waiting up to h6Wait when it came
//     first: exit_first), reads frames into m.h6Add, and never writes. No
//     conn (or a conn without h6): the role-8 sub is dropped, the raw stream
//     carries every byte.
var capExtLiteH6 uint32 = 1 << 17

const mconnRoleH6 byte = 8

var (
	h6Dur  = time.Duration(hedge3EnvU64("HS2_LITE_H6_MS", 1500)) * time.Millisecond
	h6Wait = time.Duration(hedge3EnvU64("HS2_LITE_H6_WAIT_MS", 2000)) * time.Millisecond
)

const (
	h6FrData = 1
	h6FrEnd  = 2
	h6HdrLen = 7
)

var h6Wire struct {
	started  atomic.Uint64 // Iran: conns that started a copy
	opened   atomic.Uint64 // Iran: role-8 subs opened
	openFail atomic.Uint64
	sentB    atomic.Uint64 // Iran: copy bytes written
	ended    atomic.Uint64 // Iran: end frames written
	capped   atomic.Uint64 // Iran: budget capped, no copy
	joined   atomic.Uint64 // exit: role-8 subs attached
	first    atomic.Uint64 // exit: role-8 sub came before the raw sub (exit_first)
	dropped  atomic.Uint64 // exit: no conn within h6Wait, or no h6 on it
	refused  atomic.Uint64 // exit: role 8 with the key off
	recvB    atomic.Uint64 // exit: copy bytes received
	broken   atomic.Uint64 // exit: frame errors, or the sub died inside a frame
	closed   atomic.Uint64 // exit: the sub closed between frames before the end frame (Iran's conn ended inside the window)
}

// h6Send (Iran): the copy of the raw request prefix.
type h6Send struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
	wake   chan struct{}
}

func (h *h6Send) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// add (writeRaw, under wmu): raw bytes just written at the stream's end.
func (h *h6Send) add(b []byte) {
	h.mu.Lock()
	if !h.closed {
		if room := h6N - len(h.buf); len(b) >= room {
			b = b[:room]
			h.closed = true
		}
		h.buf = append(h.buf, b...)
	}
	h.mu.Unlock()
	h.signal()
}

func (h *h6Send) close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.signal()
}

// h6PeerOK: the exit behind link l reads role 8.
func h6PeerOK(l Link) bool {
	if !liteH6 {
		return false
	}
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi.mconnRaw() && pi.Ext&capExtLiteH6 != 0
}

// mconnH6Sub: a raw first sub toward an exit announcing capExtLiteH6.
type mconnH6Sub interface{ mconnH6() bool }

func (s *mconnSub) mconnH6() bool { return s.h6 }

// h6Mark (in an open's avoid list): open the sub with mconnRoleH6.
type h6Mark struct{}

func takeH6Mark(avoid []any) ([]any, bool) {
	for i, a := range avoid {
		if _, ok := a.(h6Mark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), true
		}
	}
	return avoid, false
}

// h6StartLocked (Iran, newMConnIranRaw's caller, under m.mu, no byte
// written yet): start the copy when the first sub's exit reads role 8.
func (m *mConn) h6StartLocked(sub mSub, firstKey any) {
	if !liteH6 || !m.iran || m.open == nil || m.raw1 == nil {
		return
	}
	if s, ok := sub.(mconnH6Sub); !ok || !s.mconnH6() {
		return
	}
	now := time.Now()
	if liteBud.capped(now.Unix()) {
		h6Wire.capped.Add(1)
		return
	}
	h6Wire.started.Add(1)
	hs := &h6Send{wake: make(chan struct{}, 1)}
	m.h6s = hs
	avoid := make([]any, 0, 2+len(m.deadKeys))
	avoid = append(avoid, avoidKey(firstKey))
	avoid = m.callAvoidLocked(append(avoid, m.deadAvoidLocked()...))
	avoid = append(avoid, h6Mark{})
	go m.h6Run(hs, avoid, now.Add(h6Dur))
	h6Log(now)
}

// h6Run (Iran): open the role-8 sub, write the copy, end it, close it.
func (m *mConn) h6Run(hs *h6Send, avoid []any, until time.Time) {
	defer hs.close()
	sub, _, err := m.open(planeLight, avoid)
	if err != nil {
		h6Wire.openFail.Add(1)
		return
	}
	defer sub.Close()
	h6Wire.opened.Add(1)
	if d, ok := sub.(interface{ SetWriteDeadline(time.Time) error }); ok {
		d.SetWriteDeadline(until.Add(2 * time.Second))
	}
	m.tr.ev("h6: copy sub open")
	sent := m.h6Copy(hs, sub, until)
	m.tr.ev("h6: copy end %d", sent)
}

// h6Frame: one role-8 frame.
func h6Frame(typ byte, off int, b []byte) []byte {
	fr := make([]byte, h6HdrLen, h6HdrLen+len(b))
	fr[0] = typ
	binary.BigEndian.PutUint32(fr[1:5], uint32(off))
	binary.BigEndian.PutUint16(fr[5:7], uint16(len(b)))
	return append(fr, b...)
}

// h6Copy (Iran): write the copy to w as it grows until it is closed (h6N
// bytes), until, or the conn's end; then the end frame. It returns the
// bytes sent.
func (m *mConn) h6Copy(hs *h6Send, w io.Writer, until time.Time) int {
	tm := time.NewTimer(time.Until(until))
	defer tm.Stop()
	sent := 0
	for {
		hs.mu.Lock()
		b, closed := hs.buf[sent:], hs.closed
		hs.mu.Unlock()
		for len(b) > 0 {
			n := min(len(b), 16<<10)
			if _, err := w.Write(h6Frame(h6FrData, sent, b[:n])); err != nil {
				return sent
			}
			liteBud.add(time.Now().Unix(), 0, n) // the copy counts as lite duplicates
			obs.mconnDupBytes.Add(uint64(n))
			h6Wire.sentB.Add(uint64(n))
			sent += n
			b = b[n:]
		}
		if closed {
			break
		}
		select {
		case <-hs.wake:
			continue
		case <-tm.C:
			hs.close()
		case <-m.GetDieCh():
			return sent
		}
	}
	if _, err := w.Write(h6Frame(h6FrEnd, sent, nil)); err == nil {
		h6Wire.ended.Add(1)
	}
	return sent
}

// h6ExitInit (Kharej, newMConnExitRaw): the merge state, before any read.
func (m *mConn) h6ExitInit() {
	if liteH6 {
		m.h6 = newH6Recv(h6N)
	}
}

// h6Abandon (exit): no more copy bytes come (the role-8 sub ended).
func (m *mConn) h6Abandon() {
	m.h6mu.Lock()
	m.h6.ended = true
	m.h6.maybeFree()
	m.h6mu.Unlock()
}

// serveH6Sub (Kharej, serveMConnSub, role 8): never writes on st.
func serveH6Sub(ctx context.Context, st *smux.Stream, tbl *mconnTable, id uint64) {
	defer st.Close()
	if !liteH6 {
		h6Wire.refused.Add(1)
		return
	}
	mc := h6Join(tbl, id, ctx.Done())
	if mc == nil {
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-mc.GetDieCh():
			st.Close()
		case <-done:
		}
	}()
	st.SetReadDeadline(time.Now().Add(h6Dur + 3*time.Second))
	h6ReadLoop(st, mc)
}

// h6Join (exit): the role-8 sub's conn; when it came before the raw first
// sub (exit_first) it waits up to h6Wait for it; nil: dropped.
func h6Join(tbl *mconnTable, id uint64, stop <-chan struct{}) *mConn {
	mc := tbl.join(id, 0, stop)
	if mc == nil {
		h6Wire.first.Add(1)
		mc = tbl.join(id, h6Wait, stop)
	}
	if mc == nil || mc.h6 == nil {
		h6Wire.dropped.Add(1)
		return nil
	}
	h6Wire.joined.Add(1)
	h6Log(time.Now())
	return mc
}

// h6ReadLoop (exit): the copy's frames into mc.h6Add until the end frame;
// any error ends the copy (the raw stream carries every byte).
func h6ReadLoop(r io.Reader, mc *mConn) {
	var hdr [h6HdrLen]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				h6Wire.closed.Add(1)
			} else {
				h6Wire.broken.Add(1)
			}
			mc.h6Abandon()
			return
		}
		off := uint64(binary.BigEndian.Uint32(hdr[1:5]))
		n := int(binary.BigEndian.Uint16(hdr[5:7]))
		switch hdr[0] {
		case h6FrEnd:
			mc.h6Add(off, nil, true)
			return
		case h6FrData:
			if n == 0 || off+uint64(n) > uint64(h6N) {
				h6Wire.broken.Add(1)
				mc.h6Abandon()
				return
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(r, b); err != nil {
				h6Wire.broken.Add(1)
				mc.h6Abandon()
				return
			}
			h6Wire.recvB.Add(uint64(n))
			if !mc.h6Add(off, b, false) {
				return // a gap: addHedge ended the copy; raw carries the rest
			}
		default:
			h6Wire.broken.Add(1)
			mc.h6Abandon()
			return
		}
	}
}

var h6LogAt atomic.Int64

// h6Log: the counters, at most every 10 s (both sides).
func h6Log(now time.Time) {
	if !liteH6 {
		return
	}
	n := monoNano(now)
	at := h6LogAt.Load()
	if n-at < int64(10*time.Second) || !h6LogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("b469n h6: started=%d opened=%d open_fail=%d sent=%dKB ended=%d capped=%d joined=%d exit_first=%d dropped=%d refused=%d recv=%dKB broken=%d closed=%d from_hedge=%dKB raw_dup=%dKB hedge_dup=%dKB mismatch=%d bad=%d",
		h6Wire.started.Load(), h6Wire.opened.Load(), h6Wire.openFail.Load(), h6Wire.sentB.Load()>>10, h6Wire.ended.Load(), h6Wire.capped.Load(),
		h6Wire.joined.Load(), h6Wire.first.Load(), h6Wire.dropped.Load(), h6Wire.refused.Load(), h6Wire.recvB.Load()>>10, h6Wire.broken.Load(), h6Wire.closed.Load(),
		h6Stat.fromHedge.Load()>>10, h6Stat.rawDup.Load()>>10, h6Stat.hedgeDup.Load()>>10, h6Stat.mismatch.Load(), h6Stat.bad.Load())
}
