package engine

// probe rig knob (scratch), built only into trees with GrowByTrial:
// Z5_TRIAL=0 turns the trials off (the window as far as its need: N4).

import "os"

func init() {
	if os.Getenv("Z5_TRIAL") == "0" {
		SmuxGrowByTrial = false
	}
}
