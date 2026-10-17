//go:build race

package engine

// The race detector makes the model's runs about ten times as slow: the
// table does not hold them to the 1 s budget then.
func init() { wmRace = true }
