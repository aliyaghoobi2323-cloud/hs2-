package engine

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// hs2 mem67: the frozen-session guard (third_party/smux/stall.go).
// HS2_SMUX_STALL_S (rel58: default 0 = off; e.g. 15 turns it on): a stream left unread while its
// link's receive buffer is full is evicted after half of it without any read
// on that link, and a link whose socket accepts no byte for all of it is
// closed (LB replaces it; every goroutine blocked on it gets an error).
// rel58: off by default — in the lab the send-path close shut live but slow
// lossy links; the receive evict shares the same timer and was never tested
// alone, so both stay off unless HS2_SMUX_STALL_S is set.
var smuxStallTimeout = smuxStallFromEnv(os.Getenv("HS2_SMUX_STALL_S"))

func smuxStallFromEnv(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	if f > 0 && f < 2 {
		f = 2 // never on a single slow round trip
	}
	return time.Duration(f * float64(time.Second))
}

func init() {
	smux.StallLog = func(s string) { log.Print(s) }
	if os.Getenv("HS2_SMUX_STALL_DUMP") == "1" {
		smux.StallDump = smuxStallDump
	}
}

// smuxStallDump (HS2_SMUX_STALL_DUMP=1, diagnosis): logs the stack of the
// frozen session's sendLoop (where its conn write is stuck), at most once a
// minute (a full stack walk stops the world).
func smuxStallDump(sig string) {
	now := monoNano(time.Now())
	if last := smuxStallDumpAt.Load(); now-last < int64(time.Minute) || !smuxStallDumpAt.CompareAndSwap(last, now) {
		return
	}
	buf := make([]byte, 64<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, sig) {
			log.Printf("smux stall dump %s:\n%s", sig, g)
		}
	}
}

var smuxStallDumpAt atomic.Int64

// smuxStallLine is the guard's part of the minute mem log line.
func smuxStallLine() string {
	ev, evb, sc := smux.StallCounts()
	return fmt.Sprintf(" stall: evicts=%d evictB=%d sendcloses=%d", ev, evb, sc)
}
