package main

// Side-aware Reality targets (branch target43).
//
// The censor sees the server name (SNI) and the IP that answers it, so the
// right target depends on which server is the TLS listener of the links
// (tunnelListens; the carrier does not change it — mtcp, l3mtcp and tls all
// listen on the same side):
//
//	direct  (reverse:false): the kharej server listens, the Iran edge dials
//	reverse (reverse:true):  the Iran edge listens, the kharej server dials in
//
// An Iran listener wants an Iranian site hosted on Iranian IPs (the fallback
// splice to the target leaves from the Iran server too); a kharej listener
// wants a foreign site, ideally on the same provider / ASN. A foreign name on
// an Iranian IP (or the other way round) is an SNI <-> IP mismatch tell.
//
// The target's country / ASN comes from an offline table (well-known Iranian
// and European hosting ranges, Iranian ASNs — the registry country of some
// Iranian networks is wrong, e.g. Respina AS42337 is registered AE) and,
// when reachable, the Team Cymru DNS whois (TXT origin.asn.cymru.com, short
// timeout, never required). The TCP connect time is a third, weak signal
// when this server is the listener.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// realitySide says which server is the TLS listener of a tunnel.
type realitySide struct {
	Known        bool     `json:"known"`
	Reverse      bool     `json:"reverse"`
	Carrier      string   `json:"carrier,omitempty"`
	ListenerIran bool     `json:"listener_iran"`
	ThisListens  bool     `json:"this_listens"`
	ListenerIPs  []string `json:"listener_ips,omitempty"`
	Source       string   `json:"source,omitempty"`
}

// sideOfConfig: the listening side of fc's links.
func sideOfConfig(fc fileConfig) realitySide {
	s := realitySide{Known: true, Reverse: fc.Reverse, Carrier: fc.Carrier, ThisListens: tunnelListens(fc)}
	// Iran = the edge (mode dial); it listens exactly in reverse mode.
	s.ListenerIran = fc.Reverse
	if s.ThisListens {
		for _, e := range lbLocalList(fc) {
			s.ListenerIPs = append(s.ListenerIPs, e.IP)
		}
		if len(s.ListenerIPs) == 0 {
			if h, _, err := net.SplitHostPort(fc.Addr); err == nil && net.ParseIP(h) != nil && !net.ParseIP(h).IsUnspecified() {
				s.ListenerIPs = append(s.ListenerIPs, h)
			} else if ip := outboundIP(); ip != "" {
				s.ListenerIPs = append(s.ListenerIPs, ip)
			}
		}
	} else {
		_, peers := lbSide(fc)
		s.ListenerIPs = peers
	}
	return s
}

// sideFromFlag: --side iran|kharej (where the listener is).
func sideFromFlag(v string) (realitySide, bool) {
	switch strings.ToLower(v) {
	case "iran", "ir", "reverse":
		return realitySide{Known: true, ListenerIran: true, Reverse: true, Source: "--side"}, true
	case "kharej", "foreign", "direct":
		return realitySide{Known: true, Source: "--side"}, true
	}
	return realitySide{}, false
}

// resolveSide: --side wins, else the tunnel's config (-u; the default unit
// only when its config exists), else unknown.
func resolveSide(flagSide, unit string, unitGiven bool) realitySide {
	if s, ok := sideFromFlag(flagSide); ok {
		return s
	}
	b, err := os.ReadFile(configForUnit(unit))
	if err != nil {
		if unitGiven {
			fmt.Printf(tr("WARN: config of %s not readable (%v): the listener side is unknown\n", "هشدار: پیکربندی %s خوانده نشد (%v): سمت شنونده نامعلوم است\n"), unit, err)
		}
		return realitySide{}
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return realitySide{}
	}
	s := sideOfConfig(fc)
	s.Source = cleanUnit(unit)
	return s
}

// outboundIP: the source address of the default route ("" offline).
func outboundIP() string {
	c, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// describeSide is the operator's one-paragraph view of the side.
func describeSide(s realitySide, lgeo []ipGeo) string {
	if !s.Known {
		return tr("Listener side: unknown (give -u TUNNEL or --side iran|kharej). Candidates of both sides are checked.",
			"سمت شنونده: نامعلوم (-u TUNNEL یا --side iran|kharej بدهید). نامزدهای هر دو سمت بررسی می‌شوند.")
	}
	var b strings.Builder
	if s.ListenerIran {
		b.WriteString(tr("Listener: the IRAN server (reverse mode: kharej dials into it). The censor sees an Iranian IP, so the target must be an IRANIAN site hosted in Iran (the splice to it also leaves from Iran).",
			"شنونده: سرور ایران (حالت معکوس: خارج به آن وصل می‌شود). سانسورگر یک IP ایرانی می‌بیند، پس هدف باید یک سایت ایرانی میزبانی‌شده در ایران باشد (اتصال به هدف هم از ایران می‌رود)."))
	} else {
		b.WriteString(tr("Listener: the KHAREJ server (direct mode: the Iran server dials out to it). The target must be a FOREIGN site, best on the same provider / ASN as the kharej server.",
			"شنونده: سرور خارج (حالت مستقیم: سرور ایران به آن وصل می‌شود). هدف باید یک سایت خارجی باشد، بهتر از همان ارائه‌دهنده / ASN سرور خارج."))
	}
	for _, g := range lgeo {
		fmt.Fprintf(&b, "\n  %s %s: %s", tr("listener IP", "IP شنونده"), g.IP, g.label())
	}
	if !s.ThisListens && s.Source != "--side" {
		b.WriteString("\n" + tr("NOTE: this server DIALS; latency and reachability below are measured from here, not from the listener — run it on the listening server for the real numbers.",
			"توجه: این سرور شماره‌گیر است؛ تأخیر و دسترسی زیر از همین‌جا سنجیده شده نه از شنونده — برای عدد واقعی روی سرور شنونده اجرا کنید."))
	}
	return b.String()
}

// ---- geo ---------------------------------------------------------------------------

// ipGeo is what we know of one address.
type ipGeo struct {
	IP      string `json:"ip"`
	Country string `json:"country,omitempty"` // ISO code, "" unknown
	ASN     int    `json:"asn,omitempty"`
	Org     string `json:"org,omitempty"`
	CDN     string `json:"cdn,omitempty"` // a multi-tenant CDN edge
	Src     string `json:"src,omitempty"` // table | cymru
}

func (g ipGeo) label() string {
	if g.Country == "" && g.ASN == 0 {
		return tr("unknown", "نامعلوم")
	}
	s := g.Country
	if g.ASN != 0 {
		s += fmt.Sprintf(" AS%d", g.ASN)
	}
	if g.Org != "" {
		s += " " + g.Org
	}
	if g.CDN != "" {
		s += " [CDN]"
	}
	return strings.TrimSpace(s)
}

// asnInfo: country / name / CDN of well-known networks (the registry
// country of Iranian networks is sometimes wrong, so the ASN decides).
type asnInfo struct {
	cc, org, cdn string
}

var knownASNs = map[int]asnInfo{
	// Iran
	12880: {"IR", "ITC/DCI", ""}, 58224: {"IR", "TCI", ""}, 48159: {"IR", "TIC", ""}, 197207: {"IR", "MCI", ""},
	44244: {"IR", "Irancell", ""}, 57218: {"IR", "RighTel", ""}, 31549: {"IR", "Shatel", ""}, 43754: {"IR", "Asiatech", ""},
	16322: {"IR", "ParsOnline", ""}, 25184: {"IR", "Afranet", ""}, 42337: {"IR", "Respina", ""}, 6736: {"IR", "IPM", ""},
	49100: {"IR", "Pishgaman", ""}, 24631: {"IR", "FanapTelecom", ""}, 56402: {"IR", "HiWEB", ""}, 41881: {"IR", "Fanava", ""},
	50810: {"IR", "Mobinnet", ""}, 215708: {"IR", "MobinIDC", ""}, 34412: {"IR", "SabaHost", ""}, 198154: {"IR", "", ""},
	202468: {"IR", "ArvanCloud (cloud servers)", ""}, 43211: {"IR", "Digikala", ""}, 44932: {"IR", "Sabaidea (Aparat/Filimo)", ""},
	202798: {"IR", "Eitaa", ""}, 34947: {"IR", "Alibaba Travels", ""}, 49796: {"IR", "Shaparak", ""}, 62229: {"IR", "FarsNews", ""},
	// Iranian multi-tenant CDNs (like Cloudflare: any customer's name on the same edge)
	205585: {"IR", "ArvanCloud CDN", "ArvanCloud"}, 202319: {"IR", "Sotoon CDN", "Sotoon CDN"}, 60423: {"IR", "Derak Cloud", "Derak"},
	// Europe hosting
	24940: {"DE", "Hetzner", ""}, 213230: {"DE", "Hetzner Cloud", ""}, 212317: {"DE", "Hetzner", ""}, 16276: {"FR", "OVH", ""},
	51167: {"DE", "Contabo", ""}, 8560: {"DE", "IONOS", ""}, 197540: {"DE", "netcup", ""}, 3320: {"DE", "Deutsche Telekom", ""},
	// global CDNs
	13335: {"", "Cloudflare", "Cloudflare"}, 209242: {"", "Cloudflare", "Cloudflare"}, 54113: {"", "Fastly", "Fastly"},
	34309: {"", "Link11", "Link11"}, 20940: {"", "Akamai", "Akamai"}, 16625: {"", "Akamai", "Akamai"}, 60068: {"", "CDN77", "CDN77"}, 199524: {"", "Gcore", "Gcore"},
}

// geoNets is the offline prefix table (checked against Team Cymru from an
// Iran lab host, 2026-11-11). Order matters: the first match wins.
var geoNets = []struct {
	cidr string
	asn  int
}{
	// Iranian CDNs
	{"185.143.232.0/22", 205585}, {"185.166.104.0/22", 202319}, {"5.145.112.0/21", 60423},
	// Iran
	{"94.101.182.0/24", 202468}, {"37.32.0.0/19", 202468}, {"2.144.0.0/14", 44244}, {"92.42.48.0/21", 44244},
	{"5.106.0.0/16", 197207}, {"89.45.48.0/20", 197207}, {"217.218.0.0/16", 58224}, {"2.189.0.0/16", 48159},
	{"94.182.0.0/16", 31549}, {"85.15.0.0/18", 31549}, {"185.98.112.0/22", 43754}, {"37.156.144.0/22", 43754},
	{"185.120.220.0/22", 43754}, {"31.214.228.0/22", 16322}, {"86.104.32.0/20", 25184}, {"80.75.0.0/20", 25184},
	{"79.175.128.0/18", 25184}, {"5.160.0.0/16", 42337}, {"5.57.32.0/21", 198154}, {"185.188.104.0/22", 43211},
	{"185.147.176.0/22", 44932}, {"81.12.0.0/17", 215708}, {"185.226.140.0/22", 34412}, {"45.89.200.0/22", 34947},
	{"185.167.72.0/22", 49796}, {"45.157.244.0/22", 62229},
	// Hetzner
	{"5.9.0.0/16", 24940}, {"5.75.128.0/17", 24940}, {"37.27.0.0/16", 24940}, {"49.12.0.0/16", 24940}, {"49.13.0.0/16", 24940},
	{"65.21.0.0/16", 24940}, {"65.108.0.0/15", 24940}, {"78.46.0.0/15", 24940}, {"85.10.192.0/18", 24940}, {"88.198.0.0/16", 24940},
	{"88.99.0.0/16", 24940}, {"91.107.128.0/17", 24940}, {"94.130.0.0/16", 24940}, {"95.216.0.0/15", 24940}, {"116.202.0.0/15", 24940},
	{"128.140.0.0/17", 24940}, {"135.181.0.0/16", 24940}, {"136.243.0.0/16", 24940}, {"138.201.0.0/16", 24940}, {"142.132.128.0/17", 24940},
	{"144.76.0.0/16", 24940}, {"148.251.0.0/16", 24940}, {"157.90.0.0/16", 24940}, {"159.69.0.0/16", 24940}, {"162.55.0.0/16", 24940},
	{"167.233.0.0/16", 24940}, {"167.235.0.0/16", 24940}, {"168.119.0.0/16", 24940}, {"176.9.0.0/16", 24940}, {"178.63.0.0/16", 24940},
	{"188.40.0.0/16", 24940}, {"195.201.0.0/16", 24940}, {"213.133.96.0/19", 24940}, {"213.239.192.0/18", 24940},
	// OVH
	{"51.38.0.0/16", 16276}, {"51.68.0.0/16", 16276}, {"51.75.0.0/16", 16276}, {"51.77.0.0/16", 16276}, {"51.89.0.0/16", 16276},
	{"51.91.0.0/16", 16276}, {"54.36.0.0/16", 16276}, {"57.128.0.0/14", 16276}, {"91.121.0.0/16", 16276}, {"94.23.0.0/16", 16276},
	{"137.74.0.0/16", 16276}, {"145.239.0.0/16", 16276}, {"146.59.0.0/16", 16276}, {"178.32.0.0/15", 16276}, {"188.165.0.0/16", 16276},
}

func geoOffline(ip net.IP) (ipGeo, bool) {
	g := ipGeo{IP: ip.String()}
	if inNets(ip, cloudflareNets) {
		g.ASN, g.Org, g.CDN, g.Src = 13335, "Cloudflare", "Cloudflare", "table"
		return g, true
	}
	for _, n := range geoNets {
		if inNets(ip, []string{n.cidr}) {
			a := knownASNs[n.asn]
			g.ASN, g.Country, g.Org, g.CDN, g.Src = n.asn, a.cc, a.org, a.cdn, "table"
			return g, true
		}
	}
	return g, false
}

// geoOnline asks Team Cymru's DNS whois (nil error only with an answer).
// Tests replace it; --offline turns it off.
var geoOnline = func(ip net.IP, timeout time.Duration) (ipGeo, bool) {
	v4 := ip.To4()
	if v4 == nil {
		return ipGeo{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	q := fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", v4[3], v4[2], v4[1], v4[0])
	txt, err := net.DefaultResolver.LookupTXT(ctx, q)
	if err != nil || len(txt) == 0 {
		return ipGeo{}, false
	}
	g, ok := parseCymruOrigin(txt[0])
	if !ok {
		return ipGeo{}, false
	}
	g.IP = ip.String()
	if n, err := net.DefaultResolver.LookupTXT(ctx, fmt.Sprintf("AS%d.asn.cymru.com", g.ASN)); err == nil && len(n) > 0 {
		if f := strings.Split(n[0], "|"); len(f) >= 5 {
			g.Org = strings.TrimSpace(f[4])
		}
	}
	return g, true
}

// parseCymruOrigin: "202468 | 37.32.1.0/24 | IR | ripencc | 2012-01-02".
func parseCymruOrigin(s string) (ipGeo, bool) {
	f := strings.Split(s, "|")
	if len(f) < 3 {
		return ipGeo{}, false
	}
	asn, err := strconv.Atoi(strings.Fields(strings.TrimSpace(f[0]) + " ")[0])
	if err != nil {
		return ipGeo{}, false
	}
	return ipGeo{ASN: asn, Country: strings.TrimSpace(f[2]), Src: "cymru"}, true
}

var (
	geoMu    sync.Mutex
	geoCache = map[string]ipGeo{}
)

// geoOf: the offline table first, then (online) Cymru; a known ASN always
// sets the country / CDN (registry countries of Iranian ASNs can be wrong).
func geoOf(ip net.IP, online bool) ipGeo {
	geoMu.Lock()
	if g, ok := geoCache[ip.String()]; ok {
		geoMu.Unlock()
		return g
	}
	geoMu.Unlock()
	g, ok := geoOffline(ip)
	if !ok && online {
		if og, ok2 := geoOnline(ip, 1500*time.Millisecond); ok2 {
			g = og
			if a, known := knownASNs[g.ASN]; known {
				if a.cc != "" {
					g.Country = a.cc
				}
				g.CDN = a.cdn
				if a.org != "" {
					g.Org = a.org
				}
			}
		}
	}
	geoMu.Lock()
	geoCache[ip.String()] = g
	geoMu.Unlock()
	return g
}

// ---- the side verdict --------------------------------------------------------------

// Side match states.
const (
	sideMatch    = "match"
	sideMismatch = "mismatch"
	sideUnknown  = "unknown"
)

// Thresholds of the latency heuristic (TCP connect, this server = listener).
var (
	sideNearConnect = 10 * time.Millisecond // under: same city / region
	sideFarConnect  = 70 * time.Millisecond // over, from an Iran listener: most likely abroad
)

// applySide adds the listener-side verdict to r: the target's IPs' country /
// ASN against the listener's. A geo-certain mismatch rejects (BAD), a guess
// from latency only warns.
func applySide(r *targetCheck, s realitySide, tgeo, lgeo []ipGeo) {
	r.Geo = tgeo
	if !s.Known {
		r.SideMatch = sideUnknown
		return
	}
	r.Listener = "kharej"
	if s.ListenerIran {
		r.Listener = "iran"
	}
	for _, g := range tgeo {
		if g.CDN != "" && r.CDN == "" {
			r.CDN = g.CDN
		}
	}
	ir, foreign := 0, 0
	var where string
	for _, g := range tgeo {
		switch {
		case g.Country == "IR":
			ir++
		case g.Country != "":
			foreign++
			where = g.label()
		case g.ASN != 0: // a global CDN without a country
			foreign++
			where = g.label()
		}
	}
	if ir > 0 && where == "" {
		where = tgeo[0].label()
	}
	certain := ir+foreign > 0
	targetIran := ir > 0 && foreign == 0
	if !certain && s.ThisListens && r.ConnectMs > 0 {
		c := time.Duration(r.ConnectMs * float64(time.Millisecond))
		switch {
		case c < sideNearConnect:
			r.SideMatch = sideMatch
			r.Info = append(r.Info, fmt.Sprintf(tr("connect %.1f ms from the listener: same region (country unknown offline)", "اتصال %.1f ms از شنونده: همان منطقه (کشور به‌صورت آفلاین نامعلوم)"), r.ConnectMs))
		case s.ListenerIran && c > sideFarConnect:
			r.SideMatch = sideMismatch
			r.Warn = append(r.Warn, fmt.Sprintf(tr("MISMATCH (guess): connect %.0f ms from the Iran listener — the target is most likely hosted abroad", "ناهمخوانی (حدس): اتصال %.0f ms از شنوندهٔ ایران — هدف به احتمال زیاد خارج از ایران میزبانی می‌شود"), r.ConnectMs))
		default:
			r.SideMatch = sideUnknown
			r.Warn = append(r.Warn, tr("could not place the target's IP (offline table; online lookup off or unreachable): check its country by hand", "کشور IP هدف معلوم نشد (جدول آفلاین؛ جست‌وجوی برخط خاموش یا در دسترس نیست): کشورش را دستی بررسی کنید"))
		}
		return
	}
	if !certain {
		r.SideMatch = sideUnknown
		r.Warn = append(r.Warn, tr("could not place the target's IP (offline table; online lookup off or unreachable): check its country by hand", "کشور IP هدف معلوم نشد (جدول آفلاین؛ جست‌وجوی برخط خاموش یا در دسترس نیست): کشورش را دستی بررسی کنید"))
		return
	}
	switch {
	case s.ListenerIran && targetIran:
		r.SideMatch = sideMatch
		r.Info = append(r.Info, fmt.Sprintf(tr("side OK: Iranian site on Iranian IPs (%s), matches the Iran listener", "سمت درست: سایت ایرانی روی IP ایرانی (%s)، با شنوندهٔ ایران همخوان است"), where))
	case s.ListenerIran:
		r.SideMatch = sideMismatch
		r.Bad = append(r.Bad, fmt.Sprintf(tr("MISMATCH: the Iran server listens (reverse mode) but the target is hosted abroad (%s): an Iranian IP answering for a foreign name is a tell — pick an Iranian site", "ناهمخوانی: سرور ایران شنونده است (حالت معکوس) ولی هدف در خارج میزبانی می‌شود (%s): IP ایرانی که برای نام خارجی پاسخ می‌دهد لو می‌دهد — یک سایت ایرانی انتخاب کنید"), where))
	case targetIran || ir > 0:
		r.SideMatch = sideMismatch
		r.Bad = append(r.Bad, fmt.Sprintf(tr("MISMATCH: the kharej server listens (direct mode) but the target is hosted in Iran (%s): a foreign IP answering for an Iranian name is a tell, and the splice would cross the border — pick a foreign site", "ناهمخوانی: سرور خارج شنونده است (حالت مستقیم) ولی هدف در ایران میزبانی می‌شود (%s): IP خارجی که برای نام ایرانی پاسخ می‌دهد لو می‌دهد و اتصال به هدف از مرز می‌گذرد — یک سایت خارجی انتخاب کنید"), where))
	default:
		r.SideMatch = sideMatch
		r.Info = append(r.Info, fmt.Sprintf(tr("side OK: foreign site (%s), matches the kharej listener", "سمت درست: سایت خارجی (%s)، با شنوندهٔ خارج همخوان است"), where))
		same := false
		for _, l := range lgeo {
			for _, g := range tgeo {
				if l.ASN != 0 && l.ASN == g.ASN {
					same = true
				}
			}
		}
		if same {
			r.Info = append(r.Info, tr("same provider / ASN as the listener: best", "همان ارائه‌دهنده / ASN شنونده: بهترین"))
		} else if len(lgeo) > 0 && lgeo[0].ASN != 0 {
			r.Info = append(r.Info, fmt.Sprintf(tr("other provider than the listener (%s): fine, the same provider is better", "ارائه‌دهندهٔ دیگری جز شنونده (%s): قابل قبول، همان ارائه‌دهنده بهتر است"), lgeo[0].label()))
		}
	}
}

// geoOfHost resolves host (an IP literal as is) and places each address.
func geoOfHost(host string, online bool) []ipGeo {
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ips = targetResolve(host)
	}
	var out []ipGeo
	seen := map[string]bool{}
	for _, ip := range ips {
		if ip.To4() == nil || seen[ip.String()] || len(out) >= 4 {
			continue
		}
		seen[ip.String()] = true
		out = append(out, geoOf(ip, online))
	}
	return out
}

func geoOfIPs(ips []string, online bool) []ipGeo {
	var out []ipGeo
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			out = append(out, geoOf(ip, online))
		}
	}
	return out
}

// checkSide is probeTarget + the side verdict.
func checkSide(target, sni string, s realitySide, lgeo []ipGeo, online bool) targetCheck {
	r := probeTarget(target, sni, nil)
	host := r.Target
	if h, _, err := net.SplitHostPort(r.Target); err == nil {
		host = h
	}
	tgeo := geoOfHost(host, online)
	if len(tgeo) > 0 && len(r.Bad) == 0 || r.HandshakeMs > 0 {
		_, port, _ := net.SplitHostPort(r.Target)
		if len(tgeo) > 0 {
			if d := targetRTT(net.JoinHostPort(tgeo[0].IP, port)); d > 0 {
				r.ConnectMs = float64(d.Microseconds()) / 1000
			}
		}
	}
	applySide(&r, s, tgeo, lgeo)
	return finishCheck(r)
}

// targetRTT: the best of two TCP connects to an address (no DNS in the
// time), 0 on failure. Tests replace it.
var targetRTT = func(addr string) time.Duration {
	var best time.Duration
	for i := 0; i < 2; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return best
		}
		d := time.Since(t0)
		c.Close()
		if best == 0 || d < best {
			best = d
		}
	}
	return best
}

// ---- vetted candidates (lab 2, Iran, 2026-11-11; see docs §9.1) -------------------

// realityCandidatesIran: Iranian single-operator sites on Iranian IPs with
// TLS 1.3 + X25519 + h2, not on ArvanCloud / Sotoon / Derak CDN.
var realityCandidatesIran = []string{
	"www.digikala.com", "www.aparat.com", "www.filimo.com", "torob.com", "myket.ir",
	"eitaa.com", "web.bale.ai", "www.mehrnews.com", "www.khabaronline.ir", "www.hamshahrionline.ir",
	"www.parsonline.com", "www.digikalajet.com", "www.jabama.com", "www.sharif.edu", "www.yektanet.com",
}

// realityCandidatesForeign: foreign single-operator sites, Hetzner /
// Germany first (a kharej listener on Hetzner wants them).
var realityCandidatesForeign = []string{
	"www.hetzner.com", "docs.hetzner.com", "accounts.hetzner.com", "nextcloud.com", "www.ovhcloud.com",
	"www.heise.de", "www.check24.de", "www.gmx.net", "web.de", "www.wikipedia.org", "www.debian.org", "github.com",
}

// candidatesFor: the side's list (both when the side is unknown).
func candidatesFor(s realitySide) []string {
	if !s.Known {
		return append(append([]string{}, realityCandidatesIran...), realityCandidatesForeign...)
	}
	if s.ListenerIran {
		return append([]string{}, realityCandidatesIran...)
	}
	return append([]string{}, realityCandidatesForeign...)
}

// flagGiven: the flag was set on the command line.
func flagGiven(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func anyAccepted(rs []targetCheck) bool {
	for _, r := range rs {
		if r.OK {
			return true
		}
	}
	return false
}

// noneAcceptedAdvice: what to do when no candidate of the side passes.
func noneAcceptedAdvice(s realitySide) string {
	if s.ListenerIran {
		return tr("No Iranian candidate passed from here. Options, best first: (1) a real Iranian site of your own on this server (nginx with TLS 1.3 + h2 and a real certificate) as the target — the name then matches the IP exactly; (2) a large single-operator Iranian site you check with --add host; (3) keep the front 'tls' (or 'both') until one passes. Never use a site on ArvanCloud / Sotoon / Derak CDN.",
			"هیچ نامزد ایرانی از این‌جا پذیرفته نشد. گزینه‌ها به ترتیب: (۱) یک سایت واقعی ایرانی از خودتان روی همین سرور (nginx با TLS 1.3 و h2 و گواهی واقعی) به‌عنوان هدف — نام دقیقاً با IP می‌خواند؛ (۲) یک سایت بزرگ ایرانی تک‌اپراتوره که با --add host بررسی می‌کنید؛ (۳) تا وقتی هدفی پذیرفته نشده front را 'tls' (یا 'both') نگه دارید. هرگز سایتی روی CDN ابرآروان / سطون / درک انتخاب نکنید.")
	}
	return tr("No foreign candidate passed from here. Options: a site of your own on this server (TLS 1.3 + h2), a single-operator site on the same provider (check with --add host), or keep the front 'tls' / 'both'.",
		"هیچ نامزد خارجی از این‌جا پذیرفته نشد. گزینه‌ها: سایتی از خودتان روی همین سرور (TLS 1.3 و h2)، یک سایت تک‌اپراتوره روی همان ارائه‌دهنده (با --add host بررسی کنید)، یا front را 'tls' / 'both' نگه دارید.")
}

// frontTargetGeo places a configured target for the doctor (tests replace it).
var frontTargetGeo = func(host string) []ipGeo { return geoOfHost(host, true) }

// frontSideFinding is the doctor's side check of one configured target:
// WARN when its country does not match the listener's side.
func frontSideFinding(fc fileConfig, target, area string) []finding {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return nil
	}
	s := sideOfConfig(fc)
	var r targetCheck
	applySide(&r, s, frontTargetGeo(host), nil)
	switch r.SideMatch {
	case sideMismatch:
		why := strings.Join(append(r.Bad, r.Warn...), "; ")
		return []finding{{Sev: "WARN", Area: area, What: fmt.Sprintf(tr("reality target %s does not match the listener side", "هدف Reality %s با سمت شنونده همخوان نیست"), target),
			Why: why,
			Do:  tr("pick a target of the listener's side: hs2 reality targets -u U; then hs2 reality front -u U --ip IP reality --target NEW", "هدفی از سمت شنونده انتخاب کنید: hs2 reality targets -u U؛ سپس hs2 reality front -u U --ip IP reality --target NEW")}}
	case sideMatch:
		return []finding{{Sev: "OK", Area: area, What: fmt.Sprintf(tr("reality target %s matches the listener side (%s)", "هدف Reality %s با سمت شنونده (%s) همخوان است"), target, r.Listener)}}
	}
	return nil
}
