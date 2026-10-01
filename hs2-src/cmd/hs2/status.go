package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// The daemon publishes a small live status file so the installer's tunnel
// manager (and `hs2 status`) can show the connection pattern as it changes —
// how many parallel links are up right now, the count the autopilot wants, what
// it is doing (calibrating / probing / steady / shrinking), and the throughput —
// without parsing the log. It is written atomically every statusInterval to a
// path derived from the config path, so several tunnels never collide.

const statusInterval = 2 * time.Second

// statusRunDir is where live status files live (a tmpfs on a normal system, so
// they never touch the disk and vanish on reboot).
var statusRunDir = "/run/hs2"

// liveStatus is the flat JSON written to the status file. Flat on purpose: the
// installer reads it with the same grep/sed helpers it uses for the config.
type liveStatus struct {
	Role      string  `json:"role"`      // "Iran side" / "Kharej side"
	Dir       string  `json:"dir"`       // direct / reverse
	Carrier   string  `json:"carrier"`   // mtcp / l3mtcp / tls / udp / auto
	Transport string  `json:"transport"` // human transport label
	Endpoint  string  `json:"endpoint"`  // "listens on ..." / "connects to ..."
	Links     int     `json:"links"`     // live links now
	Target    int     `json:"target"`    // links the autopilot wants
	Min       int     `json:"min"`
	Max       int     `json:"max"`
	Users     int     `json:"users"`     // active user connections
	Mbit      float64 `json:"mbit"`      // aggregate goodput, Mbit/s
	Phase     string  `json:"phase"`     // steady/scaling/probing/holding/shrinking; exit: following/listening
	Sat       bool    `json:"sat"`       // some serving link is at its limit
	CertDays  int     `json:"cert_days"` // days until the TLS cert expires (-1 if none/unknown)
	PID       int     `json:"pid"`       // the daemon's PID (staleness check)
	Updated   int64   `json:"updated"`   // unix seconds of this write

	// The edge's pool detail (absent on the exit side). "target" counts
	// SERVING links; "links" also includes retiring ones, which take no new
	// connections and close once theirs have ended.
	Serving    *int    `json:"serving,omitempty"`
	Retiring   int     `json:"retiring,omitempty"`
	HeldBy     int     `json:"held_by,omitempty"`     // open connections still on retiring links
	HeldActive int     `json:"held_active,omitempty"` // ... of which moving data
	Flowing    int     `json:"flowing,omitempty"`     // connections actively moving data
	Pressed    int     `json:"pressed,omitempty"`     // serving links at their limit
	CapMbit    float64 `json:"cap_mbit,omitempty"`    // measured per-link limit (absent: none seen)
	PeakMbit   float64 `json:"peak_mbit,omitempty"`   // last minute's peak throughput
	Reason     string  `json:"reason,omitempty"`      // why the pattern is this size
	NextProbeS int     `json:"next_probe_s,omitempty"`
	ExitStats  string  `json:"exit_stats,omitempty"` // ok / partial / older exit: ...

	// Datagram tunnels (dgtun): loss of what this side SENDS (the peer
	// reports it), FEC, drops and the policer cap. Absent = 0 / false.
	LossPct       float64 `json:"loss_pct,omitempty"`     // pool-wide, rate-weighted
	MaxLossPct    float64 `json:"max_loss_pct,omitempty"` // worst active carrier
	ParityPct     float64 `json:"parity_pct,omitempty"`   // FEC parity per data byte
	FECAtCeiling  int     `json:"fec_at_ceiling,omitempty"`
	FECRecovered  uint64  `json:"fec_recovered,omitempty"` // received data rebuilt (live carriers)
	FECLost       uint64  `json:"fec_lost,omitempty"`      // received data lost for good
	PacerDropped  uint64  `json:"pacer_dropped,omitempty"`
	RxDropped     uint64  `json:"rx_dropped,omitempty"`
	TunDrops      uint64  `json:"tun_drops,omitempty"`
	TunRead       uint64  `json:"tun_read,omitempty"`
	SentPkts      uint64  `json:"sent_pkts,omitempty"`
	RecvPkts      uint64  `json:"recv_pkts,omitempty"`
	TunWritten    uint64  `json:"tun_written,omitempty"`
	DropNoCarrier uint64  `json:"drop_no_carrier,omitempty"`
	DropQueueFull uint64  `json:"drop_queue_full,omitempty"`
	DropAged      uint64  `json:"drop_aged,omitempty"`
	Carriers      string  `json:"carriers,omitempty"` // id:state:sent/loss% per carrier
	Policed       bool    `json:"policed,omitempty"`
	PoliceConfirm bool    `json:"police_confirmed,omitempty"`
	PoliceCapMbit float64 `json:"police_cap_mbit,omitempty"`

	// Process CPU over the last interval, % of ONE core (a 2-core box can
	// show up to 200), and the core count.
	CPUPct   float64 `json:"cpu_pct"`
	CPUCores int     `json:"cpu_cores"`
}

// statusPath maps a config path to its live status file. It is deterministic
// and collision-free (the absolute config path is sanitised into the name), so
// the daemon writing it and the manager reading it always agree.
func statusPath(cfgPath string) string {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		abs = cfgPath
	}
	name := strings.TrimLeft(abs, "/")
	name = strings.NewReplacer("/", "-", " ", "_").Replace(name)
	return filepath.Join(statusRunDir, name+".status.json")
}

// startStatusWriter publishes the live pattern to the status file every tick
// until ctx ends, then removes the file. stats returns the current snapshot.
func startStatusWriter(ctx context.Context, fc fileConfig, cfgPath string, stats engine.StatsFn) {
	path := statusPath(cfgPath)
	if err := os.MkdirAll(statusRunDir, 0o755); err != nil {
		return // /run not writable (unusual); the manager falls back to `ss`
	}
	cpu := &cpuMeter{logf: func(f string, a ...any) { log.Printf(f, a...) }}
	base := liveStatus{
		Role: role(fc), Dir: direction(fc), Carrier: carrierName(fc),
		Transport: transportLabel(fc), Endpoint: endpointLabel(fc), PID: os.Getpid(),
	}
	write := func() {
		s := stats()
		ls := base
		ls.Links, ls.Target, ls.Min, ls.Max = s.Links, s.Target, s.Min, s.Max
		ls.Users, ls.Mbit, ls.Sat = s.Users, round1(s.MbitPerS), s.Saturated
		if s.Phase != "" {
			ls.Phase = s.Phase
		}
		if s.Max > 0 && s.Phase != "following" && s.Phase != "listening" { // the edge's pool
			serving := s.Serving
			ls.Serving = &serving
			ls.Retiring, ls.HeldBy, ls.HeldActive = s.Retiring, s.HeldBy, s.HeldActive
			ls.Flowing, ls.Pressed = s.Flowing, s.Pressed
			ls.CapMbit, ls.PeakMbit = round1(s.CapMbit), round1(s.PeakMbit)
			ls.Reason, ls.NextProbeS, ls.ExitStats = s.Reason, s.NextProbeS, s.ExitStats
		}
		if s.Datagram {
			ls.LossPct, ls.MaxLossPct, ls.ParityPct = s.LossPct, s.MaxLossPct, s.ParityPct
			ls.FECAtCeiling, ls.FECRecovered, ls.FECLost = s.FECAtCeiling, s.FECRecovered, s.FECLost
			ls.PacerDropped, ls.RxDropped, ls.TunDrops = s.PacerDropped, s.RxDropped, s.TunDrops
			ls.Policed, ls.PoliceConfirm, ls.PoliceCapMbit = s.Policed, s.PoliceConfirm, s.PoliceCapMbit
			ls.TunRead, ls.SentPkts, ls.RecvPkts, ls.TunWritten = s.TunRead, s.SentPkts, s.RecvPkts, s.TunWritten
			ls.DropNoCarrier, ls.DropQueueFull, ls.DropAged, ls.Carriers = s.DropNoCarrier, s.DropQueueFull, s.DropAged, s.Carriers
		}
		ls.CPUPct, ls.CPUCores = cpu.sample(), runtime.NumCPU()
		ls.CertDays = firstCertExpiryDays()
		ls.Updated = time.Now().Unix()
		writeStatusFile(path, ls)
	}
	go func() {
		t := time.NewTicker(statusInterval)
		defer t.Stop()
		write()
		for {
			select {
			case <-ctx.Done():
				os.Remove(path)
				return
			case <-t.C:
				write()
			}
		}
	}()
}

// cpuMeter measures this process's CPU (utime + stime from /proc/self/stat)
// between samples, as % of one core, and logs when the daemon itself becomes
// the bottleneck — it uses nearly all the cores it has for several samples in a
// row — and when it no longer is.
type cpuMeter struct {
	logf     func(string, ...any)
	cores    int // 0 = runtime.NumCPU() (tests set it)
	lastT    time.Time
	lastTick uint64
	hot      int
	logged   bool
}

// clkTck is USER_HZ, 100 on every Linux the binary targets.
const clkTck = 100

func (m *cpuMeter) sample() float64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	// fields after the ")" of comm: state is field 3; utime/stime are 14/15
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	if len(f) < 13 {
		return 0
	}
	ut, _ := strconv.ParseUint(f[11], 10, 64)
	st, _ := strconv.ParseUint(f[12], 10, 64)
	now, tick := time.Now(), ut+st
	defer func() { m.lastT, m.lastTick = now, tick }()
	if m.lastT.IsZero() {
		return 0
	}
	pct := float64(tick-m.lastTick) / clkTck / now.Sub(m.lastT).Seconds() * 100
	pct = float64(int(pct*10+0.5)) / 10
	cores := float64(runtime.NumCPU())
	if m.cores > 0 {
		cores = float64(m.cores)
	}
	switch {
	case pct >= 90*cores:
		if m.hot++; m.hot >= 3 && !m.logged {
			m.logged = true
			m.logf("cpu: hs2 is using %.0f%% of its %d core(s) — the CPU is the bottleneck now, not the path (a bigger VPS, or fewer/other carriers, would carry more)", pct, int(cores))
		}
	case pct < 70*cores:
		m.hot = 0
		if m.logged {
			m.logged = false
			m.logf("cpu: back to %.0f%% of %d core(s) — no longer the bottleneck", pct, int(cores))
		}
	}
	return pct
}

// writeStatusFile writes the status atomically (temp file + rename) so a reader
// never sees a half-written file.
func writeStatusFile(path string, ls liveStatus) {
	b, err := json.Marshal(ls)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

// statusCmd implements `hs2 status -c config [--watch]`: it reads the live
// status file the running daemon publishes and prints a one-screen dashboard.
func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	watch := fs.Bool("watch", false, "refresh continuously")
	fs.Parse(args)
	if *cfgPath == "" {
		fmt.Println("usage: hs2 status -c config.json [--watch]")
		os.Exit(2)
	}
	path := statusPath(*cfgPath)
	if !*watch {
		printStatus(path)
		return
	}
	for {
		fmt.Print("\033[H\033[2J") // clear
		printStatus(path)
		time.Sleep(statusInterval)
	}
}

func printStatus(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("no live status yet (is the tunnel running? — the status file appears a few seconds after start)")
		return
	}
	var ls liveStatus
	if json.Unmarshal(b, &ls) != nil {
		fmt.Println("status file is unreadable")
		return
	}
	age := time.Now().Unix() - ls.Updated
	stale := ""
	if age > 6 {
		stale = fmt.Sprintf("  (stale: last updated %ds ago — tunnel may be down)", age)
	}
	fmt.Printf("hs2 — %s · %s · %s%s\n", ls.Role, ls.Dir, ls.Transport, stale)
	fmt.Printf("  %s\n", ls.Endpoint)
	fmt.Printf("  links:      %s\n", patternLine(ls))
	if w := whyLine(ls); w != "" {
		fmt.Printf("  why:        %s\n", w)
	}
	if ls.Users > 0 || ls.Mbit > 0 {
		fmt.Printf("  traffic:    %s\n", trafficLine(ls))
	}
	if ls.ExitStats != "" && ls.ExitStats != "ok" {
		fmt.Printf("  exit stats: %s\n", ls.ExitStats)
	}
	if strings.HasPrefix(ls.Carrier, "dgtun") || ls.LossPct > 0 || ls.ParityPct > 0 || ls.Policed {
		fmt.Printf("  loss:       %.1f%% of what this side sends (worst carrier %.1f%%)\n", ls.LossPct, ls.MaxLossPct)
		fec := fmt.Sprintf("parity %.0f%% of data", ls.ParityPct)
		if ls.FECAtCeiling > 0 {
			fec += fmt.Sprintf(" — at its ceiling on %d carrier(s)", ls.FECAtCeiling)
		}
		fmt.Printf("  fec:        %s · received: %d rebuilt, %d lost\n", fec, ls.FECRecovered, ls.FECLost)
		if ls.Policed && ls.PoliceConfirm {
			fmt.Printf("  policer:    confirmed on the path — whole pool held at %.1f Mbit/s (re-probes slowly)\n", ls.PoliceCapMbit)
		} else if ls.Policed {
			fmt.Printf("  policer:    suspected — testing with the whole pool capped at %.1f Mbit/s\n", ls.PoliceCapMbit)
		}
		fmt.Printf("  packets:    tun→carriers %d read, %d sent · carriers→tun %d received, %d written\n", ls.TunRead, ls.SentPkts, ls.RecvPkts, ls.TunWritten)
		if ls.PacerDropped+ls.RxDropped+ls.TunDrops > 0 {
			fmt.Printf("  drops:      no carrier %d · carrier queue full %d · waited >50 ms %d · pacer %d · receive queue %d\n",
				ls.DropNoCarrier, ls.DropQueueFull, ls.DropAged, ls.PacerDropped, ls.RxDropped)
		}
		if ls.Carriers != "" {
			fmt.Printf("  carriers:   %s\n              (id:state:sent/loss rRATE/bwBTLBW Mbit, flags P=pushing S=startup)\n", ls.Carriers)
		}
	}
	if ls.CPUCores > 0 {
		fmt.Printf("  cpu:        %.0f%% of one core (%d core(s))\n", ls.CPUPct, ls.CPUCores)
	}
	if ls.CertDays >= 0 {
		fmt.Printf("  certificate: valid for %d more day(s)%s\n", ls.CertDays, certWarn(ls.CertDays))
	}
}

func certWarn(days int) string {
	if days <= 7 {
		return "  (renewal due — certbot renews automatically; hot-reloaded, no restart)"
	}
	return ""
}

// patternLine renders the live parallel-link pattern, e.g.
// "7 up = 5 serving + 2 retiring / target 5 (shrinking, range 2–32)".
func patternLine(ls liveStatus) string {
	if ls.Max == 0 { // exit side with no envelope of its own
		return fmt.Sprintf("%d up (%s)", ls.Links, ls.Phase)
	}
	s := fmt.Sprintf("%d up", ls.Links)
	if ls.Serving != nil && ls.Retiring > 0 {
		s += fmt.Sprintf(" = %d serving + %d retiring", *ls.Serving, ls.Retiring)
	}
	serving := ls.Links
	if ls.Serving != nil {
		serving = *ls.Serving
	}
	if ls.Target != serving {
		s += fmt.Sprintf(" / target %d", ls.Target)
	}
	s += fmt.Sprintf(" (%s, range %d–%d)", ls.Phase, ls.Min, ls.Max)
	return s
}

// whyLine explains the size: the controller's reason, plus what keeps any
// retiring links up.
func whyLine(ls liveStatus) string {
	w := ls.Reason
	if ls.Retiring > 0 {
		held := fmt.Sprintf("%d retiring link(s) close as their connections end", ls.Retiring)
		if ls.HeldBy > 0 {
			held += fmt.Sprintf(" (held by %d open", ls.HeldBy)
			if ls.HeldActive > 0 {
				held += fmt.Sprintf(", %d active", ls.HeldActive)
			}
			held += ")"
		}
		if w != "" {
			w += "; "
		}
		w += held
	}
	return w
}

// trafficLine renders "251 connections (18 active) · 6.1 Mbit/s · 1 link at its limit".
func trafficLine(ls liveStatus) string {
	s := fmt.Sprintf("%d connections", ls.Users)
	if ls.Flowing > 0 {
		s += fmt.Sprintf(" (%d active)", ls.Flowing)
	}
	s += fmt.Sprintf(" · %.1f Mbit/s", ls.Mbit)
	switch {
	case ls.Pressed == 1:
		s += " · 1 link at its limit"
	case ls.Pressed > 1:
		s += fmt.Sprintf(" · %d links at their limit", ls.Pressed)
	case ls.Serving == nil && ls.Sat: // an older daemon's file
		s += " · links at their limit"
	}
	if ls.CapMbit > 0 {
		s += fmt.Sprintf(" (~%.1f Mbit/s each)", ls.CapMbit)
	}
	return s
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// --- config-derived labels, shared by the status writer -----------------------

func role(fc fileConfig) string {
	if fc.Mode == "dial" {
		return "Iran side"
	}
	return "Kharej side"
}

func direction(fc fileConfig) string {
	if fc.Reverse {
		return "reverse"
	}
	return "direct"
}

func carrierName(fc fileConfig) string {
	if fc.Carrier == "" {
		return "noise"
	}
	return fc.Carrier
}

func transportLabel(fc fileConfig) string {
	switch carrierName(fc) {
	case "mtcp":
		return "tcp (mtcp)"
	case "tls":
		return "tcp (tls)"
	case "l3mtcp", "l3":
		return "tun (L3 over mtcp)"
	case "udp":
		return "udp"
	case "auto":
		return "auto (udp, tcp fallback)"
	case "reality":
		return "reality"
	case "dgtun":
		return "tun (datagram pool over " + encapName(fc) + ")"
	default:
		return "tcp (noise)"
	}
}

// endpointLabel says whether this side listens on, or connects to, its addr —
// exactly the phrasing the tunnel manager uses.
func endpointLabel(fc fileConfig) string {
	if dialing(fc) {
		return "connects to " + fc.Addr
	}
	return "listens on " + fc.Addr
}
