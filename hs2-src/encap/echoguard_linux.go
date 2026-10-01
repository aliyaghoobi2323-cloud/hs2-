//go:build linux

package encap

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Keeping the kernel from answering the ICMP tunnel's echo requests — without
// silencing the whole server.
//
// An icmp listener receives the tunnel's echo REQUESTS on a raw socket. The
// kernel sees them too and would answer each with an echo reply carrying the
// same payload, bouncing every sealed datagram straight back to the peer. The
// blunt fix, net.ipv4.icmp_echo_ignore_all=1, also stops the server answering
// ordinary ping — its public IP and its tun IP go silent while the tunnel runs.
//
// Instead an OUTPUT rule drops only the echo replies that carry this tunnel's
// c2s prefix in the echo-sequence high byte (ICMP offset 6): the kernel's copy
// of one of our requests echoes the request's sequence, so it carries the c2s
// prefix, while hs2's own replies carry the s2c prefix and ordinary pings carry
// neither (the prefix byte is keyed and non-zero, so a short `ping -c N` — whose
// sequences stay in 1..255 with a zero high byte — is never caught). So normal
// ping keeps working. nft is tried first (a private table, removed as a whole on
// close), then iptables with the u32 match; only when neither works does it fall
// back to the global sysctl, with a log line. HS2_ICMP_SUPPRESS=nft|iptables|global forces one.
//
// The guard value is the c2s prefix BYTE (0..255). Every rule is named after it
// AND the owning process's PID (hs2_icmp_<byte>_<pid>, zero-padded to 4 hex), so
// a later daemon — or `hs2 cleanup` — can remove the rules of a daemon that died
// without cleaning up (SIGKILL, OOM, a power cut) without ever touching those of
// another tunnel that is still running.

type echoGuard struct {
	refs    int
	method  string
	release func()
}

var (
	guardMu sync.Mutex
	guards  = map[uint16]*echoGuard{}
)

// acquireEchoGuard takes one ICMP listener's hold on the reply suppression for
// magic (the c2s magic the listener receives), installing it on first use. Every
// successful call must be paired with exactly one releaseEchoGuard(magic).
func acquireEchoGuard(magic uint16) error {
	guardMu.Lock()
	defer guardMu.Unlock()
	if g := guards[magic]; g != nil {
		g.refs++
		return nil
	}
	method, release, err := installEchoGuard(magic)
	if err != nil {
		return err
	}
	guards[magic] = &echoGuard{refs: 1, method: method, release: release}
	return nil
}

// releaseEchoGuard drops one hold; the last one removes the rule (or restores
// the sysctl in the fallback).
func releaseEchoGuard(magic uint16) {
	guardMu.Lock()
	defer guardMu.Unlock()
	g := guards[magic]
	if g == nil {
		return
	}
	if g.refs--; g.refs == 0 {
		g.release()
		delete(guards, magic)
	}
}

// echoGuardMethod reports how magic's replies are being suppressed ("nft",
// "iptables", "global"), or "" when no listener holds it. For tests/diagnostics.
func echoGuardMethod(magic uint16) string {
	guardMu.Lock()
	defer guardMu.Unlock()
	if g := guards[magic]; g != nil {
		return g.method
	}
	return ""
}

// ReleaseAllEchoGuards removes every reply suppression this process holds,
// whatever its refcount. The daemon calls it on the way out (also on the
// forced-exit path), so no rule outlives its process.
func ReleaseAllEchoGuards() {
	guardMu.Lock()
	defer guardMu.Unlock()
	for m, g := range guards {
		g.release()
		delete(guards, m)
	}
}

var sweepOnce sync.Once

func installEchoGuard(magic uint16) (method string, release func(), err error) {
	// First install in this process: clear what dead daemons left behind.
	sweepOnce.Do(func() {
		for _, r := range SweepStaleEchoGuards(false) {
			log.Printf("encap icmp: removed a stale reply rule left by a stopped daemon: %s", r)
		}
	})
	want := strings.ToLower(strings.TrimSpace(os.Getenv("HS2_ICMP_SUPPRESS")))
	try := []string{"nft", "iptables", "global"}
	switch want {
	case "nft", "iptables", "global":
		try = []string{want}
	}
	var errs []string
	for _, m := range try {
		switch m {
		case "nft":
			if err := nftGuardInstall(magic); err == nil {
				return "nft", func() { nftGuardRemove(magic) }, nil
			} else {
				errs = append(errs, "nft: "+err.Error())
			}
		case "iptables":
			if err := iptGuardInstall(magic); err == nil {
				return "iptables", func() { iptGuardRemove(magic) }, nil
			} else {
				errs = append(errs, "iptables: "+err.Error())
			}
		case "global":
			if err := acquireEchoIgnoreMarked(); err != nil {
				errs = append(errs, err.Error())
				return "", nil, fmt.Errorf("encap icmp: cannot stop the kernel answering the tunnel's echo requests (%s)", strings.Join(errs, "; "))
			}
			if len(try) > 1 {
				log.Printf("encap icmp: neither nft nor iptables worked (%s) — turned off ALL ping replies on this server while the tunnel runs (net.ipv4.icmp_echo_ignore_all=1); install nftables to keep normal ping working", strings.Join(errs, "; "))
			}
			return "global", releaseEchoIgnoreMarked, nil
		}
	}
	return "", nil, fmt.Errorf("encap icmp: cannot stop the kernel answering the tunnel's echo requests (%s)", strings.Join(errs, "; "))
}

// ---- nft ---------------------------------------------------------------------

func nftGuardTable(magic uint16) string { return fmt.Sprintf("hs2_icmp_%04x_%d", magic, os.Getpid()) }

// nftGuardInstall (re)creates a private table whose output chain drops echo
// replies with the c2s prefix byte at ICMP offset 6, the echo-sequence high byte
// (@th,48,8). Declaring then deleting the table first makes a leftover from a
// crashed run disappear in the same transaction instead of stacking a second rule.
func nftGuardInstall(magic uint16) error {
	if _, err := exec.LookPath("nft"); err != nil {
		return err
	}
	t := nftGuardTable(magic)
	rules := fmt.Sprintf("table inet %s\ndelete table inet %s\n"+
		"table inet %s {\n  chain out {\n    type filter hook output priority 0; policy accept;\n"+
		"    icmp type echo-reply @th,48,8 0x%02x drop\n  }\n}\n", t, t, t, magic&0xff)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func nftGuardRemove(magic uint16) {
	exec.Command("nft", "delete", "table", "inet", nftGuardTable(magic)).Run()
}

// ---- iptables ----------------------------------------------------------------

func iptGuardTag(magic uint16) string { return fmt.Sprintf("hs2-icmp-%04x-%d", magic, os.Getpid()) }

// iptGuardArgs is the rule spec (after the -I/-D verb): an echo reply whose ICMP
// byte 6 (the echo-sequence high byte) is the c2s prefix. u32: 0>>22&0x3C = IP
// header length, @4 moves to ICMP offset 4 (id:2, seq:2), >>8&0xff keeps byte 6.
func iptGuardArgs(verb string, magic uint16) []string {
	return iptGuardArgsTag(verb, magic, iptGuardTag(magic))
}

func iptGuardArgsTag(verb string, magic uint16, tag string) []string {
	return []string{"-w", verb, "OUTPUT", "-p", "icmp", "--icmp-type", "echo-reply",
		"-m", "u32", "--u32", fmt.Sprintf("0>>22&0x3C@4>>8&0xff=0x%02x", magic&0xff),
		"-m", "comment", "--comment", tag, "-j", "DROP"}
}

func iptGuardInstall(magic uint16) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return err
	}
	iptGuardRemove(magic) // a leftover from a crashed run: never stack two
	if out, err := exec.Command("iptables", iptGuardArgs("-I", magic)...).CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func iptGuardRemove(magic uint16) {
	for i := 0; i < 8; i++ { // delete every copy
		if exec.Command("iptables", iptGuardArgs("-D", magic)...).Run() != nil {
			return
		}
	}
}

// ---- stale rules ---------------------------------------------------------------

// ownerAlive: pid is a running hs2 daemon. A recycled PID that belongs to
// something else does not keep a rule alive.
func ownerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return pid == os.Getpid() || IsHs2Daemon(pid)
}

// IsHs2Daemon: pid runs `hs2… run …` — an hs2 binary (whatever its file is
// called: hs2, or a lab build) in daemon mode.
func IsHs2Daemon(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return len(args) >= 2 && strings.HasPrefix(filepath.Base(args[0]), "hs2") && args[1] == "run"
}

// parseGuardName splits hs2_icmp_<magic>[_<pid>] / hs2-icmp-<magic>[-<pid>]
// (sep '_' or '-'). pid 0 = a PID-less name from an older binary.
func parseGuardName(name, prefix string, sep byte) (magic uint16, pid int, ok bool) {
	rest, found := strings.CutPrefix(name, prefix)
	if !found {
		return 0, 0, false
	}
	mh, ph, hasPid := strings.Cut(rest, string(sep))
	m, err := strconv.ParseUint(mh, 16, 16)
	if err != nil || len(mh) != 4 {
		return 0, 0, false
	}
	if !hasPid {
		return uint16(m), 0, true
	}
	p, err := strconv.Atoi(ph)
	if err != nil || p <= 0 {
		return 0, 0, false
	}
	return uint16(m), p, true
}

// SweepStaleEchoGuards removes the reply-suppression rules whose owner is
// gone — the nft tables and iptables rules named after a PID that is no longer
// a running hs2 — and returns what it removed. It never touches a rule whose
// owner still runs (another tunnel on this server). legacy also removes the
// PID-less names older binaries used; pass it only when no older hs2 binary can
// still be running (`hs2 cleanup` checks that). It also undoes the global
// icmp_echo_ignore_all fallback of a dead daemon when no live one needs it.
func SweepStaleEchoGuards(legacy bool) []string {
	var removed []string
	stale := func(pid int) bool { return (pid == 0 && legacy) || (pid != 0 && !ownerAlive(pid)) }
	if _, err := exec.LookPath("nft"); err == nil {
		out, _ := exec.Command("nft", "list", "tables", "inet").CombinedOutput()
		for _, ln := range strings.Split(string(out), "\n") {
			f := strings.Fields(ln)
			if len(f) != 3 || f[0] != "table" {
				continue
			}
			if _, pid, ok := parseGuardName(f[2], "hs2_icmp_", '_'); ok && stale(pid) {
				if exec.Command("nft", "delete", "table", "inet", f[2]).Run() == nil {
					removed = append(removed, "nft table "+f[2])
				}
			}
		}
	}
	if _, err := exec.LookPath("iptables"); err == nil {
		out, _ := exec.Command("iptables", "-w", "-S", "OUTPUT").CombinedOutput()
		for _, ln := range strings.Split(string(out), "\n") {
			f := strings.Fields(ln)
			for i := 0; i+1 < len(f); i++ {
				if f[i] != "--comment" {
					continue
				}
				tag := strings.Trim(f[i+1], `"`)
				if magic, pid, ok := parseGuardName(tag, "hs2-icmp-", '-'); ok && stale(pid) {
					if exec.Command("iptables", iptGuardArgsTag("-D", magic, tag)...).Run() == nil {
						removed = append(removed, "iptables rule "+tag)
					}
				}
			}
		}
	}
	if sweepEchoIgnoreMarkers() {
		removed = append(removed, "net.ipv4.icmp_echo_ignore_all back to 0 (a stopped daemon had turned it on)")
	}
	return removed
}

// ---- the global fallback, crash-safe ---------------------------------------------

// echoMarkDir holds one marker per daemon that turned icmp_echo_ignore_all on
// (the fallback when neither nft nor iptables works), named <pid>.<netns>. A
// daemon that dies with it on leaves its marker behind, and the next sweep in
// the same network namespace turns ping replies back on once no live daemon
// there holds one.
var echoMarkDir = "/run/hs2/icmp-echo-ignore"

func netnsID() string {
	l, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "0"
	}
	return strings.Trim(strings.TrimPrefix(l, "net:"), "[]")
}

func echoMarkPath() string {
	return filepath.Join(echoMarkDir, fmt.Sprintf("%d.%s", os.Getpid(), netnsID()))
}

func acquireEchoIgnoreMarked() error {
	if err := acquireEchoIgnore(); err != nil {
		return err
	}
	echoMu.Lock()
	weSet := echoWeSet
	echoMu.Unlock()
	if weSet {
		_ = os.MkdirAll(echoMarkDir, 0o755)
		_ = os.WriteFile(echoMarkPath(), nil, 0o644)
	}
	return nil
}

func releaseEchoIgnoreMarked() {
	releaseEchoIgnore()
	echoMu.Lock()
	refs := echoRefs
	echoMu.Unlock()
	if refs == 0 {
		_ = os.Remove(echoMarkPath())
	}
}

// sweepEchoIgnoreMarkers removes this namespace's markers of dead daemons; if
// there was one and no live daemon here still holds the setting, ping replies
// are turned back on. Reports whether it turned them on.
func sweepEchoIgnoreMarkers() bool {
	ents, err := os.ReadDir(echoMarkDir)
	if err != nil {
		return false
	}
	ns := netnsID()
	dead, live := 0, 0
	for _, e := range ents {
		pidS, nsS, ok := strings.Cut(e.Name(), ".")
		if !ok || nsS != ns {
			continue
		}
		pid, _ := strconv.Atoi(pidS)
		if ownerAlive(pid) {
			live++
			continue
		}
		dead++
		_ = os.Remove(filepath.Join(echoMarkDir, e.Name()))
	}
	if dead > 0 && live == 0 && EchoIgnored() {
		return os.WriteFile(echoIgnorePath, []byte("0\n"), 0o644) == nil
	}
	return false
}
