package engine

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// burstLog keeps per-link log lines readable at hundreds of links. One kind of
// event (a link coming up, a link going down, a retired link closing) is
// logged line by line while it is occasional; when more than burstLines of
// them land within burstWin, the rest of that window is folded into one
// summary line at its end — "+K more links up in 10s (latest: …)". At a 300
// link pool a restart is otherwise 300 "link up" lines in a few seconds on
// each side.
const (
	burstLines = 8
	burstWin   = 10 * time.Second
)

type burstLog struct {
	prefix string // the lines' common prefix, e.g. "mtcp: "
	what   string // the summary's noun phrase, e.g. "links up"
	logf   func(string, ...any)

	mu         sync.Mutex
	winStart   time.Time
	n          int    // lines in this window, logged or not
	suppressed int    // lines not logged in this window
	latest     string // the newest suppressed line
	flushing   bool   // a summary is scheduled
}

func newBurstLog(prefix, what string, logf func(string, ...any)) *burstLog {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &burstLog{prefix: prefix, what: what, logf: logf}
}

// log logs one event line, or folds it into the window's summary.
func (b *burstLog) log(format string, args ...any) {
	now := time.Now()
	b.mu.Lock()
	if now.Sub(b.winStart) >= burstWin {
		b.winStart, b.n = now, 0
	}
	b.n++
	if b.n <= burstLines {
		b.mu.Unlock()
		b.logf(format, args...)
		return
	}
	b.suppressed++
	b.latest = strings.TrimPrefix(fmt.Sprintf(format, args...), b.prefix)
	if !b.flushing {
		b.flushing = true
		time.AfterFunc(b.winStart.Add(burstWin).Sub(now), b.flush)
	}
	b.mu.Unlock()
}

// flush writes the summary of the suppressed lines.
func (b *burstLog) flush() {
	b.mu.Lock()
	n, latest := b.suppressed, b.latest
	b.suppressed, b.latest, b.flushing = 0, "", false
	b.mu.Unlock()
	if n > 0 {
		b.logf("%s+%d more %s in the last %s (latest: %s)", b.prefix, n, b.what, fmtDur(burstWin), latest)
	}
}
