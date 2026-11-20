//go:build !linux

package engine

import (
	"net"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

func censorTCP(tc *net.TCPConn, d *censor.Death) {}
