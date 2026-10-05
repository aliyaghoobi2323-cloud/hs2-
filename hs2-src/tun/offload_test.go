package tun

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// tcpPkt builds an IPv4 TCP packet (timestamps option when ts) with payload.
func tcpPkt(src, dst [4]byte, sport, dport uint16, seq, ack uint32, flags byte, win uint16, ts uint32, payload []byte) []byte {
	thl := 20
	if ts != 0 {
		thl = 32
	}
	p := make([]byte, 20+thl+len(payload))
	p[0], p[8], p[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
	binary.BigEndian.PutUint16(p[4:], 0x1234)
	p[6] = 0x40 // DF
	copy(p[12:], src[:])
	copy(p[16:], dst[:])
	ipv4Csum(p[:20])
	t := p[20:]
	binary.BigEndian.PutUint16(t[0:], sport)
	binary.BigEndian.PutUint16(t[2:], dport)
	binary.BigEndian.PutUint32(t[4:], seq)
	binary.BigEndian.PutUint32(t[8:], ack)
	t[12] = byte(thl/4) << 4
	t[13] = flags
	binary.BigEndian.PutUint16(t[14:], win)
	if ts != 0 {
		t[20], t[21], t[22], t[23] = 1, 1, 8, 10
		binary.BigEndian.PutUint32(t[24:], ts)
		binary.BigEndian.PutUint32(t[28:], ts-7)
	}
	copy(t[thl:], payload)
	binary.BigEndian.PutUint16(t[16:], ^csumFold(csumAdd(pseudo4(p[12:16], p[16:20], 6, len(t)), t)))
	return p
}

// validV4 reports whether an IPv4 TCP packet's IP and TCP checksums verify.
func validV4(p []byte) bool {
	ihl := int(p[0]&0x0f) * 4
	if csumFold(csumAdd(0, p[:ihl])) != 0xffff {
		return false
	}
	t := p[ihl:]
	return csumFold(csumAdd(pseudo4(p[12:16], p[16:20], 6, len(t)), t)) == 0xffff
}

var (
	ipA = [4]byte{10, 66, 0, 1}
	ipB = [4]byte{10, 66, 0, 2}
)

// A 64 KB-class offload packet is cut into gso_size segments the kernel
// would have sent: valid checksums, sequence and IP ID advancing, FIN/PSH on
// the last only, CWR on the first only, the payload intact.
func TestSplitTCP(t *testing.T) {
	payload := make([]byte, 10000)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	big := tcpPkt(ipA, ipB, 40000, 443, 1000, 77, tcpACK|tcpPSH|tcpFIN|tcpCWR, 500, 12345, payload)
	out, ends, err := splitTCP(big, 1228, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ends) != 9 { // 8 × 1228 + 176
		t.Fatalf("%d segments, want 9", len(ends))
	}
	var got []byte
	start := 0
	for i, e := range ends {
		s := out[start:e]
		start = e
		if !validV4(s) {
			t.Fatalf("segment %d: bad checksum", i)
		}
		if int(binary.BigEndian.Uint16(s[2:])) != len(s) || len(s) > 20+32+1228 {
			t.Fatalf("segment %d: length %d", i, len(s))
		}
		if id := binary.BigEndian.Uint16(s[4:]); id != 0x1234+uint16(i) {
			t.Fatalf("segment %d: IP ID %#x", i, id)
		}
		if seq := binary.BigEndian.Uint32(s[24:]); seq != 1000+uint32(i*1228) {
			t.Fatalf("segment %d: seq %d", i, seq)
		}
		f := s[33]
		last := i == len(ends)-1
		if (f&tcpFIN != 0) != last || (f&tcpPSH != 0) != last || (f&tcpCWR != 0) != (i == 0) || f&tcpACK == 0 {
			t.Fatalf("segment %d: flags %#x", i, f)
		}
		got = append(got, s[52:]...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload changed")
	}
	if _, _, err := splitTCP(big, 10, nil, nil); err == nil {
		t.Fatal("a 10-byte gso_size was accepted")
	}
}

// A partial checksum (the kernel's pseudo-header sum in the field) is
// completed to the full one.
func TestCompleteCsum(t *testing.T) {
	p := tcpPkt(ipA, ipB, 1, 2, 3, 4, tcpACK, 9, 0, []byte("hello, partial checksum"))
	want := binary.BigEndian.Uint16(p[36:])
	binary.BigEndian.PutUint16(p[36:], csumFold(pseudo4(p[12:16], p[16:20], 6, len(p)-20)))
	if err := completeCsum(p, 20, 16); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint16(p[36:]); got != want || !validV4(p) {
		t.Fatalf("checksum %#x, want %#x", got, want)
	}
	if completeCsum(p, len(p)-1, 16) == nil {
		t.Fatal("an offset past the end was accepted")
	}
}

// Segments cut from one packet coalesce back into one, whose header the
// kernel takes (gso TCPV4, partial checksum) and whose payload is the whole.
func TestCoalesceRoundTrip(t *testing.T) {
	payload := make([]byte, 30000)
	for i := range payload {
		payload[i] = byte(i)
	}
	big := tcpPkt(ipA, ipB, 5000, 80, 7, 9, tcpACK|tcpPSH, 300, 99, payload)
	out, ends, _ := splitTCP(big, 1228, nil, nil)
	var pkts [][]byte
	start := 0
	for _, e := range ends {
		pkts = append(pkts, out[start:e])
		start = e
	}
	items := coalesce(pkts, nil, map[[12]byte]int{})
	if len(items) != 1 || len(items[0].segs) != len(pkts) {
		t.Fatalf("%d items (first has %d segments), want 1 of %d", len(items), len(items[0].segs), len(pkts))
	}
	m := buildMerged(nil, pkts, &items[0])
	var h vnetHdr
	h.decode(m)
	ip := m[vnetHdrLen:]
	if h.gsoType != gsoTCPv4 || h.flags != vnetNeedsCsum || h.gsoSize != 1228 || h.hdrLen != 52 || h.csumStart != 20 || h.csumOffset != 16 {
		t.Fatalf("header %+v", h)
	}
	if !bytes.Equal(ip[52:], payload) || int(binary.BigEndian.Uint16(ip[2:])) != len(ip) {
		t.Fatal("merged payload or length wrong")
	}
	if ip[33]&tcpPSH == 0 {
		t.Fatal("PSH of the last segment lost")
	}
	completeCsum(ip, 20, 16) // what the kernel would do with the partial sum
	if !validV4(ip) {
		t.Fatal("merged packet's checksums do not verify once completed")
	}
}

// What must not merge: a gap, another ack or window, other options, a flag
// but ACK/PSH, a segment after a shorter one or after PSH — and a connection
// whose non-mergeable packet came between keeps its order.
func TestCoalesceRules(t *testing.T) {
	d := make([]byte, 1000)
	seg := func(seq uint32, ack uint32, fl byte, win uint16, ts uint32, n int) []byte {
		return tcpPkt(ipA, ipB, 1, 2, seq, ack, fl, win, ts, d[:n])
	}
	runs := func(pkts ...[]byte) (out [][]int) {
		for _, it := range coalesce(pkts, nil, map[[12]byte]int{}) {
			out = append(out, it.segs)
		}
		return
	}
	eq := func(name string, got [][]int, want ...[]int) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %v, want %v", name, got, want)
		}
		for i := range got {
			if len(got[i]) != len(want[i]) {
				t.Fatalf("%s: %v, want %v", name, got, want)
			}
			for j := range got[i] {
				if got[i][j] != want[i][j] {
					t.Fatalf("%s: %v, want %v", name, got, want)
				}
			}
		}
	}
	eq("in order", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 1, tcpACK, 9, 5, 1000), seg(2000, 1, tcpACK, 9, 5, 400)), []int{0, 1, 2})
	eq("gap", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1500, 1, tcpACK, 9, 5, 1000)), []int{0}, []int{1})
	eq("ack", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 2, tcpACK, 9, 5, 1000)), []int{0}, []int{1})
	eq("window", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 1, tcpACK, 8, 5, 1000)), []int{0}, []int{1})
	eq("timestamps", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 1, tcpACK, 9, 6, 1000)), []int{0}, []int{1})
	eq("after a shorter one", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 1, tcpACK, 9, 5, 500), seg(1500, 1, tcpACK, 9, 5, 1000)), []int{0, 1}, []int{2})
	eq("after PSH", runs(seg(0, 1, tcpACK|tcpPSH, 9, 5, 1000), seg(1000, 1, tcpACK, 9, 5, 1000)), []int{0}, []int{1})
	eq("larger than the first", runs(seg(0, 1, tcpACK, 9, 5, 500), seg(500, 1, tcpACK, 9, 5, 1000)), []int{0}, []int{1})
	eq("FIN not merged", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 1, tcpACK|tcpFIN, 9, 5, 1000)), []int{0}, []int{1})
	// A pure ACK of the same connection in between ends the run: the segment
	// after it starts a new one, after it.
	eq("order kept", runs(seg(0, 1, tcpACK, 9, 5, 1000), seg(1000, 1, tcpACK, 9, 5, 0), seg(1000, 1, tcpACK, 9, 5, 1000)), []int{0}, []int{1}, []int{2})
	// Another connection's segments interleaved do not stop a run.
	other := tcpPkt(ipA, ipB, 3, 4, 0, 1, tcpACK, 9, 5, d[:1000])
	eq("interleaved", runs(seg(0, 1, tcpACK, 9, 5, 1000), other, seg(1000, 1, tcpACK, 9, 5, 1000)), []int{0, 2}, []int{1})
}

// Random batches of several connections' segments, some lost, some
// duplicated, some reordered: every byte written is what was given, each
// connection's bytes in the order given, and every packet valid.
func TestCoalesceRandomBatches(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 300; round++ {
		var pkts [][]byte
		type conn struct{ seq uint32 }
		conns := make([]conn, 1+r.IntN(4))
		var want = map[uint16][]byte{}
		for k := 0; k < 1+r.IntN(80); k++ {
			c := r.IntN(len(conns))
			n := 1228
			if r.IntN(8) == 0 {
				n = r.IntN(1228)
			}
			d := make([]byte, n)
			for i := range d {
				d[i] = byte(r.Uint32())
			}
			fl := byte(tcpACK)
			if r.IntN(10) == 0 {
				fl |= tcpPSH
			}
			p := tcpPkt(ipA, ipB, uint16(1000+c), 80, conns[c].seq, 1, fl, 9, 5, d)
			if r.IntN(12) != 0 { // else: lost
				conns[c].seq += uint32(n)
			}
			pkts = append(pkts, p)
			want[uint16(1000+c)] = append(want[uint16(1000+c)], d...)
		}
		items := coalesce(pkts, nil, map[[12]byte]int{})
		got := map[uint16][]byte{}
		var buf []byte
		for _, it := range items {
			var ip []byte
			if len(it.segs) == 1 {
				ip = pkts[it.segs[0]]
			} else {
				buf = buildMerged(buf, pkts, &it)
				ip = append([]byte(nil), buf[vnetHdrLen:]...)
				completeCsum(ip, 20, 16)
			}
			if !validV4(ip) {
				t.Fatalf("round %d: an invalid packet", round)
			}
			hl := 20 + int(ip[32]>>4)*4
			sp := binary.BigEndian.Uint16(ip[20:])
			got[sp] = append(got[sp], ip[hl:]...)
		}
		for sp, w := range want {
			if !bytes.Equal(got[sp], w) {
				t.Fatalf("round %d: connection %d's bytes differ", round, sp)
			}
		}
	}
}

func FuzzSplitTCP(f *testing.F) {
	f.Add(tcpPkt(ipA, ipB, 1, 2, 3, 4, tcpACK, 5, 6, make([]byte, 3000)), uint16(1228))
	f.Fuzz(func(t *testing.T, p []byte, gso uint16) {
		splitTCP(p, int(gso), nil, nil)
		coalesce([][]byte{p, p}, nil, map[[12]byte]int{})
		parseGRO(p)
	})
}
