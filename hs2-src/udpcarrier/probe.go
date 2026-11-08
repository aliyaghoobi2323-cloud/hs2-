package udpcarrier

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"hash"
	"net"
	"sync"
	"time"

	"crypto/hmac"

	"golang.org/x/crypto/blake2s"
)

// The probe is a lightweight reachability-and-loss check that is completely
// separate from the data path: its own tiny request/echo protocol on the same
// UDP port, gated by the shared secret so the server is not an open reflector
// and a censor cannot elicit a reply without the key. It exists so the
// transport selector can decide UDP-vs-TCP BEFORE committing user traffic, and
// so it can keep deciding without disturbing a live carrier.
//
// A probe packet is:
//
//	[magic:4]["hs2P"] [nonce:8] [seq:4] [sendNanos:8] [tag:16]
//
// tag = HMAC(shared, "hs2-udp-probe-v1" || nonce || seq || sendNanos). The
// server echoes the request verbatim (a valid tag is required first), so the
// client measures per-packet loss and RTT. The nonce makes each probe run
// distinct; the tag makes the exchange unforgeable and unresponsive to anyone
// without the secret.

const (
	probeMagicLen = 4
	probeNonceLen = 8
	probeHdrLen   = probeMagicLen + probeNonceLen + 4 + 8 // magic+nonce+seq+sendNanos
	probeTagLen   = 16
	probeLen      = probeHdrLen + probeTagLen
)

var probeMagic = [probeMagicLen]byte{'h', 's', '2', 'P'}

func probeTag(shared, body []byte) []byte {
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, shared)
	m.Write([]byte("hs2-udp-probe-v1"))
	m.Write(body)
	return m.Sum(nil)[:probeTagLen]
}

func buildProbe(shared, nonce []byte, seq uint32, sendNanos int64) []byte {
	p := make([]byte, probeLen)
	copy(p[0:probeMagicLen], probeMagic[:])
	copy(p[probeMagicLen:probeMagicLen+probeNonceLen], nonce)
	binary.BigEndian.PutUint32(p[probeMagicLen+probeNonceLen:], seq)
	binary.BigEndian.PutUint64(p[probeMagicLen+probeNonceLen+4:], uint64(sendNanos))
	copy(p[probeHdrLen:], probeTag(shared, p[:probeHdrLen]))
	return p
}

// validProbe reports whether p is a well-formed, authentic probe packet.
func validProbe(shared, p []byte) bool {
	if len(p) != probeLen {
		return false
	}
	if string(p[:probeMagicLen]) != string(probeMagic[:]) {
		return false
	}
	return macEqual(p[probeHdrLen:], probeTag(shared, p[:probeHdrLen]))
}

// ProbeResult summarises one probe run.
type ProbeResult struct {
	Sent      int
	Received  int
	Loss      float64       // fraction lost, 0..1
	RTTMin    time.Duration //
	RTTMedian time.Duration
	Reachable bool // at least one echo came back
}

// Probe sends n small probes to addr, spaced by interval, and reports loss and
// RTT. It opens its own socket and never touches a data carrier.
func Probe(ctx context.Context, addr string, shared []byte, n int, interval, timeout time.Duration) (ProbeResult, error) {
	return ProbeFrom(ctx, addr, "", shared, n, interval, timeout)
}

// ProbeFrom is Probe sending from a chosen local source IP, so the probe
// measures the same path the carrier will actually use.
func ProbeFrom(ctx context.Context, addr, bindIP string, shared []byte, n int, interval, timeout time.Duration) (ProbeResult, error) {
	if n <= 0 {
		n = 20
	}
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return ProbeResult{}, err
	}
	laddr, err := localUDPAddr(bindIP)
	if err != nil {
		return ProbeResult{}, err
	}
	conn, err := net.DialUDP("udp", laddr, ua)
	if err != nil {
		return ProbeResult{}, err
	}
	defer conn.Close()

	nonce := make([]byte, probeNonceLen)
	rand.Read(nonce)

	sentAt := make([]time.Time, n)
	var rtts []time.Duration
	received := 0
	done := make(chan struct{})

	// Receiver: match echoes to their sequence and time them.
	go func() {
		defer close(done)
		buf := make([]byte, 128)
		deadline := time.Now().Add(time.Duration(n)*interval + timeout)
		for {
			if time.Now().After(deadline) {
				return
			}
			conn.SetReadDeadline(time.Now().Add(timeout))
			m, err := conn.Read(buf)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if time.Now().After(deadline) {
					return
				}
				continue
			}
			if m != probeLen || !validProbe(shared, buf[:m]) {
				continue
			}
			seq := binary.BigEndian.Uint32(buf[probeMagicLen+probeNonceLen:])
			if int(seq) >= n {
				continue
			}
			if !sentAt[seq].IsZero() {
				rtts = append(rtts, time.Since(sentAt[seq]))
				received++
				if received >= n {
					return
				}
			}
		}
	}()

	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			return ProbeResult{}, ctx.Err()
		default:
		}
		sentAt[i] = time.Now()
		conn.Write(buildProbe(shared, nonce, uint32(i), sentAt[i].UnixNano()))
		time.Sleep(interval)
	}
	select {
	case <-done:
	case <-time.After(timeout):
	case <-ctx.Done():
		return ProbeResult{}, ctx.Err()
	}
	conn.SetReadDeadline(time.Now())
	<-done

	res := ProbeResult{Sent: n, Received: received, Reachable: received > 0}
	res.Loss = float64(n-received) / float64(n)
	if len(rtts) > 0 {
		res.RTTMin, res.RTTMedian = minMedian(rtts)
	}
	return res, nil
}

func minMedian(d []time.Duration) (min, median time.Duration) {
	// insertion sort: probe counts are tiny
	s := append([]time.Duration(nil), d...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s[0], s[len(s)/2]
}

// ProbeResponder answers probe packets on a shared UDP socket. The listener
// runs one so the same port serves both carriers and probes; a standalone one
// can also be created for tests. readFrom/writeTo are the socket operations.
//
// An authentic probe is echoed at most once, and only while its sendNanos is
// within probeWindow of our clock. Without that, one captured probe could be
// replayed forever from any (spoofed) source — an unlimited reflector, and an
// active-probing oracle (replay an observed probe, get an echo: an hs2 server).
// The client side is unchanged (it always stamped sendNanos with its wall clock
// and never repeats a nonce/seq), so this needs no wire change; the window is
// the handshake's own clock tolerance (core tsWindow), so a client whose clock
// is too far off to complete a handshake anyway is the only one refused.
type probeResponder struct {
	shared []byte

	mu        sync.Mutex
	cur, prev map[probeKey]struct{} // two generations, each kept >= probeMemTTL
	rotated   time.Time
}

type probeKey [probeNonceLen + 4]byte // nonce || seq

const (
	probeWindow = 90 * time.Second // |our clock - sendNanos| accepted
	probeMemTTL = 2 * probeWindow  // a probe is remembered at least this long
	probeMemCap = 1 << 16          // entries across both generations
)

func newProbeResponder(shared []byte) *probeResponder {
	return &probeResponder{shared: shared, cur: map[probeKey]struct{}{}, prev: map[probeKey]struct{}{}}
}

// handle echoes p if it is a valid, fresh, first-seen probe; returns true if
// p is an authentic probe at all (consumed, echoed or not), so the caller does
// not treat it as a handshake.
func (pr *probeResponder) handle(p []byte, echo func([]byte)) bool {
	if !validProbe(pr.shared, p) {
		return false
	}
	if pr.fresh(p, time.Now()) {
		echo(append([]byte(nil), p...))
	}
	return true
}

// fresh reports whether an authentic probe is inside the time window and seen
// for the first time, and records it. A full memory refuses (fails closed):
// only key holders can add entries, at a few dozen per dial.
func (pr *probeResponder) fresh(p []byte, now time.Time) bool {
	sent := int64(binary.BigEndian.Uint64(p[probeMagicLen+probeNonceLen+4:]))
	if d := now.UnixNano() - sent; d > int64(probeWindow) || d < -int64(probeWindow) {
		return false
	}
	var k probeKey
	copy(k[:], p[probeMagicLen:probeHdrLen-8])
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.cur == nil {
		pr.cur, pr.prev = map[probeKey]struct{}{}, map[probeKey]struct{}{}
	}
	if pr.rotated.IsZero() {
		pr.rotated = now
	} else if now.Sub(pr.rotated) >= probeMemTTL {
		pr.prev, pr.cur = pr.cur, make(map[probeKey]struct{}, len(pr.cur))
		pr.rotated = now
	}
	if _, ok := pr.cur[k]; ok {
		return false
	}
	if _, ok := pr.prev[k]; ok {
		return false
	}
	if len(pr.cur)+len(pr.prev) >= probeMemCap {
		return false
	}
	pr.cur[k] = struct{}{}
	return true
}

func (pr *probeResponder) size() int {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return len(pr.cur) + len(pr.prev)
}
