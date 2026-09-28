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

	// custom tunnel IP (optional): egress/listen on a specific server IP
	BindLocalIP  string `json:"bind_local_ip"`  // iran: source IP to dial from
	UserListenIP string `json:"user_listen_ip"` // iran: IP the user ports listen on
	Stream       bool   `json:"stream"`         // true = stream mode (TCP-only), false/absent = L3 (default)

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
		fmt.Println("hs2 v2 (carriers: l3mtcp, mtcp, tls)")
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

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	raw, err := os.ReadFile(*cfgPath)
	must(err)
	var fc fileConfig
	must(json.Unmarshal(raw, &fc))

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
	case "tls":
		runTLS(ctx, eng, fc)
	case "mtcp":
		runMTCP(ctx, fc) // stream mode (TCP-only)
	case "l3mtcp", "l3":
		runL3MTCP(ctx, fc) // L3 over multi-link (default)
	case "noise", "":
		runNoise(ctx, eng, fc)
	default:
		log.Fatalf("unknown carrier %q", fc.Carrier)
	}
}

func runReality(ctx context.Context, eng *engine.Engine, fc fileConfig) {
	key := unhex(fc.SharedKey)
	if fc.Mode == "dial" {
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

func runL3MTCP(ctx context.Context, fc fileConfig) {
	key := unhex(fc.SharedKey)
	logf := func(f string, a ...any) { log.Printf(f, a...) }
	mtu := fc.MTU
	if mtu == 0 {
		mtu = 1380
	}
	dev, err := tun.Open(fc.Iface, fc.LocalCIDR, fc.PeerIP, mtu)
	must(err)
	defer dev.Close()
	logf("tun %s up: %s peer %s", dev.Name(), fc.LocalCIDR, fc.PeerIP)

	if fc.Mode == "dial" {
		min, max := fc.MinLinks, fc.MaxLinks
		if min == 0 {
			min = 4
		}
		if max == 0 {
			max = 16
		}
		d := &l3DialerCfg{addr: fc.Addr, sni: fc.SNI, key: key, bindIP: fc.BindLocalIP}
		pool := engine.NewL3PoolFromCfg(d.addr, d.sni, d.key, d.bindIP, min, max, fc.PerLink, logf)
		// start user port forwarders (DNAT-free: forward over tunnel to peer panel)
		if fc.ForwardPorts != "" {
			go func() {
				time.Sleep(4 * time.Second)
				engine.RunPortForwardersOn(fc.UserListenIP, splitComma(fc.ForwardPorts), "10.77.0.2:9999", logf)
			}()
		}
		pool.Run(ctx, dev)
		return
	}
	// kharej: panel forwarder + accept links into shared TUN
	if fc.Expose != "" {
		go engine.RunPanelForwarder("10.77.0.2:9999", fc.Expose, logf)
	}
	cert, cerr := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	must(cerr)
	backend := fc.BackendAddr
	if backend == "" || backend == "builtin" {
		addr, berr := startBuiltinBackend()
		must(berr)
		backend = addr
	}
	bind := fc.Addr
	ln, lerr := engine.ListenReuse(bind)
	must(lerr)
	srv := &tlscarrier.Server{SharedKey: key, Cert: cert, BackendAddr: backend}
	pool := engine.NewL3KharejPool(dev, logf)
	must(pool.Serve(ctx, ln, srv))
}

type l3DialerCfg struct {
	addr, sni string
	key       []byte
	bindIP    string
}

func runMTCP(ctx context.Context, fc fileConfig) {
	key := unhex(fc.SharedKey)
	logf := func(f string, a ...any) { log.Printf(f, a...) }
	if fc.Mode == "dial" {
		d := engine.NewMTCPDialer(fc.Addr, fc.SNI, key)
		min, max, per := fc.MinLinks, fc.MaxLinks, fc.PerLink
		if min == 0 {
			min = 4
		}
		if max == 0 {
			max = 16
		}
		if per == 0 {
			per = 50
		}
		must(engine.RunMTCPIran(ctx, d, splitComma(fc.ForwardPorts), min, max, per, logf))
		return
	}
	// listen (kharej)
	cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	must(err)
	backend := fc.BackendAddr
	if backend == "" || backend == "builtin" {
		addr, err := startBuiltinBackend()
		must(err)
		backend = addr
		log.Printf("builtin probe backend on %s", addr)
	}
	ln, err := engine.ListenReuse(fc.Addr)
	must(err)
	srv := &tlscarrier.Server{SharedKey: key, Cert: cert, BackendAddr: backend}
	must(engine.RunMTCPKharej(ctx, ln, srv, fc.Expose, logf))
}

func runTLS(ctx context.Context, eng *engine.Engine, fc fileConfig) {
	key := unhex(fc.SharedKey)
	if fc.Mode == "dial" {
		d := engine.NewTLSDialer(fc.Addr, fc.SNI, key)
		if fc.ForwardPorts != "" {
			ports := splitComma(fc.ForwardPorts)
			go func() {
				// give the tunnel a moment to bring hs0 up, then open user ports
				time.Sleep(4 * time.Second)
				if err := engine.RunPortForwarders(ports, "10.77.0.2:9999", func(f string, a ...any) { log.Printf(f, a...) }); err != nil {
					log.Printf("port forwarders: %v", err)
				}
			}()
		}
		must(eng.RunDial(ctx, d))
		return
	}
	if fc.BackendAddr == "" || fc.BackendAddr == "builtin" {
		addr, err := startBuiltinBackend()
		must(err)
		fc.BackendAddr = addr
		log.Printf("builtin probe backend on %s", addr)
	}
	cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	must(err)
	// Panel forwarder listens on the kharej tunnel IP; the Iran side dials it
	// across the tunnel and it relays to the real panel.
	if fc.Expose != "" {
		go func() {
			if err := engine.RunPanelForwarder("10.77.0.2:9999", fc.Expose, func(f string, a ...any) { log.Printf(f, a...) }); err != nil {
				log.Printf("panel forwarder: %v", err)
			}
		}()
	}
	ln, err := engine.NewTLSListener(fc.Addr, fc.BackendAddr, key, cert)
	must(err)
	must(eng.RunListen(ctx, ln))
}

func runNoise(ctx context.Context, eng *engine.Engine, fc fileConfig) {
	local := core.StaticKey{Public: unhex(fc.LocalPub), Private: unhex(fc.LocalPriv)}
	if fc.Mode == "dial" {
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
