package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// coldLab is a pure-reality listening IP (a real RealityServer over a local
// TLS target) that echoes every tunnel carrier.
type coldLab struct {
	addr string
	pub  *tlscarrier.FrontProfile // what the listener announces
	key  []byte
}

func newColdLab(t *testing.T) *coldLab {
	t.Helper()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "site") }))
	t.Cleanup(target.Close)
	priv, _, err := tlscarrier.RealityKeygen()
	if err != nil {
		t.Fatal(err)
	}
	lp := &tlscarrier.FrontProfile{Kind: "reality", Target: target.Listener.Addr().String(), ServerNames: []string{"www.target.test"},
		PrivateKey: priv, ShortIDs: []string{"a1b2c3d4"}}
	key := []byte("0123456789abcdef0123456789abcdef")
	rs, err := tlscarrier.NewRealityServer(lp, &tlscarrier.Server{SharedKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go rs.AcceptFront(context.Background(), c, func(car *tlscarrier.Carrier) {
				go func() { io.Copy(car.RawConn(), car.RawConn()); car.Close() }()
			})
		}
	}()
	return &coldLab{addr: ln.Addr().String(), pub: publicFront(lp), key: key}
}

// dial dials the lab with the front a dialing side's path would use.
func (l *coldLab) dial(t *testing.T, prof *tlscarrier.FrontProfile) error {
	t.Helper()
	car, err := tlscarrier.DialPathFront(context.Background(), tlscarrier.DialPath{Addr: l.addr, SNI: "tunnel.example", Key: l.key, Profile: prof, Connect: 2 * time.Second})
	if err != nil {
		return err
	}
	defer car.Close()
	msg := []byte("cold start")
	car.RawConn().Write(msg)
	got := make([]byte, len(msg))
	car.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(car.RawConn(), got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo %q %v", got, err)
	}
	return nil
}

// pathFrontOf is the front the dialing side's path to ip dials now.
func pathFrontOf(t *testing.T, fc fileConfig, lb *engine.LB, ip string) *tlscarrier.FrontProfile {
	t.Helper()
	for _, p := range lb.Paths() {
		if p.Remote == ip {
			return pathFront(fc, p.Front)
		}
	}
	t.Fatalf("no path to %s: %+v", ip, lb.Paths())
	return nil
}

// The cold-dialer bug and its fix, end to end over a real reality listener:
//  1. a dialer with no key (fresh start) dials tls and is refused by a pure
//     reality IP — before rel44 the tunnel stayed down for good;
//  2. once an announcement arrives the dialer writes the public profiles to
//     disk (0600, nothing secret) ...
//  3. ... and a restarted dialer dials the remembered profile (as "both") at
//     once and gets in, before any announcement.
//  4. an announcement back to tls removes the file.
func TestColdDialerRemembersFronts(t *testing.T) {
	lab := newColdLab(t)
	cfg := filepath.Join(t.TempDir(), "c.json")
	writeCfg(t, cfg, lbIranDirDial)
	resetLBRun(t, cfg)
	fc := lbFC(t, lbIranDirDial)
	fp := peerFrontsPath(cfg)

	lb := newLB(fc, t.Logf)
	setupLB(fc, lb, nil, t.Logf)
	f0 := pathFrontOf(t, fc, lb, "10.1.0.1")
	if f0.NormKind() != tlscarrier.FrontTLS {
		t.Fatalf("cold dialer front %q, want tls (no key known)", f0.NormKind())
	}
	if err := lab.dial(t, f0); err == nil {
		t.Fatal("a tls dial reached a pure reality listener: the repro is wrong")
	}

	curLB().applyAdvert([]engine.LBAdvertIP{{IP: "10.1.0.1", Weight: 70, Front: lab.pub}, {IP: "10.1.0.2", Weight: 30}})
	st, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("announced fronts not remembered: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(fp)
	if strings.Contains(string(b), "private") || strings.Contains(string(b), `"target"`) || !strings.Contains(string(b), lab.pub.PublicKey) {
		t.Fatalf("file content: %s", b)
	}
	if err := lab.dial(t, pathFrontOf(t, fc, lb, "10.1.0.1")); err != nil {
		t.Fatalf("after the announcement: %v", err)
	}

	// restart: a new runtime, no announcement yet
	resetLBRun(t, cfg)
	lb2 := newLB(fc, t.Logf)
	setupLB(fc, lb2, nil, t.Logf)
	f2 := pathFrontOf(t, fc, lb2, "10.1.0.1")
	if f2.NormKind() != tlscarrier.FrontBoth || f2.PublicKey != lab.pub.PublicKey {
		t.Fatalf("restarted dialer front %+v", f2)
	}
	if err := lab.dial(t, f2); err != nil {
		t.Fatalf("restarted dialer did not reach the pure reality IP: %v", err)
	}
	if f := pathFrontOf(t, fc, lb2, "10.1.0.2"); f.NormKind() != tlscarrier.FrontTLS {
		t.Fatalf("a tls IP got a front: %+v", f)
	}

	// the announcement replaces the remembered profiles; all-tls removes the file
	curLB().applyAdvert([]engine.LBAdvertIP{{IP: "10.1.0.1", Weight: 70}, {IP: "10.1.0.2", Weight: 30}})
	if f := pathFrontOf(t, fc, lb2, "10.1.0.1"); f.NormKind() != tlscarrier.FrontTLS {
		t.Fatalf("announcement did not replace the remembered front: %+v", f)
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("file kept after an all-tls announcement: %v", err)
	}
}

// hs2 reality pin: the listener's code, written on a dialer that never heard
// an announcement, is used at the next start and on SIGHUP while waiting.
func TestRealityPinCode(t *testing.T) {
	lab := newColdLab(t)
	code := pinCode(map[string]*tlscarrier.FrontProfile{"10.1.0.1": lab.pub})
	if !strings.HasPrefix(code, pinCodePrefix) {
		t.Fatal(code)
	}
	m, err := parsePinCode(code)
	if err != nil || m["10.1.0.1"].PublicKey != lab.pub.PublicKey {
		t.Fatalf("%v %+v", err, m)
	}
	for _, bad := range []string{"", "hs2rp1.!!", pinCodePrefix + "e30"} {
		if _, err := parsePinCode(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// a private key smuggled into a code never reaches the file
	lp := *lab.pub
	lp.PrivateKey = "x"
	if m, _ := parsePinCode(pinCode(map[string]*tlscarrier.FrontProfile{"10.1.0.1": &lp})); m["10.1.0.1"].PrivateKey != "" {
		t.Fatal("private key kept")
	}

	cfg := filepath.Join(t.TempDir(), "c.json")
	writeCfg(t, cfg, lbIranDirDial)
	resetLBRun(t, cfg)
	fc := lbFC(t, lbIranDirDial)
	lb := newLB(fc, t.Logf)
	setupLB(fc, lb, nil, t.Logf)
	if f := pathFrontOf(t, fc, lb, "10.1.0.1"); f.NormKind() != tlscarrier.FrontTLS {
		t.Fatal("front before pin")
	}
	if err := savePeerFronts(peerFrontsPath(cfg), lbPeerKey(fc), "pin", m); err != nil {
		t.Fatal(err)
	}
	reloadLB("pin") // the daemon is waiting with no key: SIGHUP picks it up
	f := pathFrontOf(t, fc, lb, "10.1.0.1")
	if f.NormKind() != tlscarrier.FrontBoth {
		t.Fatalf("after pin+reload: %+v", f)
	}
	if err := lab.dial(t, f); err != nil {
		t.Fatal(err)
	}
	// another peer's file is ignored
	if m, err := loadPeerFronts(peerFrontsPath(cfg), "10.9.9.9:443"); err == nil || m != nil {
		t.Fatalf("foreign peer accepted: %v %v", m, err)
	}
}

// The listening side refuses to make its last tls-capable IP pure reality
// unless the dialer has shown it has the key; the doctor warns.
func TestColdDialerGuard(t *testing.T) {
	_, pub, _ := tlscarrier.RealityKeygen()
	priv, _, _ := tlscarrier.RealityKeygen()
	_ = pub
	re := &tlscarrier.FrontProfile{Kind: "reality", Target: "www.site.test:443", ServerNames: []string{"www.site.test"}, PrivateKey: priv, ShortIDs: []string{"a1b2c3d4"}}
	bo := *re
	bo.Kind = "both"
	fc := fileConfig{Mode: "listen", Carrier: "mtcp", Addr: "10.1.0.1:443"}
	fc.LB = &lbConfig{Enabled: true, Kharej: []lbIP{{IP: "10.1.0.1", Weight: 50, Front: re}, {IP: "10.1.0.2", Weight: 50, Front: &bo}}}
	cfg := filepath.Join(t.TempDir(), "c.json")
	if msg := coldDialerGuard(fc, "reality", cfg, false); msg != "" {
		t.Fatalf("a both IP remains, refused: %s", msg)
	}
	fc.LB.Kharej[1].Front = re
	if msg := coldDialerGuard(fc, "reality", cfg, false); msg == "" {
		t.Fatal("last tls-capable IP made pure reality without proof")
	}
	if msg := coldDialerGuard(fc, "reality", cfg, true); msg != "" {
		t.Fatal("--dialer-has-key ignored")
	}
	pure, tlsOK := listenFrontSplit(fc)
	if len(pure) != 2 || len(tlsOK) != 0 {
		t.Fatalf("%v %v", pure, tlsOK)
	}
	if c := pinCode(ownPublicFronts(fc)); c == "" || strings.Contains(c, priv) {
		t.Fatal("pin code")
	}
}
