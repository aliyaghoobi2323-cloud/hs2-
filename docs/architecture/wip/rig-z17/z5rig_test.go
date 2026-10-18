package engine

// Gap 5 rig, part 2 (scratch, not part of the product). One real hs2 link --
// tlscarrier dial and server (uTLS, auth, the production socket tuning),
// newEdgeLink on the edge, newSession with a meter on the exit as
// stream_kharej.go builds it -- across the TUN path emulator (z5emu_test.go:
// kernel TCP end to end, per-direction rate, drop-tail queue, one-way delay,
// random loss). Run it inside its own network namespace with the production
// "high" profile TCP buffers (tcp_rmem/tcp_wmem max 32 MiB), see run.sh.
//
// The smux buffers come from the environment, per end, so one binary covers
// every option: Z5_EDGE / Z5_EXIT = "streamMiB,sessMiB" (default: what the
// tree has). The exit's bulk sources are a real loopback "panel" relayed with
// io.CopyBuffer and 32 KiB buffers as the exit relays a panel (frame sizes as
// in production); Z5_PANEL=0 writes 32 KiB straight into the stream.
//
// Streams the edge opens (each reader reads the stream directly, or through
// relayStream and a net.Pipe "app" under the link's real guard if Z5_RELAY=1):
//
//	F (Z5_FAST n):     bulk download, reader as fast as it can
//	S (Z5_SLOW n):     bulk download, reader paced at Z5_SLOW_MBIT
//	Z (Z5_FROZEN n):   bulk download, reader takes Z5_FREEZE_KB then stops for good
//	T (Z5_FTS n):      bulk download, reader fast for Z5_FTS_S s, then Z5_FTS_KBS KiB/s
//	U (Z5_UPL n):      bulk upload, the exit's panel reads as fast as it can
//	I (Z5_INT 1):      interactive, 200 B request / 2048 B reply every 100 ms
//
// F and U open at Z5_F_AT s, the others at Z5_BG_AT s (from when the link is
// up). After Z5_WARM_S it measures Z5_DUR_S s and appends one JSON line to
// Z5_OUT. Path: Z5_DN_MBIT / Z5_UP_MBIT, Z5_RTT_MS, Z5_QMS (bottleneck
// queue, ms at its rate), Z5_LOSS (share of packets, both ways), Z5_MTU.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
	"golang.org/x/sys/unix"
)

func z5Env(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return def
}

// z5Bufs parses "streamMiB,sessMiB" (fractions allowed).
func z5Bufs(k string) (stream, sess int, ok bool) {
	v := os.Getenv(k)
	if v == "" {
		return 0, 0, false
	}
	p := strings.Split(v, ",")
	a, _ := strconv.ParseFloat(p[0], 64)
	b, _ := strconv.ParseFloat(p[1], 64)
	return int(a * (1 << 20)), int(b * (1 << 20)), true
}

// z5CfgMu serializes the per-end override of the package's smux variables
// around each newSession.
var z5CfgMu sync.Mutex

func z5WithBufs(k string, f func()) {
	z5CfgMu.Lock()
	defer z5CfgMu.Unlock()
	st, se := SmuxStreamBuffer, SmuxSessionBuffer
	if a, b, ok := z5Bufs(k); ok {
		SmuxStreamBuffer, SmuxSessionBuffer = a, b
	}
	f()
	SmuxStreamBuffer, SmuxSessionBuffer = st, se
}

type z5Lat struct {
	mu   sync.Mutex
	v    []float64 // ms
	lost int
}

func (l *z5Lat) add(d time.Duration) {
	l.mu.Lock()
	l.v = append(l.v, float64(d)/1e6)
	l.mu.Unlock()
}

func z5Pct(v []float64) map[string]float64 {
	v = append([]float64(nil), v...)
	sort.Float64s(v)
	q := func(p float64) float64 {
		if len(v) == 0 {
			return -1
		}
		i := int(math.Ceil(p*float64(len(v)))) - 1
		if i < 0 {
			i = 0
		}
		return math.Round(v[i]*10) / 10
	}
	return map[string]float64{"n": float64(len(v)), "p50": q(0.5), "p90": q(0.9), "p99": q(0.99), "max": q(1)}
}

// z5Bins counts bytes per 100 ms from t0.
type z5Bins struct {
	b [1200]atomic.Int64
}

func (z *z5Bins) add(t0 time.Time, n int) {
	if n <= 0 {
		return
	}
	if i := int(time.Since(t0) / (100 * time.Millisecond)); i >= 0 && i < len(z.b) {
		z.b[i].Add(int64(n))
	}
}

// mbit over [from, to) seconds
func (z *z5Bins) mbit(from, to float64) float64 {
	var s int64
	for i := int(from * 10); i < int(to*10) && i < len(z.b); i++ {
		s += z.b[i].Load()
	}
	return float64(s) * 8 / (to - from) / 1e6
}

// per-second Mbit/s from 0 to n s
func (z *z5Bins) secs(n int) []float64 {
	var out []float64
	for s := 0; s < n; s++ {
		out = append(out, math.Round(z.mbit(float64(s), float64(s+1))*10)/10)
	}
	return out
}

type z5Res struct {
	Scen     string             `json:"scen"`
	Ver      string             `json:"ver"`
	Rep      string             `json:"rep"`
	Env      map[string]string  `json:"env"`
	Fast     []float64          `json:"fastMbit"`  // each F over the window
	FastSum  float64            `json:"fastSum"`   // all F together over the window
	Fast1s   []float64          `json:"fast1s"`    // all F together, per second from t0
	FMarks   []float64          `json:"f0MarksMs"` // F0: ms from its open to 1, 8, 32, 64 MiB
	Up       []float64          `json:"upMbit"`    // each U over the window
	UpSum    float64            `json:"upSum"`
	Up1s     []float64          `json:"up1s"`
	Slow     []float64          `json:"slowMbit"` // each S over the window
	FTS      float64            `json:"ftsSum"`   // all T over the window
	Int      map[string]float64 `json:"int"`
	Parks    [2]uint32          `json:"parks"`       // edge, exit (whole run)
	Brakes   [2]uint32          `json:"brakes"`      // edge, exit (whole run)
	HeldMax  [2]float64         `json:"heldMaxMiB"`  // most unread payload (MaxReceiveBuffer + extra - bucket), edge, exit
	ExtraMax [2]float64         `json:"extraMaxMiB"` // need: most the bucket grew past MaxReceiveBuffer (claims + spare), edge, exit
	Borrows  [2]uint32          `json:"borrows"`     // need: bucket growths from the pool as it ran full
	Pauses   [2]uint32          `json:"pauses"`      // delay: windows taken down as their data paused
	CapMax   [2]float64         `json:"capMaxMiB"`   // most bytes in smux receive blocks (cap of every buffered frame)
	HeapMax  float64            `json:"heapInuseMaxMiB"`
	Heap0    float64            `json:"heapInuse0MiB"` // at t0
	CPU      float64            `json:"cpuS"`
	EmuCPU   float64            `json:"emuCpuS"` // of cpuS, the emulator's threads (with the kernel's work done there)
	Retrans  [2]uint32          `json:"retrans"`
	SegsOut  [2]uint32          `json:"segsOut"` // data segments sent, resends included (TCP_INFO), over the window
	Srtt     [2]float64         `json:"srttMs"`
	Drops    [2]int64           `json:"drops"`
	PolDrops int64              `json:"polDrops,omitempty"` // z17: the policer's (download)
	MaxPace  float64            `json:"maxPaceMbit,omitempty"` // z17: the exit's SO_MAX_PACING_RATE at the end (Mbit/s)
	NetHeld  int32              `json:"netHeld"`            // z17: times the exit's pacing net held its pacing
	NetShare float64            `json:"netShare,omitempty"` // ... the largest resend share it saw, %
	WireDn   float64            `json:"wireDnMbit"`
	WireUp   float64            `json:"wireUpMbit"`
	Ended    int                `json:"relaysEnded"` // Z5_RELAY: relays the guard (or anything) ended
	Errs     []string           `json:"errs,omitempty"`
	// red: per-second cumulative exit Total_retrans, edge Data_segs_in,
	// exit Data_segs_out, edge/exit extra (MiB), from t0 (Z5_TRACE)
	TRt  []uint32  `json:"trRt,omitempty"`
	TSi  []uint32  `json:"trSi,omitempty"`
	TSo  []uint32  `json:"trSo,omitempty"`
	TEx  []float64 `json:"trExtra,omitempty"`
	TBor []uint32  `json:"trBorrows,omitempty"`
	TPk  []uint32  `json:"trParks,omitempty"`
	TRtE []uint32  `json:"trRtE,omitempty"` // edge Total_retrans (upload resends)
	TSoE []uint32  `json:"trSoE,omitempty"` // edge Data_segs_out
	// delay rig (Z5_TRACE2): every 100 ms from t0
	T2 map[string][]float64 `json:"t2,omitempty"`
	// window core rig (z11): its log lines (core trees), and every window
	// turnaround of F0 (edge) and U0 (exit) as the smux arm took it:
	// [ms from t0, turnaround ms, ...] (Z5_TURNS)
	WcLines []string `json:"wcLines,omitempty"`
	T0      string   `json:"t0,omitempty"` // wall clock of t0 (to align the window core's trace)
	// the netns's TcpExt and Tcp counters over the measure window (both ends: one namespace)
	Netstat map[string]int64     `json:"netstat,omitempty"`
	Turns   map[string][]float64 `json:"turns,omitempty"`
}

// z5Netstat: the namespace's TCP counters (/proc/self/net/netstat TcpExt, /proc/self/net/snmp Tcp).
func z5Netstat() map[string]int64 {
	out := map[string]int64{}
	for _, f := range []string{"/proc/self/net/netstat", "/proc/self/net/snmp"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for i := 0; i+1 < len(lines); i += 2 {
			h, v := strings.Fields(lines[i]), strings.Fields(lines[i+1])
			if len(h) != len(v) || len(h) == 0 || (h[0] != "TcpExt:" && h[0] != "Tcp:") {
				continue
			}
			for k := 1; k < len(h); k++ {
				n, _ := strconv.ParseInt(v[k], 10, 64)
				out[strings.TrimSuffix(h[0], ":")+"."+h[k]] = n
			}
		}
	}
	return out
}

// z5SetWcLog: where the window core logs (core trees: setWcoreLog).
var z5SetWcLog = func(func(string, ...any)) {}

// z5TurnPtr: a stream's latest window turnaround (smux core.turnSeen, ns),
// for a racy peek by the rig; nil on a tree without it.
func z5TurnPtr(st *smux.Stream) *int64 {
	if st == nil {
		return nil
	}
	v := reflect.ValueOf(st).Elem().FieldByName("core")
	if !v.IsValid() {
		return nil
	}
	f := v.FieldByName("turnSeen")
	if !f.IsValid() {
		return nil
	}
	return (*int64)(unsafe.Pointer(f.UnsafeAddr()))
}

// z5QueueOf: the session's QueueDelay in ms (delay trees; -1 without)
var z5QueueOf = func(*smux.Session) float64 { return -1 }

// z5Win: a stream's receive window (rcvWin) in MiB, and the peer's window
// as this side last heard it (peerWindow), racy peek for the rig.
func z5Win(st *smux.Stream) (rcv, peer float64) {
	if st == nil {
		return 0, 0
	}
	v := reflect.ValueOf(st).Elem()
	if f := v.FieldByName("rcvWin"); f.IsValid() {
		rcv = float64(atomic.LoadUint32((*uint32)(unsafe.Pointer(f.UnsafeAddr())))) / (1 << 20)
	}
	if f := v.FieldByName("peerWindow"); f.IsValid() {
		peer = float64(atomic.LoadUint32((*uint32)(unsafe.Pointer(f.UnsafeAddr())))) / (1 << 20)
	}
	return
}

func z5TCPInfo(c net.Conn) *unix.TCPInfo {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return nil
	}
	var info *unix.TCPInfo
	rc.Control(func(fd uintptr) { info, _ = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) })
	return info
}

func z5CPU() float64 {
	var ru unix.Rusage
	unix.Getrusage(unix.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

func z5Field(v reflect.Value, name string) reflect.Value {
	f := v.FieldByName(name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}

// z5Held: the session's unread payload (MaxReceiveBuffer - bucket) and the
// bytes its buffered frames take in their power-of-two blocks. Racy peeks at
// the session under its own stream lock, for the rig only.
func z5Held(s *smux.Session, sessBuf int) (unread int, caps int) {
	v := reflect.ValueOf(s).Elem()
	unread = sessBuf + z5Extra(s) - int(atomic.LoadInt32((*int32)(unsafe.Pointer(v.FieldByName("bucket").UnsafeAddr()))))
	mu := z5Field(v, "streamLock").Addr().Interface().(*sync.Mutex)
	mu.Lock()
	m := z5Field(v, "streams")
	for _, k := range m.MapKeys() {
		st := m.MapIndex(k).Elem()
		bl := z5Field(st, "bufferLock").Addr().Interface().(*sync.Mutex)
		bl.Lock()
		heads := z5Field(st, "heads")
		for i := 0; i < heads.Len(); i++ {
			caps += heads.Index(i).Cap()
		}
		bl.Unlock()
	}
	mu.Unlock()
	return
}

// z5Extra: the need prototype's growth of the bucket past MaxReceiveBuffer
// (Session.extra), 0 on a tree without it.
func z5Extra(s *smux.Session) int {
	f := reflect.ValueOf(s).Elem().FieldByName("extra")
	if !f.IsValid() {
		return 0
	}
	return int(atomic.LoadInt32((*int32)(unsafe.Pointer(f.UnsafeAddr()))))
}

// z5Count: a Session counter method where the tree has it, 0 where not.
func z5Count(s *smux.Session, name string) uint32 {
	m := reflect.ValueOf(s).MethodByName(name)
	if !m.IsValid() {
		return 0
	}
	return uint32(m.Call(nil)[0].Uint())
}

// z5Borrows: Session.Borrows() where the tree has it.
func z5Borrows(s *smux.Session) uint32 {
	m := reflect.ValueOf(s).MethodByName("Borrows")
	if !m.IsValid() {
		return 0
	}
	return uint32(m.Call(nil)[0].Uint())
}

func z5LoUp() {
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer unix.Close(s)
	r, err := unix.NewIfreq("lo")
	if err != nil {
		return
	}
	r.SetUint16(unix.IFF_UP | unix.IFF_RUNNING)
	unix.IoctlIfreq(s, unix.SIOCSIFFLAGS, r)
}

func TestZ5Rig(t *testing.T) {
	scen := os.Getenv("Z5_SCEN")
	if scen == "" {
		t.Skip("Z5_SCEN not set")
	}
	z5LoUp()
	mtu := int(z5Env("Z5_MTU", 1500))
	rtt := time.Duration(z5Env("Z5_RTT_MS", 100) * float64(time.Millisecond))
	qms := z5Env("Z5_QMS", 100)
	loss := z5Env("Z5_LOSS", 0)
	path := func(mbit float64) z5Path {
		p := z5Path{Rate: mbit * 1e6 / 8, Delay: rtt / 2, Loss: loss}
		p.Queue = int(p.Rate * qms / 1000)
		if p.Queue < 3*mtu {
			p.Queue = 3 * mtu
		}
		return p
	}
	emu, err := newZ5Emu(path(z5Env("Z5_UP_MBIT", 100)), path(z5Env("Z5_DN_MBIT", 100)), mtu)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := z5Env("Z5_RELAY", 0) > 0
	usePanel := z5Env("Z5_PANEL", 1) > 0

	// the "panel": bulk source ('D') or sink ('U') on loopback
	var upBins []z5Bins
	var t0 time.Time
	var t0Set atomic.Bool
	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pln.Close()
	go func() {
		for {
			c, err := pln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var hd [2]byte
				if _, err := io.ReadFull(c, hd[:]); err != nil {
					return
				}
				buf := make([]byte, 32<<10)
				if hd[0] == 'U' {
					for {
						n, err := c.Read(buf)
						if t0Set.Load() {
							upBins[hd[1]].add(t0, n)
						}
						if err != nil {
							return
						}
					}
				}
				// Z5_PANEL_WR: bytes per panel write (default 32 KiB), with
				// Z5_PANEL_GAP_US between writes: a panel whose replies trickle
				// in pieces gives the exit's relay reads (smux frames) of that size
				wr := int(z5Env("Z5_PANEL_WR", 32<<10))
				gap := time.Duration(z5Env("Z5_PANEL_GAP_US", 0)) * time.Microsecond
				if wr > len(buf) {
					buf = make([]byte, wr)
				}
				for {
					if _, err := c.Write(buf[:wr]); err != nil {
						return
					}
					if gap > 0 {
						time.Sleep(gap)
					}
				}
			}(c)
		}
	}()
	panel := func(kind, idx byte) net.Conn {
		c, err := net.Dial("tcp", pln.Addr().String())
		if err != nil {
			return nil
		}
		c.Write([]byte{kind, idx})
		return c
	}

	var wcMu sync.Mutex
	var wcLines []string
	tLog := time.Now()
	z5SetWcLog(func(f string, a ...any) {
		wcMu.Lock()
		if len(wcLines) < 400 {
			wcLines = append(wcLines, fmt.Sprintf("%6.1fs %s", time.Since(tLog).Seconds(), fmt.Sprintf(f, a...)))
		}
		wcMu.Unlock()
	})
	key := bytes.Repeat([]byte{0x5a}, 32)
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	ln, err := net.Listen("tcp", net.IP(z5Local[:]).String()+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	exitCh := make(chan *smux.Session, 1)
	exitMtr := &linkMeter{} // as stream_kharej.go: reported over kindStats
	exitRaw := make(chan net.Conn, 1)
	var exitCar atomic.Pointer[tlscarrier.Carrier]
	var fSt, uSt atomic.Pointer[smux.Stream] // F0 on the edge, U0 on the exit (Z5_TRACE2)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		exitRaw <- raw
		srv.Handle(ctx, raw, func(car *tlscarrier.Carrier) {
			exitCar.Store(car)
			var sess *smux.Session
			var err error
			z5WithBufs("Z5_EXIT", func() {
				sess, _, err = newSession(car.RawConn(), true, nil, exitMtr) // as stream_kharej.go
			})
			if err != nil {
				return
			}
			exitCh <- sess
			<-sess.CloseChan()
		})
	}()
	car, err := tlscarrier.DialFrom(net.IP(z5ExitIP[:]).String()+":"+strconv.Itoa(port), "lab.example.com", key, "")
	if err != nil {
		t.Fatal(err)
	}
	var link *mtcpLink
	z5WithBufs("Z5_EDGE", func() {
		link, err = newEdgeLink(car, obfs.NewHTTPSLengthSampler())
	})
	if err != nil {
		t.Fatal(err)
	}
	edge := link.sess
	var exit *smux.Session
	select {
	case exit = <-exitCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no exit session")
	}
	defer edge.Close()
	defer exit.Close()
	socks := [2]net.Conn{car.TCPConn(), <-exitRaw}
	if z5Env("Z5_CTRL", 1) > 0 { // as every edge link has (openControl)
		go openControl(ctx, link, func(string, ...any) {})
	}
	if z5Env("Z5_STATS", 1) > 0 { // the exit's stats record, polled each health tick (2 s) as LinkManager does
		go openStats(ctx, link, func(string, ...any) {})
		go func() {
			tk := time.NewTicker(2 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
					pollStats(link.mtr)
				}
			}
		}()
	}
	edgeSess, exitSess := SmuxSessionBuffer, SmuxSessionBuffer
	if _, b, ok := z5Bufs("Z5_EDGE"); ok {
		edgeSess = b
	}
	if _, b, ok := z5Bufs("Z5_EXIT"); ok {
		exitSess = b
	}

	nF, nS, nZ, nT, nU := int(z5Env("Z5_FAST", 0)), int(z5Env("Z5_SLOW", 0)), int(z5Env("Z5_FROZEN", 0)), int(z5Env("Z5_FTS", 0)), int(z5Env("Z5_UPL", 0))
	upBins = make([]z5Bins, nU)
	fBins := make([]z5Bins, nF)
	sBins := make([]z5Bins, nS)
	var tBins z5Bins
	t0 = time.Now()
	t0Set.Store(true)
	var res z5Res
	var errMu sync.Mutex
	fail := func(f string, a ...any) {
		errMu.Lock()
		if len(res.Errs) < 20 {
			res.Errs = append(res.Errs, fmt.Sprintf(f, a...))
		}
		errMu.Unlock()
	}

	// exit side
	go func() {
		for {
			st, err := exit.AcceptStream()
			if err != nil {
				return
			}
			go func(st *smux.Stream) {
				defer st.Close()
				var hd [2]byte
				if _, err := io.ReadFull(st, hd[:1]); err != nil {
					return
				}
				if hd[0] == kindCtrl { // the edge's control channel (Z5_CTRL)
					serveControl(ctx, st, exitCar.Load())
					return
				}
				if hd[0] == kindStats { // the exit's stats record (Z5_STATS)
					serveStats(ctx, st, exitCar.Load(), exitMtr)
					return
				}
				if _, err := io.ReadFull(st, hd[1:]); err != nil {
					return
				}
				if hd[0] == 'U' && hd[1] == 0 {
					uSt.Store(st)
				}
				switch hd[0] {
				case 'D':
					if usePanel {
						if pc := panel('D', hd[1]); pc != nil {
							defer pc.Close()
							bp := copyBufs.Get().(*[]byte)
							io.CopyBuffer(struct{ io.Writer }{st}, struct{ io.Reader }{pc}, *bp)
							copyBufs.Put(bp)
						}
						return
					}
					buf := make([]byte, 32<<10)
					for {
						if _, err := st.Write(buf); err != nil {
							return
						}
					}
				case 'U':
					if usePanel {
						if pc := panel('U', hd[1]); pc != nil {
							defer pc.Close()
							bp := copyBufs.Get().(*[]byte)
							io.CopyBuffer(struct{ io.Writer }{pc}, struct{ io.Reader }{st}, *bp)
							copyBufs.Put(bp)
						}
						return
					}
					buf := make([]byte, 32<<10)
					for {
						n, err := st.Read(buf)
						upBins[hd[1]].add(t0, n)
						if err != nil {
							return
						}
					}
				case 'B':
					// a burst of Z5_IDB_MB MiB, Z5_IDB_GAP_S of nothing, then
					// everything it can (Z5_IDB: a fast transfer, a pause, the next)
					buf := make([]byte, 32<<10)
					for n := 0; n < int(z5Env("Z5_IDB_MB", 96))<<20; n += len(buf) {
						if _, err := st.Write(buf); err != nil {
							return
						}
					}
					time.Sleep(time.Duration(z5Env("Z5_IDB_GAP_S", 7) * float64(time.Second)))
					for {
						if _, err := st.Write(buf); err != nil {
							return
						}
					}
				case 'I':
					req := make([]byte, 200)
					rep := make([]byte, 2048)
					for {
						if _, err := io.ReadFull(st, req); err != nil {
							return
						}
						if _, err := st.Write(rep); err != nil {
							return
						}
					}
				}
			}(st)
		}
	}()

	var ended atomic.Int64
	// open returns what the edge's reader reads: the stream itself, or a
	// net.Pipe "app" end fed by relayStream under the link's guard.
	open := func(role byte, idx int) io.ReadWriteCloser {
		st, err := edge.OpenStream()
		if err != nil {
			fail("open %c%d: %v", role, idx, err)
			return nil
		}
		if _, err := st.Write([]byte{role, byte(idx)}); err != nil {
			fail("hdr %c%d: %v", role, idx, err)
			return nil
		}
		if role == 'D' && idx == 0 {
			fSt.Store(st)
		}
		if !relay || role == 'I' {
			return st
		}
		local, app := net.Pipe()
		go func() { relayStream(local, st, link.mtr.guard); ended.Add(1) }()
		return app
	}
	readFast := func(r io.Reader, b *z5Bins, mark func(int64)) {
		buf := make([]byte, 32<<10)
		var tot int64
		for {
			n, err := r.Read(buf)
			b.add(t0, n)
			tot += int64(n)
			if mark != nil {
				mark(tot)
			}
			if err != nil {
				return
			}
		}
	}
	// paced: rate bytes/s, no credit beyond 64 KiB (as the gap 2 simulator)
	readPaced := func(r io.Reader, rate float64, b *z5Bins) {
		buf := make([]byte, 32<<10)
		start := time.Now()
		var got int64
		for {
			allowed := int64(rate*time.Since(start).Seconds()) - got
			if allowed > 64<<10 {
				got += allowed - 64<<10
				allowed = 64 << 10
			}
			if allowed < 4096 {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if allowed > int64(len(buf)) {
				allowed = int64(len(buf))
			}
			n, err := r.Read(buf[:allowed])
			got += int64(n)
			if b != nil {
				b.add(t0, n)
			}
			if err != nil {
				return
			}
		}
	}
	marks := []int64{1 << 20, 8 << 20, 32 << 20, 64 << 20}
	markAt := make([]atomic.Int64, len(marks))
	at := func(s float64, f func()) {
		go func() {
			time.Sleep(time.Until(t0.Add(time.Duration(s * float64(time.Second)))))
			f()
		}()
	}
	fAt, bgAt := z5Env("Z5_F_AT", 0), z5Env("Z5_BG_AT", 0)
	// red: Z5_STOP n streams open at Z5_STOP_AT s, read as fast as they can
	// for Z5_STOP_S s, then never read again (an app paused after a fast
	// start: a video buffered, a download paused)
	at(z5Env("Z5_STOP_AT", 0), func() {
		for i := 0; i < int(z5Env("Z5_STOP", 0)); i++ {
			if r := open('D', 60+i); r != nil {
				go func() {
					until := time.Now().Add(time.Duration(z5Env("Z5_STOP_S", 6) * float64(time.Second)))
					buf := make([]byte, 32<<10)
					for time.Now().Before(until) {
						if _, err := r.Read(buf); err != nil {
							return
						}
					}
				}()
			}
		}
	})
	at(fAt, func() {
		for i := 0; i < nF; i++ {
			r := open('D', i)
			if r == nil {
				continue
			}
			var mark func(int64)
			if i == 0 {
				opened := time.Now()
				mark = func(tot int64) {
					for k, m := range marks {
						if tot >= m && markAt[k].Load() == 0 {
							markAt[k].Store(int64(time.Since(opened)))
						}
					}
				}
			}
			go readFast(r, &fBins[i], mark)
		}
		for i := 0; i < nU; i++ {
			st, err := edge.OpenStream()
			if err != nil {
				fail("open U%d: %v", i, err)
				continue
			}
			st.Write([]byte{'U', byte(i)})
			go func() {
				buf := make([]byte, 32<<10)
				for {
					if _, err := st.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	})
	at(bgAt, func() {
		for i := 0; i < nS; i++ {
			if r := open('D', 100+i); r != nil {
				go readPaced(r, z5Env("Z5_SLOW_MBIT", 2)*1e6/8, &sBins[i])
			}
		}
		freeze := int64(z5Env("Z5_FREEZE_KB", 16)) << 10
		takeThenStop := func(r io.Reader) {
			buf := make([]byte, 32<<10)
			var got int64
			for got < freeze {
				n, err := r.Read(buf[:min(int64(len(buf)), freeze-got)])
				got += int64(n)
				if err != nil {
					return
				}
			}
		}
		// Z5_FREEZE_SEQ_S > 0: the frozen readers open one at a time, each
		// that long after the previous one stopped
		if seq := z5Env("Z5_FREEZE_SEQ_S", 0); seq > 0 {
			n := nZ // the goroutine must not see the nZ = 0 below
			go func() {
				for i := 0; i < n; i++ {
					if r := open('D', 150+i); r != nil {
						takeThenStop(r)
					}
					time.Sleep(time.Duration(seq * float64(time.Second)))
				}
			}()
			nZ = 0
		}
		for i := 0; i < nZ; i++ {
			if r := open('D', 150+i); r != nil {
				go takeThenStop(r)
			}
		}
		// Z5_IDB n: fast through the first burst, then Z5_IDB_KBS KiB/s
		// (the next burst finds a slow reader)
		for i := 0; i < int(z5Env("Z5_IDB", 0)); i++ {
			if r := open('B', 230+i); r != nil {
				go func() {
					buf := make([]byte, 32<<10)
					first := int64(z5Env("Z5_IDB_MB", 96)) << 20
					for got := int64(0); got < first; {
						n, err := r.Read(buf)
						got += int64(n)
						tBins.add(t0, n)
						if err != nil {
							return
						}
					}
					readPaced(r, z5Env("Z5_IDB_KBS", 32)*1024, &tBins)
				}()
			}
		}
		ftsFor := time.Duration(z5Env("Z5_FTS_S", 3) * float64(time.Second))
		for i := 0; i < nT; i++ {
			if r := open('D', 200+i); r != nil {
				go func() {
					until := time.Now().Add(ftsFor)
					buf := make([]byte, 32<<10)
					for time.Now().Before(until) {
						n, err := r.Read(buf)
						tBins.add(t0, n)
						if err != nil {
							return
						}
					}
					readPaced(r, z5Env("Z5_FTS_KBS", 32)*1024, &tBins)
				}()
			}
		}
	})
	var lint z5Lat
	// Z5_TURNS: every window turnaround of F0 (edge) and U0 (exit), polled
	// every 2 ms (a turnaround comes per window update: tens a second)
	var turnMu sync.Mutex
	turns := map[string][]float64{}
	if z5Env("Z5_TURNS", 0) > 0 {
		go func() {
			var last [2]int64
			var ptr [2]*int64
			tk := time.NewTicker(2 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
				}
				for i, sp := range []*atomic.Pointer[smux.Stream]{&fSt, &uSt} {
					if ptr[i] == nil {
						ptr[i] = z5TurnPtr(sp.Load())
						if ptr[i] == nil {
							continue
						}
					}
					v := atomic.LoadInt64(ptr[i])
					if v != 0 && v != last[i] {
						last[i] = v
						k := [2]string{"f", "u"}[i]
						turnMu.Lock()
						if len(turns[k]) < 40000 {
							turns[k] = append(turns[k], math.Round(float64(time.Since(t0))/1e5)/10, math.Round(float64(v)/1e4)/100)
						}
						turnMu.Unlock()
					}
				}
			}
		}()
	}
	warm := z5Env("Z5_WARM_S", 6)
	dur := z5Env("Z5_DUR_S", 15)
	wStart := t0.Add(time.Duration(warm * float64(time.Second)))
	wEnd := wStart.Add(time.Duration(dur * float64(time.Second)))
	// speed-change rig: both directions' rate becomes Z5_CHG_MBIT (the queue
	// Z5_QMS at that rate) Z5_CHG_AT s after t0, for Z5_CHG_S s, then back
	if cm := z5Env("Z5_CHG_MBIT", 0); cm > 0 {
		at := t0.Add(time.Duration(z5Env("Z5_CHG_AT", 10) * float64(time.Second)))
		back := at.Add(time.Duration(z5Env("Z5_CHG_S", 10) * float64(time.Second)))
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(at)):
			}
			pu, pd := path(cm), path(cm)
			emu.up.chg.Store(&pu)
			emu.dn.chg.Store(&pd)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(back)):
			}
			emu.up.chg.Store(nil)
			emu.dn.chg.Store(nil)
		}()
	}
	// z17: a policer on the download direction from Z5_POL_AT s after t0
	// (default 0): Z5_POL_MBIT, its bucket Z5_POL_MS ms at that rate
	// (default 20); Z5_POL2_MBIT from Z5_POL2_AT s: the policer tightens
	if pm := z5Env("Z5_POL_MBIT", 0); pm > 0 {
		pol := func(mbit float64) *z5Pol {
			r := mbit * 1e6 / 8
			return &z5Pol{rate: r, burst: r * z5Env("Z5_POL_MS", 20) / 1000}
		}
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(t0.Add(time.Duration(z5Env("Z5_POL_AT", 0) * float64(time.Second))))):
			}
			emu.dn.pol.Store(pol(pm))
			if p2 := z5Env("Z5_POL2_MBIT", 0); p2 > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(t0.Add(time.Duration(z5Env("Z5_POL2_AT", 20) * float64(time.Second))))):
				}
				emu.dn.pol.Store(pol(p2))
			}
		}()
	}
	if z5Env("Z5_INT", 0) > 0 {
		st := open('I', 0)
		go func() {
			req := make([]byte, 200)
			rep := make([]byte, 2048)
			tk := time.NewTicker(100 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
				}
				s := time.Now()
				binary.BigEndian.PutUint64(req, uint64(s.UnixNano()))
				if _, err := st.Write(req); err != nil {
					return
				}
				if _, err := io.ReadFull(st, rep); err != nil {
					return
				}
				if s.After(wStart) && s.Before(wEnd) {
					lint.add(time.Since(s))
				}
			}
		}()
	}

	// sampler: held bytes and heap, both ends, every 100 ms over the whole run
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	res.Heap0 = float64(ms.HeapInuse) / (1 << 20)
	var cpu0, emu0 float64
	var dn0, up0 int64
	var rt0, so0 [2]uint32
	var ns0 map[string]int64
	sampled := false
	trace := z5Env("Z5_TRACE", 0) > 0
	trace2 := z5Env("Z5_TRACE2", 0) > 0
	if trace2 {
		res.T2 = map[string][]float64{}
	}
	r1 := func(x float64) float64 { return math.Round(x*10) / 10 }
	tick := 0
	for time.Now().Before(wEnd) {
		time.Sleep(100 * time.Millisecond)
		tick++
		if trace2 {
			add := func(k string, v float64) { res.T2[k] = append(res.T2[k], r1(v)) }
			add("qd", emu.qms(&emu.dn))
			add("qu", emu.qms(&emu.up))
			add("dropd", float64(emu.dn.drops.Load()))
			add("dropu", float64(emu.up.drops.Load()))
			for i, nm := range []string{"e", "x"} {
				var sr, mr, cw, un, rt, nsent, rr, ts float64
				if ti := z5TCPInfo(socks[i]); ti != nil {
					sr, mr, cw, un, rt, nsent = float64(ti.Rtt)/1000, float64(ti.Min_rtt)/1000, float64(ti.Snd_cwnd), float64(ti.Unacked), float64(ti.Total_retrans), float64(ti.Notsent_bytes)/1024
					rr, ts = float64(ti.Rcv_rtt)/1000, float64(ti.Options&1)
				}
				if ti := z5TCPInfo(socks[i]); ti != nil {
					add("pace"+nm, float64(ti.Pacing_rate)*8/1e6)
					add("dlv"+nm, float64(ti.Delivery_rate)*8/1e6)
					add("ooo"+nm, float64(ti.Rcv_ooopack))
				} else {
					add("pace"+nm, 0)
					add("dlv"+nm, 0)
					add("ooo"+nm, 0)
				}
				add("rcvrtt"+nm, rr)
				add("tsopt"+nm, ts)
				add("srtt"+nm, sr)
				add("minrtt"+nm, mr)
				add("cwnd"+nm, cw)
				add("unacked"+nm, un)
				add("retr"+nm, rt)
				add("notsentK"+nm, nsent)
			}
			add("ctrl", float64(link.mtr.rttMicros.Load())/1000)
			add("ctrlMin", float64(link.mtr.rttMin.Load())/1000)
			fw, fp := z5Win(fSt.Load())
			uw, up := z5Win(uSt.Load())
			add("fwin", fw)
			add("fpeer", fp)
			add("uwin", uw)
			add("upeer", up)
			add("qE", z5QueueOf(edge))
			add("qX", z5QueueOf(exit))
			add("xtraE", float64(z5Extra(edge))/(1<<20))
			add("xtraX", float64(z5Extra(exit))/(1<<20))
		}
		if trace && tick%10 == 0 {
			var a, b, c uint32
			if ti := z5TCPInfo(socks[1]); ti != nil {
				a, c = ti.Total_retrans, ti.Data_segs_out
			}
			var e1, e2 uint32
			if ti := z5TCPInfo(socks[0]); ti != nil {
				b, e1, e2 = ti.Data_segs_in, ti.Total_retrans, ti.Data_segs_out
			}
			res.TRtE, res.TSoE = append(res.TRtE, e1), append(res.TSoE, e2)
			res.TRt, res.TSi, res.TSo = append(res.TRt, a), append(res.TSi, b), append(res.TSo, c)
			res.TEx = append(res.TEx, math.Round(float64(z5Extra(edge))/(1<<20)*10)/10)
			res.TBor = append(res.TBor, z5Borrows(edge))
			res.TPk = append(res.TPk, edge.Parks())
		}
		for i, s := range []*smux.Session{edge, exit} {
			sb := edgeSess
			if i == 1 {
				sb = exitSess
			}
			u, c := z5Held(s, sb)
			res.HeldMax[i] = max(res.HeldMax[i], float64(u)/(1<<20))
			res.CapMax[i] = max(res.CapMax[i], float64(c)/(1<<20))
			res.ExtraMax[i] = max(res.ExtraMax[i], float64(z5Extra(s))/(1<<20))
		}
		runtime.ReadMemStats(&ms)
		res.HeapMax = max(res.HeapMax, float64(ms.HeapInuse)/(1<<20))
		if !sampled && time.Now().After(wStart) {
			sampled = true
			cpu0 = z5CPU()
			emu0 = z5EmuCPU()
			dn0, up0 = emu.dn.bytes.Load(), emu.up.bytes.Load()
			ns0 = z5Netstat()
			for i, c := range socks {
				if ti := z5TCPInfo(c); ti != nil {
					rt0[i] = ti.Total_retrans
					so0[i] = ti.Data_segs_out
				}
			}
		}
	}
	res.CPU = z5CPU() - cpu0
	res.EmuCPU = z5EmuCPU() - emu0
	res.WireDn = float64(emu.dn.bytes.Load()-dn0) * 8 / dur / 1e6
	res.WireUp = float64(emu.up.bytes.Load()-up0) * 8 / dur / 1e6
	for i, c := range socks {
		if ti := z5TCPInfo(c); ti != nil {
			res.Retrans[i] = ti.Total_retrans - rt0[i]
			res.SegsOut[i] = ti.Data_segs_out - so0[i]
			res.Srtt[i] = float64(ti.Rtt) / 1000
		}
	}
	res.Drops = [2]int64{emu.up.drops.Load(), emu.dn.drops.Load()}
	res.PolDrops = emu.dn.polDrops.Load()
	if wc := exitMtr.wc; wc != nil { // z17: the pacing net on the exit's socket
		res.NetHeld = wc.inStats.netHeld.Load()
		res.NetShare = math.Round(1000*math.Float64frombits(wc.inStats.netShare.Load())) / 10
	}
	if ti := z5TCPInfo(socks[1]); ti != nil {
		res.MaxPace = float64(ti.Max_pacing_rate) * 8 / 1e6
	}
	if ns0 != nil {
		res.Netstat = map[string]int64{}
		for k, v := range z5Netstat() {
			if d := v - ns0[k]; d != 0 {
				res.Netstat[k] = d
			}
		}
	}
	res.Parks = [2]uint32{edge.Parks(), exit.Parks()}
	res.Brakes = [2]uint32{edge.Brakes(), exit.Brakes()}
	res.Borrows = [2]uint32{z5Borrows(edge), z5Borrows(exit)}
	res.Pauses = [2]uint32{z5Count(edge, "Pauses"), z5Count(exit, "Pauses")}
	res.Ended = int(ended.Load())
	wcMu.Lock()
	res.WcLines = append(res.WcLines, wcLines...)
	wcMu.Unlock()
	turnMu.Lock()
	if len(turns) > 0 {
		res.Turns = turns
	}
	turnMu.Unlock()

	n := int(warm + dur)
	agg := func(bs []z5Bins) (each []float64, sum float64, s1 []float64) {
		var all z5Bins
		for i := range bs {
			v := bs[i].mbit(warm, warm+dur)
			each = append(each, math.Round(v*10)/10)
			sum += v
			for k := range all.b {
				all.b[k].Add(bs[i].b[k].Load())
			}
		}
		return each, math.Round(sum*10) / 10, all.secs(n)
	}
	res.Fast, res.FastSum, res.Fast1s = agg(fBins)
	res.Up, res.UpSum, res.Up1s = agg(upBins)
	res.Slow, _, _ = agg(sBins)
	res.FTS = math.Round(tBins.mbit(warm, warm+dur)*10) / 10
	for k := range marks {
		res.FMarks = append(res.FMarks, math.Round(float64(markAt[k].Load())/1e5)/10)
	}
	res.Int = z5Pct(lint.v)
	res.Scen, res.Ver, res.Rep = scen, os.Getenv("Z5_VER"), os.Getenv("Z5_REP")
	res.T0 = t0.UTC().Format(time.RFC3339Nano)
	res.Env = map[string]string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "Z5_") {
			p := strings.SplitN(kv, "=", 2)
			res.Env[p[0]] = p[1]
		}
	}
	j, _ := json.Marshal(res)
	if out := os.Getenv("Z5_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(j, '\n'))
			f.Close()
		}
	}
	t.Log(string(j))
}
