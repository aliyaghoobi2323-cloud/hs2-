# an.py RESDIR...: wj178 summary per arm
import json, re, sys, glob, os
for d in sys.argv[1:]:
    print("==", d, open(d + "/env.txt").read().strip()[:120])
    try:
        r = json.loads(open(d + "/result.json").read()); p = r.get("probe") or {}
    except Exception as e:
        r, p = {}, {}; print("  no result", e)
    c = p.get("crowd", {}); g = p.get("game") or {}
    print("  crowd p50/p99/fail", c.get("p50_ms"), c.get("p99_ms"), c.get("fail"), "| game p99", g.get("p99_ms"), "| ping p50/p99", r.get("ping_p50"), r.get("ping_p99"))
    hv = []
    for f in sorted(glob.glob(d + "/hv-d-*.json"), key=lambda s: int(re.findall(r"(\d+)\.json", s)[0])):
        try: hv.append(round(json.load(open(f))["end"]["sum_received"]["bits_per_second"] / 1e6))
        except Exception: hv.append(None)
    print("  heavy down x4 Mbit/s", hv)
    ev = [json.loads(l) for l in open(d + "/calls.jsonl")] if os.path.exists(d + "/calls.jsonl") else []
    died = [e for e in ev if e["ev"] == "DIED"]; st = [e for e in ev if e["ev"] == "stall"]
    print("  calls: died", len(died), "stalls>5s", len(st), [(e["i"], e["el"]) for e in died + st][:12])
    kh = open(d + "/kh.log", errors="replace").read().splitlines() if os.path.exists(d + "/kh.log") else []
    cuts = [l for l in kh if "lb auto2: path" in l and " cut (" in l]
    recs = [l for l in kh if "recovering (" in l]
    print("  exit cuts", len(cuts), "recoveries", len(recs))
    jumps = []
    for l in kh:
        if "lb auto2: weight factor" not in l: continue
        for m in re.finditer(r"(\S+) (\d\.\d\d)→(\d\.\d\d) \(([^)]*)\)", l):
            a, b = float(m.group(2)), float(m.group(3))
            if b > a: jumps.append((round(b - a, 2), m.group(1)[-22:], a, b, m.group(4)[:30]))
    nchg = sum(len(re.findall(r"\d\.\d\d→\d\.\d\d", l)) for l in kh if "lb auto2: weight factor" in l)
    noise = sum(1 for l in kh if "lb auto2: weight factor" in l for m in re.finditer(r"1\.00→0\.\d\d \(lower: loss (\d+\.\d)%", l) if float(m.group(1)) < 1.0)
    quiet = sum(len(re.findall(r"192\.168\.5[135]\.1 \d\.\d\d→", l)) for l in kh if "lb auto2: weight factor" in l)
    print("  weight changes", nchg, "(/h %d)" % (nchg * 3600 / 620), "| on event-free paths b,d,f", quiet, "| drops from 1.00 at loss<1%", noise)
    jumps.sort(reverse=True)
    print("  biggest rises", jumps[:6])
    ir = open(d + "/ir.log", errors="replace").read().splitlines() if os.path.exists(d + "/ir.log") else []
    st = [l for l in ir if "lb call stuck:" in l]
    rs = [l for l in ir if "lb rescue" in l]
    rl = [l for l in ir if "copies given past the budget" in l]
    if rl:
        m = re.search(r"past the budget so far (\d+), copy subs moved off (\d+) \(refused by the rate bound (\d+)", rl[-1])
        print("  rescue: past budget %s, copy subs moved off %s, rate-refused %s" % m.groups() if m else "  rescue ?")
    print("  iran 'lb rescue' lines", len(rs), (rs[-1][rs[-1].index("lb rescue"):][:200] if rs else ""))
    print("  stuck:", st[-1][st[-1].index("lb call stuck"):] if st else "(no line)")
