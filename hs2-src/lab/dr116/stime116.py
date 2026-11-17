#!/usr/bin/env python3
# stime114.py RESDIR MODE: per run (arm) heavy Mbit/s, iperf start -> first "striped over 2" (s), -> last grow (s),
# final subs per conn, subs added per second after first stripe, first second >= 50 Mbit/s; then per-arm medians.
import sys, os, re, json, gzip, statistics as st, calendar, time
R, M = sys.argv[1], sys.argv[2]
rows = []
for t in sorted(os.listdir(R)):
    if not t.startswith(M): continue
    d = os.path.join(R, t)
    try:
        h = json.load(open(os.path.join(d, "heavy.json"))); res = json.load(open(os.path.join(d, "result.json")))
        meta = dict(x.split("=", 1) for x in open(os.path.join(d, "meta.txt")).read().split() if "=" in x)
    except Exception: continue
    mb = res.get("heavy", {}).get("mbps", 0)
    if not mb or "intervals" not in h: continue
    t0 = h["start"]["timestamp"]["timesecs"] % 86400
    iv = [i["sum"]["bits_per_second"] / 1e6 for i in h["intervals"]]
    f50 = next((k for k, v in enumerate(iv) if v >= 50), 99)
    F = {}; first = None
    lg = os.path.join(d, t + "-ir.log.gz")
    if not os.path.exists(lg): continue
    for l in gzip.open(lg, "rt", errors="replace"):
        m = re.match(r"(\d+):(\d+):([\d.]+) mconn (\w+): striped over (\d+) subs", l)
        if not m: continue
        s = int(m.group(1)) * 3600 + int(m.group(2)) * 60 + float(m.group(3)) - t0
        c = F.setdefault(m.group(4), [s, s, 0]); c[1] = s; c[2] = int(m.group(5))
    if not F: rows.append((meta["ARM"], t, mb, 99, 99, 0, 0, f50, round(st.mean(iv)), 0)); continue
    fs = min(v[0] for v in F.values()); lgw = max(v[1] for v in F.values())
    subs = sum(v[2] for v in F.values()); rate = st.mean((v[2] - 2) / max(v[1] - v[0], 0.5) for v in F.values())
    k = int(fs); pre = round(st.mean(iv[:max(k,1)])); post = round(st.mean(iv[k:] or [0]))
    rows.append((meta["ARM"], t, mb, round(fs, 1), round(lgw, 1), subs, round(rate, 2), f50, pre, post))
for r in sorted(rows, key=lambda r: (r[0], r[2])): print(*r)
print("arm n | Mbps med | first-stripe s med (mean) | last-grow s med | subs sum med | grow subs/s med | first>=50M s med | raw-phase Mbps med | stripe-phase Mbps med")
for a in sorted(set(r[0] for r in rows)):
    v = [r for r in rows if r[0] == a]; md = lambda i: round(st.median(x[i] for x in v), 2)
    print(a, len(v), md(2), md(3), round(st.mean(x[3] for x in v), 2), md(4), md(5), md(6), md(7), md(8), md(9))
