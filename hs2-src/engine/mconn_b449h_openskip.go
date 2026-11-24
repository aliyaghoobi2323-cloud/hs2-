package engine

import (
	"context"
	"slices"
	"sync/atomic"
)

// b449h open skip (HS2_B449H_OPENSKIP, default on; Iran only, no wire change):
// a new user connection's open retries (mconnEdge.open's lite first sub,
// openStreamOn, and serveUserTCP's one-stream fallback after a failed mconn
// open) picked with pickWait, which knows nothing of the try that just
// failed. The pool's order can rank the same link first again (b449g lab run
// b5: one crowd conn failed 3 OpenStream tries and the fallback too, ~30 s
// after a blackhole). Now a retry leaves out the links this connection's
// open already failed on whenever another link can take it; with no other
// link the old choice stands.
var b449hSkipOn atomic.Bool

func init() { b449hSkipOn.Store(envBoolDefault("HS2_B449H_OPENSKIP", true)) }

var b449hSkip struct {
	moved atomic.Uint64 // a retry was moved off a link that had just failed
	same  atomic.Uint64 // the failed link was the only choice
}

// pickWaitSkip is pickWait leaving out the links in skip when another link
// can take the connection.
func pickWaitSkip(ctx context.Context, lm *LinkManager, hold bool, skip []Link) (Link, func(), bool) {
	l, rel, ok := pickWait(ctx, lm, hold)
	if !ok || len(skip) == 0 || !b449hSkipOn.Load() || !slices.Contains(skip, l) {
		return l, rel, ok
	}
	if ol, orel, ook := lm.pickSkip(skip); ook {
		rel()
		b449hSkip.moved.Add(1)
		return ol, orel, true
	}
	b449hSkip.same.Add(1)
	return l, rel, ok
}

// pickSkip is Pick leaving out the links in skip.
func (m *LinkManager) pickSkip(skip []Link) (Link, func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ml, _ := m.pickLockedSkipFP(0, skip)
	if ml == nil {
		return nil, nil, false
	}
	m.placeLocked(ml)
	return ml.link, m.releaseFor(ml), true
}

// failedAny is e.failed as avoid entries (avoided matches a Link value too).
// Caller holds e.mu.
func (e *mconnEdge) failedAny() []any {
	a := make([]any, len(e.failed))
	for i, l := range e.failed {
		a[i] = l
	}
	return a
}
