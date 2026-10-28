//go:build !linux

package main

import "syscall"

// rcvbufControl: the rig runs on Linux only; elsewhere the buffer is the
// system's, so the module still builds.
func rcvbufControl(n int) func(network, address string, c syscall.RawConn) error { return nil }
