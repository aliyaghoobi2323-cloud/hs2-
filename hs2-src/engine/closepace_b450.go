package engine

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// b450 close pacing (HS2_CLOSE_PACE). A mass close (thousands of user
// connections ending in the same instant: a Kharej drain end, the HH:01 xray
// restart, a client network drop) woke every relay, mconn manager, sub reader
// and writer of every one at once. Lab1 runtime trace (6000 users): ~15k
// goroutines runnable together for ~0.4 s, and every smux recvLoop (the TUN's
// L3 links ride the same sessions) waited for a P behind them: one-way TUN
// delay 37 -> 250-380 ms both ways, though the CPU was not full.
//
// closePace is called once per relay direction when its copy ends, before
// the teardown it starts. Under a normal close rate it returns at once; in a
// burst the relays queue on timers (parked, not runnable) and tear down at
// the paced rate, so the run queues stay short and the TUN path keeps its
// turn. GCRA: rate per second, burst of rate/50 (20 ms worth) free.
//
//	HS2_CLOSE_PACE unset  on: 500 x GOMAXPROCS closes a second
//	HS2_CLOSE_PACE=0      off (the pre-b450 behaviour)
//	HS2_CLOSE_PACE=N      N closes a second
//
// b452 wait cap (HS2_CLOSE_PACE_MAXWAIT, default 2.5s, 0 = no cap): no single
// close waits longer than the cap. When the queue is longer than the cap, the
// next close starts a new paced lane from now instead of joining the tail, so
// a storm bigger than rate x cap (a 4x HH:01 xray kill on Kharej: ~20k
// closes at 5500/s, ~7 s uncapped) still tears down within the cap at a
// multiple of the rate, spread over it (never one synchronized burst).
var (
	closePaceIv  atomic.Int64 // ns between paced closes; 0 = off
	closePaceTau atomic.Int64 // burst allowance, ns
	closePaceTAT atomic.Int64 // theoretical arrival time (monoNano)
	closePaced   atomic.Uint64
	closePaceCap atomic.Int64  // max wait of one close, ns; 0 = no cap
	closeLanes   atomic.Uint64 // closes that started a new lane (queue past the cap)
)

func init() {
	setClosePace(closePaceRate(os.Getenv("HS2_CLOSE_PACE"), runtime.GOMAXPROCS(0)))
	closePaceCap.Store(int64(closePaceMaxWait(os.Getenv("HS2_CLOSE_PACE_MAXWAIT"))))
}

// closePaceMaxWait: the per-close wait cap HS2_CLOSE_PACE_MAXWAIT asks for
// (a Go duration or plain milliseconds; 0: no cap; unset or bad: 2.5s).
func closePaceMaxWait(env string) time.Duration {
	const def = 2500 * time.Millisecond
	env = strings.TrimSpace(env)
	if env == "" {
		return def
	}
	if d, err := time.ParseDuration(env); err == nil && d >= 0 {
		return d
	}
	if n, err := strconv.Atoi(env); err == nil && n >= 0 {
		return time.Duration(n) * time.Millisecond
	}
	return def
}

// closePaceRate: the closes-a-second rate HS2_CLOSE_PACE asks for (0: off).
func closePaceRate(env string, procs int) int {
	env = strings.TrimSpace(env)
	if env == "" {
		return 500 * max(procs, 1)
	}
	n, err := strconv.Atoi(env)
	if err != nil || n < 0 {
		return 500 * max(procs, 1)
	}
	return n
}

func setClosePace(rate int) {
	if rate <= 0 {
		closePaceIv.Store(0)
		return
	}
	iv := int64(time.Second) / int64(rate)
	closePaceTau.Store(iv * int64(max(rate/50, 1)))
	closePaceIv.Store(iv)
}

// closePace waits for this close's turn (at once unless a burst is on).
func closePace() {
	iv := closePaceIv.Load()
	if iv == 0 {
		return
	}
	if d := closePaceDelay(monoNano(time.Now()), iv, closePaceTau.Load(), closePaceCap.Load()); d > 0 {
		closePaced.Add(1)
		time.Sleep(time.Duration(d))
	}
}

// closePaceDelay reserves the next slot at now and returns how long to wait.
// With a cap (> iv), a slot more than capNs away is moved back by capNs: the
// close opens a new lane from now (the TAT then never runs more than the cap
// ahead, so the wait stays <= capNs and the pace holds no stale backlog).
func closePaceDelay(now, iv, tau, capNs int64) int64 {
	for {
		tat := closePaceTAT.Load()
		next := max(tat, now) + iv
		d := next - iv - tau - now
		wrap := capNs > iv && d > capNs
		if wrap {
			next -= capNs
			d -= capNs
		}
		if closePaceTAT.CompareAndSwap(tat, next) {
			if wrap {
				closeLanes.Add(1)
			}
			return d
		}
	}
}
