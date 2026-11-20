//go:build linux

package handoff

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Default timeouts. Step is one HELLO/FDS exchange on a local socket (a
// healthy peer answers in microseconds); Ready is how long the old process
// waits for the new one to warm its links (plan §3 step 5); DrainSilence is
// how long the new process waits for a DRAINSTAT before it treats the old
// process as gone (it reports every 2 s; the engine also drops a report
// older than 10 s on its own).
const (
	StepTimeout     = 5 * time.Second
	ReadyTimeout    = 90 * time.Second
	DrainSilence    = 10 * time.Second
	DrainStatPeriod = 2 * time.Second
)

// Policy is who may be on the other end of a named channel.
type Policy struct {
	UID     uint32   // required peer uid (0: root)
	Self    string   // this binary (resolved); a peer running it is allowed
	Allowed []string // other allowed binaries (the new release's path)
}

// DefaultPolicy: root, this binary, plus allowed.
func DefaultPolicy(allowed ...string) Policy {
	self, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	return Policy{UID: 0, Self: self, Allowed: allowed}
}

// Cred is what SO_PEERCRED reported plus the peer's binary.
type Cred struct {
	PID int32
	UID uint32
	Exe string
}

// PeerCred reads SO_PEERCRED and /proc/PID/exe of the socket's peer.
func PeerCred(c *net.UnixConn) (Cred, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var uc *unix.Ucred
	var serr error
	if err := rc.Control(func(fd uintptr) {
		uc, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Cred{}, err
	}
	if serr != nil {
		return Cred{}, serr
	}
	cr := Cred{PID: uc.Pid, UID: uc.Uid}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", uc.Pid))
	if err == nil {
		cr.Exe = strings.TrimSuffix(exe, " (deleted)") // the old binary was replaced on disk
	}
	return cr, nil
}

// Check applies the policy to a peer.
func (p Policy) Check(cr Cred) error {
	if cr.UID != p.UID {
		return fmt.Errorf("%w: uid %d (want %d)", ErrAuth, cr.UID, p.UID)
	}
	if cr.Exe == "" {
		return fmt.Errorf("%w: pid %d binary unknown", ErrAuth, cr.PID)
	}
	if p.Self != "" && cr.Exe == p.Self {
		return nil
	}
	for _, a := range p.Allowed {
		if a != "" && cr.Exe == a {
			return nil
		}
	}
	return fmt.Errorf("%w: binary %s", ErrAuth, cr.Exe)
}

func (c *Conn) auth(p Policy) error {
	cr, err := PeerCred(c.c)
	if err != nil {
		return fmt.Errorf("%w: peercred: %v", ErrAuth, err)
	}
	return p.Check(cr)
}

// Listener is the named channel the old process opens for the new one.
type Listener struct {
	l    *net.UnixListener
	path string
	pol  Policy
}

// Listen creates the channel socket at path: its directory must be a real
// directory owned by root (uid of the policy) and not writable by others
// (created 0700 when missing); the socket is created under a private name,
// set to mode 0600 and renamed into place, so it is never reachable with a
// wider mode. A stale socket at path is replaced (a crashed old process);
// anything else there is an error.
func Listen(path string, pol Policy) (*Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("handoff: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("handoff: %w", err)
	}
	sys, _ := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || sys == nil || sys.Uid != pol.UID || st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("handoff: %s must be a directory of uid %d, not group/other writable", dir, pol.UID)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("handoff: %s exists and is not a socket", path)
		}
		os.Remove(path)
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	os.Remove(tmp)
	old := unix.Umask(0o177)
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: tmp, Net: "unixpacket"})
	unix.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("handoff: listen: %w", err)
	}
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		l.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("handoff: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		l.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("handoff: %w", err)
	}
	return &Listener{l: l, path: path, pol: pol}, nil
}

// Path is the socket path.
func (l *Listener) Path() string { return l.path }

// Close closes the listener and removes the socket file.
func (l *Listener) Close() error {
	err := l.l.Close()
	os.Remove(l.path)
	return err
}

// Accept waits up to timeout for one peer that passes the policy. A peer
// that fails it is closed and Accept returns ErrAuth (the caller decides
// whether to wait for another).
func (l *Listener) Accept(timeout time.Duration) (*Conn, error) {
	if timeout > 0 {
		l.l.SetDeadline(time.Now().Add(timeout))
		defer l.l.SetDeadline(time.Time{})
	}
	uc, err := l.l.AcceptUnix()
	if err != nil {
		return nil, mapErr(err)
	}
	c := &Conn{c: uc}
	if err := c.auth(l.pol); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Dial connects to the old process's channel and checks that its owner
// passes the policy.
func Dial(path string, pol Policy, timeout time.Duration) (*Conn, error) {
	d := net.Dialer{Timeout: timeout}
	nc, err := d.Dial("unixpacket", path)
	if err != nil {
		return nil, mapErr(err)
	}
	c := &Conn{c: nc.(*net.UnixConn)}
	if err := c.auth(pol); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) abort(reason string) {
	c.Send(&Msg{Type: TypeAbort, Reason: reason}, time.Second)
}

func compatible(a, b Hello) error {
	if a.Proto != b.Proto {
		return fmt.Errorf("%w: %d vs %d", ErrProto, a.Proto, b.Proto)
	}
	return nil
}

// Give is the old process's side, after Accept: HELLO in, HELLO out, FDS
// out, then wait for READY (ready timeout) and answer GO. On any error it
// sends ABORT (when it can) and the old process keeps serving with nothing
// changed: the fds it sent are dups, its own copies stay open.
func (c *Conn) Give(self Hello, fds []FD, step, ready time.Duration) (Hello, error) {
	return c.GiveListening(self, fds, step, ready, nil)
}

// GiveListening is Give with a hook for the new process's LISTENING: when
// both Hellos carry FeatListening, listening runs once the new process sent
// it (its listeners are up, the fds adopted); otherwise right after FDS. The
// READY wait (ready) counts from FDS. listening may be nil.
func (c *Conn) GiveListening(self Hello, fds []FD, step, ready time.Duration, listening func()) (Hello, error) {
	m, err := c.expect(TypeHello, step)
	if err != nil {
		if !errors.Is(err, ErrAborted) {
			c.abort(err.Error())
		}
		return Hello{}, err
	}
	peer := m.Hello
	if err := compatible(self, peer); err != nil {
		c.abort(err.Error())
		return peer, err
	}
	if err := c.Send(&Msg{Type: TypeHello, Hello: self}, step); err != nil {
		return peer, err
	}
	if err := c.Send(&Msg{Type: TypeFDs, FDs: fds}, step); err != nil {
		c.abort(err.Error())
		return peer, err
	}
	end := time.Now().Add(ready)
	if self.Features&peer.Features&FeatListening != 0 {
		if _, err := c.expect(TypeListening, ready); err != nil {
			if !errors.Is(err, ErrAborted) {
				c.abort(err.Error())
			}
			return peer, err
		}
	}
	if listening != nil {
		listening()
	}
	left := time.Until(end)
	if left < time.Millisecond {
		left = time.Millisecond
	}
	if _, err := c.expect(TypeReady, left); err != nil {
		if !errors.Is(err, ErrAborted) {
			c.abort(err.Error())
		}
		return peer, err
	}
	return peer, c.Send(&Msg{Type: TypeGo}, step)
}

// Listening tells the old process this one's listeners are up (before the
// warm wait and READY); a no-op unless both Hellos carry FeatListening. The
// bit is the one of the Hello this side SENT in Take (zdu391: the old process
// decides from that Hello whether it waits for LISTENING; a different Hello
// here sent LISTENING where the old process waited for READY).
func (t *Taken) Listening(step time.Duration) error {
	if t.Self.Features&t.Peer.Features&FeatListening == 0 {
		return nil
	}
	return t.Conn.Send(&Msg{Type: TypeListening}, step)
}

// Taken is what the new process received.
type Taken struct {
	Conn *Conn
	Self Hello // the Hello this side sent (Take)
	Peer Hello
	FDs  []FD
}

// File returns the fd named name and removes it from t (the caller owns it).
func (t *Taken) File(name string) *os.File {
	for i, f := range t.FDs {
		if f.Name == name {
			t.FDs = append(t.FDs[:i], t.FDs[i+1:]...)
			return f.File
		}
	}
	return nil
}

// Close closes every fd not taken and the channel.
func (t *Taken) Close() {
	for _, f := range t.FDs {
		f.File.Close()
	}
	t.FDs = nil
	t.Conn.Close()
}

// Take is the new process's side: HELLO out, HELLO in (ABORT on a version
// mismatch), FDS in. Any error = cold start (the fds received so far are
// closed).
func (c *Conn) Take(self Hello, step time.Duration) (*Taken, error) {
	if err := c.Send(&Msg{Type: TypeHello, Hello: self}, step); err != nil {
		return nil, err
	}
	m, err := c.expect(TypeHello, step)
	if err != nil {
		if !errors.Is(err, ErrAborted) {
			c.abort(err.Error())
		}
		return nil, err
	}
	if err := compatible(self, m.Hello); err != nil {
		c.abort(err.Error())
		return nil, err
	}
	f, err := c.expect(TypeFDs, step)
	if err != nil {
		if !errors.Is(err, ErrAborted) {
			c.abort(err.Error())
		}
		return nil, err
	}
	return &Taken{Conn: c, Self: self, Peer: m.Hello, FDs: f.FDs}, nil
}

// Ready tells the old process the new one is up (its links are warm) and
// waits for GO: from then on the old process drains.
func (t *Taken) Ready(step time.Duration) error {
	if err := t.Conn.Send(&Msg{Type: TypeReady}, step); err != nil {
		return err
	}
	_, err := t.Conn.expect(TypeGo, step)
	return err
}

// SendDrainStat is the old process's periodic report while it drains.
func (c *Conn) SendDrainStat(d DrainStat) error {
	return c.Send(&Msg{Type: TypeDrainStat, Drain: d}, StepTimeout)
}

// FollowDrain is the new process's DRAINSTAT reader: set(links) for every
// report (Iran: LinkManager.SetZDUForeignDrain), set(0) when the old process
// is gone (EOF, ABORT, a bad message, or silence longer than silence). It
// returns when the channel ends.
func (c *Conn) FollowDrain(set func(links int), silence time.Duration) error {
	for {
		m, err := c.Recv(silence)
		if err != nil {
			set(0)
			return err
		}
		if m.Type != TypeDrainStat {
			for _, f := range m.FDs {
				f.File.Close()
			}
			set(0)
			return fmt.Errorf("%w: type %d during drain", ErrUnexpect, m.Type)
		}
		set(int(m.Drain.Links))
	}
}

// TakeOrCold runs take; when it fails (timeout, auth, version, anything)
// it runs cold and returns (nil, take's error, cold's error). It never
// leaves the process without its fds: either it has the handed-off ones or
// cold opened its own.
func TakeOrCold(take func() (*Taken, error), cold func() error) (*Taken, error, error) {
	t, err := take()
	if err == nil {
		return t, nil, nil
	}
	return nil, err, cold()
}
