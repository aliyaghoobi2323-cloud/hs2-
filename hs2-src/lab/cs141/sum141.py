#!/usr/bin/env python3
# sum141.py [RESDIR]: cs141 A/B — the test download (Mbit/s, per-second, when it stripes) among background
# users, the background users' total, light p99 per class. Stripe time: the first 1 s interval at
# >= 1.5x the median of intervals 1-3 (one link) or a "download stripe at" line of a new mconn.
import json, os, sys, glob, statistics as st, datetime
R = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/cs141/res"
PFX = sys.argv[2] if len(sys.argv) > 2 else "pq"
rows = []
for t in sorted(os.listdir(R)):
    d = os.path.join(R, t)
    if not os.path.isfile(os.path.join(d, "result.json")) or t[0] not in PFX: continue
    arm = t[-1]
    try: res = json.load(open(os.path.join(d, "result.json")))
    except Exception: print(t, "MISSING result"); continue
    try:
        h = json.load(open(os.path.join(d, "heavy.json")))
        iv = [i["sum"]["bits_per_second"] / 1e6 for i in h["intervals"]]
        mb = h["end"]["sum_received"]["bits_per_second"] / 1e6
    except Exception:
        print(t, "test iperf3 missing (crash)"); continue
    base = st.median(iv[1:4]); ts = next((i for i, v in enumerate(iv) if i >= 1 and v >= 1.5 * base and v > 300), None)
    bg = []
    for f in sorted(glob.glob(os.path.join(d, "bg*.json"))):
        try: bg.append(json.load(open(f))["end"]["sum_received"]["bits_per_second"] / 1e6)
        except Exception: bg.append(None)
    hs = float(open(os.path.join(d, "hstart")).read())
    hsd = datetime.datetime.fromtimestamp(hs).strftime("%H:%M:%S")
    dl = [l for l in open(os.path.join(d, "stripe.log")) if "download stripe at" in l and l[:8] >= hsd]
    fair = sum(1 for l in open(os.path.join(d, "stripe.log")) if "dlfair" in l)
    seen = {}; hsf = datetime.datetime.fromtimestamp(hs).strftime("%H:%M:%S.%f")
    for l in open(os.path.join(d, "stripe.log")):
        if "striped over" in l: seen.setdefault(l.split()[2], l[:15])
    new = sorted(v for v in seen.values() if v >= hsf)
    def secs(s): h, mi, x = s.split(":"); return int(h) * 3600 + int(mi) * 60 + float(x)
    ns = round(secs(new[0]) - secs(hsf[:15]), 1) if new else None
    p = res.get("probe", {}); c = p.get("crowd", {})
    rows.append(dict(t=t, arm=arm, ns=ns, nnew=len(new), mb=mb, iv=iv, ts=ts, bg=bg, nbg=len(bg), dl=len(dl), fair=fair,
        crowd=c.get("p99_ms", 0), fail=c.get("fail", 0), call=p.get("call", {}).get("p99_ms", 0),
        game=p.get("game", {}).get("p99_ms", 0), echo=p.get("echo_p99_ms", 0)))
for r in rows:
    print("%-5s test %6.0f newstripe@%-5s(%d) stripe@%-4s dlfast-after-start %d rel %d | bg n=%d sum %6.0f | crowd %5.1f f%d call %5.1f game %5.1f echo %5.1f | %s" % (
        r["t"], r["mb"], r["ns"], r["nnew"], r["ts"], r["dl"], r["fair"], r["nbg"], sum(x or 0 for x in r["bg"]), r["crowd"], r["fail"], r["call"], r["game"], r["echo"],
        " ".join("%.0f" % v for v in r["iv"])))
def q(v, p): v = sorted(v); return v[min(len(v) - 1, int(p * (len(v) - 1) + 0.5))]
for a in sorted(set(r["arm"] for r in rows)):
    g = [r for r in rows if r["arm"] == a]; m = [r["mb"] for r in g]
    tsv = [r["ts"] for r in g if r["ts"] is not None]
    nsv = [r["ns"] for r in g if r["ns"] is not None]
    print("ARM %s n=%d test mean %.0f med %.0f p10 %.0f min %.0f | rate-jump %d/%d (%.0f%%) @med %s | new-mconn-striped %d/%d @med %s | bg sum mean %.0f | p99 med crowd %.1f call %.1f game %.1f echo %.1f fails %d" % (
        a, len(g), st.mean(m), st.median(m), q(m, .1), min(m), len(tsv), len(g), 100 * len(tsv) / len(g),
        st.median(tsv) if tsv else "-", len(nsv), len(g), st.median(nsv) if nsv else "-", st.mean(sum(x or 0 for x in r["bg"]) for r in g),
        st.median(r["crowd"] for r in g), st.median(r["call"] for r in g), st.median(r["game"] for r in g), st.median(r["echo"] for r in g), sum(r["fail"] for r in g)))
# paired: same background set
P = {}
for r in rows: P.setdefault(r["t"][:3], {})[r["arm"]] = r
A = "X" if any("X" in v for v in P.values()) else "F"
d = [(v[A]["mb"] - v["K"]["mb"], sum(x or 0 for x in v[A]["bg"]) - sum(x or 0 for x in v["K"]["bg"])) for v in P.values() if A in v and "K" in v]
if d: print("paired " + A + "-K n=%d test diff mean %+.0f med %+.0f (F wins %d) | bg sum diff mean %+.0f" % (len(d), st.mean(x[0] for x in d), st.median(x[0] for x in d), sum(x[0] > 0 for x in d), st.mean(x[1] for x in d)))
