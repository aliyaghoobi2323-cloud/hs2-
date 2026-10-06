//go:build linux

package encap

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/mmsg"
	"golang.org/x/sys/unix"
)

// Raw-socket encapsulations on Linux. Each transport is one AF_INET SOCK_RAW
// socket of the kind's IP protocol, driven through Go's *net.IPConn so reads
// and writes use the runtime poller (deadlines work, no thread per socket).
//
// The kernel builds the outer IPv4 header on send (no IP_HDRINCL); on receive a
// raw socket delivers the whole IPv4 datagram, header included, which we parse
// ourselves — that also gives the listener the local address each peer
// targeted, so a wildcard listener can answer from it.

func init() {
	dialRawFn = dialRawLinux
	listenRawFn = listenRawLinux
}

// rawPeerTTL / rawPeerMax bound the listener's per-peer state (the reply
// counter, the local address the peer targeted).
const (
	rawPeerTTL   = 5 * time.Minute
	rawPeerMax   = 16384
	rawPeerSweep = time.Minute
)

func rawNetwork(proto int) string { return "ip4:" + strconv.Itoa(proto) }

func parseBindIP(s string) (net.IP, error) {
	if s == "" {
		return nil, nil
	}
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return nil, fmt.Errorf("encap: bind IP %q is not an IPv4 address", s)
	}
	return ip, nil
}

// tuneRawSocket enlarges the buffers, installs the receive filter and, when
// pmtudisc >= 0, sets the IP Don't-Fragment policy (IP_MTU_DISCOVER). The icmp
// encapsulation uses it so the DF bit of its packets matches real ping: the
// dial socket (echo requests) sets IP_PMTUDISC_DO (DF=1, like iputils ping) and
// the listen socket (echo replies) IP_PMTUDISC_DONT (DF=0, like the kernel's own
// echo replies) — otherwise a DF mismatch separates the tunnel from real ping.
func tuneRawSocket(c *net.IPConn, prog []bpfInsn, pmtudisc int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	cerr := rc.Control(func(fd uintptr) {
		s := int(fd)
		forceSockBufs(s, sockBuf)
		if pmtudisc >= 0 {
			// Best-effort: a failure here only loses the DF-match cosmetic.
			_ = unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, pmtudisc)
		}
		filter := make([]unix.SockFilter, len(prog))
		for i, in := range prog {
			filter[i] = unix.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K}
		}
		fprog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
		serr = unix.SetsockoptSockFprog(s, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog)
	})
	if cerr != nil {
		return cerr
	}
	if serr != nil {
		return fmt.Errorf("attach receive filter: %w", serr)
	}
	return nil
}

// pmtudiscFor returns the IP_MTU_DISCOVER value for a kind and socket role, or
// -1 to leave the kernel default. Only icmp sets it (to match real ping's DF).
func pmtudiscFor(kind string, dial bool) int {
	if kind != KindICMP {
		return -1
	}
	if dial {
		return unix.IP_PMTUDISC_DO // echo requests: DF=1
	}
	return unix.IP_PMTUDISC_DONT // echo replies: DF=0, like the kernel's own
}

// randReplyCtr seeds a peer's reply counter (icmp) with a random 16-bit value,
// so the first reply to each peer does not start at a fixed sequence.
func randReplyCtr() uint16 {
	var b [2]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

// bufPool holds packet-build scratch so concurrent writers (the carrier's
// pacer and its control sender) never serialise on one buffer.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 2048); return &b }}

// ---------------------------------------------------------------------------
// Dial side: one shared raw socket per (kind, key, bind IP, peer IP), with the
// links on it told apart by their link id in userspace.
//
// The kernel hands every received packet of a raw socket's IP protocol to
// EVERY raw socket of that protocol (raw_v4_input clones it to each and runs
// each one's filter), so one socket per link made each received packet cost
// O(links): measured over GRE, 5.6–7.1 µs a packet with one socket and 77–81 µs
// with 300 — one core then received only ~12k packets a second. One socket per
// peer makes it O(1): a single reader takes each packet once and hands it to
// its link by id. Link ids are unique per (kind, peer IP) in this process, so
// two links never share one (they used to be drawn at random, and at 300
// links two collided about half the time — that dial then failed).

// rawMuxQueue is how many received packets wait for one link's reader before
// further ones are dropped (plain loss, which FEC and the inner transport
// handle): a link whose reader stalls must not hold up the others on the
// shared socket.
const rawMuxQueue = 512

type rawMuxKey struct {
	kind  string
	proto int
	key   string // the framing key (it keys the magic and icmp's masking)
	bind  [4]byte
	peer  [4]byte
}

// rawIDKey scopes link-id uniqueness: the listener tells links apart by
// (source IP, id), and two muxes to one peer (a wildcard and an explicit bind
// that routes the same way) can share a source IP.
type rawIDKey struct {
	kind string
	peer [4]byte
}

var (
	rawMuxMu sync.Mutex
	rawMuxes = map[rawMuxKey]*rawMux{}
	rawIDs   = map[rawIDKey]map[uint16]bool{}
)

// rawMux is one shared dial socket and the links on it.
type rawMux struct {
	key     rawMuxKey
	ipc     *net.IPConn
	f       *framer
	raddrIP *net.IPAddr
	tx      *rawTx        // send-only socket (nil: send on ipc; see rawTx)
	dead    chan struct{} // closed when the socket fails or closes
	err     error         // why, set before dead closes

	mu    sync.RWMutex
	links map[uint16]*rawConn
	refs  int // under rawMuxMu

	dropped atomic.Uint64 // packets dropped for a full link queue
}

// rawConn is one link (one link id) on a shared dial socket.
type rawConn struct {
	mx    *rawMux
	ipc   *net.IPConn // the shared socket (tests)
	f     *framer
	id    uint16
	seq   atomic.Uint32 // icmp echo sequence, like ping's
	laddr *Addr
	raddr *Addr

	q chan *[]byte // received transport payloads, header included
	// recv, once set (SetReceiver), takes every packet straight from the
	// shared reader instead of q — one hand-off less per packet.
	recv   atomic.Pointer[func([]byte)]
	onErr  atomic.Pointer[func(error)]
	done   chan struct{}
	once   sync.Once
	rdl    atomic.Int64  // read deadline, unix ns (0 = none)
	rdlSet chan struct{} // a new read deadline (wakes a blocked Read)

	// WriteBatch's reusable arrays (its one writer is the carrier's pacer).
	wmu   sync.Mutex
	wb    *mmsg.Batch
	wsa   *unix.RawSockaddrInet4
	wpkts [][]byte
	wbps  []*[]byte
}

func dialRawLinux(kind, addr string, opt Options) (net.Conn, error) {
	f, err := newFramer(kind, opt, true)
	if err != nil {
		return nil, err
	}
	ra, err := net.ResolveIPAddr("ip4", hostOf(addr))
	if err != nil {
		return nil, fmt.Errorf("encap %s: %w", f.kind, err)
	}
	peer := ra.IP.To4()
	if peer == nil {
		return nil, fmt.Errorf("encap %s: %s is not an IPv4 address", f.kind, ra.IP)
	}
	bip, err := parseBindIP(opt.BindIP)
	if err != nil {
		return nil, err
	}
	k := rawMuxKey{kind: f.kind, proto: f.proto, key: string(opt.Key)}
	copy(k.peer[:], peer)
	if bip != nil {
		copy(k.bind[:], bip)
	}
	ik := rawIDKey{kind: f.kind, peer: k.peer}

	rawMuxMu.Lock()
	defer rawMuxMu.Unlock()
	ids := rawIDs[ik]
	if len(ids) >= 65535 {
		return nil, fmt.Errorf("encap %s: every link id to %s is in use", f.kind, ra.IP)
	}
	mx := rawMuxes[k]
	if mx == nil || mx.failed() {
		if mx, err = openRawMux(k, f, bip, peer); err != nil {
			return nil, err
		}
		rawMuxes[k] = mx // a failed one closes with its last link
	}
	if ids == nil {
		ids = map[uint16]bool{}
		rawIDs[ik] = ids
	}
	id := uniqueLinkID(ids)
	ids[id] = true
	c := &rawConn{mx: mx, ipc: mx.ipc, f: mx.f, id: id,
		raddr: &Addr{IP: peer, ID: id, Kind: f.kind},
		q:     make(chan *[]byte, rawMuxQueue), done: make(chan struct{}), rdlSet: make(chan struct{}, 1)}
	local := net.IPv4zero
	if bip != nil {
		local = bip
	}
	c.laddr = &Addr{IP: local, ID: id, Kind: f.kind}
	var s [2]byte
	rand.Read(s[:])
	c.seq.Store(uint32(binary.BigEndian.Uint16(s[:])))
	mx.refs++
	mx.mu.Lock()
	mx.links[id] = c
	mx.mu.Unlock()
	return c, nil
}

// uniqueLinkID draws a random non-zero link id not in use. Caller holds
// rawMuxMu and has checked that one is free.
func uniqueLinkID(inUse map[uint16]bool) uint16 {
	var b [2]byte
	for {
		rand.Read(b[:])
		if id := binary.BigEndian.Uint16(b[:]); id != 0 && !inUse[id] {
			return id
		}
	}
}

// openRawMux opens the shared socket for k and starts its reader. Caller
// holds rawMuxMu.
func openRawMux(k rawMuxKey, f *framer, bip, peer net.IP) (*rawMux, error) {
	// The socket is UNCONNECTED (net.ListenIP, not DialIP). A connected raw
	// socket reports ICMP errors for the flow as fatal read/write errors, so
	// one ICMP packet — a router's frag-needed, a stray port/protocol
	// unreachable from an ip_gre/ipip module on the server, or a spoofed one
	// from an off-path attacker who knows only the two IPs and the protocol —
	// would tear every link to that server down. An unconnected socket
	// ignores those errors; we restrict delivery to the peer ourselves, with a
	// source-IP match in the BPF filter and in the reader.
	la := &net.IPAddr{IP: net.IPv4zero}
	if bip != nil {
		la = &net.IPAddr{IP: bip}
	}
	ipc, err := net.ListenIP(rawNetwork(f.proto), la)
	if err != nil {
		return nil, rawErr(f.kind, err)
	}
	if err := tuneRawSocket(ipc, f.recvFilter(0, binary.BigEndian.Uint32(peer)), pmtudiscFor(f.kind, true)); err != nil {
		ipc.Close()
		return nil, fmt.Errorf("encap %s: %w", f.kind, err)
	}
	mx := &rawMux{key: k, ipc: ipc, f: f, raddrIP: &net.IPAddr{IP: append(net.IP(nil), peer...)},
		tx:   openRawTx(f, ipc, bip),
		dead: make(chan struct{}), links: map[uint16]*rawConn{}}
	go mx.readLoop()
	return mx, nil
}

// rawBatch / rawBufSize: datagrams one receive call takes off a raw socket,
// and the room for each (the largest tunnel MTU, 9000, plus headers).
const (
	rawBatch   = 32
	rawBufSize = 9216
)

// readLoop takes every packet off the shared socket once and hands it to its
// link; a packet for no link, or not from the peer, is dropped. It takes
// what is waiting in one call (recvmmsg) — at 60 Mbit/s one receive syscall
// per datagram was a large share of a small server's CPU.
func (mx *rawMux) readLoop() {
	fail := func(err error) {
		mx.err = err
		close(mx.dead)
		mx.mu.RLock()
		for _, c := range mx.links {
			if fn := c.onErr.Load(); fn != nil {
				go (*fn)(err)
			}
		}
		mx.mu.RUnlock()
	}
	if rc, err := mx.ipc.SyscallConn(); err == nil && mmsg.Supported && !rawNoBatch {
		b := mmsg.NewBatch(rawBatch)
		bufs := make([][]byte, rawBatch)
		for i := range bufs {
			bufs[i] = make([]byte, rawBufSize)
		}
		sizes, trunc := make([]int, rawBatch), make([]bool, rawBatch)
		for {
			n, err := b.Recv(rc, bufs, sizes, trunc, false, nil, nil)
			if err != nil {
				if softErr(err) {
					continue // an ICMP error for this socket; not the link dying
				}
				fail(err)
				return
			}
			for i := 0; i < n; i++ {
				if !trunc[i] {
					mx.deliver(bufs[i][:sizes[i]])
				}
			}
		}
	}
	buf := make([]byte, 65536)
	for {
		n, _, _, _, err := mx.ipc.ReadMsgIP(buf, nil)
		if err != nil {
			if softErr(err) {
				continue
			}
			fail(err)
			return
		}
		mx.deliver(buf[:n])
	}
}

// rawNoBatch (HS2_RAW_BATCH=0) sends and receives one datagram per syscall,
// as before batching.
var rawNoBatch = os.Getenv("HS2_RAW_BATCH") == "0"

// deliver hands one received IP datagram to its link (or drops it).
func (mx *rawMux) deliver(pkt []byte) {
	src, _, tp, ok := mx.f.ipv4Payload(pkt)
	if !ok || !src.Equal(mx.raddrIP.IP) {
		return // not from the peer (the socket is unconnected)
	}
	id, _, ok := mx.f.parse(tp)
	if !ok {
		return
	}
	mx.mu.RLock()
	c := mx.links[id]
	mx.mu.RUnlock()
	if c == nil {
		return
	}
	if fn := c.recv.Load(); fn != nil {
		(*fn)(tp[mx.f.hdr:]) // must not block or keep the slice
		return
	}
	bp := bufPool.Get().(*[]byte)
	*bp = append((*bp)[:0], tp...)
	select {
	case c.q <- bp:
	default:
		bufPool.Put(bp)
		mx.dropped.Add(1)
	}
}

// failed reports whether the socket has failed (its links are ending).
func (mx *rawMux) failed() bool {
	select {
	case <-mx.dead:
		return true
	default:
		return false
	}
}

// releaseLocked drops one link's hold; the last one closes the socket.
// Caller holds rawMuxMu.
func (mx *rawMux) releaseLocked() {
	if mx.refs--; mx.refs <= 0 {
		if rawMuxes[mx.key] == mx {
			delete(rawMuxes, mx.key)
		}
		mx.ipc.Close() // ends readLoop
		mx.tx.close()
	}
}

func (c *rawConn) Read(b []byte) (int, error) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var expire <-chan time.Time
		if d := c.rdl.Load(); d != 0 {
			wait := time.Until(time.Unix(0, d))
			if wait <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			expire = timer.C
		}
		select {
		case bp := <-c.q:
			n := copy(b, (*bp)[c.f.hdr:])
			bufPool.Put(bp)
			return n, nil
		case <-c.done:
			return 0, net.ErrClosed
		case <-c.mx.dead:
			return 0, fmt.Errorf("encap %s: %w", c.f.kind, c.mx.err)
		case <-expire:
		case <-c.rdlSet:
		}
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (c *rawConn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	bp := bufPool.Get().(*[]byte)
	if tx := c.mx.tx; tx.ok() {
		pkt := tx.frame(bp, c.f, c.id, uint16(c.seq.Add(1)), p, c.mx.raddrIP.IP, nil)
		err := tx.sendAll([][]byte{pkt}, c.mx.raddrIP.IP, nil, c.legacyWrite)
		*bp = pkt[:0]
		bufPool.Put(bp)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	pkt := c.f.build(*bp, c.id, uint16(c.seq.Add(1)), p)
	err := c.legacyWrite(pkt)
	*bp = pkt[:0]
	bufPool.Put(bp)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// legacyWrite sends one framed transport packet on the shared receive socket
// (the kernel builds the IP header). A soft error (ENOBUFS, an ICMP error)
// drops it and counts it. Also rawTx's fallback.
func (c *rawConn) legacyWrite(tp []byte) error {
	_, err := c.mx.ipc.WriteToIP(tp, c.mx.raddrIP)
	if err != nil {
		if !softErr(err) {
			return err
		}
		sendRefused.Add(1)
	}
	return nil
}

// WriteBatch sends several datagrams to the link's peer in one syscall
// (sendmmsg); a soft error (ENOBUFS, an ICMP error) drops that datagram, as
// in Write.
func (c *rawConn) WriteBatch(ps [][]byte) error {
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}
	if rawNoBatch || !mmsg.Supported {
		for _, p := range ps {
			if _, err := c.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	if tx := c.mx.tx; tx.ok() {
		return c.writeBatchTx(tx, ps)
	}
	rc, err := c.mx.ipc.SyscallConn()
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.wb == nil {
		c.wb = mmsg.NewBatch(rawBatch)
		c.wsa = mmsg.Inet4(c.mx.raddrIP.IP.To4(), 0)
	}
	for len(ps) > 0 {
		chunk := ps[:min(len(ps), rawBatch)]
		ps = ps[len(chunk):]
		pkts := c.wpkts[:0]
		bps := c.wbps[:0]
		for _, p := range chunk {
			bp := bufPool.Get().(*[]byte)
			pkt := c.f.build(*bp, c.id, uint16(c.seq.Add(1)), p)
			*bp = pkt[:0]
			bps = append(bps, bp)
			pkts = append(pkts, pkt)
		}
		err := sendAll(c.wb, rc, pkts, c.wsa, nil)
		for _, bp := range bps {
			bufPool.Put(bp)
		}
		c.wpkts, c.wbps = pkts[:0], bps[:0]
		if err != nil {
			return err
		}
	}
	return nil
}

// writeBatchTx is WriteBatch through the send-only socket: whole IP packets,
// no lock shared with the other links to this peer.
func (c *rawConn) writeBatchTx(tx *rawTx, ps [][]byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for len(ps) > 0 {
		chunk := ps[:min(len(ps), rawBatch)]
		ps = ps[len(chunk):]
		pkts := c.wpkts[:0]
		bps := c.wbps[:0]
		for _, p := range chunk {
			bp := bufPool.Get().(*[]byte)
			pkt := tx.frame(bp, c.f, c.id, uint16(c.seq.Add(1)), p, c.mx.raddrIP.IP, nil)
			*bp = pkt[:0]
			bps = append(bps, bp)
			pkts = append(pkts, pkt)
		}
		err := tx.sendAll(pkts, c.mx.raddrIP.IP, nil, c.legacyWrite)
		for _, bp := range bps {
			bufPool.Put(bp)
		}
		c.wpkts, c.wbps = pkts[:0], bps[:0]
		if err != nil {
			return err
		}
	}
	return nil
}

// sendAll sends every datagram, dropping one that meets a soft error.
func sendAll(b *mmsg.Batch, rc syscall.RawConn, pkts [][]byte, to *unix.RawSockaddrInet4, oob []byte) error {
	for len(pkts) > 0 {
		n, err := b.Send(rc, pkts, to, oob)
		if err == nil {
			pkts = pkts[n:]
			continue
		}
		if !softErr(err) {
			return err
		}
		sendRefused.Add(1)
		pkts = pkts[min(n+1, len(pkts)):] // that one is lost, as with Write
	}
	return nil
}

// Close takes the link off the shared socket (closing the socket with the
// last link) and frees its id.
func (c *rawConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.mx.mu.Lock()
		delete(c.mx.links, c.id)
		c.mx.mu.Unlock()
		rawMuxMu.Lock()
		ik := rawIDKey{kind: c.mx.key.kind, peer: c.mx.key.peer}
		if ids := rawIDs[ik]; ids != nil {
			delete(ids, c.id)
			if len(ids) == 0 {
				delete(rawIDs, ik)
			}
		}
		c.mx.releaseLocked()
		rawMuxMu.Unlock()
		for {
			select {
			case bp := <-c.q:
				bufPool.Put(bp)
				continue
			default:
			}
			break
		}
	})
	return nil
}

// SetReceiver hands every packet received from now on to fn, on the shared
// socket's reader goroutine — fn must neither block nor keep the slice (copy
// it); packets already queued go first. onErr is called (once, on its own
// goroutine) if the shared socket fails. Read is not used after this.
func (c *rawConn) SetReceiver(fn func([]byte), onErr func(error)) {
	c.onErr.Store(&onErr)
	for {
		select {
		case bp := <-c.q:
			fn((*bp)[c.f.hdr:])
			bufPool.Put(bp)
			continue
		default:
		}
		break
	}
	c.recv.Store(&fn)
	for { // a packet queued between the drain and the switch
		select {
		case bp := <-c.q:
			fn((*bp)[c.f.hdr:])
			bufPool.Put(bp)
			continue
		default:
		}
		break
	}
	if c.mx.failed() {
		go onErr(c.mx.err)
	}
}

func (c *rawConn) LocalAddr() net.Addr  { return c.laddr }
func (c *rawConn) RemoteAddr() net.Addr { return c.raddr }

// SetDeadline sets the read deadline; writes on a raw socket do not wait for
// the peer, and the shared socket's write deadline belongs to every link.
func (c *rawConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }
func (c *rawConn) SetReadDeadline(t time.Time) error {
	var v int64
	if !t.IsZero() {
		v = t.UnixNano()
	}
	c.rdl.Store(v)
	select {
	case c.rdlSet <- struct{}{}:
	default:
	}
	return nil
}
func (c *rawConn) SetWriteDeadline(time.Time) error { return nil }

// rawMuxStats is the number of shared dial sockets open and the packets they
// dropped for a full link queue (tests, diagnostics).
func rawMuxStats() (sockets int, dropped uint64) {
	rawMuxMu.Lock()
	defer rawMuxMu.Unlock()
	for _, mx := range rawMuxes {
		sockets++
		dropped += mx.dropped.Load()
	}
	return
}

// ---------------------------------------------------------------------------
// Listen side: one raw socket bound to a local IP (or the wildcard), serving
// every peer link; peers are told apart by (IP, link id).

type rawKey struct {
	ip [4]byte
	id uint16
}

type rawPeer struct {
	addr *Addr
	// replyCtr is this listener's OWN echo-sequence counter for the peer,
	// independent of the request sequence. Each reply takes the next value, so
	// consecutive replies to a peer carry distinct, ascending sequences — a real
	// ping never repeats one, and the old "answer with the request's sequence"
	// made several replies between two requests share one (id,seq).
	replyCtr uint16
	local    net.IP // the local address the peer targeted (wildcard bind)
	seen     time.Time
}

type rawPacketConn struct {
	ipc       *net.IPConn
	tx        *rawTx // send-only socket (nil: send on ipc; see rawTx)
	f         *framer
	wildcard  bool
	guardKey  uint16 // icmp: the echo-guard key (c2s prefix byte) this listener holds
	laddr     *Addr
	closeEcho sync.Once // releases this listener's echo-reply suppression, once

	mu        sync.Mutex
	peers     map[rawKey]*rawPeer
	lastSweep time.Time

	// ReadBatch's arrays (its one reader is the listener's serve loop).
	rb     *mmsg.Batch
	rbufs  [][]byte
	rsizes []int
	rtrunc []bool
	rbuf1  []byte
}

// writeScratch is one batch send's arrays; the listener's carriers send
// concurrently, each with its own from the pool.
type writeScratch struct {
	b    *mmsg.Batch
	pkts [][]byte
	bps  []*[]byte
}

var writeScratchPool = sync.Pool{New: func() any { return &writeScratch{b: mmsg.NewBatch(rawBatch)} }}

func listenRawLinux(kind, addr string, opt Options) (net.PacketConn, error) {
	f, err := newFramer(kind, opt, false)
	if err != nil {
		return nil, err
	}
	host := hostOf(addr)
	var ip net.IP
	if host != "" {
		if ip = net.ParseIP(host).To4(); ip == nil {
			return nil, fmt.Errorf("encap %s: listen address %q is not an IPv4 address", f.kind, host)
		}
	}
	if ip == nil || ip.IsUnspecified() {
		if ip, err = parseBindIP(opt.BindIP); err != nil {
			return nil, err
		}
	}
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	guardKey := f.rxPrefix >> 8 // c2s prefix byte: the kernel's bounce of a request carries it
	if f.kind == KindICMP {
		// The server side receives echo REQUESTS; if the kernel also answered
		// them it would echo every sealed datagram straight back to the peer.
		// Suppress just those replies (echoguard_linux.go), so the server still
		// answers ordinary ping; Close() removes the suppression.
		if err := acquireEchoGuard(guardKey); err != nil {
			return nil, err
		}
	}
	ipc, err := net.ListenIP(rawNetwork(f.proto), &net.IPAddr{IP: ip})
	if err != nil {
		if f.kind == KindICMP {
			releaseEchoGuard(guardKey)
		}
		return nil, rawErr(f.kind, err)
	}
	if err := tuneRawSocket(ipc, f.recvFilter(0, 0), pmtudiscFor(f.kind, false)); err != nil {
		ipc.Close()
		if f.kind == KindICMP {
			releaseEchoGuard(guardKey)
		}
		return nil, fmt.Errorf("encap %s: %w", f.kind, err)
	}
	var bind net.IP
	if !ip.IsUnspecified() {
		bind = ip
	}
	return &rawPacketConn{
		ipc: ipc, f: f, wildcard: ip.IsUnspecified(), guardKey: guardKey,
		tx:    openRawTx(f, ipc, bind),
		laddr: &Addr{IP: ip, Kind: f.kind},
		peers: make(map[rawKey]*rawPeer),
	}, nil
}

func (c *rawPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, _, _, _, err := c.ipc.ReadMsgIP(b, nil)
		if err != nil {
			if softErr(err) {
				continue
			}
			return 0, nil, err
		}
		src, dst, tp, ok := c.f.ipv4Payload(b[:n])
		if !ok {
			continue
		}
		id, _, ok := c.f.parse(tp)
		if !ok {
			continue
		}
		a := c.notePeer(src, dst, id)
		return copy(b, tp[c.f.hdr:]), a, nil
	}
}

// ReadBatch takes the datagrams waiting on the socket in one call (at least
// one: it waits for the first) and hands each transport payload and its peer
// to fn, which must not keep the slice. It returns after one batch, so the
// caller can stop between batches.
func (c *rawPacketConn) ReadBatch(fn func(payload []byte, peer net.Addr)) error {
	if rawNoBatch || !mmsg.Supported {
		buf := c.rbufs1()
		n, a, err := c.ReadFrom(buf)
		if err != nil {
			return err
		}
		fn(buf[:n], a)
		return nil
	}
	rc, err := c.ipc.SyscallConn()
	if err != nil {
		return err
	}
	if c.rb == nil {
		c.rb = mmsg.NewBatch(rawBatch)
		c.rbufs = make([][]byte, rawBatch)
		for i := range c.rbufs {
			c.rbufs[i] = make([]byte, rawBufSize)
		}
		c.rsizes, c.rtrunc = make([]int, rawBatch), make([]bool, rawBatch)
	}
	for {
		n, err := c.rb.Recv(rc, c.rbufs, c.rsizes, c.rtrunc, false, nil, nil)
		if err != nil {
			if softErr(err) {
				continue
			}
			return err
		}
		for i := 0; i < n; i++ {
			if c.rtrunc[i] {
				continue
			}
			src, dst, tp, ok := c.f.ipv4Payload(c.rbufs[i][:c.rsizes[i]])
			if !ok {
				continue
			}
			id, _, ok := c.f.parse(tp)
			if !ok {
				continue
			}
			fn(tp[c.f.hdr:], c.notePeer(src, dst, id))
		}
		return nil
	}
}

// rbufs1 is the one-at-a-time read buffer (no batches).
func (c *rawPacketConn) rbufs1() []byte {
	if c.rbuf1 == nil {
		c.rbuf1 = make([]byte, 65536)
	}
	return c.rbuf1
}

// WriteBatchTo sends several datagrams to one peer in one syscall, each with
// the next reply sequence, from the address the peer targeted.
func (c *rawPacketConn) WriteBatchTo(ps [][]byte, addr net.Addr) error {
	if rawNoBatch || !mmsg.Supported {
		for _, p := range ps {
			if _, err := c.WriteTo(p, addr); err != nil {
				return err
			}
		}
		return nil
	}
	a, ok := addr.(*Addr)
	if !ok || a.IP.To4() == nil {
		return fmt.Errorf("encap %s: bad peer address %v", c.f.kind, addr)
	}
	rc, err := c.ipc.SyscallConn()
	if err != nil {
		return err
	}
	var k rawKey
	copy(k.ip[:], a.IP.To4())
	k.id = a.ID
	ws := writeScratchPool.Get().(*writeScratch)
	defer writeScratchPool.Put(ws)
	for len(ps) > 0 {
		chunk := ps[:min(len(ps), rawBatch)]
		ps = ps[len(chunk):]
		var seq0 uint16
		var local net.IP
		c.mu.Lock()
		if pr := c.peers[k]; pr != nil {
			seq0, local = pr.replyCtr, pr.local
			pr.replyCtr += uint16(len(chunk))
		}
		c.mu.Unlock()
		pkts, bps := ws.pkts[:0], ws.bps[:0]
		tx := c.tx
		if !tx.ok() {
			tx = nil
		}
		for i, p := range chunk {
			bp := bufPool.Get().(*[]byte)
			var pkt []byte
			if tx != nil {
				pkt = tx.frame(bp, c.f, a.ID, seq0+uint16(i), p, a.IP, local)
			} else {
				pkt = c.f.build(*bp, a.ID, seq0+uint16(i), p)
			}
			*bp = pkt[:0]
			bps = append(bps, bp)
			pkts = append(pkts, pkt)
		}
		if tx != nil {
			err := tx.sendAll(pkts, a.IP, local, func(tp []byte) error { return c.legacyWriteTo(tp, a.IP, local) })
			for _, bp := range bps {
				bufPool.Put(bp)
			}
			ws.pkts, ws.bps = pkts[:0], bps[:0]
			if err != nil {
				return err
			}
			continue
		}
		var oob []byte
		if local != nil {
			info := &unix.Inet4Pktinfo{}
			copy(info.Spec_dst[:], local.To4())
			oob = unix.PktInfo4(info)
		}
		err := sendAll(ws.b, rc, pkts, mmsg.Inet4(a.IP.To4(), 0), oob)
		for _, bp := range bps {
			bufPool.Put(bp)
		}
		ws.pkts, ws.bps = pkts[:0], bps[:0]
		if err != nil {
			return err
		}
	}
	return nil
}

// notePeer records a peer so a reply can be addressed to it (its local target
// for a wildcard bind, and the last-seen time for the sweep) and returns its
// (shared, immutable) address. It does NOT store the request's echo sequence:
// replies carry this listener's own counter (rawPeer.replyCtr), not the
// request's, so the request sequence is not needed after demux.
func (c *rawPacketConn) notePeer(src, dst net.IP, id uint16) *Addr {
	var k rawKey
	copy(k.ip[:], src)
	k.id = id
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.peers[k]
	if p == nil {
		if len(c.peers) >= rawPeerMax {
			c.sweepLocked(now, 10*time.Second)
			if len(c.peers) >= rawPeerMax {
				c.peers = make(map[rawKey]*rawPeer)
			}
		}
		p = &rawPeer{addr: &Addr{IP: append(net.IP(nil), src...), ID: id, Kind: c.f.kind}}
		if c.f.obf {
			// icmp: start the reply counter at a random offset so a flow's first
			// reply sequence is not a constant (0) across flows, exactly as the
			// dial side seeds its request sequence randomly.
			p.replyCtr = randReplyCtr()
		}
		c.peers[k] = p
	}
	p.seen = now
	if c.wildcard && !p.local.Equal(dst) {
		p.local = append(net.IP(nil), dst...)
	}
	if now.Sub(c.lastSweep) >= rawPeerSweep {
		c.lastSweep = now
		c.sweepLocked(now, rawPeerTTL)
	}
	return p.addr
}

func (c *rawPacketConn) sweepLocked(now time.Time, ttl time.Duration) {
	for k, p := range c.peers {
		if now.Sub(p.seen) > ttl {
			delete(c.peers, k)
		}
	}
}

func (c *rawPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	a, ok := addr.(*Addr)
	if !ok || a.IP.To4() == nil {
		return 0, fmt.Errorf("encap %s: bad peer address %v", c.f.kind, addr)
	}
	var k rawKey
	copy(k.ip[:], a.IP.To4())
	k.id = a.ID
	var seq uint16
	var local net.IP
	c.mu.Lock()
	if pr := c.peers[k]; pr != nil {
		// Each reply takes this listener's next counter for the peer, not the
		// request's sequence, so consecutive replies get distinct, ascending
		// (id,seq) pairs. build() stamps the s2c directional prefix over the
		// counter's high byte, so the reply still carries the reply direction's
		// prefix — the echo guard drops only the kernel's bounce of our own
		// requests (c2s prefix), never these genuine replies.
		seq, local = pr.replyCtr, pr.local
		pr.replyCtr++
	}
	c.mu.Unlock()

	bp := bufPool.Get().(*[]byte)
	var pkt []byte
	var err error
	if tx := c.tx; tx.ok() {
		pkt = tx.frame(bp, c.f, a.ID, seq, p, a.IP, local)
		err = tx.sendAll([][]byte{pkt}, a.IP, local, func(tp []byte) error { return c.legacyWriteTo(tp, a.IP, local) })
	} else {
		pkt = c.f.build(*bp, a.ID, seq, p)
		err = c.legacyWriteTo(pkt, a.IP, local)
	}
	*bp = pkt[:0]
	bufPool.Put(bp)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// legacyWriteTo sends one framed transport packet on the receive socket (the
// kernel builds the IP header), from local when set: answer from the address
// the peer targeted — its connected socket accepts nothing else, and neither
// would a stateful middlebox. A soft error drops it and counts it. Also
// rawTx's fallback.
func (c *rawPacketConn) legacyWriteTo(tp []byte, ip, local net.IP) error {
	dst := &net.IPAddr{IP: ip}
	var err error
	if local != nil {
		info := &unix.Inet4Pktinfo{}
		copy(info.Spec_dst[:], local.To4())
		_, _, err = c.ipc.WriteMsgIP(tp, unix.PktInfo4(info), dst)
	} else {
		_, err = c.ipc.WriteToIP(tp, dst)
	}
	if err != nil {
		if !softErr(err) {
			return err
		}
		sendRefused.Add(1)
	}
	return nil
}

func (c *rawPacketConn) Close() error {
	err := c.ipc.Close()
	c.tx.close()
	if c.f.kind == KindICMP {
		c.closeEcho.Do(func() { releaseEchoGuard(c.guardKey) })
	}
	return err
}
func (c *rawPacketConn) LocalAddr() net.Addr                { return c.laddr }
func (c *rawPacketConn) SetDeadline(t time.Time) error      { return c.ipc.SetDeadline(t) }
func (c *rawPacketConn) SetReadDeadline(t time.Time) error  { return c.ipc.SetReadDeadline(t) }
func (c *rawPacketConn) SetWriteDeadline(t time.Time) error { return c.ipc.SetWriteDeadline(t) }

// ---------------------------------------------------------------------------

const echoIgnorePath = "/proc/sys/net/ipv4/icmp_echo_ignore_all"

// icmp-echo-ignore is refcounted across the ICMP listeners in this process: the
// first to open sets net.ipv4.icmp_echo_ignore_all=1 (so the kernel stops
// answering the echo requests the tunnel carries), and the last to close puts
// it back — but only to a value this process itself changed. A host where the
// operator had already disabled echo replies (or does so while we run) is left
// untouched. echoWeSet records that 0->1 transition.
var (
	echoMu    sync.Mutex
	echoRefs  int
	echoWeSet bool
)

// acquireEchoIgnore takes one ICMP listener's hold on icmp_echo_ignore_all,
// turning it on if it was off. Unable to set it is an error that says what to
// do. Every successful call must be paired with exactly one releaseEchoIgnore.
func acquireEchoIgnore() error {
	echoMu.Lock()
	defer echoMu.Unlock()
	if echoRefs == 0 {
		already := false
		if b, err := os.ReadFile(echoIgnorePath); err == nil && strings.TrimSpace(string(b)) == "1" {
			already = true
		}
		if !already {
			if err := os.WriteFile(echoIgnorePath, []byte("1\n"), 0o644); err != nil {
				return fmt.Errorf("encap icmp: the kernel would answer the tunnel's echo requests itself; "+
					"set net.ipv4.icmp_echo_ignore_all=1 (sysctl -w) or run as root: %w", err)
			}
			echoWeSet = true
		}
	}
	echoRefs++
	return nil
}

// releaseEchoIgnore drops one hold; when the last ICMP listener in this process
// goes and we were the one that turned echo off, turn it back on. A best-effort
// write: if it fails there is nothing useful to do and the socket is closing.
func releaseEchoIgnore() {
	echoMu.Lock()
	defer echoMu.Unlock()
	if echoRefs == 0 {
		return
	}
	echoRefs--
	if echoRefs == 0 && echoWeSet {
		_ = os.WriteFile(echoIgnorePath, []byte("0\n"), 0o644)
		echoWeSet = false
	}
}

// EchoIgnored reports whether the kernel's automatic echo replies are off in
// this network namespace (for status/diagnostics).
func EchoIgnored() bool {
	b, err := os.ReadFile(echoIgnorePath)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// sendRefused counts the datagrams this process's raw sockets dropped because
// the kernel refused them with a soft error (see softErr) — a too-big
// datagram, an ICMP error reported on the socket. The peer counts each as path
// loss (its wire sequence was taken). A full device queue usually does NOT
// show here: without IP_RECVERR the kernel reports that drop as a success
// (it is in the host's Ip OutDiscards instead).
var sendRefused atomic.Uint64

// SendRefused is sendRefused (see there), for the status.
func SendRefused() uint64 { return sendRefused.Load() }

// softErr reports whether a raw-socket read/write error is a transient ICMP
// error the kernel reports on a CONNECTED raw socket (dest/host/net
// unreachable, port unreachable, a reset, a fragmentation-needed) rather than
// the socket dying. A connected raw socket delivers such an error — triggered
// by ANY host on the path, or a spoofed packet — as the result of the NEXT
// read or write; treating it as fatal would let one ICMP packet tear the
// carrier down. It is dropped and the operation retried instead.
func softErr(err error) bool {
	return errors.Is(err, unix.EHOSTUNREACH) || errors.Is(err, unix.ECONNREFUSED) ||
		errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.ECONNRESET) ||
		errors.Is(err, unix.EHOSTDOWN) || errors.Is(err, unix.ENETDOWN) ||
		errors.Is(err, unix.EMSGSIZE) || errors.Is(err, unix.ENOBUFS) ||
		errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.ENONET) ||
		errors.Is(err, unix.EPROTO)
}

func rawErr(kind string, err error) error {
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return fmt.Errorf("encap %s: a raw socket needs root or CAP_NET_RAW: %w", kind, err)
	}
	return fmt.Errorf("encap %s: %w", kind, err)
}
