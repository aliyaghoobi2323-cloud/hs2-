//go:build !linux

// Package mmsg: batches need sendmmsg/recvmmsg (Linux); elsewhere callers send
// and receive one datagram at a time.
package mmsg

import (
	"errors"
	"syscall"
)

// Supported reports whether batches work on this platform.
const Supported = false

var errUnsupported = errors.New("mmsg: not supported on this platform")

// RawSockaddrInet4 stands in for the Linux type.
type RawSockaddrInet4 struct{}

// Batch is unusable off Linux.
type Batch struct{}

func NewBatch(int) *Batch                   { return &Batch{} }
func (b *Batch) Size() int                  { return 0 }
func (b *Batch) Name(int) *RawSockaddrInet4 { return nil }
func Inet4([]byte, int) *RawSockaddrInet4   { return nil }
func (b *Batch) Send(syscall.RawConn, [][]byte, *RawSockaddrInet4, []byte) (int, error) {
	return 0, errUnsupported
}
func (b *Batch) Recv(syscall.RawConn, [][]byte, []int, []bool, bool, [][]byte, []int) (int, error) {
	return 0, errUnsupported
}
