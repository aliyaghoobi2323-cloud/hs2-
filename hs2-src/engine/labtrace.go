package engine

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/xtaci/smux"
	"golang.org/x/sys/unix"
)

// Lab only (branch r1-diag, docs/architecture/wip/r1/round2-diagnosis.fa.md;
// not a candidate). Every switch is off unless its variable is set:
//
//	HS2_LAB_TRACE=DIR     trace lines (smux.LabTrace's, the shaper's W lines,
//	                      the TUN side channel's T lines) to DIR/trace-PID.txt;
//	                      with HS2_LAB_TRACE_NOFRAMES=1 the S lines only
//	HS2_LAB_PACE_MBIT=X   every link socket, both ends, paced at X Mbit/s
//	                      (SO_MAX_PACING_RATE, fixed: C4-lite's and the
//	                      pacing net's writes take X instead)
//	HS2_LAB_WCLAMP=B      SO_RCVBUF 4B (locked) and TCP_WINDOW_CLAMP of B bytes
//	                      on every link socket: each end advertises at most
//	                      B, the peer's bytes in flight
//	HS2_LAB_PIN_PORT=P    user connections on port P open on the first link
//	                      only, the others never on it (a probe on a TCP
//	                      connection of its own)
//	HS2_LAB_NEWBYTES=B    a flow not judged yet goes light for its first B
//	                      bytes (classNewBytes, 32 KiB; 0: as bulk)
//
//	W shaper sid len key t_write t_record off lport rport srtt_us minrtt_us
//	  rttvar_us cwnd unacked notsent outq total_retrans lost ca_state
//	  delivery_Bps pacing_Bps retrans_now sacked
//
// off is the TCP stream offset of the end of the TLS record that carries the
// frame (bytes_acked + SIOCOUTQ, read so that no ACK moved between them).
var (
	labTraceOn bool
	labPaceBps uint64
	labWclamp  int
	labPinPort int
	labNewB    = uint64(classNewBytes)
)

func init() {
	if v, err := strconv.ParseFloat(os.Getenv("HS2_LAB_PACE_MBIT"), 64); err == nil && v > 0 {
		labPaceBps = uint64(v * 1e6 / 8)
	}
	if v, err := strconv.Atoi(os.Getenv("HS2_LAB_WCLAMP")); err == nil && v > 0 {
		labWclamp = v
	}
	if v, err := strconv.Atoi(os.Getenv("HS2_LAB_PIN_PORT")); err == nil && v > 0 {
		labPinPort = v
	}
	if v, err := strconv.Atoi(os.Getenv("HS2_LAB_NEWBYTES")); err == nil && v >= 0 {
		labNewB = uint64(v)
	}
	d := os.Getenv("HS2_LAB_TRACE")
	if d == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(d, fmt.Sprintf("trace-%d.txt", os.Getpid())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	var mu sync.Mutex
	smux.LabTrace = func(s string) {
		mu.Lock()
		f.WriteString(s)
		mu.Unlock()
	}
	labTraceOn = true
	smux.LabNoFrames = os.Getenv("HS2_LAB_TRACE_NOFRAMES") == "1"
	smux.LabTrace(fmt.Sprintf("H %d %d pace=%d wclamp=%d pin=%d newbytes=%d args=%q\n", os.Getpid(), time.Now().UnixNano(), labPaceBps, labWclamp, labPinPort, labNewB, os.Args))
}

// labSock applies the socket switches to a new link socket.
func labSock(tc *net.TCPConn) {
	if tc == nil {
		return
	}
	if labPaceBps > 0 {
		setMaxPacing(tc, labPaceBps)
	}
	if labWclamp > 0 {
		if rc, err := tc.SyscallConn(); err == nil {
			rc.Control(func(fd uintptr) {
				// lock the receive buffer first: autotuning raises an
				// unlocked socket's clamp with its buffer (the first
				// try, without it, left the peer a 321 KB window under a
				// 52 KB clamp)
				unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4*labWclamp)
				unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_WINDOW_CLAMP, labWclamp)
			})
		}
	}
}

type labPend struct {
	end    uint64 // the shaper's input offset past the frame
	sid, n uint32
	key    string
	t      int64
}

// labNote: p, one smux frame, goes into the shaper (under wmu).
func (c *shapedConn) labNote(p []byte) {
	if smux.LabSmallFrame(p) {
		n := int(p[2]) | int(p[3])<<8
		sid := uint32(p[4]) | uint32(p[5])<<8 | uint32(p[6])<<16 | uint32(p[7])<<24
		c.labPend = append(c.labPend, labPend{end: c.labIn + uint64(len(p)), sid: sid, n: uint32(n), key: smux.LabKey(p[8 : 8+n]), t: time.Now().UnixNano()})
	}
	c.labIn += uint64(len(p))
}

// labRecord: a record of n data bytes went to the conn (under wmu).
func (c *shapedConn) labRecord(n int) {
	c.labOut += uint64(n)
	for len(c.labPend) > 0 && c.labPend[0].end <= c.labOut {
		p := c.labPend[0]
		c.labPend = c.labPend[1:]
		smux.LabTrace(fmt.Sprintf("W %p %d %d %s %d %d %s\n", c, p.sid, p.n, p.key, p.t, time.Now().UnixNano(), labSockLine(c.labTCP)))
	}
}

// labSockLine: the socket's offset and TCP_INFO for a W line.
func labSockLine(tc *net.TCPConn) string {
	if tc == nil {
		return "-1 0 0"
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return "-1 0 0"
	}
	var out string
	rc.Control(func(fd uintptr) {
		var ti *unix.TCPInfo
		var off uint64
		for range 8 {
			a, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			if err != nil {
				return
			}
			q, err := unix.IoctlGetInt(int(fd), unix.SIOCOUTQ)
			if err != nil {
				return
			}
			b, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			if err != nil {
				return
			}
			ti, off = b, b.Bytes_acked+uint64(q)
			if a.Bytes_acked == b.Bytes_acked {
				break
			}
			off = 1<<63 | off // moved each time: flagged
		}
		if ti == nil {
			return
		}
		la, _ := tc.LocalAddr().(*net.TCPAddr)
		ra, _ := tc.RemoteAddr().(*net.TCPAddr)
		lp, rp := 0, 0
		if la != nil {
			lp = la.Port
		}
		if ra != nil {
			rp = ra.Port
		}
		q := off &^ (1 << 63)
		out = fmt.Sprintf("%d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d",
			int64(off), lp, rp, ti.Rtt, ti.Min_rtt, ti.Rttvar, ti.Snd_cwnd, ti.Unacked, ti.Notsent_bytes,
			q-ti.Bytes_acked, ti.Total_retrans, ti.Lost, ti.Ca_state, ti.Delivery_rate, ti.Pacing_rate,
			ti.Retrans, ti.Sacked, ti.Snd_mss, ti.Backoff)
	})
	if out == "" {
		return "-1 0 0"
	}
	return out
}

// labPinAvoid (HS2_LAB_PIN_PORT): the links a connection on port may not
// open on: every link but the first for port P, the first for any other.
func (m *LinkManager) labPinAvoid(port int) []Link {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.links) < 2 {
		return nil
	}
	if port == labPinPort {
		out := make([]Link, 0, len(m.links)-1)
		for _, c := range m.links[1:] {
			out = append(out, c.link)
		}
		return out
	}
	return []Link{m.links[0].link}
}

// labTun: a TUN packet b (an echo, a ping) goes into link l's queue.
func labTun(l *l3Link, b []byte) {
	if labTraceOn && !smux.LabNoFrames {
		smux.LabTrace(fmt.Sprintf("T %p %d %s %d\n", l, len(b), smux.LabKey(b), time.Now().UnixNano()))
	}
}
