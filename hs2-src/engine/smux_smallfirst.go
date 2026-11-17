package engine

import (
	"strconv"

	"github.com/xtaci/smux"
)

// HS2_SMUX_SMALL_FIRST (lat58, off by default): a link's smux session sends a
// small data frame before the bulk frames queued ahead of it.
//
// Why: smux's write scheduler is a FIFO of data frames, one per writing
// stream (each waits for its frame to reach the socket). On a link that also
// carries downloads or uploads, an interactive stream's few-hundred-byte
// frame waits a 16 KiB turn of every bulk stream there before it is written,
// and the socket then sends it behind them. With the key, a data frame of at
// most smuxSmallFirst bytes overtakes up to smux.SmallFirstJump earlier
// requests (bounded: a bulk frame is never starved). Only the sender's order
// changes — the wire format is the same, so one side may run it alone.
//
//	HS2_SMUX_SMALL_FIRST=1     frames up to smallFirstDefault bytes
//	HS2_SMUX_SMALL_FIRST=<n>   frames up to n bytes (n > 1)
var smuxSmallFirst int // loadEnvKeys

const smallFirstDefault = 4 << 10

// smallFirstFromEnv parses HS2_SMUX_SMALL_FIRST.
func smallFirstFromEnv(v string) int {
	n, err := strconv.Atoi(v)
	switch {
	case err != nil || n <= 0:
		return 0
	case n == 1:
		return smallFirstDefault
	case n > SmuxFrameSize:
		return SmuxFrameSize
	}
	return n
}

// SmuxSmallFirstOvertakes: small frames written ahead of an earlier queued
// data frame since start (status obs_smux_small_first).
func SmuxSmallFirstOvertakes() uint64 { return smux.SmallFirstOvertakes() }

// SmuxStaleUPDs: window updates the smux sender dropped as older than the
// stream's last one since start (status obs_smux_stale_upd; aw163). Each was
// a stream that would have stalled for good before the fix.
func SmuxStaleUPDs() uint64 { return smux.StaleUPDs() }
