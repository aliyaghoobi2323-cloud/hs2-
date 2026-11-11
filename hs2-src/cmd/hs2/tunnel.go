package main

// The "Tunnels" section of the new menu (menu M3). Every screen action is a
// command:
//
//	hs2 tunnel list    [--json] [--lang fa]
//	hs2 tunnel create  [--from-json spec.json] [--yes] [--dry-run] [--advanced]
//	hs2 tunnel join    <code> [--update] [--user-ports P] [--expose A] [--cert self|PATH] [--yes] [--dry-run]
//	hs2 tunnel code    -u U [--public-ip IP]         connection code for the peer
//	hs2 tunnel clone   -u U --to-code [--public-ip IP] code that recreates THIS side elsewhere
//	hs2 tunnel edit    -u U [--section S] [--set k=v …] [--confirm-within 5m] [--yes]
//	hs2 tunnel renew   -u U [--pick-ip] [--upgrade-from BIN] [--yes] [--confirm-within 5m]
//	hs2 tunnel start|stop|restart -u U [--yes]
//	hs2 tunnel logs    -u U [-n 80] [-f]
//	hs2 tunnel delete  -u U [--yes --confirm-name U]
//	hs2 tunnel preflight (--from-json spec.json | -u U)
//
// Creation only writes anything at its very last step (after the summary is
// accepted), so a wizard aborted midway leaves nothing behind; a created
// tunnel that does not come up is removed again (create's own rollback).

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	hs2BinPath  = "/usr/local/bin/hs2"
	deletedRoot = "/var/lib/hs2/deleted"
	certDir     = "/etc/hs2/certs"
)

// tunnelSpec is the wizard's answer sheet (and the --from-json format).
type tunnelSpec struct {
	Name      string    `json:"name"`                // systemd unit: hs2 or hs2-<x>
	Role      string    `json:"role"`                // iran|kharej
	Direction string    `json:"direction"`           // direct|reverse
	Carrier   string    `json:"carrier"`             // mtcp|l3mtcp|tls|dgtun
	Encap     string    `json:"encap,omitempty"`     // dgtun: udp|icmp
	Front     string    `json:"front,omitempty"`     // tls (default) | reality (coming in M5)
	PublicIP  string    `json:"public_ip,omitempty"` // this server's address the peer dials (listener)
	Peer      string    `json:"peer,omitempty"`      // the other server's IP (the dialer needs it)
	BindIP    string    `json:"bind_ip,omitempty"`   // listener: listen IP; dialer: source IP
	Port      int       `json:"tunnel_port"`
	Domain    string    `json:"domain,omitempty"`
	Cert      string    `json:"cert,omitempty"` // listener+TLS: "self" | "auto" (letsencrypt live dir) | PEM path
	Key       string    `json:"key,omitempty"`
	UserPorts string    `json:"user_ports,omitempty"` // iran: user ports
	Expose    string    `json:"expose,omitempty"`     // kharej: default target (panel)
	Targets   string    `json:"targets,omitempty"`    // kharej: port_map "P=host:port,…"
	Subnet    string    `json:"subnet,omitempty"`     // /30 base; "" = a free one
	Iface     string    `json:"iface,omitempty"`      // "" = a free hsN
	MTU       int       `json:"mtu,omitempty"`
	UDP       bool      `json:"udp,omitempty"`
	SharedKey string    `json:"shared_key,omitempty"` // "" = generated
	Stats     bool      `json:"stats,omitempty"`      // enable the 24h chart sampler
	LB        *lbConfig `json:"lb,omitempty"`
}

func (s tunnelSpec) listener() bool { return (s.Role == "kharej") != (s.Direction == "reverse") }
func (s tunnelSpec) tlsFront() bool { return s.Carrier != "dgtun" }

var unitRe = regexp.MustCompile(`^hs2(-[a-z0-9][a-z0-9_-]{0,30})?$`)

func unitNameErr(n string) error {
	if !unitRe.MatchString(n) {
		return fmt.Errorf("tunnel name %q: use hs2 or hs2-<letters/digits>", n)
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// tunHostEnv is what normalize/preflight read from the host (fixtures in tests).
type tunHostEnv struct {
	etcDir    string
	validate  validateHost
	ifaceUsed func(string) bool
	ping      func(src, dst string) (ms float64, ok bool)
	dial      func(addr string) error
	lookup    func(host string) ([]string, error)
	exists    func(path string) bool
	sysctl    func(key string) string
	unitFile  func(unit string) bool
	publicIPs func() []string
}

// normalize fills defaults and rejects impossible answers. It never touches
// the host beyond reading (free names, subnets, ifaces).
func normalize(s *tunnelSpec, h tunHostEnv) error {
	s.Role = strings.ToLower(strings.TrimSpace(s.Role))
	if s.Role != "iran" && s.Role != "kharej" {
		return errors.New(tr("role must be iran or kharej", "نقش باید iran یا kharej باشد"))
	}
	if s.Direction == "" {
		s.Direction = "direct"
	}
	if s.Direction != "direct" && s.Direction != "reverse" {
		return fmt.Errorf("direction must be direct or reverse")
	}
	if s.Carrier == "" {
		s.Carrier = "mtcp"
	}
	if s.Carrier == "tun+mtcp" {
		s.Carrier = "l3mtcp"
	}
	if !validCarrier(s.Carrier) {
		return fmt.Errorf("carrier %q: use mtcp, l3mtcp (tun+mtcp) or dgtun", s.Carrier)
	}
	if s.Carrier == "dgtun" {
		if s.Encap == "" {
			s.Encap = "udp"
		}
		if s.Encap != "udp" && s.Encap != "icmp" {
			return fmt.Errorf("dgtun encap %q: the wizard offers udp or icmp (others: classic menu)", s.Encap)
		}
	} else {
		s.Encap = ""
	}
	if s.Front == "" {
		s.Front = "tls"
	}
	if s.Front == "reality" {
		return errors.New(tr("the Reality front is coming in a later release (M5) — use tls for now",
			"جلوی Reality در نسخهٔ بعدی (M5) می‌آید — فعلاً tls را انتخاب کنید"))
	}
	if s.Front != "tls" {
		return fmt.Errorf("front %q: tls (reality: coming)", s.Front)
	}
	if s.Name == "" {
		s.Name = freeUnitName(h)
	}
	if err := unitNameErr(s.Name); err != nil {
		return err
	}
	if h.unitFile != nil && h.unitFile(s.Name) {
		return fmt.Errorf(tr("a tunnel named %s already exists — pick another name (or edit it)", "تونلی به نام %s وجود دارد — نام دیگری بدهید (یا آن را ویرایش کنید)"), s.Name)
	}
	if s.Port <= 0 || s.Port > 65535 {
		return errors.New(tr("tunnel_port must be 1-65535", "پورت تونل باید ۱ تا ۶۵۵۳۵ باشد"))
	}
	if !s.listener() {
		if net.ParseIP(strings.Trim(s.Peer, "[]")) == nil && s.Peer == "" {
			return errors.New(tr("peer: the other server's IP is needed on the dialing side", "peer: سمتی که وصل می‌شود IP سرور مقابل را لازم دارد"))
		}
	}
	for _, ip := range []string{s.PublicIP, s.BindIP} {
		if ip != "" && net.ParseIP(ip) == nil {
			return fmt.Errorf("%q is not an IP address", ip)
		}
	}
	if s.Role == "iran" && s.UserPorts != "" {
		if _, err := parsePortList(s.UserPorts); err != nil {
			return fmt.Errorf("user_ports: %v", err)
		}
	}
	if s.Role == "kharej" && s.Expose == "" {
		s.Expose = "127.0.0.1:443"
	}
	if s.MTU == 0 {
		s.MTU = 1380
		if s.Carrier == "dgtun" {
			s.MTU = 1280
		}
	}
	if s.SharedKey == "" {
		s.SharedKey = randHex(32)
	}
	if s.Subnet == "" {
		s.Subnet = freeSubnet(h)
	}
	if ip := net.ParseIP(s.Subnet).To4(); ip == nil || ip[3]%4 != 0 {
		return fmt.Errorf("subnet %q must be an IPv4 /30 base (last byte a multiple of 4)", s.Subnet)
	}
	if s.Iface == "" {
		s.Iface = freeIface(h)
	}
	if s.listener() && s.tlsFront() && s.Cert == "" {
		s.Cert = "auto"
	}
	return nil
}

func parsePortList(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := strconv.Atoi(f)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("bad port %q", f)
		}
		out = append(out, p)
	}
	return out, nil
}

func otherConfigs(h tunHostEnv) []tunnelCfg { return listTunnelConfigs(h.etcDir) }

func freeUnitName(h tunHostEnv) string {
	taken := map[string]bool{}
	for _, t := range otherConfigs(h) {
		taken[t.unit] = true
	}
	ok := func(n string) bool { return !taken[n] && (h.unitFile == nil || !h.unitFile(n)) }
	if ok("hs2") {
		return "hs2"
	}
	for i := 2; ; i++ {
		if n := "hs2-t" + strconv.Itoa(i); ok(n) {
			return n
		}
	}
}

func freeSubnet(h tunHostEnv) string {
	used := map[string]bool{}
	for _, t := range otherConfigs(h) {
		if ip, _, err := net.ParseCIDR(t.fc.LocalCIDR); err == nil {
			used[subnetBase(ip.String())] = true
		}
	}
	for {
		var b [2]byte
		rand.Read(b[:])
		base := fmt.Sprintf("10.77.%d.%d", b[0], (b[1]%64)*4)
		if base != "10.77.0.0" && !used[base] {
			return base
		}
	}
}

func subnetBase(ip string) string {
	v := net.ParseIP(ip).To4()
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3]&^3)
}

func addToIP(base string, n byte) string {
	v := net.ParseIP(base).To4()
	return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3]+n)
}

func freeIface(h tunHostEnv) string {
	used := map[string]bool{}
	for _, t := range otherConfigs(h) {
		used[t.fc.Iface] = true
	}
	for i := 0; ; i++ {
		n := "hs" + strconv.Itoa(i)
		if !used[n] && (h.ifaceUsed == nil || !h.ifaceUsed(n)) {
			return n
		}
	}
}

// buildConfig turns a normalized spec into the tunnel config JSON, the same
// shape the classic installer writes.
func buildConfig(s tunnelSpec, coverSeed string) ([]byte, error) {
	m := map[string]any{
		"carrier": s.Carrier, "reverse": s.Direction == "reverse", "iface": s.Iface, "mtu": s.MTU,
		"shared_key": s.SharedKey,
	}
	iranIP, kharejIP := addToIP(s.Subnet, 1), addToIP(s.Subnet, 2)
	if s.Role == "iran" {
		m["mode"] = "dial"
		m["local_cidr"], m["peer_ip"] = iranIP+"/30", kharejIP
		if s.UserPorts != "" {
			m["forward_ports"] = s.UserPorts
		}
		if s.UDP {
			m["udp"] = true
		}
	} else {
		m["mode"] = "listen"
		m["local_cidr"], m["peer_ip"] = kharejIP+"/30", iranIP
		m["expose"] = s.Expose
		if s.Targets != "" {
			m["port_map"] = s.Targets
		}
	}
	if s.Carrier == "dgtun" {
		m["encap"] = s.Encap
	}
	if s.listener() {
		bind := s.BindIP
		if bind == "" {
			bind = "0.0.0.0"
			if ip := net.ParseIP(s.PublicIP); ip != nil && ip.To4() == nil {
				bind = "::"
			}
		}
		m["addr"] = net.JoinHostPort(bind, strconv.Itoa(s.Port))
		if s.tlsFront() {
			switch s.Cert {
			case "self":
				s.Cert, s.Key = filepath.Join(certDir, s.Name+".crt"), filepath.Join(certDir, s.Name+".key")
			case "auto":
				s.Cert, s.Key = "/etc/letsencrypt/live/"+s.Domain+"/fullchain.pem", "/etc/letsencrypt/live/"+s.Domain+"/privkey.pem"
			}
			m["backend_addr"] = "builtin"
			m["cover_seed"] = coverSeed
			m["cert_file"], m["key_file"] = s.Cert, s.Key
			// the listener's own tls name too: the per-IP "both" front
			// answers tls only for it (tlsNames), reality server_names
			// must differ from it, and codes / clones made from this
			// config carry the domain (specFromConfig)
			if s.Domain != "" {
				m["sni"] = s.Domain
			}
		}
	} else {
		m["addr"] = net.JoinHostPort(strings.Trim(s.Peer, "[]"), strconv.Itoa(s.Port))
		if s.tlsFront() && s.Domain != "" {
			m["sni"] = s.Domain
		}
		if s.BindIP != "" {
			m["bind_local_ip"] = s.BindIP
		}
	}
	if s.Carrier != "tls" {
		m["min_links"], m["max_links"], m["per_link"] = 2, 0, 8
	}
	if s.LB != nil {
		m["lb"] = s.LB
	}
	return marshalConfig(m)
}

// ---- connection code <-> spec -------------------------------------------

// codeFor is the code the PEER pastes. endpoint: the listener's host:port.
func codeFor(s tunnelSpec) tunnelCode {
	c := tunnelCode{Name: s.Name, Role: s.Role, Direction: s.Direction, Carrier: s.Carrier, Encap: s.Encap, Front: s.Front,
		SNI: s.Domain, SharedKey: s.SharedKey, MTU: s.MTU, Subnet: s.Subnet, UDP: s.UDP, LB: s.LB, HS2: "v3"}
	if s.listener() {
		host := s.PublicIP
		if host == "" {
			host = s.BindIP
		}
		c.Endpoint = net.JoinHostPort(host, strconv.Itoa(s.Port))
	} else {
		c.Endpoint = net.JoinHostPort(strings.Trim(s.Peer, "[]"), strconv.Itoa(s.Port))
		c.Dialer = s.PublicIP
	}
	if s.Role == "iran" {
		c.UserPorts = s.UserPorts
	} else {
		c.Panel = s.Expose
	}
	return c
}

// specFromCode is the joiner's prefilled answer sheet: the other role (or the
// same one for a clone code), the same direction/carrier/key/subnet.
func specFromCode(c tunnelCode, h tunHostEnv) tunnelSpec {
	s := tunnelSpec{Name: c.Name, Role: otherRole(c.Role), Direction: c.Direction, Carrier: c.Carrier, Encap: c.Encap,
		Front: c.Front, Domain: c.SNI, SharedKey: c.SharedKey, MTU: c.MTU, Subnet: c.Subnet, UDP: c.UDP, LB: c.LB}
	if c.Clone {
		s.Role = c.Role
		s.UserPorts, s.Expose, s.Targets = c.UserPorts, c.Panel, c.Targets
	}
	host, port, _ := net.SplitHostPort(c.Endpoint)
	s.Port, _ = strconv.Atoi(port)
	if s.listener() {
		// I am the listener: bind the endpoint's IP if it is mine (else all).
		if ip := net.ParseIP(host); ip != nil && h.validate.localIP != nil && h.validate.localIP(ip) {
			s.BindIP = host
		}
		s.PublicIP = host
		if c.Dialer != "" {
			s.Peer = c.Dialer
		}
	} else {
		s.Peer = host
	}
	if s.Role == "kharej" && s.Expose == "" {
		s.Expose = c.Panel
	}
	if s.LB != nil && !c.Clone {
		// the lists are per side; they stay as written (iran/kharej keys).
	}
	return s
}

// specFromConfig reverses buildConfig for an existing tunnel (code/clone/renew).
func specFromConfig(unit string, fc fileConfig, publicIP string) tunnelSpec {
	s := tunnelSpec{Name: cleanUnit(unit), Carrier: fc.Carrier, Encap: fc.Encap, Front: "tls", Domain: fc.SNI,
		SharedKey: fc.SharedKey, MTU: fc.MTU, UDP: fc.UDP, Iface: fc.Iface, LB: fc.LB,
		UserPorts: fc.ForwardPorts, Expose: fc.Expose, Targets: fc.PortMap, Cert: fc.CertFile, Key: fc.KeyFile}
	s.Role = "kharej"
	if fc.Mode == "dial" {
		s.Role = "iran"
	}
	s.Direction = "direct"
	if fc.Reverse {
		s.Direction = "reverse"
	}
	if ip, _, err := net.ParseCIDR(fc.LocalCIDR); err == nil {
		s.Subnet = subnetBase(ip.String())
	}
	host, port, _ := net.SplitHostPort(fc.Addr)
	s.Port, _ = strconv.Atoi(port)
	if s.listener() {
		if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
			s.BindIP = host
		}
		s.PublicIP = publicIP
		if s.PublicIP == "" {
			s.PublicIP = s.BindIP
		}
	} else {
		s.Peer = host
		s.BindIP = fc.BindLocalIP
		s.PublicIP = publicIP
	}
	return s
}

// ---- pre-flight -----------------------------------------------------------

type preCheck struct {
	Name, Status, Detail string // Status: ok|warn|fail|skip
}

func preflight(s tunnelSpec, cfg []byte, cfgPath string, h tunHostEnv) []preCheck {
	var out []preCheck
	add := func(name, st, d string) { out = append(out, preCheck{name, st, d}) }

	// ports + local IPs (shared with `hs2 config validate`)
	errs, warns := checkConfig(cfg, h.validate.localIP, time.Now())
	e2, w2 := crossCheck(cfg, nil, cfgPath, h.validate)
	errs, warns = append(errs, e2...), append(warns, w2...)
	if s.Cert == "self" { // generated at create, after the summary
		errs = slices.DeleteFunc(errs, selfCertPending)
	}
	if len(errs) == 0 {
		add(tr("ports & local IPs", "پورت‌ها و IPهای محلی"), "ok", tr("free, not used by another tunnel", "آزاد و بدون تداخل با تونل دیگر"))
	}
	for _, e := range errs {
		add(tr("ports & local IPs", "پورت‌ها و IPهای محلی"), "fail", e)
	}
	for _, w := range warns {
		add(tr("config", "پیکربندی"), "warn", w)
	}
	if s.listener() && s.PublicIP != "" && h.validate.localIP != nil {
		if ip := net.ParseIP(s.PublicIP); ip != nil && !h.validate.localIP(ip) {
			add(tr("public IP", "IP عمومی"), "warn", fmt.Sprintf(tr("%s is not on a local interface — fine behind NAT; the peer must reach it", "%s روی کارت شبکه نیست — پشت NAT اشکالی ندارد؛ سمت مقابل باید به آن برسد"), s.PublicIP))
		}
	}
	if s.listener() && s.PublicIP == "" {
		add(tr("public IP", "IP عمومی"), "warn", tr("not set — the code needs the address the peer dials (set public_ip)", "تعیین نشده — کد به آدرسی که سمت مقابل وصل می‌شود نیاز دارد (public_ip)"))
	}

	// peer reachability
	if s.Peer != "" && h.ping != nil {
		if ms, ok := h.ping(s.BindIP, strings.Trim(s.Peer, "[]")); ok {
			add(tr("peer ping", "پینگ سرور مقابل"), "ok", fmt.Sprintf("%s %.0f ms", s.Peer, ms))
		} else {
			add(tr("peer ping", "پینگ سرور مقابل"), "warn", fmt.Sprintf(tr("%s does not answer ping (ICMP may be filtered)", "%s به پینگ جواب نمی‌دهد (ممکن است ICMP بسته باشد)"), s.Peer))
		}
	}
	if !s.listener() && s.Carrier != "dgtun" && h.dial != nil {
		ep := net.JoinHostPort(strings.Trim(s.Peer, "[]"), strconv.Itoa(s.Port))
		if err := h.dial(ep); err == nil {
			add(tr("peer port", "پورت سرور مقابل"), "ok", ep+tr(" accepts TCP", " اتصال TCP را می‌پذیرد"))
		} else {
			add(tr("peer port", "پورت سرور مقابل"), "warn", fmt.Sprintf(tr("%s: %v — fine if the other side is not created yet; otherwise check its firewall", "%s: %v — اگر سمت مقابل هنوز ساخته نشده طبیعی است؛ وگرنه فایروال آن را بررسی کنید"), ep, err))
		}
	}

	// TLS: domain + certificate (listener only holds the cert)
	if s.tlsFront() && s.listener() {
		if s.Domain == "" {
			add(tr("domain", "دامنه"), "warn", tr("no domain: the tunnel works with a self-signed cover certificate (weaker camouflage). Better: a domain + Let's Encrypt (ACME HTTP-01, or DNS-01 when port 80 is busy), or the Reality front (coming)",
				"بدون دامنه: تونل با گواهی خودامضا کار می‌کند (استتار ضعیف‌تر). بهتر: دامنه + Let's Encrypt (ACME HTTP-01 یا DNS-01 وقتی پورت ۸۰ اشغال است) یا جلوی Reality (به‌زودی)"))
		} else if h.lookup != nil {
			ips, err := h.lookup(s.Domain)
			switch {
			case err != nil:
				add(tr("domain DNS", "DNS دامنه"), "warn", fmt.Sprintf("%s: %v", s.Domain, err))
			case s.PublicIP != "" && !slices.Contains(ips, s.PublicIP):
				add(tr("domain DNS", "DNS دامنه"), "warn", fmt.Sprintf(tr("%s points to %s, not to this server (%s)", "%s به %s اشاره می‌کند، نه به این سرور (%s)"), s.Domain, strings.Join(ips, ","), s.PublicIP))
			default:
				add(tr("domain DNS", "DNS دامنه"), "ok", s.Domain+" → "+strings.Join(ips, ","))
			}
		}
		switch {
		case s.Cert == "self":
			add(tr("certificate", "گواهی"), "ok", tr("a self-signed certificate is generated at create", "گواهی خودامضا هنگام ساخت تولید می‌شود"))
		case s.Cert != "" && s.Cert != "auto" && h.exists != nil && h.exists(s.Cert) && h.exists(s.Key):
			add(tr("certificate", "گواهی"), "ok", s.Cert)
		default:
			add(tr("certificate", "گواهی"), "fail", tr("no certificate: give cert/key PEM paths, cert=self (self-signed), or issue one first (classic menu → certificate, ACME HTTP-01 / DNS-01)",
				"گواهی نیست: مسیر cert/key بدهید، cert=self (خودامضا)، یا اول یکی صادر کنید (منوی کلاسیک → گواهی، ACME HTTP-01 / DNS-01)"))
		}
	}

	// kernel tuning (hs2 applies it at every start; this only reports)
	if h.sysctl != nil {
		cc := h.sysctl("net.ipv4.tcp_congestion_control")
		rm, _ := strconv.Atoi(h.sysctl("net.core.rmem_max"))
		if cc == "bbr" && rm >= 16<<20 {
			add(tr("kernel tuning", "تنظیمات کرنل"), "ok", "bbr, rmem_max "+strconv.Itoa(rm))
		} else {
			add(tr("kernel tuning", "تنظیمات کرنل"), "warn", fmt.Sprintf(tr("congestion=%s rmem_max=%d — hs2 tunes the kernel when the tunnel starts", "congestion=%s rmem_max=%d — hs2 هنگام شروع تونل کرنل را تنظیم می‌کند"), cc, rm))
		}
	}
	return out
}

func preflightFailed(cs []preCheck) bool {
	for _, c := range cs {
		if c.Status == "fail" {
			return true
		}
	}
	return false
}

func printChecks(cs []preCheck) {
	for _, c := range cs {
		mark := map[string]string{"ok": "[ ok ]", "warn": "[warn]", "fail": "[FAIL]", "skip": "[skip]"}[c.Status]
		fmt.Printf("  %s %-20s %s\n", mark, c.Name, c.Detail)
	}
}

// ---- list -------------------------------------------------------------------

type tunnelRow struct {
	Unit, State, Role, Direction, Carrier, Addr, Ports, Cfg string
}

func tunnelRows(etcDir string, info func(string) (string, int)) []tunnelRow {
	var rows []tunnelRow
	for _, t := range listTunnelConfigs(etcDir) {
		r := tunnelRow{Unit: t.unit, Cfg: t.cfg, Carrier: t.fc.Carrier, Addr: t.fc.Addr, Role: "kharej", Direction: "direct"}
		if t.fc.Mode == "dial" {
			r.Role = "iran"
		}
		if t.fc.Reverse {
			r.Direction = "reverse"
		}
		if t.fc.Carrier == "dgtun" && t.fc.Encap != "" {
			r.Carrier += "/" + t.fc.Encap
		}
		if r.Role == "iran" {
			r.Ports = t.fc.ForwardPorts
		} else {
			r.Ports = "→ " + t.fc.Expose
			if t.fc.PortMap != "" {
				r.Ports += " +" + t.fc.PortMap
			}
		}
		r.State = "?"
		if info != nil {
			if st, _ := info(t.unit); st != "" {
				r.State = st
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Unit < rows[b].Unit })
	return rows
}

func renderTunnelList(rows []tunnelRow) string {
	if len(rows) == 0 {
		return tr("No tunnel on this server yet — create one: hs2 tunnel create (or join a code: hs2 tunnel join <code>)\n",
			"هنوز تونلی روی این سرور نیست — بسازید: hs2 tunnel create (یا با کد: hs2 tunnel join <code>)\n")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  #  %-12s %-10s %-6s %-7s %-12s %-22s %s\n", tr("tunnel", "تونل"), tr("state", "وضعیت"), tr("role", "نقش"), tr("dir", "جهت"), tr("carrier", "حامل"), tr("tunnel addr", "آدرس تونل"), tr("user ports / targets", "پورت‌ها / مقصدها"))
	for i, r := range rows {
		fmt.Fprintf(&b, " %2d  %-12s %-10s %-6s %-7s %-12s %-22s %s\n", i+1, r.Unit, r.State, r.Role, r.Direction, r.Carrier, r.Addr, r.Ports)
	}
	return b.String()
}

// ---- SSH-over-the-tunnel guard ---------------------------------------------

// sshRidesTunnel: does this admin session (SSH_CONNECTION) come in through
// the tunnel (a user port of it, or a route via its interface)?
func sshRidesTunnel(sshConn string, fc fileConfig, routeDev func(ip string) string) string {
	f := strings.Fields(sshConn)
	if len(f) < 4 {
		return ""
	}
	client, sport := f[0], f[3]
	if ps, err := parsePortList(fc.ForwardPorts); err == nil {
		p, _ := strconv.Atoi(sport)
		if contains(ps, p) {
			return fmt.Sprintf(tr("your SSH session arrives on port %d, a user port of this tunnel — restarting/changing it can cut this session (safe apply rolls back on its own if you cannot confirm)",
				"نشست SSH شما از پورت %d می‌آید که پورت کاربر همین تونل است — ری‌استارت/تغییر می‌تواند این نشست را قطع کند (اعمال امن اگر تأیید نکنید خودش برمی‌گرداند)"), p)
		}
	}
	if routeDev != nil && fc.Iface != "" && routeDev(client) == fc.Iface {
		return fmt.Sprintf(tr("your SSH client %s is routed through %s (this tunnel) — changing it can cut this session",
			"کلاینت SSH شما (%s) از %s (همین تونل) عبور می‌کند — تغییر آن می‌تواند این نشست را قطع کند"), client, fc.Iface)
	}
	return ""
}

// selfCertPending: a check error that only says the (not yet generated)
// self-signed certificate is missing.
func selfCertPending(e string) bool {
	for _, p := range []string{"certificate", "key", "cert_file", "key_file", `"cert_file"`} {
		if strings.HasPrefix(e, p) {
			return true
		}
	}
	return false
}
