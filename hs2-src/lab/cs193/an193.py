# an193.py ARMDIR...: cs193 summary per arm (rep.sh runs, callers193.py gap lines).
# Calls: DIED per event window (teardown cut), CORRUPT, longest receive gap per call generation (gap events +
# DIED maxgap_ms), gaps > 1.5 s per window. Crowd fails/p99, probe call/game p99 and per-second p99 per window
# (quiet = pre/post). RSS max (proc.txt). darkcall: last counter line per side, dark/switch/rrec lines per window
# (any in loss_all = false trigger: no path is dark there).
import json, re, sys, collections, statistics, os, time, calendar
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
W = [k for _, k in ST]
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
for A in sys.argv[1:]:
    env = open(A + "/env.txt").read()
    TT = int(re.search(r"TT=(\d+)", env).group(1)); t0 = int(open(A + "/t0").read())
    print("==", A, "darkcall" if "DARKCALL=1" in env else "base", re.search(r"edge=(\w+)", env).group(1))
    died = collections.Counter(); corrupt = 0; td = 0; mg = {}; gapw = collections.Counter()
    for l in open(A + "/calls.jsonl"):
        d = json.loads(l); el = d.get("el")
        if d["ev"] == "CORRUPT": corrupt += 1
        if el is None: continue
        k = (d.get("i"), d.get("gen"))
        if d["ev"] == "gap" and el < TT - 5:
            mg[k] = max(mg.get(k, 0), d["ms"])
            if d["ms"] > 1500: gapw[win(el)] += 1
        if d["ev"] == "DIED":
            if el >= TT - 5: td += 1; continue
            mg[k] = max(mg.get(k, 0), d.get("maxgap_ms", 0), int(1000 * (d.get("lastrx_ago") or 0)))  # the silence before death counts
            died[win(el)] += 1
    print("  calls DIED in-run %d %s teardown %d CORRUPT %d" % (sum(died.values()), dict(died), td, corrupt))
    v = sorted(mg.values())
    print("  longest gap per call: n %d  >1.5s %d  >3s %d  >10s %d  max %s ms | gaps>1.5s by window %s" % (
        len(v), sum(x > 1500 for x in v), sum(x > 3000 for x in v), sum(x > 10000 for x in v), v[-1] if v else 0, dict(gapw)))
    fw = collections.Counter()
    for l in open(A + "/fails.txt"):
        try: fw[win(float(l))] += 1
        except ValueError: pass
    r = json.load(open(A + "/result.json")); p = r.get("probe", r)
    print("  crowd fails %d %s" % (sum(fw.values()), dict(fw)))
    for k in ("crowd", "call", "game"):
        x = p.get(k, {})
        print("  %-5s p50 %s p99 %s max %s fail/lost %s" % (k, x.get("p50_ms"), x.get("p99_ms"), x.get("max_ms"), x.get("fail", x.get("lost"))))
        s = x.get("sec_p99")
        if s:
            g = {}
            for i, y in enumerate(s):
                if y is None or y < 0: continue
                g.setdefault(win(i), []).append(y)
            print("        sec_p99 med/max by window: " + " ".join("%s %d/%d" % (w, statistics.median(g[w]), max(g[w])) for w in W if w in g))
    rss = collections.defaultdict(int)
    for l in open(A + "/proc.txt"):
        m = re.match(r"\d+ (ir|kh) pid=\d+ rss_kb=(\d+)", l)
        if m: rss[m.group(1)] = max(rss[m.group(1)], int(m.group(2)) >> 10)
    print("  RSS max MB ir %d kh %d" % (rss["ir"], rss["kh"]))
    for side in ("ir", "kh"):
        f = A + "/full/" + side + ".log"
        if not os.path.exists(f): continue
        last = ""; ev = collections.Counter(); rec = ""
        for l in open(f, errors="replace"):
            if "darkcall" not in l and "mconn rescue-rec:" not in l: continue
            if "mconn darkcall:" in l: last = l[l.index("mconn darkcall:"):].strip(); continue
            if "mconn rescue-rec:" in l: rec = l; continue
            m = re.search(r"(\d\d):(\d\d):(\d\d)\.\d+ \S+ darkcall (\w+)", l)
            if m:
                h, mi, se = int(m.group(1)), int(m.group(2)), int(m.group(3))
                el = (h * 3600 + mi * 60 + se - t0 % 86400) % 86400
                ev[(m.group(4), win(el))] += 1
                continue
        print("  %s %s" % (side, last))
        if ev: print("  %s darkcall events by window: %s" % (side, ", ".join("%s@%s=%d" % (a, b, c) for (a, b), c in sorted(ev.items()))))
        m = re.search(r"started (\d+) acked (\d+) nakd (\d+) · served (\d+) nak (\d+) refused (\d+) · nocover (\d+)", rec)
        if m: print("  %s rrec started/acked/nakd/served/nak/refused/nocover %s" % (side, "/".join(m.groups())))
