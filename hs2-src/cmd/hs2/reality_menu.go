package main

// Menu M5 (= Reality phase 5): the "Security & camouflage" section.
//
//	hs2 reality front -u U --ip IP tls|reality|both [--target host:443] [--sni name] [--new-key]
//	hs2 reality rotate-key -u U (--ip IP | --all)
//	hs2 reality targets [--add host,host] [--json]
//	hs2 reality check-target host[:443] [server_name] [--json]
//	hs2 reality status -u U [--json]
//	hs2 reality keygen
//
// A front lives in the LISTENING side's lb list (lb.<side>[i].front); the
// dialing side learns kind, server names, public key and short ids from the
// announcement over the control channel, so a front change touches one
// server only. The change goes through the safe apply (preview diff, backup,
// confirm timer, automatic rollback) with a RELOAD (SIGHUP) instead of a
// restart: established links keep running, new links use the new front.

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// ---- target check ----------------------------------------------------------------

// targetCheck is the verdict on one candidate reality target.
type targetCheck struct {
	Target      string   `json:"target"`
	SNI         string   `json:"server_name"`
	OK          bool     `json:"ok"`
	TLS13       bool     `json:"tls13"`
	X25519      bool     `json:"x25519"`
	H2          bool     `json:"h2"`
	CertCovers  bool     `json:"cert_covers"`
	CertBytes   int      `json:"cert_bytes"`
	HandshakeMs float64  `json:"handshake_ms"`
	CDN         string   `json:"cdn,omitempty"`
	Issuer      string   `json:"issuer,omitempty"`
	Bad         []string `json:"bad,omitempty"`  // reasons to reject
	Warn        []string `json:"warn,omitempty"` // worth knowing
	Info        []string `json:"info,omitempty"`
}

// Thresholds of the target check.
var (
	targetMaxHandshake  = 1500 * time.Millisecond // slower: BAD (every probe sees our listener stall)
	targetWarnHandshake = 400 * time.Millisecond
	targetWarnCertBytes = 8192 // a chain this big is unusual for a single site
	targetMaxSANs       = 60   // more SANs than this: a shared (multi-tenant) certificate
)

// cdnHeader names a multi-tenant CDN from one HTTP response header.
func cdnHeader(name, value string) string {
	n, v := strings.ToLower(name), strings.ToLower(value)
	switch {
	case n == "cf-ray" || (n == "server" && strings.Contains(v, "cloudflare")):
		return "Cloudflare"
	case n == "x-amz-cf-id" || n == "x-amz-cf-pop" || (n == "via" && strings.Contains(v, "cloudfront")) || (n == "x-cache" && strings.Contains(v, "cloudfront")):
		return "CloudFront"
	case n == "x-served-by" && strings.Contains(v, "cache-"), n == "x-fastly-request-id", n == "fastly-debug-digest", n == "via" && strings.Contains(v, "varnish"):
		return "Fastly"
	case n == "x-akamai-transformed" || n == "akamai-grn" || (n == "server" && strings.Contains(v, "akamai")):
		return "Akamai"
	case n == "x-azure-ref" || n == "x-msedge-ref":
		return "Azure Front Door"
	case n == "server" && (v == "gws" || strings.Contains(v, "google frontend") || v == "esf"):
		return "Google"
	case n == "x-cdn" && v != "":
		return value
	case n == "server" && (strings.Contains(v, "arvancloud") || strings.Contains(v, "derakcloud")):
		return value
	}
	return ""
}

// inNets: ip in one of the CIDRs.
func inNets(ip net.IP, nets []string) bool {
	for _, c := range nets {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// targetDialer is the TCP dial of the target check (tests replace it).
var targetDialer = func(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 6 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// targetResolve resolves the target host (tests replace it).
var targetResolve = func(host string) []net.IP {
	ips, _ := net.LookupIP(host)
	return ips
}

// probeTarget checks one candidate: TLS 1.3 with X25519 offered alone, ALPN
// h2, a certificate that covers the name, the chain size, the handshake time,
// and that it is not a multi-tenant CDN (by name, Cloudflare address ranges,
// a shared certificate, or the CDN's response headers).
func probeTarget(target, sni string, roots *x509.CertPool) targetCheck {
	if !strings.Contains(target, ":") {
		target += ":443"
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return targetCheck{Target: target, SNI: sni, Bad: []string{"want host:port"}}
	}
	if sni == "" {
		sni = host
	}
	r := targetCheck{Target: target, SNI: sni}
	if net.ParseIP(sni) != nil {
		r.Bad = append(r.Bad, tr("server name is an IP literal: browsers never send that as SNI", "نام سرور یک IP است: مرورگر هرگز IP را به‌عنوان SNI نمی‌فرستد"))
	}
	if cdn := multiTenantCDN(sni); cdn != "" {
		r.CDN = cdn
	}
	if net.ParseIP(host) == nil {
		for _, ip := range targetResolve(host) {
			if inNets(ip, cloudflareNets) {
				r.CDN = "Cloudflare"
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	t0 := time.Now()
	raw, err := targetDialer(ctx, target)
	if err != nil {
		r.Bad = append(r.Bad, fmt.Sprintf(tr("unreachable: %v", "در دسترس نیست: %v"), err))
		return finishCheck(r)
	}
	c := tls.Client(raw, &tls.Config{ServerName: sni, NextProtos: []string{"h2", "http/1.1"},
		CurvePreferences: []tls.CurveID{tls.X25519}, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
	c.SetDeadline(time.Now().Add(8 * time.Second))
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		r.Bad = append(r.Bad, fmt.Sprintf(tr("TLS handshake with X25519 alone failed: %v", "دست‌دهی TLS فقط با X25519 شکست خورد: %v"), err))
		return finishCheck(r)
	}
	hs := time.Since(t0)
	r.HandshakeMs = float64(hs.Microseconds()) / 1000
	cs := c.ConnectionState()
	c.Close()
	r.X25519 = true // offered alone and the handshake completed
	r.TLS13 = cs.Version == tls.VersionTLS13
	r.H2 = cs.NegotiatedProtocol == "h2"
	if !r.TLS13 {
		r.Bad = append(r.Bad, fmt.Sprintf(tr("TLS version 0x%x, reality needs TLS 1.3", "نسخهٔ TLS 0x%x است، Reality به TLS 1.3 نیاز دارد"), cs.Version))
	}
	if !r.H2 {
		r.Bad = append(r.Bad, fmt.Sprintf(tr("ALPN %q, not h2: a Chrome ClientHello offers h2 and the real site would answer it", "ALPN %q است نه h2: ClientHello کروم h2 پیشنهاد می‌دهد و سایت واقعی باید آن را بپذیرد"), cs.NegotiatedProtocol))
	}
	if len(cs.PeerCertificates) > 0 {
		crt := cs.PeerCertificates[0]
		r.Issuer = crt.Issuer.CommonName
		for _, x := range cs.PeerCertificates {
			r.CertBytes += len(x.Raw)
		}
		r.CertCovers = crt.VerifyHostname(sni) == nil
		if !r.CertCovers {
			r.Bad = append(r.Bad, fmt.Sprintf(tr("certificate does not cover %s", "گواهی %s را پوشش نمی‌دهد"), sni))
		}
		if len(crt.DNSNames) > targetMaxSANs {
			r.CDN = firstNonEmpty(r.CDN, fmt.Sprintf(tr("shared certificate (%d names)", "گواهی مشترک (%d نام)"), len(crt.DNSNames)))
		}
		for _, n := range crt.DNSNames {
			if strings.Contains(n, "cloudflaressl") || strings.HasSuffix(n, ".cloudflare.com") && !strings.HasSuffix(sni, "cloudflare.com") {
				r.CDN = "Cloudflare"
			}
		}
		inter := x509.NewCertPool()
		for _, x := range cs.PeerCertificates[1:] {
			inter.AddCert(x)
		}
		if _, err := crt.Verify(x509.VerifyOptions{DNSName: sni, Intermediates: inter, Roots: roots}); err != nil {
			r.Warn = append(r.Warn, tr("chain does not verify against the system roots", "زنجیره با ریشه‌های سیستم تأیید نمی‌شود"))
		}
		if r.CertBytes > targetWarnCertBytes {
			r.Warn = append(r.Warn, fmt.Sprintf(tr("large certificate chain (%d bytes)", "زنجیرهٔ گواهی بزرگ (%d بایت)"), r.CertBytes))
		}
	}
	switch {
	case hs > targetMaxHandshake:
		r.Bad = append(r.Bad, fmt.Sprintf(tr("handshake %v: too slow from this server", "دست‌دهی %v: از این سرور خیلی کند است"), hs.Round(time.Millisecond)))
	case hs > targetWarnHandshake:
		r.Warn = append(r.Warn, fmt.Sprintf(tr("handshake %v: slowish", "دست‌دهی %v: کمی کند"), hs.Round(time.Millisecond)))
	}
	// The response headers (an http/1.1 request on a second connection).
	if r.CDN == "" {
		if h := targetHeaders(ctx, target, sni); h != nil {
			for k, v := range h {
				if cdn := cdnHeader(k, v); cdn != "" {
					r.CDN = cdn
				}
				if strings.EqualFold(k, "location") {
					r.Info = append(r.Info, "redirect "+v)
				}
			}
		}
	}
	return finishCheck(r)
}

func finishCheck(r targetCheck) targetCheck {
	if r.CDN != "" {
		r.Bad = append(r.Bad, fmt.Sprintf(tr("on %s, a multi-tenant CDN: any stranger can reach every site of it through our listener, and the CDN's own fingerprint differs", "روی %s است، یک CDN چندمستأجره: هر غریبه‌ای از راه شنونده به همهٔ سایت‌های آن می‌رسد و اثر انگشت CDN فرق دارد"), r.CDN))
	}
	r.OK = len(r.Bad) == 0
	return r
}

// targetHeaders: the response headers of HEAD / over http/1.1 (nil on error).
func targetHeaders(ctx context.Context, target, sni string) map[string]string {
	raw, err := targetDialer(ctx, target)
	if err != nil {
		return nil
	}
	defer raw.Close()
	c := tls.Client(raw, &tls.Config{ServerName: sni, NextProtos: []string{"http/1.1"}, InsecureSkipVerify: true})
	c.SetDeadline(time.Now().Add(6 * time.Second))
	if c.HandshakeContext(ctx) != nil {
		return nil
	}
	fmt.Fprintf(c, "HEAD / HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36\r\nAccept: */*\r\nConnection: close\r\n\r\n", sni)
	b, _ := io.ReadAll(io.LimitReader(c, 16384))
	h := map[string]string{}
	for i, l := range strings.Split(string(b), "\r\n") {
		if i == 0 {
			continue
		}
		if l == "" {
			break
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return h
}

func printTargetCheck(w io.Writer, r targetCheck) {
	yn := func(b bool) string {
		if b {
			return "OK  "
		}
		return "BAD "
	}
	fmt.Fprintf(w, "%s %s  (sni %s)\n", tr("target", "هدف"), r.Target, r.SNI)
	if r.HandshakeMs > 0 {
		fmt.Fprintf(w, "  %s TLS 1.3\n  %s X25519\n  %s ALPN h2\n  %s %s\n", yn(r.TLS13), yn(r.X25519), yn(r.H2), yn(r.CertCovers), tr("certificate covers the name", "گواهی نام را پوشش می‌دهد"))
		fmt.Fprintf(w, "  INFO %s %d B, %s %q, %s %.0f ms\n", tr("chain", "زنجیره"), r.CertBytes, tr("issuer", "صادرکننده"), r.Issuer, tr("handshake", "دست‌دهی"), r.HandshakeMs)
	}
	for _, s := range r.Info {
		fmt.Fprintln(w, "  INFO "+s)
	}
	for _, s := range r.Warn {
		fmt.Fprintln(w, "  WARN "+s)
	}
	for _, s := range r.Bad {
		fmt.Fprintln(w, "  BAD  "+s)
	}
	if r.OK {
		fmt.Fprintln(w, tr("VERDICT: ACCEPT — usable as a reality target", "نتیجه: پذیرفته — برای هدف Reality مناسب است"))
	} else {
		fmt.Fprintln(w, tr("VERDICT: REJECT — reasons above", "نتیجه: رد — دلیل‌ها در بالا"))
	}
}

// realityCandidates are the built-in candidate targets of the picker: large,
// single-operator sites with TLS 1.3 + h2. The check decides; the list only
// saves typing (hs2 reality targets --add …).
var realityCandidates = []string{
	"www.lovelive-anime.jp", "addons.mozilla.org", "www.mozilla.org", "github.com",
	"www.apple.com", "www.microsoft.com", "www.amazon.com", "www.yahoo.com",
	"www.digikala.com", "www.aparat.com", "www.varzesh3.com",
}

func checkTargets(list []string) []targetCheck {
	out := make([]targetCheck, len(list))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, t := range list {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h := t
			if hh, _, err := net.SplitHostPort(t); err == nil {
				h = hh
			}
			out[i] = probeTarget(t, h, nil)
		}(i, t)
	}
	wg.Wait()
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].OK != out[b].OK {
			return out[a].OK
		}
		return out[a].HandshakeMs < out[b].HandshakeMs
	})
	return out
}

func renderTargets(w io.Writer, rs []targetCheck) {
	fmt.Fprintf(w, "%-3s %-26s %-7s %-8s %s\n", "#", tr("target", "هدف"), tr("verdict", "نتیجه"), "ms", tr("why", "چرا"))
	for i, r := range rs {
		v := "ACCEPT"
		why := strings.Join(r.Warn, "; ")
		if !r.OK {
			v = "REJECT"
			why = strings.Join(r.Bad, "; ")
		}
		fmt.Fprintf(w, "%-3d %-26s %-7s %-8.0f %s\n", i+1, clip(r.Target, 26), v, r.HandshakeMs, clip(why, 90))
	}
}

// ---- config editing ----------------------------------------------------------------

// ownLBEntry finds this side's lb entry of ip in the raw config map.
func ownLBEntry(m map[string]any, side, ip string) (map[string]any, error) {
	lb, _ := m["lb"].(map[string]any)
	if lb == nil {
		return nil, errors.New(tr("lb is off: add the IP first (hs2 lb add-ip -u U --ip IP; the primary IP alone works too)", "lb خاموش است: اول IP را اضافه کنید (hs2 lb add-ip -u U --ip IP؛ تنها IP اصلی هم کافی است)"))
	}
	l, _ := lb[side].([]any)
	want := net.ParseIP(ip)
	for _, x := range l {
		if e, ok := x.(map[string]any); ok {
			if s, _ := e["ip"].(string); want != nil && net.ParseIP(s).Equal(want) {
				return e, nil
			}
		}
	}
	return nil, fmt.Errorf(tr("%s is not in this side's lb list (hs2 lb add-ip -u U --ip %s)", "%s در فهرست lb این سمت نیست (hs2 lb add-ip -u U --ip %s)"), ip, ip)
}

func newShortID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// frontSpec is the requested change of one IP's front.
type frontSpec struct {
	Kind, Target, SNI string
	NewKey            bool
	ForceTarget       bool
}

// applyFrontSpec rewrites the entry's "front": tls removes it (the default);
// reality / both keep what is there (key, short ids, limits) unless asked.
func applyFrontSpec(e map[string]any, s frontSpec) error {
	if s.Kind == tlscarrier.FrontTLS {
		delete(e, "front")
		return nil
	}
	f, _ := e["front"].(map[string]any)
	if f == nil {
		f = map[string]any{}
	}
	f["kind"] = s.Kind
	if s.Target != "" {
		t := s.Target
		if !strings.Contains(t, ":") {
			t += ":443"
		}
		f["target"] = t
		if s.SNI == "" { // a new target: its own name unless given
			h, _, _ := net.SplitHostPort(t)
			f["server_names"] = []any{h}
		}
	}
	if s.SNI != "" {
		f["server_names"] = []any{s.SNI}
	}
	if t, _ := f["target"].(string); t == "" {
		return errors.New(tr("reality / both need --target host:443 (pick one: hs2 reality targets)", "reality / both به --target host:443 نیاز دارد (انتخاب: hs2 reality targets)"))
	}
	if pk, _ := f["private_key"].(string); pk == "" || s.NewKey {
		priv, _, err := tlscarrier.RealityKeygen()
		if err != nil {
			return err
		}
		f["private_key"] = priv
		delete(f, "public_key")
	}
	if sid, _ := f["short_ids"].([]any); len(sid) == 0 || s.NewKey {
		f["short_ids"] = []any{newShortID()}
	}
	if s.ForceTarget {
		f["force_target"] = true
	}
	e["front"] = f
	return nil
}

// keepPrivateKeys copies every lb front private_key of old into nb (the lb
// list round-trips through lbIP, whose FrontProfile never marshals the key).
func keepPrivateKeys(oldRaw []byte, nb []byte) []byte {
	om, err := loadConfigMap(oldRaw)
	if err != nil {
		return nb
	}
	keys := map[string]string{}
	if lb, ok := om["lb"].(map[string]any); ok {
		for _, side := range []string{"iran", "kharej"} {
			l, _ := lb[side].([]any)
			for _, x := range l {
				e, _ := x.(map[string]any)
				f, _ := e["front"].(map[string]any)
				ip, _ := e["ip"].(string)
				if pk, _ := f["private_key"].(string); pk != "" && net.ParseIP(ip) != nil {
					keys[side+"|"+net.ParseIP(ip).String()] = pk
				}
			}
		}
	}
	if len(keys) == 0 {
		return nb
	}
	nm, err := loadConfigMap(nb)
	if err != nil {
		return nb
	}
	lb, ok := nm["lb"].(map[string]any)
	if !ok {
		return nb
	}
	for _, side := range []string{"iran", "kharej"} {
		l, _ := lb[side].([]any)
		for _, x := range l {
			e, _ := x.(map[string]any)
			f, _ := e["front"].(map[string]any)
			ip, _ := e["ip"].(string)
			if f == nil || net.ParseIP(ip) == nil {
				continue
			}
			if pk := keys[side+"|"+net.ParseIP(ip).String()]; pk != "" {
				if cur, _ := f["private_key"].(string); cur == "" {
					f["private_key"] = pk
				}
			}
		}
	}
	out, err := marshalConfig(nm)
	if err != nil {
		return nb
	}
	return out
}

// ---- commands ----------------------------------------------------------------------

func realityFrontCmd(sub string, args []string) int {
	fs := flag.NewFlagSet("reality "+sub, flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	ip := fs.String("ip", "", "this server's lb IP")
	all := fs.Bool("all", false, "rotate-key: every reality/both IP")
	target := fs.String("target", "", "reality/both: the real site host:443")
	sni := fs.String("sni", "", "reality/both: server name (default: the target's host)")
	newKey := fs.Bool("new-key", false, "generate a new key pair and short id")
	force := fs.Bool("force-target", false, "accept a target the check rejects (not recommended)")
	skip := fs.Bool("skip-check", false, "do not check the target (offline)")
	within := fs.Duration("confirm-within", 5*time.Minute, "trial time before the automatic rollback")
	yes := fs.Bool("yes", false, "apply without asking")
	dry := fs.Bool("dry-run", false, "show the change only")
	lang := langFlag(fs)
	pos := parseInterleaved(fs, args)
	setLang(*lang)
	if !*dry {
		rel, err := acquireLock(os.Getpid(), false, time.Now())
		if err != nil {
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
		defer rel()
	}
	path, fc, cur := loadUnit(*unit)
	switch fc.Carrier {
	case "mtcp", "l3mtcp", "l3":
	default:
		fmt.Printf(tr("ERROR: carrier %s has no per-IP fronts (mtcp / l3mtcp only)\n", "خطا: حامل %s front جداگانه برای هر IP ندارد (فقط mtcp / l3mtcp)\n"), fc.Carrier)
		return 1
	}
	own, peers := lbSide(fc)
	if !tunnelListens(fc) {
		other := "<IP>"
		if len(peers) > 0 {
			other = peers[0]
		}
		fmt.Println(tr("This server DIALS the links: fronts are chosen on the LISTENING server (the other one) and announced to this side automatically.",
			"این سرور لینک‌ها را شماره‌گیری می‌کند: front روی سرورِ شنونده (سرور مقابل) انتخاب و خودکار به این سمت اعلام می‌شود."))
		fmt.Printf(tr("   on the other server run: hs2 reality %s -u %s --ip %s %s\n", "   روی سرور مقابل اجرا کنید: hs2 reality %s -u %s --ip %s %s\n"), sub, cleanUnit(*unit), other, strings.Join(pos, " "))
		return 1
	}
	m, err := loadConfigMap(cur)
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	var reason string
	switch sub {
	case "front":
		if len(pos) != 1 || (pos[0] != "tls" && pos[0] != "reality" && pos[0] != "both") || *ip == "" {
			fmt.Println("usage: hs2 reality front -u U --ip IP tls|reality|both [--target host:443] [--sni name] [--new-key]")
			return 2
		}
		e, err := ownLBEntry(m, own, *ip)
		if err != nil {
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
		spec := frontSpec{Kind: pos[0], Target: *target, SNI: *sni, NewKey: *newKey, ForceTarget: *force}
		if err := applyFrontSpec(e, spec); err != nil {
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
		if spec.Kind != tlscarrier.FrontTLS && !*skip {
			f := e["front"].(map[string]any)
			t, _ := f["target"].(string)
			sn := ""
			if l, _ := f["server_names"].([]any); len(l) > 0 {
				sn, _ = l[0].(string)
			}
			fmt.Printf(tr("checking the target %s ...\n", "بررسی هدف %s ...\n"), t)
			r := probeTarget(t, sn, nil)
			printTargetCheck(os.Stdout, r)
			if !r.OK && !*force {
				fmt.Println(tr("ERROR: the target is rejected — pick another (hs2 reality targets) or add --force-target", "خطا: هدف رد شد — هدف دیگری انتخاب کنید (hs2 reality targets) یا --force-target بدهید"))
				return 1
			}
		}
		if spec.Kind == tlscarrier.FrontReality {
			if ls, ok := readLive(path); !ok || frontKindOf(ls, *ip) != tlscarrier.FrontBoth {
				fmt.Println(tr("WARN: a 'reality' IP refuses every dialer that has not learned its key yet (after a restart the other server dials before it hears the announcement). Safer order: tls -> both -> (check `hs2 reality status` on the other server) -> reality.",
					"هشدار: IP با 'reality' هر شماره‌گیری را که هنوز کلیدش را نگرفته رد می‌کند (پس از ری‌استارت سرور مقابل پیش از شنیدن اعلام شماره می‌گیرد). ترتیب امن‌تر: tls -> both -> (بررسی `hs2 reality status` روی سرور مقابل) -> reality."))
			}
		}
		reason = "front-" + spec.Kind
	case "rotate-key":
		lb, _ := m["lb"].(map[string]any)
		l, _ := lb[own].([]any)
		n := 0
		for _, x := range l {
			e, _ := x.(map[string]any)
			f, _ := e["front"].(map[string]any)
			eip, _ := e["ip"].(string)
			k, _ := f["kind"].(string)
			if f == nil || (k != "reality" && k != "both") {
				continue
			}
			if !*all && !net.ParseIP(eip).Equal(net.ParseIP(*ip)) {
				continue
			}
			if err := applyFrontSpec(e, frontSpec{Kind: k, NewKey: true}); err != nil {
				fmt.Println("ERROR: " + err.Error())
				return 1
			}
			n++
		}
		if n == 0 {
			fmt.Println(tr("ERROR: no reality/both front to rotate (give --ip IP of a reality/both IP, or --all)", "خطا: front از نوع reality/both برای چرخش کلید نیست (--ip یک IP با reality/both یا --all بدهید)"))
			return 1
		}
		fmt.Printf(tr("%d key(s) rotated: the other server receives the new public key with the next announcement (right after the reload); a 'both' IP bridges any gap over tls, a pure 'reality' IP may refuse new links for a few seconds.\n",
			"%d کلید چرخید: سرور مقابل کلید عمومی تازه را با اعلام بعدی (بلافاصله پس از reload) می‌گیرد؛ IP با 'both' فاصله را با tls پر می‌کند، IP فقط 'reality' ممکن است چند ثانیه لینک تازه نپذیرد.\n"), n)
		reason = "front-rotate-key"
	}
	nb, err := marshalConfig(m)
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	if sub == "front" && pos[0] != tlscarrier.FrontTLS {
		var nfc fileConfig
		if err := json.Unmarshal(nb, &nfc); err != nil {
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
		if msg := frontPreflight(nfc, pos[0], path); msg != "" {
			fmt.Println("ERROR: " + msg)
			return 1
		}
	}
	rc := safeApplyMode(*unit, path, cur, nb, reason, *within, *yes, *dry, lbOn(fc))
	if rc == 0 && !*dry {
		fmt.Println(tr("Bilateral: nothing to run on the other server — it learns the front from the announcement (hs2 reality status -u U shows which front each IP really uses).",
			"دوطرفه: روی سرور مقابل کاری لازم نیست — front را از اعلام می‌گیرد (hs2 reality status -u U نشان می‌دهد هر IP واقعاً از کدام front استفاده می‌کند)."))
	}
	return rc
}

// frontPreflight refuses a reality / both front the other server could
// never use: "" = fine.
//   - the announcement (which carries the front, its public key and short
//     ids to the dialer) is empty while this side listens on a wildcard
//     address: the dialer would keep dialing tls, and a "reality" IP would
//     then refuse every new link;
//   - "both" answers tls only for the tunnel's own server names (sni): a
//     tunnel without one would send every tls dialer to the cover target.
func frontPreflight(fc fileConfig, kind, cfgPath string) string {
	if lbOwnAdvert(fc) == nil {
		h, p, _ := net.SplitHostPort(fc.Addr)
		return fmt.Sprintf(tr("this server listens on the wildcard %s: the announcement that carries the front to the other server needs an exact address. First set "addr" in %s to <this IP>:%s (menu 2) Tunnels -> edit, or the config editor; a restart), then retry. (%s)",
			"این سرور روی نشانی عمومی %s گوش می‌دهد: اعلامی که front را به سرور مقابل می‌برد نشانی دقیق می‌خواهد. اول "addr" را در %s به <IP همین سرور>:%s تغییر دهید (منوی ۲) تونل‌ها -> ویرایش، یا ویرایشگر پیکربندی؛ با ری‌استارت)، سپس دوباره. (%s)"),
			firstNonEmpty(h, "*")+":"+p, cfgPath, p, fc.Addr)
	}
	if kind == tlscarrier.FrontBoth && len(tlsNames(fc)) == 0 {
		return tr("'both' answers tls only for the tunnel's server name (sni), and this tunnel has none (no domain): tls dialers would be sent to the cover target. Set the same sni on both servers first, or use 'reality' once the other server shows it in `hs2 reality status`.",
			"'both' فقط برای نام سرور (sni) تونل tls پاسخ می‌دهد و این تونل sni ندارد (بدون دامنه): شماره‌گیرهای tls به هدف پوششی فرستاده می‌شوند. اول sni یکسان روی هر دو سرور بگذارید، یا وقتی سرور مقابل در `hs2 reality status` آن را نشان داد از 'reality' استفاده کنید.")
	}
	return ""
}

// frontKindOf: the front ip serves now per the status file ("" = unknown).
func frontKindOf(ls liveStatus, ip string) string {
	p := net.ParseIP(ip)
	for _, f := range ls.Fronts {
		if net.ParseIP(f.IP).Equal(p) {
			return f.Kind
		}
	}
	return ""
}

// frontState is `hs2 reality status`: per IP (listening side) or per path
// (dialing side) the configured and the effective front, counters and the
// recent fallback events.
type frontState struct {
	Unit      string                   `json:"unit"`
	Role      string                   `json:"role"` // listen | dial
	Running   bool                     `json:"running"`
	IPs       []frontIPRow             `json:"ips,omitempty"`
	Paths     []frontPathStatus        `json:"paths,omitempty"`
	Reality   *tlscarrier.RealityStats `json:"reality,omitempty"`
	Fallbacks []string                 `json:"fallback_events,omitempty"`
}

type frontIPRow struct {
	IP         string                        `json:"ip"`
	Configured string                        `json:"configured"`
	InUse      string                        `json:"in_use"`
	Target     string                        `json:"target,omitempty"`
	ServerName string                        `json:"server_name,omitempty"`
	PublicKey  string                        `json:"public_key,omitempty"`
	Counters   *tlscarrier.FrontCountersSnap `json:"counters,omitempty"`
}

func buildFrontState(unit string, fc fileConfig, ls liveStatus, running bool, journal string) frontState {
	st := frontState{Unit: cleanUnit(unit), Running: running, Reality: ls.Reality, Paths: ls.FrontPaths}
	if tunnelListens(fc) {
		st.Role = "listen"
		live := map[string]frontIPStatus{}
		for _, f := range ls.Fronts {
			live[f.IP] = f
		}
		for _, e := range lbLocalList(lbCur(fc)) {
			ip := net.ParseIP(e.IP)
			if ip == nil {
				continue
			}
			row := frontIPRow{IP: ip.String(), Configured: tlscarrier.FrontTLS, InUse: "-"}
			f := e.Front
			if f.IsDefault() {
				f = tunnelFront(fc)
			}
			if !f.IsDefault() {
				row.Configured = f.NormKind()
				row.Target = f.Target
				if len(f.ServerNames) > 0 {
					row.ServerName = f.ServerNames[0]
				}
				if f.PrivateKey != "" {
					row.PublicKey, _ = tlscarrier.RealityPublicOf(f.PrivateKey)
				}
			}
			if l, ok := live[row.IP]; ok && running {
				row.InUse, row.Counters = l.Kind, l.Counters
			} else if running && len(ls.Fronts) == 0 {
				row.InUse = tunnelFront(fc).NormKind()
			}
			st.IPs = append(st.IPs, row)
		}
	} else {
		st.Role = "dial"
	}
	for _, l := range strings.Split(journal, "\n") {
		if strings.Contains(l, "front both:") || strings.Contains(l, "front: ") && strings.Contains(l, "now serves") {
			st.Fallbacks = append(st.Fallbacks, strings.TrimSpace(l))
		}
	}
	if len(st.Fallbacks) > 8 {
		st.Fallbacks = st.Fallbacks[len(st.Fallbacks)-8:]
	}
	return st
}

func renderFrontState(w io.Writer, st frontState) {
	run := tr("running", "در حال اجرا")
	if !st.Running {
		run = tr("NOT running (no fresh status)", "اجرا نمی‌شود (وضعیت تازه نیست)")
	}
	role := tr("listening side: the fronts are chosen here", "سمت شنونده: front اینجا انتخاب می‌شود")
	if st.Role == "dial" {
		role = tr("dialing side: fronts come from the other server's announcement", "سمت شماره‌گیر: front از اعلام سرور مقابل می‌آید")
	}
	fmt.Fprintf(w, "%s — %s — %s\n", st.Unit, run, role)
	if len(st.IPs) > 0 {
		fmt.Fprintf(w, "%-16s %-10s %-8s %-26s %s\n", "IP", tr("config", "تنظیم"), tr("in use", "فعال"), tr("target", "هدف"), tr("links: reality/tls/cover/rejected", "لینک: reality/tls/پوشش/رد"))
		for _, r := range st.IPs {
			c := "-"
			if r.Counters != nil {
				c = fmt.Sprintf("%d/%d/%d/%d", r.Counters.Reality, r.Counters.TLS, r.Counters.Cover, r.Counters.Rejected)
			}
			fmt.Fprintf(w, "%-16s %-10s %-8s %-26s %s\n", r.IP, r.Configured, r.InUse, clip(firstNonEmpty(r.Target, "-"), 26), c)
		}
	}
	for _, p := range st.Paths {
		line := fmt.Sprintf("  %s %-34s %-8s", tr("path", "مسیر"), p.Path, p.Kind)
		if p.Both != nil {
			line += fmt.Sprintf(" %s %s  reality ok/fail %d/%d  tls ok/fail %d/%d  %s %d", tr("now", "اکنون"), p.Both.Mode, p.Both.RealityOK, p.Both.RealityFail, p.Both.TLSOK, p.Both.TLSFail, tr("fallbacks", "بازگشت به tls"), p.Both.Fallbacks)
		}
		fmt.Fprintln(w, line)
	}
	if r := st.Reality; r != nil {
		fmt.Fprintf(w, "reality: %s %d, %s %d (%d B)", tr("authenticated links", "لینک‌های احراز شده"), r.Authed, tr("probes spliced to the target", "کاوش‌های هدایت‌شده به هدف"), r.FallbackConns, r.FallbackBytes)
		if len(r.Rejected) > 0 {
			var ks []string
			for k, v := range r.Rejected {
				ks = append(ks, fmt.Sprintf("%s=%d", k, v))
			}
			sort.Strings(ks)
			fmt.Fprintf(w, ", %s %s", tr("rejected", "رد"), strings.Join(ks, " "))
		}
		fmt.Fprintln(w)
	}
	if len(st.Fallbacks) > 0 {
		fmt.Fprintln(w, tr("recent front events (24 h):", "رویدادهای اخیر front (۲۴ ساعت):"))
		for _, l := range st.Fallbacks {
			fmt.Fprintln(w, "  "+clip(l, 160))
		}
	} else {
		fmt.Fprintln(w, tr("no front fallback events in the last 24 h", "در ۲۴ ساعت گذشته بازگشتی در front نبوده است"))
	}
}

func realityStatusCmd(args []string) int {
	fs := flag.NewFlagSet("reality status", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	js := fs.Bool("json", false, "machine-readable")
	lang := langFlag(fs)
	fs.Parse(args)
	setLang(*lang)
	path, fc, _ := loadUnit(*unit)
	ls, running := readLive(path)
	j, _ := exec.Command("journalctl", "-u", cleanUnit(*unit), "--since", "-24h", "--no-pager", "-o", "short-iso", "-q").Output()
	st := buildFrontState(*unit, fc, ls, running, string(j))
	if *js {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	renderFrontState(os.Stdout, st)
	return 0
}

func realityTargetsCmd(args []string) int {
	fs := flag.NewFlagSet("reality targets", flag.ExitOnError)
	add := fs.String("add", "", "more candidates, comma separated")
	only := fs.Bool("only", false, "check only the --add candidates")
	js := fs.Bool("json", false, "machine-readable")
	lang := langFlag(fs)
	fs.Parse(args)
	setLang(*lang)
	var list []string
	if !*only {
		list = append(list, realityCandidates...)
	}
	for _, s := range strings.Split(*add, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, s)
		}
	}
	rs := checkTargets(list)
	if *js {
		b, _ := json.MarshalIndent(rs, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	renderTargets(os.Stdout, rs)
	fmt.Println(tr("Use one: hs2 reality front -u U --ip IP reality --target HOST:443", "استفاده: hs2 reality front -u U --ip IP reality --target HOST:443"))
	return 0
}

func realityCheckTargetCmd(args []string) int {
	fs := flag.NewFlagSet("reality check-target", flag.ExitOnError)
	js := fs.Bool("json", false, "machine-readable")
	lang := langFlag(fs)
	pos := parseInterleaved(fs, args)
	setLang(*lang)
	if len(pos) < 1 {
		fmt.Println("usage: hs2 reality check-target host[:443] [server_name] [--json]")
		return 2
	}
	sni := ""
	if len(pos) > 1 {
		sni = pos[1]
	}
	r := probeTarget(pos[0], sni, nil)
	if *js {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
	} else {
		printTargetCheck(os.Stdout, r)
	}
	if !r.OK {
		return 1
	}
	return 0
}

// ---- doctor ------------------------------------------------------------------------

// checkFronts is the doctor's "front" category: per tunnel with a reality /
// both front, the target is reachable (listening side), the keys match (no
// authenticated reality link while the peer keeps trying = a key or short id
// mismatch), and the dialing side's "both" paths are not falling back to tls.
func checkFronts(statuses []namedStatus, cfgOf func(unit string) (fileConfig, bool), dial func(addr string) error) []finding {
	var out []finding
	for _, s := range statuses {
		u := unitFromStatusFile(s.file)
		fc, ok := cfgOf(u)
		if !ok {
			continue
		}
		area := "front " + u
		if tunnelListens(fc) {
			seen := map[string]bool{}
			nRealityIPs := 0
			for _, e := range lbLocalList(lbCur(fc)) {
				f := e.Front
				if f.IsDefault() {
					f = tunnelFront(fc)
				}
				k := f.NormKind()
				if k != tlscarrier.FrontReality && k != tlscarrier.FrontBoth {
					continue
				}
				nRealityIPs++
				if f.Target == "" || seen[f.Target] {
					continue
				}
				seen[f.Target] = true
				if err := dial(f.Target); err != nil {
					out = append(out, finding{Sev: "BAD", Area: area, What: fmt.Sprintf(tr("reality target %s unreachable: %v", "هدف Reality %s در دسترس نیست: %v"), f.Target, err),
						Why: tr("probes that are not ours get a dead end instead of the real site — that is a fingerprint", "کاوشگرهای غریبه به‌جای سایت واقعی به بن‌بست می‌خورند — این خودش اثر انگشت است"),
						Do:  tr("pick another target: hs2 reality targets; then hs2 reality front -u U --ip IP reality --target NEW", "هدف دیگری انتخاب کنید: hs2 reality targets؛ سپس hs2 reality front -u U --ip IP reality --target NEW")})
				} else {
					out = append(out, finding{Sev: "OK", Area: area, What: fmt.Sprintf(tr("reality target %s reachable", "هدف Reality %s در دسترس است"), f.Target)})
				}
			}
			if nRealityIPs == 0 {
				continue
			}
			if r := s.ls.Reality; r != nil && s.age < 60 {
				auth := r.Rejected["tunnel_auth"] + r.Rejected["handshake"]
				switch {
				case r.Authed == 0 && auth >= 5:
					out = append(out, finding{Sev: "BAD", Area: area, What: fmt.Sprintf(tr("no authenticated reality link but %d failed authentications: key / short id mismatch", "هیچ لینک Reality احراز نشده ولی %d احراز ناموفق: کلید یا short id ناهمخوان"), auth),
						Why: tr("the other side dials with a public key or short id this side does not hold", "سمت مقابل با کلید عمومی یا short id ای می‌آید که این سمت ندارد"),
						Do:  tr("let the announcement carry the key: remove an explicit public_key in the other side's lb list, or rotate: hs2 reality rotate-key -u U --all", "بگذارید کلید با اعلام برسد: public_key صریح را از فهرست lb سمت مقابل بردارید یا کلید را بچرخانید: hs2 reality rotate-key -u U --all")})
				case r.Authed > 0:
					out = append(out, finding{Sev: "OK", Area: area, What: fmt.Sprintf(tr("%d authenticated reality links; %d probes spliced to the target", "%d لینک Reality احراز شده؛ %d کاوش به هدف هدایت شد"), r.Authed, r.FallbackConns)})
				}
				if n := r.Rejected["target_down"] + r.Rejected["target_dial"]; n > 0 {
					out = append(out, finding{Sev: "WARN", Area: area, What: fmt.Sprintf(tr("%d spliced probes could not reach the target", "%d کاوش هدایت‌شده به هدف نرسید"), n),
						Do: tr("check the target: hs2 reality check-target HOST", "هدف را بررسی کنید: hs2 reality check-target HOST")})
				}
			}
		}
		for _, p := range s.ls.FrontPaths {
			if p.Both == nil {
				continue
			}
			b := p.Both
			tries := b.RealityOK + b.RealityFail
			switch {
			case b.Mode == tlscarrier.FrontTLS:
				out = append(out, finding{Sev: "WARN", Area: area, What: fmt.Sprintf(tr("path %s fell back to tls (reality failed %d times, %d fallbacks)", "مسیر %s به tls برگشته است (reality %d بار شکست، %d بازگشت)"), p.Path, b.RealityFail, b.Fallbacks),
					Why: tr("reality is being blocked or the keys do not match; the path still carries traffic over tls", "Reality مسدود می‌شود یا کلیدها ناهمخوان‌اند؛ مسیر هنوز روی tls ترافیک می‌برد"),
					Do:  tr("hs2 reality status -u U; on the listening server: hs2 doctor --only front", "hs2 reality status -u U؛ روی سرور شنونده: hs2 doctor --only front")})
			case tries >= 10 && b.RealityFail*5 > tries:
				out = append(out, finding{Sev: "WARN", Area: area, What: fmt.Sprintf(tr("path %s: high reality failure rate (%d of %d)", "مسیر %s: نرخ شکست reality بالا (%d از %d)"), p.Path, b.RealityFail, tries)})
			}
		}
	}
	return out
}

func doctorFrontFindings(statuses []namedStatus) []finding {
	return checkFronts(statuses, func(u string) (fileConfig, bool) {
		b, err := os.ReadFile(configForUnit(u))
		if err != nil {
			return fileConfig{}, false
		}
		var fc fileConfig
		if json.Unmarshal(b, &fc) != nil {
			return fileConfig{}, false
		}
		return fc, true
	}, func(addr string) error {
		c, err := net.DialTimeout("tcp", addr, 4*time.Second)
		if err == nil {
			c.Close()
		}
		return err
	})
}

// realityTargetsOf: the listening side's reality / both targets of fc.
func realityTargetsOf(fc fileConfig) []string {
	if !tunnelListens(fc) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(f *tlscarrier.FrontProfile) {
		if k := f.NormKind(); (k == tlscarrier.FrontReality || k == tlscarrier.FrontBoth) && f.Target != "" && !seen[f.Target] {
			seen[f.Target] = true
			out = append(out, f.Target)
		}
	}
	add(tunnelFront(fc))
	if lbOn(fc) {
		for _, e := range lbLocalList(fc) {
			add(e.Front)
		}
	}
	return out
}
