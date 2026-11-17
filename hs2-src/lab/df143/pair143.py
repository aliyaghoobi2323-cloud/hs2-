#!/usr/bin/env python3
# pair143.py [RESDIR] [PFX]: df143 paired A/B (K = rel62k2 edge, N = rel62k3 edge) on the cs141 background sets.
# Per pair (same bg set, same round): test download Mbit/s, bg users' sum, crowd/call/game/echo/ping p99, crowd
# fails; N-K differences; exact sign test and exact Wilcoxon signed-rank (two-sided, DP over rank sums).
import json, os, sys, glob, statistics as st
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/df143/res"
PFX = sys.argv[2] if len(sys.argv) > 2 else "gh"
rows = {}
for t in sorted(os.listdir(R)):
    d = os.path.join(R, t)
    if t[0] not in PFX or not os.path.isfile(os.path.join(d, "result.json")): continue
    try:
        res = json.load(open(os.path.join(d, "result.json")))
        h = json.load(open(os.path.join(d, "heavy.json")))
        mb = h["end"]["sum_received"]["bits_per_second"] / 1e6
    except Exception:
        print(t, "missing (iperf3 crash?)"); continue
    bg = 0.0
    for f in glob.glob(os.path.join(d, "bg*.json")):
        try: bg += json.load(open(f))["end"]["sum_received"]["bits_per_second"] / 1e6
        except Exception: pass
    p = res.get("probe", {}); c = p.get("crowd", {})
    sl = open(os.path.join(d, "stripe.log")).read() if os.path.exists(os.path.join(d, "stripe.log")) else ""
    rows[t] = dict(test=mb, bg=bg, crowd=c.get("p99_ms", 0), fail=c.get("fail", 0), call=p.get("call", {}).get("p99_ms", 0),
                   game=p.get("game", {}).get("p99_ms", 0), echo=p.get("echo_p99_ms", 0), ping=res.get("ping_p99", 0),
                   extra=sl.count("past the cap"), panics=open(os.path.join(d, "panics.txt")).read().split().count("0"))
def signp(d):
    d = [x for x in d if x != 0]; n = len(d); k = sum(x > 0 for x in d)
    from math import comb
    lo = min(k, n - k); p = 2 * sum(comb(n, i) for i in range(lo + 1)) / 2 ** n if n else 1
    return min(1.0, p), k, n
def wilcox(d):
    d = [x for x in d if x != 0]; n = len(d)
    if not n: return 1.0, 0
    a = sorted(range(n), key=lambda i: abs(d[i])); rk = [0.0] * n; i = 0
    while i < n:
        j = i
        while j + 1 < n and abs(d[a[j + 1]]) == abs(d[a[i]]): j += 1
        for q in range(i, j + 1): rk[a[q]] = (i + j) / 2 + 1
        i = j + 1
    W = sum(r for r, x in zip(rk, d) if x > 0)
    r2 = [int(round(2 * r)) for r in rk]; tot = sum(r2); dist = {0: 1}
    for r in r2:
        nd = dict(dist)
        for s, c in dist.items(): nd[s + r] = nd.get(s + r, 0) + c
        dist = nd
    w2 = int(round(2 * W)); m = tot / 2
    p = sum(c for s, c in dist.items() if abs(s - m) >= abs(w2 - m)) / 2 ** n
    return min(1.0, p), W
pairs = []
for t in rows:
    if t.endswith("K") and t[:-1] + "N" in rows: pairs.append((t[:-1], rows[t], rows[t[:-1] + "N"]))
pairs.sort()
keys = ["test", "bg", "crowd", "call", "game", "echo", "ping", "fail"]
print("pair  " + " ".join("%13s" % k for k in keys) + "   (K / N)")
for n, k, v in pairs:
    print("%-5s " % n + " ".join("%6.0f/%-6.0f" % (k[x], v[x]) if x in ("test", "bg") else "%6.1f/%-6.1f" % (k[x], v[x]) for x in keys), " extraN=%d" % v["extra"])
print("n pairs =", len(pairs))
for x in keys:
    d = [v[x] - k[x] for _, k, v in pairs]
    if not d: continue
    sp, kk, nn = signp(d); wp, W = wilcox(d)
    print("%-6s K med %7.1f  N med %7.1f | N-K mean %+7.1f med %+7.1f | N higher %d/%d sign p=%.3f | Wilcoxon W+=%.1f p=%.3f" % (
        x, st.median(k[x] for _, k, _ in pairs), st.median(v[x] for _, _, v in pairs), st.mean(d), st.median(d), kk, nn, sp, W, wp))
print("crowd fails total K %d N %d; runs with fails K %d N %d" % (sum(k["fail"] for _, k, _ in pairs), sum(v["fail"] for _, _, v in pairs),
      sum(k["fail"] > 0 for _, k, _ in pairs), sum(v["fail"] > 0 for _, _, v in pairs)))
