package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	Users     int     `json:"users"`   // active user connections
	Mbit      float64 `json:"mbit"`    // aggregate goodput, Mbit/s
	Phase     string  `json:"phase"`   // calibrating/probing/steady/shrinking/following/listening
	Sat       bool    `json:"sat"`     // links pushing against a limit
	PID       int     `json:"pid"`     // the daemon's PID (staleness check)
	Updated   int64   `json:"updated"` // unix seconds of this write
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
	if ls.Users > 0 || ls.Mbit > 0 {
		fmt.Printf("  traffic:    %d user connections · %.1f Mbit/s%s\n", ls.Users, ls.Mbit, satLabel(ls.Sat))
	}
}

// patternLine renders the live parallel-link pattern, e.g.
// "8 up / 10 target (probing, range 2–32)".
func patternLine(ls liveStatus) string {
	if ls.Max == 0 { // exit side with no envelope of its own
		return fmt.Sprintf("%d up (%s)", ls.Links, ls.Phase)
	}
	s := fmt.Sprintf("%d up", ls.Links)
	if ls.Target != ls.Links {
		s += fmt.Sprintf(" / %d target", ls.Target)
	}
	s += fmt.Sprintf(" (%s, range %d–%d)", ls.Phase, ls.Min, ls.Max)
	return s
}

func satLabel(b bool) string {
	if b {
		return " · saturated (a bigger pattern may help)"
	}
	return ""
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
