//go:build !linux

package main

import "time"

// clockMono: the rig runs on Linux only; elsewhere the wall clock stands in,
// so the module still builds.
func clockMono() int64 { return time.Now().UnixNano() }
