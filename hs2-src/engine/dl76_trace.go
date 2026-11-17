package engine

import (
	"log"
	"os"
	"time"

	"github.com/xtaci/smux"
)

// HS2_DL76_TRACE=1 (diagnostics, off by default): every second one log line
// per busy mconn sub stream (over 256 KiB moved in the second) with its smux
// window state — on the exit side how long its writers waited for the edge's
// window (window-bound) or in the session writer (socket-bound), on the edge
// its advertised window and unread backlog. dl76 used it to find what holds a
// striped download's ramp.
var dl76Trace = os.Getenv("HS2_DL76_TRACE") == "1"

func init() {
	if !dl76Trace {
		return
	}
	smux.EnableTrace()
	go func() {
		for range time.Tick(time.Second) {
			for _, r := range smux.TraceRows() {
				if r.Wr < 256<<10 && r.Rd < 256<<10 {
					continue
				}
				log.Printf("dl76tr id=%d wr=%d rd=%d winwait=%d sock=%d pwin=%d infl=%d rwin=%d buf=%d aw=%t off=%t",
					r.ID, r.Wr>>10, r.Rd>>10, r.WinWait.Milliseconds(), r.Sock.Milliseconds(),
					r.PeerWin>>10, r.Inflight>>10, r.RecvWin>>10, r.Buffered>>10, r.Auto, r.AutoOff)
			}
		}
	}()
}

// dl76Mark marks a sub stream for the trace (no-op unless HS2_DL76_TRACE).
func dl76Mark(st any) {
	if t, ok := st.(interface{ Trace() }); ok && dl76Trace {
		t.Trace()
	}
}
