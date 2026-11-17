package engine

import (
	"math/rand/v2"
	"sync/atomic"
)

// pickRandHook (rel69h, tests only): when set, the pool pickers' random
// choices — the equal-key tie-break (pickLockedFP, pickPlaneTiersFP) and the
// steering draw (steerPick) — come from it, so a test can walk every choice
// the picker can make (TestLMFastPickEquivalence). nil in production:
// math/rand/v2, as before.
var pickRandHook atomic.Pointer[pickChooser]

// pickChooser returns one of n choices (0 <= result < n).
type pickChooser struct{ choose func(n int) int }

// pickTie is the reservoir tie-break "the ties-th equal candidate replaces
// the current one" (rand.IntN(ties) == 0); under the hook a two-way choice.
func pickTie(ties int) bool {
	if h := pickRandHook.Load(); h != nil {
		return h.choose(2) == 0
	}
	return rand.IntN(ties) == 0
}
