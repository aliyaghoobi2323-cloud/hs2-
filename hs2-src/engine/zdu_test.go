package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"reflect"
	"testing"
	"time"
)

func setZDU(t *testing.T, on bool) {
	t.Helper()
	old := zduOn.Load()
	zduOn.Store(on)
	// the B3 gauges are package sums of per-pool deltas: a test's pools are
	// dropped without Run (whose exit calls zduForget), so their share is
	// taken back here; without it zdu_peerold leaked into later tests
	p, c := zduGauge.peerOld.Load(), zduGauge.oldConns.Load()
	// the pin/count tests flip at the first new link: the stepped shift
	// (zdu_phase.go) has its own tests, which turn it back on
	ph := zduPhaseOn.Swap(false)
	t.Cleanup(func() { zduOn.Store(old); zduGauge.peerOld.Store(p); zduGauge.oldConns.Store(c); zduPhaseOn.Store(ph) })
}

// readInfo69p is readInfo exactly as rel69p-rc3 (abda029a) shipped it: the
// pre-ZDU reader, for the mixed-version tests.
func readInfo69p(r io.Reader) (peerInfo, error) {
	var pi peerInfo
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return pi, err
	}
	if h[0] < 1 {
		return pi, errInfoVersion
	}
	p := make([]byte, int(h[1]))
	if _, err := io.ReadFull(r, p); err != nil {
		return pi, err
	}
	if len(p) >= 2 {
		pi.MaxLinks = int(binary.BigEndian.Uint16(p))
	}
	if len(p) >= infoFixedLen {
		pi.V2 = true
		pi.Caps, pi.Flags = p[2], p[3]
		n := int(p[4])
		if have := (len(p) - infoFixedLen) / 2; n > have {
			n, pi.Flags = have, pi.Flags|flagCut
		}
		for i := 0; i < n; i++ {
			if v := int(binary.BigEndian.Uint16(p[infoFixedLen+2*i:])); v > 0 {
				pi.Ports = append(pi.Ports, v)
			}
		}
		if o := infoFixedLen + 2*int(p[4]); o < len(p) {
			m := min(int(p[o]), len(p)-o-1)
			for i := 0; i < m && i < infoExtMax; i++ {
				pi.Ext |= uint32(p[o+1+i]) << (8 * i)
			}
		}
	}
	return pi, nil
}

// TestZDUWireUnchangedOff: with HS2_ZDU off the kindInfo bytes both sides send
// are byte-identical to rel69p-rc3's (golden bytes written by hand from the
// abda029a encoder), whatever other ext bits are on.
func TestZDUWireUnchangedOff(t *testing.T) {
	setZDU(t, false)
	if infoExt()&capExtZDU != 0 || zduInfoExt() != 0 || zduInfoInst().known() {
		t.Fatalf("key off but ext %#x inst %+v", infoExt(), zduInfoInst())
	}
	cases := []struct {
		pi   peerInfo
		want []byte
	}{
		// no ext: [ver 2][n 7][maxLinks 48][caps][flags][count 1][443]
		{peerInfo{MaxLinks: 48, Caps: capPortTags | capL3Quiet, Flags: flagUDP, Ports: []int{443}, Inst: zduInfoInst()},
			[]byte{2, 7, 0, 48, 3, 1, 1, 0x01, 0xbb}},
		// ext 0x2000 (half-close, the prod default) + 0x4000 (xs261): 2 bytes
		{peerInfo{MaxLinks: 0, Caps: capMConn, Ext: capExtHalfClose | capExtMConnXS, Inst: zduInfoInst()},
			[]byte{2, 8, 0, 0, 8, 0, 0, 2, 0x00, 0x60}},
		// ext bit8 + bit0: [m 2][0x01 0x01]
		{peerInfo{MaxLinks: 70, Ext: capExtMConnStripe | capExtMConnRescue, Ports: []int{2053, 8443}, Inst: zduInfoInst()},
			[]byte{2, 12, 0, 70, 0, 0, 2, 0x08, 0x05, 0x20, 0xfb, 2, 0x01, 0x01}},
	}
	for i, c := range cases {
		if got := encodeInfo(c.pi); !bytes.Equal(got, c.want) {
			t.Fatalf("case %d: % x, want % x", i, got, c.want)
		}
	}
	// and what the live code builds (infoExt as configured in this test
	// binary) equals the same message without the instance field
	base := peerInfo{MaxLinks: 64, Caps: capPortTags | capL3Quiet | mconnCap(), Ext: infoExt(), Ports: []int{443, 13109}}
	with := base
	with.Inst = zduInfoInst()
	if !bytes.Equal(encodeInfo(base), encodeInfo(with)) {
		t.Fatal("key off: the instance changed the bytes")
	}
	// a long port list keeps the pre-ZDU cut (no trailer room taken)
	many := make([]int, 200)
	for i := range many {
		many[i] = 1000 + i
	}
	got, _ := readInfo(bytes.NewReader(encodeInfo(peerInfo{Ports: many, Ext: capExtHalfClose, Inst: zduInfoInst()})))
	if len(got.Ports) != (255-infoFixedLen-1-infoExtMax)/2 {
		t.Fatalf("key off, long list: %d ports", len(got.Ports))
	}
}

// TestZDUInfoRoundTrip: with HS2_ZDU=1 the message carries bit 15 and the
// instance; the new reader gets both back; the trailer coexists with other
// ext bits (b367's bit 16 included) and with a full port list.
func TestZDUInfoRoundTrip(t *testing.T) {
	setZDU(t, true)
	if infoExt()&capExtZDU == 0 || zduInfoInst() != zduSelf || !zduSelf.known() {
		t.Fatalf("key on: ext %#x inst %+v self %+v", infoExt(), zduInfoInst(), zduSelf)
	}
	in := peerInfo{MaxLinks: 48, Caps: capPortTags, Flags: flagUDP, Ports: []int{443, 8443},
		Ext: infoExt() | 1<<16, Inst: zduInfoInst()}
	b := encodeInfo(in)
	got, err := readInfo(bytes.NewReader(b))
	if err != nil || got.Inst != zduSelf || got.Ext != in.Ext || fmt.Sprint(got.Ports) != "[443 8443]" ||
		got.MaxLinks != 48 || got.Flags != flagUDP || !got.zdu() {
		t.Fatalf("round trip: %+v %v (sent %+v)", got, err, in)
	}
	// the trailer alone (no other ext bit) still writes an ext block
	solo := encodeInfo(peerInfo{Inst: zduInst{Start: 7, Rand: 9}})
	got, _ = readInfo(bytes.NewReader(solo))
	if got.Inst != (zduInst{Start: 7, Rand: 9}) || got.Ext != 0 || got.zdu() {
		t.Fatalf("inst without the bit: %+v", got)
	}
	// a full port list: cut so that ext + trailer fit, nothing lost silently
	many := make([]int, 200)
	for i := range many {
		many[i] = 1000 + i
	}
	full := encodeInfo(peerInfo{Ports: many, Ext: capExtZDU, Inst: zduSelf})
	if len(full) > 2+255 {
		t.Fatalf("message %d bytes", len(full))
	}
	got, _ = readInfo(bytes.NewReader(full))
	if got.Inst != zduSelf || got.Ext != capExtZDU || got.Flags&flagCut == 0 ||
		len(got.Ports) != (255-infoFixedLen-1-infoExtMax-infoTrailerMax)/2 {
		t.Fatalf("long list: %d ports, %+v", len(got.Ports), got)
	}
}

// TestZDUMixedVersion: a rel69p reader (readInfo69p) reads a ZDU message as
// exactly the same fields as the message without the ZDU parts (the bit
// aside); the ZDU reader reads a rel69p message as "no instance".
func TestZDUMixedVersion(t *testing.T) {
	setZDU(t, true)
	plain := peerInfo{MaxLinks: 48, Caps: capPortTags | capMConn | capMConnRaw, Flags: flagDefault,
		Ports: []int{2053}, Ext: capExtHalfClose | capExtMConnRescue}
	zd := plain
	zd.Ext |= capExtZDU
	zd.Inst = zduSelf
	old, err := readInfo69p(bytes.NewReader(encodeInfo(zd)))
	if err != nil {
		t.Fatal(err)
	}
	want := plain
	want.V2 = true
	want.Ext |= capExtZDU // an old reader keeps an unknown bit and never looks at it
	if !reflect.DeepEqual(old, want) {
		t.Fatalf("old reader: %+v, want %+v", old, want)
	}
	// unknown trailing tags and a future longer ext are skipped
	b := encodeInfo(zd)
	b = append(b, 9, 3, 1, 2, 3) // tag 9, len 3
	b[1] += 5
	got, err := readInfo(bytes.NewReader(b))
	if err != nil || got.Inst != zduSelf || !got.zdu() {
		t.Fatalf("unknown tag: %+v %v", got, err)
	}
	if o, err := readInfo69p(bytes.NewReader(b)); err != nil || !reflect.DeepEqual(o, want) {
		t.Fatalf("old reader, unknown tag: %+v %v", o, err)
	}
	// a cut trailer: no instance, no error
	c := encodeInfo(zd)
	c = c[:len(c)-3]
	c[1] -= 3
	if got, err := readInfo(bytes.NewReader(c)); err != nil || got.Inst.known() || got.zdu() || got.Ext&capExtZDU == 0 {
		t.Fatalf("cut trailer: %+v %v", got, err)
	}
	// new reader, rel69p sender (no trailer, no bit)
	setZDU(t, false)
	got, err = readInfo(bytes.NewReader(encodeInfo(plain)))
	if err != nil || got.Inst.known() || got.zdu() {
		t.Fatalf("new reader, old message: %+v %v", got, err)
	}
	// v1 and port-cut messages: never an instance
	if got, _ := readInfo(bytes.NewReader([]byte{1, 2, 0, 48})); got.Inst.known() {
		t.Fatal("v1 has an instance")
	}
}

// TestZDUServeInfoMixed: the kindInfo exchange itself (serveInfo over a pipe)
// between a ZDU edge and a rel69p exit, and the other way.
func TestZDUServeInfoMixed(t *testing.T) {
	setZDU(t, true)
	edge := peerInfo{MaxLinks: 64, Caps: capPortTags, Ext: infoExt(), Inst: zduInfoInst()}
	exit69p := peerInfo{MaxLinks: 32, Caps: capPortTags, Ext: capExtHalfClose}

	run := func(edgeMsg, exitMsg peerInfo, exitRead func(io.Reader) (peerInfo, error)) (atExit, atEdge peerInfo) {
		a, b := net.Pipe()
		defer a.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer b.Close()
			b.SetDeadline(time.Now().Add(5 * time.Second))
			pi, err := exitRead(b)
			if err != nil {
				return
			}
			atExit = pi
			b.Write(encodeInfo(exitMsg))
		}()
		a.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := a.Write(encodeInfo(edgeMsg)); err != nil {
			t.Fatal(err)
		}
		atEdge, err := readInfo(a)
		if err != nil {
			t.Fatal(err)
		}
		<-done
		return atExit, atEdge
	}
	// ZDU edge -> rel69p exit: the exit reads everything it knew before
	atExit, atEdge := run(edge, exit69p, readInfo69p)
	if atExit.MaxLinks != 64 || !atExit.tags() || atExit.Inst.known() {
		t.Fatalf("rel69p exit read %+v", atExit)
	}
	if atEdge.zdu() || atEdge.Inst.known() || atEdge.MaxLinks != 32 {
		t.Fatalf("ZDU edge read the rel69p exit as %+v", atEdge)
	}
	// rel69p edge -> ZDU exit (serveInfo, the real exit code)
	a, b := net.Pipe()
	stored := make(chan peerInfo, 1)
	go serveInfo(b, edge, func(p peerInfo) { stored <- p })
	a.SetDeadline(time.Now().Add(5 * time.Second))
	a.Write(encodeInfo(peerInfo{MaxLinks: 16, Caps: capPortTags}))
	reply, err := readInfo69p(a)
	a.Close()
	if err != nil || reply.MaxLinks != 64 || !reply.tags() {
		t.Fatalf("rel69p edge read the ZDU exit as %+v %v", reply, err)
	}
	if p := <-stored; p.zdu() || p.MaxLinks != 16 {
		t.Fatalf("ZDU exit stored %+v", p)
	}
}

func TestZDUInstOrder(t *testing.T) {
	a := zduInst{Start: 10, Rand: 5}
	b := zduInst{Start: 11, Rand: 1}
	if !b.newer(a) || a.newer(b) || a.newer(a) {
		t.Fatal("start order")
	}
	if !(zduInst{Start: 10, Rand: 6}).newer(a) {
		t.Fatal("rand tie-break")
	}
	if !a.newer(zduInst{}) || (zduInst{}).known() {
		t.Fatal("zero")
	}
	n := newZDUInst(time.Unix(0, 0))
	if !n.known() {
		t.Fatal("instance at time 0 must still be known")
	}
	x, y := newZDUInst(time.Unix(5, 0)), newZDUInst(time.Unix(5, 0))
	if x == y {
		t.Fatal("two instances in the same ns collided")
	}
	if !(zduInst{Start: uint64(time.Now().Add(time.Second).UnixNano())}).newer(zduSelf) {
		t.Fatal("a later process is not newer than this one")
	}
}
