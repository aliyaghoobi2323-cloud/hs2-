//go:build !linux

package udpcarrier

import "net"

func udpConnWriteBatch(*net.UDPConn) func([][]byte) error       { return nil }
func udpConnReadBatch(*net.UDPConn) func(fn func([]byte)) error { return nil }

type udpListenBatch struct{}

func newUDPListenBatch(*net.UDPConn, bool) *udpListenBatch          { return nil }
func (l *udpListenBatch) read(func([]byte, net.Addr, net.IP)) error { return nil }
func udpWriteBatchTo(uc *net.UDPConn, ps [][]byte, to *net.UDPAddr, src net.IP) error {
	for _, p := range ps {
		if _, err := uc.WriteToUDP(p, to); err != nil {
			return err
		}
	}
	return nil
}
