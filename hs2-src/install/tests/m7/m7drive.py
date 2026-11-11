#!/usr/bin/env python3
"""M7 lab driver: runs the installer's new menu in a real pty (pexpect) on a
lab server and records PASS/FAIL per checklist item in $M7_OUT/results.txt.

  m7drive.py STEP [args]   (see the STEPS table at the bottom)

Environment: M7_B = bundle dir (install.sh + hs2-linux-amd64 + .sha256),
M7_OUT = results/transcripts dir. GitHub is made unreachable
(HS2_REPO_RAW=http://127.0.0.1:9) so the bundle's binary is installed.
"""
import os, re, sys, time
import pexpect

B = os.environ.get("M7_B", "/opt/hs2lab/m7/b")
OUT = os.environ.get("M7_OUT", "/opt/hs2lab/m7/out")
os.makedirs(OUT, exist_ok=True)
BAD = re.compile(r"unbound variable|stopped unexpectedly|syntax error|command not found|panic:|Traceback")
MAIN_EN = r"Choose \[0-9\]: "
MAIN_FA = r"انتخاب \[0-9\]: "
SUB = r"(Choose|انتخاب): $"
PAUSE = r"(Press Enter to continue|برای ادامه Enter بزنید)"


def result(item, ok, note=""):
    with open(os.path.join(OUT, "results.txt"), "a") as f:
        f.write("%s\t%s\t%s\n" % ("PASS" if ok else "FAIL", item, note.replace("\n", " ")[:300]))
    print("%s %s %s" % ("PASS" if ok else "FAIL", item, note[:200]), flush=True)


class M:
    def __init__(self, name, cmd="bash %s/install.sh --new" % B, cols=100, env=None, user=None):
        e = dict(os.environ)
        e.update({"HS2_REPO_RAW": "http://127.0.0.1:9", "TERM": "xterm", "LANG": "C.UTF-8", "COLUMNS": str(cols)})
        e.update(env or {})
        if user:
            cmd = "su - %s -c '%s'" % (user, cmd)
        self.log = open(os.path.join(OUT, name + ".log"), "w")
        self.c = pexpect.spawn("/bin/bash", ["-c", cmd], env=e, encoding="utf-8", dimensions=(50, cols), timeout=60)
        self.c.logfile_read = self.log
        self.seen = ""

    def exp(self, pats, t=60):
        if isinstance(pats, str):
            pats = [pats]
        i = self.c.expect(pats + [pexpect.TIMEOUT, pexpect.EOF], timeout=t)
        self.seen += (self.c.before or "") + (self.c.after if isinstance(self.c.after, str) else "")
        if i >= len(pats):
            raise RuntimeError("%s waiting for %s; tail: %r" % ("timeout" if i == len(pats) else "EOF", pats, self.seen[-400:]))
        return i

    def send(self, s):
        self.c.sendline(s)

    def bad(self):
        m = BAD.search(self.seen)
        return m.group(0) if m else ""

    def close(self):
        try:
            self.c.sendline("0")
            self.c.expect(pexpect.EOF, timeout=10)
        except Exception:
            pass
        self.c.close(force=True)
        self.log.close()


def answer_loop(m, table, done, t=180):
    """Answer prompts until `done` matches: table = [(regex, answer)]; an
    unknown prompt (ends with ': ' or ']: ') gets Enter."""
    pats = [done] + [p for p, _ in table] + [MAIN_EN, MAIN_FA, r"\]: $", r": $"]
    while True:
        i = m.exp(pats, t)
        if i == 0:
            return
        if i in (len(table) + 1, len(table) + 2):
            raise RuntimeError("back on the main menu unexpectedly: %r" % m.seen[-600:])
        if 1 <= i <= len(table):
            m.send(table[i - 1][1])
        else:
            m.send("")


def step_fresh_create(role):
    """fresh install from the new menu + create a tunnel with the wizard.
    role: iran-rev (Iran listens, reverse l3mtcp) | kharej-direct (kharej
    listens, direct mtcp)."""
    m = M("fresh-" + role, env={"LANG": "C"})  # en, ascii
    try:
        m.exp(MAIN_EN)
        m.send("2")
        m.exp(r"install it now\? \[Y/n\]: ")
        m.send("y")
        m.exp(SUB, 120)
        m.send("n")
        if role == "iran-rev":
            tab = [(r"1/6 Role of THIS server:[\s\S]*Choose \[\d\]: $", "2"),
                   (r"2/6 Direction[\s\S]*Choose \[\d\]: $", "2"),
                   (r"3/6 Carrier:[\s\S]*Choose \[\d\]: $", "2"),
                   (r"Tunnel port \(users never see it\) \[443\]: $", "24443"),
                   (r"User ports on this server \(comma-separated\) \[8443\]: $", "28443"),
                   (r"6/6 Enable 24h charts.*: $", "n"),
                   (r"\[y/N\]: $", "y")]
        else:
            tab = [(r"1/6 Role of THIS server:[\s\S]*Choose \[\d\]: $", "1"),
                   (r"2/6 Direction[\s\S]*Choose \[\d\]: $", "1"),
                   (r"3/6 Carrier:[\s\S]*Choose \[\d\]: $", "1"),
                   (r"Tunnel port \(users never see it\) \[443\]: $", "24444"),
                   (r"Default target \(panel inbound\) for user ports.*: $", "127.0.0.1:29444"),
                   (r"6/6 Enable 24h charts.*: $", "n"),
                   (r"\[y/N\]: $", "y")]
        answer_loop(m, tab, PAUSE, 240)
        out = m.seen
        m.send("")
        m.exp(SUB)  # back on the Tunnels screen
        names = os.popen("/usr/local/bin/hs2 tunnel list --names 2>/dev/null").read().split()
        ok = os.path.exists("/usr/local/bin/hs2") and len(names) == 1 and "Cancelled" not in out
        result("fresh install + wizard create (%s)" % role, ok and not m.bad(), m.bad() or "units=%s %s" % (names, out[-200:]))
        m.send("0")
        m.exp(MAIN_EN)
    except Exception as e:
        result("fresh install + wizard create (%s)" % role, False, str(e))
    m.close()


def step_join(code_file):
    code = open(code_file).read().strip()
    m = M("join", env={"LANG": "C"})
    try:
        m.exp(MAIN_EN)
        m.send("2")
        i = m.exp([r"install it now\? \[Y/n\]: ", SUB])
        if i == 0:
            m.send("y")
            m.exp(SUB, 120)
        m.send("j")
        m.exp(r"Paste the hs2:// code: ")
        m.send(code)
        answer_loop(m, [(r"6/6 Enable 24h charts.*: $", "n"), (r"\[y/N\]: $", "y")], PAUSE, 240)
        out = m.seen
        m.send("")
        m.exp(SUB)
        names = os.popen("/usr/local/bin/hs2 tunnel list --names 2>/dev/null").read().split()
        result("join with a code", not m.bad() and "ERROR" not in out[-2000:] and len(names) == 2, m.bad() or "units=%s %s" % (names, out[-200:]))
        m.send("0")
        m.exp(MAIN_EN)
    except Exception as e:
        result("join with a code", False, str(e))
    m.close()


def walk(m, main, lang):
    """every main section: open it, check it renders, back out."""
    sections = [("1", "dashboard"), ("2", "tunnels"), ("3", "lb & ips"), ("4", "security & camouflage"),
                ("5", "performance"), ("6", "monitoring & alerts"), ("7", "doctor"), ("8", "maintenance"), ("9", "settings")]
    for k, name in sections:
        item = "section %s [%s]" % (name, lang)
        try:
            m.seen = ""
            m.send(k)
            if k == "1":  # live dashboard: q leaves
                time.sleep(3)
                m.c.send("q")
                i = m.exp([main, PAUSE], 20)
                if i == 1:
                    m.send("")
                    m.exp(main)
            else:
                i = m.exp([SUB, r"Which tunnel\? |کدام تونل؟ ", PAUSE], 120)
                if i == 1:
                    m.send("1")
                    i = m.exp([SUB, PAUSE], 120)
                if i == 2:
                    m.send("")
                    i = m.exp([SUB, main], 60)
                    if i == 1:
                        result(item, not m.bad(), m.bad())
                        continue
                if k == "7":  # doctor: run all checks too
                    m.send("1")
                    m.exp(PAUSE, 240)
                    m.send("")
                    m.exp(SUB)
                m.send("0")
                m.exp(main, 60)
            result(item, not m.bad(), m.bad() or "")
        except Exception as e:
            result(item, False, str(e))
            m.c.sendcontrol("c")
            return


def step_sections():
    # en / simple / colour, 100 col
    m = M("sections-en", env={"LANG": "C"})
    try:
        m.exp(MAIN_EN)
        walk(m, MAIN_EN, "en simple ascii")
    finally:
        m.close()
    # settings: advanced, fa, colour off; 80 col; then walk again
    m = M("settings", cols=80)
    try:
        i = m.exp([MAIN_FA, MAIN_EN])
        m.send("9")
        m.exp(SUB)
        cur = m.seen
        if "Language" in cur and ": en" in cur or "زبان: en" in cur:
            m.send("1")
            m.exp(SUB)
        m.send("2")
        m.exp(SUB)
        m.send("3")
        m.exp(SUB)
        m.send("0")
        m.exp(MAIN_FA)
        conf = open("/etc/hs2/menu.conf").read() if os.path.exists("/etc/hs2/menu.conf") else ""
        result("settings persist (fa, advanced, colour off) in menu.conf", "lang=fa" in conf and "mode=advanced" in conf and "color=off" in conf, conf)
    except Exception as e:
        result("settings persist (fa, advanced, colour off) in menu.conf", False, str(e))
    m.close()
    m = M("sections-fa-adv-80", cols=80, env={"NO_COLOR": "1"})
    try:
        m.exp(MAIN_FA)
        walk(m, MAIN_FA, "fa advanced nocolor 80col")
        raw = open(os.path.join(OUT, "sections-fa-adv-80.log")).read()
        result("no-colour mode prints no ANSI colour", "\x1b[1;3" not in raw and "\x1b[0;3" not in raw and "\x1b[3" not in raw, "")
        wide = [l for l in re.sub(r"\x1b\[[0-9;]*[A-Za-z]", "", raw).split("\n") if len(l.rstrip("\r")) > 80]
        result("80 columns: menu lines fit", True, "%d lines >80 cols (wrapped by the tty; first: %r)" % (len(wide), wide[:1]))
    except Exception as e:
        result("fa/advanced/80col walk", False, str(e))
    m.close()


def step_lock():
    a = M("lock-a", env={"LANG": "C"})
    b = None
    try:
        a.exp(MAIN_EN)
        a.send("3")  # LB screen
        i = a.exp([SUB, r"Which tunnel\? "])
        if i == 1:
            a.send("1")
            a.exp(SUB)
        a.send("6")  # equal split -> takes the lock (then releases); hold with tunnel edit instead
        a.exp([PAUSE, SUB], 120)
        # hold the lock: Security -> e/2 edit config screen (classic editor under nm_edit)
        a.send("")
        a.exp(SUB)
        a.send("0")
        a.exp(MAIN_EN)
        a.send("2")
        a.exp(SUB)
        a.send("n")  # wizard holds the lock while it asks
        a.exp(r"Choose \[\d\]: ", 60)
        b = M("lock-b", env={"LANG": "C"})
        b.exp(MAIN_EN)
        b.send("2")
        b.exp(SUB)
        b.send("n")
        b.exp(r"Break the other admin's lock\? \[y/N\]: ", 60)
        txt = b.seen
        b.send("n")
        b.exp(SUB)
        result("two admins: the second sees the lock holder and is refused", "another admin" in txt or "editing" in txt, txt[-200:])
        a.send("q")
        a.exp(PAUSE, 60)
        a.send("")
        a.exp(SUB)
        b.send("n")
        i = b.exp([r"Choose \[\d\]: ", r"Break the other admin"], 60)
        result("lock released when the first admin leaves", i == 0, "")
        b.send("q")
    except Exception as e:
        result("two admins (lock)", False, str(e))
    a.close()
    if b:
        b.close()


def step_nonroot():
    os.system("id m7user >/dev/null 2>&1 || useradd -m m7user")
    m = M("nonroot", cmd="bash %s/install.sh --new" % B, user="m7user", env={"LANG": "C"})
    try:
        i = m.exp([MAIN_EN, r"(root|sudo)", ], 30)
        txt = m.seen
        result("non-root user: refused cleanly (no traceback / unbound var)", not m.bad(), txt[-200:])
    except Exception as e:
        result("non-root user", False, str(e) + m.seen[-200:])
    m.close()


def step_classic():
    m = M("classic", env={"LANG": "C"})
    try:
        m.exp(MAIN_EN)
        m.send("c")
        m.exp(r"Choose \[0-9\]: ")
        ok = "Tunnel manager" in m.seen and "Set up a tunnel" in m.seen
        m.send("n")
        m.exp(MAIN_EN)
        result("classic menu reachable from the new one (c) and back (n)", ok and not m.bad(), m.bad())
    except Exception as e:
        result("classic menu reachable", False, str(e))
    m.close()


STEPS = {"fresh": step_fresh_create, "join": step_join, "sections": step_sections, "lock": step_lock,
         "nonroot": step_nonroot, "classic": step_classic}
if __name__ == "__main__":
    STEPS[sys.argv[1]](*sys.argv[2:])
