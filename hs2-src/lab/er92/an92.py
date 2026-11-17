#!/usr/bin/env python3
# an92.py RESDIR...: one line per er92 run. Blackhole fails per event (b 40-100 s, c 160-220 s),
# light p99 (crowd all / in event windows, echo, call, game, icmp ping), RSS (MB) of edge / exit
# (median of t<60 s, max), rescue counters (last "mconn rescue:" line of each side).
import json, os, re, sys
def p99(v):
    v = sorted(v); return round(v[int(len(v) * 0.99)], 1) if v else None
print("%-4s %5s %5s %5s %7s %7s %6s %6s %6s %6s %13s %13s %s" % ("run", "fB", "fC", "fAll", "c99", "c99ev", "echo", "call", "game", "ping", "rssIR med/max", "rssKH med/max", "rescue ir | kh"))
for d in sys.argv[1:]:
    t = os.path.basename(d.rstrip("/"))
    try: r = json.load(open(d + "/result.json"))
    except Exception: continue
    F, C = [], []
    for ln in open(d + "/samples.txt"):
        p = ln.split()
        if len(p) < 3: continue
        if p[0] == "F": F.append(float(p[1]))
        elif p[0] == "c": C.append((float(p[1]), float(p[2])))
    fb = sum(1 for s in F if 40 <= s < 100); fc = sum(1 for s in F if 160 <= s < 220)
    ev = [m for s, m in C if 40 <= s < 100 or 160 <= s < 220]
    pr = r.get("probe") or {}; k = pr.get("call") or {}; g = pr.get("game") or {}
    pings = [float(x) for x in re.findall(r"time=([0-9.]+) ms", open(d + "/ping.txt").read())] if os.path.exists(d + "/ping.txt") else []
    rss = {"ir": [], "kh": []}; t0 = None
    if os.path.exists(d + "/rss.txt"):
        for ln in open(d + "/rss.txt"):
            p = ln.split()
            if len(p) == 5 and p[3] in ("hs2", "hs2kh"):
                ts = int(p[0]); t0 = t0 or ts
                rss[p[1][:2]].append((ts - t0, int(p[4]) / 1024))
    def rs(v):
        if not v: return "-"
        early = sorted(x for s, x in v if s < 60) or [v[0][1]]
        return "%d/%d" % (early[len(early) // 2], max(x for s, x in v))
    def resc(f):
        last = ""
        try:
            for ln in open(f):
                if "mconn rescue:" in ln: last = ln
        except Exception: pass
        m = re.search(r"triggers (\d+) · started (\d+) acked (\d+) served (\d+) · nocover (\d+)(?: nak \d+)? failed (\d+)", last)
        return "t%s s%s a%s v%s nc%s f%s" % m.groups() if m else "-"
    print("%-4s %5d %5d %5d %7s %7s %6s %6s %6s %6s %13s %13s %s | %s" % (t, fb, fc, len(F), p99([m for s, m in C]), p99(ev),
          pr.get("echo_p99_ms") and round(pr["echo_p99_ms"], 1), k.get("p99_ms") and round(k["p99_ms"], 1),
          g.get("p99_ms") and round(g["p99_ms"], 1), p99(pings), rs(rss["ir"]), rs(rss["kh"]), resc(d + "/ir.log"), resc(d + "/kh.log")))
