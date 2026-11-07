//go:build !(linux || darwin || freebsd)

package engine

import (
	"net"
	"time"
)

// acmePeekRequestLine: no MSG_PEEK path on this platform, so the responder
// never intercepts (every connection takes the normal path untouched).
func acmePeekRequestLine(net.Conn, time.Duration) ([]byte, bool) { return nil, false }
