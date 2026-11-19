package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTTY (b376): a real terminal answers the termios ioctl; /dev/null and
// other character devices do not (the old char-device test took /dev/null
// for a terminal, so `hs2 tunnel join </dev/null` showed the no-echo prompt).
func isTTY(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
