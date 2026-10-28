package main

// User classes (R1 round 3, docs/architecture/wip/r1/classes): what a gamer,
// a caller, a viewer and a browser feel, next to the bulk users.
//
//	game     one connection per player through a user port: the client
//	         sends 64 Hz messages of 60-120 B, the server 30 Hz messages of
//	         100-600 B
//	gameudp  the same over UDP through the user port (the edge's "udp": true;
//	         hs2 carries each UDP flow on a stream of its own)
//	call     a video call on one connection: a frame every 20 ms each way,
//	         Rate (1.5) Mbit/s mean, sizes x U(0.7, 1.3), a keyframe of 5x
//	         every 2 s (the mean holds with the keyframes)
//	stream   a DASH/HLS player on one connection: segments of bitrate x 4 s,
//	         back to back until 30 s are buffered, then one whenever the
//	         buffer drops below 26 s; playback starts once 2 s are in and
//	         stalls (a rebuffer) when the buffer empties, until 2 s are in again
//	web      page loads: a 60 KB HTML fetch on a fresh connection, then 30
//	         objects (5-200 KB, log-uniform) over 6 connections (the first
//	         one and 5 new ones), one page every Every (5) s
//
// game and call messages carry their sequence number and the CLOCK_MONOTONIC
// time they were made: both ends run on one host, so each direction has its
// own one-way delay. Each side makes its messages on a fixed schedule into a
// queue that its writer drains, so a slow connection delays the messages, not
// the schedule. The server keeps, per flow, what it made and what it got in
// REPORT/rt-uUSER-fFLOW.json; the client reads that after the run. A message
// not in by stop + grace (2 s) never arrived.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rtHdr      = 16          // TCP message: length u32, seq u32, made (ns) i64, padding
	udpUpHdr   = hdrLen + 20 // UDP client message: the RL header, seq u32, made i64, stop (ns) i64
	udpDnHdr   = 12          // UDP server message: seq u32, made i64
	rtQueue    = 1 << 13     // messages a side may have made and not yet written
	rtReadMore = 300 * time.Millisecond
	rtFileWait = 5 * time.Second

	segDur    = 4.0  // s of media per segment
	bufMax    = 30.0 // s: fetch back to back until this much is buffered
	startNeed = 2.0  // s buffered to start, and to resume after a stall

	htmlBytes = 60000
	webObjs   = 30
	webConns  = 6
	objMin    = 5000
	objMax    = 200000
)

// GenSpec: one side's messages. The client sends the server's in the
// connection's parameters (TCP); the UDP server knows the game's.
type GenSpec struct {
	StopNs   int64   `json:"stop_ns"` // CLOCK_MONOTONIC: no message made from then on
	Hz       float64 `json:"hz"`
	Min      int     `json:"min,omitempty"` // game: sizes uniform in [Min, Max] B
	Max      int     `json:"max,omitempty"`
	RateBps  float64 `json:"rate_bps,omitempty"`  // call: the mean rate, keyframes included
	Var      float64 `json:"var,omitempty"`       // call: sizes x U(1-Var, 1+Var)
	KeyEvery int     `json:"key_every,omitempty"` // call: frame 0, KeyEvery, 2 KeyEvery ... are keyframes
	KeyMult  float64 `json:"key_mult,omitempty"`
	// Seq0: the first message's number (a flow's later connection after a
	// cut, cuts.go: each connection's numbers apart); 0 before
	Seq0 uint32 `json:"seq0,omitempty"`
}

func gameUp(stop int64) GenSpec { return GenSpec{StopNs: stop, Hz: 64, Min: 60, Max: 120} }
func gameDn(stop int64) GenSpec { return GenSpec{StopNs: stop, Hz: 30, Min: 100, Max: 600} }
func callSpec(stop int64, mbit float64) GenSpec {
	return GenSpec{StopNs: stop, Hz: 50, RateBps: mbit * 1e6, Var: 0.3, KeyEvery: 100, KeyMult: 5}
}

func (g *GenSpec) period() time.Duration { return time.Duration(float64(time.Second) / g.Hz) }

// size of message k (bytes, the whole message)
func (g *GenSpec) size(k int, r *rand.Rand) int {
	var n float64
	if g.RateBps > 0 {
		base := g.RateBps / 8 / g.Hz
		if g.KeyEvery > 0 && g.KeyMult > 0 {
			base /= 1 + (g.KeyMult-1)/float64(g.KeyEvery)
		}
		n = base * (1 - g.Var + 2*g.Var*r.Float64())
		if g.KeyEvery > 0 && k%g.KeyEvery == 0 {
			n *= g.KeyMult
		}
	} else {
		n = float64(g.Min + r.IntN(g.Max-g.Min+1))
	}
	return max(int(n), rtHdr)
}

// monoAt: the time.Time at which CLOCK_MONOTONIC reads ns (for deadlines)
func monoAt(ns int64) time.Time { return time.Now().Add(time.Duration(ns - clockMono())) }

// gen makes g's messages from start until g.StopNs: message k is due at
// start + k periods (the schedule does not drift) and stamped when made.
func gen(g GenSpec, start time.Time, out func(seq uint32, made int64, size int)) {
	genQ(g, start, nil, out)
}

// genQ is gen that also ends when quit closes (its connection was cut:
// cuts.go); message k is numbered g.Seq0 + k.
func genQ(g GenSpec, start time.Time, quit <-chan struct{}, out func(seq uint32, made int64, size int)) {
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	per := g.period()
	stop := monoAt(g.StopNs)
	for k := 0; ; k++ {
		due := start.Add(time.Duration(k) * per)
		if !due.Before(stop) {
			return
		}
		if quit == nil {
			sleepUntil(due)
		} else if d := time.Until(due); d > 0 {
			select {
			case <-quit:
				return
			case <-time.After(d):
			}
		}
		select {
		case <-quit:
			return
		default:
		}
		out(g.Seq0+uint32(k), clockMono(), g.size(k, r))
	}
}

func rtMsg(seq uint32, made int64, size int) []byte {
	b := make([]byte, max(size, rtHdr))
	binary.BigEndian.PutUint32(b[0:], uint32(len(b)))
	binary.BigEndian.PutUint32(b[4:], seq)
	binary.BigEndian.PutUint64(b[8:], uint64(made))
	return b
}

// rtSend writes q's messages until q is closed or the deadline; on an error
// it keeps draining q, so the maker never waits on a dead writer
func rtSend(conn net.Conn, q <-chan []byte, dl time.Time) error {
	conn.SetWriteDeadline(dl)
	for m := range q {
		if _, err := conn.Write(m); err != nil {
			for range q {
			}
			return err
		}
	}
	return nil
}

// rtRecv reads messages until the deadline, EOF or an error; a message
// arrives when its last byte is read
func rtRecv(conn net.Conn, dl time.Time, got func(seq uint32, made, at int64, size int)) error {
	conn.SetReadDeadline(dl)
	br := bufio.NewReaderSize(conn, 64<<10)
	var h [rtHdr]byte
	for {
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(h[0:]))
		if n < rtHdr || n > 1<<24 {
			return fmt.Errorf("message length %d", n)
		}
		if _, err := br.Discard(n - rtHdr); err != nil {
			return err
		}
		got(binary.BigEndian.Uint32(h[4:]), int64(binary.BigEndian.Uint64(h[8:])), clockMono(), n)
	}
}

func quietErr(err error) bool {
	var ne net.Error
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || (errors.As(err, &ne) && ne.Timeout())
}

// rtRecord: one flow at the server, for the client to read after the run
type rtRecord struct {
	Tx [][3]int64 `json:"tx"` // seq, made (ns), size: what the server made
	Rx [][2]int64 `json:"rx"` // seq, arrival (ns): what it got
}

func recordPath(dir string, user, flow uint32) string {
	return filepath.Join(dir, fmt.Sprintf("rt-u%d-f%d.json", user, flow))
}

func writeRecord(dir string, user, flow uint32, rec *rtRecord) {
	if dir == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	p := recordPath(dir, user, flow)
	if os.WriteFile(p+".tmp", b, 0o644) == nil {
		os.Rename(p+".tmp", p)
	}
}

// ---------------------------------------------------------------- server

// serveRT: a game ('G') or call ('V') connection: the parameters (u16 length,
// GenSpec JSON) after the header, then messages both ways until stop + grace
func serveRT(c net.Conn, h []byte, report string) {
	user, flow := binary.BigEndian.Uint32(h[4:]), binary.BigEndian.Uint32(h[8:])
	var lb [2]byte
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return
	}
	pb := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	var g GenSpec
	if json.Unmarshal(pb, &g) != nil || g.Hz <= 0 || g.StopNs == 0 {
		log.Printf("rt u%d f%d: bad parameters", user, flow)
		return
	}
	var mu sync.Mutex
	rec := &rtRecord{}
	q := make(chan []byte, rtQueue)
	made := make(chan struct{})
	// quit: the connection ended before the flow's stop (cut): no more
	// messages for it (the client opens another: cuts.go). At the stop the
	// maker has ended already.
	quit := make(chan struct{})
	go func() {
		defer close(made)
		genQ(g, time.Now().Add(phase(g.period())), quit, func(seq uint32, at int64, size int) {
			mu.Lock()
			rec.Tx = append(rec.Tx, [3]int64{int64(seq), at, int64(size)})
			mu.Unlock()
			q <- rtMsg(seq, at, size)
		})
		close(q)
	}()
	stopT := monoAt(g.StopNs)
	sent := make(chan struct{})
	go func() {
		rtSend(c, q, stopT.Add(grace))
		close(sent)
	}()
	rtRecv(c, stopT.Add(grace+rtReadMore), func(seq uint32, _, at int64, _ int) {
		mu.Lock()
		rec.Rx = append(rec.Rx, [2]int64{int64(seq), at})
		mu.Unlock()
	})
	close(quit)
	<-made
	<-sent
	mu.Lock()
	writeRecord(report, user, flow, rec)
	mu.Unlock()
}

// serveReq: request and response ('Q', stream and web): each request is a
// size (u64, 0 ends), answered with that many bytes
func serveReq(c net.Conn) {
	var b [8]byte
	buf := make([]byte, bulkBuf)
	for {
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint64(b[:])
		if n == 0 || n > 1<<30 {
			return
		}
		for n > 0 {
			k := min(n, uint64(len(buf)))
			if _, err := c.Write(buf[:k]); err != nil {
				return
			}
			n -= k
		}
	}
}

type udpRT struct {
	mu   sync.Mutex
	addr net.Addr
	rec  rtRecord
}

// serveUDP: gameudp. Every client datagram carries the RL header (cmd 'g'),
// its seq, when it was made and the flow's stop, so the first one that
// arrives starts the server's side (30 Hz to whatever address the flow's
// datagrams last came from: the exit's socket for that flow).
func serveUDP(pc net.PacketConn, report string) {
	flows := map[uint64]*udpRT{}
	buf := make([]byte, 64<<10)
	for {
		n, addr, err := pc.ReadFrom(buf)
		at := clockMono()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		b := buf[:n]
		if n < udpUpHdr || b[0] != 'R' || b[1] != 'L' || b[2] != 'g' {
			continue
		}
		user, flow := binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:])
		seq := binary.BigEndian.Uint32(b[12:])
		stop := int64(binary.BigEndian.Uint64(b[24:]))
		key := uint64(user)<<32 | uint64(flow)
		f := flows[key]
		if f == nil {
			f = &udpRT{addr: addr}
			flows[key] = f
			go f.run(pc, gameDn(stop), user, flow, report)
		}
		f.mu.Lock()
		f.addr = addr
		f.rec.Rx = append(f.rec.Rx, [2]int64{int64(seq), at})
		f.mu.Unlock()
	}
}

func (f *udpRT) run(pc net.PacketConn, g GenSpec, user, flow uint32, report string) {
	gen(g, time.Now().Add(phase(g.period())), func(seq uint32, made int64, size int) {
		m := make([]byte, max(size, udpDnHdr))
		binary.BigEndian.PutUint32(m[0:], seq)
		binary.BigEndian.PutUint64(m[4:], uint64(made))
		f.mu.Lock()
		f.rec.Tx = append(f.rec.Tx, [3]int64{int64(seq), made, int64(len(m))})
		a := f.addr
		f.mu.Unlock()
		pc.WriteTo(m, a)
	})
	sleepUntil(monoAt(g.StopNs).Add(grace + rtReadMore))
	f.mu.Lock()
	writeRecord(report, user, flow, &f.rec)
	f.mu.Unlock()
}

// ---------------------------------------------------------------- client: game, call

type rtFlow struct {
	us   *ustate
	kind string
	fid  int
	mu   sync.Mutex
	tx   [][3]int64       // what the client made: seq, made, size
	rx   map[uint32]int64 // what it got from the server: seq -> arrival
	srv  *rtRecord        // the server's record (nil: none)
	// nconn: its connections (more than one after a cut, cuts.go); next:
	// the number its next message takes
	nconn int
	next  uint32
}

func (fl *rtFlow) sent(seq uint32, made int64, size int) {
	fl.mu.Lock()
	fl.tx = append(fl.tx, [3]int64{int64(seq), made, int64(size)})
	fl.next = max(fl.next, seq+1)
	fl.mu.Unlock()
}

// nextSeq is the number the flow's next message takes.
func (fl *rtFlow) nextSeq() uint32 {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.next
}

func (fl *rtFlow) got(seq uint32, at int64) {
	fl.mu.Lock()
	if _, dup := fl.rx[seq]; !dup {
		fl.rx[seq] = at
	}
	fl.mu.Unlock()
}

func (c *client) rt(us *ustate, fid int, f Flow) {
	fl := &rtFlow{us: us, kind: f.Kind, fid: fid, rx: map[uint32]int64{}}
	c.mu.Lock()
	c.rts = append(c.rts, fl)
	c.mu.Unlock()
	stop := c.stopOf(f)
	stopNs := clockMono() + int64(time.Until(stop))
	var up, dn GenSpec
	if f.Kind == "call" {
		mbit := f.Rate
		if mbit <= 0 {
			mbit = 1.5
		}
		up, dn = callSpec(stopNs, mbit), callSpec(stopNs, mbit)
	} else {
		up, dn = gameUp(stopNs), gameDn(stopNs)
	}
	sleepUntil(c.at(f.StartS).Add(phase(up.period())))
	if time.Until(stop) <= 0 {
		return
	}
	if f.Kind == "gameudp" {
		c.rtUDP(fl, us, fid, up)
	} else {
		c.rtTCP(fl, us, fid, up, dn)
	}
	if c.srvdir == "" {
		c.err("%s: no -srvdir, no server record", f.Kind)
		return
	}
	until := monoAt(stopNs).Add(grace + rtReadMore + rtFileWait)
	// one record per connection of the flow (more than one after a cut:
	// cuts.go), merged
	for j := 0; j < max(fl.nconn, 1); j++ {
		p := recordPath(c.srvdir, uint32(us.idx), uint32(fid+rtConnFlow*j))
		for {
			if b, err := os.ReadFile(p); err == nil {
				var r rtRecord
				if json.Unmarshal(b, &r) == nil {
					if fl.srv == nil {
						fl.srv = &r
					} else {
						fl.srv.Tx = append(fl.srv.Tx, r.Tx...)
						fl.srv.Rx = append(fl.srv.Rx, r.Rx...)
					}
					break
				}
			}
			if time.Now().After(until) {
				c.err("%s: no server record %s", f.Kind, filepath.Base(p))
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// rtConnFlow: a game or call flow's j-th connection (after j cuts) is flow
// fid + j·rtConnFlow at the server (its own record), and its server's
// messages are numbered from j·rtConnSeq (cuts.go)
const (
	rtConnFlow = 100000
	rtConnSeq  = 1 << 24
)

// rtTCP runs a game or call flow on one connection; in a kill cell
// (cuts.go) a connection cut before the stop is opened again rtRedial
// later, as an app would, its messages numbered on.
func (c *client) rtTCP(fl *rtFlow, us *ustate, fid int, up, dn GenSpec) {
	var fc flowCut
	stopT := monoAt(up.StopNs)
	for j := 0; ; j++ {
		cut := c.rtConn(fl, us, fid, j, up, dn, &fc)
		if !cut || !c.track() || time.Until(stopT) < rtRedial+200*time.Millisecond {
			break
		}
		time.Sleep(rtRedial)
		up.Seq0 = fl.nextSeq()
	}
	c.flowEnd(us, fl.kind, &fc)
}

// rtConn is connection j of a game or call flow; cut: it ended before the
// flow's stop
func (c *client) rtConn(fl *rtFlow, us *ustate, fid, j int, up, dn GenSpec, fc *flowCut) (cut bool) {
	addr := c.target(us, false)
	conn, err := c.dial(us, addr)
	if err != nil {
		us.dialErrs.Add(1)
		c.err("dial %s %s: %v", fl.kind, addr, err)
		return j > 0 // after a cut: try again; the first: as before
	}
	us.conns.Add(1)
	fl.nconn = j + 1
	rec := c.opened(us, fl.kind)
	defer conn.Close()
	cmd := byte('G')
	if fl.kind == "call" {
		cmd = 'V'
	}
	dn.Seq0 = uint32(j) * rtConnSeq
	p, _ := json.Marshal(dn)
	hello := append(header(cmd, us.idx, fid+rtConnFlow*j), byte(len(p)>>8), byte(len(p)))
	hello = append(hello, p...)
	conn.SetWriteDeadline(time.Now().Add(dialWait))
	if _, err := conn.Write(hello); err != nil {
		c.err("%s header: %v", fl.kind, err)
		c.closed(rec, j > 0)
		return j > 0
	}
	stopT := monoAt(up.StopNs)
	done := make(chan struct{})
	var quit chan struct{}
	var quitOnce sync.Once
	if c.track() {
		quit = make(chan struct{})
	}
	stopGen := func() {
		if quit != nil {
			quitOnce.Do(func() { close(quit) })
		}
	}
	var rerr error
	go func() {
		defer close(done)
		first := true
		rerr = rtRecv(conn, stopT.Add(grace+rtReadMore), func(seq uint32, _, at int64, size int) {
			fl.got(seq, at)
			us.dn.add(c.off(), size)
			if first {
				first = false
				c.recovered(us, fl.kind, fc)
			}
		})
		if !quietErr(rerr) {
			c.err("%s read: %v", fl.kind, rerr)
		}
		if time.Until(stopT) > 200*time.Millisecond {
			stopGen() // cut: no more messages for this connection
		}
	}()
	q := make(chan []byte, rtQueue)
	go func() {
		genQ(up, time.Now(), quit, func(seq uint32, made int64, size int) {
			fl.sent(seq, made, size)
			q <- rtMsg(seq, made, size)
		})
		close(q)
	}()
	werr := rtSend(conn, q, stopT.Add(grace))
	if !quietErr(werr) {
		c.err("%s write: %v", fl.kind, werr)
	}
	if werr != nil && c.track() && time.Until(stopT) > 200*time.Millisecond {
		stopGen()
		conn.Close() // the reader ends too
	}
	<-done
	cut = time.Until(stopT) > 200*time.Millisecond
	if cut {
		fc.cut(c)
	}
	c.closed(rec, cut)
	return cut
}

// rtUDP: gameudp. The socket is not connected: the edge answers from a
// socket bound to the wildcard address, so the kernel picks the answer's
// source, and in the rig (the users on the edge's own lo) that is the user's
// own address, not the user port's; a connected socket would drop every
// answer. Where the answers came from is noted (Out.Notes).
func (c *client) rtUDP(fl *rtFlow, us *ustate, fid int, up GenSpec) {
	addr := c.target(us, false)
	ra, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		c.err("gameudp %s: %v", addr, err)
		return
	}
	la := &net.UDPAddr{}
	switch {
	case us.u.Src != "":
		la.IP = net.ParseIP(us.u.Src)
	case !us.tun() && us.u.V6:
		la.IP = randV6(c.s.V6Prefix)
	}
	conn, err := net.ListenUDP("udp", la)
	if err != nil {
		us.dialErrs.Add(1)
		c.err("gameudp socket %s: %v", la, err)
		return
	}
	us.conns.Add(1)
	defer conn.Close()
	stopT := monoAt(up.StopNs)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.SetReadDeadline(stopT.Add(grace + rtReadMore))
		b := make([]byte, 64<<10)
		noted := false
		for {
			n, from, err := conn.ReadFromUDP(b)
			at := clockMono()
			if err != nil {
				if quietErr(err) {
					return
				}
				c.err("gameudp read: %v", err) // ICMP unreachable: no UDP on the user port
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if n < udpDnHdr {
				continue
			}
			if !noted {
				noted = true
				if from.String() == ra.String() {
					c.note("gameudp answers from the user port")
				} else {
					c.note("gameudp answers from another address than the user port (port %d; from the user's own address: %v)",
						from.Port, from.IP.Equal(la.IP))
				}
			}
			fl.got(binary.BigEndian.Uint32(b[0:]), at)
			us.dn.add(c.off(), n)
		}
	}()
	h := header('g', us.idx, fid)
	gen(up, time.Now(), func(seq uint32, made int64, size int) {
		m := make([]byte, max(size, udpUpHdr))
		copy(m, h)
		binary.BigEndian.PutUint32(m[12:], seq)
		binary.BigEndian.PutUint64(m[16:], uint64(made))
		binary.BigEndian.PutUint64(m[24:], uint64(up.StopNs))
		fl.sent(seq, made, len(m))
		if _, err := conn.WriteToUDP(m, ra); err != nil {
			c.err("gameudp write: %v", err)
		}
	})
	<-done
}

// ---------------------------------------------------------------- client: stream

// player: a DASH/HLS player's buffer in media seconds, advanced lazily (all
// times in s from the client's t0)
type player struct {
	rate    float64 // media bytes per second (bitrate / 8)
	need    float64 // s buffered to start and to resume
	t0      float64 // the session's start
	got     float64 // media s received
	pos     float64 // media s played
	last    float64 // pos holds at this time
	state   int     // 0 starting, 1 playing, 2 stalled
	startup float64 // s from t0 to the first frame; -1 until then
	stalls  int
	stallS  float64
	stallAt float64
}

func newPlayer(rate, t0 float64) *player {
	return &player{rate: rate, need: startNeed, t0: t0, last: t0, startup: -1}
}

func (p *player) at(t float64) {
	if t <= p.last {
		return
	}
	if p.state == 1 {
		if buf := p.got - p.pos; t-p.last < buf {
			p.pos += t - p.last
		} else {
			p.pos = p.got
			p.state = 2
			p.stalls++
			p.stallAt = p.last + buf
		}
	}
	p.last = t
}

func (p *player) recv(t float64, n int) {
	p.at(t)
	p.got += float64(n) / p.rate
	if p.state != 1 && p.got-p.pos >= p.need {
		if p.state == 0 {
			p.startup = t - p.t0
		} else {
			p.stallS += t - p.stallAt
		}
		p.state = 1
	}
}

func (p *player) buffer(t float64) float64 {
	p.at(t)
	return p.got - p.pos
}

// end: the session ends at t (a stall still on counts up to t)
func (p *player) end(t float64) {
	p.at(t)
	if p.state == 2 {
		p.stallS += t - p.stallAt
		p.stallAt = t
	}
}

type StreamOut struct {
	BitrateMbit float64 `json:"bitrate_mbit"`
	SessionS    float64 `json:"session_s"`
	StartupS    float64 `json:"startup_s"` // -1: never started
	Rebuffers   int     `json:"rebuffers"`
	RebufferS   float64 `json:"rebuffer_s"`
	Segs        int     `json:"segs"`
	SegMbit     float64 `json:"seg_mbit"`      // finished segments' bytes x 8 / their fetch time
	SegMbitMean float64 `json:"seg_mbit_mean"` // the mean of each segment's own rate
	BufEndS     float64 `json:"buffer_end_s"`
	Reconnects  int     `json:"reconnects,omitempty"`
}

func (c *client) stream(us *ustate, fid int, f Flow) {
	mbit := f.Rate
	if mbit <= 0 {
		mbit = 5
	}
	rate := mbit * 1e6 / 8
	seg := int(rate * segDur)
	stop := c.stopOf(f)
	sleepUntil(c.at(f.StartS)) // the spec staggers the viewers' starts
	now := func() float64 { return c.off().Seconds() }
	p := newPlayer(rate, now())
	o := &StreamOut{BitrateMbit: mbit}
	var conn net.Conn
	var rec *connRec // (cuts.go)
	var fc flowCut
	fresh := false // conn is new: its first byte may end a cut's outage
	var segBytes, segTime, rateSum float64
	buf := make([]byte, 64<<10)
	var rq [8]byte
	binary.BigEndian.PutUint64(rq[:], uint64(seg))
	for time.Until(stop) > 0 {
		if p.buffer(now()) >= bufMax {
			for time.Until(stop) > 0 && p.buffer(now()) >= bufMax-segDur {
				time.Sleep(20 * time.Millisecond)
			}
			continue
		}
		if conn == nil {
			cn, err := c.dial(us, c.target(us, false))
			if err != nil {
				us.dialErrs.Add(1)
				c.err("dial stream: %v", err)
				time.Sleep(time.Second)
				continue
			}
			us.conns.Add(1)
			cn.SetDeadline(stop)
			if _, err := cn.Write(header('Q', us.idx, fid)); err != nil {
				cn.Close()
				continue
			}
			conn = cn
			rec, fresh = c.opened(us, "stream"), true
		}
		ts := time.Now()
		left := seg
		if _, err := conn.Write(rq[:]); err == nil {
			for left > 0 {
				n, err := conn.Read(buf[:min(left, len(buf))])
				if n > 0 {
					left -= n
					p.recv(now(), n)
					us.dn.add(c.off(), n)
					if fresh {
						fresh = false
						c.recovered(us, "stream", &fc)
					}
				}
				if err != nil {
					break
				}
			}
		}
		if left > 0 {
			conn.Close()
			conn = nil
			cut := time.Until(stop) > 200*time.Millisecond
			if cut {
				o.Reconnects++
				c.err("stream connection ended %.1fs before its stop", time.Until(stop).Seconds())
				fc.cut(c)
			}
			c.closed(rec, cut)
			continue
		}
		dt := time.Since(ts).Seconds()
		segBytes += float64(seg)
		segTime += dt
		rateSum += float64(seg) * 8 / dt / 1e6
		o.Segs++
	}
	if conn != nil {
		conn.Close()
		c.closed(rec, false)
	}
	c.flowEnd(us, "stream", &fc)
	end := stop.Sub(c.t0).Seconds()
	p.end(end)
	o.SessionS = round3(end - p.t0)
	o.StartupS = round3(p.startup)
	o.Rebuffers = p.stalls
	o.RebufferS = round3(p.stallS)
	o.BufEndS = round3(p.got - p.pos)
	if segTime > 0 {
		o.SegMbit = round3(segBytes * 8 / segTime / 1e6)
		o.SegMbitMean = round3(rateSum / float64(o.Segs))
	}
	us.mu.Lock()
	us.stream = o
	us.mu.Unlock()
}

// ---------------------------------------------------------------- client: web

type pageRec struct {
	S    float64 // start, s from t0
	Fb   float64 // first byte of the HTML, ms from the start; -1: never
	Load float64 // the last object in, ms from the start; -1: not by the deadline
}

// webObjects: n sizes, log-uniform in [objMin, objMax]
func webObjects(r *rand.Rand, n int) []int {
	out := make([]int, n)
	lo, hi := math.Log(objMin), math.Log(objMax)
	for i := range out {
		out[i] = int(math.Exp(lo + r.Float64()*(hi-lo)))
	}
	return out
}

func (c *client) web(us *ustate, fid int, f Flow) {
	every := time.Duration(f.Every * float64(time.Second))
	if every <= 0 {
		every = 5 * time.Second
	}
	stop := c.stopOf(f)
	hard := stop.Add(grace)
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	next := c.at(f.StartS).Add(phase(every))
	var fc flowCut // (cuts.go) a page whose connection was cut fails; the next page is the app's retry
	for next.Before(stop) {
		sleepUntil(next)
		pg := c.page(us, fid, hard, webObjects(r, webObjs), &fc)
		if pg.Load >= 0 {
			c.recovered(us, "web", &fc) // a whole page again after a cut
		}
		us.mu.Lock()
		us.pages = append(us.pages, pg)
		us.mu.Unlock()
		for !next.After(time.Now()) {
			next = next.Add(every)
		}
	}
	c.flowEnd(us, "web", &fc)
}

func (c *client) page(us *ustate, fid int, hard time.Time, sizes []int, fc *flowCut) pageRec {
	t0 := time.Now()
	pg := pageRec{S: c.off().Seconds(), Fb: -1, Load: -1}
	ms := func() float64 { return math.Round(float64(time.Since(t0))/1e4) / 100 }
	addr := c.target(us, false)
	// the page's connections in a kill cell (cuts.go): cut when a fetch
	// fails before the deadline; all end with the page
	var rmu sync.Mutex
	recs := map[net.Conn]*connRec{}
	defer func() {
		rmu.Lock()
		for _, rec := range recs {
			c.closed(rec, rec.X >= 0)
		}
		rmu.Unlock()
	}()
	cutOf := func(cn net.Conn) {
		if time.Until(hard) <= 200*time.Millisecond {
			return
		}
		rmu.Lock()
		if rec := recs[cn]; rec != nil && rec.X < 0 {
			rec.X = c.off().Seconds()
		}
		rmu.Unlock()
		fc.cut(c)
	}
	open := func() net.Conn {
		cn, err := c.dial(us, addr)
		if err != nil {
			us.dialErrs.Add(1)
			c.err("dial web %s: %v", addr, err)
			return nil
		}
		us.conns.Add(1)
		if rec := c.opened(us, "web"); rec != nil {
			rmu.Lock()
			recs[cn] = rec
			rmu.Unlock()
		}
		cn.SetDeadline(hard)
		if _, err := cn.Write(header('Q', us.idx, fid)); err != nil {
			cn.Close()
			cutOf(cn)
			return nil
		}
		return cn
	}
	fetch := func(cn net.Conn, n int, first func()) bool {
		var rq [8]byte
		binary.BigEndian.PutUint64(rq[:], uint64(n))
		if _, err := cn.Write(rq[:]); err != nil {
			cutOf(cn)
			return false
		}
		buf := make([]byte, 64<<10)
		for n > 0 {
			k, err := cn.Read(buf[:min(n, len(buf))])
			if k > 0 {
				if first != nil {
					first()
					first = nil
				}
				n -= k
				us.dn.add(c.off(), k)
			}
			if err != nil && n > 0 {
				cutOf(cn)
				return false
			}
		}
		return true
	}
	c1 := open()
	if c1 == nil {
		return pg
	}
	defer c1.Close()
	if !fetch(c1, htmlBytes, func() { pg.Fb = ms() }) {
		return pg
	}
	var next, done atomic.Int32
	work := func(cn net.Conn) {
		for {
			i := int(next.Add(1)) - 1
			if i >= len(sizes) || !fetch(cn, sizes[i], nil) {
				return
			}
			done.Add(1)
		}
	}
	var wg sync.WaitGroup
	for k := 1; k < webConns; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cn := open(); cn != nil {
				defer cn.Close()
				work(cn)
			}
		}()
	}
	work(c1)
	wg.Wait()
	if int(done.Load()) == len(sizes) {
		pg.Load = ms()
	}
	return pg
}

// ---------------------------------------------------------------- client: the classes' numbers

type sample struct {
	s    float64 // made, s from t0
	ms   float64 // one-way delay; < 0: never arrived
	size int
}

// samples of one direction of fl: "up" client to server, "dn" server to client
func (fl *rtFlow) samples(dir string, t0m int64) []sample {
	tx, rx := fl.tx, fl.rx
	if dir == "up" {
		rx = map[uint32]int64{}
		for _, r := range fl.srv.Rx {
			if _, dup := rx[uint32(r[0])]; !dup {
				rx[uint32(r[0])] = r[1]
			}
		}
	} else {
		tx = fl.srv.Tx
	}
	out := make([]sample, 0, len(tx))
	for _, t := range tx {
		s := sample{s: float64(t[1]-t0m) / 1e9, ms: -1, size: int(t[2])}
		if at, ok := rx[uint32(t[0])]; ok {
			s.ms = float64(at-t[1]) / 1e6
		}
		out = append(out, s)
	}
	return out
}

// Owd: one-way delays (ms) of the messages made in a window
type Owd struct {
	N             int     `json:"n"`
	Sent          int     `json:"sent"`
	Lost          int     `json:"lost"` // never arrived (by stop + grace)
	Min           float64 `json:"min"`
	P50           float64 `json:"p50"`
	P99           float64 `json:"p99"`
	P999          float64 `json:"p999"`
	Max           float64 `json:"max"`
	Jit99         float64 `json:"jit_p99"` // p99 of |delay - the previous arrival's delay| in one flow
	Late          int     `json:"late"`    // arrived, later than floor + late_ms
	LateMs        float64 `json:"late_ms"`
	OfferedMbit   float64 `json:"offered_mbit,omitempty"`   // call: what was made
	DeliveredMbit float64 `json:"delivered_mbit,omitempty"` // call: what of it arrived
}

func pct2(v []float64, p float64) float64 {
	if len(v) == 0 {
		return -1
	}
	i := int(math.Ceil(p*float64(len(v)))) - 1
	return math.Round(v[max(i, 0)]*100) / 100
}

func owdOf(flows [][]sample, from, to, floor, lateMs float64, rates bool) *Owd {
	var v, j []float64
	o := &Owd{LateMs: lateMs}
	var off, del float64
	for _, f := range flows {
		prev := -1.0
		for _, s := range f {
			if s.s < from || s.s >= to {
				continue
			}
			o.Sent++
			off += float64(s.size)
			if s.ms < 0 {
				o.Lost++
				continue
			}
			del += float64(s.size)
			v = append(v, s.ms)
			if prev >= 0 {
				j = append(j, math.Abs(s.ms-prev))
			}
			prev = s.ms
			if floor >= 0 && s.ms > floor+lateMs {
				o.Late++
			}
		}
	}
	if o.Sent == 0 {
		return nil
	}
	sort.Float64s(v)
	sort.Float64s(j)
	o.N = len(v)
	o.Min, o.P50, o.P99, o.P999, o.Max = pct2(v, 0), pct2(v, 0.5), pct2(v, 0.99), pct2(v, 0.999), pct2(v, 1)
	o.Jit99 = pct2(j, 0.99)
	if rates && to > from {
		o.OfferedMbit = round3(off * 8 / (to - from) / 1e6)
		o.DeliveredMbit = round3(del * 8 / (to - from) / 1e6)
	}
	return o
}

// floorOf: the least one-way delay of the messages made in [from, to) (all of
// them if none there); -1: nothing arrived
func floorOf(flows [][]sample, from, to float64) float64 {
	best := -1.0
	for pass := 0; pass < 2 && best < 0; pass++ {
		for _, f := range flows {
			for _, s := range f {
				if s.ms >= 0 && (pass == 1 || (s.s >= from && s.s < to)) && (best < 0 || s.ms < best) {
					best = s.ms
				}
			}
		}
	}
	return math.Round(best*100) / 100
}

type RTClass struct {
	Flows  int                `json:"flows"`
	Errs   int                `json:"no_record,omitempty"` // flows without the server's record
	LateMs float64            `json:"late_ms"`
	Floor  map[string]float64 `json:"floor_ms"` // per direction: the least one-way delay in the window "all"
	Up     map[string]*Owd    `json:"up"`       // client to server, per window
	Dn     map[string]*Owd    `json:"dn"`
}

type StreamClass struct {
	Users        int     `json:"users"`
	NotStarted   int     `json:"not_started"`
	StartupP50   float64 `json:"startup_p50_s"`
	StartupMax   float64 `json:"startup_max_s"` // of those that started
	Rebuffers    int     `json:"rebuffers"`
	RebufferS    float64 `json:"rebuffer_s"`
	RebufUsers   int     `json:"rebuffer_users"`
	SegMbit      float64 `json:"seg_mbit_mean"` // the mean of the users' seg_mbit
	BitrateMbit  float64 `json:"bitrate_mbit"`
	SessionMeanS float64 `json:"session_mean_s"`
}

type WebStats struct {
	Pages   int     `json:"pages"`
	Failed  int     `json:"failed"` // not loaded by stop + grace
	LoadP50 float64 `json:"load_p50_ms"`
	LoadP95 float64 `json:"load_p95_ms"` // -1: the percentile is a failed page
	LoadMax float64 `json:"load_max_ms"`
	FbP50   float64 `json:"fb_p50_ms"`
	FbP95   float64 `json:"fb_p95_ms"`
}

func webOf(pages []pageRec, from, to float64) *WebStats {
	var ld, fb []float64
	w := &WebStats{}
	for _, p := range pages {
		if p.S < from || p.S >= to {
			continue
		}
		w.Pages++
		if p.Load < 0 {
			w.Failed++
			ld = append(ld, math.Inf(1))
		} else {
			ld = append(ld, p.Load)
		}
		if p.Fb < 0 {
			fb = append(fb, math.Inf(1))
		} else {
			fb = append(fb, p.Fb)
		}
	}
	if w.Pages == 0 {
		return nil
	}
	sort.Float64s(ld)
	sort.Float64s(fb)
	inf := func(x float64) float64 {
		if math.IsInf(x, 1) {
			return -1
		}
		return x
	}
	w.LoadP50, w.LoadP95, w.LoadMax = inf(pct2(ld, 0.5)), inf(pct2(ld, 0.95)), inf(pct2(ld, 1))
	w.FbP50, w.FbP95 = inf(pct2(fb, 0.5)), inf(pct2(fb, 0.95))
	return w
}

func (c *client) window(name string) Window {
	for _, w := range c.s.Windows {
		if w.Name == name {
			return w
		}
	}
	if name == "main" && len(c.s.Windows) > 0 {
		return c.s.Windows[0]
	}
	return Window{name, 0, c.s.TotalS}
}

// classes: the classes' numbers once every flow is done; also each user's
// own (main window), and the client-to-server bytes of game and call (from
// the server's records) into the users' upload bins
func (c *client) classes() map[string]any {
	res := map[string]any{}
	mw, aw := c.window("main"), c.window("all")
	for _, kind := range []string{"game", "gameudp", "call"} {
		var fs []*rtFlow
		for _, fl := range c.rts {
			if fl.kind == kind {
				fs = append(fs, fl)
			}
		}
		if len(fs) == 0 {
			continue
		}
		late, rates := 100.0, kind == "call"
		if rates {
			late = 150
		}
		rc := &RTClass{Flows: len(fs), LateMs: late, Floor: map[string]float64{}, Up: map[string]*Owd{}, Dn: map[string]*Owd{}}
		for _, fl := range fs {
			if fl.srv == nil {
				rc.Errs++
				continue
			}
			size := map[uint32]int{}
			for _, t := range fl.tx {
				size[uint32(t[0])] = int(t[2])
			}
			for _, r := range fl.srv.Rx {
				fl.us.up.add(time.Duration(r[1]-c.t0m), size[uint32(r[0])])
			}
		}
		for _, dir := range []string{"up", "dn"} {
			var all [][]sample
			per := map[*ustate][][]sample{}
			for _, fl := range fs {
				if fl.srv == nil {
					continue
				}
				sm := fl.samples(dir, c.t0m)
				all = append(all, sm)
				per[fl.us] = append(per[fl.us], sm)
			}
			floor := floorOf(all, aw.From, aw.To)
			rc.Floor[dir] = floor
			m := rc.Up
			if dir == "dn" {
				m = rc.Dn
			}
			for _, w := range c.s.Windows {
				if o := owdOf(all, w.From, w.To, floor, late, rates); o != nil {
					m[w.Name] = o
				}
			}
			for us, sm := range per {
				if o := owdOf(sm, mw.From, mw.To, floor, late, rates); o != nil {
					if us.rt == nil {
						us.rt = map[string]*Owd{}
					}
					us.rt[dir] = o
				}
			}
		}
		res[kind] = rc
	}
	sc := &StreamClass{}
	var starts []float64
	var seg, sess float64
	for _, us := range c.users {
		o := us.stream
		if o == nil {
			continue
		}
		sc.Users++
		sc.BitrateMbit = o.BitrateMbit
		if o.StartupS < 0 {
			sc.NotStarted++
		} else {
			starts = append(starts, o.StartupS)
		}
		sc.Rebuffers += o.Rebuffers
		sc.RebufferS += o.RebufferS
		if o.Rebuffers > 0 {
			sc.RebufUsers++
		}
		seg += o.SegMbit
		sess += o.SessionS
	}
	if sc.Users > 0 {
		sort.Float64s(starts)
		sc.StartupP50, sc.StartupMax = pct2(starts, 0.5), pct2(starts, 1)
		sc.RebufferS = round3(sc.RebufferS)
		sc.SegMbit = round3(seg / float64(sc.Users))
		sc.SessionMeanS = round3(sess / float64(sc.Users))
		res["stream"] = sc
	}
	var pages []pageRec
	for _, us := range c.users {
		pages = append(pages, us.pages...)
		us.web = webOf(us.pages, mw.From, mw.To)
	}
	if len(pages) > 0 {
		wm := map[string]*WebStats{}
		for _, w := range c.s.Windows {
			if ws := webOf(pages, w.From, w.To); ws != nil {
				wm[w.Name] = ws
			}
		}
		res["web"] = wm
	}
	return res
}
