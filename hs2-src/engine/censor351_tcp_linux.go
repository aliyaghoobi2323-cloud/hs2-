//go:build linux

package engine

import (
	"net"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

func censorTCP(tc *net.TCPConn, d *censor.Death) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) {
		info, e := tcpInfo(int(fd))
		if e != nil {
			return
		}
		d.HaveTCP = true
		d.Bytes = info.Bytes_acked + info.Bytes_received
		d.LastSentMs = info.Last_data_sent
		d.LastRecvMs = info.Last_data_recv
		d.MinRTTUs = info.Min_rtt
	})
}
