package engine

import "sync/atomic"

// TuneInt is a switch (HS2_TUNE_*) that goroutines read while it may be set:
// the process sets it once at its start, but the package's tests set it
// between rigs whose loops (the cores' runner, the mover, the exit's hint
// loop) still run; plain ints there are data races (R1 round 4's -race).
type TuneInt struct{ v atomic.Int64 }

func newTuneInt(v int) *TuneInt {
	t := &TuneInt{}
	t.v.Store(int64(v))
	return t
}

// Get is the switch's value.
func (t *TuneInt) Get() int { return int(t.v.Load()) }

// Set sets it.
func (t *TuneInt) Set(v int) { t.v.Store(int64(v)) }
