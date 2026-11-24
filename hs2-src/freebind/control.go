package freebind

import (
	"context"
	"net"
	"syscall"
)

// Control is a net.ListenConfig Control that sets freebind when address is a
// specific IP. Use Chain to add it to an existing Control.
func Control(network, address string, c syscall.RawConn) error {
	if !Specific(address) {
		return nil
	}
	return c.Control(Set)
}

// Chain runs Control and then next (if non-nil).
func Chain(next func(network, address string, c syscall.RawConn) error) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		if err := Control(network, address, c); err != nil {
			return err
		}
		if next != nil {
			return next(network, address, c)
		}
		return nil
	}
}

// Listen is net.Listen("tcp"-family) with freebind for a specific address,
// plain TCP (never MPTCP), then Watch.
func Listen(network, addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: Control}
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(context.Background(), network, addr)
	if err == nil {
		Watch(addr)
	}
	return ln, err
}

// ListenPacket is net.ListenPacket with freebind for a specific address, then
// Watch.
func ListenPacket(network, addr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: Control}
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err == nil {
		Watch(addr)
	}
	return pc, err
}
