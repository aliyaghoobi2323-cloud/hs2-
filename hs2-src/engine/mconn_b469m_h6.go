package engine

import (
	"os"
	"sync/atomic"
)

// b469m H6 (HS2_LITE_H6, default off; design: lab/b455/PROGRESS.txt
// "b469m F1m"). The response of a hedged lite connection stays on the RAW
// first sub; the hedge sub carries only a records copy of the client's
// request prefix [0, H6N). This file holds the exit-side receive core: one
// delivered counter D for the user's request bytes fed by two sources, the
// raw stream (offset R) and the hedge copy (contiguous prefix [0, hEnd)).
// Every byte goes to the user once, in order, whichever copy comes first.
//
// Wired in b469n (mconn_b469n_h6wire.go: cap bit, role 8, Iran copy sender,
// exit role-8 reader).
var liteH6 = os.Getenv("HS2_LITE_H6") == "1"

// h6N: the request prefix the hedge sub copies (HS2_B469M_H6_BYTES, default
// 4096, capped at 16 KiB).
var h6N = func() int {
	n := hedge3EnvU64("HS2_B469M_H6_BYTES", 4096)
	if n > 16<<10 {
		n = 16 << 10
	}
	return int(n)
}()

var h6Stat struct {
	fromHedge atomic.Uint64 // request bytes the user got from the hedge copy first
	rawDup    atomic.Uint64 // raw bytes dropped (already delivered from the hedge)
	hedgeDup  atomic.Uint64 // hedge bytes dropped (already held or delivered)
	mismatch  atomic.Uint64 // raw bytes differing from the hedge copy (must stay 0)
	bad       atomic.Uint64 // hedge records out of order (protocol error)
}

// h6Recv: the exit's request-side merge (callers hold the raw reader's rmu
// for fromHedge/rawGot and the same lock for addHedge).
type h6Recv struct {
	d, r  uint64 // delivered to the user / raw stream bytes read
	hbuf  []byte // hedge copy [0, hEnd) (nil once freed)
	hEnd  uint64
	ended bool // the copy is complete (h6End seen) or abandoned
	freed bool
	max   int
}

func newH6Recv(max int) *h6Recv { return &h6Recv{max: max} }

// addHedge stores a hedge record at off (records on one stream arrive in
// order). It returns false on a gap (off > hEnd): the caller drops the hedge
// sub; the raw stream still carries every byte.
func (h *h6Recv) addHedge(off uint64, b []byte, end bool) bool {
	if end {
		h.ended = true
	}
	if h.freed || h.ended && len(b) == 0 {
		h.maybeFree()
		return true
	}
	if off > h.hEnd {
		h6Stat.bad.Add(1)
		h.ended = true
		h.maybeFree()
		return false
	}
	if k := h.hEnd - off; k > 0 { // overlap: keep only the new part
		if k >= uint64(len(b)) {
			h6Stat.hedgeDup.Add(uint64(len(b)))
			return true
		}
		h6Stat.hedgeDup.Add(k)
		b = b[k:]
	}
	if room := h.max - len(h.hbuf); len(b) > room {
		if room < 0 {
			room = 0
		}
		b = b[:room]
		h.ended = true // past the cap: the raw stream carries the rest
	}
	h.hbuf = append(h.hbuf, b...)
	h.hEnd += uint64(len(b))
	h.maybeFree()
	return true
}

// ready: the hedge copy holds bytes the user has not had yet.
func (h *h6Recv) ready() bool { return !h.freed && h.hEnd > h.d }

// fromHedge copies the next undelivered hedge bytes into p (0: none).
func (h *h6Recv) fromHedge(p []byte) int {
	if !h.ready() {
		return 0
	}
	n := copy(p, h.hbuf[h.d:h.hEnd])
	h.d += uint64(n)
	h6Stat.fromHedge.Add(uint64(n))
	h.maybeFree()
	return n
}

// rawGot: the raw stream just put n bytes (stream offsets [r, r+n)) into
// p[:n]. Bytes below d (already delivered from the hedge) are dropped; the
// kept ones are moved to p[0:] and their count returned (0: all were
// duplicates, the caller reads again).
func (h *h6Recv) rawGot(p []byte, n int) int {
	r0 := h.r
	h.r += uint64(n)
	if h.d <= r0 {
		h.d = h.r
		return n
	}
	k := h.d - r0
	if k > uint64(n) {
		k = uint64(n)
	}
	if !h.freed { // the dropped part is in hbuf: it must be the same bytes
		for i := uint64(0); i < k && r0+i < h.hEnd; i++ {
			if p[i] != h.hbuf[r0+i] {
				h6Stat.mismatch.Add(1)
				break
			}
		}
	}
	h6Stat.rawDup.Add(k)
	kept := copy(p, p[k:n])
	h.d += uint64(kept)
	h.maybeFree()
	return kept
}

// maybeFree drops hbuf once every byte in it was delivered and no more come.
func (h *h6Recv) maybeFree() {
	if !h.freed && h.ended && h.d >= h.hEnd && h.r >= h.hEnd {
		h.hbuf, h.freed = nil, true
	}
}

// switchNext: reassembly's next offset at the switch to records (UPGRADE at
// lim or RESCUE at Ri): never below what the user already has.
func (h *h6Recv) switchNext(lim uint64) uint64 {
	if h.d > lim {
		return h.d
	}
	return lim
}

// h6Add (exit): a hedge record of the request prefix arrived; a raw reader
// blocked in Read is woken (the timeout path loops back to fromHedge).
func (m *mConn) h6Add(off uint64, b []byte, end bool) bool {
	m.h6mu.Lock()
	ok := m.h6.addHedge(off, b, end)
	ready := m.h6.ready()
	m.h6mu.Unlock()
	if ready {
		m.mu.Lock()
		if m.raw1 != nil && m.rawR.Load() {
			m.interruptRead()
		}
		m.mu.Unlock()
	}
	return ok
}
