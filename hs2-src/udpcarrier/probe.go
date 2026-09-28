package udpcarrier

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"hash"
	"net"
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
	conn, err := net.DialUDP("udp", nil, ua)
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
type probeResponder struct {
	shared []byte
}

// handle echoes p if it is a valid probe; returns true if it consumed the
// packet (so the caller does not treat it as a handshake).
func (pr *probeResponder) handle(p []byte, echo func([]byte)) bool {
	if !validProbe(pr.shared, p) {
		return false
	}
	echo(append([]byte(nil), p...))
	return true
}
