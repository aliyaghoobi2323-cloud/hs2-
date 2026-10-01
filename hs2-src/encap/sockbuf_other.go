//go:build !linux

package encap

import "syscall"

// setSockBufs asks for n-byte send and receive buffers on c (best-effort; the
// kernel caps the request at its own limits).
func setSockBufs(c syscall.Conn, n int) {
	if b, ok := c.(interface {
		SetReadBuffer(int) error
		SetWriteBuffer(int) error
	}); ok {
		b.SetReadBuffer(n)
		b.SetWriteBuffer(n)
	}
}
