#!/usr/bin/env python3
# ana142.py [SOAKDIR]: df142 soak summary — RSS/goroutines/fds over time (both sides), stripe_active over time,
# test downloads (Mbit/s, striped?), background users, crowd p99 / fails inside vs outside event windows, panics.
import json, os, sys, statistics as st, glob, re, datetime
O = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/df142/soak"
rd = lambda f: open(os.path.join(O, f)).read().splitlines() if os.path.exists(os.path.join(O, f)) else []
print("env:", " ".join(rd("env.txt")))
# process
P = {}
for l in rd("proc.txt"):
    p = l.split()
    if len(p) < 3 or "rss_kb=" not in l: continue
    kv = dict(x.split("=", 1) for x in p[2:] if "=" in x)
    P.setdefault(p[1], []).append((int(p[0]), int(kv.get("rss_kb", 0)) // 1024, kv.get("gor", "?"), int(kv.get("fds", 0)), kv.get("tcp", "?")))
for s, v in P.items():
    print("%s proc (t rssMB gor fds tcp), every ~5 min:" % s)
    for i, r in enumerate(v):
        if i % 5 == 0 or i == len(v) - 1: print("   ", *r)
    rs = [r[1] for r in v]; n = len(rs); h = rs[n // 2:]
    print("    rss first/max/last %d/%d/%d MB; 2nd half min/max %d/%d; pids %s" % (rs[0], max(rs), rs[-1], min(h), max(h), len(set(x for l in rd("proc.txt") for x in l.split()[2:3] if l.split()[1] == s))))
print("MISSING lines:", sum(1 for l in rd("proc.txt") if "MISSING" in l), " alarm:", rd("alarm.txt")[:5])
# stripe_active
S = [l.split() for l in rd("stripe.txt")]
S = [(int(p[0]), int(p[1]), int(p[2]), int(p[3])) for p in S if len(p) >= 4 and p[1].lstrip("-").isdigit()]
if S:
    act = [x[1] for x in S]
    print("stripe_active samples %d: max %d, mean %.1f, p90 %d; >8 in %d samples, >=12 in %d; refused %d -> %d, subs %d -> %d" % (
        len(act), max(act), st.mean(act), sorted(act)[int(len(act) * .9)], sum(a > 8 for a in act), sum(a >= 12 for a in act), S[0][2], S[-1][2], S[0][3], S[-1][3]))
    for k in range(0, S[-1][0] + 1, 300):
        w = [x[1] for x in S if k <= x[0] < k + 300]
        if w: print("    t %4d-%4d stripe_active mean %.1f max %d" % (k, k + 300, st.mean(w), max(w)))
# events windows (+60 s tail)
W = []
for l in rd("events.txt"):
    p = l.split()
    if len(p) == 3: W.append((int(p[0]), int(p[1]) + 60, p[2]))
inw = lambda t: any(a <= t < b for a, b, _ in W)
# tests
T = []
for l in rd("tests.txt"):
    p = l.split(); kv = dict(x.split("=", 1) for x in p[2:] if "=" in x); t = int(p[0])
    striped = "?"
    try:
        h = json.load(open(os.path.join(O, "tests", "t%d.json" % t)))
        iv = [i["sum"]["bits_per_second"] / 1e6 for i in h["intervals"]]
        base = st.median(iv[0:3]); striped = "Y" if any(x >= 1.5 * base and x >= 300 for x in iv[1:]) or max(iv) >= 500 else "n"
        ivs = " ".join("%d" % x for x in iv)
    except Exception: ivs = "-"
    T.append((t, kv.get("test_mbit"), striped, kv.get("bg_conns"), "W" if inw(t) else "-", ivs))
print("test downloads (t mbit striped bg_conns inWindow per-second):")
for r in T: print("   ", *r)
ok = [int(r[1]) for r in T if r[1] not in (None, "ERR")]
if ok:
    q = sorted(ok); print("tests n=%d ok, %d ERR: mean %.0f median %.0f p10 %d min %d; striped %d/%d; outside windows mean %.0f (n=%d)" % (
        len(ok), len(T) - len(ok), st.mean(ok), st.median(ok), q[int(len(q) * .1)], q[0], sum(r[2] == "Y" for r in T), len(T),
        st.mean([int(r[1]) for r in T if r[1] not in (None, "ERR") and r[4] == "-"] or [0]), sum(1 for r in T if r[1] not in (None, "ERR") and r[4] == "-")))
# background users
B = [l.split() for l in rd("bg.txt")]
bv = [dict(x.split("=", 1) for x in p[2:] if "=" in x) for p in B]
okb = [b for b in bv if b.get("mbit") != "ERR"]
for d in "du":
    x = [int(b["mbit"]) / int(b["rate"]) for b in okb if b["dir"] == d]
    if x: print("bg %s: n=%d, delivered/target mean %.2f, min %.2f" % (d, len(x), st.mean(x), min(x)))
print("bg runs %d, ERR %d" % (len(bv), len(bv) - len(okb)))
# concurrency of bg users over time
if B:
    iv = [(int(p[0]), int(p[1])) for p in B]
    conc = [sum(a <= t < b for a, b in iv) for t in range(60, max(b for _, b in iv), 10)]
    print("bg concurrent: mean %.1f min %d max %d" % (st.mean(conc), min(conc), max(conc)))
# crowd
F = [float(x) for x in rd("fails.txt") if x.strip()]
print("crowd fails total %d, inside windows %d, outside %d: %s" % (len(F), sum(inw(f) for f in F), sum(not inw(f) for f in F), [round(f) for f in F if not inw(f)][:30]))
M = [l.split() for l in rd("min.txt")]
c99 = [(int(m[0]), float(m[3])) for m in M if len(m) >= 7 and m[3] != "-"]
oc = [v for m, v in c99 if not inw(m * 60) and not inw(m * 60 + 59)]
print("crowd p99 per minute: median %.1f (outside windows %.1f, n=%d), max %.1f" % (st.median([v for _, v in c99]), st.median(oc) if oc else -1, len(oc), max(v for _, v in c99)))
for col, nm in ((5, "call"), (6, "game")):
    x = [float(m[col]) for m in M if len(m) >= 7 and m[col] != "-"]
    if x: print("%s p99 per minute: median %.1f max %.1f" % (nm, st.median(x), max(x)))
print("panics ir/kh:", rd("ir.panics"), rd("kh.panics"))
sl = rd("stripe.log")
print("ir stripe.log: dlfair lines %d (released %d, past cap %d); 'download stripe at' %d; 'striped over' %d" % (
    sum("dlfair:" in l for l in sl), sum("fast slot released" in l for l in sl), sum("past the cap" in l for l in sl),
    sum("download stripe at" in l for l in sl), sum("striped over" in l for l in sl)))
print("result:", " ".join(rd("result.json"))[:600])
