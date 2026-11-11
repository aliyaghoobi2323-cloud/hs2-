package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

const m5Cfg = `{"mode":"listen","carrier":"mtcp","addr":"0.0.0.0:443","key":"k",
 "lb":{"enabled":true,"mode":"auto","kharej":[{"ip":"10.0.0.1","weight":50},
  {"ip":"10.0.0.2","weight":50,"front":{"kind":"both","target":"www.site.ir:443","server_names":["www.site.ir"],"private_key":"PRIV2","short_ids":["aa"]}}],"iran":[]}}`

func m5Entry(t *testing.T, m map[string]any, ip string) map[string]any {
	t.Helper()
	e, err := ownLBEntry(m, "kharej", ip)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFrontSpecEdit(t *testing.T) {
	m, _ := loadConfigMap([]byte(m5Cfg))
	e := m5Entry(t, m, "10.0.0.1")
	if err := applyFrontSpec(e, frontSpec{Kind: "reality"}); err == nil {
		t.Fatal("reality without a target accepted")
	}
	if err := applyFrontSpec(e, frontSpec{Kind: "reality", Target: "www.example.ir"}); err != nil {
		t.Fatal(err)
	}
	f := e["front"].(map[string]any)
	pk, _ := f["private_key"].(string)
	if f["target"] != "www.example.ir:443" || pk == "" || len(f["short_ids"].([]any)) != 1 || f["server_names"].([]any)[0] != "www.example.ir" {
		t.Fatalf("front %v", f)
	}
	if _, err := tlscarrier.RealityPublicOf(pk); err != nil {
		t.Fatalf("bad key: %v", err)
	}
	// reality -> both keeps the key and the target
	if err := applyFrontSpec(e, frontSpec{Kind: "both"}); err != nil {
		t.Fatal(err)
	}
	if f := e["front"].(map[string]any); f["private_key"] != pk || f["kind"] != "both" || f["target"] != "www.example.ir:443" {
		t.Fatalf("both changed the key/target: %v", f)
	}
	// rotate
	applyFrontSpec(e, frontSpec{Kind: "both", NewKey: true})
	if e["front"].(map[string]any)["private_key"] == pk {
		t.Fatal("key not rotated")
	}
	// tls keeps the key (and target, short ids) in a tls block: switching
	// back needs no new key distribution
	pk2 := e["front"].(map[string]any)["private_key"]
	sid := e["front"].(map[string]any)["short_ids"].([]any)[0]
	applyFrontSpec(e, frontSpec{Kind: "tls"})
	if f, _ := e["front"].(map[string]any); f == nil || f["kind"] != "tls" || f["private_key"] != pk2 {
		t.Fatalf("tls lost the key: %v", e["front"])
	}
	if err := applyFrontSpec(e, frontSpec{Kind: "reality"}); err != nil { // no --target: the kept one
		t.Fatal(err)
	}
	if f := e["front"].(map[string]any); f["kind"] != "reality" || f["private_key"] != pk2 || f["short_ids"].([]any)[0] != sid || f["target"] != "www.example.ir:443" {
		t.Fatalf("back to reality changed the key: %v", f)
	}
	// a block without a key: tls removes it
	e2 := m5Entry(t, m, "10.0.0.1")
	e2["front"] = map[string]any{"kind": "tls"}
	applyFrontSpec(e2, frontSpec{Kind: "tls"})
	if _, ok := e2["front"]; ok {
		t.Fatal("tls kept a key-less front block")
	}
	if _, err := ownLBEntry(m, "kharej", "10.9.9.9"); err == nil {
		t.Fatal("unknown IP accepted")
	}
	if _, err := ownLBEntry(map[string]any{}, "kharej", "10.0.0.1"); err == nil {
		t.Fatal("lb off accepted")
	}
}

// The M4 lb editors round-trip the list through lbIP, whose front never
// marshals the private key: it must survive.
func TestLBEditKeepsPrivateKey(t *testing.T) {
	var fc fileConfig
	if err := json.Unmarshal([]byte(m5Cfg), &fc); err != nil {
		t.Fatal(err)
	}
	nb, err := withOwnList([]byte(m5Cfg), fc, func(l []lbIP) ([]lbIP, error) { return splitEqual(l), nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nb), `"private_key": "PRIV2"`) {
		t.Fatalf("private key lost:\n%s", nb)
	}
}

func tlsTarget(t *testing.T, hdr map[string]string) (addr string) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestProbeTarget(t *testing.T) {
	good := tlsTarget(t, nil)
	cdn := tlsTarget(t, map[string]string{"CF-RAY": "8a1b2c3d-FRA", "Server": "cloudflare"})
	old := targetDialer
	oldRes := targetResolve
	t.Cleanup(func() { targetDialer, targetResolve = old, oldRes })
	targetResolve = func(string) []net.IP { return nil }
	route := map[string]string{"good.example.com:443": good, "cdn.example.com:443": cdn}
	targetDialer = func(ctx context.Context, a string) (net.Conn, error) {
		if r, ok := route[a]; ok {
			return (&net.Dialer{}).DialContext(ctx, "tcp", r)
		}
		return nil, errors.New("connection refused")
	}
	// httptest's cert covers example.com
	r := probeTarget("good.example.com", "example.com", nil)
	if !r.OK || !r.TLS13 || !r.H2 || !r.X25519 || !r.CertCovers || r.CertBytes == 0 {
		t.Fatalf("good target rejected: %+v", r)
	}
	r = probeTarget("cdn.example.com:443", "example.com", nil)
	if r.OK || r.CDN != "Cloudflare" {
		t.Fatalf("CDN target accepted: %+v", r)
	}
	r = probeTarget("down.example.com", "", nil)
	if r.OK || len(r.Bad) == 0 || !strings.Contains(r.Bad[0], "refused") {
		t.Fatalf("down target: %+v", r)
	}
	r = probeTarget("good.example.com", "www.cloudflare.com", nil)
	if r.OK {
		t.Fatal("CDN by name accepted")
	}
	r = probeTarget("good.example.com", "1.2.3.4", nil)
	if r.OK {
		t.Fatal("IP literal SNI accepted")
	}
	for _, c := range []struct{ k, v, want string }{
		{"x-amz-cf-id", "abc", "CloudFront"}, {"x-served-by", "cache-fra-1", "Fastly"}, {"server", "nginx", ""}, {"server", "gws", "Google"},
	} {
		if g := cdnHeader(c.k, c.v); g != c.want {
			t.Errorf("cdnHeader(%s: %s) = %q, want %q", c.k, c.v, g, c.want)
		}
	}
	_ = tls.VersionTLS13
}

// rel48: the suite a target picks is reported, and HS2_REALITY_PREFER_AES128
// ranks AES-128 targets first among otherwise equal ones.
func TestTargetSuiteRank(t *testing.T) {
	good := tlsTarget(t, nil)
	old := targetDialer
	oldRes := targetResolve
	t.Cleanup(func() { targetDialer, targetResolve = old, oldRes })
	targetResolve = func(string) []net.IP { return nil }
	targetDialer = func(ctx context.Context, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", good)
	}
	r := probeTarget("good.example.com", "example.com", nil)
	if !strings.HasPrefix(r.Suite, "TLS_AES_") && !strings.HasPrefix(r.Suite, "TLS_CHACHA20") {
		t.Fatalf("suite not reported: %+v", r)
	}
	a128 := targetCheck{OK: true, Suite: "TLS_AES_128_GCM_SHA256", HandshakeMs: 90}
	a256 := targetCheck{OK: true, Suite: "TLS_AES_256_GCM_SHA384", HandshakeMs: 30}
	t.Setenv("HS2_REALITY_PREFER_AES128", "")
	if targetLess(a128, a256) {
		t.Fatal("default ranking must stay by handshake time")
	}
	t.Setenv("HS2_REALITY_PREFER_AES128", "1")
	if !targetLess(a128, a256) || targetLess(a256, a128) {
		t.Fatal("HS2_REALITY_PREFER_AES128=1: AES-128 target must rank first")
	}
	bad := targetCheck{OK: false, Suite: "TLS_AES_128_GCM_SHA256"}
	if targetLess(bad, a256) {
		t.Fatal("a rejected target never ranks first")
	}
}

// rel48: rotate-sid puts a new id first and keeps only the previous first id.
func TestRotateShortIDs(t *testing.T) {
	l := rotateShortIDs([]any{"0a0a0a0a", "0b0b0b0b"})
	if len(l) != 2 || l[1] != "0a0a0a0a" || l[0] == "0a0a0a0a" || len(l[0].(string)) != 16 {
		t.Fatalf("rotate: %v", l)
	}
	l2 := rotateShortIDs(l)
	if len(l2) != 2 || l2[1] != l[0] {
		t.Fatalf("second rotate must retire the oldest id: %v", l2)
	}
	if l3 := rotateShortIDs(nil); len(l3) != 1 {
		t.Fatalf("no ids: %v", l3)
	}
}

func TestDoctorFronts(t *testing.T) {
	var fc fileConfig
	json.Unmarshal([]byte(m5Cfg), &fc)
	st := []namedStatus{{file: "/run/hs2/hs2-m5.status.json", age: 2}}
	cfgOf := func(string) (fileConfig, bool) { return fc, true }
	down := func(string) error { return errors.New("i/o timeout") }
	up := func(string) error { return nil }
	sev := func(fs []finding, what string) string {
		for _, f := range fs {
			if strings.Contains(f.What, what) {
				return f.Sev
			}
		}
		return ""
	}
	fs := checkFronts(st, cfgOf, down)
	if sev(fs, "unreachable") != "BAD" {
		t.Fatalf("target down not BAD: %+v", fs)
	}
	st[0].ls.Reality = &tlscarrier.RealityStats{Rejected: map[string]int64{"tunnel_auth": 9}}
	fs = checkFronts(st, cfgOf, up)
	if sev(fs, "reachable") != "OK" || sev(fs, "mismatch") != "BAD" {
		t.Fatalf("key mismatch not found: %+v", fs)
	}
	st[0].ls.Reality = &tlscarrier.RealityStats{Authed: 12}
	if fs = checkFronts(st, cfgOf, up); sev(fs, "mismatch") != "" || sev(fs, "12 authenticated") != "OK" {
		t.Fatalf("healthy: %+v", fs)
	}
	// the dialing side: a "both" path in its tls cool-down
	var dc fileConfig
	json.Unmarshal([]byte(`{"mode":"dial","carrier":"mtcp","addr":"10.0.0.1:443"}`), &dc)
	ds := []namedStatus{{file: "/run/hs2/hs2-m5.status.json", ls: liveStatus{FrontPaths: []frontPathStatus{
		{Path: "a>10.0.0.2:443", Kind: "both", Both: &tlscarrier.BothPathStats{Mode: "tls", RealityFail: 4, Fallbacks: 1}}}}}}
	fs = checkFronts(ds, func(string) (fileConfig, bool) { return dc, true }, up)
	if sev(fs, "fell back to tls") != "WARN" {
		t.Fatalf("fallback not reported: %+v", fs)
	}
}

func TestAlertFrontRules(t *testing.T) {
	cfg := defaultAlertCfg()
	cfg.Lang = "en"
	st := newAlertState()
	t0 := time.Date(2026, 11, 11, 3, 0, 0, 0, time.Local)
	snap := func(i int, mode string, fb int64, terr string) alertSnap {
		return alertSnap{Now: t0.Add(time.Duration(i) * time.Minute), Unit: "hs2-m5", Active: "active", UptimeS: 3600, MainPID: 7, NRestarts: 0,
			PSI60: -1, CertDays: -1, TargetErr: map[string]string{"www.site.ir:443": terr},
			Status: &alertStatus{Links: 8, Target: 8, Updated: t0.Unix(), FrontPaths: []frontPathStatus{
				{Path: "a>b", Kind: "both", Both: &tlscarrier.BothPathStats{Mode: mode, Fallbacks: fb}}}}}
	}
	has := func(ms []alertMsg, key, sev string) bool {
		for _, m := range ms {
			if m.Key == key && m.Sev == sev {
				return true
			}
		}
		return false
	}
	evalAlerts(cfg, st, snap(0, "reality", 0, ""))
	if ms := evalAlerts(cfg, st, snap(1, "reality", 3, "")); !has(ms, "fallback:a>b", sevWarn) {
		t.Fatalf("fallback spike not alerted: %+v", ms)
	}
	ms := evalAlerts(cfg, st, snap(2, "reality", 3, "dial tcp: i/o timeout")) // target: need 2
	if !has(ms, "fallback:a>b", sevOK) || has(ms, "rtarget:www.site.ir:443", sevWarn) {
		t.Fatalf("fallback resolve / target streak: %+v", ms)
	}
	ms = evalAlerts(cfg, st, snap(3, "reality", 3, "dial tcp: i/o timeout"))
	if !has(ms, "rtarget:www.site.ir:443", sevWarn) {
		t.Fatalf("target down / fallback resolve: %+v", ms)
	}
	if ms = evalAlerts(cfg, st, snap(4, "tls", 3, "")); !has(ms, "fallback:a>b", sevWarn) || !has(ms, "rtarget:www.site.ir:443", sevOK) {
		t.Fatalf("cool-down / target back: %+v", ms)
	}
	cfg.Disabled = map[string]bool{"fallback": true}
	if alertRuleOf("fallback:a>b") != "fallback" {
		t.Fatal("rule name")
	}
}

func TestApplyReloadMode(t *testing.T) {
	f, cfg := setupApply(t)
	if _, _, err := startApply(f, applyReq{Unit: "hs2-lab", Cfg: cfg, Reason: "front-reality", New: []byte(`{"mode":"dial","max_links":16}`), Within: time.Minute, HealthGate: true, Reload: true}); err != nil {
		t.Fatal(err)
	}
	runWorker(f, "hs2-lab") // never confirmed -> rollback, also by reload
	s, _ := loadState("hs2-lab")
	if s.State != stRolledBack || !s.Reload {
		t.Fatalf("state %+v", s)
	}
	cfgIs(t, cfg, `"max_links":8`)
	if f.restarts != 0 || f.reloads != 2 {
		t.Fatalf("restarts %d reloads %d, want 0 / 2", f.restarts, f.reloads)
	}
}

func TestFrontStateRender(t *testing.T) {
	var fc fileConfig
	json.Unmarshal([]byte(m5Cfg), &fc)
	ls := liveStatus{Fronts: []frontIPStatus{{IP: "10.0.0.1", Kind: "tls"}, {IP: "10.0.0.2", Kind: "both", Counters: &tlscarrier.FrontCountersSnap{Reality: 5, TLS: 1, Cover: 2}}},
		Reality: &tlscarrier.RealityStats{Authed: 5, FallbackConns: 2, Rejected: map[string]int64{"handshake": 1}}}
	j := "Nov 11 hs2[1]: front: 10.0.0.2 now serves both (server_names [www.site.ir], target www.site.ir:443); established links keep running\n"
	st := buildFrontState("hs2-m5", fc, ls, true, j)
	if len(st.IPs) != 2 || st.IPs[1].Configured != "both" || st.IPs[1].InUse != "both" || st.IPs[0].InUse != "tls" || len(st.Fallbacks) != 1 {
		t.Fatalf("%+v", st)
	}
	var b strings.Builder
	deepLang = "fa"
	renderFrontState(&b, st)
	deepLang = "en"
	if !strings.Contains(b.String(), "5/1/2/0") || !strings.Contains(b.String(), "سمت شنونده") {
		t.Fatalf("render:\n%s", b.String())
	}
}

// M6 leftover: delete removed the unit but left <unit>.service.d behind.
func TestFrontDiffOneSidedAndHidden(t *testing.T) {
	if isBilateral("lb.kharej.0.front.kind") || !isBilateral("lb.kharej.0.ip") && !isBilateral("lb") {
		t.Fatal("bilateral rules")
	}
	if !sensitiveKey("lb.kharej.0.front.private_key") || !sensitiveKey("front.private_key") || sensitiveKey("lb.kharej.0.front.public_key") {
		t.Fatal("private keys must be hidden in diffs")
	}
}

func TestFrontPreflight(t *testing.T) {
	var fc fileConfig
	json.Unmarshal([]byte(m5Cfg), &fc) // addr 0.0.0.0:443: announces its explicit list
	priv, _, _ := tlscarrier.RealityKeygen()
	fc.LB.Kharej[1].Front.PrivateKey = priv
	if msg := frontPreflight(fc, "reality", "/etc/hs2/x.json"); msg != "" {
		t.Fatalf("wildcard listener refused: %q", msg)
	}
	bad := fc
	bad.LB = &lbConfig{Enabled: true, Kharej: []lbIP{{IP: "10.0.0.1", Weight: 90}, {IP: "10.0.0.2", Weight: 90}}}
	if msg := frontPreflight(bad, "reality", "/etc/hs2/x.json"); !strings.Contains(msg, "announcement") {
		t.Fatalf("unbuildable announcement accepted: %q", msg)
	}
	fc.Addr = "10.0.0.1:443"
	if msg := frontPreflight(fc, "reality", "/etc/hs2/x.json"); msg != "" {
		t.Fatalf("exact listener refused: %q", msg)
	}
	if msg := frontPreflight(fc, "both", "/etc/hs2/x.json"); !strings.Contains(msg, "sni") {
		t.Fatalf("both without an sni accepted: %q", msg)
	}
	fc.SNI = "t.example.ir"
	if msg := frontPreflight(fc, "both", "/etc/hs2/x.json"); msg != "" {
		t.Fatalf("both with an sni refused: %q", msg)
	}
}

func TestDeleteRemovesDropinDir(t *testing.T) {
	menuEnv(t)
	oc, od := certDir, deletedRoot
	t.Cleanup(func() { certDir, deletedRoot = oc, od })
	certDir = t.TempDir()
	os.MkdirAll(filepath.Join(systemdDir, "hs2-x.service.d"), 0o755)
	os.WriteFile(filepath.Join(systemdDir, "hs2-x.service"), []byte("[Service]\n"), 0o644)
	os.WriteFile(filepath.Join(systemdDir, "hs2-x.service.d", "50-preset.conf"), []byte("[Service]\nEnvironment=A=1\n"), 0o644)
	cfg := configForUnit("hs2-x")
	os.WriteFile(cfg, []byte(`{"mode":"dial"}`), 0o600)
	deletedRoot = t.TempDir()
	dir, err := backupTunnel("hs2-x", cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "hs2-x.service.d", "50-preset.conf")); err != nil || !strings.Contains(string(b), "A=1") {
		t.Fatalf("drop-in not backed up: %v", err)
	}
	// rev53a: its alert timer went on running every 60 s after the delete
	os.WriteFile(filepath.Join(systemdDir, "hs2-alert-hs2-x.timer"), []byte("[Timer]\n"), 0o644)
	os.WriteFile(filepath.Join(systemdDir, "hs2-alert-hs2-x.service"), []byte("[Service]\n"), 0o644)
	os.WriteFile(filepath.Join(systemdDir, "hs2-alert-hs2-y.timer"), []byte("[Timer]\n"), 0o644)
	removeUnitFiles("hs2-x", cfg)
	if _, err := os.Stat(filepath.Join(systemdDir, "hs2-x.service.d")); !os.IsNotExist(err) {
		t.Fatalf("drop-in dir left behind: %v", err)
	}
	for _, f := range []string{"hs2-alert-hs2-x.timer", "hs2-alert-hs2-x.service"} {
		if _, err := os.Stat(filepath.Join(systemdDir, f)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "hs2-alert-hs2-y.timer")); err != nil {
		t.Fatalf("another tunnel's alert timer was removed: %v", err)
	}
}

// M6 leftover: config history marked every version equal to the live
// config as current; only the newest one is.
func TestHistoryOneCurrent(t *testing.T) {
	menuEnv(t)
	cfg := configForUnit("hs2-h")
	a, b := []byte(`{"max_links":8}`), []byte(`{"max_links":16}`)
	now := time.Now()
	saveVersion("hs2-h", a, nil, "pre-apply", now)
	saveVersion("hs2-h", b, nil, "apply", now.Add(time.Second))
	saveVersion("hs2-h", a, nil, "rollback-1", now.Add(2*time.Second))
	os.WriteFile(cfg, a, 0o600)
	vs, _ := listVersions("hs2-h")
	if len(vs) != 3 {
		t.Fatalf("versions %d", len(vs))
	}
	if c := currentVersion("hs2-h", cfg, vs); c != vs[2].N {
		t.Fatalf("current v%d, want v%d", c, vs[2].N)
	}
}
