// hs2 is the tunnel binary.
//
//	hs2 keygen                 -> print a fresh static keypair
//	hs2 run -c <config.json>   -> run the engine with the configured carrier
package main

import (
	"context"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tun"
)

type fileConfig struct {
	// Role & interface
	Mode      string `json:"mode"` // "dial" or "listen"
	Iface     string `json:"iface"`
	LocalCIDR string `json:"local_cidr"`
	PeerIP    string `json:"peer_ip"`
	MTU       int    `json:"mtu"`

	// Carrier selection
	Carrier string `json:"carrier"` // "reality" or "noise"
	Addr    string `json:"addr"`    // dial target or listen bind

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
	PerLink  int `json:"per_link"`

	// port forwarding (Backhaul-style reverse path)
	Expose       string `json:"expose"`        // kharej: real panel addr, e.g. 127.0.0.1:443
	ForwardPorts string `json:"forward_ports"` // iran: comma-sep user ports to open
	PeerPanel    string `json:"peer_panel"`    // iran: panel addr as known to kharej (informational)

	// tls / reality carrier
	SNI         string `json:"sni"`          // domain (dial)
	CoverAddr   string `json:"cover_addr"`   // reality: real site to forward probes to
	BackendAddr string `json:"backend_addr"` // tls: local backend probes are proxied to
	SharedKey   string `json:"shared_key"`   // hex, both sides
	CertFile    string `json:"cert_file"`    // listen: real cert PEM (Let's Encrypt)
	KeyFile     string `json:"key_file"`     // listen: real key PEM

	// noise carrier
	LocalPriv    string `json:"local_priv"`
	LocalPub     string `json:"local_pub"`
	RemoteStatic string `json:"remote_static"`
	PSK          string `json:"psk"`
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Println("usage: hs2 keygen | hs2 run -c config.json")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Println("hs2 v3 (stream core; carriers: mtcp, l3mtcp, tls, udp, auto; auth: tls-exporter bound, mutual)")
	case "keygen":
		k, err := core.GenerateStatic()
		must(err)
		fmt.Printf("private: %s\npublic:  %s\n", hex.EncodeToString(k.Private), hex.EncodeToString(k.Public))
	case "run":
		runCmd(os.Args[2:])
	default:
		fmt.Println("unknown command")
		os.Exit(2)
	}
}

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
	applyTuning()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	raw, err := os.ReadFile(*cfgPath)
	must(err)
	var fc fileConfig
	must(json.Unmarshal(raw, &fc))

	// The UDP/auto transports carry datagrams: keep the tunnel MTU small enough
	// that a sealed, FEC-wrapped IP packet still fits a 1500-byte path without
	// fragmenting (≈ MTU + 47 bytes on the wire).
	if (fc.Carrier == "udp" || fc.Carrier == "auto") && fc.MTU == 0 {
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
		os.Exit(0)
	}()

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
		// Defaults chosen in the lab: 4 links cannot get past per-connection
		// throttling, 8 can; the pool grows toward 16 with users.
		min, max, per := fc.MinLinks, fc.MaxLinks, fc.PerLink
		if min == 0 {
			min = 8
		}
		if max == 0 {
			max = 16
		}
		if per == 0 {
			per = 8
		}
		if links > 0 {
			min, max = links, links
		}
		cfg := engine.IranConfig{
			Min: min, Max: max, PerLink: per,
			ListenIP: fc.UserListenIP,
			Ports:    splitComma(fc.ForwardPorts),
			UDP:      fc.UDP,
			Log:      logf,
		}
		if dev != nil {
			cfg.TUN = dev
		}
		if fc.Reverse {
			// Reverse edge: the iran side LISTENS for links the kharej dials in.
			// It needs a cert (it is the TLS server now) and a probe backend.
			cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
			must(err)
			ln, err := engine.ListenReuse(fc.Addr)
			must(err)
			cfg.RevServer = &tlscarrier.Server{SharedKey: key, Cert: cert, BackendAddr: streamBackend(fc), Logf: logf}
			cfg.RevListener = ln
			logf("stream edge (reverse): listening for kharej links on %s", fc.Addr)
		} else {
			cfg.Dialer = engine.NewMTCPDialer(fc.Addr, fc.SNI, key, fc.BindLocalIP)
		}
		must(engine.RunIran(ctx, cfg))
		return
	}

	// exit (kharej): has the panel.
	cfg := engine.KharejConfig{Panel: fc.Expose, Log: logf}
	if dev != nil {
		cfg.TUN = dev
	}
	if fc.Reverse {
		// Reverse exit: the kharej DIALS the iran edge (a TLS server) and runs
		// a fixed pool of links. No cert here; it is the TLS client now.
		n := fc.MinLinks
		if links > 0 {
			n = links
		}
		if n == 0 {
			n = 8
		}
		cfg.RevLinks = n
		cfg.RevDial = func() (*tlscarrier.Carrier, error) {
			return tlscarrier.DialFrom(fc.Addr, fc.SNI, key, fc.BindLocalIP)
		}
		logf("stream exit (reverse): dialing %d links to edge %s", n, fc.Addr)
		must(engine.RunKharej(ctx, cfg))
		return
	}
	cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	must(err)
	ln, err := engine.ListenReuse(fc.Addr)
	must(err)
	cfg.Listener = ln
	cfg.Server = &tlscarrier.Server{SharedKey: key, Cert: cert, BackendAddr: streamBackend(fc), Logf: logf}
	must(engine.RunKharej(ctx, cfg))
}

// streamBackend returns the probe-forwarding backend for a TLS-server side,
// starting the built-in boring website when none is configured.
func streamBackend(fc fileConfig) string {
	backend := fc.BackendAddr
	if backend == "" || backend == "builtin" {
		addr, err := startBuiltinBackend()
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
			d = engine.NewAutoDialer(fc.Addr, shared, mtu, logf)
		} else {
			d = engine.NewUDPDialer(fc.Addr, shared, mtu)
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

func runNoise(ctx context.Context, eng *engine.Engine, fc fileConfig) {
	local := core.StaticKey{Public: unhex(fc.LocalPub), Private: unhex(fc.LocalPriv)}
	if dialing(fc) {
		d := engine.NewNoiseDialer(fc.Addr, local, unhex(fc.RemoteStatic), unhex(fc.PSK))
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
func startBuiltinBackend() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	page := []byte("<!doctype html><html><head><title>Welcome</title></head><body><h1>Welcome</h1><p>This site is under construction.</p></body></html>")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
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
		log.Fatal(err)
	}
}
