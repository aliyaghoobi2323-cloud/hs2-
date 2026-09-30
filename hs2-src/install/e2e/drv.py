"""Drives the real installer / tunnel manager inside systemd containers, the way
a person at the keyboard would, and records PASS/FAIL for every step."""
import pexpect, re, subprocess, sys, time, os, hashlib

SC = os.path.dirname(os.path.abspath(__file__))
RAW = "http://10.30.0.1:8099"
RESULTS = []
ANSI = re.compile(r'\x1b\[[0-9;]*[A-Za-z]|\x1b[()][0-9A-Za-z]|\x1b[=>]|\r')


def res(name, ok, info=""):
    RESULTS.append((name, ok, info))
    print(("PASS " if ok else "FAIL ") + name + (("  — " + info) if info else ""), flush=True)


def sh(c, cmd, timeout=60):
    p = subprocess.run(["docker", "exec", c, "bash", "-c", cmd], capture_output=True, text=True, timeout=timeout)
    return p.stdout.strip()


def spawn(c, cmd, log):
    ch = pexpect.spawn("docker", ["exec", "-it", c, "bash", "-c", cmd], encoding="utf-8",
                       timeout=90, dimensions=(50, 140))
    ch.logfile_read = open(os.path.join(SC, "tr_" + log + ".txt"), "a")
    return ch


def clean(s):
    return ANSI.sub("", s or "")


def data(port=8443, size=1 << 20):
    """user -> iran user port -> tunnel -> kharej panel (echo) -> back."""
    py = ("import socket,os,sys\n"
          "try:\n"
          f" c=socket.create_connection(('127.0.0.1',{port}),timeout=3);m=os.urandom({size});c.sendall(m);r=b'';c.settimeout(8)\n"
          " while len(r)<len(m):\n"
          "  b=c.recv(262144)\n"
          "  if not b:break\n"
          "  r+=b\n"
          " print('OK' if r==m else 'SHORT %d'%len(r))\n"
          "except Exception as e: print('ERR',e)\n")
    return sh("ir", f"python3 -c \"{py}\"", timeout=30) == "OK"


def wait_data(port=8443, secs=40):
    t0 = time.time()
    while time.time() - t0 < secs:
        if data(port, 64 * 1024):
            return time.time() - t0
        time.sleep(1)
    return None


def state(c, unit="hs2"):
    return sh(c, f"systemctl is-active {unit}; systemctl is-enabled {unit}").split("\n")


def pid(c):
    return sh(c, "systemctl show -p MainPID --value hs2")


def cfgsum(c):
    return sh(c, "sha256sum /etc/hs2/config.json | cut -c1-16")


# nano helpers --------------------------------------------------------------
def nano_replace(ch, old, new):
    ch.expect("GNU nano")
    time.sleep(0.8)
    ch.send("\x1c")                  # Ctrl+\  (replace)
    ch.expect("Search")
    ch.send(old + "\r")
    ch.expect("Replace with")
    ch.send(new + "\r")
    ch.expect("this instance")
    ch.send("y")
    time.sleep(0.5)


def nano_save_exit(ch):
    ch.send("\x18")                  # Ctrl+X
    ch.expect("Save modified buffer")
    ch.send("y")
    ch.expect("File Name to Write")
    ch.send("\r")


def nano_exit_unchanged(ch):
    ch.expect("GNU nano")
    time.sleep(0.8)
    ch.send("\x18")


# ------------------------------------------------------------------------------
def phase_install():
    # Iran: reverse edge, mtcp over tcp, listens on 2082, user port 8443
    ch = spawn("ir", f"curl -fsSL {RAW}/install.sh | HS2_REPO_RAW={RAW} bash", "install_iran")
    ch.expect(r"Choose \[0-8\]"); ch.sendline("2")
    ch.expect("Direction"); ch.expect(r"Choose \[1\]"); ch.sendline("2")
    ch.expect("Public IP of THIS iran server"); ch.sendline("")
    ch.expect("Transport:"); ch.expect(r"Choose \[1\]"); ch.sendline("3")
    ch.expect("Tunnel port to LISTEN"); ch.sendline("2082")
    ch.expect("Only one local IP here")   # single-IP server: no listen-IP question
    ch.expect("IP that USERS connect to"); ch.sendline("")
    ch.expect(r"User port\(s\)"); ch.sendline("8443")
    ch.expect("Domain for THIS iran server"); ch.sendline("germanytunnel.test")
    ch.expect("TLS mode"); ch.expect(r"Choose \[1\]"); ch.sendline("1")
    ch.expect("Also forward UDP"); ch.sendline("n")
    ch.expect("Certificate for"); ch.expect(r"Choose \[1\]"); ch.sendline("3")
    ch.expect("Path to certificate"); ch.sendline("/root/cert.pem")
    ch.expect("Path to private key"); ch.sendline("/root/key.pem")
    i = ch.expect([r"hs2://([A-Za-z0-9+/=]+)", "failed to start"], timeout=120)
    out = clean(ch.before)
    res("install iran: service healthy + link printed", i == 0 and "is running" in out, "" if i == 0 else out[-400:])
    link = "hs2://" + ch.match.group(1) if i == 0 else ""
    ch.expect(pexpect.EOF)
    res("install iran: autostart message", "Autostart on boot: ON" in out)
    res("install iran: hs2-menu shortcut installed", sh("ir", "test -x /usr/local/bin/hs2-menu && echo y") == "y")

    # Kharej: reverse exit, dials from the second IP (10.30.0.21)
    ch = spawn("kh", f"curl -fsSL {RAW}/install.sh | HS2_REPO_RAW={RAW} bash", "install_kharej")
    ch.expect(r"Choose \[0-8\]"); ch.sendline("1")
    ch.expect("Direction"); ch.expect(r"Choose \[1\]"); ch.sendline("2")
    ch.expect("Paste the hs2:// setup link"); ch.sendline(link)
    ch.expect("This server has several IPs")
    ch.expect("Dial out FROM which local IP"); ch.sendline("10.30.0.99")      # not ours
    ch.expect("is not on this server")                                        # re-asks, no abort
    ch.expect("Dial out FROM which local IP"); ch.sendline("10.30.0.21")
    ch.expect("Panel inbound address"); ch.sendline("")
    i = ch.expect(["is running", "failed to start"], timeout=120)
    res("install kharej: bad egress IP re-asked, then healthy", i == 0)
    ch.expect(pexpect.EOF)

    t = wait_data()
    res("tunnel carries data after install", t is not None, f"first data after {t:.1f}s" if t else "")
    src = sh("kh", "ss -Htn state established '( dport = :2082 )' | awk '{print $3}' | sed 's/:[0-9]*$//' | sort | uniq -c")
    res("kharej links leave from 10.30.0.21 only", "10.30.0.21" in src and "10.30.0.20" not in src, src.replace("\n", "; "))
    unit = sh("ir", "cat /etc/systemd/system/hs2.service")
    res("unit: StartLimitIntervalSec=0 + modprobe tun + Restart=always",
        all(k in unit for k in ("StartLimitIntervalSec=0", "ExecStartPre=-/sbin/modprobe tun", "Restart=always", "WantedBy=multi-user.target")))
    for c in ("ir", "kh"):
        a, e = state(c)
        res(f"{c}: active + enabled", (a, e) == ("active", "enabled"), f"{a}/{e}")


def menu_open(c, log):
    ch = spawn(c, "hs2-menu", log)
    ch.expect(r"Choose \[0-8\]")
    ch.sendline("3")
    ch.expect("Pick a tunnel number")
    return ch


def phase_manager():
    ch = menu_open("ir", "manager_iran")
    lst = clean(ch.before)
    res("list: shows tunnel 1 running with links + autostart",
        "1) hs2" in lst and "running" in lst and "links" in lst and "autostart ON" in lst, lst.strip().replace("\n", " | ")[-260:])
    res("list: describes role/direction/transport/endpoint",
        "Iran side · reverse · tcp (mtcp) · listens on 0.0.0.0:2082" in lst)
    ch.expect("Choose:"); ch.sendline("1")
    ch.expect("Choose:")
    det = clean(ch.before)
    res("details: shows connected kharej IP + user ports + autostart", "connected: 10.30.0.21" in det and "User ports:  8443" in det and "ON" in det,
        det.strip().replace("\n", " | ")[-300:])

    # invalid choice
    ch.sendline("9"); ch.expect("Invalid choice"); ch.expect("Choose:")
    res("tunnel menu: invalid choice handled", True)

    # stop (confirm)
    ch.sendline("2"); ch.expect(r"Stop hs2 now\?"); ch.sendline("y")
    ch.expect("hs2 stopped"); ch.expect("Choose:")
    out = clean(ch.before)
    res("stop: service stopped + reboot note", state("ir")[0] == "inactive" and "starts again after a reboot" in out)
    res("stop: users really cut off", not data(size=4096))
    res("details after stop show '○ stopped'", "stopped" in out)

    # stop again -> already stopped
    ch.sendline("2"); ch.expect("already stopped"); ch.expect("Choose:")
    res("stop when stopped: 'already stopped'", True)

    # start
    ch.sendline("1"); i = ch.expect(["hs2 is running", "did not stay up"], timeout=60); ch.expect("Choose:")
    t = wait_data()
    res("start: running + data flows again", i == 0 and t is not None, f"data after {t:.1f}s" if t else "")

    # restart -> new PID
    p0 = pid("ir")
    ch.sendline("3"); i = ch.expect(["restarted and running", "did not stay up"], timeout=60); ch.expect("Choose:")
    p1 = pid("ir"); t = wait_data()
    res("restart: new PID, running, data flows", i == 0 and p0 != p1 and t is not None, f"pid {p0}->{p1}")

    # edit, no change
    p0, s0 = pid("ir"), cfgsum("ir")
    ch.sendline("4"); ch.expect("Save: Ctrl\\+O then Enter"); ch.expect("Press Enter to open the editor"); ch.sendline("")
    nano_exit_unchanged(ch)
    ch.expect("No changes"); ch.expect("Choose:")
    res("edit unchanged: no restart", pid("ir") == p0 and cfgsum("ir") == s0)

    # edit valid: add user port 9443
    ch.sendline("4"); ch.expect("Press Enter to open the editor"); ch.sendline("")
    nano_replace(ch, '"forward_ports": "8443"', '"forward_ports": "8443,9443"')
    nano_save_exit(ch)
    ch.expect("Your changes:"); i = ch.expect(["Applied — hs2 is running with the new config", "did not come up", "NOT applied"], timeout=60)
    out = clean(ch.before)
    ch.expect("Choose:")
    t = wait_data(9443)
    res("edit valid: diff shown, check OK, applied, new port 9443 works",
        i == 0 and "config OK" in out and '+ ' in out and t is not None, out[-200:] if i else "")
    res("edit valid: .prev saved with 600", sh("ir", "stat -c %a /etc/hs2/config.json.prev") == "600"
        and sh("ir", "stat -c %a /etc/hs2/config.json") == "600")

    # edit invalid JSON -> discard
    p0, s0 = pid("ir"), cfgsum("ir")
    ch.sendline("4"); ch.expect("Press Enter to open the editor"); ch.sendline("")
    nano_replace(ch, '"reverse": true,', '"reverse": true')
    nano_save_exit(ch)
    ch.expect("NOT applied"); out = clean(ch.before)
    ch.expect(r"Choose \[1\]"); ch.sendline("2"); ch.expect("Changes thrown away"); ch.expect("Choose:")
    res("edit broken JSON: error with line number, not applied, discarded",
        "not valid JSON at line" in out and pid("ir") == p0 and cfgsum("ir") == s0, out.strip().split("\n")[-3:].__str__())

    # edit invalid -> reopen -> fix (back to the original) -> nothing to apply
    p0 = pid("ir")
    ch.sendline("4"); ch.expect("Press Enter to open the editor"); ch.sendline("")
    nano_replace(ch, '"reverse": true,', '"reverse": true')
    nano_save_exit(ch)
    ch.expect("NOT applied"); ch.expect(r"Choose \[1\]"); ch.sendline("1")
    nano_replace(ch, 'true "udp"', 'true, "udp"')
    nano_save_exit(ch)
    i = ch.expect(["No changes — hs2 was not restarted", "Applied — hs2 is running", "NOT applied", "did not come up"], timeout=60); ch.expect("Choose:")
    res("edit broken -> reopen -> fixed back to original -> no restart", i == 0 and pid("ir") == p0 and wait_data() is not None)

    # semantic error caught by `hs2 check`: user port == tunnel port
    p0, s0 = pid("ir"), cfgsum("ir")
    ch.sendline("4"); ch.expect("Press Enter to open the editor"); ch.sendline("")
    nano_replace(ch, '"8443,9443"', '"2082"')
    nano_save_exit(ch)
    ch.expect("NOT applied"); out = clean(ch.before)
    ch.expect(r"Choose \[1\]"); ch.sendline("2"); ch.expect("Choose:")
    res("edit semantic error (user port = tunnel port) blocked", "tunnel port" in out and pid("ir") == p0 and cfgsum("ir") == s0)

    # passes the check but cannot start (port busy) -> automatic rollback offer
    sh("ir", "systemd-run --unit=busyport python3 -c \"import socket,time;s=socket.socket();s.bind(('0.0.0.0',2090));s.listen();time.sleep(9999)\"")
    time.sleep(1)
    ch.sendline("4"); ch.expect("Press Enter to open the editor"); ch.sendline("")
    nano_replace(ch, '0.0.0.0:2082', '0.0.0.0:2090')
    nano_save_exit(ch)
    ch.expect("config OK")
    ch.expect("did not come up with the new config", timeout=60)
    ch.expect(r"Put the previous config back and restart\? \[Y/n\]"); ch.sendline("")
    i = ch.expect(["Previous config restored — hs2 is running again", "still not running"], timeout=60); ch.expect("Choose:")
    addr = sh("ir", "grep -o '0.0.0.0:20[0-9]*' /etc/hs2/config.json")
    t = wait_data(secs=60)
    res("runtime failure -> rollback restored old config and tunnel", i == 0 and addr == "0.0.0.0:2082" and t is not None, f"addr={addr}")
    sh("ir", "systemctl stop busyport")

    # live log, Ctrl+C returns to the menu (script must survive)
    ch.sendline("5"); ch.expect("press Ctrl\\+C"); time.sleep(2); ch.sendcontrol("c")
    i = ch.expect(["Choose:", pexpect.EOF], timeout=20)
    res("live log: Ctrl+C returns to the menu", i == 0)

    # autostart OFF
    ch.sendline("6"); ch.expect("Autostart OFF"); ch.expect("Choose:")
    res("autostart OFF -> disabled, still running", state("ir") == ["active", "disabled"], str(state("ir")))
    ch.sendline("0")
    ch.expect("Pick a tunnel number")
    lst = clean(ch.before)
    res("list shows autostart OFF", "autostart OFF" in lst)
    ch.expect("Choose:"); ch.sendline("x"); ch.expect("Invalid choice")
    ch.expect("Choose:"); ch.sendline("5"); ch.expect("There is no tunnel 5")
    ch.expect("Choose:"); ch.sendline("0")
    ch.expect(r"Choose \[0-8\]"); ch.sendline("9"); ch.expect("Invalid choice")
    ch.expect(r"Choose \[0-8\]"); ch.sendline("8"); ch.expect("Remove the hs2 tunnel"); ch.sendline("n")
    ch.expect("Nothing removed"); ch.expect(pexpect.EOF)
    res("menus: invalid input handled; uninstall 'n' removes nothing", state("ir")[0] == "active"
        and sh("ir", "test -f /etc/hs2/config.json && echo y") == "y")

    # kharej side view (dialer)
    ch = menu_open("kh", "manager_kharej")
    lst = clean(ch.before)
    ch.expect("Choose:"); ch.sendline("1"); ch.expect("Choose:")
    det = clean(ch.before)
    res("kharej list/details: connects to iran from 10.30.0.21",
        "Kharej side · reverse · tcp (mtcp) · connects to 10.30.0.10:2082" in lst and "from 10.30.0.21" in det and "Panel:       127.0.0.1:8443" in det,
        lst.strip().replace("\n", " | ")[-200:])
    ch.sendline("0"); ch.expect("Choose:"); ch.sendline("0"); ch.expect(r"Choose \[0-8\]"); ch.sendline("0"); ch.expect(pexpect.EOF)


def reboot(c):
    subprocess.run(["docker", "exec", c, "systemctl", "reboot"], capture_output=True, timeout=30)


def wait_boot(c, secs=90):
    t0 = time.time()
    time.sleep(3)
    while time.time() - t0 < secs:
        try:
            s = sh(c, "systemctl is-system-running", timeout=10)
            if s in ("running", "degraded"):
                return time.time() - t0
        except Exception:
            pass
        time.sleep(1)
    return None


def phase_reboot():
    # autostart OFF (set by the manager test) must keep it down after a reboot
    reboot("ir"); b = wait_boot("ir")
    res("reboot with autostart OFF: stays stopped", b is not None and state("ir") == ["inactive", "disabled"], str(state("ir")))
    # turn it back ON with the manager (and start it)
    ch = menu_open("ir", "manager_iran2")
    ch.expect("Choose:"); ch.sendline("1"); ch.expect("Choose:")
    ch.sendline("6"); ch.expect("Autostart ON"); ch.expect("Choose:")
    ch.sendline("1"); ch.expect(["hs2 is running", "did not stay up"], timeout=60); ch.expect("Choose:")
    ch.sendline("0"); ch.expect("Choose:"); ch.sendline("0"); ch.expect(r"Choose \[0-8\]"); ch.sendline("0"); ch.expect(pexpect.EOF)
    res("autostart ON + start from manager", state("ir") == ["active", "enabled"] and wait_data() is not None)

    # reboot BOTH servers; the kharej's egress IP only appears 5 s after boot
    reboot("kh"); reboot("ir")
    bk, bi = wait_boot("kh"), wait_boot("ir")
    t = wait_data(secs=90)
    res("reboot both: hs2 up on both by itself", state("ir")[0] == "active" and state("kh")[0] == "active", f"{state('ir')} {state('kh')}")
    res("reboot both: tunnel carries data again", t is not None, f"boot ir {bi:.0f}s kh {bk:.0f}s, data {t:.0f}s after boot" if t and bi and bk else "")
    log = sh("kh", "journalctl -u hs2 -b --no-pager -o cat | grep -E 'reverse dial to edge failed|reverse link up' | head -3")
    res("kharej waited for its late IP instead of giving up", "reverse link up" in log, log.replace("\n", " | ")[:300])
    src = sh("kh", "ss -Htn state established '( dport = :2082 )' | awk '{print $3}' | sed 's/:[0-9]*$//' | sort -u")
    res("after reboot still leaves from 10.30.0.21", src == "10.30.0.21", src)

    # crash: kill -9 -> systemd brings it back
    p0 = pid("ir"); sh("ir", "kill -9 $(systemctl show -p MainPID --value hs2)"); time.sleep(5)
    t = wait_data()
    res("crash (kill -9): restarted automatically, data flows", pid("ir") not in (p0, "0") and t is not None, f"pid {p0}->{pid('ir')}")


def phase_upgrade():
    # an install made by the previous installer: old unit, no hs2-menu
    old_unit = """[Unit]
Description=hs2 DPI-resistant tunnel (kharej)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/hs2 run -c /etc/hs2/config.json
Restart=always
RestartSec=3
TimeoutStopSec=8
KillMode=mixed
KillSignal=SIGTERM
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
"""
    sh("kh", f"cat > /etc/systemd/system/hs2.service <<'EOF'\n{old_unit}EOF\nsystemctl daemon-reload; systemctl disable hs2 >/dev/null 2>&1; rm -f /usr/local/bin/hs2-menu /root/hs2-backups/*")
    ch = spawn("kh", f"curl -fsSL {RAW}/install.sh | HS2_REPO_RAW={RAW} bash -s upgrade", "upgrade_kharej")
    i = ch.expect(["hs2 upgraded and running", "failed to start"], timeout=180); ch.expect(pexpect.EOF)
    unit = sh("kh", "cat /etc/systemd/system/hs2.service")
    res("upgrade: running", i == 0)
    res("upgrade: old unit rewritten (StartLimitIntervalSec=0, modprobe) + role kept",
        "StartLimitIntervalSec=0" in unit and "modprobe tun" in unit and "(kharej)" in unit)
    res("upgrade: autostart re-enabled", state("kh") == ["active", "enabled"], str(state("kh")))
    res("upgrade: hs2-menu installed + backup taken",
        sh("kh", "test -x /usr/local/bin/hs2-menu && ls /root/hs2-backups/*.tar.gz | wc -l") == "1")
    res("upgrade: tunnel carries data", wait_data() is not None)
    # hs2 check on the real configs of both sides
    for c in ("ir", "kh"):
        out = sh(c, "hs2 check -c /etc/hs2/config.json")
        res(f"{c}: hs2 check on the live config", out.endswith("config OK") and "ERROR" not in out, out.replace("\n", " | "))
    # `manage` entry point
    ch = spawn("ir", f"curl -fsSL {RAW}/install.sh | HS2_REPO_RAW={RAW} bash -s manage", "manage_cli")
    ch.expect("Pick a tunnel number"); ch.expect("Choose:"); ch.sendline("0"); ch.expect(pexpect.EOF)
    res("bash -s manage opens the manager directly", True)


def uninstall(c):
    ch = spawn(c, "hs2-menu", "uninstall_" + c)
    ch.expect(r"Choose \[0-8\]"); ch.sendline("8")
    ch.expect("Remove the hs2 tunnel"); ch.sendline("y")
    i = ch.expect(["hs2 removed", pexpect.TIMEOUT], timeout=60); ch.expect(pexpect.EOF)
    return i == 0


def phase_lifecycle():
    ok_ir, ok_kh = uninstall("ir"), uninstall("kh")
    gone = sh("ir", "test -f /etc/hs2/config.json || test -f /etc/systemd/system/hs2.service || ip link show hs0 >/dev/null 2>&1 && echo left || echo gone")
    res("uninstall 'y': service, config and hs0 removed on both", ok_ir and ok_kh and gone == "gone" and not data(size=4096), gone)
    res("uninstall kept a backup", sh("ir", "ls /root/hs2-backups/*.tar.gz | wc -l") != "0")

    for c in ("ir", "kh"):
        ch = spawn(c, "hs2-menu", "restore_" + c)
        ch.expect(r"Choose \[0-8\]"); ch.sendline("7")
        ch.expect("Restore which file"); ch.sendline("")
        i = ch.expect(["Restored and running", "failed to start"], timeout=60); ch.expect(pexpect.EOF)
        res(f"{c}: restore newest backup -> running", i == 0 and state(c) == ["active", "enabled"], str(state(c)))
    t = wait_data(secs=60)
    res("after restore: reverse tunnel carries data", t is not None)



def phase_direct():
    # fresh DIRECT tunnel: kharej listens on its 2nd IP, iran dials
    uninstall("ir"); uninstall("kh")
    sh("kh", 'openssl req -x509 -newkey rsa:2048 -keyout /root/key.pem -out /root/cert.pem -days 30 -nodes -subj "/CN=kharej.test" -addext "subjectAltName=DNS:kharej.test" >/dev/null 2>&1')
    ch = spawn("kh", "hs2-menu", "direct_kharej")
    ch.expect(r"Choose \[0-8\]"); ch.sendline("1")
    ch.expect("Direction"); ch.expect(r"Choose \[1\]"); ch.sendline("1")
    ch.expect("Public IP ="); ch.expect("Public IP of THIS kharej server"); ch.sendline("10.30.0.21")
    ch.expect("Transport:"); ch.expect(r"Choose \[1\]"); ch.sendline("3")
    ch.expect("Tunnel port"); ch.sendline("2096")
    ch.expect(r"LISTEN on which local IP\? \[10.30.0.21\]"); ch.sendline("")        # default = the public IP
    ch.expect("Panel inbound address"); ch.sendline("")
    ch.expect("Domain"); ch.sendline("kharej.test")
    ch.expect("TLS mode"); ch.expect(r"Choose \[1\]"); ch.sendline("1")
    ch.expect("Also forward UDP"); ch.sendline("n")
    ch.expect("Certificate for"); ch.expect(r"Choose \[1\]"); ch.sendline("3")
    ch.expect("Path to certificate"); ch.sendline("/root/cert.pem")
    ch.expect("Path to private key"); ch.sendline("/root/key.pem")
    i = ch.expect([r"hs2://([A-Za-z0-9+/=]+)", "failed to start"], timeout=120)
    link = "hs2://" + ch.match.group(1) if i == 0 else ""
    ch.expect(pexpect.EOF)
    res("direct kharej installed (listen IP defaulted to the public IP)", i == 0 and '"addr": "10.30.0.21:2096"' in sh("kh", "cat /etc/hs2/config.json"))

    ch = spawn("ir", "hs2-menu", "direct_iran")
    ch.expect(r"Choose \[0-8\]"); ch.sendline("2")
    ch.expect("Direction"); ch.expect(r"Choose \[1\]"); ch.sendline("1")
    ch.expect("Paste the hs2:// setup link"); ch.sendline(link)
    ch.expect("Only one local IP here")          # single-IP server: egress question skipped
    ch.expect("IP that USERS connect to"); ch.sendline("")
    ch.expect(r"User port\(s\)"); ch.sendline("8443")
    i = ch.expect(["is running", "failed to start"], timeout=120); ch.expect(pexpect.EOF)
    t = wait_data()
    res("direct iran installed + data flows", i == 0 and t is not None)

    for c, want in (("ir", "Iran side · direct · tcp (mtcp) · connects to 10.30.0.21:2096"),
                    ("kh", "Kharej side · direct · tcp (mtcp) · listens on 10.30.0.21:2096")):
        ch = menu_open(c, "direct_mgr_" + c)
        lst = clean(ch.before)
        ch.expect("Choose:"); ch.sendline("0"); ch.expect(r"Choose \[0-8\]"); ch.sendline("0"); ch.expect(pexpect.EOF)
        res(f"{c}: manager shows the direct tunnel correctly", want in lst and "links" in lst, lst.strip().split("\n")[-2:].__str__())

    reboot("kh"); reboot("ir"); wait_boot("kh"); wait_boot("ir")
    t = wait_data(secs=90)
    res("direct: reboot both -> back up by itself, data flows", t is not None and state("ir")[0] == "active" and state("kh")[0] == "active")


if __name__ == "__main__":
    for ph in sys.argv[1:]:
        try:
            globals()["phase_" + ph]()
        except Exception as e:
            res(f"phase {ph} crashed", False, f"{type(e).__name__}: {str(e)[:300]}")
    print("SUMMARY: %d passed, %d failed" % (sum(1 for r in RESULTS if r[1]), sum(1 for r in RESULTS if not r[1])))
