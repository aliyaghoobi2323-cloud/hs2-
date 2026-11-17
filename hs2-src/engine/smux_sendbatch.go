package engine

import (
	"strconv"
	"strings"

	"github.com/xtaci/smux"
)

// HS2_SMUX_SENDBATCH (lk179, off by default): a link's smux session writes
// the frames already queued for it with one conn write (smux
// Config.SendBatch, third_party/smux/sendbatch.go) instead of one write and
// two goroutine hand-offs per frame. Same frames, same order on the wire; one
// side may run it alone.
//
// Why: the sc179 storm profile (lab3) showed the storm's opens were no longer
// held by a lock (the edge's open path blocked ~0.3 ms per open), while a
// third of both sides' CPU went to write syscalls (smux sendLoop: one per
// 16 KiB frame, window update, SYN, FIN) and to the scheduler waking the
// shaper/sendLoop/writer goroutines for every frame. At 3-4x the load the box
// runs out of CPU first, and an open waits for CPU.
//
//	HS2_SMUX_SENDBATCH=1     batches up to sendBatchDefault bytes
//	HS2_SMUX_SENDBATCH=<n>   batches up to n bytes (n > 1, at most sendBatchMax)
var smuxSendBatch int // loadEnvKeys

const (
	sendBatchDefault = 32 << 10
	sendBatchMax     = 256 << 10
)

// sendBatchFromEnv parses HS2_SMUX_SENDBATCH.
func sendBatchFromEnv(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	switch {
	case err != nil || n <= 0:
		return 0
	case n == 1:
		return sendBatchDefault
	case n < SmuxFrameSize+8:
		return SmuxFrameSize + 8 // at least one full data frame
	case n > sendBatchMax:
		return sendBatchMax
	}
	return n
}

// SmuxSendBatchStats: batches written by sessions with the key and the
// frames in them, since start (frames/batches > 1: writes saved).
func SmuxSendBatchStats() (batches, frames uint64) { return smux.SendBatchStats() }
