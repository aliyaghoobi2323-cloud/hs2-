package engine

// judge rig knob (scratch), built only into trees with the need window:
// Z5_POOL (MiB) is the process's window pool as cmd/hs2 would set it from
// the server's RAM (tune.WindowPoolMiB: 128 on the 17 and 22 GB field
// servers, 32 on a 4 GB one). Both ends of the rig share it (one process).

import (
	"os"
	"strconv"
)

func init() {
	if v, err := strconv.ParseFloat(os.Getenv("Z5_POOL"), 64); err == nil {
		SmuxWindowPool = int(v * (1 << 20))
	}
	if v, err := strconv.ParseFloat(os.Getenv("Z5_NEEDBUF"), 64); err == nil {
		SmuxNeedStreamBuffer = int(v * (1 << 20))
	}
}
