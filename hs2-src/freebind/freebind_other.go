//go:build !linux

package freebind

// Set is a no-op off Linux.
func Set(fd uintptr) {}

// Get is false off Linux.
func Get(fd uintptr) bool { return false }
