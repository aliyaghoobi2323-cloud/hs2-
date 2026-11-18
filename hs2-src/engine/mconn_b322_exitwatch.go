package engine

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/xtaci/smux"
)

// b322 (st322 reconnect-storm open cost): HS2_B322_EXITNOWATCH (default ON
// since b332 / rel69o: st328 lab3 A/B with an equality round; =0 off).
//
// attachExitSub started one watcher goroutine per exit sub, parked on
// select{st died, mc died}, only to close the stream when the connection
// died and then run the sub's exit bookkeeping (traffic.done,
// exitSubs gauge). In the hourly xray-restart storm (~2400 conns closed and
// reopened in 1 s) that is ~2 extra goroutine spawns + wakeups per reopen on
// the exit, on top of serveStream/readLoop/writeLoop/manage/relay, in the
// 0.5 s run-queue burst the light pings wait in (st319: Kharej sched latency
// p99 25-58 ms).
//
// With the key on there is no watcher. The same two events are caught where
// they already happen:
//   - the sub closes (its reader saw the stream die, the connection retired
//     or ended it): msub.close runs onClose -> the bookkeeping, once;
//   - the connection dies: end() fires every registered sub not closed yet
//     (a sub a rescue moved off m.subs, the only kind end does not close
//     itself): close the stream, then the bookkeeping, exactly as the
//     watcher's mc-died branch did.
//
// A sub refused by attach (connection ended or closing) runs it at once.
// Each sub's bookkeeping runs exactly once whichever comes first.
var b322ExitNoWatch atomic.Bool

// b322ExitNoWatchDefault: the key's default (b332: on, see above).
const b322ExitNoWatchDefault = true

func init() { b322ExitNoWatch.Store(envBoolDefault("HS2_B322_EXITNOWATCH", b322ExitNoWatchDefault)) }

var b322Stat struct {
	attached atomic.Int64 // exit subs attached without a watcher goroutine
	byClose  atomic.Int64 // ... whose bookkeeping ran from the sub's close
	byDie    atomic.Int64 // ... from the connection's end (stream closed there)
}

// b322Line: " b322: nowatch=N close=N die=N" on the mem line once used.
func b322Line() string {
	n := b322Stat.attached.Load()
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" b322: nowatch=%d close=%d die=%d", n, b322Stat.byClose.Load(), b322Stat.byDie.Load())
}

// b322Ent is one exit sub's pending bookkeeping.
type b322Ent struct {
	st   *smux.Stream
	fn   func()
	done atomic.Bool
}

// fire runs the bookkeeping once; closeSt: the connection died first (close
// the stream as the watcher did).
func (e *b322Ent) fire(closeSt bool) bool {
	if !e.done.CompareAndSwap(false, true) {
		return false
	}
	if closeSt {
		e.st.Close()
	}
	e.fn()
	return true
}

// b322Watch is an mConn's registered exit subs (exit side only).
type b322Watch struct {
	mu   sync.Mutex
	ents []*b322Ent
	died bool
}

// add registers e; false when the connection already died (the caller fires
// it). Fired entries are dropped as the list grows, so a long-lived
// connection with sub churn keeps only its live ones.
func (w *b322Watch) add(e *b322Ent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.died {
		return false
	}
	if len(w.ents) >= 8 {
		live := w.ents[:0]
		for _, x := range w.ents {
			if !x.done.Load() {
				live = append(live, x)
			}
		}
		clear(w.ents[len(live):])
		w.ents = live
	}
	w.ents = append(w.ents, e)
	return true
}

// b322Died: m died; fire every registered sub not closed yet. Called once,
// right after close(m.die); never blocks.
func (m *mConn) b322Died() {
	w := &m.b322w
	w.mu.Lock()
	w.died = true
	ents := w.ents
	w.ents = nil
	w.mu.Unlock()
	n := 0
	for _, e := range ents {
		if !e.done.Load() {
			ents[n] = e
			n++
		}
	}
	if n == 0 {
		return // every sub closed with the connection (the usual end)
	}
	// a stream close can wait for its FIN frame (openCloseTimeout on a stuck
	// link): never in end(); one goroutine per dying conn, not per sub
	go func(ents []*b322Ent) {
		for _, e := range ents {
			if e.fire(true) {
				b322Stat.byDie.Add(1)
			}
		}
	}(ents[:n])
}

// attachExitNoWatch is attachExitSub's body with the key on: sub (st with
// the exit's counting) joins mc, fn runs once when the sub closes or mc dies.
func attachExitNoWatch(st *smux.Stream, sub mSub, mc *mConn, role byte, fn func()) {
	b322Stat.attached.Add(1)
	e := &b322Ent{st: st, fn: fn}
	if !mc.b322w.add(e) {
		// mc already died: what the watcher did at once
		if e.fire(true) {
			b322Stat.byDie.Add(1)
		}
		return
	}
	mc.attachWith(sub, role, func() {
		if e.fire(false) {
			b322Stat.byClose.Add(1)
		}
	})
}
