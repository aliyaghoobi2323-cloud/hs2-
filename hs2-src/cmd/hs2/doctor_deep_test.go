package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixInterrupts1 = `           CPU0       CPU1       CPU2       CPU3
  0:         38          0          0          0   IO-APIC   2-edge      timer
 24:          0          0          0          0   PCI-MSI 49152-edge      virtio0-config
 25:        100          0        200       5000   PCI-MSI 49153-edge      virtio0-input.0
 26:         10         10         10         10   PCI-MSI 49154-edge      virtio0-output.0
NMI:          0          0          0          0   Non-maskable interrupts
`

const fixInterrupts2 = `           CPU0       CPU1       CPU2       CPU3
  0:         38          0          0          0   IO-APIC   2-edge      timer
 24:          0          0          0          0   PCI-MSI 49152-edge      virtio0-config
 25:        110          0        200       9000   PCI-MSI 49153-edge      virtio0-input.0
 26:         90         10         10         10   PCI-MSI 49154-edge      virtio0-output.0
NMI:          0          0          0          0   Non-maskable interrupts
`

func TestParseInterruptsAndNICIRQ(t *testing.T) {
	a, b := parseInterrupts([]byte(fixInterrupts1)), parseInterrupts([]byte(fixInterrupts2))
	if len(b) != 4 || b["25"].Name != "PCI-MSI 49153-edge virtio0-input.0" || b["25"].Counts[3] != 9000 {
		t.Fatalf("parse: %+v", b)
	}
	per, n := nicIRQCPUs(a, b, []string{"eth0", "virtio0-"})
	// Only the input queue counts (output's CPU0 burst is ignored).
	if n != 1 || len(per) != 4 || per[3] != 4000 || per[0] != 10 {
		t.Fatalf("per=%v n=%d", per, n)
	}
}

const fixStat1 = `cpu  1000 0 1000 8000 0 0 1000 0 0 0
cpu0 250 0 250 2000 0 0 0 0 0 0
cpu1 250 0 250 2000 0 0 1000 0 0 0
intr 1
`
const fixStat2 = `cpu  1100 0 1100 8100 0 0 1300 0 0 0
cpu0 275 0 275 2050 0 0 0 0 0 0
cpu1 275 0 275 2050 0 0 1300 0 0 0
`

func TestPerCPUDeltas(t *testing.T) {
	d := perCPUDeltas(parsePerCPU([]byte(fixStat1)), parsePerCPU([]byte(fixStat2)))
	if len(d) != 2 {
		t.Fatalf("%v", d)
	}
	if c := d[1]; c.Softirq < 74 || c.Softirq > 76 || c.Idle < 12 || c.Idle > 13 {
		t.Fatalf("cpu1 %+v", c)
	}
	if c := d[0]; c.Softirq != 0 || c.Idle < 49 || c.Idle > 51 {
		t.Fatalf("cpu0 %+v", c)
	}
}

func TestPressureAndPidStat(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pressure"), 0o755)
	os.WriteFile(filepath.Join(dir, "pressure", "cpu"), []byte("some avg10=17.50 avg60=9.25 avg300=3.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"), 0o644)
	a10, a60, ok := readPSI(dir)
	if !ok || a10 != 17.5 || a60 != 9.25 {
		t.Fatalf("%v %v %v", a10, a60, ok)
	}
	env := &deepEnv{proc: dir}
	fs := checkPressure(env, map[string]uint64{"10 xray": 100, "11 xray": 0}, map[string]uint64{"10 xray": 400, "11 xray": 150}, 3*time.Second, nil, nil)
	if fs[0].Sev != "WARN" || !strings.Contains(fs[0].What, "xray 150%") {
		t.Fatalf("%+v", fs[0])
	}
	ticks, ok := parsePidStatTicks("1234 (x ray) S 1 1 1 0 -1 4194560 100 0 0 0 70 30 0 0 20 0 9 0 100 0 0")
	if !ok || ticks != 100 {
		t.Fatalf("ticks %d %v", ticks, ok)
	}
}

func TestParseCPUList(t *testing.T) {
	m := parseCPUList("0-2,5,7-8\n")
	for _, c := range []int{0, 1, 2, 5, 7, 8} {
		if !m[c] {
			t.Fatalf("missing %d in %v", c, m)
		}
	}
	if m[3] || len(m) != 6 {
		t.Fatalf("%v", m)
	}
}

const fixXray = `{
 "log": {"loglevel": "warning"},
 "inbounds": [
  {"tag": "api", "port": 62789, "protocol": "dokodemo-door"},
  {"tag": "inbound-443", "port": 443, "protocol": "vless",
   "settings": {"clients": [{"id": "SECRET-UUID"}]},
   "streamSettings": {"network": "tcp", "security": "reality",
     "realitySettings": {"dest": "www.cloudflare.com:443", "privateKey": "SECRET-KEY", "serverNames": ["www.cloudflare.com"]}}},
  {"tag": "inbound-8443", "port": "8443", "protocol": "vless",
   "streamSettings": {"security": "reality",
     "realitySettings": {"target": "www.example.ir:443",
       "limitFallbackUpload": {"afterBytes": 1048576, "bytesPerSec": 51200}}}},
  {"tag": "tls", "port": 2053, "streamSettings": {"security": "tls"}}
 ]
}`

func TestParseRealityInbounds(t *testing.T) {
	ins := parseRealityInbounds([]byte(fixXray))
	if len(ins) != 2 {
		t.Fatalf("%+v", ins)
	}
	if ins[0].Dest != "www.cloudflare.com:443" || ins[0].Port != 443 || ins[0].LimitedFallback {
		t.Fatalf("%+v", ins[0])
	}
	if ins[1].Dest != "www.example.ir:443" || ins[1].Port != 8443 || !ins[1].LimitedFallback {
		t.Fatalf("%+v", ins[1])
	}
	for _, in := range ins {
		if strings.Contains(in.Dest+in.Tag, "SECRET") {
			t.Fatal("secret leaked")
		}
	}
	if multiTenantCDN("www.cloudflare.com") != "Cloudflare" || multiTenantCDN("dl.google.com") != "Google" || multiTenantCDN("www.example.ir") != "" {
		t.Fatal("cdn detection")
	}
}

func TestParseProcNetTCP(t *testing.T) {
	// 104.16.124.96:443 established; a LISTEN row is ignored.
	b := []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1
   1: 0A00000A:D2C4 607C1068:01BB 01 00000000:00000000 00:00000000 00000000     0        0 222 1
`)
	c := parseProcNetTCP(b)
	if len(c) != 1 || c[0].Remote.String() != "104.16.124.96" || c[0].Port != 443 || c[0].Inode != "222" || c[0].LocalPort != 0xD2C4 {
		t.Fatalf("%+v", c)
	}
}

func TestXUICounts(t *testing.T) {
	log := strings.Join([]string{
		"2026/11/10 21:00:01 INFO - XRAY: Remove Inbound User a@x",
		"2026/11/10 21:00:02 INFO - XRAY: Remove Inbound User b@x",
		"2026/11/10 21:00:03 WARNING - node 2 reconcile inbounds failed: rpc error",
		"2026/11/10 21:00:04 ERROR - sync node: context deadline exceeded",
		"2026/11/10 21:00:05 INFO - add user c@x",
	}, "\n")
	rm, se := xuiCounts(log)
	if rm != 2 || se != 2 {
		t.Fatalf("rm=%d se=%d", rm, se)
	}
	if countPanics("x\npanic: runtime error: index out of range\ngoroutine 1 [running]:\nfatal error: concurrent map writes\n") != 2 {
		t.Fatal("panics")
	}
}

func TestParsePingAndJudge(t *testing.T) {
	out := `PING 10.0.0.2 (10.0.0.2) from 10.0.0.1 : 56(84) bytes of data.

--- 10.0.0.2 ping statistics ---
10 packets transmitted, 9 received, 10% packet loss, time 1810ms
rtt min/avg/max/mdev = 40.100/95.300/130.000/20.100 ms
`
	p := parsePing(out)
	if !p.OK || p.Loss != 10 || p.Min != 40.1 || p.Avg != 95.3 {
		t.Fatalf("%+v", p)
	}
	clean := pingResult{OK: true, Min: 40, Avg: 42, Max: 45}
	fs := judgePaths([]*pathProbe{
		{local: "1.1.1.1", remote: "2.2.2.2", ping: p, tunMin: 40},
		{local: "1.1.1.3", remote: "2.2.2.2", ping: clean, tunRTT: 120, tunMin: 40},
	})
	if fs[0].Sev != "BAD" || !strings.Contains(fs[0].Why, "WITHOUT the tunnel") {
		t.Fatalf("%+v", fs[0])
	}
	if fs[1].Sev != "WARN" || !strings.Contains(fs[1].Why, "bare path is clean") {
		t.Fatalf("%+v", fs[1])
	}
	if len(fs) != 2 { // the second path is not congested: no network-wide line
		t.Fatalf("%d findings", len(fs))
	}
	// Every path far above its lowest RTT: the whole network is congested.
	fs = judgePaths([]*pathProbe{
		{remote: "2.2.2.2", ping: pingResult{OK: true, Min: 90, Avg: 100}, tunMin: 40},
		{remote: "2.2.2.3", ping: pingResult{OK: true, Min: 95, Avg: 110}, tunMin: 42},
	})
	if last := fs[len(fs)-1]; last.Area != "whole network" || last.Sev != "WARN" {
		t.Fatalf("%+v", fs)
	}
	deepLang = "fa"
	defer func() { deepLang = "en" }()
	if fs := judgePaths([]*pathProbe{{remote: "2.2.2.2", ping: p, tunMin: 40}}); !strings.Contains(fs[0].Why, "بدون تونل") {
		t.Fatalf("fa: %+v", fs[0])
	}
}

func TestEthtoolCombined(t *testing.T) {
	out := `Channel parameters for eth0:
Pre-set maximums:
RX:		n/a
TX:		n/a
Other:		n/a
Combined:	4
Current hardware settings:
RX:		n/a
TX:		n/a
Other:		n/a
Combined:	1
`
	if ethtoolCombined(out) != 1 {
		t.Fatal(ethtoolCombined(out))
	}
}
