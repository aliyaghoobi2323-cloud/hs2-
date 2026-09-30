#!/usr/bin/env python3
"""A small, REAL stand-in for systemctl + journalctl, for testing the installer
where no systemd runs (containers, CI). It is not a mock: `start` really runs
the unit's ExecStart under a supervisor process that restarts it when it dies
(Restart=always, RestartSec), `stop` sends SIGTERM and then SIGKILL after
TimeoutStopSec, `reload` sends SIGHUP to the main process, and the unit's output
is its journal. Units are read from $FAKESD_UNITS, state is kept in
$FAKESD_STATE (one server = one pair of directories).

Invoked as `systemctl ...` or `journalctl ...` (symlinks / wrappers), or as
`fakesd.py systemctl ...`.

Supported: daemon-reload, enable/disable [--now], is-enabled, mask/unmask
[--runtime], start, stop, restart, reload, try-reload-or-restart, is-active,
show -p PROP --value, status, reset-failed; journalctl -u UNIT [-n N] [-f]
[-o cat] [--since ...] [--no-pager].
"""
import json
import os
import shlex
import signal
import subprocess
import sys
import time

UNITS = os.environ.get("FAKESD_UNITS", "/etc/systemd/system")
STATE = os.environ.get("FAKESD_STATE", "/tmp/fakesd")


def unit_name(u):
    return u[:-8] if u.endswith(".service") else u


def unit_path(u):
    return os.path.join(UNITS, unit_name(u) + ".service")


def sp(u, what):
    return os.path.join(STATE, "%s.%s" % (unit_name(u), what))


def read(path, default=""):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return default


def write(path, val):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write(str(val))
    os.replace(tmp, path)


def alive(pid):
    if not pid:
        return False
    try:
        os.kill(pid, 0)
    except OSError:
        return False
    # a zombie is not alive
    try:
        with open("/proc/%d/stat" % pid) as f:
            return f.read().split(")")[-1].split()[0] != "Z"
    except OSError:
        return False


def masked(u):
    p = unit_path(u)
    return os.path.islink(p) and os.path.realpath(p) == "/dev/null"


def parse_unit(u):
    """[Service] keys we honour; ExecStartPre may repeat."""
    conf = {"ExecStartPre": [], "ExecStart": "", "Restart": "no", "RestartSec": "0.1", "TimeoutStopSec": "8"}
    try:
        lines = open(unit_path(u)).read().splitlines()
    except OSError:
        return None
    for ln in lines:
        ln = ln.strip()
        if not ln or ln.startswith(("#", ";", "[")) or "=" not in ln:
            continue
        k, v = ln.split("=", 1)
        if k == "ExecStartPre":
            conf["ExecStartPre"].append(v)
        elif k in conf:
            conf[k] = v
    return conf


def sup_pid(u):
    try:
        return int(read(sp(u, "sup"), "0"))
    except ValueError:
        return 0


def main_pid(u):
    try:
        pid = int(read(sp(u, "main"), "0"))
    except ValueError:
        return 0
    return pid if alive(pid) and alive(sup_pid(u)) else 0


def log(u, msg):
    with open(sp(u, "log"), "a") as f:
        f.write(msg + "\n")


# ------------------------------------------------------------- supervisor ---
def supervise(u):
    """Runs detached: ExecStartPre, then ExecStart; restarts it per Restart=."""
    conf = parse_unit(u)
    stopping = {"v": False}
    child = {"p": None}

    def on_term(*_):
        # KillMode=mixed: SIGTERM to the main process, then SIGKILL to whatever
        # is left of its process group (the cgroup, for real systemd).
        stopping["v"] = True
        p = child["p"]
        if p and p.poll() is None:
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(float(conf["TimeoutStopSec"]))
            except subprocess.TimeoutExpired:
                pass
        if p:
            try:
                os.killpg(p.pid, signal.SIGKILL)
            except OSError:
                pass
            try:
                p.wait(1)
            except subprocess.TimeoutExpired:
                pass
        os._exit(0)

    signal.signal(signal.SIGTERM, on_term)
    restarts = 0
    logf = open(sp(u, "log"), "a")
    while not stopping["v"]:
        for pre in conf["ExecStartPre"]:
            ignore = pre.startswith("-")
            try:
                rc = subprocess.call(shlex.split(pre.lstrip("-")), stdout=logf, stderr=logf)
            except OSError:
                rc = 127
            if rc != 0 and not ignore:
                log(u, "fakesd: ExecStartPre failed")
        try:
            child["p"] = subprocess.Popen(shlex.split(conf["ExecStart"]), stdout=logf, stderr=logf,
                                          stdin=subprocess.DEVNULL, start_new_session=True)
        except OSError as e:
            log(u, "fakesd: cannot run ExecStart: %s" % e)
            write(sp(u, "result"), "failed")
            break
        write(sp(u, "main"), child["p"].pid)
        write(sp(u, "since"), time.time())
        write(sp(u, "restarts"), restarts)
        rc = child["p"].wait()
        if stopping["v"]:
            break
        log(u, "fakesd: main process exited, status=%d" % rc)
        write(sp(u, "main"), 0)
        if conf["Restart"] not in ("always", "on-failure") or (conf["Restart"] == "on-failure" and rc == 0):
            write(sp(u, "result"), "failed" if rc else "exited")
            break
        restarts += 1
        write(sp(u, "restarts"), restarts)
        time.sleep(float(conf["RestartSec"].rstrip("s") or 0.1))
    os._exit(0)


# --------------------------------------------------------------- commands ---
def start(u):
    if masked(u):
        print("Failed to start %s.service: Unit %s.service is masked." % (u, u), file=sys.stderr)
        return 1
    if parse_unit(u) is None:
        print("Failed to start %s.service: Unit %s.service not found." % (u, u), file=sys.stderr)
        return 5
    if alive(sup_pid(u)):
        return 0
    for f in ("main", "result"):
        try:
            os.remove(sp(u, f))
        except OSError:
            pass
    p = subprocess.Popen([sys.executable, os.path.abspath(__file__), "__supervise", u],
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         start_new_session=True, env=os.environ.copy())
    write(sp(u, "sup"), p.pid)
    deadline = time.time() + 3
    while time.time() < deadline and not main_pid(u):
        time.sleep(0.05)
    return 0


def stop(u):
    pid = sup_pid(u)
    if alive(pid):
        os.kill(pid, signal.SIGTERM)
        deadline = time.time() + 15
        while alive(pid) and time.time() < deadline:
            time.sleep(0.05)
    for f in ("sup", "main"):
        try:
            os.remove(sp(u, f))
        except OSError:
            pass
    return 0


def active_state(u):
    if alive(sup_pid(u)):
        return "active" if main_pid(u) else "activating"
    return "failed" if read(sp(u, "result")) == "failed" else "inactive"


def enabled(u):
    if masked(u):
        return "masked"
    if not os.path.exists(unit_path(u)):
        return ""
    return "enabled" if read(sp(u, "enabled")) == "1" else "disabled"


def systemctl(args):
    os.makedirs(STATE, exist_ok=True)
    flags = [a for a in args if a.startswith("-")]
    rest = [a for a in args if not a.startswith("-")]
    if not rest:
        return 0
    cmd, units = rest[0], [unit_name(u) for u in rest[1:]]
    now = "--now" in flags
    if cmd in ("daemon-reload", "reset-failed"):
        return 0
    if cmd == "enable":
        for u in units:
            if masked(u) or not os.path.exists(unit_path(u)):
                return 1
            write(sp(u, "enabled"), 1)
            if now:
                start(u)
        return 0
    if cmd == "disable":
        for u in units:
            write(sp(u, "enabled"), 0)
            if now:
                stop(u)
        return 0
    if cmd == "is-enabled":
        st = enabled(units[0])
        if st:
            print(st)
        return 0 if st == "enabled" else 1
    if cmd == "mask":
        for u in units:
            stop(u)
            if os.path.lexists(unit_path(u)):
                os.remove(unit_path(u))
            os.symlink("/dev/null", unit_path(u))
        return 0
    if cmd == "unmask":
        for u in units:
            if masked(u):
                os.remove(unit_path(u))
        return 0
    if cmd == "start":
        return max([start(u) for u in units] or [0])
    if cmd == "stop":
        for u in units:
            stop(u)
        return 0
    if cmd == "restart":
        rc = 0
        for u in units:
            stop(u)
            rc = max(rc, start(u))
        return rc
    if cmd in ("reload", "try-reload-or-restart"):
        for u in units:
            pid = main_pid(u)
            if pid:
                os.kill(pid, signal.SIGHUP)
            elif cmd == "reload":
                return 1
        return 0
    if cmd == "is-active":
        st = active_state(units[0])
        print(st)
        return 0 if st == "active" else 3
    if cmd == "show":
        props = []
        for i, a in enumerate(args):
            if a == "-p" and i + 1 < len(args):
                props.append(args[i + 1])
            elif a.startswith("--property="):
                props.append(a.split("=", 1)[1])
        # `show -p X --value UNIT`: the value after -p is not a unit
        cands = [a for a in rest[1:] if a not in props]
        u = unit_name(cands[-1]) if cands else ""
        out = []
        for p in props:
            if p == "MainPID":
                out.append(str(main_pid(u)))
            elif p == "ActiveState":
                out.append(active_state(u))
            elif p == "NRestarts":
                out.append(read(sp(u, "restarts"), "0"))
            elif p == "ActiveEnterTimestamp":
                t = read(sp(u, "since"))
                out.append(time.strftime("%a %Y-%m-%d %H:%M:%S UTC", time.gmtime(float(t))) if t and alive(sup_pid(u)) else "")
            else:
                out.append("")
        print("\n".join(out))
        return 0
    if cmd == "status":
        u = units[0]
        st = active_state(u)
        print("● %s.service - fakesd" % u)
        print("     Loaded: loaded (%s; %s)" % (unit_path(u), enabled(u) or "not-found"))
        print("     Active: %s" % st)
        if main_pid(u):
            print("   Main PID: %d" % main_pid(u))
        return 0 if st == "active" else 3
    print("fakesd: unsupported systemctl command: %s" % " ".join(args), file=sys.stderr)
    return 1


def journalctl(args):
    unit, n, i = None, 10, 0
    while i < len(args):
        a = args[i]
        if a == "-u" and i + 1 < len(args):
            unit = unit_name(args[i + 1]); i += 1
        elif a.startswith("--unit="):
            unit = unit_name(a.split("=", 1)[1])
        elif a == "-n" and i + 1 < len(args):
            n = int(args[i + 1]); i += 1
        elif a in ("--since", "-o") and i + 1 < len(args):
            i += 1
        i += 1
    if not unit:
        return 0
    lines = read(sp(unit, "log")).splitlines()
    for ln in lines[-n:]:
        print(ln)
    return 0


def main():
    argv = sys.argv[1:]
    me = os.path.basename(sys.argv[0])
    if argv and argv[0] == "__supervise":
        return supervise(argv[1])
    if argv and argv[0] in ("systemctl", "journalctl"):
        me, argv = argv[0], argv[1:]
    if me == "journalctl":
        return journalctl(argv)
    return systemctl(argv)


if __name__ == "__main__":
    sys.exit(main() or 0)
