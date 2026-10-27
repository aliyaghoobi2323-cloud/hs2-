//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// clockMono is CLOCK_MONOTONIC in ns: one clock for every process on the
// host, so the client and the server (both ends of the rig run on one host,
// in two network namespaces) stamp and read the same time, and a message's
// one-way delay is its arrival minus the stamp it carries.
func clockMono() int64 {
	var ts syscall.Timespec
	syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0)
	return ts.Nano()
}
