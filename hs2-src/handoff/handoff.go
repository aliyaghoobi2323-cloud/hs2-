//go:build linux

// Package handoff is the zero-downtime upgrade (ZDU, proposals/zdu350.md)
// hand-off channel, building block B1: the local channel between the old
// process and the new one over which the old process hands over its
// listener and TUN fds (SCM_RIGHTS), the metadata of both processes, and,
// while it drains, its DRAINSTAT reports.
//
// The channel is an AF_UNIX SOCK_SEQPACKET socket (one message per packet,
// message boundaries kept, fds ride with the message that names them). Two
// ways to get one:
//
//   - Listen/Dial: a named socket under /run/hs2 (owner root, mode 0600),
//     where the peer is authenticated with SO_PEERCRED (uid 0) and the path
//     of its binary (/proc/PID/exe: this binary or an allowed one).
//   - Pair: a socketpair, one end handed to a child as an inherited fd.
//
// The package does not read HS2_ZDU: callers gate it (cmd/hs2 creates no
// socket while the key is off). Every receive has a deadline; any error
// (timeout, auth, version, a bad message) means "no hand-off": the new
// process opens everything itself (a cold start) and the old one keeps
// serving.
//
// Wire (all integers big endian), one message per packet:
//
//	[ver u8 = 1][type u8][len u16][payload len bytes]
//
//	HELLO      proto u16, features u32, inst.start u64, inst.rand u32,
//	           gen u32, config hash [32]
//	FDS        n u8, n x (namelen u8, name); the n fds ride as SCM_RIGHTS
//	READY      (empty)                          new -> old
//	GO         (empty)                          old -> new
//	ABORT      reason (utf-8, <= 512)           either way
//	DRAINSTAT  links u32, users u32             old -> new, every 2 s
//	LISTENING  (empty)                          new -> old (FeatListening)
//	UNUSED     n u8, n x (namelen u8, name)     new -> old (FeatUnused, b428)
//	LBSTATE    opaque engine blob (<= 32 KiB)   old -> new (FeatLBState, b438):
//	           right after FDS, and again right before GO
//
// A reader rejects an unknown version, an unknown type, a payload whose
// length does not match its type, and fds on any message but FDS (they are
// closed, never leaked).
package handoff

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// Proto is the hand-off protocol version a HELLO carries. Two processes hand
// off only when their Proto is equal; otherwise the new one ABORTs and the
// deploy falls back to a cold restart.
const Proto = 1

const wireVer = 1

// Message types.
const (
	TypeHello     = 1
	TypeFDs       = 2
	TypeReady     = 3
	TypeGo        = 4
	TypeAbort     = 5
	TypeDrainStat = 6
	// TypeListening (new process -> old, before READY, only when both Hellos
	// carry FeatListening): its listeners are up and the fds are adopted;
	// the old process moves link accept over now (zdu350 review M2).
	TypeListening = 7
	// TypeUnused (b428, new process -> old, right before READY, only when
	// both Hellos carry FeatUnused): the names of the handed-off fds this
	// process did not adopt (its config no longer binds them). At GO the old
	// process closes its copies of those sockets so the port is free for
	// another unit (moving a user port between tunnels). Payload as FDS
	// (n u8, names), n may be 0; no fds ride with it.
	TypeUnused = 8
	// TypeLBState (b438, old process -> new, only when both Hellos carry
	// FeatLBState): the old process's LB judge state (an engine blob, may be
	// empty), once right after FDS and once right before GO. The new process
	// seeds its judge from it (engine.ZDULBStateSeed); no fds ride with it.
	TypeLBState = 9
)

// FeatListening (Hello.Features): this side sends / waits for LISTENING.
const FeatListening uint32 = 1

// FeatUnused (b428, Hello.Features): this side sends / waits for UNUSED.
const FeatUnused uint32 = 2

// FeatLBState (b438, Hello.Features): this side sends / reads LBSTATE.
const FeatLBState uint32 = 4

// MaxLBState is the largest LBSTATE payload.
const MaxLBState = 32 << 10

const (
	hdrLen       = 4
	helloLen     = 2 + 4 + 8 + 4 + 4 + 32
	drainStatLen = 8
	maxAbort     = 512
	// MaxFDs is the most fds one FDS message carries (listeners per lb IP +
	// user ports + the TUN; prod has well under 20).
	MaxFDs  = 32
	maxName = 64
	maxMsg  = hdrLen + 1 + MaxFDs*(1+maxName)
)

// Errors. ErrTimeout and every other error of this package mean the same to
// a caller: no hand-off, go cold.
var (
	ErrTimeout  = errors.New("handoff: timeout")
	ErrProto    = errors.New("handoff: protocol version mismatch")
	ErrAuth     = errors.New("handoff: peer not allowed")
	ErrAborted  = errors.New("handoff: peer aborted")
	ErrBadMsg   = errors.New("handoff: malformed message")
	ErrUnexpect = errors.New("handoff: unexpected message")
)

// Hello is the metadata each side announces.
type Hello struct {
	Proto      uint16
	Features   uint32
	InstStart  uint64 // the process instance (engine zduInst): start ns
	InstRand   uint32 // and its random part
	Gen        uint32 // hand-off generation: old's Gen+1 in the new process
	ConfigHash [32]byte
}

// DrainStat is what the draining old process reports.
type DrainStat struct {
	Links uint32 // its live links (Iran: the foreign-drain term)
	Users uint32 // its user connections
}

// FD is one named fd of an FDS message. The receiver owns File.
type FD struct {
	Name string
	File *os.File
}

// Msg is one decoded message.
type Msg struct {
	Type   uint8
	Hello  Hello
	FDs    []FD
	Drain  DrainStat
	Reason string
	Names  []string // UNUSED (b428)
	Blob   []byte   // LBSTATE (b438)
}

// Conn is one end of the channel.
type Conn struct {
	c *net.UnixConn
	// LBState (old process, b438; nil: none) builds the LBSTATE payload when
	// both Hellos carry FeatLBState.
	LBState func() []byte
}

// NewConn wraps a connected SOCK_SEQPACKET unix socket.
func NewConn(c *net.UnixConn) *Conn { return &Conn{c: c} }

// Pair returns both ends of a socketpair(AF_UNIX, SOCK_SEQPACKET).
func Pair() (*Conn, *Conn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("handoff: socketpair: %w", err)
	}
	a, err := fileConn(fds[0], "handoff-a")
	if err != nil {
		unix.Close(fds[1])
		return nil, nil, err
	}
	b, err := fileConn(fds[1], "handoff-b")
	if err != nil {
		a.Close()
		return nil, nil, err
	}
	return a, b, nil
}

// FromFD wraps an inherited channel fd (a socketpair end given to a child).
func FromFD(fd int) (*Conn, error) { return fileConn(fd, "handoff") }

func fileConn(fd int, name string) (*Conn, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close() // FileConn dups
	nc, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("handoff: fileconn: %w", err)
	}
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		nc.Close()
		return nil, fmt.Errorf("handoff: fd %d is not a unix socket", fd)
	}
	return &Conn{c: uc}, nil
}

// File returns a dup of the channel fd (to pass to a child).
func (c *Conn) File() (*os.File, error) { return c.c.File() }

// Close closes the channel.
func (c *Conn) Close() error { return c.c.Close() }

// Unix is the underlying socket.
func (c *Conn) Unix() *net.UnixConn { return c.c }

func putHdr(b []byte, typ uint8, n int) {
	b[0] = wireVer
	b[1] = typ
	binary.BigEndian.PutUint16(b[2:], uint16(n))
}

// encode builds a message (no fds) for typ.
func encode(m *Msg) ([]byte, error) {
	switch m.Type {
	case TypeHello:
		b := make([]byte, hdrLen+helloLen)
		putHdr(b, TypeHello, helloLen)
		p := b[hdrLen:]
		h := m.Hello
		binary.BigEndian.PutUint16(p[0:], h.Proto)
		binary.BigEndian.PutUint32(p[2:], h.Features)
		binary.BigEndian.PutUint64(p[6:], h.InstStart)
		binary.BigEndian.PutUint32(p[14:], h.InstRand)
		binary.BigEndian.PutUint32(p[18:], h.Gen)
		copy(p[22:], h.ConfigHash[:])
		return b, nil
	case TypeFDs:
		if len(m.FDs) == 0 || len(m.FDs) > MaxFDs {
			return nil, fmt.Errorf("handoff: %d fds (1..%d)", len(m.FDs), MaxFDs)
		}
		n := 1
		for _, f := range m.FDs {
			if len(f.Name) == 0 || len(f.Name) > maxName {
				return nil, fmt.Errorf("handoff: fd name %q", f.Name)
			}
			n += 1 + len(f.Name)
		}
		b := make([]byte, hdrLen+n)
		putHdr(b, TypeFDs, n)
		p := b[hdrLen:]
		p[0] = byte(len(m.FDs))
		i := 1
		for _, f := range m.FDs {
			p[i] = byte(len(f.Name))
			i += 1 + copy(p[i+1:], f.Name)
		}
		return b, nil
	case TypeUnused:
		if len(m.Names) > MaxFDs {
			return nil, fmt.Errorf("handoff: %d unused names (0..%d)", len(m.Names), MaxFDs)
		}
		n := 1
		for _, nm := range m.Names {
			if len(nm) == 0 || len(nm) > maxName {
				return nil, fmt.Errorf("handoff: unused name %q", nm)
			}
			n += 1 + len(nm)
		}
		b := make([]byte, hdrLen+n)
		putHdr(b, TypeUnused, n)
		p := b[hdrLen:]
		p[0] = byte(len(m.Names))
		i := 1
		for _, nm := range m.Names {
			p[i] = byte(len(nm))
			i += 1 + copy(p[i+1:], nm)
		}
		return b, nil
	case TypeReady, TypeGo, TypeListening:
		b := make([]byte, hdrLen)
		putHdr(b, m.Type, 0)
		return b, nil
	case TypeLBState:
		if len(m.Blob) > MaxLBState {
			return nil, fmt.Errorf("handoff: lbstate %d bytes (max %d)", len(m.Blob), MaxLBState)
		}
		b := make([]byte, hdrLen+len(m.Blob))
		putHdr(b, TypeLBState, len(m.Blob))
		copy(b[hdrLen:], m.Blob)
		return b, nil
	case TypeAbort:
		r := m.Reason
		if len(r) > maxAbort {
			r = r[:maxAbort]
		}
		b := make([]byte, hdrLen+len(r))
		putHdr(b, TypeAbort, len(r))
		copy(b[hdrLen:], r)
		return b, nil
	case TypeDrainStat:
		b := make([]byte, hdrLen+drainStatLen)
		putHdr(b, TypeDrainStat, drainStatLen)
		binary.BigEndian.PutUint32(b[hdrLen:], m.Drain.Links)
		binary.BigEndian.PutUint32(b[hdrLen+4:], m.Drain.Users)
		return b, nil
	}
	return nil, fmt.Errorf("handoff: encode type %d", m.Type)
}

// decode parses one packet; nfds is how many fds came with it (FDS only).
// The fds themselves are attached by the caller.
func decode(b []byte, nfds int) (*Msg, []string, error) {
	if len(b) < hdrLen || b[0] != wireVer {
		return nil, nil, ErrBadMsg
	}
	typ := b[1]
	n := int(binary.BigEndian.Uint16(b[2:]))
	p := b[hdrLen:]
	if len(p) != n {
		return nil, nil, ErrBadMsg
	}
	if typ != TypeFDs && nfds != 0 {
		return nil, nil, ErrBadMsg
	}
	m := &Msg{Type: typ}
	switch typ {
	case TypeHello:
		if n != helloLen {
			return nil, nil, ErrBadMsg
		}
		m.Hello = Hello{
			Proto:     binary.BigEndian.Uint16(p[0:]),
			Features:  binary.BigEndian.Uint32(p[2:]),
			InstStart: binary.BigEndian.Uint64(p[6:]),
			InstRand:  binary.BigEndian.Uint32(p[14:]),
			Gen:       binary.BigEndian.Uint32(p[18:]),
		}
		copy(m.Hello.ConfigHash[:], p[22:])
	case TypeFDs:
		if n < 1 || int(p[0]) == 0 || int(p[0]) > MaxFDs || int(p[0]) != nfds {
			return nil, nil, ErrBadMsg
		}
		names := make([]string, 0, p[0])
		i := 1
		for k := 0; k < int(p[0]); k++ {
			if i >= len(p) {
				return nil, nil, ErrBadMsg
			}
			l := int(p[i])
			if l == 0 || l > maxName || i+1+l > len(p) {
				return nil, nil, ErrBadMsg
			}
			names = append(names, string(p[i+1:i+1+l]))
			i += 1 + l
		}
		if i != len(p) {
			return nil, nil, ErrBadMsg
		}
		return m, names, nil
	case TypeUnused:
		if n < 1 || int(p[0]) > MaxFDs {
			return nil, nil, ErrBadMsg
		}
		i := 1
		for k := 0; k < int(p[0]); k++ {
			if i >= len(p) {
				return nil, nil, ErrBadMsg
			}
			l := int(p[i])
			if l == 0 || l > maxName || i+1+l > len(p) {
				return nil, nil, ErrBadMsg
			}
			m.Names = append(m.Names, string(p[i+1:i+1+l]))
			i += 1 + l
		}
		if i != len(p) {
			return nil, nil, ErrBadMsg
		}
	case TypeReady, TypeGo, TypeListening:
		if n != 0 {
			return nil, nil, ErrBadMsg
		}
	case TypeLBState:
		if n > MaxLBState {
			return nil, nil, ErrBadMsg
		}
		m.Blob = append([]byte(nil), p...)
	case TypeAbort:
		if n > maxAbort {
			return nil, nil, ErrBadMsg
		}
		m.Reason = string(p)
	case TypeDrainStat:
		if n != drainStatLen {
			return nil, nil, ErrBadMsg
		}
		m.Drain = DrainStat{Links: binary.BigEndian.Uint32(p), Users: binary.BigEndian.Uint32(p[4:])}
	default:
		return nil, nil, ErrBadMsg
	}
	return m, nil, nil
}

// Send writes one message; the fds of an FDS message ride as SCM_RIGHTS
// (the sender keeps its own copies: the kernel dups them into the receiver).
func (c *Conn) Send(m *Msg, timeout time.Duration) error {
	b, err := encode(m)
	if err != nil {
		return err
	}
	var oob []byte
	if m.Type == TypeFDs {
		// Never f.File.Fd(): on a pollable file (net's File() dups) it clears
		// O_NONBLOCK on the SHARED file description, which makes the live
		// listener/UDP socket of this process blocking (a paused Accept then
		// cannot be kicked). Dup the raw fd under Control instead: a dup
		// shares the description but changes no flag.
		fds := make([]int, 0, len(m.FDs))
		defer func() {
			for _, fd := range fds {
				unix.Close(fd)
			}
		}()
		for _, f := range m.FDs {
			fd, err := rawDup(f.File)
			if err != nil {
				return fmt.Errorf("handoff: fd %s: %w", f.Name, err)
			}
			fds = append(fds, fd)
		}
		oob = unix.UnixRights(fds...)
	}
	if timeout > 0 {
		c.c.SetWriteDeadline(time.Now().Add(timeout))
		defer c.c.SetWriteDeadline(time.Time{})
	}
	_, _, err = c.c.WriteMsgUnix(b, oob, nil)
	runtime.KeepAlive(m.FDs) // the files must not be finalized (closed) before the write
	return mapErr(err)
}

// rawDup returns a close-on-exec dup of f's fd without touching the file's
// blocking mode (see Send).
func rawDup(f *os.File) (int, error) {
	if f == nil {
		return -1, errors.New("nil file")
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return -1, err
	}
	nfd, derr := -1, error(nil)
	if err := rc.Control(func(fd uintptr) {
		nfd, derr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return -1, err
	}
	return nfd, derr
}

// Dup returns a close-on-exec dup of f as a new file, without f.Fd() (which
// makes a pollable file's shared description blocking).
func Dup(f *os.File, name string) (*os.File, error) {
	fd, err := rawDup(f)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Recv reads one message, waiting at most timeout (0 = no deadline). An
// ABORT comes back as the message and ErrAborted.
func (c *Conn) Recv(timeout time.Duration) (*Msg, error) {
	if timeout > 0 {
		c.c.SetReadDeadline(time.Now().Add(timeout))
		defer c.c.SetReadDeadline(time.Time{})
	}
	b := make([]byte, max(maxMsg, hdrLen+MaxLBState)+1)
	oob := make([]byte, unix.CmsgSpace(4*MaxFDs))
	n, oobn, flags, _, err := c.c.ReadMsgUnix(b, oob)
	if err != nil {
		return nil, mapErr(err)
	}
	fds, perr := parseRights(oob[:oobn])
	closeAll := func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}
	if perr != nil || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		closeAll()
		return nil, ErrBadMsg
	}
	if n == 0 {
		closeAll()
		return nil, ErrBadMsg // a SEQPACKET peer that closed reads as EOF, not 0
	}
	m, names, err := decode(b[:n], len(fds))
	if err != nil {
		closeAll()
		return nil, err
	}
	if m.Type == TypeFDs {
		m.FDs = make([]FD, len(fds))
		for i, fd := range fds {
			unix.CloseOnExec(fd)
			m.FDs[i] = FD{Name: names[i], File: os.NewFile(uintptr(fd), names[i])}
		}
	}
	if m.Type == TypeAbort {
		return m, fmt.Errorf("%w: %s", ErrAborted, m.Reason)
	}
	return m, nil
}

func parseRights(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, s := range scms {
		r, err := unix.ParseUnixRights(&s)
		if err != nil {
			for _, fd := range fds {
				unix.Close(fd)
			}
			return nil, err
		}
		fds = append(fds, r...)
	}
	return fds, nil
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	return err
}

// expect reads one message and requires typ.
func (c *Conn) expect(typ uint8, timeout time.Duration) (*Msg, error) {
	m, err := c.Recv(timeout)
	if err != nil {
		return nil, err
	}
	if m.Type != typ {
		for _, f := range m.FDs {
			f.File.Close()
		}
		return nil, fmt.Errorf("%w: got type %d, want %d", ErrUnexpect, m.Type, typ)
	}
	return m, nil
}
