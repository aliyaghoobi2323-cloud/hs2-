// hs2 is the tunnel binary.
//
//	hs2 keygen                 -> print a fresh static keypair
//	hs2 run -c <config.json>   -> run the engine with the configured carrier
//	hs2 check -c <config.json> -> validate a config without starting anything
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
	MaxLinks int `json:"max_links"`
	PerLink  int `json:"per_link"` // concurrently active flows per link the pool sizes for
	// DrainIdleSec: when the pool shrinks, a connection on a retiring link
	// that has moved nothing for this many seconds is closed so the link can
	// finish. Unset = 310 (just above xray's 300 s connIdle); 0 = never.
	DrainIdleSec *int `json:"drain_idle_sec,omitempty"`

	// port forwarding (Backhaul-style reverse path)
	Expose       string `json:"expose"`        // kharej: real panel addr, e.g. 127.0.0.1:443
	ForwardPorts string `json:"forward_ports"` // iran: comma-sep user ports to open
	PeerPanel    string `json:"peer_panel"`    // iran: panel addr as known to kharej (informational)

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
	case "cleanup":
		cleanupCmd(os.Args[2:])
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
	num("HS2_TUNE_SMUX_SESSBUF", &engine.SmuxSessionBuffer)
	if v, ok := os.LookupEnv("HS2_TUNE_CC"); ok {
		tlscarrier.CongestionControl = v
		log.Printf("tuning: HS2_TUNE_CC=%q", v)
	}
}

func runCmd(args []string) {
	// Whatever path runCmd leaves by, no icmp reply rule outlives the daemon
	// (the listeners release theirs on close; this catches the rest).
	defer encap.ReleaseAllEchoGuards()
	applyTuning()
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
	// single stuck read makes systemctl stop wait for the kill timeout.
	go func() {
		<-ctx.Done()
		time.Sleep(3 * time.Second)
		encap.ReleaseAllEchoGuards() // os.Exit skips defers
		os.Exit(0)
	}()

	// SIGHUP hot-reloads the TLS certificate (the renewal deploy-hook sends it via
	// `systemctl reload`), so a Let's Encrypt renewal swaps the cert without
	// dropping the tunnel. A background watcher also picks up an on-disk change
	// and warns as expiry nears.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				reloadAllCerts("SIGHUP")
			}
		}
	}()
	go watchCerts(ctx)

	switch fc.Carrier {
	case "reality":
		runReality(ctx, eng, fc)
	case "mtcp": // link pool, no TUN
		runStream(ctx, fc, false, 0)
	case "l3mtcp", "l3": // link pool + hs0 side channel
		runStream(ctx, fc, true, 0)
	case "tls": // one link + hs0 side channel
		runStream(ctx, fc, true, 1)
	case "noise", "":
		runNoise(ctx, eng, fc)
	case "udp": // Noise + adaptive FEC over UDP (udp-only transport)
		runUDP(ctx, eng, fc, false)
	case "auto": // probe UDP, else fall back to TCP (default transport)
		runUDP(ctx, eng, fc, true)
	case "dgtun": // routed TUN over a POOL of datagram carriers (any encap) + autopilot
		runDgTun(ctx, fc)
	default:
		log.Fatalf("unknown carrier %q", fc.Carrier)
	}
}

// dialing reports whether this side initiates the connection. mode is the fixed
// role ("dial"=edge/iran, "listen"=exit/kharej); Reverse flips who dials. So the
// edge dials in direct and listens in reverse, and vice versa for the exit.
func dialing(fc fileConfig) bool { return (fc.Mode == "dial") != fc.Reverse }

func runReality(ctx context.Context, eng *engine.Engine, fc fileConfig) {
	key := unhex(fc.SharedKey)
	if dialing(fc) {
		d := engine.NewRealityDialer(fc.Addr, fc.SNI, key)
		must(eng.RunDial(ctx, d))
		return
	}
	cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	must(err)
	ln, err := engine.NewRealityListener(fc.Addr, fc.CoverAddr, key, cert)
	must(err)
	must(eng.RunListen(ctx, ln))
}

// runStream runs any TLS carrier in stream mode (see engine/stream.go). User
// TCP (and optionally UDP) always rides smux streams; withTUN adds hs0 as a
// side channel for other traffic; links > 0 pins the pool size.
//
// The role is the config mode: "dial" = edge (iran, user ports), "listen" =
// exit (kharej, panel). fc.Reverse flips WHO dials the TLS carrier without
// changing those roles: in reverse the edge LISTENS and the exit DIALS.
func runStream(ctx context.Context, fc fileConfig, withTUN bool, links int) {
	key := unhex(fc.SharedKey)
	logf := func(f string, a ...any) { log.Printf(f, a...) }
	var dev *tun.Device
	if withTUN {
		mtu := fc.MTU
		if mtu == 0 {
			mtu = 1380
		}
		d, err := tun.Open(fc.Iface, fc.LocalCIDR, fc.PeerIP, mtu)
		must(err)
		defer d.Close()
		dev = d
		logf("tun %s up: %s peer %s (side channel)", d.Name(), fc.LocalCIDR, fc.PeerIP)
	}
	edge := fc.Mode == "dial"

	if edge {
		min, max, per := linkEnvelope(fc)
		if links > 0 { // tls carrier: pin to a single link
			min, max = links, links
		}
		cfg := engine.IranConfig{
			Min: min, Max: max, PerLink: per, DrainIdle: drainIdle(fc),
			ListenIP: fc.UserListenIP,
			Ports:    splitComma(fc.ForwardPorts),
			UDP:      fc.UDP,
			Log:      logf,
			OnStart:  func(s engine.StatsFn) { startStatusWriter(ctx, fc, configPath, s) },
		}
		if dev != nil {
			cfg.TUN = dev
		}
		if fc.Reverse {
			// Reverse edge: the iran side LISTENS for links the kharej dials in.
			// It needs a cert (it is the TLS server now) and a probe backend.
			cr, err := newCertReloader(fc.CertFile, fc.KeyFile)
			must(err)
			ln, err := engine.ListenReuse(fc.Addr)
			must(err)
			cfg.RevServer = &tlscarrier.Server{SharedKey: key, GetCertificate: cr.getCertificate, BackendAddr: streamBackend(fc), Logf: logf}
			cfg.RevListener = ln
			logf("stream edge (reverse): listening for kharej links on %s", fc.Addr)
		} else {
			cfg.Dialer = engine.NewMTCPDialer(fc.Addr, fc.SNI, key, fc.BindLocalIP)
		}
		must(engine.RunIran(ctx, cfg))
		return
	}

	// exit (kharej): has the panel.
	cfg := engine.KharejConfig{Panel: fc.Expose, Log: logf,
		OnStart: func(s engine.StatsFn) { startStatusWriter(ctx, fc, configPath, s) }}
	if dev != nil {
		cfg.TUN = dev
	}
	if fc.Reverse {
		// Reverse exit: the kharej DIALS the iran edge (a TLS server) and runs a
		// DYNAMIC pool of links. No cert here; it is the TLS client now. The edge
		// drives the count over the pool-control channel between RevMin and RevMax;
		// RevLinks is only the size held until the edge first speaks.
		min, max, _ := linkEnvelope(fc)
		if links > 0 { // tls carrier: single link
			min, max = links, links
		}
		cfg.RevMin, cfg.RevMax = min, max
		initial := 8
		if initial < min {
			initial = min
		}
		if initial > max {
			initial = max
		}
		cfg.RevLinks = initial
		cfg.RevDial = func() (*tlscarrier.Carrier, error) {
			return tlscarrier.DialFrom(fc.Addr, fc.SNI, key, fc.BindLocalIP)
		}
		logf("stream exit (reverse): dynamic link pool %d–%d to edge %s (edge drives the count)", min, max, fc.Addr)
		must(engine.RunKharej(ctx, cfg))
		return
	}
	cr, err := newCertReloader(fc.CertFile, fc.KeyFile)
	must(err)
	ln, err := engine.ListenReuse(fc.Addr)
	must(err)
	cfg.Listener = ln
	cfg.Server = &tlscarrier.Server{SharedKey: key, GetCertificate: cr.getCertificate, BackendAddr: streamBackend(fc), Logf: logf}
	must(engine.RunKharej(ctx, cfg))
}

// buildTunePlan turns the config's tuning section (or the auto default) plus the
// detected hardware into a tuning Plan.
func buildTunePlan(fc fileConfig) *tune.Plan {
	var cfg tune.Config
	if fc.Tuning != nil {
		cfg = *fc.Tuning
	}
	ramMB, cpus := tune.Detect()
	return tune.Build(cfg, ramMB, cpus, tune.AvailableCC, tune.AvailableQdisc)
}

func tuneSkipReason() string {
	if os.Geteuid() != 0 {
		return "not root"
	}
	return "HS2_NO_TUNE set"
}

// tuneCmd implements `hs2 tune -c config [--apply]`: print the tuning plan for
// this server and config, and optionally apply it. It lets the operator see
// exactly what auto-tuning chose, and try manual overrides, without starting the
// tunnel.
func tuneCmd(args []string) {
	fs := flag.NewFlagSet("tune", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON); optional — without it, shows the auto plan for this server")
	apply := fs.Bool("apply", false, "apply the plan now (needs root)")
	fs.Parse(args)
	var fc fileConfig
	if *cfgPath != "" {
		if raw, err := os.ReadFile(*cfgPath); err == nil {
			json.Unmarshal(raw, &fc)
		}
	}
	plan := buildTunePlan(fc)
	fmt.Print(plan.Report())
	if *apply {
		if os.Geteuid() != 0 {
			fmt.Println("(need root to apply)")
			os.Exit(1)
		}
		plan.Apply(func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	}
}

// recommendLinksCmd implements `hs2 recommend-links [--why]`: print the adaptive
// link-pool ceiling this server's RAM and cores justify. The installer calls the
// bare form and writes the number straight into the config, so the ceiling is
// chosen once, from the hardware, and is then visible and fixed in the file (not
// a hidden default). --why adds the profile and the detected hardware for the
// human-facing menu. It is read-only and needs no config or root.
func recommendLinksCmd(args []string) {
	fs := flag.NewFlagSet("recommend-links", flag.ExitOnError)
	why := fs.Bool("why", false, "also print the profile and detected hardware")
	fs.Parse(args)
	ramMB, cpus := tune.Detect()
	maxLinks := tune.RecommendedMaxLinks(ramMB, cpus)
	if !*why {
		fmt.Println(maxLinks)
		return
	}
	fmt.Printf("%d  (%s profile: %s RAM, %d core(s))\n", maxLinks, tune.ProfileFor(ramMB, cpus), ramStr(ramMB), cpus)
}

// ramStr prints a RAM size in MB the way the tune report does (GB past 1024).
func ramStr(mb int) string {
	if mb <= 0 {
		return "unknown"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// linkEnvelope resolves the adaptive link-pool bounds from the config, applying
// the defaults: the pattern lives anywhere in 2–32 links, with at least one link
// for every per_link (default 8) connections that are actively moving data. The
// pool is never fixed at these numbers — the autopilot moves it continuously
// inside the envelope from the measured traffic, up and back down (see
// engine/autopilot.go).
func linkEnvelope(fc fileConfig) (min, max, per int) {
	min, max, per = fc.MinLinks, fc.MaxLinks, fc.PerLink
	if min <= 0 {
		min = 2
	}
	if max <= 0 {
		max = 32
	}
	if max < min {
		max = min
	}
	if per <= 0 {
		per = 8
	}
	return min, max, per
}

// drainIdle maps drain_idle_sec to the engine's setting: unset → 0 (the
// engine default), 0 → never (negative), n → n seconds.
func drainIdle(fc fileConfig) time.Duration {
	switch {
	case fc.DrainIdleSec == nil:
		return 0
	case *fc.DrainIdleSec <= 0:
		return -1
	default:
		return time.Duration(*fc.DrainIdleSec) * time.Second
	}
}

// streamBackend returns the probe-forwarding backend for a TLS-server side,
// starting the built-in boring website when none is configured.
func streamBackend(fc fileConfig) string {
	backend := fc.BackendAddr
	if backend == "" || backend == "builtin" {
		addr, err := startBuiltinBackend(fc.CoverSeed)
		must(err)
		backend = addr
	}
	return backend
}

// runUDP runs the UDP transport (Noise + adaptive FEC). With auto=true the
// dialer probes UDP and silently falls back to the TCP noise carrier when UDP
// is unreachable or too lossy; the listener serves both. Keys are derived from
// shared_key, so no static keypair config is needed.
func runUDP(ctx context.Context, eng *engine.Engine, fc fileConfig, auto bool) {
	shared := unhex(fc.SharedKey)
	mtu := fc.MTU
	if mtu == 0 {
		mtu = 1280
	}
	logf := func(f string, a ...any) { log.Printf(f, a...) }
	if dialing(fc) {
		var d engine.CarrierDialer
		if auto {
			d = engine.NewAutoDialer(fc.Addr, fc.BindLocalIP, shared, mtu, logf)
		} else {
			d = engine.NewUDPDialer(fc.Addr, fc.BindLocalIP, shared, mtu)
		}
		must(eng.RunDial(ctx, d))
		return
	}
	var ln engine.CarrierListener
	var err error
	if auto {
		ln, err = engine.NewAutoListener(fc.Addr, shared, mtu)
	} else {
		ln, err = engine.NewUDPListener(fc.Addr, shared, mtu)
	}
	must(err)
	must(eng.RunListen(ctx, ln))
}

// runDgTun runs the gen-2 datagram TUN: a routed interface carried over a POOL
// of datagram carriers (udpcarrier over the configured encapsulation), sized by
// the same autopilot as the stream pool, with an optional userspace port
// forwarder so user ports on the edge reach the panel over the tun. There is no
// TCP inside the carrier — user connections ride as ordinary IP packets, so no
// TCP-in-TCP.
func runDgTun(ctx context.Context, fc fileConfig) {
	shared := unhex(fc.SharedKey)
	mtu := fc.MTU
	if mtu == 0 {
		mtu = 1280
	}
	logf := func(f string, a ...any) { log.Printf(f, a...) }
	dev, err := tun.Open(fc.Iface, fc.LocalCIDR, fc.PeerIP, mtu)
	must(err)
	defer dev.Close()
	logf("tun %s up: %s peer %s mtu %d (datagram pool, encap %s)", dev.Name(), fc.LocalCIDR, fc.PeerIP, mtu, encapName(fc))

	ec := engine.EncapConfig{Kind: fc.Encap, BindIP: fc.BindLocalIP, Proto: fc.Proto}
	min, max, per := linkEnvelope(fc)
	edge := fc.Mode == "dial" // iran = edge (users), kharej = exit (panel)

	cfg := engine.DgConfig{Dev: dev, Min: min, Max: max, PerLink: per, Reverse: fc.Reverse, Log: logf,
		OnStart: func(s engine.StatsFn) { startStatusWriter(ctx, fc, configPath, s) }}

	// Who dials the carriers: direct = edge dials; reverse = exit dials.
	if engineDialsTransport(fc) {
		cfg.Dialer = engine.NewDgDialer(fc.Addr, ec, shared, mtu)
	} else {
		ln, err := engine.NewDgListener(fc.Addr, ec, shared, mtu)
		must(err)
		cfg.Listener = ln
	}

	// User-port forwarder (Backhaul-style [ports]): the edge opens forward_ports
	// and sends them over the tun to the exit's single on-tun port; the exit hands
	// what arrives there to the panel (expose). The ports live only on the edge.
	if edge {
		if ports := splitComma(fc.ForwardPorts); len(ports) > 0 {
			must(engine.StartDgForwarders(ctx, true, ports, fc.UserListenIP, fc.PeerIP, "", "", fc.UDP, logf))
		}
	} else if fc.Expose != "" {
		must(engine.StartDgForwarders(ctx, false, nil, "", "", ipOfCIDR(fc.LocalCIDR), fc.Expose, fc.UDP, logf))
	}

	if edge {
		must(engine.RunDgEdge(ctx, cfg))
	} else {
		must(engine.RunDgExit(ctx, cfg))
	}
}

// engineDialsTransport reports whether THIS side dials the datagram carriers.
// Direct: the edge (mode=dial) dials. Reverse: the exit (mode=listen) dials.
func engineDialsTransport(fc fileConfig) bool { return (fc.Mode == "dial") != fc.Reverse }

// encapName is the encapsulation label for logs ("udp" when unset).
func encapName(fc fileConfig) string {
	if fc.Encap == "" {
		return "udp"
	}
	return fc.Encap
}

// ipOfCIDR returns the IP part of a CIDR ("10.77.0.2/30" -> "10.77.0.2").
func ipOfCIDR(cidr string) string {
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		return cidr[:i]
	}
	return cidr
}

func runNoise(ctx context.Context, eng *engine.Engine, fc fileConfig) {
	local := core.StaticKey{Public: unhex(fc.LocalPub), Private: unhex(fc.LocalPriv)}
	if dialing(fc) {
		d := engine.NewNoiseDialer(fc.Addr, fc.BindLocalIP, local, unhex(fc.RemoteStatic), unhex(fc.PSK))
		must(eng.RunDial(ctx, d))
		return
	}
	ln, err := engine.NewNoiseListener(fc.Addr, local, unhex(fc.PSK))
	must(err)
	must(eng.RunListen(ctx, ln))
}

// startBuiltinBackend serves an ordinary-looking static site on loopback. Probes
// that fail tunnel auth are proxied here (already TLS-decrypted), so they get a
// plain, boring web page from a real HTTP server instead of a tunnel error.
//
// The page is per-install: buildCover turns the config's cover_seed into a page
// whose brand, copy, colours, layout and size vary, so a bulk scanner cannot
// find every hs2 server by one known sha256 (see cover.go). An empty seed (an
// old or hand-written config) yields the fixed legacy page, unchanged.
func startBuiltinBackend(seed string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	// The page is fully self-contained: no external fonts, scripts or images —
	// each of those would be its own request, fingerprint or dependency — with
	// an inline data-URI favicon, system fonts, light/dark and no JavaScript.
	// Last-Modified is a seed-derived span in the past, computed at startup from
	// THIS host's clock and truncated to the hour: relative (not a frozen
	// absolute date) so it is always earlier than the auto Date header — no
	// "modified in the future" even on a lagging clock — and the per-install
	// offset means the "last modified N days ago" distance is not a constant.
	page, offDays := buildCover(seed, time.Now())
	modtime := time.Now().Add(-time.Duration(offDays) * 24 * time.Hour).Truncate(time.Hour)
	// ETag, when set, is SALTED with the seed — NOT a plain hash of the body. A
	// bare sha256(body) ETag is a single-probe fingerprint: anyone can fetch the
	// page, hash it, and confirm the server uses hs2's exact rule. Salting keeps
	// the cache semantics (stable per seed+content, changes when content does)
	// while making it unpredictable from the body alone, like a real server's
	// mtime/inode ETag. The seedless legacy page sets NO ETag at all, exactly as
	// the pre-per-install binary did (un-migrated installs stay byte-for-byte).
	var etag string
	if seed != "" {
		h := sha256.Sum256(append([]byte("hs2-cover-etag\x00"+seed+"\x00"), page...))
		etag = fmt.Sprintf("%q", hex.EncodeToString(h[:16]))
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		// Everything but "/" is a plain Go 404, like any static file server. No
		// Server header is sent: our TLS terminator is Go's, so a stray "nginx"
		// claim only contradicted that at the TLS layer (Caddy omits it too).
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=3600")
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		// ServeContent gives genuine static-server behaviour for free:
		// Last-Modified, If-Modified-Since / If-None-Match (304), Content-Length
		// and range requests.
		http.ServeContent(w, r, "", modtime, bytes.NewReader(page))
	}
	srv := &http.Server{Handler: http.HandlerFunc(handler), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return ln.Addr().String(), nil
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func unhex(s string) []byte { b, _ := hex.DecodeString(s); return b }
func must(err error) {
	if err != nil {
		encap.ReleaseAllEchoGuards() // log.Fatal exits without running defers
		log.Fatal(err)
	}
}
