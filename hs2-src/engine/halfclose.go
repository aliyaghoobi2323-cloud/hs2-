package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// hc183: TCP half-close through the tunnel.
//
// A user app that half-closes (shutdown(SHUT_WR): curl --data then FIN,
// nc -N, SMTP/IMAP and RPC clients, uploaders) still reads the response. The
// relay used to end the whole connection on the first direction's EOF: the
// download still in flight was cut and handed to the app with a clean FIN.
//
// Now (both sides announce capExtHalfClose) an EOF on one direction ends only
// that direction:
//   - Iran, user→tunnel EOF: the stream's CloseWrite (an mConn FIN record at
//     its offset; a raw first sub or a one-stream path: the smux stream's
//     cmdFIN, Stream.CloseWrite) — the download keeps flowing to the user.
//   - Exit, target→tunnel EOF: the same toward Iran; Iran then CloseWrites
//     the user socket (TCPConn.CloseWrite) and keeps relaying the upload.
//   - Exit, tunnel→target EOF (the user's FIN): CloseWrite on the target.
//
// The relay ends (both closed, a plain FIN) once both directions ended with
// EOF. Abnormal ends keep fx170's rule: a direction cut before its peer's
// EOF resets (RST) the local side; in the half-closed state a cut of the
// remaining direction resets too.
//
// Bound: a half-closed connection whose remaining direction moves nothing
// for halfCloseIdle is reset (local RST, stream aborted). It reuses udpIdle
// (stream.go: the idle limit of a relayed UDP flow, 2 min): the tunnel keeps
// no other idle limit on user connections, and a half-closed one is the case
// most likely to be forgotten by a peer that ignores the FIN. And at most
// halfCloseMax relays wait half-closed at once per process; past that the
// plain close at once (see halfCloseMax).
//
// Wire: the FIN itself is not new (mConn FIN records carry their offset; smux
// has cmdFIN per stream). What an older peer cannot do is keep writing after
// the FIN it received, or keep reading after the FIN it sent: it ends the
// whole connection on the first EOF, and its smux Write gives up waiting for
// window once the peer's FIN came. So each side half-closes only toward a
// peer that announced capExtHalfClose (ext bit13; bits 11/12 are taken by the
// unmerged rescue-rst branches); otherwise the old full close.
//
// HS2_HALF_CLOSE=0 turns it off (and stops announcing the bit).
const capExtHalfClose = 1 << 13

var halfCloseOn atomic.Bool

func init() { halfCloseOn.Store(envDefault("HS2_HALF_CLOSE") != "0") }

// halfCloseIdle bounds a half-closed connection (see above); atomic so tests
// can shorten it while older relays still run.
var halfCloseIdle, halfClosePoll atomic.Int64

func init() {
	halfCloseIdle.Store(int64(udpIdle))
	halfClosePoll.Store(int64(5 * time.Second))
}

// hcMode: relayStreamHC's half-close mode.
type hcMode uint8

const (
	hcOff  hcMode = iota // the old full close (the peer did not announce it)
	hcEdge               // Iran: the user's side
	hcExit               // the exit: the target's side
)

func hcIf(ok bool, m hcMode) hcMode {
	if ok {
		return m
	}
	return hcOff
}

// idle: the edge decides first (halfCloseIdle); the exit waits half as long
// again. On the one-stream path and a raw first sub nothing on the wire says
// "reset" (smux has no RST frame): an exit giving up first would hand the
// user a clean FIN, the edge giving up first resets the user — and its abort
// or close then ends the exit's side.
func (m hcMode) idle() time.Duration {
	d := time.Duration(halfCloseIdle.Load())
	if m == hcExit {
		d += d / 2
	}
	return d
}

// halfClose reports whether the other server announced half-close.
func (pi *peerInfo) halfClose() bool {
	return halfCloseOn.Load() && pi != nil && pi.V2 && pi.Ext&capExtHalfClose != 0
}

// halfCloseLink: the peer behind a link meter announced half-close.
func halfCloseLink(mtr *linkMeter) bool {
	return mtr != nil && mtr.peerInfo.Load().halfClose()
}

// halfCloseStream: the one-stream path's stream (edge) rides a link whose
// exit announced half-close.
func halfCloseStream(st io.ReadWriteCloser) bool {
	if cs, ok := st.(*countedStream); ok && cs.link != nil {
		return halfCloseLink(cs.link.mtr)
	}
	return false
}

type closeWriter interface{ CloseWrite() error }

type halfOpener interface{ SetHalfOpen(bool) }

// CloseWrite (hc183) half-closes the exit's target.
func (t exitTarget) CloseWrite() error {
	if cw, ok := t.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return errNoCloseWrite
}

var errNoCloseWrite = &net.OpError{Op: "closewrite", Err: io.ErrClosedPipe}

// hc183 counters: relays that half-closed one direction, and half-closed
// relays reset for halfCloseIdle.
var (
	halfCloses     atomic.Uint64
	halfCloseIdles atomic.Uint64
	halfOpenNow    atomic.Int64 // relays waiting half-closed right now
	halfCloseOver  atomic.Uint64
	halfCloseGone  atomic.Uint64 // hc183b: ended because the peer dropped its side
	// hc213: an EOF that came with the peer's "dropped" mark (closed without
	// CloseWrite): relayed as a cut (RST), not a half-close
	halfCloseAborts atomic.Uint64
)

// halfCloseMax (hc183 bound): at most this many relays wait half-closed at
// once per process (HS2_HALF_CLOSE_MAX, default 16384, 0 = no cap). Iran cannot
// tell a user's close() from SHUT_WR, so every user close waits for the
// target's FIN; a target that ignores it holds the relay (fds, goroutines,
// a pool stream on both sides) for halfCloseIdle. Over the cap a relay whose
// first direction ended closes at once with a FIN — the pre-hc183 behaviour
// — so the cost stays bounded whatever the targets do (halfCloseOver).
// audit246 L7: 4096 -> 16384 (halfCloseMaxDefault). Over the cap a relay
// now ends with a reset (hc213 abandon mark), not a FIN; lab hc183 held
// ~530-730 half-open per side at today's load, so 4x (45-60k users) could
// bind 4096 routinely. ~50 KB, 1 fd and ~2 goroutines each: the cap costs
// at most ~0.8 GB per side.
var halfCloseMax atomic.Int64

const halfCloseMaxDefault = 16384

func init() {
	n := int64(halfCloseMaxDefault)
	if v, err := strconv.ParseInt(envDefault("HS2_HALF_CLOSE_MAX"), 10, 64); err == nil && v >= 0 {
		n = v
	}
	halfCloseMax.Store(n)
}

// halfOpenAdmit takes a half-open slot (halfOpenNow; the caller gives it back)
// or reports the cap reached.
func halfOpenAdmit() bool {
	max := halfCloseMax.Load()
	for {
		n := halfOpenNow.Load()
		if max > 0 && n >= max {
			halfCloseOver.Add(1)
			return false
		}
		if halfOpenNow.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// HalfCloseStats is the hc183 counters for the pprof endpoint
// (/debug/hs2halfclose): relays half-closed so far, of them reset for
// halfCloseIdle, relays waiting half-closed right now, and relays closed at
// once because halfCloseMax were waiting.
func HalfCloseStats() string {
	return fmt.Sprintf("halfCloses=%d halfCloseIdles=%d halfOpen=%d halfCloseOver=%d halfCloseGone=%d halfCloseAborts=%d rawHCUpgradeSkips=%d rawHCUpgradeLate=%d",
		halfCloses.Load(), halfCloseIdles.Load(), halfOpenNow.Load(), halfCloseOver.Load(), halfCloseGone.Load(), halfCloseAborts.Load(), rawHCUpgradeSkips.Load(), rawHCUpgradeLate.Load())
}

// SetLinger (hc183) lets relayStream reset the exit's target (a half-closed
// relay cut or idle).
func (t exitTarget) SetLinger(sec int) error {
	if tc, ok := t.Conn.(*net.TCPConn); ok {
		return tc.SetLinger(sec)
	}
	return nil
}

// SetHalfOpen (hc183): the peer announced half-close. A raw first sub's
// stream then keeps writing after the peer's FIN, and CloseWrite / a raw
// EOF end one direction only.
func (m *mConn) SetHalfOpen(on bool) {
	m.rawHC.Store(on)
	if m.raw1 != nil {
		if h, ok := m.raw1.rw.(halfOpener); ok {
			h.SetHalfOpen(on)
		}
	}
}

// rawCloseWrite: CloseWrite while this side still writes raw. done=false:
// no longer raw (the caller sends a FIN record).
func (m *mConn) rawCloseWrite() (done bool, err error) {
	if !m.rawHC.Load() {
		return true, errors.ErrUnsupported // old peer: the relay closes all
	}
	cw, ok := m.raw1.rw.(closeWriter)
	if !ok {
		return true, errors.ErrUnsupported
	}
	m.wmu.Lock() // no raw write in flight; no switch to records meanwhile
	defer m.wmu.Unlock()
	if !m.rawW.Load() {
		return false, nil
	}
	m.mu.Lock()
	if m.handshaking || m.lightOpening || m.stOpening {
		// hc213 (audit211 M1): an upgrade is under way — the peer may get
		// its UPGRADE after this FIN (a protocol error at the exit, its RST
		// overtaking the raw tail), or this side's boundary would follow its
		// own FIN. Not now: CloseWrite fails (rawW still set) and the relay
		// takes the old full close.
		m.mu.Unlock()
		rawHCUpgradeSkips.Add(1)
		return false, nil
	}
	if m.ended {
		err = m.writeErrLocked()
		m.mu.Unlock()
		return true, err
	}
	if m.wclosed {
		m.mu.Unlock()
		return true, nil
	}
	m.wclosed = true
	m.sendOff = uint64(m.rawSent.Load())
	m.mu.Unlock()
	m.tr.ev("raw: half-close at %d", m.rawSent.Load())
	if ct, ok := m.raw1.rw.(closeWriteTimeouter); ok {
		// hc213 (audit211 L1): wmu is held — on a dark link the FIN must
		// not wait for the session's death; past rawFinTimeout CloseWrite
		// fails and the relay takes the full close (the stream then counts
		// as abandoned: smux CloseWriteTimeout)
		return true, ct.CloseWriteTimeout(rawFinTimeout)
	}
	return true, cw.CloseWrite()
}

// rawFinTimeout (hc213): the longest a raw half-close's FIN waits for the
// smux writer while rawCloseWrite holds mConn.wmu.
const rawFinTimeout = 3 * time.Second

type closeWriteTimeouter interface {
	CloseWriteTimeout(time.Duration) error
}

// rawHCUpgradeSkips (hc213): raw half-closes refused because an upgrade was
// under way (the relay closed in full instead).
var rawHCUpgradeSkips atomic.Uint64

// CloseWriteTimeout (hc213) reaches a raw first sub's smux stream.
func (s *mconnSub) CloseWriteTimeout(d time.Duration) error {
	if ct, ok := s.stream.(closeWriteTimeouter); ok {
		return ct.CloseWriteTimeout(d)
	}
	return s.CloseWrite()
}

// rawHalfFin: the raw reader read the stream's EOF and the peer announced
// half-close: that is the peer's FIN at the raw bytes read; this side keeps
// writing raw. false: not now (handshaking, no longer writing raw, ended):
// the old end.
func (m *mConn) rawHalfFin() bool {
	if !m.rawHC.Load() {
		return false
	}
	m.mu.Lock()
	if m.ended || m.closing || !m.rawW.Load() || m.handshaking {
		m.mu.Unlock()
		return false
	}
	m.peerFin = true
	m.rawR.Store(false)
	m.mu.Unlock()
	off := uint64(m.rawRecvd.Load())
	r := &m.r
	r.mu.Lock()
	if r.next < off {
		r.next = off
	}
	if !r.fin {
		r.fin, r.finOff = true, off
	}
	r.lastProgress = time.Now()
	r.cond.Broadcast()
	r.mu.Unlock()
	m.tr.ev("raw: peer half-closed at %d", off)
	return true
}

// CloseWrite / SetHalfOpen (hc183) reach a raw first sub's smux stream.
func (s *mconnSub) CloseWrite() error {
	if cw, ok := s.stream.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (s *mconnSub) SetHalfOpen(on bool) {
	if h, ok := s.stream.(halfOpener); ok {
		h.SetHalfOpen(on)
	}
}

// hc183b: the peer gave up a half-closed connection (its halfCloseIdle, a
// reset of its user) after this side read its FIN. Records say so (an RST
// record ends the mConn: GetDieCh); a raw first sub and the one-stream path
// have only the smux stream, whose peer then sends a second FIN on Close
// (smux Stream.PeerGone). Without it this side kept writing into nothing
// for its own, longer halfCloseIdle.
type peerGoner interface{ PeerGone() <-chan struct{} }

// halfGone: closed when the peer dropped st after its FIN (nil: no signal).
func halfGone(st io.ReadWriteCloser) <-chan struct{} {
	if g, ok := st.(peerGoner); ok {
		return g.PeerGone()
	}
	return nil
}

// hc213: the peer's "abandoned" mark (smux Stream.PeerAbandoned): it closed
// the stream without a CloseWrite. Unlike PeerGone (also closed by the
// second FIN that follows a normal end after CloseWrite) it never fires on a
// complete exchange.
type peerAbandoner interface{ PeerAbandoned() <-chan struct{} }

// halfGoneNow (hc213): st's peer already abandoned it (non-blocking; a mark
// that no longer applies — records carry the connection now — is ignored).
func halfGoneNow(st io.ReadWriteCloser) bool {
	a, ok := st.(peerAbandoner)
	if !ok {
		return false
	}
	g := a.PeerAbandoned()
	if g == nil {
		return false
	}
	select {
	case <-g:
	default:
		return false
	}
	if v, ok := st.(interface{ goneValid() bool }); ok && !v.goneValid() {
		return false
	}
	return true
}

// PeerGone: the raw first sub's smux stream (its records end the mConn
// itself). Only while this side writes raw: once records carry the
// connection (a rescue), the raw stream's end says nothing about it.
func (m *mConn) PeerGone() <-chan struct{} {
	if m.raw1 != nil && m.rawW.Load() {
		if g, ok := m.raw1.rw.(peerGoner); ok {
			return g.PeerGone()
		}
	}
	return nil
}

// PeerAbandoned (hc213): as PeerGone, the raw first sub's mark.
func (m *mConn) PeerAbandoned() <-chan struct{} {
	if m.raw1 != nil && m.rawW.Load() {
		if a, ok := m.raw1.rw.(peerAbandoner); ok {
			return a.PeerAbandoned()
		}
	}
	return nil
}

func (s *mconnSub) PeerAbandoned() <-chan struct{} {
	if a, ok := s.stream.(peerAbandoner); ok {
		return a.PeerAbandoned()
	}
	return nil
}

// goneValid: checked when PeerGone fires (a rescue may have moved the
// connection to records meanwhile).
func (m *mConn) goneValid() bool { return m.rawW.Load() }

func (s *mconnSub) PeerGone() <-chan struct{} {
	if g, ok := s.stream.(peerGoner); ok {
		return g.PeerGone()
	}
	return nil
}
