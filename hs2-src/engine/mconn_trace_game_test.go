package engine

import (
	"testing"
	"time"
)

// TestMTraceGameBand: HS2_MCONN_TRACE_GAME adds the game-like band (light,
// two-way) to the trace's volume rule; crowd-like request/reply pairs (one
// direction far smaller) and call-like volumes keep their old handling.
func TestMTraceGameBand(t *testing.T) {
	oldOn, oldGame, oldPort := mtraceOn, mtraceGame, mtracePort
	defer func() { mtraceOn, mtraceGame, mtracePort = oldOn, oldGame, oldPort }()
	mtraceOn, mtracePort = true, 0
	decide := func(game bool, sent, recvd int64) int {
		mtraceGame = game
		tr := newMTrace(1, true)
		tr.sent.Store(sent)
		tr.recvd.Store(recvd)
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.decideLocked(tr.t0.Add(mTraceWindow + time.Millisecond))
		return tr.state
	}
	cases := []struct {
		name        string
		game        bool
		sent, recvd int64
		want        int
	}{
		{"game off: game volume not traced", false, 40_000, 39_000, -1},
		{"game on: game volume traced", true, 40_000, 39_000, 1},
		{"game on: crowd pair (small requests, big replies) not traced", true, 2_000, 40_000, -1},
		{"game on: idle not traced", true, 500, 500, -1},
		{"game on: call volume still traced", true, 2_500_000, 2_450_000, 1},
		{"game on: bulk not traced", true, 50_000_000, 10_000, -1},
	}
	for _, c := range cases {
		if got := decide(c.game, c.sent, c.recvd); got != c.want {
			t.Errorf("%s: state %d, want %d", c.name, got, c.want)
		}
	}
}
