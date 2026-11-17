#!/usr/bin/env python3
# ana63d.py [RESDIR] [BASE=P]: r63d paired A/B. Tags MODE x RR ARM (d8x03N); a round's arms ran at once on the 3
# labslots (arm/slot rotated). Per mode, per arm vs BASE: paired differences over rounds where both exist, median
# diff, wins (arm better), sign test p and Wilcoxon signed-rank p (normal approx, two-sided). Lower is better
# except heavy mbps.
import json, os, sys, math, statistics as st
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/r63c/res63d"
BASE = sys.argv[2] if len(sys.argv) > 2 else "P"
MET = [("heavy", lambda r: r["heavy"]["mbps"], 1), ("call99", lambda r: r["probe"]["call"]["p99_ms"], -1),
       ("call95", lambda r: r["probe"]["call"]["p95_ms"], -1), ("callmax", lambda r: r["probe"]["call"]["max_ms"], -1),
       ("late%", lambda r: r["probe"]["call"]["late_pct"], -1),
       ("crowd99", lambda r: r["probe"]["crowd"]["p99_ms"], -1), ("game99", lambda r: r["probe"]["game"]["p99_ms"], -1),
       ("echo99", lambda r: r["probe"]["echo_p99_ms"], -1), ("ping99", lambda r: r["ping_p99"], -1),
       ("cfail", lambda r: r["probe"]["crowd"]["fail"], -1)]
D = {}
for t in sorted(os.listdir(R)):
    if t.startswith("dk") or len(t) != 6 or t[2] != "x": continue
    try: r = json.load(open(os.path.join(R, t, "result.json")))
    except Exception: print(t, "MISSING"); continue
    if "error" in r.get("heavy", {}): print(t, "iperf error (excluded)"); continue
    pan = open(os.path.join(R, t, "panics.txt")).read().split()
    if any(x not in ("ir", "kh", "panics", "0") for x in pan): print(t, "PANIC", pan)
    D.setdefault(t[:2], {}).setdefault(int(t[3:5]), {})[t[5]] = r
def sign_p(w, l):
    n = w + l
    if n == 0: return 1.0
    k = min(w, l); return min(1.0, 2 * sum(math.comb(n, i) for i in range(k + 1)) / 2 ** n)
def wil_p(d):
    d = [x for x in d if x != 0]; n = len(d)
    if n == 0: return 1.0
    a = sorted(range(n), key=lambda i: abs(d[i])); rk = [0.0] * n; i = 0
    while i < n:
        j = i
        while j + 1 < n and abs(d[a[j + 1]]) == abs(d[a[i]]): j += 1
        for q in range(i, j + 1): rk[a[q]] = (i + j) / 2 + 1
        i = j + 1
    wp = sum(rk[i] for i in range(n) if d[i] > 0); mu = n * (n + 1) / 4; sd = math.sqrt(n * (n + 1) * (2 * n + 1) / 24)
    z = (wp - mu) / sd; return math.erfc(abs(z) / math.sqrt(2))
for md in sorted(D):
    arms = sorted({a for r in D[md].values() for a in r})
    print(f"== {md}: rounds {len(D[md])}, base {BASE}")
    for a in arms:
        vals = {m: [f(D[md][k][a]) for k in sorted(D[md]) if a in D[md][k]] for m, f, _ in MET}
        print(f"  {a} n={len(vals['heavy'])} " + " ".join(f"{m}={st.median(v):.1f}" for m, v in vals.items() if v))
    for a in arms:
        if a == BASE: continue
        ks = [k for k in sorted(D[md]) if a in D[md][k] and BASE in D[md][k]]
        out = []
        for m, f, sgn in MET:
            d = [f(D[md][k][a]) - f(D[md][k][BASE]) for k in ks]
            w = sum(1 for x in d if x * sgn > 0); l = sum(1 for x in d if x * sgn < 0)
            out.append(f"{m} d={st.median(d):+.1f} win {w}/{w+l} sp={sign_p(w,l):.2f} wp={wil_p(d):.2f}")
        print(f"  {a}-{BASE} pairs={len(ks)}: " + " | ".join(out))
    if md == "u1" or True:
        for k in sorted(D[md]):
            print("   r%02d " % k + " ".join(f"{a}:{D[md][k][a]['heavy']['mbps']:.0f}/{D[md][k][a]['probe']['call']['p99_ms']:.0f}" for a in sorted(D[md][k])))
