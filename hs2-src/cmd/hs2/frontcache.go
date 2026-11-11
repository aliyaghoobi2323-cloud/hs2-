package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Persisted peer fronts (rel44, the cold-dialer fix).
//
// A dialing side learns each listening IP's front (kind, public key, short
// ids, server names) from the lb announcement, which travels over a LINK.
// When every listening IP is pure "reality", a dialer that starts cold (fresh
// install, restart, reboot) has no key yet, every link it dials is refused by
// the reality front, so it never gets a link to hear the announcement on: the
// tunnel stays down unless the key is pinned in its config.
//
// So the dialer keeps the last announced public profiles on disk (written
// atomically, mode 0600, next to the config: <config>.peer-fronts.json) and
// loads them at start; "hs2 reality pin" writes the same file from a code the
// listening side prints ("hs2 reality front" / "hs2 reality pin-code"), for a
// dialer that never heard an announcement. Until the first announcement
// arrives, a remembered profile is dialed as "both" (reality first, tls at once
// on the same path if reality fails), so a remembered profile can never be
// worse than none: a listener that went back to tls is still reached. The
// first announcement replaces the remembered profiles entirely.

const peerFrontsVer = 1

// peerFronts is the file's content. Peer binds it to the tunnel's addr, so a
// config pointed at another server ignores it.
type peerFronts struct {
	Ver    int                                 `json:"ver"`
	Peer   string                              `json:"peer"`
	Saved  string                              `json:"saved,omitempty"`
	Source string                              `json:"source,omitempty"` // announcement | pin
	Fronts map[string]*tlscarrier.FrontProfile `json:"fronts"`
}

// peerFrontsPath is the remembered-fronts file of a config.
func peerFrontsPath(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	return strings.TrimSuffix(cfgPath, ".json") + ".peer-fronts.json"
}

// advertFronts: the non-default fronts of an announcement, by IP (public
// parts only).
func advertFronts(adv []engine.LBAdvertIP) map[string]*tlscarrier.FrontProfile {
	m := map[string]*tlscarrier.FrontProfile{}
	for _, a := range adv {
		ip := net.ParseIP(a.IP)
		if ip == nil {
			continue
		}
		if f := publicFront(a.Front); f != nil {
			m[ip.String()] = f
		}
	}
	return m
}

// loadPeerFronts reads path for peer (nil: none, unreadable or another peer).
func loadPeerFronts(path, peer string) (map[string]*tlscarrier.FrontProfile, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pf peerFronts
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if pf.Ver != peerFrontsVer {
		return nil, fmt.Errorf("%s: unknown version %d", path, pf.Ver)
	}
	if peer != "" && pf.Peer != "" && pf.Peer != peer {
		return nil, fmt.Errorf("%s: saved for %s, this tunnel dials %s — ignored", path, pf.Peer, peer)
	}
	out := map[string]*tlscarrier.FrontProfile{}
	for k, f := range pf.Fronts {
		ip := net.ParseIP(k)
		if ip == nil || f == nil {
			continue
		}
		if pub := publicFront(f); pub != nil && pub.PublicKey != "" {
			out[ip.String()] = pub
		}
	}
	return out, nil
}

// savePeerFronts writes m for peer atomically (temp file in the same dir,
// fsync, rename, fsync dir), mode 0600. An empty m removes the file.
func savePeerFronts(path, peer, source string, m map[string]*tlscarrier.FrontProfile) error {
	if path == "" {
		return nil
	}
	if len(m) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(peerFronts{Ver: peerFrontsVer, Peer: peer, Saved: time.Now().UTC().Format(time.RFC3339), Source: source, Fronts: m}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".peer-fronts-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// overlayPeerFronts gives every path without a front of its own the
// remembered profile of its remote IP, dialed as "both" (see above).
func overlayPeerFronts(paths []engine.LBPath, m map[string]*tlscarrier.FrontProfile) int {
	n := 0
	for i := range paths {
		if !paths[i].Front.IsDefault() {
			continue
		}
		ip := net.ParseIP(paths[i].Remote)
		if ip == nil {
			continue
		}
		f := m[ip.String()]
		if f == nil {
			continue
		}
		c := *f
		c.ServerNames = append([]string(nil), f.ServerNames...)
		c.ShortIDs = append([]string(nil), f.ShortIDs...)
		c.Kind = tlscarrier.FrontBoth
		paths[i].Front = &c
		n++
	}
	return n
}

// lbPeerKey identifies the other server a dialing config points at.
func lbPeerKey(fc fileConfig) string { return fc.Addr }

// ---- pin code: the listening side's public profiles, for the dialer -------

const pinCodePrefix = "hs2rp1."

// pinCode encodes the public profiles of fronts (by IP) for "hs2 reality pin".
func pinCode(m map[string]*tlscarrier.FrontProfile) string {
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return pinCodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// parsePinCode decodes a pin code (public profiles only; a private key in a
// hand-made code is dropped).
func parsePinCode(s string) (map[string]*tlscarrier.FrontProfile, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, pinCodePrefix) {
		return nil, errors.New("not an hs2 reality pin code (it starts with " + pinCodePrefix + ")")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, pinCodePrefix))
	if err != nil {
		return nil, fmt.Errorf("pin code: %v", err)
	}
	var raw map[string]*tlscarrier.FrontProfile
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("pin code: %v", err)
	}
	out := map[string]*tlscarrier.FrontProfile{}
	for k, f := range raw {
		ip := net.ParseIP(k)
		if ip == nil || f == nil {
			continue
		}
		pub := publicFront(f)
		if pub == nil || pub.PublicKey == "" {
			continue
		}
		if err := pub.Validate(); err != nil {
			return nil, fmt.Errorf("pin code: %s: %v", ip, err)
		}
		out[ip.String()] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("pin code: no reality profile in it")
	}
	return out, nil
}

// ownPublicFronts: the listening side's public profiles of its reality/both IPs.
func ownPublicFronts(fc fileConfig) map[string]*tlscarrier.FrontProfile {
	m := map[string]*tlscarrier.FrontProfile{}
	for _, e := range lbLocalList(fc) {
		ip := net.ParseIP(e.IP)
		if ip == nil {
			continue
		}
		if f := publicFront(e.Front); f != nil && f.PublicKey != "" {
			m[ip.String()] = f
		}
	}
	return m
}

// pureRealityIPs / tlsCapableIPs: the listening side's IPs by what a cold
// dialer (no key) can still reach.
func listenFrontSplit(fc fileConfig) (pure, tlsOK []string) {
	for _, e := range lbLocalList(fc) {
		ip := net.ParseIP(e.IP)
		if ip == nil {
			continue
		}
		k := tlscarrier.FrontTLS
		if !e.Front.IsDefault() {
			k = e.Front.NormKind()
		} else if f := tunnelFront(fc); f != nil {
			k = f.NormKind()
		}
		if k == tlscarrier.FrontReality {
			pure = append(pure, ip.String())
		} else {
			tlsOK = append(tlsOK, ip.String())
		}
	}
	sort.Strings(pure)
	sort.Strings(tlsOK)
	return
}

func frontsEqual(a, b map[string]*tlscarrier.FrontProfile) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok || !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// ---- commands --------------------------------------------------------------

// printPinHint prints, on the listening side, the command that pins its
// public profiles on the dialing server (needed only for a dialer that has
// never heard an announcement: a new install, or after a key rotation while
// the dialer was down).
func printPinHint(unit string, fc fileConfig) {
	code := pinCode(ownPublicFronts(fc))
	if code == "" {
		return
	}
	fmt.Println(tr("Cold-start insurance: the other server remembers the announced key on disk; to pin it now (new install, or it was down during the change) run there:",
		"بیمهٔ شروع سرد: سرور مقابل کلید اعلام‌شده را روی دیسک نگه می‌دارد؛ برای ثبت فوری آن (نصب تازه یا خاموش بودن هنگام تغییر) آنجا اجرا کنید:"))
	fmt.Printf("   hs2 reality pin -u %s %s\n", cleanUnit(unit), code)
}

// realityPinCodeCmd: hs2 reality pin-code -u U (listening side).
func realityPinCodeCmd(args []string) int {
	fs := flag.NewFlagSet("reality pin-code", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	lang := langFlag(fs)
	parseInterleaved(fs, args)
	setLang(*lang)
	_, fc, _ := loadUnit(*unit)
	if !tunnelListens(fc) {
		fmt.Println(tr("ERROR: this server dials the links: run pin-code on the LISTENING server, and pin here with hs2 reality pin",
			"خطا: این سرور لینک‌ها را شماره‌گیری می‌کند: pin-code را روی سرورِ شنونده اجرا کنید و اینجا با hs2 reality pin ثبت کنید"))
		return 1
	}
	code := pinCode(ownPublicFronts(fc))
	if code == "" {
		fmt.Println(tr("no reality/both IP: nothing to pin", "هیچ IP با reality/both نیست: چیزی برای ثبت نیست"))
		return 0
	}
	fmt.Printf("hs2 reality pin -u %s %s\n", cleanUnit(*unit), code)
	return 0
}

// realityPinCmd: hs2 reality pin -u U CODE (dialing side): remembers the
// listening side's public profiles exactly as an announcement would, then
// reloads the tunnel (a dialer waiting with no key picks them up at once).
func realityPinCmd(args []string) int {
	fs := flag.NewFlagSet("reality pin", flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	noReload := fs.Bool("no-reload", false, "only write the file")
	lang := langFlag(fs)
	pos := parseInterleaved(fs, args)
	setLang(*lang)
	if len(pos) != 1 {
		fmt.Println("usage: hs2 reality pin -u U " + pinCodePrefix + "… (the code from hs2 reality pin-code / front on the listening server)")
		return 2
	}
	path, fc, _ := loadUnit(*unit)
	if tunnelListens(fc) {
		fmt.Println(tr("ERROR: this server LISTENS for the links: pin on the dialing server (the code comes from here: hs2 reality pin-code)",
			"خطا: این سرور شنوندهٔ لینک‌هاست: روی سرور شماره‌گیر ثبت کنید (کد از اینجا می‌آید: hs2 reality pin-code)"))
		return 1
	}
	m, err := parsePinCode(pos[0])
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	fp := peerFrontsPath(path)
	if err := savePeerFronts(fp, lbPeerKey(fc), "pin", m); err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	ips := make([]string, 0, len(m))
	for ip := range m {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	fmt.Printf(tr("pinned %d front(s) (%s) in %s (0600); the next announcement replaces them\n", "%d front (%s) در %s (0600) ثبت شد؛ اعلام بعدی جایگزینشان می‌کند\n"), len(m), strings.Join(ips, ", "), fp)
	if !lbOn(fc) {
		fmt.Println(tr("WARN: this tunnel has no lb section: remembered fronts apply to lb paths only; set \"front\" with public_key in the config instead",
			"هشدار: این تونل بخش lb ندارد: frontهای ذخیره‌شده فقط روی مسیرهای lb اعمال می‌شوند؛ به جای آن \"front\" با public_key در پیکربندی بگذارید"))
	}
	if !*noReload && (realOps{}).active(cleanUnit(*unit)) {
		if err := (realOps{}).reload(cleanUnit(*unit)); err != nil {
			fmt.Println("WARN: " + err.Error())
		}
	}
	return 0
}

// checkColdDialer (doctor): a pure reality front only works for a dialer that
// has the key before its first link — pinned in its config, or remembered
// from an earlier announcement (or hs2 reality pin).
func checkColdDialer(d *doctorReport, fc fileConfig, cfgPath string) {
	const area = "reality cold start"
	if !lbOn(fc) {
		return
	}
	if tunnelListens(fc) {
		pure, tlsOK := listenFrontSplit(fc)
		switch {
		case len(pure) == 0:
		case len(tlsOK) == 0:
			d.warn(area, fmt.Sprintf(tr("every IP (%s) is pure 'reality': a dialing server without the key can never connect (no tls path to hear the announcement). Make sure the other server remembers it (hs2 doctor there: %q OK) or pin it there: hs2 reality pin-code -u U prints the command",
				"همهٔ IPها (%s) فقط 'reality' هستند: سرور شماره‌گیر بدون کلید هرگز وصل نمی‌شود (مسیر tls برای شنیدن اعلام نیست). مطمئن شوید سرور مقابل کلید را نگه داشته (hs2 doctor آنجا: %q OK) یا آنجا ثبتش کنید: hs2 reality pin-code -u U دستور را چاپ می‌کند"), strings.Join(pure, ", "), area))
		default:
			d.ok(area, fmt.Sprintf(tr("pure reality on %s; %s still answer tls, so a dialer without the key gets in and learns it", "reality خالص روی %s؛ %s هنوز tls پاسخ می‌دهند، پس شماره‌گیر بی‌کلید وارد می‌شود و کلید را یاد می‌گیرد"), strings.Join(pure, ", "), strings.Join(tlsOK, ", ")))
		}
		return
	}
	m, err := loadPeerFronts(peerFrontsPath(cfgPath), lbPeerKey(fc))
	if err != nil {
		d.warn(area, err.Error())
	}
	pinned := fc.Front != nil && fc.Front.PublicKey != ""
	_, remote, _, _ := lbLists(fc)
	for _, e := range remote {
		if e.Front != nil && e.Front.PublicKey != "" {
			pinned = true
		}
	}
	ls, _ := readLive(cfgPath)
	reality := false
	for _, p := range ls.FrontPaths {
		if p.Kind == tlscarrier.FrontReality {
			reality = true
		}
	}
	switch {
	case len(m) > 0:
		d.ok(area, fmt.Sprintf(tr("%d front profile(s) of the other server remembered in %s: a restart reaches its reality IPs at once", "%d پروفایل front سرور مقابل در %s نگه داشته شده: ری‌استارت فوراً به IPهای reality آن می‌رسد"), len(m), peerFrontsPath(cfgPath)))
	case pinned:
		d.ok(area, tr("reality public key pinned in the config", "کلید عمومی reality در پیکربندی ثابت شده"))
	case reality:
		d.warn(area, tr("a path uses pure 'reality' but its key is neither pinned nor remembered on disk: after a restart this server may not reach the other one. Run on the listening server: hs2 reality pin-code -u U, then here the printed hs2 reality pin command",
			"مسیری reality خالص دارد ولی کلیدش نه ثابت است نه روی دیسک: پس از ری‌استارت ممکن است به سرور مقابل نرسد. روی سرور شنونده: hs2 reality pin-code -u U، سپس اینجا دستور hs2 reality pin چاپ‌شده را اجرا کنید"))
	}
}
