package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// R1 round 2, candidate β2 (docs/architecture/wip/r1/decisions.fa.md T16 to
// T18; REDESIGN §3.5 C5): QUIET links at the pool level.
//
// The round-2 diagnosis measured that on one TCP connection with BBRv1 a
// light flow cannot stay within RTT_floor + 20 ms next to bulk (PROBE_RTT
// stalls the link'