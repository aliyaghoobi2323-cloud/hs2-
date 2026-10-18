# anal.py FILE... [-v v1,v2,...] [-k keys]: per scenario, per version: median [runs] of
#   T: Mbit/s of the fast transfer (F, or U for an upload, or F+U for dlul)
#   rs: resends % over the window (the sender of the bulk direction: retrans/segs out)
#   rs3: the largest resend share of a 3 s window (Z5_TRACE), as the loss rule counts it
#   p50/p99: interactive reply ms; cpu: whole process ms per MB; hcpu: without the emulator's threads
import json, sys, statistics as st
args = sys.argv[1:]
def opt(n, d):
    if n in args:
        i = args.index(n); v = args[i+1]; del args[i:i+2]; return v
    return d
vs = opt("-v", "")
keys = opt("-k", "T,rs,rs3,p50,p99,cpu,hcpu").split(",")
R = {}
for f in args:
    for l in open(f):
        if l.strip():
            d = json.loads(l)["r"]; R.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
def rs3(d, up):
    if up:
        rt, so = d.get("trRtE"), d.get("trSoE")
        if not rt: return None
        w0 = int(float(d["env"].get("Z5_WARM_S", 6)))
        best = 0
        for i in range(w0 + 3, len(rt), 3):
            ds = so[i]-so[i-3]
            if ds > 1000: best = max(best, (rt[i]-rt[i-3]) / ds)
        return 100*best
    rt, si = d.get("trRt"), d.get("trSi")
    if not rt: return None
    w0 = int(float(d["env"].get("Z5_WARM_S", 6)))
    best = 0
    for i in range(w0 + 3, len(rt), 3):
        dr, ds = rt[i]-rt[i-3], si[i]-si[i-3]
        if ds + dr > 1000: best = max(best, dr / (ds + dr))
    return 100*best
def met(d):
    o = {}
    F, U = d.get("fastSum", 0) or 0, d.get("upSum", 0) or 0
    up = U > 0 and F == 0
    o["T"] = F + U
    o["F"], o["U"] = F, U
    i = 0 if up else 1
    if d.get("segsOut") and d["segsOut"][i] > 0:
        o["rs"] = 100 * d["retrans"][i] / d["segsOut"][i]
    if F > 0 and U > 0:
        o["rs"] = max(100 * d["retrans"][k] / max(1, d["segsOut"][k]) for k in (0, 1))
    o["rs3"] = rs3(d, up)
    if d["int"]["n"] > 0:
        o["p50"], o["p99"], o["p90"] = d["int"]["p50"], d["int"]["p99"], d["int"]["p90"]
    dur = float(d["env"].get("Z5_DUR_S", 15))
    mb = (F + U + (d.get("ftsSum", 0) or 0)) * dur / 8
    if mb > 0:
        o["cpu"] = 1000 * d["cpuS"] / mb
        if "emuCpuS" in d: o["hcpu"] = 1000 * (d["cpuS"] - d["emuCpuS"]) / mb
    w0 = int(float(d["env"].get("Z5_WARM_S", 6)))
    f1 = d.get("fast1s") or []
    if f1: o["stall"] = sum(1 for v in f1[w0:] if v < 1)
    o["parks"] = sum(d["parks"]); o["borr"] = sum(d.get("borrows", [0, 0])); o["ended"] = d.get("relaysEnded", 0)
    o["pauses"] = sum(d.get("pauses", [0, 0])); o["xtra"] = max(d.get("extraMaxMiB", [0, 0])); o["held"] = max(d["heldMaxMiB"])
    o["heap"] = d["heapInuseMaxMiB"]
    if d.get("slowMbit"): o["Smin"] = min(d["slowMbit"])
    return o
def cell(xs, runs=True):
    xs = [x for x in xs if x is not None]
    if not xs: return "-"
    md = st.median(xs); nd = 0 if abs(md) >= 100 else 1
    s = f"{md:.{nd}f}"
    if runs and len(xs) > 1: s += "[" + " ".join(f"{x:.{nd}f}" for x in xs) + "]"
    return s
allv = vs.split(",") if vs else sorted({v for s in R.values() for v in s})
for scen in sorted(R):
    print(scen)
    for v in allv:
        ds = sorted(R[scen].get(v, []), key=lambda d: d["rep"])
        if not ds: continue
        ms = [met(d) for d in ds]
        print(f"   {v[:10]:<10} n={len(ds)} " + "  ".join(f"{k}={cell([x.get(k) for x in ms])}" for k in keys))
