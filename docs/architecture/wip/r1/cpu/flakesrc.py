#!/usr/bin/env python3
"""flakesrc.py SRC LIST [--json] — the structural flake sources of class (b) tests (R1 speed).

LIST: "package test elapsed" lines (sumsuite.py --slow). For each test, its
body and the test-file helpers it calls (two levels deep, same package) are
scanned (gofmt'd Go: a top-level func runs from "func " at column 0 to the
first "}" at column 0) for:
  wall    an emulated real-time link or pacer (angleWire/angleLink, paceTun,
          the quiet/kill/hint rigs, classPair, wireClock, pacenet, time.Sleep
          driven by a rate or delay)
  abs     an absolute bound on time or rate in a check: a comparison with
          N*time.Millisecond/Second, or with a literal ms / Mbit/s number
  rel     a bound relative to something measured in the same run (floor,
          base, min RTT, baseline, "of the")
  port    a port picked free and closed before the code under test binds it
          (freePort, freePortOn, tcpUDPPair), or a fixed port
  sleep   time.Sleep in the test itself (waits for wall time)
  env     process-wide settings (t.Setenv, os.Setenv, package tunables set)
"""
import json
import os
import re
import sys

MARK_WALL = re.compile(r"\b(newAngleLink\w*|angleWire|angleLink|newAngleWire\w*|paceTun|newPaceTun\w*|newQuietRig|quietRig|hintRig|newKillRig|killRig|startQuietPool\w*|newClassPair|classPair|wireClock|pacenet\w*|paceNet\w*|newPacer\w*|emuLink\w*|shapedConn\w*|delayConn\w*|netsim\.\w+|netem\.\w+)\b")
MARK_ABS = re.compile(r"([<>]=?)\s*(\d+(\.\d+)?)\s*\*\s*time\.(Millisecond|Second|Microsecond)|([<>]=?)\s*time\.(Millisecond|Second)\b|\b(\w*[mM]s|\w*[mM]bit\w*|\w*Ms)\s*[<>]=?\s*\d+(\.\d+)?|[<>]=?\s*\d+(\.\d+)?\s*\)?\s*\{\s*//.*\b(ms|Mbit)")
MARK_REL = re.compile(r"\b(floor|Floor|baseline|base[A-Z]\w*|minRTT|MinRTT|min_rtt|rttFloor|calib\w*)\b")
MARK_PORT = re.compile(r"\b(freePort\w*|tcpUDPPair|pickPort\w*)\b|\"[\w.:\[\]]*:[1-9]\d{2,4}\"")
MARK_SLEEP = re.compile(r"time\.Sleep\(")
MARK_ENV = re.compile(r"\bt\.Setenv\(|\bos\.Setenv\(|^\s*(tune\.|engine\.)?[A-Z]\w+\s*=\s*", re.M)
CALL = re.compile(r"(?<![.\w])([a-zA-Z_]\w*)\(")  # plain calls only: methods are not followed


def funcs_of(pkgdir):
    """name -> body of every top-level func (and method: Type.name -> name) in the package's test files"""
    fs = {}
    for fn in sorted(os.listdir(pkgdir)):
        if not fn.endswith("_test.go"):
            continue
        lines = open(os.path.join(pkgdir, fn)).read().split("\n")
        i = 0
        while i < len(lines):
            l = lines[i]
            m = re.match(r"^func ()([A-Za-z_]\w*)", l)  # top-level funcs, not methods
            if m:
                j = i
                while j < len(lines) and lines[j] != "}" and not (j > i and lines[j].startswith("func ")):
                    if j == i and l.rstrip().endswith("}"):
                        break
                    j += 1
                body = "\n".join(lines[i:j + 1])
                fs.setdefault(m.group(2), []).append((fn, i + 1, body))
                i = j + 1
                continue
            i += 1
    return fs


def scan(fs, name, depth=2):
    seen, out = set(), []
    todo = [(name, 0)]
    while todo:
        n, d = todo.pop()
        if n in seen or n not in fs:
            continue
        seen.add(n)
        for fn, ln, body in fs[n]:
            out.append((n, fn, ln, body, d))
            if d < depth:
                for c in set(CALL.findall(body)):
                    if c in fs and c not in seen:
                        todo.append((c, d + 1))
    return out


def main():
    src, lst = sys.argv[1], sys.argv[2]
    rows = [l.split() for l in open(lst) if l.strip()]
    cache = {}
    res = []
    for p, t, e in rows:
        rel = p.split("hs2-tunnel")[-1].lstrip("/") or "."
        d = os.path.join(src, rel)
        if d not in cache:
            cache[d] = funcs_of(d)
        fs = cache[d]
        parts = scan(fs, t)
        if not parts:
            res.append({"pkg": rel, "test": t, "s": float(e), "missing": True})
            continue
        own = parts[0]
        r = {"pkg": rel, "test": t, "s": float(e), "file": "%s:%d" % (own[1], own[2])}
        for key, rx, where in (("wall", MARK_WALL, "all"), ("abs", MARK_ABS, "all"), ("rel", MARK_REL, "all"),
                               ("port", MARK_PORT, "all"), ("sleep", MARK_SLEEP, "own"), ("env", MARK_ENV, "own")):
            hits = []
            for n, fn, ln, body, dep in parts:
                if where == "own" and dep > 0:
                    continue
                for k, line in enumerate(body.split("\n")):
                    if line.strip().startswith("//"):
                        continue
                    if rx.search(line):
                        hits.append("%s:%d" % (fn, ln + k))
            r[key] = len(hits)
            if hits:
                r[key + "_at"] = hits[:3]
        res.append(r)
    if "--json" in sys.argv:
        print(json.dumps(res, indent=1))
        return
    print("%-11s %-44s %6s %4s %4s %4s %4s %5s %4s" % ("pkg", "test", "s", "wall", "abs", "rel", "port", "sleep", "env"))
    for r in res:
        if r.get("missing"):
            print("%-11s %-44s %6.1f (not found)" % (r["pkg"], r["test"], r["s"]))
            continue
        print("%-11s %-44s %6.1f %4d %4d %4d %4d %5d %4d" % (r["pkg"], r["test"][:44], r["s"], r["wall"], r["abs"], r["rel"], r["port"], r["sleep"], r["env"]))
    n = len(res)
    for k in ("wall", "abs", "rel", "port", "sleep", "env"):
        print("tests with %s: %d of %d" % (k, sum(1 for r in res if r.get(k)), n))
    print("abs without rel: %d" % sum(1 for r in res if r.get("abs") and not r.get("rel")))


if __name__ == "__main__":
    main()
