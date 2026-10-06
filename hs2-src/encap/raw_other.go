//go:build !linux

package encap

// Raw-socket encapsulations are Linux-only; dialRawFn/listenRawFn stay nil, so
// dialRaw/listenRaw return errRawUnsupported. The udp encapsulation works on
// every platform.

// ReleaseAllEchoGuards and SweepStaleEchoGuards: no reply rules off Linux.
func ReleaseAllEchoGuards()              {}
func SweepStaleEchoGuards(bool) []string { return nil }

// SendRefused: no raw sockets off Linux.
func SendRefused() uint64 { return 0 }
