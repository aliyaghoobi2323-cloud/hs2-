package encap

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

var rawKinds = []string{KindICMP, KindGRE, KindIPIP, KindIPX}

func testFramers(t *testing.T, kind string, key []byte) (cli, srv *framer) {
	t.Helper()
	var err error
	if cli, err = newFramer(kind, Options{Key: key}, true); err != nil {
		t.Fatalf("%s dial framer: %v", kind, err)
	}
	if srv, err = newFramer(kind, Options{Key: key}, false); err != nil {
		t.Fatalf("%s listen framer: %v", kind, err)
	}
	return cli, srv
}

// Every raw kind round-trips a payload in both directions, the header is
// exactly Overhead bytes, and the link id (and icmp sequence) survive.
func TestRawFrameRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xa5, 0x5a, 0x00, 0xff}, 333)
	for _, k := range rawKinds {
		cli, srv := testFramers(t, k, []byte("key-1"))
		for _, dir := range []struct {
			name     string
			from, to *framer
		}{{"c2s", cli, srv}, {"s2c", srv, cli}} {
			pkt := dir.from.build(nil, 0xbeef, 0x1234, payload)
			if got, want := len(pkt)-len(payload), Overhead(k); got != want {
				t.Fatalf("%s %s: header %d bytes, Overhead says %d", k, dir.name, got, want)
			}
			id, seq, ok := dir.to.parse(pkt)
			if !ok {
				t.Fatalf("%s %s: receiver rejected a valid packet", k, dir.name)
			}
			if id != 0xbeef {
				t.Fatalf("%s %s: id %#x", k, dir.name, id)
			}
			if k == KindICMP {
				// The wire sequence is the sender's keyed directional prefix in
				// the high byte and the counter (here 0x1234's low byte) in the low.
				if seq&obfSeqCounterMask != 0x1234&obfSeqCounterMask {
					t.Fatalf("%s %s: counter byte not preserved, seq %#04x", k, dir.name, seq)
				}
				if !obfSeqHasPrefix(seq, dir.from.txPrefix) {
					t.Fatalf("%s %s: wrong directional prefix, seq %#04x", k, dir.name, seq)
				}
			}
			if !bytes.Equal(pkt[dir.to.hdr:], payload) {
				t.Fatalf("%s %s: payload corrupted", k, dir.name)
			}
			// A side never accepts its own direction's packets (a host echoing
			// them back, or loopback delivering them to the sender too).
			if _, _, ok := dir.from.parse(pkt); ok {
				t.Fatalf("%s %s: sender accepted its own packet", k, dir.name)
			}
		}
	}
}

// The ICMP echo is well-formed: request from the dialer, reply from the
// listener, code 0, valid Internet checksum — what a middlebox checks.
func TestRawFrameICMPWellFormed(t *testing.T) {
	cli, srv := testFramers(t, KindICMP, nil)
	for _, c := range []struct {
		f    *framer
		typ  byte
		name string
	}{{cli, 8, "request"}, {srv, 0, "reply"}} {
		for _, n := range []int{0, 1, 2, 3, 1300, 1301} {
			pkt := c.f.build(nil, 7, 9, bytes.Repeat([]byte{0xee}, n))
			if pkt[0] != c.typ || pkt[1] != 0 {
				t.Fatalf("%s: type/code %d/%d", c.name, pkt[0], pkt[1])
			}
			if inetChecksum(pkt) != 0 {
				t.Fatalf("%s len %d: checksum does not verify", c.name, n)
			}
		}
	}
}

// GRE is RFC 2890 with the key present: version 0, K bit, IPv4 protocol type.
func TestRawFrameGREWellFormed(t *testing.T) {
	cli, _ := testFramers(t, KindGRE, nil)
	pkt := cli.build(nil, 3, 0, []byte("x"))
	if binary.BigEndian.Uint16(pkt[0:]) != 0x2000 || binary.BigEndian.Uint16(pkt[2:]) != 0x0800 {
		t.Fatalf("gre header % x", pkt[:4])
	}
}

// protoOf returns the IP protocol number a real framer of this kind uses (icmp
// 1, gre 47, ipip 4, ipx 253), so the magic tests derive magics for the SAME
// (kind, proto) the sockets actually use — not one fixed constant for every kind.
func protoOf(t *testing.T, kind string) int {
	t.Helper()
	f, err := newFramer(kind, Options{}, true)
	if err != nil {
		t.Fatalf("%s framer: %v", kind, err)
	}
	return f.proto
}

// rawMagicKinds are the raw encapsulations that still carry a keyed framing
// magic (icmp moved its discriminator into the keyed echo-sequence prefix).
var rawMagicKinds = []string{KindGRE, KindIPIP, KindIPX}

// A different shared secret means a different keyed discriminator: a second hs2
// (or anything else) using the same protocol on the host is discarded at the
// framing. For the magic kinds this is a full 16-bit magic; for icmp it is the
// 8-bit sequence prefix (a cheap first cut — the AEAD is the real boundary), so
// a foreign packet is rejected unless its prefix byte collides (1/256).
func TestRawFrameKeySeparation(t *testing.T) {
	for _, k := range rawMagicKinds {
		a, _ := testFramers(t, k, []byte("key-A"))
		_, b := testFramers(t, k, []byte("key-B"))
		if _, _, ok := b.parse(a.build(nil, 1, 1, []byte("hi"))); ok {
			t.Fatalf("%s: a packet keyed with another secret was accepted", k)
		}
	}
	// icmp: different keys give different directional prefixes, and a foreign
	// packet whose prefix byte differs is rejected at the framing. Deterministic
	// for these fixed keys (asserted), with the 1/256-collision case skipped.
	aC2s, _ := obfSeqPrefixes([]byte("key-A"))
	bC2s, _ := obfSeqPrefixes([]byte("key-B"))
	if aC2s == bC2s {
		t.Skip("icmp prefix collision for the chosen keys (1/256); separation is best-effort at the framing")
	}
	a, _ := testFramers(t, KindICMP, []byte("key-A"))
	_, b := testFramers(t, KindICMP, []byte("key-B"))
	if _, _, ok := b.parse(a.build(nil, 1, 1, []byte("hi"))); ok {
		t.Fatal("icmp: a packet keyed with another secret was accepted despite a different prefix")
	}

	// For each MAGIC kind, at the REAL protocol its socket uses, assert the
	// properties the framing relies on (deterministic for these fixed inputs):
	//   - the two direction magics differ, so a side never mistakes its own
	//     packet (or an echo of it) for the peer's;
	//   - the magic is stable for a given (key, kind, proto);
	//   - changing ONLY the key changes the magic.
	for _, k := range rawMagicKinds {
		proto := protoOf(t, k)
		c2sA, s2cA := framingMagics([]byte("key-A"), k, proto)
		if c2sA == s2cA {
			t.Fatalf("%s: c2s == s2c (a side would accept its own packets)", k)
		}
		if c2sA2, s2cA2 := framingMagics([]byte("key-A"), k, proto); c2sA2 != c2sA || s2cA2 != s2cA {
			t.Fatalf("%s: framingMagics is not deterministic for a fixed (key,kind,proto)", k)
		}
		c2sB, s2cB := framingMagics([]byte("key-B"), k, proto)
		if c2sB == c2sA && s2cB == s2cA {
			t.Fatalf("%s: magics did not change with the key — framing is not keyed by the secret", k)
		}
	}
	// icmp's equivalent: the two directional prefixes differ, are stable, and
	// change with the key.
	if c2s, s2c := obfSeqPrefixes([]byte("key-A")); c2s == s2c {
		t.Fatal("icmp: c2s == s2c prefix (a side would accept its own packets)")
	}
	if c2s2, _ := obfSeqPrefixes([]byte("key-A")); c2s2 != aC2s {
		t.Fatal("icmp: obfSeqPrefixes is not deterministic for a fixed key")
	}
	if aC2s == bC2s {
		t.Fatal("icmp: prefixes did not change with the key")
	}
}

func TestRawFrameRejectsGarbage(t *testing.T) {
	for _, k := range rawKinds {
		cli, srv := testFramers(t, k, nil)
		good := cli.build(nil, 1, 1, []byte("payload"))
		for i := 0; i < srv.hdr; i++ {
			if k == KindICMP && i != 0 && i != 1 && i != 6 {
				// icmp validates only the echo type (0), code (1) and the keyed
				// prefix in the sequence high byte (6); checksum (2,3), id (4,5),
				// the counter low byte (7) and the per-packet nonce (8..15) are not.
				continue
			}
			if k == KindGRE && (i == 2 || i == 3 || i == 6 || i == 7) {
				continue // protocol type and link id are free fields
			}
			if (k == KindIPIP || k == KindIPX) && (i == 2 || i == 3) {
				continue // link id
			}
			bad := append([]byte(nil), good...)
			bad[i] ^= 0x40
			if _, _, ok := srv.parse(bad); ok {
				t.Fatalf("%s: corrupting header byte %d still parsed", k, i)
			}
		}
		if _, _, ok := srv.parse(good[:srv.hdr-1]); ok {
			t.Fatalf("%s: short packet parsed", k)
		}
	}
}

func TestIPXProto(t *testing.T) {
	for _, p := range []int{0, 1, 4, 6, 17, 47, 255, 256, -1} {
		if ValidIPXProto(p) {
			t.Fatalf("proto %d should be refused", p)
		}
		if p != 0 {
			if _, err := newFramer(KindIPX, Options{Proto: p}, true); err == nil {
				t.Fatalf("framer accepted proto %d", p)
			}
		}
	}
	f, err := newFramer(KindIPX, Options{}, true)
	if err != nil || f.proto != DefaultIPXProto {
		t.Fatalf("default ipx proto: %v %d", err, f.proto)
	}
	f, err = newFramer(KindIPX, Options{Proto: 200}, false)
	if err != nil || f.proto != 200 {
		t.Fatalf("ipx proto 200: %v", err)
	}
	// A kernel-handled protocol (41 = 6in4, 137 = MPLS-in-IP) is refused.
	for _, bad := range []int{41, 137, 2, 50, 132} {
		if ValidIPXProto(bad) {
			t.Fatalf("kernel-handled proto %d should be refused for ipx", bad)
		}
	}
	// Different ipx protocols derive different magics: the protocol number is
	// folded into the framing, so two ipx tunnels on different IP protocols do
	// not share a magic. Deterministic for these fixed inputs, so assert it.
	a, _ := framingMagics(nil, KindIPX, 200)
	b, _ := framingMagics(nil, KindIPX, 253)
	if a == b {
		t.Fatalf("ipx proto 200 and 253 derive the same magic %#x (proto not folded into the framing)", a)
	}
}

// ipv4 builds an IPv4 packet around a transport payload the way a raw socket
// delivers it.
func ipv4(proto byte, src, dst net.IP, tp []byte, frag uint16) []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(20+len(tp)))
	binary.BigEndian.PutUint16(h[6:], frag)
	h[8], h[9] = 64, proto
	copy(h[12:], src.To4())
	copy(h[16:], dst.To4())
	return append(h, tp...)
}

func TestIPv4Payload(t *testing.T) {
	f, _ := newFramer(KindGRE, Options{}, false)
	src, dst := net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2)
	tp := []byte("transport")
	p := ipv4(47, src, dst, tp, 0)
	s, d, got, ok := f.ipv4Payload(p)
	if !ok || !s.Equal(src) || !d.Equal(dst) || !bytes.Equal(got, tp) {
		t.Fatalf("ipv4Payload: ok=%v %v %v %q", ok, s, d, got)
	}
	// Trailing bytes beyond the IP total length are not payload.
	if _, _, got, _ := f.ipv4Payload(append(p, 1, 2, 3)); !bytes.Equal(got, tp) {
		t.Fatalf("padding leaked into payload: %q", got)
	}
	for name, bad := range map[string][]byte{
		"truncated":  p[:len(p)-1],
		"wrong prot": ipv4(4, src, dst, tp, 0),
		"fragment":   ipv4(47, src, dst, tp, 0x2000),
		"offset":     ipv4(47, src, dst, tp, 0x0001),
		"short":      p[:10],
		"ipv6":       append([]byte{0x60}, p[1:]...),
	} {
		if _, _, _, ok := f.ipv4Payload(bad); ok {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// runBPF is a tiny classic-BPF interpreter for the opcodes recvFilter emits,
// so the kernel filter's logic is tested on every platform (the Linux socket
// tests then prove the kernel agrees).
func runBPF(t *testing.T, prog []bpfInsn, pkt []byte) uint32 {
	t.Helper()
	var a, x uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLdxMsh:
			if int(in.K) >= len(pkt) {
				return 0
			}
			x = 4 * uint32(pkt[in.K]&0x0f)
		case bpfLdwAbs:
			o := int(in.K)
			if o+4 > len(pkt) {
				return 0
			}
			a = binary.BigEndian.Uint32(pkt[o:])
		case bpfLdhInd:
			o := int(x + in.K)
			if o+2 > len(pkt) {
				return 0
			}
			a = uint32(binary.BigEndian.Uint16(pkt[o:]))
		case bpfLdbInd:
			o := int(x + in.K)
			if o+1 > len(pkt) {
				return 0
			}
			a = uint32(pkt[o])
		case bpfJeqK:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRetK:
			return in.K
		default:
			t.Fatalf("unexpected opcode %#x", in.Code)
		}
	}
	t.Fatal("filter fell off the end")
	return 0
}

func TestRecvFilter(t *testing.T) {
	src, dst := net.IPv4(1, 1, 1, 1), net.IPv4(2, 2, 2, 2)
	for _, k := range rawKinds {
		cli, srv := testFramers(t, k, []byte("k"))
		other, _ := testFramers(t, k, []byte("other-key"))
		proto := byte(cli.proto)
		// listener filter: any id, peer's direction and magic only
		lf := srv.recvFilter(0, 0)
		if runBPF(t, lf, ipv4(proto, src, dst, cli.build(nil, 5, 1, []byte("d")), 0)) == 0 {
			t.Fatalf("%s: listener filter dropped a client packet", k)
		}
		if runBPF(t, lf, ipv4(proto, src, dst, cli.build(nil, 999, 1, []byte("d")), 0)) == 0 {
			t.Fatalf("%s: listener filter dropped another link id", k)
		}
		if runBPF(t, lf, ipv4(proto, src, dst, srv.build(nil, 5, 1, []byte("d")), 0)) != 0 {
			t.Fatalf("%s: listener filter passed its own direction", k)
		}
		if runBPF(t, lf, ipv4(proto, src, dst, other.build(nil, 5, 1, []byte("d")), 0)) != 0 {
			// icmp's cheap reject is the 8-bit prefix byte; a collision (1/256)
			// between the two keys is acceptable — the AEAD is the real boundary.
			if k == KindICMP && srv.rxPrefix>>8 == other.txPrefix>>8 {
				// prefix collision for these keys; nothing to assert here
			} else {
				t.Fatalf("%s: listener filter passed a foreign key", k)
			}
		}
		// dial filter: only its own id
		df := cli.recvFilter(5, binary.BigEndian.Uint32(net.IPv4(2, 2, 2, 2).To4()))
		if runBPF(t, df, ipv4(proto, dst, src, srv.build(nil, 5, 1, []byte("d")), 0)) == 0 {
			t.Fatalf("%s: dial filter dropped its reply", k)
		}
		if runBPF(t, df, ipv4(proto, dst, src, srv.build(nil, 6, 1, []byte("d")), 0)) != 0 {
			t.Fatalf("%s: dial filter passed another link's reply", k)
		}
		if runBPF(t, df, ipv4(proto, dst, src, cli.build(nil, 5, 1, []byte("d")), 0)) != 0 {
			t.Fatalf("%s: dial filter passed its own request", k)
		}
		// The dial filter drops a reply whose source IP is not the peer's, even
		// when kind, magic and link id all match: the source-IP BPF check that
		// keeps an off-path host from injecting into the unconnected dial socket.
		foreign := net.IPv4(9, 9, 9, 9)
		if runBPF(t, df, ipv4(proto, foreign, src, srv.build(nil, 5, 1, []byte("d")), 0)) != 0 {
			t.Fatalf("%s: dial filter accepted a reply from a foreign source IP", k)
		}
		// A header WITH options (IHL 6, four option bytes). The total-length
		// field must match the real, options-extended length; otherwise
		// ipv4Payload sees a packet whose declared length fits within 20 bytes of
		// header and returns ok with a TRUNCATED payload, and the IP-options path
		// (slicing at ihl, not a fixed 20) is never exercised. With the length
		// set, require the options packet to parse and yield EXACTLY the
		// transport bytes — a fixed-offset-20 slice would return the 4 option
		// bytes plus a short payload instead.
		tp := cli.build(nil, 5, 1, []byte("d"))
		p := ipv4(proto, src, dst, tp, 0)
		opt := append(append([]byte(nil), p[:20]...), 0, 0, 0, 0)
		opt = append(opt, p[20:]...)
		opt[0] = 0x46                                         // IHL = 6 (24-byte header)
		binary.BigEndian.PutUint16(opt[2:], uint16(len(opt))) // total length now includes the options
		if runBPF(t, lf, opt) == 0 {
			t.Fatalf("%s: filter mis-handles IP options", k)
		}
		_, _, got, ok := srv.ipv4Payload(opt)
		if !ok {
			t.Fatalf("%s: ipv4Payload rejected a valid options packet", k)
		}
		if !bytes.Equal(got, tp) {
			t.Fatalf("%s: ipv4Payload with options returned %d bytes, want the %d transport bytes (IHL not honoured)", k, len(got), len(tp))
		}
	}
}
