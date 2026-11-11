package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The four roles: reverse iran (listens, announces), reverse kharej (dials),
// direct kharej (listens, announces), direct iran (dials).
const (
	lbIranRevListen = `{"mode":"dial","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443",
 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}]}}`
	lbKharejDirListen = `{"mode":"listen","carrier":"mtcp","addr":"10.1.0.1:443",
 "lb":{"enabled":true,"kharej":[{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}]}}`
	lbIranDirDial = `{"mode":"dial","carrier":"mtcp","addr":"10.1.0.1:443","bind_local_ip":"10.0.0.1",
 "lb":{"enabled":true,"iran":[{"ip":"10.0.0.1","weight":100}],
       "kharej":[{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}]}}`
)

func TestLBOwnAdvertRoles(t *testing.T) {
	for _, c := range []struct {
		name, js string
		want     bool
	}{
		{"reverse iran listens", lbIranRevListen, true},
		{"direct kharej listens", lbKharejDirListen, true},
		{"reverse kharej dials", lbKharejRev, false},
		{"direct iran dials", lbIranDirDial, false},
	} {
		got := lbOwnAdvert(lbFC(t, c.js))
		if c.want != (got != nil) {
			t.Errorf("%s: advert %v", c.name, got)
		}
		if c.want && (len(got) != 2 || got[0] != (engine.LBAdvertIP{IP: "10.1.0.1", Weight: 70})) {
			t.Errorf("%s: advert %v", c.name, got)
		}
	}
	// a wildcard listen (what `hs2 tunnel create` writes) announces its
	// explicit list, so per-IP fronts reach the dialer
	for _, w := range []string{"0.0.0.0:443", "[::]:443", ":443"} {
		a := lbOwnAdvert(lbFC(t, strings.Replace(lbKharejDirListen, `"addr":"10.1.0.1:443"`, `"addr":"`+w+`"`, 1)))
		if len(a) != 2 || a[0] != (engine.LBAdvertIP{IP: "10.1.0.1", Weight: 70}) || a[1] != (engine.LBAdvertIP{IP: "10.1.0.2", Weight: 30}) {
			t.Errorf("wildcard %s announced %v", w, a)
		}
	}
	// ... and nothing with an empty list (the peer keeps its local list)
	if a := lbOwnAdvert(lbFC(t, `{"mode":"listen","carrier":"mtcp","addr":"0.0.0.0:443","lb":{"enabled":true,"kharej":[]}}`)); a != nil {
		t.Errorf("empty wildcard list announced %v", a)
	}
}

// A wildcard listener's list is announced as written when it sums to 100,
// else scaled to 100; its per-IP fronts are carried (public part only).
func TestLBWildcardAdvert(t *testing.T) {
	for _, c := range []struct {
		ws, want []int
	}{
		{[]int{70, 30}, []int{70, 30}},
		{[]int{50}, []int{100}},
		{[]int{40, 20}, []int{67, 33}},
		{[]int{0, 0, 0}, []int{34, 33, 33}},
		{[]int{0, 100}, []int{0, 100}},
	} {
		var l []lbIP
		for i, w := range c.ws {
			l = append(l, lbIP{IP: fmt.Sprintf("10.2.0.%d", i+1), Weight: w})
		}
		ips, w, err := lbWildcardAdvert(l)
		if err != nil || len(ips) != len(c.want) || fmt.Sprint(w) != fmt.Sprint(c.want) {
			t.Errorf("%v: %v %v %v", c.ws, ips, w, err)
		}
	}
	if _, _, err := lbWildcardAdvert([]lbIP{{IP: "10.2.0.1", Weight: 80}, {IP: "10.2.0.2", Weight: 80}}); err == nil {
		t.Error("sum 160 accepted")
	}
	priv, _, _ := tlscarrier.RealityKeygen()
	pub, _ := tlscarrier.RealityPublicOf(priv)
	fc := lbFC(t, `{"mode":"listen","carrier":"mtcp","addr":"0.0.0.0:443","lb":{"enabled":true,"kharej":[{"ip":"10.2.0.1","weight":50},
	 {"ip":"10.2.0.2","weight":50,"front":{"kind":"reality","target":"www.site.ir:443","server_names":["www.site.ir"],"short_ids":["aa"]}}]}}`)
	fc.LB.Kharej[1].Front.PrivateKey = priv
	a := lbOwnAdvert(fc)
	if len(a) != 2 || a[0].Front != nil || a[1].Front == nil || a[1].Front.Kind != "reality" || a[1].Front.PublicKey != pub ||
		a[1].Front.PrivateKey != "" || a[1].Front.Target != "" {
		t.Fatalf("wildcard advert fronts: %v", a)
	}
	// the dialer takes the front from that announcement
	ps, err := lbPathsAdvert(lbFC(t, lbIranDirDialW), a)
	if err != nil || len(ps) != 2 || ps[1].Remote != "10.2.0.2" || ps[1].Front == nil || ps[1].Front.PublicKey != pub || ps[0].Front != nil {
		t.Fatalf("dialer paths from a wildcard advert: %+v %v", ps, err)
	}
	// a tls block that keeps a reality key announces nothing for that IP
	fc.LB.Kharej[1].Front.Kind = "tls"
	if a := lbOwnAdvert(fc); len(a) != 2 || a[1].Front != nil {
		t.Fatalf("kept key announced: %v", a)
	}
}

const lbIranDirDialW = `{"mode":"dial","carrier":"mtcp","addr":"10.2.0.1:443","bind_local_ip":"10.0.0.1",
 "lb":{"enabled":true,"iran":[{"ip":"10.0.0.1","weight":100}],"kharej":[]}}`

// fakeLNSet is a ListenerSet that records removals.
type fakeLNSet struct {
	net.Listener
	ips     []string
	removed []string
}

func (f *fakeLNSet) AddListener(ln net.Listener) { ln.Close() }
func (f *fakeLNSet) RemoveListener(ip string) bool {
	f.removed = append(f.removed, ip)
	return true
}
func (f *fakeLNSet) ListenIPs() []string { return f.ips }

// A wildcard addr's listener shows up as "::" (or "0.0.0.0") whatever addr
// says: an lb reload must never close it; a removed exact address is closed.
func TestSyncListenersKeepsWildcard(t *testing.T) {
	for _, c := range []struct{ addr, member string }{
		{"0.0.0.0:443", "::"}, {"0.0.0.0:443", "0.0.0.0"}, {":443", "::"}, {"[::]:443", "::"}, {"[::]:443", "0.0.0.0"},
	} {
		fc := lbFC(t, `{"mode":"listen","carrier":"mtcp","addr":"`+c.addr+`","lb":{"enabled":true,"kharej":[{"ip":"10.3.0.1","weight":100}]}}`)
		ln := &fakeLNSet{ips: []string{c.member, "10.3.0.9"}}
		rt := &lbRuntime{fc: fc, ln: ln, logf: func(string, ...any) {}}
		rt.syncListenersLocked()
		if len(ln.removed) != 1 || ln.removed[0] != "10.3.0.9" {
			t.Errorf("addr %s member %s: removed %v", c.addr, c.member, ln.removed)
		}
	}
	// an exact addr: its own listener stays too
	ln := &fakeLNSet{ips: []string{"10.1.0.1", "10.1.0.2", "10.1.0.7"}}
	rt := &lbRuntime{fc: lbFC(t, lbKharejDirListen), ln: ln, logf: func(string, ...any) {}}
	rt.syncListenersLocked()
	if fmt.Sprint(ln.removed) != "[10.1.0.7]" {
		t.Errorf("exact addr: removed %v", ln.removed)
	}
}

// The dialing side, in both modes, takes the remote addresses from the
// announcement and keeps its own source addresses; the primary stays.
func TestLBPathsAdvertBothModes(t *testing.T) {
	for _, js := range []string{lbKharejRev, lbIranDirDial} {
		fc := lbFC(t, js)
		adv := []engine.LBAdvertIP{{IP: "10.1.0.1", Weight: 20}, {IP: "10.1.0.2", Weight: 30}, {IP: "10.1.0.3", Weight: 50}}
		ps, err := lbPathsAdvert(fc, adv)
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) != 3 || !ps[0].Primary || ps[0].Local != "10.0.0.1" || ps[2].Addr != "10.1.0.3:443" ||
			ps[0].Weight != 2000 || ps[2].Weight != 5000 || ps[1].Source != "advert" {
			t.Fatalf("%s: %+v", fc.Mode, ps)
		}
		if n := lbAdvertNote(fc, adv); !strings.Contains(n, "announcement is used") {
			t.Errorf("no note for a differing local list: %q", n)
		}
		// the announcement leaves out the primary: it stays, with an equal share
		ps, err = lbPathsAdvert(fc, []engine.LBAdvertIP{{IP: "10.1.0.2", Weight: 100}})
		if err != nil || len(ps) != 2 || !ps[0].Primary || ps[0].Remote != "10.1.0.1" || ps[0].Source != "local" {
			t.Fatalf("primary must stay: %+v %v", ps, err)
		}
		// invalid announcements are refused
		if _, err := lbPathsAdvert(fc, []engine.LBAdvertIP{{IP: "10.1.0.2", Weight: 50}, {IP: "10.1.0.2", Weight: 50}}); err == nil {
			t.Fatal("duplicate accepted")
		}
		// no announcement (an older peer): stage 5, the local list
		ps, err = lbPathsAdvert(fc, nil)
		if err != nil || len(ps) != 2 || ps[1].Remote != "10.1.0.2" || ps[1].Source != "local" {
			t.Fatalf("local paths %+v %v", ps, err)
		}
	}
}

func writeCfg(t *testing.T, path, js string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resetLBRun(t *testing.T, cfg string) {
	old := configPath
	configPath = cfg
	t.Cleanup(func() {
		configPath = old
		lbRunMu.Lock()
		lbRun = nil
		lbRunMu.Unlock()
	})
}

// Dialing side (both modes): SIGHUP re-reads the source weights; the
// announced remote list stays; a bad edit is refused.
func TestLBReloadDialSide(t *testing.T) {
	for _, js := range []string{lbKharejRev, lbIranDirDial} {
		cfg := filepath.Join(t.TempDir(), "c.json")
		writeCfg(t, cfg, js)
		resetLBRun(t, cfg)
		fc := lbFC(t, js)
		lb := newLB(fc, t.Logf)
		adv := setupLB(fc, lb, nil, t.Logf)
		if adv == nil || lbStatusNow().Source != "local" {
			t.Fatal("setup")
		}
		curLB().applyAdvert([]engine.LBAdvertIP{{IP: "10.1.0.1", Weight: 50}, {IP: "10.1.0.3", Weight: 50}})
		if s := lbStatusNow(); s.Source != "advert" || s.Note == "" {
			t.Fatalf("status %+v", s)
		}
		side := `"iran"`
		if fc.Mode == "listen" {
			side = `"kharej"`
		}
		nj := strings.Replace(js, side+`:[{"ip":"10.0.0.1","weight":100}]`, side+`:[{"ip":"10.0.0.1","weight":50},{"ip":"10.0.0.2","weight":50}]`, 1)
		if nj == js {
			t.Fatal("test config not changed")
		}
		writeCfg(t, cfg, nj)
		reloadLB("test")
		var names []string
		for _, p := range lb.Paths() {
			names = append(names, p.Name())
		}
		want := "10.0.0.1>10.1.0.1 10.0.0.1>10.1.0.3 10.0.0.2>10.1.0.1 10.0.0.2>10.1.0.3"
		if strings.Join(names, " ") != want {
			t.Fatalf("%s: paths %v", fc.Mode, names)
		}
		writeCfg(t, cfg, strings.Replace(nj, `"weight":50}]`, `"weight":40}]`, 1))
		reloadLB("test")
		if len(lb.Paths()) != 4 {
			t.Fatalf("bad reload changed the paths: %+v", lb.Paths())
		}
	}
}

// Listening side (both modes): SIGHUP opens a listener for an added address
// and closes the one of a removed address (never addr's); the new list is
// announced.
func TestLBReloadListenSide(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("no 127.0.0.2")
	}
	probe.Close()
	for _, base := range []string{lbIranRevListen, lbKharejDirListen} {
		pl, _ := net.Listen("tcp", "127.0.0.1:0")
		_, port, _ := net.SplitHostPort(pl.Addr().String())
		pl.Close()
		one := strings.NewReplacer("10.1.0.1:443", "127.0.0.1:"+port,
			`[{"ip":"10.1.0.1","weight":70},{"ip":"10.1.0.2","weight":30}]`, `[{"ip":"127.0.0.1","weight":100}]`).Replace(base)
		two := strings.Replace(one, `[{"ip":"127.0.0.1","weight":100}]`, `[{"ip":"127.0.0.1","weight":40},{"ip":"127.0.0.2","weight":60}]`, 1)
		cfg := filepath.Join(t.TempDir(), "c.json")
		writeCfg(t, cfg, one)
		resetLBRun(t, cfg)
		fc := lbFC(t, one)
		ln, err := listenLB(fc, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err == net.ErrClosed {
					return
				}
				if err == nil {
					c.Close()
				}
			}
		}()
		adv := setupLB(fc, nil, ln, t.Logf)
		writeCfg(t, cfg, two)
		reloadLB("test")
		if c, err := net.Dial("tcp", "127.0.0.2:"+port); err != nil {
			t.Fatalf("%s: added address not listening: %v", fc.Mode, err)
		} else {
			c.Close()
		}
		if m := adv.Mine(); fmt.Sprint(m) != "[{127.0.0.1 40} {127.0.0.2 60}]" {
			t.Fatalf("%s: announced %v", fc.Mode, m)
		}
		if s := lbStatusNow(); s.Source != "announces" || !s.Reload {
			t.Fatalf("status %+v", s)
		}
		writeCfg(t, cfg, one)
		reloadLB("test")
		if c, err := net.Dial("tcp", "127.0.0.2:"+port); err == nil {
			c.Close()
			t.Fatalf("%s: removed address still listening", fc.Mode)
		}
		if c, err := net.Dial("tcp", "127.0.0.1:"+port); err != nil {
			t.Fatalf("%s: primary closed: %v", fc.Mode, err)
		} else {
			c.Close()
		}
		if m := adv.Mine(); fmt.Sprint(m) != "[{127.0.0.1 100}]" {
			t.Fatalf("%s: announced %v", fc.Mode, m)
		}
		ln.Close()
	}
}
