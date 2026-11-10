package engine

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Front profiles in the lb advert (reality phase 1: plumbing only).
//
// The listening side owns its addresses AND the front each one runs
// (tlscarrier/front.go), so it announces a per-IP FrontProfile next to its
// address list. The announcement is an OPTIONAL extension that old peers
// ignore and that is NEVER sent while every profile is the default ("tls"):
// then the advert is byte-for-byte the stage-6 one (ver=1).
//
// With a non-default profile the listening side sends:
//
//	advert: [ver=2][n u8] n × ([iplen u8][ip][weight u8])   (unchanged entries)
//	        ... whatever followed before (the exit's report, lb_report.go) ...
//	ext:    [frontExtMagic][frontExtVer=1][len u16 BE][len bytes: JSON [{ip, front}]]
//
// at the very END of its message. An old parser accepts ver>=1 (readAdvert
// checks h[0] < 1 only), reads its n entries, reads the report (which still
// directly follows the entries) and never looks further, so the ext is
// ignored. A new parser that sees ver>=2 reads the ext after the report (or
// right after the entries when no report follows: the report magic differs
// from frontExtMagic).

const (
	lbAdvertVerFront = 2
	frontExtMagic    = 0xF5
	frontExtVer      = 1
	frontExtMax      = 8 << 10
)

var errFrontExt = errors.New("lb advert: malformed front extension")

type frontExtEntry struct {
	IP    string                   `json:"ip"`
	Front *tlscarrier.FrontProfile `json:"front"`
}

// advertHasFront: some entry carries a non-default profile (ver 2 + ext).
func advertHasFront(l []LBAdvertIP) bool {
	for _, e := range l {
		if !e.Front.IsDefault() {
			return true
		}
	}
	return false
}

// encodeFrontExt is the ext block of l ("" when every profile is default).
func encodeFrontExt(l []LBAdvertIP) []byte {
	if !advertHasFront(l) {
		return nil
	}
	var es []frontExtEntry
	for _, e := range l {
		ip := net.ParseIP(e.IP)
		if ip == nil || e.Front.IsDefault() {
			continue
		}
		es = append(es, frontExtEntry{IP: ip.String(), Front: e.Front})
	}
	js, err := json.Marshal(es)
	if err != nil || len(js) > frontExtMax {
		return nil
	}
	b := []byte{frontExtMagic, frontExtVer, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(len(js)))
	return append(b, js...)
}

// readFrontExt reads one ext block from r.
func readFrontExt(r io.Reader) (map[string]*tlscarrier.FrontProfile, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if h[0] != frontExtMagic || h[1] < 1 {
		return nil, errFrontExt
	}
	n := int(binary.BigEndian.Uint16(h[2:]))
	if n > frontExtMax {
		return nil, errFrontExt
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var es []frontExtEntry
	if err := json.Unmarshal(buf, &es); err != nil {
		return nil, errFrontExt
	}
	out := map[string]*tlscarrier.FrontProfile{}
	for _, e := range es {
		ip := net.ParseIP(e.IP)
		if ip == nil || e.Front == nil {
			continue
		}
		out[ip.String()] = e.Front
	}
	return out, nil
}

// attachFronts sets each entry's Front from m (by IP).
func attachFronts(l []LBAdvertIP, m map[string]*tlscarrier.FrontProfile) {
	for i := range l {
		if ip := net.ParseIP(l[i].IP); ip != nil {
			if f, ok := m[ip.String()]; ok {
				l[i].Front = f
			}
		}
	}
}

// readAdvertVer is readAdvert that also returns the version byte.
func readAdvertVer(r io.Reader) ([]LBAdvertIP, byte, error) {
	var vb [1]byte
	if _, err := io.ReadFull(r, vb[:]); err != nil {
		return nil, 0, err
	}
	l, err := readAdvert(io.MultiReader(bytesReader(vb[:]), r))
	return l, vb[0], err
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// peekFrontExt: the next byte of br starts a front ext block.
func peekFrontExt(br *bufio.Reader) bool {
	b, err := br.Peek(1)
	return err == nil && b[0] == frontExtMagic
}

// frontEqual compares two profiles by value (nil == default).
func frontEqual(a, b *tlscarrier.FrontProfile) bool {
	if a.IsDefault() && b.IsDefault() {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return reflect.DeepEqual(*a, *b)
}

// readFrontTail reads what follows a ver-2 advert in the exit's reply: the
// optional report, then the front ext, whose profiles it attaches to l.
func readFrontTail(br *bufio.Reader, l []LBAdvertIP) (LBReport, bool) {
	var rep LBReport
	got := false
	if !peekFrontExt(br) {
		rep, got = readReport(br)
	}
	if m, err := readFrontExt(br); err == nil {
		attachFronts(l, m)
	}
	return rep, got
}

// String keeps the stage-6 text of an entry ("{ip weight}") for logs and
// status while its front is the default; a non-default front is appended.
func (e LBAdvertIP) String() string {
	if e.Front.IsDefault() {
		return fmt.Sprintf("{%s %d}", e.IP, e.Weight)
	}
	return fmt.Sprintf("{%s %d front=%s}", e.IP, e.Weight, e.Front.NormKind())
}

// EncodeLBAdvertWire is the listening side's announcement as sent (entries +
// the front ext), for tests and tools that check what leaves the host.
func EncodeLBAdvertWire(l []LBAdvertIP) []byte {
	return append(encodeAdvert(l), encodeFrontExt(l)...)
}
