package engine

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// cp331 (lab/rb331 v6): HS2_CALLPATH_LOG=1 (Iran, default off) logs a call's
// sub placement: each time the set of live subs of a copying (call-on)
// connection, or of a connection from a user port in [31000,31100) (the lab
// call probes' -callport), changes, one line with every live sub's path and
// link (local socket address). Log only; nothing else changes.
var cp331On atomic.Bool

func init() { cp331On.Store(envBoolDefault("HS2_CALLPATH_LOG", false)) }

const cp331LogMax = 20000

var cp331Logged atomic.Int64

type cp331State struct {
	port int    // the user connection's source port (0: unknown)
	sig  string // the last logged placement
}

// cp331SetPort: the user connection's source port (serveUserMConn).
func (m *mConn) cp331SetPort(user any) {
	if !cp331On.Load() {
		return
	}
	if c, ok := user.(interface{ RemoteAddr() net.Addr }); ok {
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			m.mu.Lock()
			m.cp331.port = a.Port
			m.mu.Unlock()
		}
	}
}

func cp331LinkTag(l Link) string {
	if ml, ok := l.(*mtcpLink); ok && ml != nil && ml.tls != nil {
		if c := ml.tls.TCPConn(); c != nil {
			return c.RemoteAddr().String() // the dialer's port: unique per link (the local side is the shared :2096)
		}
	}
	return fmt.Sprintf("%p", l)
}

// cp331LogLocked (m.mu held; callRollLocked, once per window).
func (m *mConn) cp331LogLocked(now time.Time) {
	if !cp331On.Load() || !m.iran {
		return
	}
	probe := m.cp331.port >= 31000 && m.cp331.port < 31100
	if !m.callOn && !probe {
		return
	}
	var b strings.Builder
	n := 0
	for _, s := range m.subs {
		if s.dead || s.retired {
			continue
		}
		l, _ := s.key.(Link)
		path := ""
		if mtr := m.subMeterOf(s); mtr != nil {
			path = mtr.path
		}
		tag := "?"
		if l != nil {
			tag = cp331LinkTag(l)
		}
		fmt.Fprintf(&b, " %d=%s@%s", s.idx, path, tag)
		n++
	}
	sig := b.String()
	if sig == m.cp331.sig {
		return
	}
	m.cp331.sig = sig
	if cp331Logged.Add(1) > cp331LogMax {
		return
	}
	log.Printf("%x callpath port %d call %v subs %d:%s", m.id, m.cp331.port, m.callOn, n, sig)
}
