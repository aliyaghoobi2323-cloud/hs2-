//go:build !linux

package encap

// Raw-socket encapsulations are Linux-only; dialRawFn/listenRawFn stay nil, so
// dialRaw/listenRaw return errRawUnsupported. The udp encapsulation works on
// every platform.
