package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// --- the stage-6 (base 5ae865ee) advert code, verbatim, as the "old peer" ---

func oldServe(a *LBAdvert, st io.ReadWriteCloser) {
	defer st.Close()
	l, err := readAdvert(st)
	if err != nil {
		return
	}
	a.received(l)
	out := oldEncodeAdvert(a.getMine())
	a.mu.Lock()
	rf := a.report
	a.mu.Unlock()
	if rf != nil {
		out = append(out, rf()...)
	}
	st.Write(out)
}

func oldExchange(a *LBAdvert, ro rawStreamOpener) error {
	st, err := ro.OpenRawStream()
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(infoTimeout))
	if _, err := st.Write(append([]byte{kindLBAdvert}, oldEncodeAdvert(a.getMine())...)); err != nil {
		return err
	}
	l, err := readAdvert(st)
	if err != nil {
		return err
	}
	a.received(l)
	if r, ok := readReport(st); ok {
		a.gotReport(r)
	}
	return nil
}

// oldEncodeAdvert is encodeAdvert as of 5ae865ee (always ver 1, no ext).
func oldEncodeAdvert(l []LBAdvertIP) []byte {
	b := []byte{1, 0}
	n := 0
	for _, e := range l {
		ip := net.ParseIP(e.IP)
		if ip == nil || n >= lbAdvertMax {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		w := e.Weight
		if w < 0 {
			w = 0
		} else if w > 255 {
			w = 255
		}
		b = append(b, byte(len(ip)))
		b = append(b, ip...)
		b = append(b, byte(w))
		n++
	}
	b[1] = byte(n)
	return b
}

var testReality = &tlscarrier.FrontProfile{Kind: "reality", Target: "www.example.com:443",
	ServerNames: []string{"www.example.com"}, PublicKey: "pubkey", PrivateKey: "never-sent", ShortIDs: []string{"0a1b"},
	FallbackMaxBytes: 1 << 20, FallbackRateBps: 1 << 16}

// Default fronts: the advert is byte-for-byte the stage-6 one, and no ext.
func TestFrontAdvertDefaultBytesUnchanged(t *testing.T) {
	for _, l := range [][]LBAdvertIP{
		{{IP: "10.1.0.1", Weight: 60}, {IP: "2001:db8::1", Weight: 40}},
		{{IP: "10.1.0.1", Weight: 60, Front: &tlscarrier.FrontProfile{}}, {IP: "10.1.0.2", Weight: 40, Front: &tlscarrier.FrontProfile{Kind: "tls"}}},
		nil,
	} {
		if got, want := encodeAdvert(l), oldEncodeAdvert(l); !bytes.Equal(got, want) {
			t.Fatalf("%v: % x != % x", l, got, want)
		}
		if ext := encodeFrontExt(l); ext != nil {
			t.Fatalf("default fronts sent an ext: % x", ext)
		}
	}
}

func frontList() []LBAdvertIP {
	return []LBAdvertIP{{IP: "10.1.0.1", Weight: 70}, {IP: "10.1.0.2", Weight: 30, Front: testReality}}
}

// New message -> old parser: entries and report read as before, ext ignored.
func TestFrontAdvertNewMessageOldParser(t *testing.T) {
	l := frontList()
	rep := LBReport{Auto: true, Auto2: true}
	for _, withRep := range []bool{true, false} {
		msg := encodeAdvert(l)
		if msg[0] != lbAdvertVerFront {
			t.Fatalf("ver %d", msg[0])
		}
		if withRep {
			msg = append(msg, encodeReport(rep)...)
		}
		msg = append(msg, encodeFrontExt(l)...)
		r := bytes.NewReader(msg)
		got, err := readAdvert(r) // the old parser
		if err != nil || len(got) != 2 || got[0].IP != "10.1.0.1" || got[0].Weight != 70 || got[1].IP != "10.1.0.2" || got[1].Weight != 30 {
			t.Fatalf("old parser: %v %v", got, err)
		}
		gr, ok := readReport(r)
		if ok != withRep || (ok && (!gr.Auto || !gr.Auto2)) {
			t.Fatalf("old report parse: %v %+v", ok, gr)
		}
		if ValidateLBAdvert(got) != nil {
			t.Fatal("the old side would reject it")
		}
	}
}

// Old message -> new parser, and new -> new with fronts attached.
func TestFrontAdvertParsers(t *testing.T) {
	rep := LBReport{Auto: true}
	old := append(oldEncodeAdvert(frontList()), encodeReport(rep)...)
	br := bufio.NewReader(bytes.NewReader(old))
	l, ver, err := readAdvertVer(br)
	if err != nil || ver != 1 || len(l) != 2 || l[1].Front != nil {
		t.Fatalf("old msg: %v %d %v", l, ver, err)
	}
	if r, ok := readReport(br); !ok || !r.Auto {
		t.Fatal("old msg report lost")
	}
	for _, withRep := range []bool{true, false} {
		msg := encodeAdvert(frontList())
		if withRep {
			msg = append(msg, encodeReport(rep)...)
		}
		msg = append(msg, encodeFrontExt(frontList())...)
		br := bufio.NewReader(bytes.NewReader(msg))
		l, ver, err := readAdvertVer(br)
		if err != nil || ver != lbAdvertVerFront {
			t.Fatal(ver, err)
		}
		r, ok := readFrontTail(br, l)
		if ok != withRep || (ok && !r.Auto) {
			t.Fatalf("report %v %+v", ok, r)
		}
		f := l[1].Front
		if l[0].Front != nil || f == nil || f.Target != testReality.Target || f.PrivateKey != "" ||
			len(f.ShortIDs) != 1 || f.ShortIDs[0] != "0a1b" || f.FallbackRateBps != testReality.FallbackRateBps {
			t.Fatalf("fronts %+v %+v", l[0].Front, f)
		}
	}
	// a malformed ext never breaks the address list
	msg := append(encodeAdvert(frontList()), frontExtMagic, 1, 0, 3, '{', 'x', '}')
	br = bufio.NewReader(bytes.NewReader(msg))
	l, _, err = readAdvertVer(br)
	readFrontTail(br, l)
	if err != nil || len(l) != 2 || l[1].Front != nil {
		t.Fatalf("malformed ext: %v %v", l, err)
	}
}

type advSide struct {
	name     string
	exchange func(a *LBAdvert, ro rawStreamOpener) error
	serve    func(a *LBAdvert, st io.ReadWriteCloser)
}

var (
	newAdvSide = advSide{"new", func(a *LBAdvert, ro rawStreamOpener) error { return a.exchange(ro) },
		func(a *LBAdvert, st io.ReadWriteCloser) { a.serve(st) }}
	oldAdvSide = advSide{"old", oldExchange, oldServe}
)

func serveWith(ss *smux.Session, adv *LBAdvert, side advSide) {
	go func() {
		for {
			st, err := ss.AcceptStream()
			if err != nil {
				return
			}
			var k [1]byte
			if _, err := io.ReadFull(st, k[:]); err != nil || k[0] != kindLBAdvert {
				st.Close()
				continue
			}
			go side.serve(adv, st)
		}
	}()
}

// Every old/new pairing in both directions over a real smux stream: the
// addresses (and the exit's report) always arrive; fronts arrive new<->new.
func TestFrontAdvertMixedVersions(t *testing.T) {
	list := frontList()
	for _, edgeSide := range []advSide{oldAdvSide, newAdvSide} {
		for _, exitSide := range []advSide{oldAdvSide, newAdvSide} {
			for _, mode := range []string{"reverse", "direct"} {
				t.Run(edgeSide.name+"-edge/"+exitSide.name+"-exit/"+mode, func(t *testing.T) {
					got := make(chan []LBAdvertIP, 4)
					take := func(l []LBAdvertIP) { got <- l }
					var edge, exit *LBAdvert
					if mode == "reverse" {
						edge, exit = NewLBAdvert(list, nil, t.Logf), NewLBAdvert(nil, take, t.Logf)
					} else {
						edge, exit = NewLBAdvert(nil, take, t.Logf), NewLBAdvert(list, nil, t.Logf)
					}
					exit.report = func() []byte { return encodeReport(LBReport{Auto: true}) }
					cs, ss := advSmuxPair(t)
					serveWith(ss, exit, exitSide)
					if err := edgeSide.exchange(edge, sessOpener{cs}); err != nil {
						t.Fatal(err)
					}
					var l []LBAdvertIP
					select {
					case l = <-got:
					case <-time.After(2 * time.Second):
						t.Fatal("no advert reached the dialing side")
					}
					if len(l) != 2 || l[0].IP != list[0].IP || l[1].IP != list[1].IP || l[1].Weight != 30 {
						t.Fatalf("got %v", l)
					}
					// The listening side's fronts reach the dialer only when both
					// run the new code (reverse: the edge announces; direct: the exit).
					both := edgeSide.name == "new" && exitSide.name == "new"
					if has := l[1].Front != nil && l[1].Front.Kind == "reality"; has != both {
						t.Fatalf("front arrived=%v, want %v", has, both)
					}
					if l[1].Front != nil && l[1].Front.PrivateKey != "" {
						t.Fatal("the private key crossed the wire")
					}
					edge.mu.Lock()
					rep := edge.rep
					edge.mu.Unlock()
					if rep == nil || !rep.Auto {
						t.Fatal("the exit's report was lost")
					}
					// the same list again: not re-applied (fronts compared by value)
					edgeSide.exchange(edge, sessOpener{cs})
					select {
					case l := <-got:
						t.Fatalf("re-applied an unchanged list %v", l)
					case <-time.After(200 * time.Millisecond):
					}
				})
			}
		}
	}
}

// The text of an entry (logs, status, tests) is the stage-6 one by default.
func TestFrontAdvertStringUnchanged(t *testing.T) {
	l := []LBAdvertIP{{IP: "127.0.0.1", Weight: 40}, {IP: "127.0.0.2", Weight: 60, Front: &tlscarrier.FrontProfile{Kind: "tls"}}}
	if s := fmt.Sprint(l); s != "[{127.0.0.1 40} {127.0.0.2 60}]" {
		t.Fatal(s)
	}
	if s := fmt.Sprint(frontList()[1]); s != "{10.1.0.2 30 front=reality}" {
		t.Fatal(s)
	}
}
