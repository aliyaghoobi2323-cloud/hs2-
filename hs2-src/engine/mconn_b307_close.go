package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// b307 (sc307 scale test): HS2_B307_ASYNCCLOSE (default ON since b315:
// b310 had turned it off after a lab2 1x re-check measured about -20 %
// heavy throughput, but p313's fair A/B with per-half RPS masks found the
// loss was the H1 softirq bias: async is neutral on heavy and equal or
// better on ICMP/call p99; heavy connections keep the synchronous close via
// b311's HS2_B311_HEAVYSYNC; =0 is rel69m's synchronous close).
//
// msub.close closed the sub's stream inside its closeOnce. A smux
// Stream.Close queues its FIN (and, for an abandoned half-open stream, an
// empty push and a second FIN) on the session's writer and waits for it to
// be written, up to 30 s: at 3-4x the FIN waits behind every queued link
// write, and every other caller of the same close (the writer, the reader,
// subDied, the manager's retire) waited in the Once behind it: 409-5900 s of
// wait per 20 s, on Iran and Kharej.
//
// With the key on, close does its bookkeeping (msubsLive, the link index,
// done, the stripe count) at once as before, starts the stream's close in
// its own goroutine and waits for it at most b307CloseWait. A close that
// finishes in that time is what it was (rw closed, onClose run, rwClosed set
// before close returns). One that does not goes on alone: close returns, and
// the goroutine finishes it — rw.Close, then onClose, then rwClosed, in that
// order as before — and then gives a dead raw first sub its release
// (releaseRaw1 needs rwClosed; the caller's own try came too early).
//
// Order: the FIN is queued by the same Stream.Close, after every Write that
// returned before close was called (a smux Write returns once its frames
// are written or queued in order), so the FIN still follows the stream's
// data. No leak: the goroutine ends when Stream.Close does, which is bounded
// by its own deadline (openCloseTimeout) or the session's death.
var b307AsyncClose atomic.Bool

// b307AsyncCloseDefault: the key's default (b315: on, see above).
const b307AsyncCloseDefault = true

func init() { b307AsyncClose.Store(envBoolDefault("HS2_B307_ASYNCCLOSE", b307AsyncCloseDefault)) }

// b307CloseWait: how long close waits for its stream's close.
const b307CloseWait = time.Millisecond

// b307TestCloseDelay (tests only, set before the connections start): every
// async stream close sleeps this long first, so it always goes on alone.
var b307TestCloseDelay time.Duration

var b307Close struct {
	async atomic.Int64 // closes that went on alone
	late  atomic.Int64 // of those, finished
}

// closeLate: the stream's close that went on alone finished.
func (s *msub) closeLate() {
	b307Close.late.Add(1)
	m := s.m
	if m == nil {
		return
	}
	m.mu.Lock()
	raw := m.raw1 == s
	m.mu.Unlock()
	if raw { // releaseRaw1 checks the rest (dead, loops, raw off)
		m.releaseRaw1()
	}
}

// b307CloseLine: " b307close: async=N pending=M" on the mem line once a close
// went on alone (pending: still closing; it must stay bounded).
func b307CloseLine() string {
	a := b307Close.async.Load()
	if a == 0 {
		return ""
	}
	return fmt.Sprintf(" b307close: async=%d pending=%d", a, a-b307Close.late.Load())
}
