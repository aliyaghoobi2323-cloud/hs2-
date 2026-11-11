#!/usr/bin/env python3
"""reg50.py RESDIR... — g{48,50}-{s,c1}-N runs: rel48 vs rel50 crowd/call/ping p99 (and Mbit/s in c1), mean ± sd, permutation p."""
import json, os, re, random, statistics as st, sys
g = {}
for D in sys.argv[1:]:
    for n in os.listdir(D):
        m = re.match(r"g(48|50)-(s|c1)-(\d+)$", n)
        if not m: continue
        try: r = json.load(open(os.path.join(D, n, "result.json")))
        except Exception: continue
        p = r.get("probe") or {}
        g.setdefault(m.group(2), {}).setdefault(m.group(1), []).append(dict(
            crowd99=(p.get("crowd") or {}).get("p99_ms"), call99=(p.get("call") or {}).get("p99_ms"),
            ping99=r.get("ping_p99"), mbps=(r.get("heavy") or {}).get("mbps")))
def perm(a, b, k=5000):
    if len(a) < 2 or len(b) < 2: return float("nan")
    obs = abs(st.mean(a) - st.mean(b)); pool = a + b; c = 0; rnd = random.Random(1)
    for _ in range(k):
        rnd.shuffle(pool)
        if abs(st.mean(pool[:len(a)]) - st.mean(pool[len(a):])) >= obs - 1e-9: c += 1
    return c / k
for v in sorted(g):
    for k in ("crowd99", "call99", "ping99", "mbps"):
        a = [x[k] for x in g[v].get("48", []) if isinstance(x[k], (int, float))]
        b = [x[k] for x in g[v].get("50", []) if isinstance(x[k], (int, float))]
        if not a or not b: continue
        f = lambda z: f"{st.mean(z):.0f} ± {st.stdev(z):.0f} (n{len(z)})" if len(z) > 1 else f"{z[0]:.0f} (n1)"
        print(f"| {v} | {k} | {f(a)} | {f(b)} | {perm(a, b):.2f} |")
