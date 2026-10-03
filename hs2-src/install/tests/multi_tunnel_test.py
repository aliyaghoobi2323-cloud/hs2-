#!/usr/bin/env python3
"""Named tunnels, end to end: several hs2 tunnels side by side on the same two
servers, each its own service — set up through the REAL installer prompts, run
by the REAL hs2 binary, managed through the real menu.

Two network namespaces are the two servers (kharej, iran). Each has its own
sandboxed /etc/hs2, unit directory, binary and backup folder. There is no
systemd in a container, so tests/fakesd.py stands in for systemctl and
journalctl — a small real supervisor: it runs the unit's ExecStart, restarts it
when it dies (Restart=always), stops it with SIGTERM/SIGKILL and reloads it with
SIGHUP. Everything else is the installer as users run it.

Scenarios (each checks what the user sees AND the real state behind it):
  1. default tunnel "hs2", direct, tun over udp — link made on kharej, pasted
     on iran, verified end to end, users reach the panel;
  2. "de1", reverse, tun over TLS (mtcp): the name and the tun subnet travel in
     the link; the second tunnel gets its own interface (hs1) and subnet; the
     first tunnel keeps running untouched (same PIDs);
  3. "x3", direct, tcp/mtcp — three tunnels at once, all carrying traffic;
  4. name collisions: reusing an existing name on the link-making side asks
     before replacing (answer no → pick another); on the pasting side an
     existing tunnel of that name that talks to another server is kept and the
     new one gets a free name (hs2-k4-2); tun over icmp has no port, and normal
     ping between the servers keeps working;
  5. a link whose subnet is already used on the pasting side is refused with a
     clear message and NOTHING changes there; delete a tunnel from the manager;
  6. setting the same tunnel up again (replace) on both sides keeps its subnet
     and interface, even when its transport changes; a re-setup aborted halfway
     puts the old tunnel back on its old config;
  7. manager restart / stop / start touch only that tunnel; a crashed tunnel
     is restarted by its service;
  8. upgrade restarts every tunnel and checks each reconnects;
  9. backup → delete → restore brings the tunnel back working;
 9b. deleting a tcp/mtcp tunnel (no tun device) never removes the interface
     another tunnel really uses;
 10. status shows every tunnel; uninstall removes all of them, their processes
     and interfaces;
 11. an older 11-field link still parses (default name, 10.77.0.0/30).

Needs root, iproute2, go (or HS2_BIN / PROBE_BIN). Usage:
  python3 install/tests/multi_tunnel_test.py      (exit 0 = all PASS)
"""
import base64
import json
import os
import pty
import re
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.abspath(os.path.join(HERE, "..", ".."))
INST = os.environ.get("HS2_INSTALL_SH") or os.path.join(SRC, "install", "install.sh")
FAKESD = os.path.join(HERE, "fakesd.py")
FAILS = []
ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")

NS = {"kh": "mtkh", "ir": "mtir"}
IP = {"kh": "192.168.71.2", "ir": "192.168.71.1"}
PANEL = "127.0.0.1:18443"


def res(name, ok, info=""):
    print(("PASS " if ok else "FAIL ") + name + (("  - " + info) if info and not ok else ""), flush=True)
    if not ok:
        FAILS.append(name)
    return ok


def sh(cmd, timeout=60, env=None):
    return subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=timeout, env=env)


class Pty:
    """A child process on a pty, so the installer's `read … </dev/tty` prompts
    are answered like a person types them."""

    def __init__(self, argv, env):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execvpe(argv[0], argv, env)
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
        self.out += ANSI.sub("", d.decode("utf-8", "replace")).replace("\r", "")
        return True

    def expect(self, rx, timeout, auto=()):
        """Wait for rx. auto is [(regex, answer)] for optional prompts the steps
        do not list: one that shows up BEFORE rx is answered on the way."""
        deadline = time.time() + timeout
        pat = re.compile(rx)
        autos = [(re.compile(a), ans) for a, ans in auto]
        while True:
            m = pat.search(self.out, self.pos)
            first = None
            for apat, ans in autos:
                am = apat.search(self.out, self.pos)
                if am and (m is None or am.start() < m.start()) and (first is None or am.start() < first[0].start()):
                    first = (am, ans)
            if first:
                self.pos = first[0].end()
                self.send(first[1])
                continue
            if m:
                self.pos = m.end()
                return True
            left = deadline - time.time()
            if left <= 0 or (self.eof and not self._pull(0)):
                return False
            self._pull(min(left, 0.5))

    def send(self, line):
        os.write(self.fd, (line + "\n").encode())

    def finish(self, timeout):
        deadline = time.time() + timeout
        while not self.eof and time.time() < deadline:
            self._pull(0.5)
        if not self.eof:
            try:
                os.kill(self.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        try:
            _, st = os.waitpid(self.pid, 0)
            rc = os.waitstatus_to_exitcode(st)
        except ChildProcessError:
            rc = -1
        os.close(self.fd)
        return rc


# ------------------------------------------------------------------ sandbox --
class World:
    def __init__(self, sb, hs2, probe):
        self.sb, self.hs2, self.probe = sb, hs2, probe
        self.lib = os.path.join(sb, "lib.sh")
        lines = open(INST).read().rstrip("\n").split("\n")
        assert lines[-1].strip() == "main_menu", "install.sh no longer ends with main_menu"
        body = "\n".join(lines[:-1]) + "\n"
        body = re.sub(r'^\[ "\$\(id -u\)" = 0 \] \|\| die .*$', ":", body, flags=re.M)
        open(self.lib, "w").write(body)
        self.fakebin = os.path.join(sb, "fakebin")
        os.makedirs(self.fakebin)
        for n in ("systemctl", "journalctl"):
            os.symlink(FAKESD, os.path.join(self.fakebin, n))
        self.cert, self.key = os.path.join(sb, "cert.pem"), os.path.join(sb, "key.pem")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
                        "-keyout", self.key, "-out", self.cert, "-days", "30", "-nodes", "-subj", "/CN=test.local",
                        "-addext", "subjectAltName=DNS:test.local"], check=True, capture_output=True)
        for s in NS:
            r = self.root(s)
            for d in ("etc/hs2", "etc/systemd/system", "usr/local/bin", "root/hs2-backups", "sd"):
                os.makedirs(os.path.join(r, d), exist_ok=True)
            shutil.copy(hs2, os.path.join(r, "usr/local/bin/hs2"))

    def root(self, s):
        return os.path.join(self.sb, s)

    def env(self, s, extra=None):
        r = self.root(s)
        e = dict(os.environ, LIB=self.lib, ROOT=r, TEST_CERT=self.cert, TEST_KEY=self.key, TERM="dumb",
                 PATH=self.fakebin + ":" + os.environ["PATH"], FAKESD_UNITS=os.path.join(r, "etc/systemd/system"),
                 FAKESD_STATE=os.path.join(r, "sd"), HS2_NO_TUNE="1", HS2_VERIFY_SECS="40")
        e.update(extra or {})
        return e

    PRELUDE = r'''
source "$LIB"
R="$ROOT"
BIN="$R/usr/local/bin/hs2"; CFG_DIR="$R/etc/hs2"; UNIT_DIR="$R/etc/systemd/system"
BACKUP_DIR="$R/root/hs2-backups"; MENU_BIN="$R/usr/local/bin/hs2-menu"
use_unit hs2
install_prereqs(){ :; }
install_binary(){ ok "(test) hs2 binary in place"; }
get_cert(){ printf "%s|%s" "$TEST_CERT" "$TEST_KEY"; }
'''

    def run(self, s, call, steps, timeout=120, extra=None):
        """Run installer function(s) on server s through a pty. steps are
        (prompt regex, answer) — answer may be a function of the output so far.
        Returns (rc, output)."""
        script = self.PRELUDE + call + "\n"
        p = Pty(["ip", "netns", "exec", NS[s], "bash", "-c", script], self.env(s, extra))
        for rx, ans in steps:
            if not p.expect(rx, 60, auto=OPTIONAL):
                p.finish(3)
                return -99, p.out + "\n<<no prompt /%s/>>" % rx
            if callable(ans):
                ans = ans(p.out)
            p.send(ans)
        rc = p.finish(timeout)
        return rc, p.out

    # -------------------------------------------------------------- state --
    def sd(self, s, *args):
        return sh(" ".join([FAKESD, "systemctl"] + list(args)), env=self.env(s)).stdout.strip()

    def units(self, s):
        d = os.path.join(self.root(s), "etc/systemd/system")
        return sorted(f[:-8] for f in os.listdir(d) if f.startswith("hs2") and f.endswith(".service"))

    def pid(self, s, u):
        v = self.sd(s, "show", "-p", "MainPID", "--value", u)
        return int(v) if v.isdigit() else 0

    def pids(self, s):
        return {u: self.pid(s, u) for u in self.units(s)}

    def active(self, s, u):
        return self.sd(s, "is-active", u) == "active"

    def cfg(self, s, u):
        name = "config.json" if u == "hs2" else u + ".json"
        try:
            return json.load(open(os.path.join(self.root(s), "etc/hs2", name)))
        except (OSError, ValueError):
            return None

    def backups(self, s):
        d = os.path.join(self.root(s), "root/hs2-backups")
        return sorted(f for f in os.listdir(d) if f.endswith(".tar.gz"))

    def iface_up(self, s, ifc):
        return sh("ip netns exec %s ip link show %s" % (NS[s], ifc)).returncode == 0

    def traffic(self, port):
        p = sh("ip netns exec %s %s -addr 127.0.0.1:%s -bulk 1 -t 2s -warm 500ms" % (NS["ir"], self.probe, port), timeout=60)
        try:
            r = json.loads(p.stdout.strip().splitlines()[-1])
        except (ValueError, IndexError):
            return False, (p.stdout + p.stderr)[-200:]
        good = r.get("mbps", 0) > 0.5 and r.get("conn_fail", 1) == 0 and r.get("echo_n", 0) > 0 and r.get("echo_lost", 1) == 0
        return good, "mbps=%.1f echo=%s lost=%s conn_fail=%s" % (r.get("mbps", 0), r.get("echo_n"), r.get("echo_lost"), r.get("conn_fail"))


def link_of(out):
    m = re.findall(r"hs2://([A-Za-z0-9+/=]+)", out)
    return "hs2://" + m[-1] if m else ""


def link_fields(link):
    return base64.b64decode(link[len("hs2://"):]).decode().split("|")


def block(cidr):
    ip = (cidr or "").split("/")[0]
    if not ip:
        return ""
    a, b, c, d = ip.split(".")
    return "%s.%s.%s.%d" % (a, b, c, int(d) // 4 * 4)


def menu_pick(name):
    """Answer for the tunnel list: the number printed next to name."""
    def f(out):
        tail = out[out.rfind("Tunnel manager"):]
        m = re.search(r"(\d+)\) %s\b" % re.escape(name), tail)
        return m.group(1) if m else "0"
    return f


# prompt regexes (what the user sees)
DIR = r"Direction \(who starts[\s\S]*?Choose \[1\]: "
NAME = r"Service name for this tunnel \[hs2\]"
PUB_K = r"Public IP of THIS kharej server"
PUB_I = r"Public IP of THIS iran server"
TRANSPORT = r"Transport:[\s\S]*?Choose \[1\]: "
ENCAP = r"cross the wire\?[\s\S]*?Choose \[1\]: "
TLS_TUN = r"TLS mode for the tun:[\s\S]*?Choose \[1\]: "
TLS_TCP = r"TLS mode:  1\) mtcp[\s\S]*?Choose \[1\]: "
TPORT_K = r"Tunnel port \(clients never see this\)"
TPORT_I = r"Tunnel port to LISTEN on"
IFACE = r"TUN interface name on THIS server \[\w+\]"
PANEL_Q = r"Panel inbound address on this server"
PASTE = r"Paste the hs2:// setup link"
USERIP = r"IP that USERS connect to"
PORTS = r"User port\(s\) to open here"
DOM_K = r"Domain \(its A record"
DOM_I = r"Domain for THIS iran server"
MTU = r"TUN MTU"
UDPQ = r"Also forward UDP"
LIST = r"Pick a tunnel number[\s\S]*?Choose: "
# Optional prompts the scenarios do not list, answered with Enter (keep the
# default) wherever they appear: the link-making side is asked the tunnel
# subnet (ask_subnet, U5) right after the transport.
OPTIONAL = [(r"Tunnel subnet /30 base in 10\.77\.0\.0/16 \[[^\]]*\]: ", "")]
TMENU = r"9\) Delete this tunnel[\s\S]*?Choose: "


def scenarios(w):
    kh, ir = "kh", "ir"

    # ---- 1. default tunnel, direct, tun/udp ---------------------------------
    rc, out = w.run(kh, "setup_kharej", [(DIR, "1"), (NAME, ""), (PUB_K, ""), (TRANSPORT, "4"), (ENCAP, "1"),
                                         (TPORT_K, "2096"), (IFACE, ""), (PANEL_Q, PANEL)])
    l1 = link_of(out)
    c = w.cfg(kh, "hs2")
    res("1 kharej: default name hs2 → service hs2, config /etc/hs2/config.json", rc == 0 and w.units(kh) == ["hs2"]
        and c is not None and w.active(kh, "hs2"), out[-400:])
    res("1 kharej: first side says it is waiting, not ready", "waits for the Iran server" in out and "KHAREJ ready" not in out, out[-300:])
    f = link_fields(l1) if l1 else []
    res("1 link carries the service name and the tunnel subnet", len(f) == 13 and f[11] == "hs2"
        and c and block(c.get("local_cidr")) == f[12] and f[12] != "10.77.0.0", str(f[11:]) if f else out[-200:])
    rc, out = w.run(ir, "setup_iran", [(DIR, "1"), (PASTE, l1), (IFACE, ""), (USERIP, ""), (PORTS, "8443")])
    ci = w.cfg(ir, "hs2")
    res("1 iran: name from the link, verified UP, then ready", rc == 0 and "Service name from the link: hs2" in out
        and "Tunnel is UP" in out and "IRAN ready" in out, out[-500:])
    res("1 both sides on the link's subnet, complementary addresses", ci and c and block(ci["local_cidr"]) == block(c["local_cidr"])
        and ci["peer_ip"] == c["local_cidr"].split("/")[0] and c["peer_ip"] == ci["local_cidr"].split("/")[0],
        "%s vs %s" % (ci and ci.get("local_cidr"), c and c.get("local_cidr")))
    ok, info = w.traffic(8443)
    res("1 users on iran:8443 reach the kharej panel", ok, info)

    # ---- 2. "de1", reverse, tun over TLS (mtcp) ------------------------------
    before = (w.pids(kh), w.pids(ir))
    rc, out = w.run(ir, "setup_iran", [(DIR, "2"), (NAME, "de1"), (PUB_I, ""), (TRANSPORT, "4"), (ENCAP, "6"),
                                       (TLS_TUN, "1"), (TPORT_I, "2097"), (USERIP, ""), (PORTS, "9443"),
                                       (DOM_I, "test.local"), (IFACE, ""), (MTU, ""), (UDPQ, "n")])
    l2 = link_of(out)
    ci2 = w.cfg(ir, "hs2-de1")
    res("2 iran: name de1 → service hs2-de1, config hs2-de1.json", rc == 0 and "hs2-de1" in w.units(ir) and ci2 is not None
        and w.active(ir, "hs2-de1"), out[-400:])
    res("2 iran: the second tunnel defaults to its own interface hs1", "TUN interface name on THIS server [hs1]" in out
        and ci2 and ci2.get("iface") == "hs1", ci2 and ci2.get("iface"))
    res("2 iran: the tun IP of tunnel 1 is not offered as a server IP", "10.77." not in out.split("Public IP of THIS iran server")[0].split("This server's IP addresses:")[-1],
        out[:600])
    rc, out = w.run(kh, "setup_kharej", [(DIR, "2"), (PASTE, l2), (IFACE, ""), (PANEL_Q, PANEL)])
    ck2 = w.cfg(kh, "hs2-de1")
    res("2 kharej: pasting the link makes service hs2-de1 too, verified UP", rc == 0 and "Service name from the link: hs2-de1" in out
        and "Tunnel is UP" in out and ck2 is not None, out[-500:])
    res("2 the subnet from the link, and it differs from tunnel 1", ck2 and ci2 and block(ck2["local_cidr"]) == block(ci2["local_cidr"])
        and block(ck2["local_cidr"]) != block(c["local_cidr"]), "%s %s" % (ck2 and ck2.get("local_cidr"), ci2 and ci2.get("local_cidr")))
    res("2 tunnel 1 was not touched (same PIDs on both servers)", w.pids(kh)["hs2"] == before[0]["hs2"] and w.pids(ir)["hs2"] == before[1]["hs2"])
    for port in ("8443", "9443"):
        ok, info = w.traffic(port)
        res("2 both tunnels carry traffic: iran:%s" % port, ok, info)

    # ---- 3. "x3", direct, tcp/mtcp -------------------------------------------
    rc, out = w.run(kh, "setup_kharej", [(DIR, "1"), (NAME, "x3"), (PUB_K, ""), (TRANSPORT, "3"), (TPORT_K, "2098"),
                                         (PANEL_Q, PANEL), (DOM_K, "test.local"), (TLS_TCP, "1"), (UDPQ, "n")])
    l3 = link_of(out)
    rc2, out2 = w.run(ir, "setup_iran", [(DIR, "1"), (PASTE, l3), (USERIP, ""), (PORTS, "7443")])
    res("3 x3 (tcp/mtcp): made on kharej, pasted on iran, verified UP", rc == 0 and rc2 == 0 and "Tunnel is UP" in out2
        and "hs2-x3" in w.units(kh) and "hs2-x3" in w.units(ir), out2[-400:])
    for port in ("8443", "9443", "7443"):
        ok, info = w.traffic(port)
        res("3 three tunnels side by side: iran:%s reaches the panel" % port, ok, info)

    # ---- 4. collisions + icmp ------------------------------------------------
    rc, out = w.run(ir, "setup_iran", [(DIR, "2"), (NAME, "k4"), (PUB_I, ""), (TRANSPORT, "4"), (ENCAP, "1"),
                                       (TPORT_I, "2099"), (IFACE, ""), (USERIP, ""), (PORTS, "6443")])
    res("4 iran has its own hs2-k4 (a listener no kharej ever joins)", rc == 0 and "hs2-k4" in w.units(ir), out[-300:])
    de1_pid = w.pid(kh, "hs2-de1")
    rc, out = w.run(kh, "setup_kharej", [(DIR, "1"), (NAME, "de1"), (r"Replace it with this new tunnel\? \[y/N\]", "n"),
                                         (NAME, "k4"), (PUB_K, ""), (TRANSPORT, "4"), (ENCAP, "2"), (IFACE, ""), (PANEL_Q, PANEL)])
    l4 = link_of(out)
    ck4 = w.cfg(kh, "hs2-k4")
    res("4 kharej: an existing name is not replaced without a yes (answered no → picked k4)",
        rc == 0 and "already exists here" in out and w.pid(kh, "hs2-de1") == de1_pid and ck4 is not None, out[-400:])
    res("4 kharej: tun over icmp asks no tunnel port; addr is the bare IP", ck4 and ck4.get("addr") == "0.0.0.0"
        and "Tunnel port" not in out.split("cross the wire?")[-1], ck4 and ck4.get("addr"))
    res("4 kharej: third tun interface defaults to hs2", ck4 and ck4.get("iface") == "hs2", ck4 and ck4.get("iface"))
    rc, out = w.run(ir, "setup_iran", [(DIR, "1"), (PASTE, l4),
                                       (r"Keep it, and run this one under another name[\s\S]*?Choose \[2\]: ", ""),
                                       (r"Service name for this tunnel here \[hs2-k4-2\]", ""),
                                       (IFACE, ""), (USERIP, ""), (PORTS, "5443")])
    res("4 iran: same name, other server → kept hs2-k4, new one runs as hs2-k4-2, UP",
        rc == 0 and "hs2-k4" in w.units(ir) and "hs2-k4-2" in w.units(ir) and "Tunnel is UP" in out, out[-500:])
    ok, info = w.traffic(5443)
    res("4 the icmp tunnel carries traffic (iran:5443)", ok, info)
    res("4 normal ping between the servers still works with an icmp tunnel up",
        sh("ip netns exec %s ping -c2 -W1 %s" % (NS[ir], IP[kh])).returncode == 0
        and sh("ip netns exec %s ping -c2 -W1 %s" % (NS[kh], IP[ir])).returncode == 0)

    # ---- 5. subnet collision on the pasting side; delete from the manager ----
    rc, out = w.run(kh, "setup_kharej", [(DIR, "1"), (NAME, "sc"), (PUB_K, ""), (TRANSPORT, "4"), (ENCAP, "1"),
                                         (TPORT_K, "2100"), (IFACE, ""), (PANEL_Q, PANEL)])
    f = link_fields(link_of(out))
    f[12] = block(w.cfg(ir, "hs2")["local_cidr"])       # iran's tunnel 1 subnet
    bad = "hs2://" + base64.b64encode("|".join(f).encode()).decode()
    before_units, before_pids = w.units(ir), w.pids(ir)
    rc, out = w.run(ir, "setup_iran", [(DIR, "1"), (PASTE, bad)])
    res("5 iran: a link on a subnet already used here is refused, naming the tunnel",
        rc != 0 and "already used here by tunnel hs2" in out and "Nothing was changed here" in out, out[-400:])
    res("5 iran: nothing changed (same tunnels, same PIDs)", w.units(ir) == before_units and w.pids(ir) == before_pids)
    nb = len(w.backups(kh))
    pk = {u: p for u, p in w.pids(kh).items() if u != "hs2-sc"}
    rc, out = w.run(kh, "tunnel_manager", [(LIST, menu_pick("hs2-sc")), (TMENU, "9"),
                                           (r"Type the service name to delete it \(hs2-sc\)", "hs2-sc"),
                                           (r"Press Enter to continue", ""), (LIST, "0")])
    res("5 manager delete: hs2-sc gone (service, config)", rc == 0 and "hs2-sc" not in w.units(kh) and w.cfg(kh, "hs2-sc") is None
        and "hs2-sc deleted" in out, out[-400:])
    res("5 manager delete: a backup was saved first", len(w.backups(kh)) == nb + 1)
    res("5 manager delete: the other tunnels kept running (same PIDs)", {u: w.pid(kh, u) for u in pk} == pk)
    rc, out = w.run(kh, "tunnel_manager", [(LIST, menu_pick("hs2-x3")), (TMENU, "9"),
                                           (r"Type the service name to delete it \(hs2-x3\)", "nope"),
                                           (TMENU, "0"), (LIST, "0")])
    res("5 manager delete: a wrong name cancels", "Not deleted." in out and "hs2-x3" in w.units(kh) and w.active(kh, "hs2-x3"), out[-300:])

    # ---- 6. replace the same tunnel on both sides ---------------------------
    old_blk, old_if = block(w.cfg(ir, "hs2-de1")["local_cidr"]), w.cfg(ir, "hs2-de1")["iface"]
    others_ir = {u: p for u, p in w.pids(ir).items() if u != "hs2-de1"}
    rc, out = w.run(ir, "setup_iran", [(DIR, "2"), (NAME, "de1"), (r"Replace it with this new tunnel\? \[y/N\]", "y"),
                                       (PUB_I, ""), (TRANSPORT, "4"), (ENCAP, "1"), (TPORT_I, "2097"),
                                       (IFACE, ""), (USERIP, ""), (PORTS, "9443")])
    l6 = link_of(out)
    c6 = w.cfg(ir, "hs2-de1")
    res("6 iran: replacing de1 (now tun/udp) keeps its subnet and interface", rc == 0 and c6 and c6.get("carrier") == "dgtun"
        and block(c6["local_cidr"]) == old_blk and c6["iface"] == old_if and link_fields(l6)[12] == old_blk, out[-400:])
    rc, out = w.run(kh, "setup_kharej", [(DIR, "2"), (PASTE, l6),
                                         (r"same tunnel set up again[\s\S]*?Replace it with this one[\s\S]*?Choose \[1\]: ", ""),
                                         (IFACE, ""), (PANEL_Q, PANEL)])
    res("6 kharej: same name + same server → replace is the default, verified UP", rc == 0 and "Tunnel is UP" in out
        and w.cfg(kh, "hs2-de1")["carrier"] == "dgtun" and block(w.cfg(kh, "hs2-de1")["local_cidr"]) == old_blk, out[-400:])
    ok, info = w.traffic(9443)
    res("6 the replaced tunnel carries traffic", ok, info)
    res("6 the other iran tunnels were not touched", {u: w.pid(ir, u) for u in others_ir} == others_ir)

    # ---- 6b. an aborted re-setup puts the old tunnel back ------------------
    old = w.cfg(kh, "hs2")
    oldpid = w.pid(kh, "hs2")
    rc, out = w.run(kh, "setup_kharej", [(DIR, "1"), (NAME, ""), (r"Replace it with this new tunnel\? \[y/N\]", "y"),
                                         (PUB_K, ""), (TRANSPORT, "9")])
    res("6b abort after 'replace': the stopped tunnel is started again on its OLD config",
        rc != 0 and "hs2 was stopped and is not running" in out and w.active(kh, "hs2")
        and w.pid(kh, "hs2") not in (0, oldpid) and w.cfg(kh, "hs2") == old, out[-400:])
    ok, info = w.traffic(8443)
    res("6b ...and it carries traffic again", ok, info)

    # ---- 7. manager restart / stop / start; crash recovery ------------------
    p = w.pids(ir)
    rc, out = w.run(ir, "tunnel_manager", [(LIST, menu_pick("hs2-x3")), (TMENU, "3"), (TMENU, "0"), (LIST, "0")])
    q = w.pids(ir)
    res("7 restart hs2-x3: new PID for it, the same for every other tunnel",
        q["hs2-x3"] not in (0, p["hs2-x3"]) and all(q[u] == p[u] for u in p if u != "hs2-x3"), out[-300:])
    rc, out = w.run(ir, "tunnel_manager", [(LIST, menu_pick("hs2-x3")), (TMENU, "2"), (r"Stop hs2-x3 now\?", "y"),
                                           (TMENU, "0"), (LIST, "0")])
    res("7 stop hs2-x3: it is down, the others are up", not w.active(ir, "hs2-x3")
        and all(w.active(ir, u) for u in w.units(ir) if u != "hs2-x3"), out[-300:])
    rc, out = w.run(ir, "tunnel_manager", [(LIST, menu_pick("hs2-x3")), (TMENU, "1"), (TMENU, "0"), (LIST, "0")])
    ok, info = w.traffic(7443)
    res("7 start hs2-x3: running and carrying traffic again", w.active(ir, "hs2-x3") and ok, info)
    victim = w.pid(ir, "hs2")
    os.kill(victim, signal.SIGKILL)
    time.sleep(1)
    back = False
    for _ in range(40):
        ok, info = w.traffic(8443)
        if ok:
            back = True
            break
        time.sleep(0.5)
    res("7 a crashed tunnel is restarted by its service and carries traffic", back and w.pid(ir, "hs2") not in (0, victim), info)

    # ---- 8. upgrade restarts every tunnel -----------------------------------
    p = w.pids(ir)
    # upgrade lists the tunnels it will restart and asks before doing it (U2)
    rc, out = w.run(ir, "upgrade", [(r"Proceed\? \[y/N\]: ", "y")], timeout=400, extra={"HS2_VERIFY_SECS": "15"})
    q = w.pids(ir)
    res("8 upgrade restarts every tunnel on its own config", rc == 0 and set(q) == set(p)
        and all(q[u] not in (0, p[u]) for u in p), out[-400:])
    res("8 upgrade checks each reconnect: only the lone listener hs2-k4 is reported not reconnected",
        "hs2-k4 has not reconnected yet" in out and "hs2-x3 has not reconnected" not in out
        and "hs2-de1 has not reconnected" not in out and "hs2 has not reconnected" not in out, out[-600:])

    # ---- 9. backup → delete → restore ---------------------------------------
    rc, out = w.run(ir, "backup >/dev/null", [])
    bk = w.backups(ir)[-1] if w.backups(ir) else ""
    rc, out = w.run(ir, "remove_tunnel hs2", [])
    gone = "hs2" not in w.units(ir) and w.cfg(ir, "hs2") is None and not w.iface_up(ir, "hs0")
    res("9 remove hs2: service, config and hs0 gone; the rest still up", gone
        and all(w.active(ir, u) for u in w.units(ir)))
    rc, out = w.run(ir, "restore %s" % os.path.join(w.root(ir), "root/hs2-backups", bk), [], timeout=300)
    ok, info = w.traffic(8443)
    res("9 restore brings hs2 back, running and carrying traffic", rc == 0 and "hs2" in w.units(ir)
        and w.active(ir, "hs2") and ok, out[-400:] + " " + info)

    # ---- 9b. deleting a tcp/mtcp tunnel never removes another tunnel's tun ----
    # x3 (mtcp) runs without a tun interface; its config still names one, and
    # on kharej that name is the one k4's real icmp tun uses.
    k4if = w.cfg(kh, "hs2-k4")["iface"]
    rc, out = w.run(kh, "remove_tunnel hs2-x3", [])
    res("9b remove the mtcp tunnel: k4's tun interface %s and service stay up" % k4if,
        "hs2-x3" not in w.units(kh) and w.iface_up(kh, k4if) and w.active(kh, "hs2-k4"), out[-300:])
    ok, info = w.traffic(5443)
    res("9b the icmp tunnel still carries traffic after that", ok, info)

    # ---- 10. status, uninstall ------------------------------------------------
    # Status judges a tunnel by whether it reaches the other server, not by
    # its interface being up: hs2-k4 never had a partner, and hs2-x3 lost its
    # kharej side in 9b — both still have their tun interface up. The restore
    # in 9 restarted every iran tunnel; a reverse kharej side notices only after
    # its liveness timeout (15 s) and redials, so the live ones get up to 60 s
    # to show connected (the dead ones never can).
    want = {"hs2": True, "hs2-de1": True, "hs2-k4-2": True, "hs2-k4": False, "hs2-x3": False}
    deadline = time.time() + 60
    while True:
        rc, out = w.run(ir, "status", [])
        blocks = {}
        for u in w.units(ir):
            m = re.search(r"^ %s — [\s\S]*?(?=^ hs2[\w-]* — |\Z)" % re.escape(u), out, re.M)
            blocks[u] = m.group(0) if m else ""
        got = {u: ("✓ connected" in b) and "NOT connected" not in b for u, b in blocks.items()}
        if got == want or time.time() > deadline:
            break
        time.sleep(3)
    res("10 status shows every tunnel", all(u in out for u in w.units(ir)), out[-300:])
    res("10 status says which tunnels really reach the other server (dead ones NOT connected)", got == want,
        "got %s" % got)
    for s in (ir, kh):
        units = w.units(s)
        cfgs = [w.cfg(s, u) for u in units]
        # wiping every tunnel needs the phrase typed, not a single 'y' (U14)
        rc, out = w.run(s, "uninstall", [(r"Type REMOVE ALL to wipe every tunnel", "REMOVE ALL")], timeout=200)
        left = sh("pgrep -af '%s'" % os.path.join(w.root(s), "usr/local/bin/hs2")).stdout.strip()
        ifs = [c["iface"] for c in cfgs if c and c.get("carrier") != "mtcp"]
        guards = sh("ip netns exec %s nft list tables 2>/dev/null | grep hs2_icmp" % NS[s]).stdout.strip()
        res("10 uninstall (%s): no icmp reply rule left in the kernel" % s, guards == "", guards)
        res("10 uninstall (%s): every tunnel removed, no process, no tun interface" % s,
            rc == 0 and w.units(s) == [] and not left and not any(w.iface_up(s, i) for i in ifs)
            and not os.listdir(os.path.join(w.root(s), "etc/hs2")), (out[-300:] + " left=" + left))

    # ---- 11. an older link without name/subnet -------------------------------
    raw = "1.2.3.4:2096|-|%s|127.0.0.1:8443|dgtun|false|tun|direct|1280|udp|" % ("ab" * 32)
    old = "hs2://" + base64.b64encode(raw.encode()).decode()
    rc, out = w.run(ir, "parse_link; echo \"NAME=$LNAME SUBNET=$LSUBNET\"", [(PASTE, old)])
    res("11 an older 11-field link still parses: default name hs2 on 10.77.0.0/30", "NAME=hs2 SUBNET=10.77.0.0" in out, out[-200:])


def build(sb):
    hs2, probe = os.environ.get("HS2_BIN"), os.environ.get("PROBE_BIN")
    if not hs2:
        hs2 = os.path.join(sb, "hs2")
        subprocess.run(["go", "build", "-o", hs2, "./cmd/hs2"], cwd=SRC, check=True, env=dict(os.environ, CGO_ENABLED="0"))
    if not probe:
        probe = os.path.join(sb, "probe")
        subprocess.run(["go", "build", "-o", probe, "./lab/probe"], cwd=SRC, check=True)
    return hs2, probe


def net(up):
    sh("ip netns del %s 2>/dev/null; ip netns del %s 2>/dev/null" % (NS["ir"], NS["kh"]))
    if not up:
        return
    for c in ("ip netns add %s && ip netns add %s" % (NS["ir"], NS["kh"]),
              "ip link add mtvi netns %s type veth peer name mtvk netns %s" % (NS["ir"], NS["kh"]),
              "ip -n %s addr add %s/24 dev mtvi && ip -n %s addr add %s/24 dev mtvk" % (NS["ir"], IP["ir"], NS["kh"], IP["kh"]),
              "for n in %s %s; do ip -n $n link set lo up; done; ip -n %s link set mtvi up; ip -n %s link set mtvk up"
              % (NS["ir"], NS["kh"], NS["ir"], NS["kh"])):
        p = sh(c)
        if p.returncode != 0:
            raise RuntimeError("%s: %s" % (c, p.stderr))


def main():
    if os.geteuid() != 0 or shutil.which("ip") is None:
        print("SKIP multi-tunnel test (needs root and iproute2)")
        return 0
    sb = tempfile.mkdtemp(prefix="hs2multi_")
    panel = None
    try:
        hs2, probe = build(sb)
        net(True)
        w = World(sb, hs2, probe)
        panel = subprocess.Popen(["ip", "netns", "exec", NS["kh"], probe, "-server", "-listen", PANEL],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        scenarios(w)
    finally:
        # stop every daemon the stand-in started, whatever state the test left
        for s in NS:
            st = os.path.join(sb, s, "sd")
            if os.path.isdir(st):
                for f in os.listdir(st):
                    if f.endswith(".sup"):
                        try:
                            os.kill(int(open(os.path.join(st, f)).read().strip() or 0), signal.SIGTERM)
                        except (OSError, ValueError):
                            pass
        time.sleep(1)
        sh("pkill -f '^%s/(kh|ir)/usr/local/bin/hs2 run' 2>/dev/null" % re.escape(sb))
        if panel:
            panel.kill()
        net(False)
        if os.environ.get("KEEP") != "1":
            shutil.rmtree(sb, ignore_errors=True)
    print("\n%s: %d failure(s)" % ("FAIL" if FAILS else "OK", len(FAILS)))
    return 1 if FAILS else 0


if __name__ == "__main__":
    sys.exit(main())
