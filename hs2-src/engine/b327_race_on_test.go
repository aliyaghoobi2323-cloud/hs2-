//go:build race

package engine

// b327RaceOn: the race detector is on; heavy simulations shrink so -race
// finishes (correctness checks unchanged, only fewer samples).
const b327RaceOn = true
