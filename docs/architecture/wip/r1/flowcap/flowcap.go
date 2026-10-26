//go:build linux

// Command flowcap records, for one day, per-flow traffic features of the user
// connections on the hs2 edge, for replay in the C1 classifier model test
// (R1 plan, task 3; REDESIGN §3.1 and §5.1).
//
// Every interval (100 ms) it reads through netlink sock_diag (INET_DIAG with
// tcp_info) the cumulative byte counters of the TCP sockets accepted on the
// hs2 user ports (forward_ports of the hs2 config), and appends to a gzip'd
// JSON-lines file one line per flow that moved bytes in that interval:
//
//	{"f":<flow id>,"t":<ms since start>,"up":<bytes client→server>,"dn":<bytes server→client>}
//
// up is the growth of tcpi_bytes_received of the accepted socket, dn the
// growth of its tcpi_bytes_acked. A socket destroyed between two reads is
// caught by the sock_diag destroy multicast groups, which carry its final
// counters, so short flows and the last interval of a closing flow are kept.
//
// Privacy: no address, port, hostname or payload is written anywhere. The
// address fields of the kernel replies are never read; the local port is read
// only to keep the sockets of a user port. A flow is keyed in memory by the
// kernel socket cookie, and its id f is a random 53-bit number drawn when the
// flow is first seen, never derived from the connection.
//
// It only reads: no socket option, no service and no config is changed.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var ne = binary.NativeEndian // netlink and sock_diag structs are in host order

const (
	netlinkSockDiag     = 4     // NETLINK_SOCK_DIAG
	sockDiagByFamily    = 20    // SOCK_DIAG_BY_FAMILY
	nlmsgError          = 2     // NLMSG_ERROR
	nlmsgDone           = 3     // NLMSG_DONE
	nlmFRequest         = 0x1   // NLM_F_REQUEST
	nlmFDump            = 0x300 // NLM_F_ROOT|NLM_F_MATCH
	nlHdrLen            = 16    // struct nlmsghdr
	diagReqLen          = 56    // struct inet_diag_req_v2
	diagMsgLen          = 72    // struct inet_diag_msg
	nlaTypeMask         = 0x3fff
	inetDiagReqBytecode = 1  // INET_DIAG_REQ_BYTECODE
	inetDiagInfo        = 2  // INET_DIAG_INFO: struct tcp_info
	bcJmp               = 1  // INET_DIAG_BC_JMP
	bcSEq               = 11 // INET_DIAG_BC_S_EQ
	tcpiBytesAcked      = 120
	tcpiBytesReceived   = 128
	tcpiMinLen          = 136
	grpTCP4Destroy      = 1 // SKNLGRP_INET_TCP_DESTROY
	grpTCP6Destroy      = 3 // SKNLGRP_INET6_TCP_DESTROY
)

// liveStates: TCP states of a full socket that can still move bytes
// (ESTABLISHED, FIN_WAIT1, FIN_WAIT2, CLOSE, CLOSE_WAIT, LAST_ACK, CLOSING).
// SYN_RECV (request socks), TIME_WAIT (no tcp_info) and LISTEN are left out.
const liveStates = 1<<1 | 1<<4 | 1<<5 | 1<<7 | 1<<8 | 1<<9 | 1<<11

const listenStates = 1 << 10

// sample is what one socket gives: its cookie (the in-memory key) and its two
// cumulative counters. Nothing else of the socket is kept.
type sample struct {
	key    uint64 // kernel socket cookie
	up, dn uint64 // tcpi_bytes_received, tcpi_bytes_acked
}

// rec is one output line.
type rec struct {
	f      uint64
	t      int64
	up, dn uint64
}

// ---------------------------------------------------------------------------
// Flow table: interval bytes are differences of cumulative counters.

type flow struct {
	id      uint64
	up, dn  uint64 // highest cumulative counters seen
	pu, pd  uint64 // bytes moved since the last flush, not yet written
	gen     uint64 // dump in which the socket was last seen
	missing int64  // ms since start when it first went missing from a dump; -1 while present
	closed  bool   // destroyed: write what is pending, then forget
	written bool
}

type tracker struct {
	flows map[uint64]*flow // key: socket cookie
	ids   map[uint64]bool  // ids of the flows in the table, to keep them unique
	gen   uint64
	newID func() uint64
	ttl   int64 // ms a flow missing from the dumps waits for its close event

	written   uint64 // flows that got at least one line
	vanished  uint64 // flows dropped after ttl without a close event
	backwards uint64 // counter decreases seen (never turned into bytes)
}

func newTracker(newID func() uint64) *tracker {
	return &tracker{flows: map[uint64]*flow{}, ids: map[uint64]bool{}, newID: newID, ttl: 600_000}
}

func (tr *tracker) id() uint64 {
	for {
		v := tr.newID() & (1<<53 - 1) // exact in every JSON reader
		if v != 0 && !tr.ids[v] {
			tr.ids[v] = true
			return v
		}
	}
}

func (tr *tracker) has(key uint64) bool { return tr.flows[key] != nil }

// baseline records the counters of the flows alive at start: what they moved
// before start is never written.
func (tr *tracker) baseline(ss []sample) {
	for _, s := range ss {
		if tr.flows[s.key] == nil {
			tr.flows[s.key] = &flow{id: tr.id(), up: s.up, dn: s.dn, gen: tr.gen, missing: -1}
		}
	}
}

func (tr *tracker) add(f *flow, s sample) {
	if s.up > f.up {
		f.pu += s.up - f.up
		f.up = s.up
	} else if s.up < f.up {
		tr.backwards++
	}
	if s.dn > f.dn {
		f.pd += s.dn - f.dn
		f.dn = s.dn
	} else if s.dn < f.dn {
		tr.backwards++
	}
}

// observe takes one full dump at now (ms since start).
func (tr *tracker) observe(now int64, ss []sample) {
	tr.gen++
	for _, s := range ss {
		f := tr.flows[s.key]
		if f == nil { // born after the previous dump: all its bytes are new
			f = &flow{id: tr.id()}
			tr.flows[s.key] = f
		}
		if f.closed {
			continue
		}
		tr.add(f, s)
		f.gen, f.missing = tr.gen, -1
	}
	for k, f := range tr.flows {
		if f.gen == tr.gen || f.closed {
			continue
		}
		if f.missing < 0 {
			f.missing = now
		} else if now-f.missing > tr.ttl && f.pu == 0 && f.pd == 0 {
			tr.forget(k, f)
			tr.vanished++
		}
	}
}

// closed takes the final counters of a destroyed socket.
func (tr *tracker) closed(s sample) {
	f := tr.flows[s.key]
	if f == nil { // lived and died between two dumps
		f = &flow{id: tr.id()}
		tr.flows[s.key] = f
	}
	tr.add(f, s)
	f.closed = true
}

// flush writes one line per flow that moved bytes since the last flush, at t.
func (tr *tracker) flush(t int64, emit func(rec)) {
	for k, f := range tr.flows {
		if f.pu != 0 || f.pd != 0 {
			emit(rec{f: f.id, t: t, up: f.pu, dn: f.pd})
			f.pu, f.pd = 0, 0
			if !f.written {
				f.written = true
				tr.written++
			}
		}
		if f.closed {
			tr.forget(k, f)
		}
	}
}

func (tr *tracker) forget(k uint64, f *flow) {
	delete(tr.flows, k)
	delete(tr.ids, f.id)
}

func appendRec(b []byte, r rec) []byte {
	b = append(b, `{"f":`...)
	b = strconv.AppendUint(b, r.f, 10)
	b = append(b, `,"t":`...)
	b = strconv.AppendInt(b, r.t, 10)
	b = append(b, `,"up":`...)
	b = strconv.AppendUint(b, r.up, 10)
	b = append(b, `,"dn":`...)
	b = strconv.AppendUint(b, r.dn, 10)
	return append(b, "}\n"...)
}

func randID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return binary.LittleEndian.Uint64(b[:])
}

// ---------------------------------------------------------------------------
// netlink sock_diag

func align4(n int) int { return (n + 3) &^ 3 }

// bytecode builds the in-kernel filter "sport == p1 || sport == p2 || ...",
// the same shape ss(8) compiles for "( sport = :p1 or sport = :p2 )".
func bytecode(ports []uint16) []byte {
	if len(ports) == 0 {
		return nil
	}
	seg := make([]byte, 8)
	seg[0], seg[1] = bcSEq, 8 // match: on to the end (accept)
	ne.PutUint16(seg[2:], 12) // no match: to the next port, or past the end (reject)
	ne.PutUint16(seg[6:], ports[0])
	rest := bytecode(ports[1:])
	if rest == nil {
		return seg
	}
	jmp := []byte{bcJmp, 4, 0, 0} // reached only after a match: jump over the rest
	ne.PutUint16(jmp[2:], uint16(len(rest)+4))
	return append(append(seg, jmp...), rest...)
}

func buildReq(fam uint8, states uint32, seq uint32, bc []byte) []byte {
	n := nlHdrLen + diagReqLen
	if bc != nil {
		n += 4 + len(bc)
	}
	b := make([]byte, n)
	ne.PutUint32(b[0:], uint32(n))
	ne.PutUint16(b[4:], sockDiagByFamily)
	ne.PutUint16(b[6:], nlmFRequest|nlmFDump)
	ne.PutUint32(b[8:], seq)
	r := b[nlHdrLen:]
	r[0] = fam
	r[1] = syscall.IPPROTO_TCP
	r[2] = 1 << (inetDiagInfo - 1)
	ne.PutUint32(r[4:], states)
	if bc != nil {
		a := b[nlHdrLen+diagReqLen:]
		ne.PutUint16(a[0:], uint16(4+len(bc)))
		ne.PutUint16(a[2:], inetDiagReqBytecode)
		copy(a[4:], bc)
	}
	return b
}

// walkMsgs calls fn for each netlink message in b until fn returns false.
func walkMsgs(b []byte, fn func(typ uint16, seq uint32, msg []byte) bool) error {
	for len(b) >= nlHdrLen {
		l := int(ne.Uint32(b))
		if l < nlHdrLen || l > len(b) {
			return errors.New("netlink: malformed message")
		}
		if !fn(ne.Uint16(b[4:]), ne.Uint32(b[8:]), b[nlHdrLen:l]) {
			return nil
		}
		if l = align4(l); l >= len(b) {
			break
		}
		b = b[l:]
	}
	return nil
}

// parseDiag reads one inet_diag_msg: the local port (only to keep the sockets
// of a user port), the socket cookie and the two counters of its tcp_info. The
// address fields (bytes 8..39) are never read.
func parseDiag(m []byte, ports map[uint16]bool) (sample, bool) {
	if len(m) < diagMsgLen || (m[0] != syscall.AF_INET && m[0] != syscall.AF_INET6) {
		return sample{}, false
	}
	if !ports[binary.BigEndian.Uint16(m[4:6])] {
		return sample{}, false
	}
	key := uint64(ne.Uint32(m[44:48])) | uint64(ne.Uint32(m[48:52]))<<32
	for a := m[diagMsgLen:]; len(a) >= 4; {
		al := int(ne.Uint16(a[0:]))
		if al < 4 || al > len(a) {
			break
		}
		if ne.Uint16(a[2:])&nlaTypeMask == inetDiagInfo && al-4 >= tcpiMinLen {
			ti := a[4:al]
			return sample{key: key, up: ne.Uint64(ti[tcpiBytesReceived:]), dn: ne.Uint64(ti[tcpiBytesAcked:])}, true
		}
		if al = align4(al); al >= len(a) {
			break
		}
		a = a[al:]
	}
	return sample{}, false
}

func errnoOf(msg []byte) error {
	if len(msg) >= 4 {
		if e := int32(ne.Uint32(msg)); e < 0 {
			return syscall.Errno(-e)
		}
	}
	return nil
}

type diag struct {
	fd       int
	seq      uint32
	ports    map[uint16]bool
	fams     []uint8 // address families walked by a dump
	bc       []byte  // in-kernel port filter; nil once the kernel refused it
	fallback bool
	buf      []byte
	raw      int // entries the kernel returned in the last dump, before the user-space check
}

var bothFams = []uint8{syscall.AF_INET, syscall.AF_INET6}

func openDiag(ports []uint16) (*diag, error) {
	d := &diag{ports: map[uint16]bool{}, fams: bothFams, bc: bytecode(ports), buf: make([]byte, 64<<10)}
	for _, p := range ports {
		d.ports[p] = true
	}
	return d, d.open()
}

func (d *diag) open() error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return fmt.Errorf("netlink bind: %w", err)
	}
	tv := syscall.Timeval{Sec: 2}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		syscall.Close(fd)
		return fmt.Errorf("netlink timeout: %w", err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1<<20)
	d.fd = fd
	return nil
}

func (d *diag) close() { syscall.Close(d.fd) }

// listeners counts the listening sockets of the user ports and narrows the
// dumps to their address families: an accepted socket has the family of its
// listener (a dual-stack [::] listener gives AF_INET6 for IPv4 clients too),
// and every dump walks the whole established hash, so a family without a
// listener is a walk for nothing. With no listener at all both are kept.
func (d *diag) listeners() (int, error) {
	var fams []uint8
	n := 0
	for _, fam := range bothFams {
		ss, err := d.dumpFamily(fam, listenStates, nil)
		if err != nil && d.bc != nil && errors.Is(err, syscall.EINVAL) {
			d.bc, d.fallback = nil, true
			ss, err = d.dumpFamily(fam, listenStates, nil)
		}
		if err != nil {
			d.close()
			if oerr := d.open(); oerr != nil {
				return 0, fmt.Errorf("%v; reopen: %w", err, oerr)
			}
			return 0, err
		}
		if len(ss) > 0 {
			fams = append(fams, fam)
			n += len(ss)
		}
	}
	if len(fams) == 0 {
		fams = bothFams
	}
	d.fams = fams
	return n, nil
}

func famNames(fams []uint8) string {
	var s []string
	for _, f := range fams {
		if f == syscall.AF_INET {
			s = append(s, "inet")
		} else {
			s = append(s, "inet6")
		}
	}
	return strings.Join(s, "+")
}

// dump appends one sample per socket of a user port in the given states, for
// the families in d.fams. After a failed dump the socket is reopened, so a
// half-read reply never mixes with the next one.
func (d *diag) dump(states uint32, out []sample) ([]sample, error) {
	d.raw = 0
	n0 := len(out)
	for _, fam := range d.fams {
		n1 := len(out)
		var err error
		out, err = d.dumpFamily(fam, states, out)
		if err != nil && d.bc != nil && errors.Is(err, syscall.EINVAL) {
			// The kernel refused the port filter: filter in user space only.
			d.bc, d.fallback = nil, true
			out, err = d.dumpFamily(fam, states, out[:n1])
		}
		if err != nil {
			d.close()
			if oerr := d.open(); oerr != nil {
				return out[:n0], fmt.Errorf("%v; reopen: %w", err, oerr)
			}
			return out[:n0], err
		}
	}
	return out, nil
}

func (d *diag) dumpFamily(fam uint8, states uint32, out []sample) ([]sample, error) {
	d.seq++
	seq := d.seq
	if err := syscall.Sendto(d.fd, buildReq(fam, states, seq, d.bc), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return out, fmt.Errorf("netlink send: %w", err)
	}
	for {
		n, err := readRetry(d.fd, d.buf)
		if err != nil {
			return out, fmt.Errorf("netlink recv: %w", err)
		}
		done := false
		var derr error
		werr := walkMsgs(d.buf[:n], func(typ uint16, mseq uint32, msg []byte) bool {
			if mseq != seq {
				return true
			}
			switch typ {
			case nlmsgDone, nlmsgError:
				done, derr = true, errnoOf(msg)
				return false
			case sockDiagByFamily:
				d.raw++
				if s, ok := parseDiag(msg, d.ports); ok {
					out = append(out, s)
				}
			}
			return true
		})
		if werr != nil {
			return out, werr
		}
		if done {
			return out, derr
		}
	}
}

func readRetry(fd int, buf []byte) (int, error) {
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		return n, err
	}
}

// closedListener receives the final counters of destroyed TCP sockets from the
// sock_diag destroy multicast groups.
type closedEv struct {
	s  sample
	at time.Time
}

type closedListener struct {
	fd   int
	stop atomic.Bool
	done chan struct{}
	lost atomic.Uint64 // events dropped: socket buffer overrun or full channel
}

func listenClosed(ports map[uint16]bool, ch chan<- closedEv) (*closedListener, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return nil, err
	}
	groups := uint32(1<<(grpTCP4Destroy-1) | 1<<(grpTCP6Destroy-1))
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: groups}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("join destroy groups: %w", err)
	}
	if syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, 4<<20) != nil {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
	}
	tv := syscall.Timeval{Usec: 200_000}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	l := &closedListener{fd: fd, done: make(chan struct{})}
	go l.loop(ports, ch)
	return l, nil
}

func (l *closedListener) loop(ports map[uint16]bool, ch chan<- closedEv) {
	defer close(l.done)
	defer syscall.Close(l.fd)
	buf := make([]byte, 64<<10)
	for !l.stop.Load() {
		n, err := syscall.Read(l.fd, buf)
		if err != nil {
			switch err {
			case syscall.EAGAIN, syscall.EINTR: // receive timeout: look at stop
			case syscall.ENOBUFS:
				l.lost.Add(1)
			default:
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		at := time.Now()
		_ = walkMsgs(buf[:n], func(typ uint16, _ uint32, msg []byte) bool {
			if typ == sockDiagByFamily {
				if s, ok := parseDiag(msg, ports); ok {
					select {
					case ch <- closedEv{s: s, at: at}:
					default:
						l.lost.Add(1)
					}
				}
			}
			return true
		})
	}
}

func (l *closedListener) close() { l.stop.Store(true); <-l.done }

// ---------------------------------------------------------------------------
// Capture

type config struct {
	Out         string
	Dur         time.Duration // 0: until ctx ends
	Interval    time.Duration
	Ports       []uint16
	MaxSize     int64 // stop when the compressed output reaches this
	MinFree     int64 // stop when the output file system has less free space
	Closed      bool  // follow destroyed sockets (final counters)
	FlushEvery  time.Duration
	StatusEvery time.Duration
}

type summary struct {
	Reason                       string
	Ticks, Late, DumpErrs        uint64
	Lines, Up, Dn                uint64
	Flows, Vanished, Backwards   uint64
	ClosedEvents, Lost, PreStart uint64
	Listeners                    int
	Fallback                     bool
	Size                         int64
	CPU                          time.Duration
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// run captures until ctx ends, cfg.Dur passes or a limit is hit. started, if
// set, gets the instant that t=0 stands for.
func run(ctx context.Context, cfg config, logf func(string, ...any), started func(time.Time)) (sum summary, err error) {
	if len(cfg.Ports) == 0 {
		return sum, errors.New("no user port")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 100 * time.Millisecond
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 10 * time.Second
	}
	if cfg.StatusEvery <= 0 {
		cfg.StatusEvery = 5 * time.Minute
	}
	if cfg.Dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Dur)
		defer cancel()
	}
	cpu0 := cpuTime()

	f, err := os.OpenFile(cfg.Out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return sum, err
	}
	cw := &countWriter{w: f}
	gz, _ := gzip.NewWriterLevel(cw, gzip.DefaultCompression) // header: no name, no time
	bw := bufio.NewWriterSize(gz, 64<<10)
	defer func() {
		e1 := bw.Flush()
		e2 := gz.Close()
		e3 := f.Sync()
		e4 := f.Close()
		sum.Size = cw.n
		if err == nil {
			err = errors.Join(e1, e2, e3, e4)
		}
	}()

	d, err := openDiag(cfg.Ports)
	if err != nil {
		return sum, err
	}
	defer d.close()

	var events chan closedEv
	var cl *closedListener
	if cfg.Closed {
		events = make(chan closedEv, 8192)
		if cl, err = listenClosed(d.ports, events); err != nil {
			logf("flowcap: destroyed-socket events unavailable (%v): the last interval of a closing flow is lost", err)
			events, cl, err = nil, nil, nil
		} else {
			defer cl.close()
		}
	}

	if sum.Listeners, err = d.listeners(); err != nil {
		return sum, fmt.Errorf("listener check: %w", err)
	}

	tr := newTracker(randID)
	base, err := d.dump(liveStates, nil)
	if err != nil {
		return sum, fmt.Errorf("first dump: %w", err)
	}
	baseStart := time.Now() // dump done: flows from here on are new
	tr.baseline(base)
	start := baseStart
	if started != nil {
		started(start)
	}
	logf("flowcap: %d user port(s), %d listening socket(s) (%s), %d live flow(s) at start; interval %v; closed-socket events %v",
		len(cfg.Ports), sum.Listeners, famNames(d.fams), len(base), cfg.Interval, cl != nil)

	var line []byte
	var werr error
	emit := func(r rec) {
		if werr != nil {
			return
		}
		line = appendRec(line[:0], r)
		_, werr = bw.Write(line)
		sum.Lines++
		sum.Up += r.up
		sum.Dn += r.dn
	}
	onClosed := func(ev closedEv) {
		if !tr.has(ev.s.key) && ev.at.Before(baseStart) {
			sum.PreStart++ // destroyed around the first dump: its bytes are from before start
			return
		}
		sum.ClosedEvents++
		tr.closed(ev.s)
	}
	drain := func() {
		for events != nil {
			select {
			case ev := <-events:
				onClosed(ev)
			default:
				return
			}
		}
	}
	buf := make([]sample, 0, 1024)
	var dumpSum, dumpMax time.Duration
	var dumpN int64
	tick := func() {
		t0 := time.Now()
		var derr error
		buf, derr = d.dump(liveStates, buf[:0])
		dt := time.Since(t0)
		dumpSum += dt
		dumpN++
		dumpMax = max(dumpMax, dt)
		ms := t0.Sub(start).Milliseconds() // when the counters were read
		if derr != nil {
			sum.DumpErrs++
			if sum.DumpErrs <= 10 || sum.DumpErrs%1000 == 0 {
				logf("flowcap: dump error #%d: %v", sum.DumpErrs, derr)
			}
		} else {
			tr.observe(ms, buf)
		}
		drain()
		tr.flush(ms, emit)
		sum.Ticks++
	}
	status := func(tag string) {
		el := time.Since(start)
		cpu := cpuTime() - cpu0
		var avg time.Duration
		if dumpN > 0 {
			avg = dumpSum / time.Duration(dumpN)
		}
		var lost uint64
		if cl != nil {
			lost = cl.lost.Load()
		}
		logf("flowcap: %s t=%ds live=%d flows=%d lines=%d out=%dB cpu=%.2f%% dump_avg=%dus dump_max=%dus closed=%d lost=%d prestart=%d vanished=%d errs=%d late=%d fallback=%v",
			tag, int64(el.Seconds()), len(tr.flows), tr.written, sum.Lines, cw.n, 100*cpu.Seconds()/max(el.Seconds(), 1e-9),
			avg.Microseconds(), dumpMax.Microseconds(), sum.ClosedEvents, lost, sum.PreStart, tr.vanished, sum.DumpErrs, sum.Late, d.fallback)
		dumpSum, dumpMax, dumpN = 0, 0, 0
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	flushT := time.NewTicker(cfg.FlushEvery)
	defer flushT.Stop()
	statusT := time.NewTicker(cfg.StatusEvery)
	defer statusT.Stop()
	firstStatus := time.After(30 * time.Second)
	lisT := time.NewTicker(30 * time.Second) // hs2 may come back with other sockets
	defer lisT.Stop()
	dir := filepath.Dir(cfg.Out)
	last := start
loop:
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				sum.Reason = "duration"
			} else {
				sum.Reason = "stopped"
			}
			break loop
		case now := <-ticker.C:
			if now.Sub(last) > cfg.Interval*3/2 {
				sum.Late++
			}
			last = now
			tick()
			if werr != nil {
				sum.Reason = "write error"
				err = werr
				break loop
			}
		case ev := <-events:
			onClosed(ev)
		case <-flushT.C:
			if e := bw.Flush(); e != nil {
				sum.Reason, err = "write error", e
				break loop
			}
			if e := gz.Flush(); e != nil {
				sum.Reason, err = "write error", e
				break loop
			}
			if cfg.MaxSize > 0 && cw.n >= cfg.MaxSize {
				sum.Reason = "max-size"
				break loop
			}
			if cfg.MinFree > 0 {
				if free, e := freeBytes(dir); e == nil && free < cfg.MinFree {
					sum.Reason = "min-free"
					break loop
				}
			}
		case <-lisT.C:
			before := famNames(d.fams)
			if n, e := d.listeners(); e != nil {
				logf("flowcap: listener check: %v", e)
			} else if f := famNames(d.fams); f != before || n != sum.Listeners {
				logf("flowcap: listening sockets now %d (%s), were %d (%s)", n, f, sum.Listeners, before)
				sum.Listeners = n
			}
		case <-firstStatus:
			status("status")
		case <-statusT.C:
			status("status")
		}
	}
	if sum.Reason != "write error" {
		tick() // the last, partial interval
	}
	sum.Flows, sum.Vanished, sum.Backwards = tr.written, tr.vanished, tr.backwards
	sum.Fallback = d.fallback
	if cl != nil {
		sum.Lost = cl.lost.Load()
	}
	status("end(" + sum.Reason + ")")
	sum.CPU = cpuTime() - cpu0
	if werr != nil && err == nil {
		err = werr
	}
	return sum, err
}

// probeCost times n dumps the way a capture tick does them and prints counts
// and times only.
func probeCost(ports []uint16, n int) error {
	d, err := openDiag(ports)
	if err != nil {
		return err
	}
	defer d.close()
	nl, err := d.listeners()
	if err != nil {
		return err
	}
	cpu0 := cpuTime()
	var tot, mx time.Duration
	var ss []sample
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if ss, err = d.dump(liveStates, ss[:0]); err != nil {
			return err
		}
		dt := time.Since(t0)
		tot += dt
		mx = max(mx, dt)
		time.Sleep(20 * time.Millisecond)
	}
	log.Printf("flowcap probe: %d user port(s), %d listening socket(s) (%s), %d live flow(s); %d dumps: avg %dus max %dus, cpu %dus per dump; port filter in kernel %v",
		len(ports), nl, famNames(d.fams), len(ss), n, (tot / time.Duration(n)).Microseconds(), mx.Microseconds(),
		((cpuTime() - cpu0) / time.Duration(n)).Microseconds(), !d.fallback)
	return nil
}

// ---------------------------------------------------------------------------
// Ports and flags

// parsePorts reads a comma-separated port list ("8443,443").
func parsePorts(s string) ([]uint16, error) {
	seen := map[uint16]bool{}
	var out []uint16
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("bad port %q", p)
		}
		if n == 22 {
			return nil, errors.New("port 22 is not a user port")
		}
		if !seen[uint16(n)] {
			seen[uint16(n)] = true
			out = append(out, uint16(n))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no user port")
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// portsFromConfig reads only forward_ports (the edge's user ports) from an
// hs2 config; no other field is kept.
func portsFromConfig(path string) ([]uint16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	var c struct {
		ForwardPorts string `json:"forward_ports"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ps, err := parsePorts(c.ForwardPorts)
	if err != nil {
		return nil, fmt.Errorf("%s forward_ports: %w", path, err)
	}
	return ps, nil
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mul := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mul, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		mul, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		mul, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mul, nil
}

func main() {
	out := flag.String("out", "", "output file (.jsonl.gz); must not exist")
	dur := flag.Duration("dur", 24*time.Hour, "capture length")
	interval := flag.Duration("interval", 100*time.Millisecond, "sampling interval")
	ports := flag.String("ports", "", "user ports, comma-separated (default: forward_ports of -config)")
	conf := flag.String("config", "/etc/hs2/config.json", "hs2 config to read forward_ports from")
	maxSize := flag.String("max-size", "1500M", "stop when the output reaches this size")
	minFree := flag.String("min-free", "1G", "stop when the output file system has less free space")
	closed := flag.Bool("closed-events", true, "follow destroyed sockets for their final counters")
	flushEvery := flag.Duration("flush", 10*time.Second, "flush the output this often")
	statusEvery := flag.Duration("status", 5*time.Minute, "status line this often")
	probe := flag.Int("probe", 0, "only time this many dumps, print the cost and exit (no output file)")
	flag.Parse()
	log.SetFlags(0)

	var ps []uint16
	var err error
	if *ports != "" {
		ps, err = parsePorts(*ports)
	} else {
		ps, err = portsFromConfig(*conf)
	}
	if err != nil {
		log.Fatalf("flowcap: %v", err)
	}
	if *probe > 0 {
		if err := probeCost(ps, *probe); err != nil {
			log.Fatalf("flowcap: %v", err)
		}
		return
	}
	if *out == "" {
		log.Fatal("flowcap: -out is required")
	}
	ms, err := parseSize(*maxSize)
	if err != nil {
		log.Fatalf("flowcap: -max-size: %v", err)
	}
	mf, err := parseSize(*minFree)
	if err != nil {
		log.Fatalf("flowcap: -min-free: %v", err)
	}
	if free, e := freeBytes(filepath.Dir(*out)); e == nil && mf > 0 && free < mf {
		log.Fatalf("flowcap: only %d bytes free next to %s (-min-free %d)", free, *out, mf)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sum, err := run(ctx, config{
		Out: *out, Dur: *dur, Interval: *interval, Ports: ps, MaxSize: ms, MinFree: mf,
		Closed: *closed, FlushEvery: *flushEvery, StatusEvery: *statusEvery,
	}, log.Printf, nil)
	log.Printf("flowcap: done (%s): %d lines, %d flows, up %d B, dn %d B, %d B written, cpu %v",
		sum.Reason, sum.Lines, sum.Flows, sum.Up, sum.Dn, sum.Size, sum.CPU.Round(time.Millisecond))
	if err != nil {
		log.Fatalf("flowcap: %v", err)
	}
}
