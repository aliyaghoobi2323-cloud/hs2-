package engine

import (
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// b469 (rc14h, HS2_B469_DRAINSMOOTH): hold the Go collector back while user
// connections end in a storm (an old exit's drain-end pace, a mass close).
//
// rc14g lab1 (12k users, ZDU exit, HS2_B468_DRAINFAST on the Kharej, 4 ABBA
// pairs vs rc12): TUN ping max in the 4 s before the exit 116-161 ms vs
// rc12 42-53. tcpdump on both hs0 ends: the stall is on the Iran side (TUN
// read to link write, the Kharej answers in 0.0 ms) and in 3 of 4 runs the
// biggest one is the Iran's GC cycle (gc 23: 13-16 ms stop-the-world sweep
// termination, 349-476 ms of mark assist CPU in ~115 ms) landing inside the
// 4 s pace: the pace's ~1500 ends a second and their reconnects allocate and
// pull the cycle in. rc12 runs the same cycle 3-4 s after its exit.
//
// With the key on, an Iran that sees >= HS2_B469_ENDS user connection ends
// (mconn) in 500 ms raises the GC target to HS2_B469_PCT% (the soft memory
// limit of setMemoryLimit still bounds the heap), and puts the previous
// target back 2 s after the storm (or after 15 s at most): the cycle runs
// once the drain is over, as with rc12.
//
//	HS2_B469_DRAINSMOOTH=0           off
//	HS2_B469_DRAINSMOOTH unset / 1   hold GC over a close storm (default on since rc14h)
//	HS2_B469_ENDS=N                  ends in 500 ms that start a hold (500)
//	HS2_B469_PCT=N                   GC target during a hold (400)
var b469Ends atomic.Int64 // user connection (mconn) ends, cumulative

var b469Stat struct {
	holds, heldMs atomic.Int64
}

func b469On() bool {
	return strings.TrimSpace(os.Getenv("HS2_B469_DRAINSMOOTH")) != "0"
}

func b469EnvInt(k string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k)))
	if err != nil || n < lo || n > hi {
		return def
	}
	return n
}

const (
	b469Win    = 500 * time.Millisecond
	b469Tick   = 100 * time.Millisecond
	b469After  = 2 * time.Second  // storm over this long: the target goes back
	b469MaxDur = 15 * time.Second // one hold at most
)

// b469Hold is the hold state machine (one per process; tests drive it).
type b469Hold struct {
	minEnds int64
	pct     int
	set     func(int) int // debug.SetGCPercent

	hist      []b469Sample
	holding   bool
	prev      int
	since     time.Time
	lastStorm time.Time
	ends0     int64
}

type b469Sample struct {
	at   time.Time
	ends int64
}

// step takes the cumulative ends at now; returns a log line when a hold ends.
func (h *b469Hold) step(ends int64, now time.Time) string {
	h.hist = append(h.hist, b469Sample{now, ends})
	i := 0
	for i < len(h.hist)-1 && now.Sub(h.hist[i+1].at) >= b469Win {
		i++
	}
	h.hist = h.hist[i:]
	storm := now.Sub(h.hist[0].at) <= b469Win+b469Tick && ends-h.hist[0].ends >= h.minEnds
	if storm {
		h.lastStorm = now
	}
	switch {
	case !h.holding && storm:
		prev := h.set(h.pct)
		if prev < 0 || prev >= h.pct { // GC off, or already as loose: leave it
			h.set(prev)
			return ""
		}
		h.holding, h.prev, h.since, h.ends0 = true, prev, now, ends
	case h.holding && (now.Sub(h.lastStorm) >= b469After || now.Sub(h.since) >= b469MaxDur):
		h.set(h.prev)
		h.holding = false
		d := now.Sub(h.since)
		b469Stat.holds.Add(1)
		b469Stat.heldMs.Add(d.Milliseconds())
		return "b469 drainsmooth: GC target held at " + strconv.Itoa(h.pct) + "% for " +
			d.Round(10*time.Millisecond).String() + " over a close storm (" +
			strconv.FormatInt(ends-h.ends0, 10) + " user ends), back to " + strconv.Itoa(h.prev) + "%; holds so far " +
			strconv.FormatInt(b469Stat.holds.Load(), 10)
	}
	return ""
}

func init() {
	if !b469On() {
		return
	}
	h := &b469Hold{
		minEnds: int64(b469EnvInt("HS2_B469_ENDS", 500, 1, 1<<30)),
		pct:     b469EnvInt("HS2_B469_PCT", 400, 101, 10000),
		set:     debug.SetGCPercent,
	}
	go func() {
		t := time.NewTicker(b469Tick)
		defer t.Stop()
		for now := range t.C {
			if s := h.step(b469Ends.Load(), now); s != "" {
				log.Print(s)
			}
		}
	}()
}
