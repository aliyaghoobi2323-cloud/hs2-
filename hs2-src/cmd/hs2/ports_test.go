package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// Per-port routing, operator side: validation, the table both servers show,
// `hs2 ports` edits, `hs2 config` keys, status and doctor.

func kharejCfg(t *testing.T, extra string) string {
	t.Helper()
	c, k := writeCert(t, time.Now().Add(60*24*time.Hour))
	return fmt.Sprintf(`{"mode": "listen", "carrier": "mtcp", "addr": "0.0.0.0:2096", "shared_key": %q,
		"cert_file": %q, "key_file": %q%s}`, testKey, c, k, extra)
}

const iranCfg = `{"mode": "dial", "carrier": "mtcp", "addr": "1.2.3.4:2096", "sni": "t.example",
	"shared_key": "` + testKey + `", "forward_ports": "8443"%s}`

func TestCheckExitTable(t *testing.T) {
	cases := []struct {
		name, cfg      string
		wantErr, wantW string
	}{
		{"port_map only", kharejCfg(t, `, "port_map": "2053,2083=10.0.0.5:443"`), "", "an Iran user port without a \"port_map\" entry is refused"},
		{"neither (mtcp)", kharejCfg(t, ``), `or give the Iran user ports their own targets in "port_map"`, ""},
		{"bad entry", kharejCfg(t, `, "expose": "127.0.0.1:8443", "port_map": "2053=nohost"`), `"port_map": "2053=nohost"`, ""},
		{"duplicate", kharejCfg(t, `, "expose": "127.0.0.1:8443", "port_map": "2053,2053=1.2.3.4:5"`), "listed twice", ""},
		{"iran with port_map", fmt.Sprintf(iranCfg, `, "port_map": "2053"`), "", "no effect on the Iran server"},
		{"udp carrier with port_map", `{"mode": "listen", "carrier": "udp", "addr": "0.0.0.0:2096", "shared_key": "` + testKey + `", "port_map": "2053"}`, "", "IP tunnel with no user ports"},
		{"dgtun target on the tag port", `{"mode": "listen", "carrier": "dgtun", "addr": "0.0.0.0:2096", "shared_key": "` + testKey + `",
			"expose": "127.0.0.1:8443", "port_map": "2053=127.0.0.1:28444"}`, "", `"port_map" 2053 uses port 28444`},
	}
	for _, c := range cases {
		errs, warns := checkConfig([]byte(c.cfg), localIs("1.2.3.4"), time.Now())
		all := strings.Join(errs, "\n")
		if c.wantErr == "" && len(errs) > 0 {
			t.Errorf("%s: unexpected errors: %s", c.name, all)
		}
		if c.wantErr != "" && !strings.Contains(all, c.wantErr) {
			t.Errorf("%s: errors %q, want %q", c.name, all, c.wantErr)
		}
		if c.wantW != "" && !strings.Contains(strings.Join(warns, "\n"), c.wantW) {
			t.Errorf("%s: warnings %q, want %q", c.name, warns, c.wantW)
		}
	}
	// An existing kharej config (expose only) is exactly as valid as before.
	if errs, warns := checkConfig([]byte(kharejCfg(t, `, "expose": "127.0.0.1:8443"`)), localIs(), time.Now()); len(errs)+len(warns) > 0 {
		t.Errorf("expose-only config: %v %v", errs, warns)
	}
}

func TestPortLinesIran(t *testing.T) {
	fc := fileConfig{Mode: "dial", Carrier: "mtcp", ForwardPorts: "8443,2053,2083", UDP: true}
	known := peerView{State: "known", Tags: true, Ports: []int{2053, 2096}, Default: true, Running: true, Links: 4}
	got := strings.Join(portLines(fc, known), "\n")
	for _, want := range []string{
		"8443, 2053, 2083 (tcp+udp) — where each one goes is set on the Kharej server",
		"8443 → the Kharej server's default panel",
		"2053 → its own target on the Kharej server",
		"2083 → the Kharej server's default panel",
		"also has a target for 2096 — not opened here",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	known.Default = false
	got = strings.Join(portLines(fc, known), "\n")
	if !strings.Contains(got, "8443 → NO target on the Kharej server") || !strings.Contains(got, "Tunnel manager → Ports → add 8443, 2083") {
		t.Errorf("no default:\n%s", got)
	}
	filtered := strings.Join(portLines(fileConfig{Mode: "dial", Carrier: "dgtun", ForwardPorts: "8443,2053"}, peerView{State: "filtered", Running: true, Links: 2}), "\n")
	if !strings.Contains(filtered, "per-port routing is OFF") || !strings.Contains(filtered, "allow 28444 on its tun") {
		t.Errorf("filtered:\n%s", filtered)
	}
	older := strings.Join(portLines(fc, peerView{State: "older", Running: true, Links: 2}), "\n")
	if !strings.Contains(older, "runs an older hs2 that does not route by port") {
		t.Errorf("older:\n%s", older)
	}
	for pv, want := range map[*peerView]string{
		{Running: false}:          "the tunnel is not running",
		{Running: true, Links: 0}: "no link is up",
	} {
		if got := strings.Join(portLines(fc, *pv), "\n"); !strings.Contains(got, want) {
			t.Errorf("unknown %+v:\n%s", *pv, got)
		}
	}
	if got := portLines(fileConfig{Mode: "dial", Carrier: "l3mtcp"}, known); !strings.Contains(got[0], "pure routed") {
		t.Errorf("no ports: %v", got)
	}
	if got := portLines(fileConfig{Mode: "dial", Carrier: "udp", ForwardPorts: "8443"}, known); got != nil {
		t.Errorf("an IP-tunnel carrier has no ports section: %v", got)
	}
}

func TestPortLinesKharej(t *testing.T) {
	fc := fileConfig{Mode: "listen", Carrier: "mtcp", Expose: "127.0.0.1:8443", PortMap: "2053,2096=10.0.0.5:443"}
	known := peerView{State: "known", Ports: []int{8443, 2053, 2083}, UDP: false, Running: true, Links: 3}
	got := strings.Join(portLines(fc, known), "\n")
	for _, want := range []string{
		"the Iran server opens 8443, 2053, 2083 (tcp); each goes to:",
		"8443 → 127.0.0.1:8443 (default panel, expose)",
		"2053 → 127.0.0.1:2053 (port_map)",
		"2083 → 127.0.0.1:8443 (default panel, expose)",
		"2096 → 10.0.0.5:443 (port_map) — the Iran server does not open this port",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	strict := fc
	strict.Expose = ""
	got = strings.Join(portLines(strict, known), "\n")
	if !strings.Contains(got, "8443 → NONE: its connections are refused") || !strings.Contains(got, "add 8443, 2083 here") {
		t.Errorf("strict:\n%s", got)
	}
	unknown := strings.Join(portLines(fc, peerView{Running: true, Links: 3}), "\n")
	for _, want := range []string{"the Iran server runs an older hs2", "2053 → 127.0.0.1:2053 (port_map)", "every other port → 127.0.0.1:8443"} {
		if !strings.Contains(unknown, want) {
			t.Errorf("unknown: missing %q in:\n%s", want, unknown)
		}
	}
	if got := portLines(fileConfig{Mode: "listen", Carrier: "l3mtcp"}, known); !strings.Contains(got[0], "pure routed") {
		t.Errorf("nothing: %v", got)
	}
	none := strings.Join(portLines(fc, peerView{State: "known", Running: true, Links: 2}), "\n")
	if !strings.Contains(none, "the Iran server opens no user ports") || strings.Contains(none, "opens  (") {
		t.Errorf("an Iran server with no user port:\n%s", none)
	}
}

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadFC(t *testing.T, p string) fileConfig {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		t.Fatal(err)
	}
	return fc
}

func edit(t *testing.T, p string, args ...string) (string, error) {
	t.Helper()
	return editPorts(p, loadFC(t, p), args)
}

func TestPortsEditIran(t *testing.T) {
	p := writeTmp(t, fmt.Sprintf(iranCfg, ""))
	if msg, err := edit(t, p, "add", "2053"); err != nil || !strings.Contains(msg, "user port 2053 added (forward_ports = 8443,2053)") || !strings.Contains(msg, "restart the tunnel") {
		t.Fatalf("add: %q %v", msg, err)
	}
	if _, err := edit(t, p, "add", "2053"); err == nil {
		t.Fatal("adding an open port twice must fail")
	}
	if _, err := edit(t, p, "add", "2083=127.0.0.1:2083"); err == nil || !strings.Contains(err.Error(), "set on the Kharej server") {
		t.Fatalf("a target on the Iran side: %v", err)
	}
	if _, err := edit(t, p, "add", "70000"); err == nil {
		t.Fatal("bad port accepted")
	}
	if msg, err := edit(t, p, "udp", "on"); err != nil || !strings.Contains(msg, "UDP forwarding on the user ports: on") || !loadFC(t, p).UDP {
		t.Fatalf("udp on: %q %v", msg, err)
	}
	if _, err := edit(t, p, "remove", "8443"); err != nil || loadFC(t, p).ForwardPorts != "2053" {
		t.Fatalf("remove: %v (%q)", err, loadFC(t, p).ForwardPorts)
	}
	if _, err := edit(t, p, "remove", "2053"); err == nil || !strings.Contains(err.Error(), "NOT saved") {
		t.Fatalf("mtcp with no user port left must be refused: %v", err)
	}
	if loadFC(t, p).ForwardPorts != "2053" {
		t.Fatal("a refused change touched the file")
	}
	if _, err := edit(t, p, "default", "127.0.0.1:1"); err == nil {
		t.Fatal("the Iran server has no default panel to set")
	}
}

func TestPortsEditKharej(t *testing.T) {
	p := writeTmp(t, kharejCfg(t, `, "expose": "127.0.0.1:8443"`))
	if msg, err := edit(t, p, "add", "2053"); err != nil || !strings.Contains(msg, "user port 2053 now goes to 127.0.0.1:2053") {
		t.Fatalf("add bare: %q %v", msg, err)
	}
	if _, err := edit(t, p, "add", "2083=10.0.0.5:443"); err != nil {
		t.Fatal(err)
	}
	if got := loadFC(t, p).PortMap; got != "2053,2083=10.0.0.5:443" {
		t.Fatalf("port_map %q", got)
	}
	if msg, err := edit(t, p, "add", "2053=127.0.0.1:9000"); err != nil || !strings.Contains(msg, "(was 127.0.0.1:2053)") {
		t.Fatalf("replace: %q %v", msg, err)
	}
	if _, err := edit(t, p, "add", "2053=nohost"); err == nil {
		t.Fatal("bad target accepted")
	}
	if msg, err := edit(t, p, "remove", "2083"); err != nil || !strings.Contains(msg, "goes to the default panel 127.0.0.1:8443") {
		t.Fatalf("remove: %q %v", msg, err)
	}
	if _, err := edit(t, p, "remove", "2083"); err == nil {
		t.Fatal("removing a port with no entry must fail")
	}
	if msg, err := edit(t, p, "default", "none"); err != nil || !strings.Contains(msg, "refused") {
		t.Fatalf("default none: %q %v", msg, err)
	}
	if _, err := edit(t, p, "remove", "2053"); err == nil || !strings.Contains(err.Error(), "NOT saved") {
		t.Fatalf("mtcp with no target at all must be refused: %v", err)
	}
	if _, err := edit(t, p, "default", "127.0.0.1:8443"); err != nil {
		t.Fatal(err)
	}
	if _, err := edit(t, p, "remove", "2053"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "port_map") {
		t.Fatalf("an emptied port_map must leave the config:\n%s", b)
	}
	if _, err := edit(t, p, "udp", "on"); err == nil {
		t.Fatal("UDP forwarding is switched on the Iran server")
	}
	if _, err := edit(t, p, "add", "2053=127.0.0.1:1,8443"); err == nil || !strings.Contains(err.Error(), "one port per add") {
		t.Fatalf("a list in one add must be refused, not half-applied: %v", err)
	}
	// A hand-broken port_map is never rewritten (its other entries would be lost).
	broken := writeTmp(t, kharejCfg(t, `, "expose": "127.0.0.1:8443", "port_map": "2053,2083=oops"`))
	if _, err := edit(t, broken, "add", "2096"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("add on a broken port_map: %v", err)
	}
	if got := loadFC(t, broken).PortMap; got != "2053,2083=oops" {
		t.Fatalf("broken port_map was rewritten to %q", got)
	}
}

func TestConfigSetPortKeys(t *testing.T) {
	p := writeTmp(t, fmt.Sprintf(iranCfg, ""))
	m, _ := readConfigMap(p)
	if err := setKey(m, configKeys["forward_ports"], " 8443 , 2053,"); err != nil || m["forward_ports"] != "8443,2053" {
		t.Fatalf("forward_ports: %v %v", m["forward_ports"], err)
	}
	if err := setKey(m, configKeys["udp"], "true"); err != nil || m["udp"] != true {
		t.Fatalf("udp: %v %v", m["udp"], err)
	}
	if err := setKey(m, configKeys["udp"], "maybe"); err == nil {
		t.Fatal("udp accepted a non-bool")
	}
	if err := setKey(m, configKeys["port_map"], "2083=10.0.0.5:443, 2053"); err != nil || m["port_map"] != "2053,2083=10.0.0.5:443" {
		t.Fatalf("port_map: %v %v", m["port_map"], err)
	}
}

func TestLocalListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ls := listeningSockets()
	if ls == nil {
		t.Skip("no /proc/net/tcp here")
	}
	if ok, checked := localListening(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), ls); !ok || !checked {
		t.Fatalf("own listener not found: %v %v", ok, checked)
	}
	if ok, checked := localListening("localhost:"+strconv.Itoa(port), ls); !ok || !checked {
		t.Fatal("localhost not mapped to 127.0.0.1")
	}
	if ok, checked := localListening("127.0.0.2:"+strconv.Itoa(port), ls); ok || !checked {
		t.Fatal("a listener on 127.0.0.1 does not serve 127.0.0.2")
	}
	if _, checked := localListening("203.0.113.9:443", ls); checked {
		t.Fatal("another host cannot be checked from here")
	}
	// A UDP-only inbound (Hysteria2, TUIC, WireGuard…) counts as listening.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	up := pc.LocalAddr().(*net.UDPAddr).Port
	if ok, _ := localListening(net.JoinHostPort("127.0.0.1", strconv.Itoa(up)), listeningSockets()); !ok {
		t.Fatal("a UDP-only inbound must count as listening")
	}
	// /proc readable but nothing on the port: checked, not "cannot tell".
	if ok, checked := localListening("127.0.0.1:1", []sockListen{}); ok || !checked {
		t.Fatalf("empty socket list: ok=%v checked=%v", ok, checked)
	}
	// localhost covers an inbound on ::1 only.
	if ok, _ := localListening("localhost:9", []sockListen{{ip: net.IPv6loopback, port: 9}}); !ok {
		t.Fatal("localhost must match a listener on ::1")
	}
}

// The status writer publishes the table; hs2 status prints it.
func TestStatusShowsPorts(t *testing.T) {
	useStatusDir(t)
	fc := fileConfig{Mode: "dial", Carrier: "mtcp", ForwardPorts: "8443,2053"}
	ls := liveStatus{Role: "Iran side", Links: 2}
	fillPeerRoutes(&ls, &engine.PeerRoutes{Known: true, Tags: true, Ports: []int{2053}, Default: true})
	ls.PortsLines = portLines(fc, viewOf(ls, true))
	ls.Updated = time.Now().Unix()
	cfg := filepath.Join(t.TempDir(), "c.json")
	writeStatusFile(statusPath(cfg), ls)
	out := captureStdout(t, func() { printStatus(statusPath(cfg)) })
	for _, want := range []string{"ports:      8443, 2053 (tcp)", "2053 → its own target on the Kharej server"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	// hs2 ports reads the same file.
	if got := strings.Join(portLines(fc, viewOf(readLiveOK(t, cfg))), "\n"); !strings.Contains(got, "2053 → its own target") {
		t.Errorf("hs2 ports: %s", got)
	}
}

func readLiveOK(t *testing.T, cfg string) (liveStatus, bool) {
	t.Helper()
	ls, running := readLive(cfg)
	if !running {
		t.Fatal("fresh status file read as not running")
	}
	return ls, running
}

func TestDoctorPorts(t *testing.T) {
	useStatusDir(t)
	cfg := filepath.Join(t.TempDir(), "c.json")
	fc := fileConfig{Mode: "dial", Carrier: "mtcp", ForwardPorts: "8443,2053"}
	ls := liveStatus{Links: 2, Updated: time.Now().Unix()}
	fillPeerRoutes(&ls, &engine.PeerRoutes{Known: true, Tags: true, Ports: []int{2053}})
	writeStatusFile(statusPath(cfg), ls)
	d := &doctorReport{}
	checkPorts(d, fc, cfg)
	out := captureStdout(t, d.print)
	if d.warns != 1 || !strings.Contains(out, "the Kharej server has no target for 8443") {
		t.Fatalf("doctor:\n%s", out)
	}
	// Kharej: a target nothing listens on is a warning.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	live := ln.Addr().String()
	kfc := fileConfig{Mode: "listen", Carrier: "mtcp", Expose: live, PortMap: "2053=127.0.0.1:1"}
	d = &doctorReport{}
	checkPorts(d, kfc, filepath.Join(t.TempDir(), "k.json"))
	out = captureStdout(t, d.print)
	if listeningSockets() != nil && (!strings.Contains(out, "nothing listens (TCP or UDP) on 127.0.0.1:1 (port 2053)") || strings.Contains(out, live)) {
		t.Fatalf("doctor kharej:\n%s", out)
	}
}

// captureStdout runs f and returns what it printed to stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	f()
	os.Stdout = old
	w.Close()
	return <-done
}

// useStatusDir points the status files at a temp dir for one test.
func useStatusDir(t *testing.T) {
	t.Helper()
	old := statusRunDir
	statusRunDir = t.TempDir()
	t.Cleanup(func() { statusRunDir = old })
}
