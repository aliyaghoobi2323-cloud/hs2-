//go:build !linux

package engine

// setMaxSeg: none off Linux.
func setMaxSeg(fd uintptr, n int) {}
