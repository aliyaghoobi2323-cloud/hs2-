#!/usr/bin/env python3
"""Regression test: the panel inbound must work over a tun transported by TLS
(transport "tun" -> "tcp", carrier l3mtcp), in BOTH directions.

It used to be that this transport wrote neither "forward_ports" (iran) nor
"expose" (kharej), so no user port listened on iran and users never reached the
panel; and on a server where hs2.service had been masked, the installer printed
"running" while the new config never loaded. This test covers all of it:

  1. installer: drives the REAL interactive installer functions (sourced from
     install.sh, run under its own `set -euo pipefail`) through a pty from the
     Transport menu (tun -> tcp -> TLS mode) for the four branches
     (kharej/iran x direct/reverse) and both TLS modes (mtcp pool + tun =
     l3mtcp, one TLS link + tun = tls), with the setup link carried from one
     side to the other, and checks the configs they write — including that the
     pasting side picks the TLS mode up from the link.
  2. service: restart_unit/unit_unmask against a fake systemctl — a masked unit
     is unmasked, and a restart that did not happen is a failure, not "running".
  3. tunnel (root + network namespaces): runs the real hs2 binary with the
     configs from part 1 and moves real traffic from iran's user ports to a
     panel on kharej, direct and reverse, on two user ports each, per TLS mode
     and datagram encap (udp, ipx, gre — the raw ones with no tunnel port) —
     and runs the installer's own verify_tunnel on the dialing side.
  4. blocked path: gre with GRE dropped toward the kharej. Both services run,
     so the old installer said "ready"; verify_tunnel must fail and name the
     cause, and connect by itself once the path passes GRE again.

Parts 1-2 need only bash/python/openssl. Part 3 needs root and `ip netns`; it
builds hs2 and the lab probe with `go` (or takes HS2_BIN / PROBE_BIN).
Usage:  python3 install/tests/tun_ports_test.py      (exit 0 = all PASS)
"""
import json
import os
import pty
import re
import select
import shutil
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.abspath(os.path.join(HERE, "..", ".."))
# HS2_INSTALL_SH points the test at another installer (e.g. an old release, to
# show the test catches the bug); HS2_SKIP_TUNNEL=1 skips the namespace part.
INST = os.environ.get("HS2_INSTALL_SH") or os.path.join(SRC, "install", "install.sh")
FAILS = []


def res(name, ok, info=""):
    print(("PASS " if ok else "FAIL ") + name + (("  - " + info) if info else ""), flush=True)
    if not ok:
        FAILS.append(name)


class Pty:
    """A child `bash -c script` whose controlling terminal is a pty, so the
    installer's `read ... </dev/tty` prompts can be answered like a person."""

    def __init__(self, script, env):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execvpe("bash", ["bash", "-c", script], env)
        self.out, self.pos, self.eof = "", 0, False

    def _pull(self, timeout):
        if self.eof:
            return False
        r, _, _ = select.select([self.fd], [], [], timeout)
        if not r:
            return False
        try:
            d = os.read(self.fd, 65536)
        except OSError:
            d = b""
        if not d:
            self.eof = True
            return False
        self.out += d.decode("utf-8", "replace")
        return True

    def expect(self, rx, timeout=20):
        deadline = time.time() + timeout
        pat = re.compile(rx)
        while True:
            m = pat.search(self.out, self.pos)
            if m:
                self.pos = m.end()
                return True
            left = deadline - time.time()
            if left <= 0 or (self.eof and not self._pull(0)):
                return False
            self._pull(min(left, 0.5))

    def send(self, line):
        os.write(self.fd, (line + "\n").encode())

    def finish(self, timeout=30):
        deadline = time.time() + timeout
        while not self.eof and time.time() < deadline:
            self._pull(0.5)
        if not self.eof:
            # still blocked (typically on a prompt the test never answers): a
            # failing branch must end the run, not hang it
            try:
                os.kill(self.pid, 9)
            except ProcessLookupError:
                pass
        try:
            _, st = os.waitpid(self.pid, 0)
            return os.waitstatus_to_exitcode(st)
        except ChildProcessError:
            return -1
        finally:
            os.close(self.fd)


def make_lib(sb):
    """install.sh as a library: without the trailing `main_menu` call and the
    top-level root check (the functions under test need neither)."""
    lines = open(INST).read().rstrip("\n").split("\n")
    assert lines[-1].strip() == "main_menu", "install.sh no longer ends with main_menu"
    body = "\n".join(lines[:-1]) + "\n"
    body = re.sub(r'^\[ "\$\(id -u\)" = 0 \] \|\| die .*$', ":", body, flags=re.M)
    lib = os.path.join(sb, "lib.sh")
    open(lib, "w").write(body)
    return lib


# ---------------------------------------------------------------- part 1 -----
STUBS = r'''
source "$LIB"
trap - EXIT
CFG="$SB/$ROLE.json"; SVC="$SB/$ROLE.service"
local_ips(){ echo "$LABIP"; }
ip_is_local(){ [ "$1" = "$LABIP" ]; }
port_free(){ return 0; }; udp_port_free(){ return 0; }
get_cert(){ printf '%s|%s' "$SB/cert.pem" "$SB/key.pem"; }
write_service(){ :; }; start_service(){ echo "STARTED $1" >&2; }
show_link(){ encode_link "$1|$2|$3|$4|$5|$6|$7|$8|${9:-}|${10:-}|${11:-}" > "$SB/link.txt"; }
'''


def run_branch(lib, sb, role, labip, pre, call, steps):
    """Run one installer branch; steps = [(prompt regex, answer)]. The link a
    branch emits is left in $SB/link.txt; LINK in an answer is replaced by it."""
    env = dict(os.environ, LIB=lib, SB=sb, ROLE=role, LABIP=labip, TERM="dumb")
    p = Pty(STUBS + pre + "\n" + call + "\necho BRANCH_DONE\n", env)
    for rx, ans in steps:
        if not p.expect(rx):
            p.finish(5)
            return False, "no prompt /%s/ — output tail: %r" % (rx, p.out[-300:])
        if ans == "LINK":
            ans = "hs2://" + open(os.path.join(sb, "link.txt")).read().strip()
        p.send(ans)
    ok = p.expect(r"BRANCH_DONE", timeout=30)
    rc = p.finish()
    return ok and rc == 0, "" if ok else "branch did not finish — tail: %r" % p.out[-400:]


def cfg(sb, role):
    return json.load(open(os.path.join(sb, role + ".json")))


def part1(sb, lib, kh_ip, ir_ip, mode):
    """Generate all four tun+tcp configs through the real prompts, starting at
    the Transport menu: tun -> tcp -> TLS mode. mode is the TLS-mode menu
    choice ("1" = mtcp pool + tun = l3mtcp, "2" = one TLS link + tun = tls);
    the side that pastes the link must pick the same carrier up from it."""
    carrier = {"1": "l3mtcp", "2": "tls"}[mode]
    sfx = "_" + carrier
    ports = "8443,9443"
    panel = "127.0.0.1:18443"
    menu = [(r"Transport:[\s\S]*?Choose \[1\]: ", "4"),
            (r"cross the wire\?[\s\S]*?Choose \[1\]: ", "6"),
            (r"TLS mode for the tun:[\s\S]*?Choose \[1\]: ", mode)]
    tag = "[%s] " % carrier
    # direct: kharej listens and generates the link, iran pastes it.
    ok, why = run_branch(lib, sb, "kh_direct" + sfx, kh_ip, "DIRECTION=direct; PUBIP=%s" % kh_ip,
                         "ask_transport; kharej_listener",
                         menu + [(r"Tunnel port \(clients never see this\)", "2096"),
                                 (r"Panel inbound address on this server", panel),
                                 (r"Domain \(its A record", "test.local"),
                                 (r"TUN interface name", ""), (r"TUN MTU", ""),
                                 (r"Also forward UDP", "n")])
    res(tag + "installer: kharej direct tun/tcp asks for the panel inbound", ok, why)
    if ok:
        c = cfg(sb, "kh_direct" + sfx)
        res(tag + "installer: kharej direct tun/tcp writes expose", c.get("carrier") == carrier and c.get("expose") == panel
            and c.get("mode") == "listen" and c.get("reverse") is False, json.dumps(c)[:200])
        ok, why = run_branch(lib, sb, "ir_direct" + sfx, ir_ip, "", "iran_dialer",
                             [(r"Paste the hs2:// setup link", "LINK"),
                              (r"TUN interface name", ""),
                              (r"IP that USERS connect to", ""),
                              (r"User port\(s\) to open here", ports)])
        res(tag + "installer: iran direct tun/tcp asks for the user ports", ok, why)
        if ok:
            c = cfg(sb, "ir_direct" + sfx)
            res(tag + "installer: iran direct tun/tcp writes forward_ports (carrier from the link)", c.get("carrier") == carrier
                and c.get("forward_ports") == ports and c.get("mode") == "dial" and c.get("reverse") is False
                and c.get("addr") == "%s:2096" % kh_ip and c.get("udp") is False, json.dumps(c)[:200])

    # reverse: iran listens and generates the link, kharej pastes it.
    ok, why = run_branch(lib, sb, "ir_reverse" + sfx, ir_ip, "DIRECTION=reverse; PUBIP=%s" % ir_ip,
                         "ask_transport; iran_listener",
                         menu + [(r"Tunnel port to LISTEN on", "2082"),
                                 (r"IP that USERS connect to", ""),
                                 (r"User port\(s\) to open here", ports),
                                 (r"Domain for THIS iran server", "test.local"),
                                 (r"TUN interface name", ""), (r"TUN MTU", ""),
                                 (r"Also forward UDP", "y")])
    res(tag + "installer: iran reverse tun/tcp asks for the user ports", ok, why)
    if ok:
        c = cfg(sb, "ir_reverse" + sfx)
        res(tag + "installer: iran reverse tun/tcp writes forward_ports (+udp)", c.get("carrier") == carrier
            and c.get("forward_ports") == ports and c.get("reverse") is True and c.get("udp") is True,
            json.dumps(c)[:200])
        ok, why = run_branch(lib, sb, "kh_reverse" + sfx, kh_ip, "", "kharej_dialer",
                             [(r"Paste the hs2:// setup link", "LINK"),
                              (r"TUN interface name", ""),
                              (r"Panel inbound address on this server", panel)])
        res(tag + "installer: kharej reverse tun/tcp asks for the panel inbound", ok, why)
        if ok:
            c = cfg(sb, "kh_reverse" + sfx)
            res(tag + "installer: kharej reverse tun/tcp writes expose (carrier from the link)", c.get("carrier") == carrier
                and c.get("expose") == panel and c.get("reverse") is True
                and c.get("addr") == "%s:2082" % ir_ip, json.dumps(c)[:200])
    return carrier


def part1_dgtun(sb, lib, kh_ip, ir_ip, encap):
    """The datagram tun (transport tun -> udp/icmp/gre/ipip/ipx), all four
    branches through the real prompts. The user ports are asked ONCE, on iran;
    the kharej asks only the panel; the ipx protocol number is asked only where
    the link is made and travels in the link. Any extra prompt on the side that
    pastes the link (a port list, the ipx number) blocks the branch and fails."""
    tag = "dgtun-" + encap
    sfx = "_" + tag
    ports, panel = "8443,9443", "127.0.0.1:18443"
    # The raw encaps (icmp/gre/ipip/ipx) are bare IP protocols: no tunnel port
    # is asked (an unexpected prompt blocks the branch) and addr is the bare IP.
    raw = encap != "udp"
    kport = [] if raw else [(r"Tunnel port \(clients never see this\)", "2096")]
    iport = [] if raw else [(r"Tunnel port to LISTEN on", "2082")]
    kh_ep = kh_ip if raw else "%s:2096" % kh_ip
    ir_ep = ir_ip if raw else "%s:2082" % ir_ip
    choice = {"udp": "1", "icmp": "2", "gre": "3", "ipip": "4", "ipx": "5"}[encap]
    menu = [(r"Transport:[\s\S]*?Choose \[1\]: ", "4"),
            (r"cross the wire\?[\s\S]*?Choose \[1\]: ", choice)]
    if encap == "ipx":
        menu.append((r"IPX raw IP protocol number", "200"))
    want_proto = 200 if encap == "ipx" else None
    t = "[%s] " % tag

    def proto_ok(c):
        return c.get("proto") == want_proto if want_proto else "proto" not in c

    # direct: kharej listens and makes the link (asks the panel, never the ports).
    ok, why = run_branch(lib, sb, "kh_direct" + sfx, kh_ip, "DIRECTION=direct; PUBIP=%s" % kh_ip,
                         "ask_transport; kharej_listener",
                         menu + kport + [(r"TUN interface name", ""),
                                         (r"Panel inbound address on this server", panel)])
    res(t + "installer: kharej direct asks only the panel (no port list%s)" % (", no tunnel port" if raw else ""), ok, why)
    if ok:
        c = cfg(sb, "kh_direct" + sfx)
        res(t + "installer: kharej direct writes the panel and no port list", c.get("carrier") == "dgtun"
            and c.get("encap") == encap and c.get("expose") == panel and "forward_ports" not in c and proto_ok(c)
            and c.get("addr") == ("0.0.0.0" if raw else "0.0.0.0:2096"), json.dumps(c)[:220])
        ok, why = run_branch(lib, sb, "ir_direct" + sfx, ir_ip, "", "iran_dialer",
                             [(r"Paste the hs2:// setup link", "LINK"),
                              (r"TUN interface name", ""),
                              (r"IP that USERS connect to", ""),
                              (r"User port\(s\) to open here", ports)])
        res(t + "installer: iran direct asks the ports once (ipx number from the link)", ok, why)
        if ok:
            c = cfg(sb, "ir_direct" + sfx)
            res(t + "installer: iran direct writes the ports", c.get("encap") == encap
                and c.get("forward_ports") == ports and c.get("addr") == kh_ep and proto_ok(c),
                json.dumps(c)[:220])

    # reverse: iran listens and makes the link (asks the ports), kharej pastes it.
    ok, why = run_branch(lib, sb, "ir_reverse" + sfx, ir_ip, "DIRECTION=reverse; PUBIP=%s" % ir_ip,
                         "ask_transport; iran_listener",
                         menu + iport + [(r"TUN interface name", ""),
                                         (r"IP that USERS connect to", ""),
                                         (r"User port\(s\) to open here", ports)])
    res(t + "installer: iran reverse asks the ports", ok, why)
    if ok:
        c = cfg(sb, "ir_reverse" + sfx)
        res(t + "installer: iran reverse writes the ports", c.get("encap") == encap
            and c.get("forward_ports") == ports and c.get("reverse") is True and proto_ok(c), json.dumps(c)[:220])
        ok, why = run_branch(lib, sb, "kh_reverse" + sfx, kh_ip, "", "kharej_dialer",
                             [(r"Paste the hs2:// setup link", "LINK"),
                              (r"TUN interface name", ""),
                              (r"Panel inbound address on this server", panel)])
        res(t + "installer: kharej reverse asks only the panel (no port list, no ipx number)", ok, why)
        if ok:
            c = cfg(sb, "kh_reverse" + sfx)
            res(t + "installer: kharej reverse writes the panel, no port list, ipx number from the link",
                c.get("encap") == encap and c.get("expose") == panel and "forward_ports" not in c
                and c.get("addr") == ir_ep and proto_ok(c), json.dumps(c)[:220])
    return tag


# ---------------------------------------------------------------- part 2 -----
FAKE_SYSTEMCTL = r'''
source "$LIB"
trap - EXIT
sleep(){ :; }
st(){ cat "$SB/sc_$1"; }
put(){ printf '%s' "$2" > "$SB/sc_$1"; }
systemctl(){
  echo "$*" >> "$SB/sc_calls"
  case "$1" in
    is-enabled) st enabled ;;
    unmask) [ "$(st unmask_works)" = 1 ] && put enabled enabled; return 0 ;;
    daemon-reload|enable) return 0 ;;
    restart) case "$(st enabled)" in masked*) return 1 ;; esac
             [ "$(st restart_works)" = 1 ] && put pid $(( $(st pid) + 100 )); return 0 ;;
    show) case "$3" in MainPID) st pid ;; ActiveState) echo active ;; *) echo 0 ;; esac ;;
    is-active) echo active ;;
  esac
}
put enabled "$ENABLED"; put pid "$PID"; put unmask_works "$UNMASK"; put restart_works "$RESTART"
if restart_unit hs2; then echo RESULT=ok; else echo RESULT=fail; fi
echo "CALLS: $(tr '\n' ';' < "$SB/sc_calls")"
'''


def part2(sb, lib):
    def run(enabled, pid, unmask, restart):
        for f in os.listdir(sb):
            if f.startswith("sc_"):
                os.remove(os.path.join(sb, f))
        env = dict(os.environ, LIB=lib, SB=sb, ENABLED=enabled, PID=str(pid), UNMASK=str(unmask), RESTART=str(restart))
        p = subprocess.run(["bash", "-c", FAKE_SYSTEMCTL], env=env, capture_output=True, text=True, timeout=30)
        return p.stdout + p.stderr

    out = run("masked", 100, 1, 1)
    res("service: a masked unit is unmasked, then restarted with a new PID",
        "RESULT=ok" in out and "unmask hs2" in out and "MASKED" in out, out.strip()[-200:])
    out = run("masked", 100, 0, 1)
    res("service: a unit that stays masked is a failure (no false 'running')", "RESULT=fail" in out, out.strip()[-200:])
    out = run("enabled", 100, 1, 0)
    res("service: a restart that did not happen (same PID) is a failure", "RESULT=fail" in out, out.strip()[-200:])
    out = run("enabled", 0, 1, 1)
    res("service: a normal (re)start passes", "RESULT=ok" in out and "unmask" not in out, out.strip()[-200:])


# ---------------------------------------------------------------- part 3 -----
def sh(cmd, timeout=60, check=False):
    p = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=timeout)
    if check and p.returncode != 0:
        raise RuntimeError("%s: %s" % (cmd, p.stderr.strip()))
    return p


VERIFY = r'''
source "$LIB"
trap - EXIT
set +e
if [ "$NOPING" = 1 ]; then ping(){ return 1; }; fi
verify_tunnel "$VCFG" "$SECS"; rc=$?
[ $rc = 0 ] || tunnel_down_help "$VCFG"
echo "VERIFY_RC=$rc"
'''


def verify(ns, lib, cfgpath, secs, noping=False):
    """Run the installer's own verify_tunnel (and tunnel_down_help on failure)
    inside netns ns against a running tunnel. noping=True stubs ping out, so only
    the daemon's live-link count in its status file can prove the tunnel."""
    env = dict(os.environ, LIB=lib, VCFG=cfgpath, SECS=str(secs), NOPING="1" if noping else "0", TERM="dumb")
    p = subprocess.run(["ip", "netns", "exec", ns, "bash", "-c", VERIFY], env=env,
                       capture_output=True, text=True, timeout=secs + 30)
    out = p.stdout + p.stderr
    m = re.search(r"VERIFY_RC=(\d+)", out)
    return (int(m.group(1)) if m else -1), out


def part3(sb, lib, kh_ip, ir_ip, bins, carrier):
    hs2, probe = bins
    IR, KH = "tpir", "tpkh"
    procs = []

    def cleanup():
        for p in procs:
            p.terminate()
        for p in procs:
            try:
                p.wait(5)
            except subprocess.TimeoutExpired:
                p.kill()
        procs.clear()
        sh("ip netns del %s 2>/dev/null; ip netns del %s 2>/dev/null" % (IR, KH))

    def start(ns, args, log):
        f = open(os.path.join(sb, log), "w")
        p = subprocess.Popen(["ip", "netns", "exec", ns, "env", "HS2_NO_TUNE=1"] + args, stdout=f, stderr=subprocess.STDOUT)
        procs.append(p)
        return p

    sfx = "_" + carrier
    for label, kh_role, ir_role in (("%s direct" % carrier, "kh_direct" + sfx, "ir_direct" + sfx),
                                    ("%s reverse" % carrier, "kh_reverse" + sfx, "ir_reverse" + sfx)):
        cleanup()
        sh("ip netns add %s && ip netns add %s" % (IR, KH), check=True)
        sh("ip link add tpvi netns %s type veth peer name tpvk netns %s" % (IR, KH), check=True)
        sh("ip -n %s addr add %s/24 dev tpvi && ip -n %s addr add %s/24 dev tpvk" % (IR, ir_ip, KH, kh_ip), check=True)
        sh("for n in %s %s; do ip -n $n link set lo up; done; ip -n %s link set tpvi up; ip -n %s link set tpvk up"
           % (IR, KH, IR, KH), check=True)

        for ns, role in ((KH, kh_role), (IR, ir_role)):
            out = sh("ip netns exec %s %s check -c %s" % (ns, hs2, os.path.join(sb, role + ".json"))).stdout
            res("tunnel %s: `hs2 check` passes the generated %s config clean" % (label, role),
                "config OK" in out and "WARN" not in out and "ERROR" not in out, out.strip()[-300:])

        start(KH, [probe, "-server", "-listen", "127.0.0.1:18443"], "panel_%s.log" % label)
        start(KH, [hs2, "run", "-c", os.path.join(sb, kh_role + ".json")], "kh_%s.log" % label)
        time.sleep(0.5)
        start(IR, [hs2, "run", "-c", os.path.join(sb, ir_role + ".json")], "ir_%s.log" % label)

        up = False
        for _ in range(60):
            if sh("ip netns exec %s ping -c1 -W1 10.77.0.2" % IR, timeout=5).returncode == 0:
                up = True
                break
            time.sleep(0.5)
        res("tunnel %s: the L3 tun is up (iran pings kharej's tun IP)" % label, up)

        # The installer's end-to-end check on the side that pasted the link
        # (direct: iran dials; reverse: kharej dials) — and, with ping stubbed
        # out, from the daemon's live-link count alone.
        vns, vrole = (IR, ir_role) if "direct" in label else (KH, kh_role)
        vcfg = os.path.join(sb, vrole + ".json")
        rc, out = verify(vns, lib, vcfg, 20)
        res("tunnel %s: installer verify_tunnel on the dialing side says UP" % label,
            rc == 0 and "Tunnel is UP" in out, out.strip()[-300:])
        rc, out = verify(vns, lib, vcfg, 20, noping=True)
        res("tunnel %s: the status file's live links alone prove it (no ping)" % label,
            rc == 0, out.strip()[-300:])

        for port in ("8443", "9443"):
            p = sh("ip netns exec %s %s -addr 127.0.0.1:%s -bulk 2 -t 4s -warm 1s" % (IR, probe, port), timeout=60)
            try:
                r = json.loads(p.stdout.strip().splitlines()[-1])
            except (ValueError, IndexError):
                r = {}
            good = (r.get("mbps", 0) > 1 and r.get("bulk_errors", 1) == 0 and r.get("conn_fail", 1) == 0
                    and r.get("echo_n", 0) > 0 and r.get("echo_lost", 1) == 0)
            res("tunnel %s: a user on iran:%s reaches the kharej panel" % (label, port), good,
                "mbps=%.1f echo=%s/%s lost conn_fail=%s bulk_errors=%s" % (r.get("mbps", 0), r.get("echo_n"),
                                                                          r.get("echo_lost"), r.get("conn_fail"), r.get("bulk_errors"))
                if r else "probe failed: %s" % (p.stdout + p.stderr).strip()[-200:])
        if FAILS:
            for log in ("kh_%s.log" % label, "ir_%s.log" % label):
                print("---- " + log + " (tail)")
                print(open(os.path.join(sb, log)).read()[-1500:])
    cleanup()


def part_blocked(sb, lib, kh_ip, ir_ip, bins):
    """gre filtered on the path — the case the installer used to call "ready":
    both servers run, GRE never arrives. verify_tunnel must fail and say why;
    once the path opens the running service connects by itself (no re-install)."""
    hs2, _ = bins
    IR, KH = "tbir", "tbkh"
    kh_cfg, ir_cfg = (os.path.join(sb, r + "_dgtun-gre.json") for r in ("kh_direct", "ir_direct"))
    if not (os.path.exists(kh_cfg) and os.path.exists(ir_cfg)):
        res("blocked gre: configs from the installer part are available", False)
        return
    procs = []

    def cleanup():
        for p in procs:
            p.terminate()
        for p in procs:
            try:
                p.wait(5)
            except subprocess.TimeoutExpired:
                p.kill()
        sh("ip netns del %s 2>/dev/null; ip netns del %s 2>/dev/null" % (IR, KH))

    try:
        cleanup()
        sh("ip netns add %s && ip netns add %s" % (IR, KH), check=True)
        sh("ip link add tbvi netns %s type veth peer name tbvk netns %s" % (IR, KH), check=True)
        sh("ip -n %s addr add %s/24 dev tbvi && ip -n %s addr add %s/24 dev tbvk" % (IR, ir_ip, KH, kh_ip), check=True)
        sh("for n in %s %s; do ip -n $n link set lo up; done; ip -n %s link set tbvi up; ip -n %s link set tbvk up"
           % (IR, KH, IR, KH), check=True)
        # the path drops GRE (IP protocol 47) toward the kharej
        sh("ip netns exec %s iptables -w -I INPUT -p gre -j DROP" % KH, check=True)
        for ns, c, log in ((KH, kh_cfg, "blk_kh.log"), (IR, ir_cfg, "blk_ir.log")):
            f = open(os.path.join(sb, log), "w")
            procs.append(subprocess.Popen(["ip", "netns", "exec", ns, "env", "HS2_NO_TUNE=1", hs2, "run", "-c", c],
                                          stdout=f, stderr=subprocess.STDOUT))
            time.sleep(0.5)
        rc, out = verify(IR, lib, ir_cfg, 8)
        res("blocked gre: verify_tunnel fails although both services run", rc == 1, out.strip()[-300:])
        res("blocked gre: the failure names the cause (GRE filtered) and the way out",
            "did NOT connect" in out and "GRE (IP protocol 47)" in out and "udp, icmp, or tcp" in out,
            out.strip()[-400:])
        sh("ip netns exec %s iptables -w -D INPUT -p gre -j DROP" % KH, check=True)
        rc, out = verify(IR, lib, ir_cfg, 30)
        res("blocked gre: once the path passes GRE the running service connects by itself", rc == 0,
            out.strip()[-300:])
    finally:
        cleanup()


def build_bins(sb):
    hs2, probe = os.environ.get("HS2_BIN"), os.environ.get("PROBE_BIN")
    if not hs2:
        hs2 = os.path.join(sb, "hs2")
        subprocess.run(["go", "build", "-o", hs2, "./cmd/hs2"], cwd=SRC, check=True, env=dict(os.environ, CGO_ENABLED="0"))
    if not probe:
        probe = os.path.join(sb, "probe")
        subprocess.run(["go", "build", "-o", probe, "./lab/probe"], cwd=SRC, check=True)
    return hs2, probe


def main():
    sb = tempfile.mkdtemp(prefix="hs2tun_")
    try:
        # a self-signed cert stands in for Let's Encrypt (the carrier binds auth
        # to the shared key, not to a CA)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
                        "-keyout", os.path.join(sb, "key.pem"), "-out", os.path.join(sb, "cert.pem"), "-days", "30",
                        "-nodes", "-subj", "/CN=test.local", "-addext", "subjectAltName=DNS:test.local"],
                       check=True, capture_output=True)
        lib = make_lib(sb)
        kh_ip, ir_ip = "192.168.61.2", "192.168.61.1"
        carriers = [part1(sb, lib, kh_ip, ir_ip, mode) for mode in ("1", "2")]
        carriers += [part1_dgtun(sb, lib, kh_ip, ir_ip, encap) for encap in ("udp", "ipx", "gre")]
        part2(sb, lib)
        if os.environ.get("HS2_SKIP_TUNNEL") == "1":
            print("SKIP tunnel part (HS2_SKIP_TUNNEL=1)")
        elif os.geteuid() != 0 or shutil.which("ip") is None:
            print("SKIP tunnel part (needs root and iproute2)")
        else:
            bins = None
            for carrier in carriers:
                roles = [r + "_" + carrier for r in ("kh_direct", "ir_direct", "kh_reverse", "ir_reverse")]
                if not all(os.path.exists(os.path.join(sb, r + ".json")) for r in roles):
                    res("tunnel %s: configs from the installer part are available" % carrier, False)
                    continue
                bins = bins or build_bins(sb)
                part3(sb, lib, kh_ip, ir_ip, bins, carrier)
            bins = bins or build_bins(sb)
            part_blocked(sb, lib, kh_ip, ir_ip, bins)
    finally:
        shutil.rmtree(sb, ignore_errors=True)
    print("\n%s: %d failure(s)" % ("FAIL" if FAILS else "OK", len(FAILS)))
    return 1 if FAILS else 0


if __name__ == "__main__":
    sys.exit(main())
