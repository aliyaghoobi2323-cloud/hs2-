//go:build linux

package encap

import (
	"fmt"
	"log"
	"os"
	"os/exec"
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
// c2s magic: the kernel's copy of one of our requests starts its ICMP payload
// with that magic (ICMP offset 8), while hs2's own replies carry the s2c magic
// and ordinary pings carry neither. So normal ping keeps working. nft is tried
// first (a private table, removed as a whole on close), then iptables with the
// u32 match; only when neither works does it fall back to the global sysctl,
// with a log line saying so. HS2_ICMP_SUPPRESS=nft|iptables|global forces one.

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

func installEchoGuard(magic uint16) (method string, release func(), err error) {
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
			if err := acquireEchoIgnore(); err != nil {
				errs = append(errs, err.Error())
				return "", nil, fmt.Errorf("encap icmp: cannot stop the kernel answering the tunnel's echo requests (%s)", strings.Join(errs, "; "))
			}
			if len(try) > 1 {
				log.Printf("encap icmp: neither nft nor iptables worked (%s) — turned off ALL ping replies on this server while the tunnel runs (net.ipv4.icmp_echo_ignore_all=1); install nftables to keep normal ping working", strings.Join(errs, "; "))
			}
			return "global", releaseEchoIgnore, nil
		}
	}
	return "", nil, fmt.Errorf("encap icmp: cannot stop the kernel answering the tunnel's echo requests (%s)", strings.Join(errs, "; "))
}

// ---- nft ---------------------------------------------------------------------

func nftGuardTable(magic uint16) string { return fmt.Sprintf("hs2_icmp_%04x", magic) }

// nftGuardInstall (re)creates a private table whose output chain drops echo
// replies with magic at ICMP offset 8 (@th,64,16). Declaring then deleting the
// table first makes a leftover from a crashed run disappear in the same
// transaction instead of stacking a second rule.
func nftGuardInstall(magic uint16) error {
	if _, err := exec.LookPath("nft"); err != nil {
		return err
	}
	t := nftGuardTable(magic)
	rules := fmt.Sprintf("table inet %s\ndelete table inet %s\n"+
		"table inet %s {\n  chain out {\n    type filter hook output priority 0; policy accept;\n"+
		"    icmp type echo-reply @th,64,16 0x%04x drop\n  }\n}\n", t, t, t, magic)
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

// iptGuardArgs is the rule spec (after the -I/-D verb): an echo reply whose ICMP
// bytes 8..9 are magic. u32: 0>>22&0x3C = IP header length, @8 moves to ICMP
// offset 8, >>16 keeps its first two bytes.
func iptGuardArgs(verb string, magic uint16) []string {
	return []string{"-w", verb, "OUTPUT", "-p", "icmp", "--icmp-type", "echo-reply",
		"-m", "u32", "--u32", fmt.Sprintf("0>>22&0x3C@8>>16=0x%04x", magic),
		"-m", "comment", "--comment", fmt.Sprintf("hs2-icmp-%04x", magic), "-j", "DROP"}
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
