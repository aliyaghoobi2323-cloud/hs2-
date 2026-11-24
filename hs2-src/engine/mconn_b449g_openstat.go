package engine

import (
	"fmt"
	"sync/atomic"
)

// b449g (diagnostic only, no behaviour change): why a new user connection's
// first mconn sub could not be opened (newMConnIran failed), and how often
// serveUserTCP then fell back to the one stream and whether that failed too.
// Before, a download that never started (the lab's bulk_err at a link-kill
// burst) left no trace: obs.openErr counts the one-stream failure only.
// The Kharej side has no symmetric open: its subs arrive by attach.
const (
	b449gNoPick  = iota // no link picked before the first-open deadline (pickWait/pickPlaneCall)
	b449gNoMConn        // a link was picked but its exit does not read kindMConn (pickFirstLite)
	b449gStream         // OpenStream failed on every try
	b449gHeader         // the header write failed on every try
	b449gEnded          // the edge ended or the context was cancelled while waiting
	b449gReasons
)

var b449gReasonName = [b449gReasons]string{"nopick", "nomconn", "stream", "header", "ended"}

var b449gOpen struct {
	fail     [b449gReasons]atomic.Uint64 // newMConnIran failures by reason
	fallback atomic.Uint64               // serveUserMConn returned false: the one stream was used
	fbFail   atomic.Uint64               // ... and that one stream failed to open too (the user was closed)
	lateFail atomic.Uint64               // newMConnIran failed after a sub had opened (user closed, no fallback)
}

// b449gOpenFail counts a first-open failure with its reason.
func b449gOpenFail(why int) {
	if why >= 0 && why < b449gReasons {
		b449gOpen.fail[why].Add(1)
	}
}

// B449gOpenSnap: totals since start (status JSON).
type B449gOpenSnap struct {
	NoPick, NoMConn, Stream, Header, Ended uint64
	Fallback, FallbackFail, LateFail       uint64
}

func b449gOpenSnap() B449gOpenSnap {
	f := &b449gOpen.fail
	return B449gOpenSnap{NoPick: f[b449gNoPick].Load(), NoMConn: f[b449gNoMConn].Load(), Stream: f[b449gStream].Load(),
		Header: f[b449gHeader].Load(), Ended: f[b449gEnded].Load(), Fallback: b449gOpen.fallback.Load(),
		FallbackFail: b449gOpen.fbFail.Load(), LateFail: b449gOpen.lateFail.Load()}
}

// b449gLogPart: the b449 log line's open-failure part.
func b449gLogPart() string {
	s := b449gOpenSnap()
	return fmt.Sprintf("first-open fails nopick %d nomconn %d stream %d header %d ended %d · one-stream fallback %d (failed %d) · late fail %d",
		s.NoPick, s.NoMConn, s.Stream, s.Header, s.Ended, s.Fallback, s.FallbackFail, s.LateFail)
}
