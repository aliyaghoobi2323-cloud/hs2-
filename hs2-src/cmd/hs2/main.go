// hs2 is the tunnel binary.
//
//	hs2 keygen                 -> print a fresh static keypair
//	hs2 run -c <config.json>   -> run the engine with the configured carrier
//	hs2 check -c <config.json> -> validate a config without starting anything
//	hs2 link -c <config.json>  -> rebuild the tunnel's hs2:// setup link (link.go)
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tun"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tune"
)

type fileConfig struct {
	// Role & interface
	Mode      string `json:"mode"` // "dial" or "listen"
	Iface     string `json:"iface"`
	LocalCIDR string `json:"local_cidr"`
	PeerIP    string `json:"peer_ip"`
	MTU       int    `json:"mtu"`

	// Carrier selection
	Carrier string `json:"carrier"` // reality|noise|mtcp|l3mtcp|tls|udp|auto|dgtun
	Addr    string `json:"addr"`    // dial target or listen bind

	// Datagram tun (carrier "dgtun"): the encapsulation the carrier POOL rides
	// on, and (ipx only) its IP protocol number. "" / "udp" is the default.
	Encap string `json:"encap"`
	Proto int    `json:"proto"`

	// Direction. Direct (default): the edge (iran, mode=dial) initiates the
	// connection to the exit (kharej, mode=listen). Reverse: the exit initiates
	// to the edge, so the edge LISTENS and the exit DIALS — the data path and
	// roles (edge=user ports, exit=panel) are unchanged, only who dials flips.
	Reverse bool `json:"reverse"`

	// custom tunnel IP (optional): egress/listen on a specific server IP
	BindLocalIP  string `json:"bind_local_ip"`  // iran: source IP to dial from
	UserListenIP string `json:"user_listen_ip"` // iran: IP the user ports listen on
	UDP          bool   `json:"udp"`            // also forward UDP on forward_ports

	// mtcp (multi-link) settings
	MinLinks int `json:"min_links"`
	// MaxLinks: a number fixes the ceiling; 0 = AUTO (from the hardware, see
	// linkCeiling); ABSENT = the historical fixed 32. maxLinksSet tells 0 and
	// absent apart (set by UnmarshalJSON), so a config written before the auto
	// ceiling existed behaves exactly as it always did.
	MaxLinks    int `json:"max_links"`
	maxLinksSet bool
	PerLink     int `json:"per_link"` // concurrently active flows per link the pool sizes for
	// DrainIdleSec: when the pool shrinks, a connection on a retiring link
	// that has moved nothing for this many seconds is closed so the link can
	// finish. Unset = 310 (just above xray's 300 s connIdle); 0 = never.
	DrainIdleSec *int `json:"drain_idle_sec,omitempty"`

	// port forwarding (Backhaul-style reverse path)
	Expose       string `json:"expose"`        // kharej: the default panel addr, e.g. 127.0.0.1:443
	ForwardPorts string `json:"forward_ports"` // iran: comma-sep user ports to open
	PeerPanel    string `json:"peer_panel"`    // iran: panel addr as known to kharej (informational)
	// PortMap (kharej): Iran user ports with their own target, comma-separated
	// "P" (= 127.0.0.1:P) or "P=host:port"; a port not listed goes to expose
	// (engine/routes.go). The Iran server only says the port number; the
	// targets live here, so it can never make this server dial anything else.
	// On the Iran server it holds only flags: "P/proxy" marks user port P as
	// behind a proxy that sends a PROXY protocol header, believed only from
	// ProxyFrom (comma-separated addresses or prefixes; default loopback).
	PortMap   string `json:"port_map,omitempty"`
	ProxyFrom string `json:"proxy_from,omitempty"`

	// tls / reality carrier
	SNI         string `json:"sni"`          // domain (dial)
	CoverAddr   string `json:"cover_addr"`   // reality: real site to forward probes to
	BackendAddr string `json:"backend_addr"` // tls: local backend probes are proxied to
	CoverSeed   string `json:"cover_seed"`   // tls builtin cover: per-install page seed (hex; NOT from shared_key)
	SharedKey   string `json:"shared_key"`   // hex, both sides
	CertFile    string `json:"cert_file"`    // listen: real cert PEM (Let's Encrypt)
	KeyFile     string `json:"key_file"`     // listen: real key PEM

	// noise carrier
	LocalPriv    string `json:"local_priv"`
	LocalPub     string `json:"local_pub"`
	RemoteStatic string `json:"remote_static"`
	PSK          string `json:"psk"`

	// kernel tuning (RAM/CPU-aware; applied at every start). Omitted = auto.
	Tuning *tune.Config `json:"tuning"`
}

// UnmarshalJSON decodes the config as usual and records whether max_links was
// present at all (null counts as absent), so linkCeiling can tell an explicit
// 0 (auto) from a config that never set it (the historical fixed 32).
func (fc *fileConfig) UnmarshalJSON(b []byte) error {
	type plain fileConfig // same fields, no UnmarshalJSON: no recursion
	if err := json.Unmarshal(b, (*plain)(fc)); err != nil {
		return err
	}
	var probe struct {
		MaxLinks *int `json:"max_links"`
	}
	_ = json.Unmarshal(b, &probe) // the full decode above already validated b
	fc.maxLinksSet = probe.MaxLinks != nil
	return nil
}

// baseVersion is the canonical capability line. It MUST keep the literal
// "hs2 v3" prefix followed by a non-digit: the installer's hs2_is_v3 gate and
// the cross-end compatibility checks grep for exactly that to accept a binary.
// Never renumber or reword the "hs2 v3 (" opening without updating install.sh.
const baseVersion = "hs2 v3 (stream core; carriers: mtcp, l3mtcp, tls, udp, auto, dgtun; tun encaps: udp/icmp/gre/ipip/ipx; auth: tls-exporter bound, mutual)"

// buildTag is an OPTIONAL human-readable release label, injected at build time
// with -ldflags "-X main.buildTag=...". It is purely cosmetic; the build is
// identified from the embedded VCS stamp even when this is empty, so no build
// command is required to carry it (see BUILD.md).
var buildTag = ""

// buildStamp returns a compact build identifier — "build <rev12>[+] <date>" —
// derived from the binary's embedded VCS metadata. Go records vcs.revision /
// vcs.time / vcs.modified automatically when the binary is built inside the git
// checkout (the default; -trimpath does NOT strip them). This is metadata only:
// two hs2 binaries with different stamps are fully wire-compatible, so the
// stamp never gates a connection — it only lets an operator confirm that both
// ends of a tunnel, and the server vs. the published build, run the same code.
// Returns "" when neither a tag nor VCS metadata is present (e.g. built from a
// source tarball outside git), so callers fall back to the bare version line.
func buildStamp() string {
	var rev, ts string
	var dirty bool
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				ts = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	// Sanitize the optional, maintainer-supplied tag so it can never break the
	// installer's single-line / single-bracket stamp contract: drop '[' and ']'
	// (they would confuse the installer's `sed … [^]]*` extraction) and any
	// control byte (a newline would split the line; the Go linker already rejects
	// a newline in an -X value, so this is belt-and-suspenders), and trim the
	// edges. An interior space is fine — the installer captures it.
	tag := strings.Map(func(r rune) rune {
		if r < 0x20 || r == '[' || r == ']' {
			return -1
		}
		return r
	}, strings.TrimSpace(buildTag))
	if rev == "" && tag == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	date := ts
	if len(date) >= 10 {
		date = date[:10] // YYYY-MM-DD from the RFC3339 vcs.time
	}
	var b strings.Builder
	b.WriteString("build ")
	if tag != "" {
		b.WriteString(tag)
		if rev != "" {
			b.WriteByte(' ')
		}
	}
	if rev != "" {
		b.WriteString(rev)
		if dirty {
			b.WriteByte('+') // '+' marks uncommitted changes at build time
		}
	}
	if date != "" {
		b.WriteByte(' ')
		b.WriteString(date)
	}
	return b.String()
}

// versionLine is the single line printed by `hs2 version`. The build stamp is
// appended to baseVersion on ONE line, in square brackets, so the installer's
// inline captures ($(hs2 version)) stay single-line and hs2_is_v3 still matches
// the unchanged "hs2 v3 (" opening. An older binary with no stamp prints just
// baseVersion, which remains valid.
func versionLine() string {
	if s := buildStamp(); s != "" {
		return baseVersion + "  [" + s + "]"
	}
	return baseVersion
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Println("usage: hs2 keygen | hs2 run -c config.json | hs2 check -c config.json")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Println(versionLine())
	case "keygen":
		k, err := core.GenerateStatic()
		must(err)
		fmt.Printf("private: %s\npublic:  %s\n", hex.EncodeToString(k.Private), hex.EncodeToString(k.Public))
	case "run":
		startPprof()
		runCmd(os.Args[2:])
	case "check":
		checkCmd(os.Args[2:])
	case "doctor":
		doctorCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	case "tune":
		tuneCmd(os.Args[2:])
	case "recommend-links":
		recommendLinksCmd(os.Args[2:])
	case "config":
		configCmd(os.Args[2:])
	case "ports":
		portsCmd(os.Args[2:])
	case "cleanup":
		cleanupCmd(os.Args[2:])
	case "link":
		os.Exit(linkCmd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	default:
		fmt.Println("unknown command")
		os.Exit(2)
	}
}

// configPath is the path of the config the running daemon loaded, so the status
// writer can derive its live-status file. Set once in runCmd.
var configPath string

// applyTuning lets the test lab override data-path tuning without a rebuild.
// Production runs use the built-in defaults; these are not config options.
func applyTuning() {
	num := func(name string, dst *int) {
		if v := os.Getenv(name); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
				log.Printf("tuning: %s=%d", name, n)
			}
		}
	}
	num("HS2_TUNE_NOTSENT", &tlscarrier.NotSentLowat)
	num("HS2_TUNE_SMUX_FRAME", &engine.SmuxFrameSize)
	num("HS2_TUNE_SMUX_STREAMBUF", &engine.SmuxStreamBuffer)
	num("HS2_TUNE_SMUX_NEEDBUF", &engine.SmuxNeedStreamBuffer)
	num("HS2_TUNE_SMUX_POOL", &engine.SmuxWindowPool)
	num("HS2_TUNE_SMUX_SESSBUF", &engine.SmuxSessionBuffer)
	num("HS2_TUNE_SMUX_MINSTREAMBUF", &engine.SmuxMinStreamBuffer)
	num("HS2_TUNE_SMUX_LAGTARGET", &engine.SmuxStreamLagTarget)
	num("HS2_TUNE_SENDGATE", &engine.SendGateRate)
	// The window core (engine/wcore_run.go): 0 turns it off (the field safety
	// switch); the queue targets in ms; stage 2; and, lab only, the exit's
	// core for uploads (off until step 3), a trace file and fixed windows in
	// place of the controller.
	num("HS2_TUNE_WCORE", &engine.Wcore)
	num("HS2_TUNE_WCORE_UPLOAD", &engine.WcoreUpload)
	num("HS2_TUNE_WCORE_QTARGET", &engine.WcoreQTarget)
	num("HS2_TUNE_WCORE_PROBEQ", &engine.WcoreProbeQ)
	num("HS2_TUNE_WCORE_STAGE", &engine.WcoreStage)
	num("HS2_TUNE_WCORE_FIXEDWIN", &engine.WcoreFixedWin)
	if v := os.Getenv("HS2_TUNE_WCORE_TRACE"); v != "" {
		engine.WcoreTrace = v
		log.Printf("tuning: HS2_TUNE_WCORE_TRACE=%q", v)
	}
	// lab experiments, off by default: the sender's pacing capped at this
	// many times its own delivery rate, and B held where a burst after the
	// sender's PROBE_RTT fits the queue learned (share of the window that
	// becomes queue)
	flt := func(name string, dst *float64) {
		if v := os.Getenv(name); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*dst = f
				log.Printf("tuning: %s=%g", name, f)
			}
		}
	}
	flt("HS2_TUNE_WCORE_PACECAP", &engine.WcorePaceCap)
	flt("HS2_TUNE_WCORE_BURST", &engine.WcoreBurstShare)
	// The exit's pacing net (engine/pacenet.go), on by default: 0 turns it
	// off; the cap's factor over the socket's delivery rate, and the resend
	// share that engages it (0.02: 2%).
	num("HS2_TUNE_PACENET", &engine.PaceNet)
	flt("HS2_TUNE_PACENET_K", &engine.PaceNetK)
	flt("HS2_TUNE_PACENET_LOSS", &engine.PaceNetLoss)
	// R1's cores (docs/architecture/REDESIGN.fa.md §3.1-3.4), on by
	// default; 0 turns each off.
	num("HS2_TUNE_CLASS", &engine.TuneClass)     // C1, the classifier: every flow bulk
	num("HS2_TUNE_SCHED", &engine.TuneSched)     // C2, the scheduler: the old heap and FIFO
	num("HS2_TUNE_HQG", &engine.TuneHQG)         // C3, the host-queue guard: the fixed mark and sendGate
	num("HS2_TUNE_SENDCAP", &engine.TuneSendCap) // C4-lite, the pacing cap: the pacing net alone, exit only
	// R1 round 2 (β2): QUIET/BULK links with the one-time move and α's cap
	// on BULK links (0: every link as before, level 1's wire), and a
	// stream's next frame queued once the one before is copied (0: written)
	num("HS2_TUNE_QUIET", &engine.TuneQuiet)
	num("HS2_TUNE_EARLYDONE", &engine.TuneEarlyDone)
	num("HS2_TUNE_ALPHA", &engine.TuneAlpha) // α's cap on BULK links (0: C4-lite's there too)
	// the judge's fixes to α: C4-lite's cap till the first queue of its
	// own (0: β2's slow start), and the QUIET link's one-way delay as its
	// queue (0: srtt − min_rtt)
	num("HS2_TUNE_ALPHA_START", &engine.TuneAlphaStart)
	num("HS2_TUNE_OWD", &engine.TuneOWD)
	num("HS2_TUNE_OWD_QUIET", &engine.TuneOWDQuiet)
	num("HS2_TUNE_ALPHA_HALF", &engine.AlphaHalf)
	num("HS2_TUNE_ALPHA_PLATEAU", &engine.AlphaPlateau)
	num("HS2_TUNE_ALPHA_FREE", &engine.AlphaFree)
	num("HS2_TUNE_QUIET_REC", &engine.QuietRec)     // lab: QUIET's interactive record sizes
	num("HS2_TUNE_QUIET_EXTRA", &engine.QuietExtra) // QUIET links next to max_links BULK ones (0: within)
	// R1 round 3 (γ2): the BULK links' in-flight budget in place of α (0:
	// α), its queue target a direction (µs) and its share of the way back
	// on a two-way link (0: the QUIET link's one-way delay alone); the QUIET
	// links of a pool (0: one, outside max_links), and a QUIET record taking
	// every frame queued (0, lab: a record a frame)
	num("HS2_TUNE_BUDGET", &engine.TuneBudget)
	num("HS2_TUNE_BUDGET_US", &engine.BudgetTargetUs)
	num("HS2_TUNE_BUDGET_TWOWAY", &engine.BudgetTwoWay)
	num("HS2_TUNE_BUDGET_LINKOWD", &engine.BudgetLinkOWD)   // a two-way BULK link's own one-way delays (0: the guess)
	num("HS2_TUNE_BUDGET_PACE", &engine.BudgetPaceK)        // its pacing cap, % of its rate at the target (0: C4-lite's)
	num("HS2_TUNE_BUDGET_INFLIGHT", &engine.BudgetInflight) // in flight as Linux counts it (0: SIOCOUTQ)
	num("HS2_TUNE_QUIET_N", &engine.QuietN)
	num("HS2_TUNE_QUIET_BATCH", &engine.QuietBatch)
	num("HS2_TUNE_QUIET_LAZY", &engine.QuietLazy) // a QUIET record waits for the socket's unsent bytes (0: none)
	num("HS2_TUNE_QUIET_THIN", &engine.QuietThin) // TCP_THIN_LINEAR_TIMEOUTS on QUIET sockets (0: none)
	// R1 round 3, the judge: the exit's move hint (0: downloads move on data
	// arriving, round 2's stand-in; its caps bit is tlscarrier's, same name)
	num("HS2_TUNE_MOVE_HINT", &engine.TuneMoveHint)
	// ... the guard of a QUIET link's one-way delays (0: off), and the exit's
	// receive buffer to a local panel (bytes; 0: the kernel's autotuning)
	num("HS2_TUNE_OWD_GUARD", &engine.TuneOWDGuard)
	num("HS2_TUNE_OWD_GUARD_BULK", &engine.OWDGuardBulk)
	num("HS2_TUNE_PANEL_RCVBUF", &engine.TunePanelRcvbuf)
	num("HS2_TUNE_PANEL_MSS", &engine.TunePanelMSS)
	num("HS2_TUNE_BUDGET_QCHECK", &engine.BudgetQuietCheck) // the QUIET queues only within the link's own round trip (0: always)
	num("HS2_TUNE_BUDGET_C4", &engine.BudgetC4)             // a budget link's pacing cap: the budget's alone (0) or the lesser with C4-lite's
	num("HS2_TUNE_LINK_SNDBUF", &engine.TuneLinkSndbuf)     // a BULK link socket's send buffer (bytes; 0: autotuning)
	num("HS2_TUNE_MOVE_BACK", &engine.TuneMoveBack)         // (lab, both ends) back to a QUIET link when light again; its caps bit is tlscarrier's
	num("HS2_TUNE_MOVED_BULK", &engine.MovedBulk)           // a moved connection stays bulk on its BULK link (0: C1's rule)
	// gamma1's coalescing hold on QUIET links (µs; 0: none), jittered
	num("HS2_TUNE_QUIET_COAL", &engine.QuietCoalesce)
	num("HS2_TUNE_QUIET_COAL_MIN", &engine.QuietCoalesceMin)
	num("HS2_TUNE_QUIET_COAL_JITTER", &engine.QuietCoalJitter)
	num("HS2_TUNE_QUIET_COAL_TUN", &engine.QuietCoalTun)
	// a user's UDP datagram older than this at the edge is dropped (ms; 0: none)
	num("HS2_TUNE_UDP_LATE", &engine.UDPLateMs)
	// R1 round 4: video over several links (a user's floor weighed by the
	// links it is active on; the edge's placement by users) and the call
	// (a new connection off QUIET past this many bytes of big writes, its
	// write held at most this many ms; the realtime lane and its rate)
	num("HS2_TUNE_XSHARE", &engine.XShare)
	num("HS2_TUNE_PLACE_USERS", &engine.PlaceUsers)
	num("HS2_TUNE_QUIET_CAP", &engine.QuietCap)
	num("HS2_TUNE_QUIET_HOLD", &engine.QuietHoldMs)
	num("HS2_TUNE_RT", &engine.TuneRT)
	num("HS2_TUNE_RT_MAX", &engine.RTMax)
	num("HS2_TUNE_TWOWAY", &engine.TwoWay) // a connection whose user sends steadily stays on QUIET
	// C3's gate variants for the judge (decision T12), and C4-lite reading
	// the writer's waits as not app-limited (lab)
	num("HS2_TUNE_HQG_SEGS", &engine.HQGSegs)
	num("HS2_TUNE_HQG_GATE", &engine.HQGGate)
	num("HS2_TUNE_HQG_QUIET_SEGS", &engine.HQGQuietSegs)
	num("HS2_TUNE_SENDCAP_BLOCKED", &engine.SendCapBlocked)
	for _, n := range engine.SmuxTuningNotes() {
		log.Printf("tuning: %s", n)
	}
	if v, ok := os.LookupEnv("HS2_TUNE_QUIET_CC"); ok {
		engine.QuietCC = v
		log.Printf("tuning: HS2_TUNE_QUIET_CC=%q", v)
	}
	if v, ok := os.LookupEnv("HS2_TUNE_CC"); ok {
		tlscarrier.CongestionControl = v
		log.Printf("tuning: HS2_TUNE_CC=%q", v)
	}
}

// warmLinks is the link target the edge had before a restart (the warm
// file), clamped to this run's envelope, logged when used; 0 when there is
// none or it is too old.
func warmLinks(logf func(string, ...any), min, max int) int {
	n := readWarm(configPath)
	if n <= 0 {
		return 0
	}
	// It only ever raises the start size: a pool that was down to 2 links at
	// night must not come up at 2 when everyone reconnects at once.
	n = clampInt(n, min, max)
	if n <= engine.WarmSize(min, max) {
		return 0
	}
	logf("link pool: coming up at %d links, the size it had before this restart (the autopilot resizes it from there)", n)
	return n
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// setMemoryLimit gives the Go runtime a soft memory limit of half the RAM
// (unless GOMEMLIMIT is set): the heap's garbage then never grows to twice
// a large live backlog (hundreds of links' receive buffers when readers
// stall) — the collector runs more often near the limit instead.
func setMemoryLimit() {
	if _, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		return
	}
	ramMB, _ := detectHW()
	if ramMB <= 0 {
		return
	}
	lim := int64(ramMB) << 20 / 2
	debug.SetMemoryLimit(lim)
	log.Printf("memory: Go soft limit %d MB (half of %d MB RAM; set GOMEMLIMIT to override)", lim>>20, ramMB)
}

func runCmd(args []string) {
	// Whatever path runCmd leaves by, no icmp reply rule outlives the daemon
	// (the listeners release theirs on close; this catches the rest).
	defer encap.ReleaseAllEchoGuards()
	if ramMB, _ := detectHW(); ramMB > 0 {
		engine.SmuxWindowPool = tune.WindowPoolMiB(ramMB) << 20 // what the auto ceiling counted on
	}
	applyTuning()
	labProfile() // (lab: HS2_PROFILE=DIR)
	setMemoryLimit()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	configPath = *cfgPath
	raw, err := os.ReadFile(*cfgPath)
	must(err)
	var fc fileConfig
	must(json.Unmarshal(raw, &fc))
	// A mistyped bind_local_ip used to be silently ignored, so traffic left from
	// the server's default IP — on a multi-IP host possibly the filtered one — and
	// the tunnel looked mysteriously dead. Refuse to start instead.
	if fc.BindLocalIP != "" && net.ParseIP(fc.BindLocalIP) == nil {
		log.Fatalf("config: bind_local_ip %q is not a valid IP address", fc.BindLocalIP)
	}
	if fc.BindLocalIP != "" {
		log.Printf("egress: all tunnel connections will leave from %s", fc.BindLocalIP)
	}

	// Apply RAM/CPU-aware kernel tuning at every start, so it always matches the
	// current hardware and config (a resized VPS is picked up on restart). The
	// chosen congestion control is also used on the tunnel's own link sockets.
	// HS2_NO_TUNE=1 (used by the lab, where tuning is controlled by HS2_TUNE_*)
	// or lack of root skips the system sysctls but still logs the plan.
	plan := buildTunePlan(fc)
	if os.Geteuid() == 0 && os.Getenv("HS2_NO_TUNE") == "" {
		plan.Apply(func(f string, a ...any) { log.Printf(f, a...) })
	} else {
		log.Printf("tuning: %s (not applied: %s)", plan.Summary(), tuneSkipReason())
	}
	if _, envCC := os.LookupEnv("HS2_TUNE_CC"); !envCC && plan.Congestion != "" {
		tlscarrier.CongestionControl = plan.Congestion
	}
	if hasLinkPool(fc) {
		log.Print(ceilingLogLine(fc))
	}

	// The UDP/auto transports carry datagrams: keep the tunnel MTU small enough
	// that a sealed, FEC-wrapped IP packet still fits a 1500-byte path without
	// fragmenting (MTU + udpcarrier.CarrierOverhead = 52 bytes, plus the encapsulation header).
	if (fc.Carrier == "udp" || fc.Carrier == "auto" || fc.Carrier == "dgtun") && fc.MTU == 0 {
		fc.MTU = 1280
	}

	eng := engine.New(engine.Config{
		Iface: fc.Iface, LocalCIDR: fc.LocalCIDR, PeerIP: fc.PeerIP, MTU: fc.MTU,
	}, func(f string, a ...any) { log.Printf(f, a...) })

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Hard guarantee of prompt shutdown: when a signal cancels ctx, exit within
	// a short grace period no matter what a goroutine is doing. Without this a
	// single stuck read makes systemctl stop w