//go:build linux

package main

import "syscall"

// rcvbufControl sets a socket's receive buffer to n bytes before it
// connects (so its window scale fits it): a frozen download's zero window
// comes after n bytes, not after what autotuning grew (frozen.go).
func rcvbufControl(n int) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
		}); err != nil {
			return err
		}
		return serr
	}
}
