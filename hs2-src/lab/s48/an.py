#!/usr/bin/env python3
"""an.py RESDIR — s48 matrix: per cell (DIR CAR FARM) medians [min-max] and n."""
import json, os, re, statistics as st, sys
D = sys.argv[1] if len(sys.argv) > 1 else "/opt/hs2lab/r48/res/"
g = {}
for n in sorted(os.listdir(D)):
    m = re.match(r"([dr])(mtcp|l3mtcp|tls)-(tls|reality|both)-(m|s|k|sc)-(\d+)$", n) or \
        re.match(r"()()(A|S)(r128|reality|tls)-(\d+)$", n)
    if not m:
        continue
    if m.group(1) == "":
        cell = ("d", "l3mtcp", m.group(4)); var = "m" if m.group(3) == "A" else "s"
    else:
        cell = (m.group(1), m.group(2), m.group(3)); var = m.group(4)
    p = os.path.join(D, n)
    try:
        r = json.load(open(p + "/result.json"))
    except Exception:
        g.setdefault(cell, {}).setdefault(var, []).append({"broken": 1}); continue
    h = r.get("heavy") or {}; pr = r.get("probe") or {}; c = pr.get("crowd") or {}; k = r.get("kill") or {}
    d = dict(mbps=h.get("mbps"), c95=c.get("p95_ms"), c99=c.get("p99_ms"), cfail=c.get("fail"),
             ping99=r.get("ping_p99"), t50=k.get("rebuild_t50_s"), t90=k.get("rebuild_t90_s"),
             links=r.get("links_max"), call99=(pr.get("call") or {}).get("p99_ms"), game99=(pr.get("game") or {}).get("p99_ms"))
    try:
        pt = open(p + "/probe.txt").read()
        d["probe_tgt"] = pt.count("CN = www.lab-target.test"); d["probe_our"] = pt.count("CN = lab.example.com")
    except Exception:
        pass
    probs = 0
    for f in ("doctor-L.txt", "doctor-D.txt"):
        try:
            mm = re.search(r"(\d+) problems?", open(p + "/" + f).read()); probs += int(mm.group(1)) if mm else 99
        except Exception:
            probs += 99
    d["doctor_problems"] = probs
    try:
        d["restart"] = json.load(open(p + "/restart.json"))
    except Exception:
        pass
    if var == "sc":
        rows = [json.loads(l) for l in open(p + "/d-status.jsonl") if l.strip()]
        d["sc_rows"] = rows
    g.setdefault(cell, {}).setdefault(var, []).append(d)

def ms(v):
    v = [x for x in v if isinstance(x, (int, float)) and x != -1]
    return "%.0f [%.0f-%.0f] n%d" % (st.median(v), min(v), max(v), len(v)) if v else "-"
def ms1(v):
    v = [x for x in v if isinstance(x, (int, float)) and x != -1]
    return "%.1f [%.1f-%.1f]" % (st.median(v), min(v), max(v)) if v else "-"
print("| dir | carrier | front | Mbps x8 (m) | Mbps x1 (s) | crowd p95 | crowd p99 | ping p99 | rebuild t50/t90 s | probe tgt/our | doctor problems | cold restart first/90% s |")
print("|---|---|---|---|---|---|---|---|---|---|---|---|")
for cell in sorted(g):
    G = g[cell]; M = G.get("m", []); S = G.get("s", []); K = G.get("k", [])
    rs = [x.get("restart") or {} for x in K]
    print("| %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s | %s | %s |" % (
        cell[0], cell[1], cell[2], ms([x.get("mbps") for x in M]), ms([x.get("mbps") for x in S]),
        ms([x.get("c95") for x in M]), ms([x.get("c99") for x in M]), ms([x.get("ping99") for x in M]),
        ms1([x.get("t50") for x in M]), ms1([x.get("t90") for x in M]),
        "/".join(str(sum(x.get(k, 0) for x in M + K)) for k in ("probe_tgt", "probe_our")),
        sum(x.get("doctor_problems", 0) for x in M + S + K),
        ", ".join("%s/%s" % (r.get("first_link_s"), r.get("links90_s")) for r in rs) or "-"))
    br = sum(1 for v in G.values() for x in v if x.get("broken"))
    if br:
        print("|  |  | broken runs: %d |" % br)
for cell in sorted(g):
    for x in g[cell].get("sc", []):
        if "sc_rows" not in x:
            continue
        rows = x["sc_rows"]
        def avg(t0, t1):
            rr = [r for r in rows if t0 <= r["t"] < t1 and r.get("lb")]
            if not rr: return None
            n = len(rr[0]["lb"])
            return [round(sum(r["lb"][i][1] for r in rr) / len(rr), 1) for i in range(n)], [r["lb"][i][3] for r in rr[-1:] for i in range(n)]
        print("SC", cell, "links avg 6-14s", avg(6, 14), "18-30s", avg(18, 30), "38-46s", avg(38, 46), "mbps", x.get("mbps"), "c99", x.get("c99"))
        st_ = [(r["t"], [p[3] for p in r["lb"]]) for r in rows if r.get("lb")]
        ch = []; last = None
        for t, s in st_:
            if s != last: ch.append((t, s)); last = s
        print("   states:", ch)
