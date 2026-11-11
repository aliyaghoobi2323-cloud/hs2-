#!/usr/bin/env python3
"""sum50.py RESDIR — s50 battery: per (dir, var) and arm: mean ± sd of Mbps, crowd/call/ping p99, CPU;
two-sided permutation p (5000) on the mean difference reality/tls, r128/tls, reality/r128; restart and judge rows."""
import json, os, re, random, statistics as st, sys
D = sys.argv[1]; g = {}
for n in sorted(os.listdir(D)):
    m = re.match(r"([dr])(c0|c1|k|sc)-(tls|reality|r128)-(\d+)$", n)
    if not m: continue
    try: r = json.load(open(os.path.join(D, n, "result.json")))
    except Exception: continue
    h = r.get("heavy") or {}; pr = r.get("probe") or {}; cpu = (h.get("cpu") or {})
    d = dict(mbps=h.get("mbps"), crowd99=(pr.get("crowd") or {}).get("p99_ms"), call99=(pr.get("call") or {}).get("p99_ms"),
             ping99=r.get("ping_p99"), cpu=round(((cpu.get("ir") or {}).get("cores") or 0) + ((cpu.get("kh") or {}).get("cores") or 0), 2))
    try: d.update(json.load(open(os.path.join(D, n, "restart.json"))))
    except Exception: pass
    g.setdefault((m.group(1), m.group(2)), {}).setdefault(m.group(3), []).append(d)
def perm(a, b, k=5000):
    a = [x for x in a if x is not None]; b = [x for x in b if x is not None]
    if len(a) < 2 or len(b) < 2: return float("nan")
    obs = abs(st.mean(a) - st.mean(b)); pool = a + b; c = 0; rnd = random.Random(1)
    for _ in range(k):
        rnd.shuffle(pool)
        if abs(st.mean(pool[:len(a)]) - st.mean(pool[len(a):])) >= obs - 1e-9: c += 1
    return c / k
def ms(v):
    v = [x for x in v if isinstance(x, (int, float))]
    if not v: return "-"
    return f"{st.mean(v):.0f} ± {st.stdev(v):.0f}" if len(v) > 1 else f"{v[0]:.0f}"
for cell in sorted(g):
    arms = g[cell]; print(f"== {cell[0]} {cell[1]}")
    for a in ("tls", "reality", "r128"):
        if a not in arms: continue
        R = arms[a]; line = f"  {a:8s} n={len(R)}"
        for k in ("mbps", "crowd99", "call99", "ping99", "cpu"): line += f" | {k} {ms([x.get(k) for x in R])}"
        if cell[1] == "k": line += " | restart first/90% " + ", ".join(f"{x.get('first_link_s')}/{x.get('links90_s')}" for x in R)
        print(line)
    for x, y in (("reality", "tls"), ("r128", "tls"), ("reality", "r128")):
        if x in arms and y in arms:
            print(f"  p mbps {x}/{y} = {perm([q['mbps'] for q in arms[x]], [q['mbps'] for q in arms[y]]):.3f}")
