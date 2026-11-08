package engine

import "syscall"

// wedgeMSS gives a test socket an internet path's segment size (see
// guardedPair).
func wedgeMSS(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 1448)
	}); err != nil {
		return err
	}
	return serr
}
