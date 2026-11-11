package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// realTunHost is tunHostEnv on the live server.
func realTunHost() tunHostEnv {
	return tunHostEnv{
		etcDir:    hs2CfgDir,
		validate:  realHost,
		ifaceUsed: func(n string) bool { _, err := net.InterfaceByName(n); return err == nil },
		ping:      realPing,
		dial: func(addr string) error {
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err == nil {
				c.Close()
			}
			return err
		},
		lookup: net.LookupHost,
		exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		sysctl: func(k string) string {
			b, _ := os.ReadFile("/proc/sys/" + strings.ReplaceAll(k, ".", "/"))
			return strings.TrimSpace(string(b))
		},
		unitFile: func(u string) bool {
			_, err := os.Stat(filepath.Join(systemdDir, u+".service"))
			return err == nil
		},
		publicIPs: localPublicIPs,
	}
}

// realPing: 3 pings (optionally from src), returns the average RTT.
func realPing(src, dst string) (float64, bool) {
	args := []string{"-n", "-q", "-c", "3", "-i", "0.3", "-W", "1"}
	if src != "" {
		args = append(args, "-I", src)
	}
	if ip := net.ParseIP(dst); ip != nil && ip.To4() == nil {
		args = append(args, "-6")
	}
	out, err := exec.Command("ping", append(args, dst)...).Output()
	if err != nil {
		return 0, false
	}
	// rtt min/avg/max/mdev = 1.0/2.0/3.0/0.1 ms
	s := string(out)
	if i := strings.Index(s, "= "); i >= 0 {
		f := strings.Split(strings.Fields(s[i+2:])[0], "/")
		if len(f) >= 2 {
			v, err := strconv.ParseFloat(f[1], 64)
			return v, err == nil
		}
	}
	return 0, false
}

func localPublicIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsMulticast() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

func routeDev(ip string) string {
	out, _ := exec.Command("ip", "route", "get", ip).Output()
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

func tunnelCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: hs2 tunnel list|create|join|code|clone|edit|renew|start|stop|restart|logs|delete|preflight …")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		tunnelListCmd(rest)
	case "create", "new":
		tunnelCreateCmd(rest, nil)
	case "join":
		tunnelJoinCmd(rest)
	case "code", "clone":
		tunnelCodeCmd(sub, rest)
	case "edit":
		tunnelEditCmd(rest)
	case "renew", "migrate":
		tunnelRenewCmd(rest)
	case "start", "stop", "restart":
		tunnelSvcCmd(sub, rest)
	case "logs":
		tunnelLogsCmd(rest)
	case "delete", "rm":
		tunnelDeleteCmd(rest)
	case "preflight":
		tunnelPreflightCmd(rest)
	default:
		die("unknown tunnel subcommand %q", sub)
	}
}

func langFlag(fs *flag.FlagSet) *string { return fs.String("lang", "en", "output language: en|fa") }

func tunnelListCmd(args []string) {
	fs := flag.NewFlagSet("tunnel list", flag.ExitOnError)
	lang := langFlag(fs)
	js := fs.Bool("json", false, "machine-readable")
	names := fs.Bool("names", false, "only the unit names, in list order (for the menu)")
	fs.Parse(args)
	deepLang = *lang
	if *names {
		for _, r := range tunnelRows(hs2CfgDir, nil) {
			fmt.Println(r.Unit)
		}
		return
	}
	rows := tunnelRows(hs2CfgDir, realUnitInfo)
	if *js {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Print(renderTunnelList(rows))
}

// ---- the interactive wizard ------------------------------------------------

var errAbort = errors.New("aborted")

type asker struct {
	r   *bufio.Reader
	out io.Writer
}

// ask prints a question; "" keeps def; "q" or EOF aborts the wizard.
func (a asker) ask(q, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(a.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(a.out, "%s: ", q)
	}
	line, err := a.r.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", errAbort
	}
	if line == "q" || line == "Q" {
		return "", errAbort
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (a asker) choose(q string, opts []string, def int) (int, error) {
	fmt.Fprintln(a.out, q)
	for i, o := range opts {
		fmt.Fprintf(a.out, "   %d) %s\n", i+1, o)
	}
	for {
		v, err := a.ask(tr("Choose", "انتخاب"), strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(v)
		if err == nil && n >= 1 && n <= len(opts) {
			return n, nil
		}
		fmt.Fprintln(a.out, tr("  enter a number from the list (q = cancel)", "  یک شماره از فهرست وارد کنید (q = لغو)"))
	}
}

// wizard asks for what the spec does not have yet. pre: prefilled (join).
func wizard(a asker, s *tunnelSpec, advanced, joining bool, h tunHostEnv) error {
	w := func(f string, x ...any) { fmt.Fprintf(a.out, f, x...) }
	w("%s\n", tr("── New tunnel ── (q = cancel at any step; nothing is changed until the last step)", "── تونل جدید ── (q = لغو در هر مرحله؛ تا مرحلهٔ آخر هیچ تغییری داده نمی‌شود)"))
	var err error
	if s.Role == "" {
		n, err := a.choose(tr("1/6 Role of THIS server:", "۱/۶ نقش این سرور:"), []string{tr("Kharej (foreign server, the panel side)", "خارج (سرور خارجی، سمت پنل)"), tr("Iran (the users connect here)", "ایران (کاربران به اینجا وصل می‌شوند)")}, 1)
		if err != nil {
			return err
		}
		s.Role = map[int]string{1: "kharej", 2: "iran"}[n]
	}
	if !joining {
		n, err := a.choose(tr("2/6 Direction — reverse helps when outgoing connections from Iran are filtered or the Kharej server is behind NAT:",
			"۲/۶ جهت — reverse وقتی کمک می‌کند که اتصال خروجی از ایران فیلتر است یا سرور خارج پشت NAT است:"),
			[]string{tr("direct — Iran connects out to Kharej (default)", "direct — ایران به خارج وصل می‌شود (پیش‌فرض)"), tr("reverse — Kharej connects in to Iran", "reverse — خارج به ایران وصل می‌شود")}, 1)
		if err != nil {
			return err
		}
		s.Direction = map[int]string{1: "direct", 2: "reverse"}[n]
		opts := []string{tr("mtcp — multi-link TLS, port forwarding (recommended)", "mtcp — چندلینکی TLS، پورت‌فوروارد (پیشنهادی)"), tr("tun+mtcp — the same plus a routed tun interface", "tun+mtcp — همان به‌همراه رابط tun مسیریابی‌شده")}
		if advanced {
			opts = append(opts, tr("dgtun over UDP (advanced)", "dgtun روی UDP (پیشرفته)"), tr("dgtun over ICMP (advanced)", "dgtun روی ICMP (پیشرفته)"))
		}
		n, err = a.choose(tr("3/6 Carrier:", "۳/۶ حامل:"), opts, 1)
		if err != nil {
			return err
		}
		s.Carrier, s.Encap = []string{"mtcp", "l3mtcp", "dgtun", "dgtun"}[n-1], []string{"", "", "udp", "icmp"}[n-1]
		if s.Carrier != "dgtun" {
			for {
				n, err = a.choose(tr("4/6 Front (what a censor sees):", "۴/۶ جلو (آنچه سانسورچی می‌بیند):"), []string{"TLS", tr("Reality — coming (M5)", "Reality — به‌زودی (M5)")}, 1)
				if err != nil {
					return err
				}
				if n == 1 {
					break
				}
				w("  %s\n", tr("Reality is not available in this release yet — TLS for now.", "Reality هنوز در این نسخه نیست — فعلاً TLS."))
			}
			s.Front = "tls"
		}
	}
	listener := s.listener()
	w("%s\n", tr("5/6 Addresses and ports", "۵/۶ آدرس‌ها و پورت‌ها"))
	if listener {
		def := s.PublicIP
		if def == "" && h.publicIPs != nil {
			if ips := h.publicIPs(); len(ips) > 0 {
				def = ips[0]
			}
		}
		if s.PublicIP, err = a.ask(tr("  Public IP of THIS server (the peer connects to it)", "  IP عمومی این سرور (سمت مقابل به آن وصل می‌شود)"), def); err != nil {
			return err
		}
	} else if s.Peer == "" {
		if s.Peer, err = a.ask(tr("  IP of the OTHER server", "  IP سرور مقابل"), ""); err != nil {
			return err
		}
	}
	if s.Port == 0 {
		p, err := a.ask(tr("  Tunnel port (users never see it)", "  پورت تونل (کاربران آن را نمی‌بینند)"), "443")
		if err != nil {
			return err
		}
		s.Port, _ = strconv.Atoi(p)
	}
	if s.Role == "iran" && s.UserPorts == "" {
		if s.UserPorts, err = a.ask(tr("  User ports on this server (comma-separated)", "  پورت‌های کاربران روی این سرور (با کاما)"), "8443"); err != nil {
			return err
		}
	}
	if s.Role == "kharej" {
		def := s.Expose
		if def == "" {
			def = "127.0.0.1:443"
		}
		if s.Expose, err = a.ask(tr("  Default target (panel inbound) for user ports", "  مقصد پیش‌فرض (اینباند پنل) برای پورت‌های کاربران"), def); err != nil {
			return err
		}
		if s.Targets, err = a.ask(tr("  Per-port targets, e.g. 2053=127.0.0.1:2054 (Enter = none)", "  مقصد هر پورت، مثل 2053=127.0.0.1:2054 (Enter = هیچ)"), s.Targets); err != nil {
			return err
		}
	}
	if s.tlsFront() {
		q := tr("  Domain (SNI; Enter = none)", "  دامنه (SNI؛ Enter = بدون دامنه)")
		if s.Domain, err = a.ask(q, s.Domain); err != nil {
			return err
		}
		if listener && s.Cert == "" {
			def := "self"
			if s.Domain != "" && h.exists != nil && h.exists("/etc/letsencrypt/live/"+s.Domain+"/fullchain.pem") {
				def = "/etc/letsencrypt/live/" + s.Domain + "/fullchain.pem"
				s.Key = "/etc/letsencrypt/live/" + s.Domain + "/privkey.pem"
			}
			if s.Cert, err = a.ask(tr("  Certificate: PEM path, or self (self-signed)", "  گواهی: مسیر PEM یا self (خودامضا)"), def); err != nil {
				return err
			}
			if s.Cert != "self" && s.Key == "" {
				if s.Key, err = a.ask(tr("  Private key PEM path", "  مسیر کلید خصوصی PEM"), ""); err != nil {
					return err
				}
			}
		}
	}
	if advanced {
		if v, err := a.ask(tr("  Tunnel name (systemd unit)", "  نام تونل (سرویس systemd)"), s.Name); err != nil {
			return err
		} else {
			s.Name = v
		}
		if v, err := a.ask(tr("  Source/listen IP on this server (Enter = automatic)", "  IP مبدأ/گوش‌دادن روی این سرور (Enter = خودکار)"), s.BindIP); err != nil {
			return err
		} else {
			s.BindIP = v
		}
		if !joining {
			if v, err := a.ask("  MTU", strconv.Itoa(s.MTU)); err != nil {
				return err
			} else {
				s.MTU, _ = strconv.Atoi(v)
			}
		}
	}
	v, err := a.ask(tr("6/6 Enable 24h charts? (cheap: one sample a minute) [y/N]", "۶/۶ نمودار ۲۴ساعته فعال شود؟ (کم‌هزینه: یک نمونه در دقیقه) [y/N]"), "n")
	if err != nil {
		return err
	}
	s.Stats = v == "y" || v == "Y" || v == "yes"
	return nil
}

// ---- create / join ---------------------------------------------------------

type createOpts struct {
	yes, dryRun, advanced, joining, interactive bool
	lang                                        string
}

func tunnelCreateCmd(args []string, pre *tunnelSpec) {
	fs := flag.NewFlagSet("tunnel create", flag.ExitOnError)
	from := fs.String("from-json", "", "non-interactive: spec file (- = stdin)")
	yes := fs.Bool("yes", false, "do not ask for the final confirmation")
	dry := fs.Bool("dry-run", false, "only print summary, config and pre-flight")
	adv := fs.Bool("advanced", false, "advanced questions (dgtun carriers, name, MTU, source IP)")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	var s tunnelSpec
	inter := *from == ""
	if !inter {
		var b []byte
		var err error
		if *from == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*from)
		}
		if err != nil {
			die("%v", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			die("spec: %v", err)
		}
	}
	os.Exit(runCreate(s, createOpts{yes: *yes, dryRun: *dry, advanced: *adv, interactive: inter, lang: *lang}, realTunHost(), realCreator{}))
}

// creator is the part of create that changes the host (fake in tests).
type creator interface {
	install(s tunnelSpec, cfgPath string, cfg []byte) error
	healthy(unit string) bool
	remove(unit, cfgPath string)
	stats(on bool)
}

func runCreate(s tunnelSpec, o createOpts, h tunHostEnv, c creator) int {
	in := asker{bufio.NewReader(os.Stdin), os.Stdout}
	if o.interactive {
		if err := wizard(in, &s, o.advanced, o.joining, h); err != nil {
			if errors.Is(err, errAbort) {
				fmt.Println(tr("Cancelled — nothing was changed on this server.", "لغو شد — هیچ تغییری روی این سرور داده نشد."))
				return 3
			}
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
	}
	if err := normalize(&s, h); err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	cfgPath := configForUnit(s.Name)
	certNote := ""
	if s.listener() && s.tlsFront() && s.Cert == "self" {
		certNote = tr(" (self-signed, generated now)", " (خودامضا، اکنون ساخته می‌شود)")
	}
	cfgSpec := s
	if s.Cert == "self" {
		cfgSpec.Cert, cfgSpec.Key = filepath.Join(certDir, s.Name+".crt"), filepath.Join(certDir, s.Name+".key")
	}
	if s.Cert == "auto" {
		cfgSpec.Cert, cfgSpec.Key = "/etc/letsencrypt/live/"+s.Domain+"/fullchain.pem", "/etc/letsencrypt/live/"+s.Domain+"/privkey.pem"
		s.Cert, s.Key = cfgSpec.Cert, cfgSpec.Key
	}
	cfg, err := buildConfig(cfgSpec, randHex(16))
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	errs, _ := checkConfig(cfg, func(net.IP) bool { return true }, time.Now())
	if s.Cert == "self" { // generated only after the summary is accepted
		errs = slices.DeleteFunc(errs, selfCertPending)
	}
	if len(errs) > 0 {
		fmt.Println("ERROR: the generated config does not pass hs2 check:")
		for _, e := range errs {
			fmt.Println("  - " + e)
		}
		return 1
	}
	// summary
	fmt.Println()
	fmt.Println(tr("── Summary ──", "── خلاصه ──"))
	fmt.Printf("  %-14s %s (%s)\n", tr("tunnel", "تونل"), s.Name, cfgPath)
	fmt.Printf("  %-14s %s, %s, %s%s\n", tr("role", "نقش"), s.Role, s.Direction, s.Carrier, map[bool]string{true: "/" + s.Encap, false: ""}[s.Encap != ""])
	if s.listener() {
		fmt.Printf("  %-14s %s %s:%d\n", tr("listens", "گوش می‌دهد"), tr("peer dials", "سمت مقابل وصل می‌شود به"), s.PublicIP, s.Port)
	} else {
		fmt.Printf("  %-14s %s:%d\n", tr("dials", "وصل می‌شود به"), s.Peer, s.Port)
	}
	if s.Role == "iran" {
		fmt.Printf("  %-14s %s\n", tr("user ports", "پورت کاربران"), s.UserPorts)
	} else {
		fmt.Printf("  %-14s %s %s\n", tr("targets", "مقصدها"), s.Expose, s.Targets)
	}
	if s.tlsFront() && s.listener() {
		fmt.Printf("  %-14s tls, sni=%s cert=%s%s\n", tr("front", "جلو"), s.Domain, s.Cert, certNote)
	} else if s.tlsFront() {
		fmt.Printf("  %-14s tls, sni=%s\n", tr("front", "جلو"), s.Domain)
	}
	fmt.Printf("  %-14s %s/30 on %s, mtu %d\n", "tun", s.Subnet, s.Iface, s.MTU)
	d, _ := semanticDiff([]byte("{}"), cfg, nil, nil)
	fmt.Println(tr("── Changes (new config) ──", "── تغییرات (پیکربندی جدید) ──"))
	printDiff(d)
	fmt.Println(tr("── Pre-flight ──", "── بررسی پیش از اجرا ──"))
	pc := preflight(s, cfg, cfgPath, h)
	printChecks(pc)
	if preflightFailed(pc) {
		fmt.Println(tr("Pre-flight failed — nothing was changed. Fix the [FAIL] lines and run again.", "بررسی ناموفق بود — هیچ تغییری داده نشد. موارد [FAIL] را رفع کنید و دوباره اجرا کنید."))
		return 1
	}
	if o.dryRun {
		fmt.Println(tr("dry run — nothing was changed.", "اجرای آزمایشی — هیچ تغییری داده نشد."))
		return 0
	}
	if !o.yes {
		if !o.interactive {
			fmt.Println(tr("add --yes to create it (non-interactive)", "برای ساخت --yes اضافه کنید (غیرتعاملی)"))
			return 1
		}
		v, err := in.ask(tr("Create this tunnel now? [y/N]", "این تونل اکنون ساخته شود؟ [y/N]"), "n")
		if err != nil || (v != "y" && v != "Y" && v != "yes") {
			fmt.Println(tr("Cancelled — nothing was changed on this server.", "لغو شد — هیچ تغییری روی این سرور داده نشد."))
			return 3
		}
	}
	if s.Cert == "self" {
		if err := selfSignedCert(cfgSpec.Cert, cfgSpec.Key, s.Domain, s.PublicIP); err != nil {
			fmt.Println("ERROR: certificate: " + err.Error())
			return 1
		}
	}
	if err := c.install(s, cfgPath, cfg); err != nil {
		c.remove(s.Name, cfgPath)
		fmt.Println("ERROR: " + err.Error() + tr(" — removed again, nothing left behind", " — دوباره حذف شد، چیزی باقی نماند"))
		return 1
	}
	if !c.healthy(s.Name) {
		c.remove(s.Name, cfgPath)
		fmt.Printf(tr("ERROR: %s did not stay up — it was removed again (see: journalctl -u %s)\n", "خطا: %s بالا نماند — دوباره حذف شد (ببینید: journalctl -u %s)\n"), s.Name, s.Name)
		return 1
	}
	saveVersion(s.Name, cfg, readDropins(s.Name), "create", time.Now())
	c.stats(s.Stats)
	fmt.Printf(tr("OK: %s is running.\n", "انجام شد: %s در حال اجراست.\n"), s.Name)
	if o.joining {
		fmt.Println(tr("Both sides are configured — check traffic: hs2 dashboard", "هر دو سمت پیکربندی شد — ترافیک را ببینید: hs2 dashboard"))
		return 0
	}
	code, err := encodeCode(codeFor(s))
	if err != nil {
		fmt.Println("WARN: " + err.Error())
		return 0
	}
	cc, _ := decodeCode(code)
	fmt.Println(tr("── Connection code — on the OTHER server run:", "── کد اتصال — روی سرور مقابل اجرا کنید:"))
	fmt.Println("   hs2 tunnel join '" + code + "'")
	fmt.Println(tr("   (contains the shared key: treat it like a password)", "   (شامل کلید مشترک است: مثل رمز عبور نگه دارید)"))
	fmt.Println(tr("   check: ", "   بررسی: ") + shortCode(cc))
	return 0
}

func tunnelJoinCmd(args []string) {
	fs := flag.NewFlagSet("tunnel join", flag.ExitOnError)
	upd := fs.Bool("update", false, "update the existing tunnel of that name from the code (safe apply)")
	ports := fs.String("user-ports", "", "iran: user ports")
	expose := fs.String("expose", "", "kharej: default target")
	targets := fs.String("targets", "", "kharej: per-port targets")
	cert := fs.String("cert", "", "listener: self | PEM path")
	key := fs.String("key", "", "listener: key PEM path")
	pub := fs.String("public-ip", "", "listener: this server's public IP")
	bind := fs.String("bind-ip", "", "source/listen IP on this server")
	name := fs.String("name", "", "override the tunnel name")
	yes := fs.Bool("yes", false, "non-interactive")
	dry := fs.Bool("dry-run", false, "only print")
	within := fs.Duration("confirm-within", 5*time.Minute, "--update: confirm window")
	lang := langFlag(fs)
	// accept the code before or after the flags
	var code string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		code, args = args[0], args[1:]
	}
	// flags may follow the code too ("join --update CODE --yes")
	pos := parseInterleaved(fs, args)
	deepLang = *lang
	if code == "" && len(pos) > 0 {
		code = pos[0]
	}
	if code == "" {
		die("usage: hs2 tunnel join <code> [flags]")
	}
	c, err := decodeCode(code)
	if err != nil {
		if errors.Is(err, errCodeNewer) {
			die("%v", err)
		}
		die(tr("%v", "%v"), err)
	}
	if c.V == 1 {
		fmt.Println(tr("note: a classic link (v1) from an older hs2 — read as compatible; the peer can upgrade for v2 codes.", "توجه: لینک کلاسیک (v1) از hs2 قدیمی‌تر — سازگار خوانده شد؛ سمت مقابل می‌تواند برای کد v2 به‌روز شود."))
	}
	h := realTunHost()
	s := specFromCode(c, h)
	if *name != "" {
		s.Name = *name
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&s.UserPorts, *ports)
	set(&s.Expose, *expose)
	set(&s.Targets, *targets)
	set(&s.Cert, *cert)
	set(&s.Key, *key)
	set(&s.PublicIP, *pub)
	set(&s.BindIP, *bind)
	if *upd {
		os.Exit(joinUpdate(s, *within, *yes, *dry))
	}
	fmt.Printf(tr("Joining %s as %s (%s, %s)\n", "پیوستن به %s به‌عنوان %s (%s، %s)\n"), c.Name, s.Role, s.Direction, s.Carrier)
	inter := !*yes && isTTY(os.Stdin)
	os.Exit(runCreate(s, createOpts{yes: *yes, dryRun: *dry, joining: true, interactive: inter, lang: *lang}, h, realCreator{}))
}

// joinUpdate rebuilds an existing tunnel from a newer code: diff + safe apply.
func joinUpdate(s tunnelSpec, within time.Duration, yes, dry bool) int {
	path := configForUnit(s.Name)
	cur, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	m, _ := readConfigMap(path)
	var fc fileConfig
	json.Unmarshal(cur, &fc)
	// bilateral fields come from the code; local ones (ports, targets, cert) stay.
	m["shared_key"], m["mtu"], m["carrier"], m["reverse"] = s.SharedKey, s.MTU, s.Carrier, s.Direction == "reverse"
	if s.Carrier == "dgtun" {
		m["encap"] = s.Encap
	}
	if !s.listener() {
		m["addr"] = net.JoinHostPort(strings.Trim(s.Peer, "[]"), strconv.Itoa(s.Port))
		if s.Domain != "" {
			m["sni"] = s.Domain
		}
	} else {
		host, _, _ := net.SplitHostPort(fc.Addr)
		m["addr"] = net.JoinHostPort(host, strconv.Itoa(s.Port))
	}
	if s.LB != nil {
		m["lb"] = s.LB
	}
	nb, err := marshalConfig(m)
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	return safeApplyNew(s.Name, path, cur, nb, "join-update", within, yes, dry)
}

// safeApplyNew: diff, bilateral warning, SSH guard, then M1's safe apply.
func safeApplyNew(unit, path string, cur, nb []byte, reason string, within time.Duration, yes, dry bool) int {
	return safeApplyMode(unit, path, cur, nb, reason, within, yes, dry, false)
}

// safeApplyMode: safeApplyNew that applies (and rolls back) with a reload
// instead of a restart when reload is set (menu M5: per-IP fronts).
func safeApplyMode(unit, path string, cur, nb []byte, reason string, within time.Duration, yes, dry, reload bool) int {
	d, err := semanticDiff(cur, nb, nil, nil)
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	if len(d) == 0 {
		fmt.Println(tr("no changes.", "تغییری نیست."))
		return 0
	}
	printDiff(d)
	errs, warns := validateFull(nb, cur, path, realHost)
	printValidation(errs, warns)
	if len(errs) > 0 {
		return 1
	}
	var fc fileConfig
	json.Unmarshal(nb, &fc)
	if w := sshRidesTunnel(os.Getenv("SSH_CONNECTION"), fc, routeDev); w != "" {
		fmt.Println("WARN: " + w)
	}
	if bl := bilateralOf(d); len(bl) > 0 {
		fmt.Println(tr("── The OTHER server must change too: ", "── سرور مقابل هم باید تغییر کند: ") + strings.Join(bl, ", "))
		pub := ""
		if ips := localPublicIPs(); len(ips) > 0 {
			pub = ips[0]
		}
		if code, err := encodeCode(codeFor(specFromConfig(unit, fc, pub))); err == nil {
			fmt.Println(tr("   on it run: ", "   روی آن اجرا کنید: ") + "hs2 tunnel join --update '" + code + "'")
		}
	}
	if dry {
		fmt.Println(tr("dry run — nothing was changed.", "اجرای آزمایشی — هیچ تغییری داده نشد."))
		return 0
	}
	if !yes {
		in := asker{bufio.NewReader(os.Stdin), os.Stdout}
		v, err := in.ask(tr("Apply now (auto-rollback unless confirmed)? [y/N]", "اکنون اعمال شود (بدون تأیید خودکار برمی‌گردد)؟ [y/N]"), "n")
		if err != nil || (v != "y" && v != "Y") {
			fmt.Println(tr("Not applied.", "اعمال نشد."))
			return 3
		}
	}
	if reload {
		fmt.Println(tr("Applied with a reload: established links keep running, new links use the change.", "با reload اعمال می‌شود: لینک‌های برقرار ادامه می‌دهند، لینک‌های تازه تغییر را می‌گیرند."))
	}
	st, _, err := startApply(realOps{}, applyReq{Unit: unit, Cfg: path, Reason: reason, New: nb, Within: within, HealthGate: true, Reload: reload})
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	printState(st)
	fmt.Printf(tr("Confirm when it works: hs2 apply --confirm -u %s  (otherwise it rolls back by itself in %s)\n", "وقتی کار کرد تأیید کنید: hs2 apply --confirm -u %s  (وگرنه پس از %s خودش برمی‌گردد)\n"), cleanUnit(unit), within)
	return 0
}

// ---- code / clone ----------------------------------------------------------

func loadUnit(unit string) (string, fileConfig, []byte) {
	path := configForUnit(unit)
	b, err := os.ReadFile(path)
	if err != nil {
		die(tr("no tunnel %s (%v)", "تونل %s نیست (%v)"), unit, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		die("%s: %v", path, err)
	}
	return path, fc, b
}

func tunnelCodeCmd(sub string, args []string) {
	fs := flag.NewFlagSet("tunnel "+sub, flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	pub := fs.String("public-ip", "", "this server's public IP (listener; default: first public IP)")
	toCode := fs.Bool("to-code", sub == "clone", "clone: print a code that recreates THIS side elsewhere")
	show := fs.Bool("show", false, "also print the decoded code with secrets masked")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	_, fc, _ := loadUnit(*unit)
	ip := *pub
	if ip == "" {
		if ips := localPublicIPs(); len(ips) > 0 {
			ip = ips[0]
		}
	}
	s := specFromConfig(*unit, fc, ip)
	c := codeFor(s)
	if sub == "clone" && *toCode {
		c.Clone = true
		c.UserPorts, c.Panel, c.Targets = s.UserPorts, s.Expose, s.Targets
		fmt.Println(tr("Clone code — on the NEW server run (it takes this server's role; the peer must then be pointed at it):",
			"کد کلون — روی سرور جدید اجرا کنید (نقش همین سرور را می‌گیرد؛ سپس سمت مقابل باید به آن اشاره کند):"))
	}
	code, err := encodeCode(c)
	if err != nil {
		die("%v", err)
	}
	fmt.Println("hs2 tunnel join '" + code + "'")
	cc, _ := decodeCode(code)
	fmt.Println(tr("check: ", "بررسی: ") + shortCode(cc))
	if *show {
		fmt.Println(cc.masked())
	}
}

// ---- edit ------------------------------------------------------------------

var editSections = map[string][]string{
	"network":     {"addr", "bind_local_ip", "user_listen_ip", "forward_ports", "expose", "port_map", "udp"},
	"transport":   {"carrier", "reverse", "mtu", "encap"},
	"security":    {"sni", "shared_key", "cert_file", "key_file", "backend_addr"},
	"performance": {"min_links", "max_links", "per_link", "drain_idle_sec"},
	"lb":          {"lb"},
}

func tunnelEditCmd(args []string) {
	fs := flag.NewFlagSet("tunnel edit", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	sec := fs.String("section", "", "network|transport|security|performance|lb (show it)")
	var sets multiFlag
	fs.Var(&sets, "set", "key=value (repeatable; JSON value or plain string)")
	within := fs.Duration("confirm-within", 5*time.Minute, "confirm window")
	yes := fs.Bool("yes", false, "do not ask")
	dry := fs.Bool("dry-run", false, "only print the diff")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	path, _, cur := loadUnit(*unit)
	m, err := readConfigMap(path)
	if err != nil {
		die("%v", err)
	}
	if len(sets) == 0 {
		secs := []string{*sec}
		if *sec == "" {
			secs = []string{"network", "transport", "security", "performance", "lb"}
		}
		for _, sn := range secs {
			keys, ok := editSections[sn]
			if !ok {
				die("unknown section %q", sn)
			}
			fmt.Printf("[%s]\n", sn)
			for _, k := range keys {
				v, ok := m[k]
				if !ok {
					continue
				}
				b, _ := json.Marshal(v)
				vs := string(b)
				if sensitiveKey(k) {
					vs = maskSecret(strings.Trim(vs, `"`))
				}
				bi := ""
				if isBilateral(k) {
					bi = tr("   (bilateral: the other server must match)", "   (دوطرفه: سرور مقابل هم باید مطابق شود)")
				}
				fmt.Printf("  %-16s %s%s\n", k, vs, bi)
			}
		}
		fmt.Printf(tr("change: hs2 tunnel edit -u %s --set key=value …\n", "تغییر: hs2 tunnel edit -u %s --set key=value …\n"), cleanUnit(*unit))
		return
	}
	allowed := map[string]bool{}
	for _, ks := range editSections {
		for _, k := range ks {
			allowed[k] = true
		}
	}
	for _, kv := range sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !allowed[k] {
			die("--set %q: key must be one of the section keys (hs2 tunnel edit -u %s)", kv, cleanUnit(*unit))
		}
		var jv any
		if json.Unmarshal([]byte(v), &jv) == nil {
			if f, isNum := jv.(float64); isNum {
				jv = json.Number(strconv.FormatFloat(f, 'f', -1, 64))
			}
			m[k] = jv
		} else if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	nb, err := marshalConfig(m)
	if err != nil {
		die("%v", err)
	}
	os.Exit(safeApplyNew(*unit, path, cur, nb, "edit", *within, *yes, *dry))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// ---- renew / migrate -------------------------------------------------------

// bestLocalIP pings the peer from every local public IP and returns the
// lowest-RTT one (ties keep the current).
func bestLocalIP(peer string, ips []string, ping func(src, dst string) (float64, bool), cur string) (string, []string) {
	type r struct {
		ip string
		ms float64
	}
	var rs []r
	var lines []string
	for _, ip := range ips {
		if (net.ParseIP(ip).To4() == nil) != (net.ParseIP(peer).To4() == nil) {
			continue // family must match the peer
		}
		ms, ok := ping(ip, peer)
		if !ok {
			lines = append(lines, fmt.Sprintf("  %-40s %s", ip, tr("no answer", "بی‌پاسخ")))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-40s %.1f ms", ip, ms))
		rs = append(rs, r{ip, ms})
	}
	sort.SliceStable(rs, func(a, b int) bool { return rs[a].ms < rs[b].ms })
	if len(rs) == 0 {
		return cur, lines
	}
	best := rs[0]
	for _, x := range rs {
		if x.ip == cur && x.ms-best.ms < 1 {
			return cur, lines // within 1 ms: keep the current one
		}
	}
	return best.ip, lines
}

func tunnelRenewCmd(args []string) {
	fs := flag.NewFlagSet("tunnel renew", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	pick := fs.Bool("pick-ip", false, "choose the best local source IP toward the peer (ping each)")
	upg := fs.String("upgrade-from", "", "install this hs2 binary first (old one kept as .prev, restored if the tunnel does not come back)")
	within := fs.Duration("confirm-within", 5*time.Minute, "confirm window")
	yes := fs.Bool("yes", false, "do not ask")
	dry := fs.Bool("dry-run", false, "only print")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	path, fc, cur := loadUnit(*unit)
	fmt.Println(tr("binary: ", "باینری: ") + versionLine())
	if *upg != "" && !*dry {
		if err := upgradeBinary(*upg, cleanUnit(*unit)); err != nil {
			die("%v", err)
		}
	} else if *upg == "" {
		fmt.Println(tr("  (to upgrade the binary too: --upgrade-from /path/to/new/hs2, or bash install.sh → upgrade)", "  (برای به‌روزرسانی باینری: --upgrade-from /path/to/new/hs2 یا bash install.sh → upgrade)"))
	}
	if _, err := snapshotCurrent(*unit, path, "pre-renew", time.Now()); err != nil {
		fmt.Println("WARN: backup: " + err.Error())
	} else {
		fmt.Println(tr("backup: saved to the version history (hs2 config history -u ", "پشتیبان: در تاریخچهٔ نسخه‌ها ذخیره شد (hs2 config history -u ") + cleanUnit(*unit) + ")")
	}
	m, _ := readConfigMap(path)
	// rebuild: fill what newer releases expect and older configs lack
	if fc.Carrier != "tls" && fc.Carrier != "udp" && fc.Carrier != "auto" && fc.Carrier != "noise" {
		if _, ok := m["max_links"]; !ok {
			m["max_links"] = json.Number("0")
		}
	}
	if fc.Mode == "listen" != fc.Reverse && fc.CertFile != "" && fc.CoverSeed == "" && fc.BackendAddr == "builtin" {
		m["cover_seed"] = randHex(16)
	}
	if *pick {
		isDialer := (fc.Mode == "dial") != fc.Reverse
		host, _, _ := net.SplitHostPort(fc.Addr)
		if !isDialer {
			fmt.Println(tr("pick-ip: this side listens — the source IP is chosen on the dialing server", "pick-ip: این سمت گوش می‌دهد — IP مبدأ روی سرور وصل‌شونده انتخاب می‌شود"))
		} else {
			best, lines := bestLocalIP(host, localPublicIPs(), realPing, fc.BindLocalIP)
			fmt.Println(tr("ping to the peer from each local IP:", "پینگ تا سرور مقابل از هر IP محلی:"))
			for _, l := range lines {
				fmt.Println(l)
			}
			if best != "" && best != fc.BindLocalIP {
				fmt.Println(tr("best: ", "بهترین: ") + best)
				m["bind_local_ip"] = best
			}
		}
	}
	nb, err := marshalConfig(m)
	if err != nil {
		die("%v", err)
	}
	os.Exit(safeApplyNew(*unit, path, cur, nb, "renew", *within, *yes, *dry))
}

// ---- service actions -------------------------------------------------------

func tunnelSvcCmd(op string, args []string) {
	fs := flag.NewFlagSet("tunnel "+op, flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	yes := fs.Bool("yes", false, "do not ask")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	_, fc, _ := loadUnit(*unit)
	if op != "start" {
		if w := sshRidesTunnel(os.Getenv("SSH_CONNECTION"), fc, routeDev); w != "" {
			fmt.Println("WARN: " + w)
		}
		if !*yes {
			in := asker{bufio.NewReader(os.Stdin), os.Stdout}
			fmt.Printf(tr("%s %s: the users on it are disconnected for a few seconds.\n", "%s %s: کاربران آن چند ثانیه قطع می‌شوند.\n"), op, cleanUnit(*unit))
			v, err := in.ask(tr("Continue? [y/N]", "ادامه؟ [y/N]"), "n")
			if err != nil || (v != "y" && v != "Y") {
				fmt.Println(tr("Not changed.", "تغییری داده نشد."))
				os.Exit(3)
			}
		}
	}
	out, err := exec.Command("systemctl", op, cleanUnit(*unit)).CombinedOutput()
	if err != nil {
		die("systemctl %s: %v %s", op, err, out)
	}
	st, _ := realUnitInfo(cleanUnit(*unit))
	fmt.Printf("%s: %s\n", cleanUnit(*unit), st)
}

func tunnelLogsCmd(args []string) {
	fs := flag.NewFlagSet("tunnel logs", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	n := fs.Int("n", 80, "lines")
	f := fs.Bool("f", false, "follow")
	fs.Parse(args)
	a := []string{"-u", cleanUnit(*unit), "-n", strconv.Itoa(*n), "--no-pager"}
	if *f {
		a = append(a, "-f")
	}
	cmd := exec.Command("journalctl", a...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	cmd.Run()
}

func tunnelDeleteCmd(args []string) {
	fs := flag.NewFlagSet("tunnel delete", flag.ExitOnError)
	unit := fs.String("u", "", "tunnel")
	yes := fs.Bool("yes", false, "step 1 confirmation")
	name := fs.String("confirm-name", "", "step 2: the tunnel name again")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	u := cleanUnit(*unit)
	if u == "" {
		die("usage: hs2 tunnel delete -u <tunnel>")
	}
	path, fc, _ := loadUnit(u)
	fmt.Printf(tr("Delete %s (%s, %s): its service, config and tun interface are removed. The other tunnels are untouched; the other server keeps its side.\n",
		"حذف %s (%s، %s): سرویس، پیکربندی و رابط tun آن حذف می‌شود. تونل‌های دیگر دست نمی‌خورند؛ سرور مقابل سمت خود را نگه می‌دارد.\n"), u, fc.Mode, fc.Carrier)
	if w := sshRidesTunnel(os.Getenv("SSH_CONNECTION"), fc, routeDev); w != "" {
		fmt.Println("WARN: " + w)
	}
	in := asker{bufio.NewReader(os.Stdin), os.Stdout}
	if !*yes {
		v, err := in.ask(tr("Step 1/2 — really delete? [y/N]", "مرحلهٔ ۱/۲ — واقعاً حذف شود؟ [y/N]"), "n")
		if err != nil || (v != "y" && v != "Y") {
			fmt.Println(tr("Not deleted.", "حذف نشد."))
			os.Exit(3)
		}
	}
	if *name == "" {
		v, err := in.ask(fmt.Sprintf(tr("Step 2/2 — type the tunnel name (%s)", "مرحلهٔ ۲/۲ — نام تونل را تایپ کنید (%s)"), u), "")
		if err != nil {
			v = ""
		}
		*name = v
	}
	if cleanUnit(*name) != u {
		fmt.Println(tr("Name does not match — not deleted.", "نام مطابقت ندارد — حذف نشد."))
		os.Exit(3)
	}
	dir, err := backupTunnel(u, path, time.Now())
	if err != nil {
		die(tr("backup failed (%v) — not deleted", "پشتیبان‌گیری ناموفق (%v) — حذف نشد"), err)
	}
	fmt.Println(tr("backup: ", "پشتیبان: ") + dir)
	realCreator{}.remove(u, path)
	restore := fmt.Sprintf("cp %s %s/ && cp %s %s/", filepath.Join(dir, filepath.Base(path)), filepath.Dir(path), filepath.Join(dir, u+".service"), systemdDir)
	if _, err := os.Stat(filepath.Join(dir, u+".service.d")); err == nil {
		restore += fmt.Sprintf(" && cp -r %s %s/", filepath.Join(dir, u+".service.d"), systemdDir)
	}
	restore += " && systemctl daemon-reload && systemctl enable --now " + u
	fmt.Printf(tr("%s deleted. Restore: %s\n", "%s حذف شد. بازگردانی: %s\n"), u, restore)
}

func backupTunnel(unit, cfgPath string, now time.Time) (string, error) {
	dir := filepath.Join(deletedRoot, unit+"-"+now.Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, f := range []string{cfgPath, filepath.Join(systemdDir, unit+".service")} {
		b, err := os.ReadFile(f)
		if err != nil {
			if f == cfgPath {
				return "", err
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o600); err != nil {
			return "", err
		}
	}
	if di := readDropins(unit); len(di) > 0 {
		dd := filepath.Join(dir, unit+".service.d")
		if err := os.MkdirAll(dd, 0o700); err != nil {
			return "", err
		}
		for n, c := range di {
			if err := os.WriteFile(filepath.Join(dd, filepath.Base(n)), []byte(c), 0o600); err != nil {
				return "", err
			}
		}
	}
	snapshotCurrent(unit, cfgPath, "pre-delete", now)
	return dir, nil
}

func tunnelPreflightCmd(args []string) {
	fs := flag.NewFlagSet("tunnel preflight", flag.ExitOnError)
	from := fs.String("from-json", "", "spec file")
	lang := langFlag(fs)
	fs.Parse(args)
	deepLang = *lang
	if *from == "" {
		die("usage: hs2 tunnel preflight --from-json spec.json")
	}
	b, err := os.ReadFile(*from)
	if err != nil {
		die("%v", err)
	}
	var s tunnelSpec
	if err := json.Unmarshal(b, &s); err != nil {
		die("%v", err)
	}
	h := realTunHost()
	if err := normalize(&s, h); err != nil {
		die("%v", err)
	}
	cfg, err := buildConfig(s, randHex(16))
	if err != nil {
		die("%v", err)
	}
	cs := preflight(s, cfg, configForUnit(s.Name), h)
	printChecks(cs)
	if preflightFailed(cs) {
		os.Exit(1)
	}
}

// ---- the host-changing part ------------------------------------------------

type realCreator struct{}

func unitFileText(unit, role, cfg string) string {
	return fmt.Sprintf(`[Unit]
Description=hs2 tunnel %s (%s)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=-/sbin/modprobe tun
ExecStart=%s run -c %s
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=3
TimeoutStopSec=8
KillMode=mixed
KillSignal=SIGTERM
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, unit, role, hs2BinPath, cfg)
}

func (realCreator) install(s tunnelSpec, cfgPath string, cfg []byte) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath+".tmp", cfg, 0o600); err != nil {
		return err
	}
	if err := os.Rename(cfgPath+".tmp", cfgPath); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(systemdDir, s.Name+".service"), []byte(unitFileText(s.Name, s.Role, cfgPath)), 0o644); err != nil {
		return err
	}
	exec.Command("systemctl", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "enable", "--now", s.Name).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %v %s", s.Name, err, out)
	}
	return nil
}

// healthy: active and not restarted for 6 s.
func (realCreator) healthy(unit string) bool {
	time.Sleep(6 * time.Second)
	st, n := realUnitInfo(unit)
	return st == "active" && n == 0
}

func (realCreator) remove(unit, cfgPath string) {
	exec.Command("systemctl", "disable", "--now", unit).Run()
	removeUnitFiles(unit, cfgPath)
	exec.Command("systemctl", "daemon-reload").Run()
	exec.Command("systemctl", "reset-failed", unit).Run()
}

// removeUnitFiles removes the tunnel's unit file, its drop-in directory
// (presets, toggles, CPUAffinity: backed up by backupTunnel), config and
// self-signed certificate.
func removeUnitFiles(unit, cfgPath string) {
	os.Remove(filepath.Join(systemdDir, unit+".service"))
	os.RemoveAll(filepath.Join(systemdDir, unit+".service.d"))
	os.Remove(cfgPath)
	os.Remove(filepath.Join(certDir, unit+".crt"))
	os.Remove(filepath.Join(certDir, unit+".key"))
}

func (realCreator) stats(on bool) {
	if !on {
		return
	}
	if err := statsTimer(true, systemdDir, selfBin(), realSystemctl); err != nil {
		fmt.Println(tr("note: could not turn on the 24h chart sampler (hs2-stats.timer): ", "توجه: نمونه‌بردار نمودار ۲۴ساعته (hs2-stats.timer) روشن نشد: ") + err.Error())
	} else {
		fmt.Println(tr("24h charts: hs2-stats.timer on (every 5 min).", "نمودار ۲۴ساعته: hs2-stats.timer روشن شد (هر ۵ دقیقه)."))
	}
}

func selfSignedCert(certPath, keyPath, domain, ip string) error {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	cn := domain
	if cn == "" {
		cn = "localhost"
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(2, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if domain != "" {
		tpl.DNSNames = []string{domain}
	}
	if p := net.ParseIP(ip); p != nil {
		tpl.IPAddresses = []net.IP{p}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(certPath), 0o700)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

// upgradeBinary installs src as the hs2 binary (after it answers `version`),
// keeps the old one as .prev, restarts unit and puts the old binary back if
// the tunnel does not stay up. The other tunnels pick the new binary up at
// their next restart.
func upgradeBinary(src, unit string) error {
	out, err := exec.Command(src, "version").Output()
	if err != nil || !strings.Contains(string(out), "hs2") {
		return fmt.Errorf("%s does not run as hs2 (%v) — not installed", src, err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	prev := hs2BinPath + ".prev"
	if old, err := os.ReadFile(hs2BinPath); err == nil {
		if err := os.WriteFile(prev, old, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(hs2BinPath+".new", b, 0o755); err != nil {
		return err
	}
	if err := os.Rename(hs2BinPath+".new", hs2BinPath); err != nil {
		return err
	}
	fmt.Println(tr("binary upgraded to: ", "باینری به‌روز شد به: ") + strings.TrimSpace(string(out)))
	exec.Command("systemctl", "restart", unit).Run()
	if (realCreator{}).healthy(unit) {
		return nil
	}
	if old, err := os.ReadFile(prev); err == nil {
		os.WriteFile(hs2BinPath+".new", old, 0o755)
		os.Rename(hs2BinPath+".new", hs2BinPath)
		exec.Command("systemctl", "restart", unit).Run()
	}
	return fmt.Errorf(tr("%s did not stay up on the new binary — the old binary was restored", "%s با باینری جدید بالا نماند — باینری قبلی برگردانده شد"), unit)
}
