package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// lbFrontConf: a listening kharej with three IPs: A tls, B reality, C both.
func lbFrontConf(t *testing.T, priv string, bFront, cFront string) string {
	t.Helper()
	if bFront == "" {
		bFront = `{"kind":"reality","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"` + priv + `","short_ids":["a1b2"]}`
	}
	if cFront == "" {
		cFront = `{"kind":"both","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"` + priv + `","short_ids":["c3d4"],"fallback_max_bytes":1000}`
	}
	return `{"mode":"listen","carrier":"l3mtcp","addr":"10.0.0.1:443","sni":"tun.ours.ir","lb":{"enabled":true,
	  "kharej":[{"ip":"10.0.0.1","weight":40},{"ip":"10.0.0.2","weight":30,"front":` + bFront + `},
	            {"ip":"10.0.0.3","weight":30,"sni":"c.ours.ir","front":` + cFront + `}],
	  "iran":[{"ip":"10.9.0.1","weight":100}]}}`
}

func parseFC(t *testing.T, raw string) fileConfig {
	t.Helper()
	var fc fileConfig
	if err := json.Unmarshal([]byte(raw), &fc); err != nil {
		t.Fatal(err)
	}
	return fc
}

// Per-IP profiles: parsed with the private key from this file, validated
// per IP (CDN target, missing keys), and announced WITHOUT any secret.
func TestLBPerIPFrontProfiles(t *testing.T) {
	priv, pub, _ := tlscarrier.RealityKeygen()
	fc := parseFC(t, lbFrontConf(t, priv, "", ""))
	if err := validateLB(fc); err != nil {
		t.Fatal(err)
	}
	if fc.LB.Kharej[1].Front.PrivateKey != priv || fc.LB.Kharej[2].Front.PrivateKey != priv {
		t.Fatal("per-IP private key not read from the file")
	}
	adv := lbOwnAdvert(fc)
	if len(adv) != 3 {
		t.Fatalf("advert %v", adv)
	}
	var w []byte
	for _, e := range adv {
		switch e.IP {
		case "10.0.0.1":
			if e.Front != nil {
				t.Fatalf("tls announced: %+v", e.Front)
			}
		case "10.0.0.2", "10.0.0.3":
			f := e.Front
			if f == nil || f.PublicKey != pub || f.PrivateKey != "" || f.Target != "" || f.FallbackMaxBytes != 0 || len(f.ServerNames) != 1 || len(f.ShortIDs) != 1 {
				t.Fatalf("announced %s: %+v", e.IP, f)
			}
		}
		b, _ := json.Marshal(e)
		w = append(w, b...)
	}
	// the wire form of the whole advert (ext included) carries no secret
	wire := engine.EncodeLBAdvertWire(adv)
	for _, secret := range []string{priv, "www.site.ir:443", "fallback", "target", "private"} {
		if strings.Contains(string(wire), secret) || strings.Contains(string(w), secret) {
			t.Fatalf("advert leaks %q: %s", secret, wire)
		}
	}
	if !strings.Contains(string(wire), pub) {
		t.Fatal("advert lacks the public key")
	}

	// per-IP validation
	for name, raw := range map[string]string{
		"cdn target": lbFrontConf(t, priv, `{"kind":"reality","target":"www.cloudflare.com:443","server_names":["www.cloudflare.com"],"private_key":"`+priv+`","short_ids":["a1"]}`, ""),
		"no private": lbFrontConf(t, priv, `{"kind":"reality","target":"www.site.ir:443","server_names":["www.site.ir"],"public_key":"`+pub+`","short_ids":["a1"]}`, ""),
		"no sids":    lbFrontConf(t, priv, "", `{"kind":"both","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"`+priv+`"}`),
		"bad kind":   lbFrontConf(t, priv, `{"kind":"quic"}`, ""),
		"foreign":    lbFrontConf(t, priv, `{"kind":"reality","target":"www.site.ir:443","server_names":["other.ir"],"private_key":"`+priv+`","short_ids":["a1"]}`, ""),
	} {
		if err := validateLB(parseFC(t, raw)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "10.0.0.") {
			t.Errorf("%s: error does not name the IP: %v", name, err)
		}
	}
	// dialing side: an explicit remote reality front needs a key
	d := parseFC(t, strings.Replace(lbFrontConf(t, priv, `{"kind":"reality","server_names":["www.site.ir"],"short_ids":["a1"]}`, ""), `"mode":"listen"`, `"mode":"dial","bind_local_ip":"10.9.0.1"`, 1))
	if err := validateLB(d); err == nil || !strings.Contains(err.Error(), "public_key") {
		t.Errorf("dialer: reality without a key: %v", err)
	}
	// the tunnel-level front cannot be "both"
	if err := validateFront(fileConfig{Mode: "listen", Carrier: "l3mtcp", Front: &tlscarrier.FrontProfile{Kind: "both",
		ServerNames: []string{"a.ir"}, PrivateKey: priv, ShortIDs: []string{"a1"}, Target: "a.ir:443"}}); err == nil {
		t.Error("tunnel both accepted")
	}
}

// Precedence on the dialing side: explicit (this side's remote list) >
// announced > the tunnel's front; nothing announced (old peer) -> tls.
func TestLBFrontPrecedence(t *testing.T) {
	priv, pub, _ := tlscarrier.RealityKeygen()
	ann := &tlscarrier.FrontProfile{Kind: "reality", ServerNames: []string{"ann.ir"}, PublicKey: pub, ShortIDs: []string{"aa"}}
	explicit := `{"kind":"both","server_names":["mine.ir"],"public_key":"` + pub + `","short_ids":["bb"]}`
	raw := `{"mode":"dial","carrier":"l3mtcp","addr":"10.0.0.1:443","bind_local_ip":"10.9.0.1","lb":{"enabled":true,
	  "kharej":[{"ip":"10.0.0.1","weight":40},{"ip":"10.0.0.2","weight":30,"front":` + explicit + `},{"ip":"10.0.0.3","weight":30}],
	  "iran":[{"ip":"10.9.0.1","weight":100}]}}`
	fc := parseFC(t, raw)
	if err := validateLB(fc); err != nil {
		t.Fatal(err)
	}
	adv := []engine.LBAdvertIP{{IP: "10.0.0.1", Weight: 40}, {IP: "10.0.0.2", Weight: 30, Front: ann}, {IP: "10.0.0.3", Weight: 30, Front: ann}}
	paths, err := lbPathsAdvert(fc, adv)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range paths {
		f := pathFront(fc, p.Front)
		k := f.NormKind()
		if f != nil && len(f.ServerNames) > 0 {
			k += ":" + f.ServerNames[0]
		}
		got[p.Remote] = k
	}
	want := map[string]string{"10.0.0.1": "tls", "10.0.0.2": "both:mine.ir", "10.0.0.3": "reality:ann.ir"}
	for ip, k := range want {
		if got[ip] != k {
			t.Fatalf("%s: %s, want %s (all %v)", ip, got[ip], k, got)
		}
	}
	// old listener: no announcement at all -> the local list -> tls (or explicit)
	paths, _ = lbPathsAdvert(fc, nil)
	for _, p := range paths {
		if p.Remote == "10.0.0.3" && pathFront(fc, p.Front) != nil {
			t.Fatalf("no announcement: %+v", p.Front)
		}
	}
	// the tunnel's front is the last resort
	fc.Front = &tlscarrier.FrontProfile{Kind: "reality", ServerNames: []string{"tun.ir"}, PublicKey: pub, ShortIDs: []string{"cc"}}
	if f := pathFront(fc, nil); f == nil || f.ServerNames[0] != "tun.ir" {
		t.Fatalf("tunnel default: %+v", f)
	}
	// a dialer config copied from the listener (private key only): public derived
	if f := pathFront(fc, &tlscarrier.FrontProfile{Kind: "reality", ServerNames: []string{"x.ir"}, PrivateKey: priv, ShortIDs: []string{"a1"}}); f.PublicKey != pub || f.PrivateKey != "" {
		t.Fatalf("derived %+v", f)
	}
}

type tagAcc struct {
	name string
	got  chan string
}

func (a *tagAcc) AcceptFront(_ context.Context, raw net.Conn, _ func(*tlscarrier.Carrier)) {
	raw.Close()
	a.got <- a.name
}

// The listening side picks the front by the local IP; a reload swaps the
// table for new connections only and keeps an unchanged listener.
func TestFrontMuxDispatchAndReload(t *testing.T) {
	priv, _, _ := tlscarrier.RealityKeygen()
	fc := parseFC(t, lbFrontConf(t, priv, "", ""))
	got := make(chan string, 4)
	def := &tagAcc{name: "default", got: got}
	m := newFrontMux(fc, def, &tlscarrier.Server{SharedKey: []byte("0123456789abcdef0123456789abcdef")}, quietLog)
	defer curFrontMux.Store(nil)
	tab := *m.byIP.Load()
	if len(tab) != 2 || tab["10.0.0.2"].kind != "reality" || tab["10.0.0.3"].kind != "both" || tab["10.0.0.3"].rs.TLS == nil {
		t.Fatalf("table %+v", tab)
	}
	if !tab["10.0.0.3"].rs.TLSName(nil, "C.OURS.IR") || !tab["10.0.0.3"].rs.TLSName(nil, "tun.ours.ir") || tab["10.0.0.3"].rs.TLSName(nil, "www.site.ir") {
		t.Fatal("tls names")
	}
	// dispatch by local IP: an IP without a front -> the tunnel's
	m.AcceptFront(context.Background(), fakeConn{local: "10.0.0.1"}, nil)
	if <-got != "default" {
		t.Fatal("default dispatch")
	}
	bRS := tab["10.0.0.2"].rs
	// reload: B unchanged, C both -> reality, A tls -> both
	fc2 := parseFC(t, strings.Replace(lbFrontConf(t, priv, "", `{"kind":"reality","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"`+priv+`","short_ids":["c3d4"]}`),
		`{"ip":"10.0.0.1","weight":40}`, `{"ip":"10.0.0.1","weight":40,"front":{"kind":"both","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"`+priv+`","short_ids":["e5"]}}`, 1))
	if err := validateLB(fc2); err != nil {
		t.Fatal(err)
	}
	m.rebuild(fc2)
	tab2 := *m.byIP.Load()
	if tab2["10.0.0.2"].rs != bRS {
		t.Fatal("unchanged listener rebuilt (replay memory lost)")
	}
	if tab2["10.0.0.3"].kind != "reality" || tab2["10.0.0.1"].kind != "both" {
		t.Fatalf("after reload %+v", tab2)
	}
	if st := frontStatusNow(); len(st) != 3 {
		t.Fatalf("status %+v", st)
	}
}

type fakeConn struct {
	net.Conn
	local string
}

func (f fakeConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.ParseIP(f.local), Port: 443} }
func (f fakeConn) Close() error        { return nil }

// Reload through the SIGHUP path (reloadLB) switches the fronts in place.
func TestReloadLBSwitchesFronts(t *testing.T) {
	priv, _, _ := tlscarrier.RealityKeygen()
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	raw := lbFrontConf(t, priv, "", "")
	os.WriteFile(path, []byte(raw), 0o600)
	fc := parseFC(t, raw)
	m := newFrontMux(fc, &tagAcc{got: make(chan string, 1)}, &tlscarrier.Server{SharedKey: []byte("0123456789abcdef0123456789abcdef")}, quietLog)
	defer curFrontMux.Store(nil)
	oldPath := configPath
	configPath = path
	defer func() { configPath = oldPath }()
	lbRunMu.Lock()
	prev := lbRun
	lbRun = &lbRuntime{fc: fc, logf: quietLog}
	lbRunMu.Unlock()
	defer func() { lbRunMu.Lock(); lbRun = prev; lbRunMu.Unlock() }()
	os.WriteFile(path, []byte(strings.Replace(raw, `"kind":"both"`, `"kind":"reality"`, 1)), 0o600)
	reloadLB("test")
	if k := (*m.byIP.Load())["10.0.0.3"].kind; k != "reality" {
		t.Fatalf("after SIGHUP reload: %s", k)
	}
}

// quietLog: the size-mirror goroutines may log after a test ends (t.Logf
// would panic then).
func quietLog(string, ...any) {}
