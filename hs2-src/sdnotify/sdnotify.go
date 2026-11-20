// Package sdnotify sends systemd service notifications (sd_notify(3)) without
// cgo: one unix datagram to $NOTIFY_SOCKET. It is the ZDU building block for
// Type=notify (proposals/zdu350.md §4): READY=1 once the tunnel is up,
// STOPPING=1 on shutdown, and MAINPID=<child> + READY=1 when a live handover
// passes probation, so systemd tracks the new process.
//
// Without $NOTIFY_SOCKET (not under systemd, or a unit that is not
// Type=notify) every call is a no-op that reports sent=false, so callers
// need no guard.
package sdnotify

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env is the variable systemd sets for a notify unit.
const Env = "NOTIFY_SOCKET"

// writeTimeout bounds one send: systemd reads the socket quickly, and a stuck
// manager must never stall the tunnel.
var writeTimeout = 2 * time.Second

// ErrBadSocket: $NOTIFY_SOCKET is neither an absolute path nor an abstract
// name ("@..."). vsock addresses are not supported.
var ErrBadSocket = errors.New("sdnotify: unsupported " + Env)

// addr turns $NOTIFY_SOCKET into a unixgram address ("" = not set).
func addr(v string) (*net.UnixAddr, error) {
	switch {
	case v == "":
		return nil, nil
	case strings.HasPrefix(v, "@"):
		// abstract namespace: Go's net package maps a leading '@' itself
		return &net.UnixAddr{Name: v, Net: "unixgram"}, nil
	case strings.HasPrefix(v, "/"):
		return &net.UnixAddr{Name: v, Net: "unixgram"}, nil
	}
	return nil, ErrBadSocket
}

// Notify sends state (newline-separated assignments, e.g. "READY=1") to
// $NOTIFY_SOCKET. sent is false and err nil when the variable is unset.
func Notify(state string) (sent bool, err error) {
	a, err := addr(os.Getenv(Env))
	if a == nil || err != nil {
		return false, err
	}
	c, err := net.DialUnix("unixgram", nil, a)
	if err != nil {
		return false, err
	}
	defer c.Close()
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// Ready: the service finished starting (Type=notify leaves "activating").
func Ready() (bool, error) { return Notify("READY=1") }

// Stopping: the service began its shutdown.
func Stopping() (bool, error) { return Notify("STOPPING=1") }

// Status sets the free-form status line systemctl status shows.
func Status(s string) (bool, error) {
	return Notify("STATUS=" + strings.ReplaceAll(s, "\n", " "))
}

// MainPID hands the unit's main process to pid (the ZDU child after
// probation) in the same datagram as READY=1, so systemd never sees a
// moment without a ready main process. With NotifyAccess=main only the
// current main process may send this, which is the old process here.
func MainPID(pid int) (bool, error) {
	if pid <= 0 {
		return false, errors.New("sdnotify: bad pid")
	}
	return Notify("MAINPID=" + strconv.Itoa(pid) + "\nREADY=1")
}
