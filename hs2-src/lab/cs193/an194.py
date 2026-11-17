# an194.py ARMDIR...: cs194 trace analysis per arm (HS2_MCONN_DARKCALL_TRACE lines, both sides; observe-only in A).
#  - darkcall dark/switch/rrec events per window (dark outside ip1/ip2x/flap = false trigger; A counts what it WOULD see)
#  - action outcomes: switch/rrec -> progress true/false; a bad switch whose target went dark within 3 s = "target died"
#  - receive-stall episodes (trace "stalled" >= 1500 ms) per side: window, subs at onset, alternative available
#    (a live sub not dark, link rx < 1 s, not primary), primary dark, max stall
#  - probe call/game: per-second p99 over seconds where the probe was alive, quiet (pre/rst/loss_all/post) vs event
import json, re, sys, collections, statistics, os, ast
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
QUIET = ("pre", "rst", "loss_all", "post")
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
TS = re.compile(r"(\d\d):(\d\d):(\d\d)\.(\d+) (\w+) darkcall (\w+) iran=(\w+)(.*)")
def q(v, p):
    v = sorted(v); return v[min(len(v) - 1, int(p * len(v)))] if v else None
for A in sys.argv[1:]:
    env = open(A + "/env.txt").read(); t0 = int(open(A + "/t0").read())
    arm = "C700" if "DARKCALL_MS=700" in env else ("B1000" if "DARKCALL=1" in env else "A-off")
    print("==", A, arm, re.search(r"edge=(\w+)", env).group(1), re.search(r"lab=(\d+)", env).group(1))
    for side in ("ir", "kh"):
        f = A + "/full/" + side + ".log"
        if not os.path.exists(f): continue
        ev = collections.Counter(); last = {}; outc = collections.Counter(); darkAt = collections.defaultdict(list)
        ep = {}; eps = []; cnt = ""
        for l in open(f, errors="replace"):
            if "darkcall" not in l: continue
            if "mconn darkcall:" in l: cnt = l[l.index("mconn darkcall:"):].strip(); continue
            m = TS.search(l)
            if not m: continue
            el = (int(m.group(1)) * 3600 + int(m.group(2)) * 60 + int(m.group(3)) + int(m.group(4)[:6]) / 1e6 - t0 % 86400) % 86400
            cid, kind, rest = m.group(5), m.group(6), m.group(8)
            if kind == "trace":
                st = int(re.search(r"stalled (-?\d+) ms", rest).group(1)); ns = int(re.search(r"subs (\d+)", rest).group(1))
                subs = re.findall(r" (\d+):([^ :]*):(\d+):(\d+):(\d+):(\d+):(-?\d+)([DCP]*)", rest.split("|", 1)[1])
                if st >= 1500:
                    if cid not in ep:
                        alt = any("P" not in s[7] and "D" not in s[7] and 0 <= int(s[6]) < 1000 for s in subs)
                        pd = any("P" in s[7] and "D" in s[7] for s in subs)
                        ep[cid] = dict(t=el, w=win(el), subs=ns, alt=alt, pdark=pd, mx=st)
                    else: ep[cid]["mx"] = max(ep[cid]["mx"], st)
                elif cid in ep: eps.append(ep.pop(cid))
                continue
            ev[(kind, win(el))] += 1
            if kind == "dark":
                pm = re.search(r"path (\S+)", rest); darkAt[pm.group(1) if pm else ""].append(el)
            if kind in ("switch", "rrec"):
                tgt = re.search(r"-> \d+\((\S+)\)", rest)
                last[cid] = (kind, el, tgt.group(1) if tgt else None)
            if kind == "outcome":
                ok = "progress=true" in rest; a = last.get(cid, ("?", 0, None))
                why = ""
                if not ok and a[0] == "switch" and a[2] and any(a[1] < x < a[1] + 3.5 for x in darkAt.get(a[2], [])): why = "/targetdied"
                outc[(a[0] + why, ok, win(a[1]))] += 1
        eps += ep.values()
        print("  %s %s" % (side, cnt[:400]))
        print("  %s events: %s" % (side, ", ".join("%s@%s=%d" % (a, b, c) for (a, b), c in sorted(ev.items()))))
        fa = sum(c for (a, b), c in ev.items() if a == "dark" and b in QUIET)
        print("  %s FALSE dark (quiet windows): %d" % (side, fa))
        if outc: print("  %s outcomes: %s" % (side, ", ".join("%s %s@%s=%d" % (a, "ok" if o else "BAD", w, c) for (a, o, w), c in sorted(outc.items()))))
        c = collections.Counter()
        for e in eps:
            k = ("1sub" if e["subs"] <= 1 else ("alt" if e["alt"] else "noalt")) + ("/pdark" if e["pdark"] else "")
            c[(e["w"], k, ">10s" if e["mx"] > 10000 else (">3s" if e["mx"] > 3000 else ">1.5s"))] += 1
        print("  %s stall episodes %d: %s" % (side, len(eps), ", ".join("%s %s %s=%d" % (a, b, d, n) for (a, b, d), n in sorted(c.items()))))
    p = json.load(open(A + "/result.json")).get("probe", {})
    for k in ("call", "game"):
        x = p.get(k, {}); x = ast.literal_eval(x) if isinstance(x, str) else x
        s = x.get("sec_p99") or []
        qq = [y for i, y in enumerate(s) if y is not None and y >= 0 and win(i) in QUIET and i >= 5]
        ee = [y for i, y in enumerate(s) if y is not None and y >= 0 and win(i) not in QUIET]
        print("  probe %s alive %ds lost %s p99 %s | sec_p99 quiet med/p90/max %s/%s/%s n%d | event med/p90/max %s/%s/%s n%d" % (
            k, len([y for y in s if y is not None and y >= 0]), x.get("lost"), round(x.get("p99_ms", 0)),
            *(round(v) if v else v for v in (q(qq, .5), q(qq, .9), max(qq) if qq else None)), len(qq),
            *(round(v) if v else v for v in (q(ee, .5), q(ee, .9), max(ee) if ee else None)), len(ee)))
