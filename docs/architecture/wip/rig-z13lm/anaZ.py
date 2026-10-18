# anaZ.py SET[,SET...] [-c] [-only PREFIX]: z13 tables (TestZ5Rig runs): per cell and version the
# median [runs] of throughput, resend share (mean of the 3 s windows, and the worst window),
# interactive p50/p99, CPU ms/MB (whole process, and without the emulator's threads), stall
# seconds (bulk transfer < 1 Mbit/s in a second of the window), relays ended (cuts), heap growth
# and the bucket's extra; then the acceptance of WINDOW_CORE §9.4 for core against base (and N4's
# gain). -c: the compact one-line-per-cell table instead; with both, the counts per criterion.
import sys, os, json, statistics as st
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from z11lib import load, mean, window, env

def med(xs):
    xs = [x for x in xs if x is not None]
    return st.median(xs) if xs else None

def cell(xs, nd=None):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "-"
    m = st.median(xs)
    if nd is None:
        nd = 0 if abs(m) >= 100 else 1
    s = f"{m:.{nd}f}"
    if len(xs) > 1:
        s += "[" + " ".join(f"{x:.{nd}f}" for x in xs) + "]"
    return s

def isup(d):
    return (d.get("upSum") or 0) > 0 and not d.get("fastSum")

def rswin(d, up, a=None, b=None):
    """resend shares (%) of the 3 s windows of the bulk direction inside [a, b) s (default: the
    measure window)"""
    w0, w1 = window(d)
    a = int(w0) if a is None else a
    b = int(w1) if b is None else b
    if up:
        rt, so = d.get("trRtE"), d.get("trSoE")
    else:
        rt, so = d.get("trRt"), d.get("trSi")
    out = []
    if not rt:
        return out
    for k in range(a + 3, min(b, len(rt) - 1) + 1, 3):
        dr = rt[k] - rt[k - 3]
        ds = so[k] - so[k - 3]
        den = ds if up else ds + dr
        if den > 1000:
            out.append(100 * dr / den)
    return out

def rsspan(d, up, a, b):
    if up:
        rt, so = d.get("trRtE"), d.get("trSoE")
    else:
        rt, so = d.get("trRt"), d.get("trSi")
    if not rt or len(rt) <= b:
        b = len(rt) - 1 if rt else 0
    if not rt or b <= a:
        return None
    dr, ds = rt[b] - rt[a], so[b] - so[a]
    den = ds if up else ds + dr
    return 100 * dr / den if den > 0 else None

def metrics(d):
    o = {}
    up = isup(d)
    F, U = d.get("fastSum", 0) or 0, d.get("upSum", 0) or 0
    o["T"] = F + U
    if F and U:
        o["Tdl"], o["Tul"] = F, U
    ws = rswin(d, up)
    o["rsm"] = mean(ws) if ws else (0.0 if ws == [] and (d.get("trRt") or d.get("trRtE")) else None)
    o["rs3"] = max(ws) if ws else o["rsm"]
    if F and U:  # both ways: the upload's too
        wu = rswin(d, True)
        o["rsmU"] = mean(wu) if wu else None
    if d["int"]["n"] > 0:
        o["p50"], o["p99"] = d["int"]["p50"], d["int"]["p99"]
    w0, w1 = window(d)
    dur = w1 - w0
    mb = o["T"] * dur / 8
    if mb > 0:
        o["cpu"] = 1000 * d["cpuS"] / mb
        if "emuCpuS" in d:
            o["hcpu"] = 1000 * (d["cpuS"] - d["emuCpuS"]) / mb
    # stall seconds: the bulk transfer under 1 Mbit/s in a second of the window
    f1 = d.get("up1s") if up else d.get("fast1s")
    if F and U:
        f1 = [min(a, b) for a, b in zip(d.get("fast1s") or [], d.get("up1s") or [])]
    if f1:
        o["stall"] = sum(1 for x in f1[int(w0):int(w1)] if x < 1)
    o["cuts"] = d.get("relaysEnded", 0) + len(d.get("errs") or [])
    if d.get("heapInuseMaxMiB") is not None:
        o["heapx"] = d["heapInuseMaxMiB"] - (d.get("heapInuse0MiB") or 0)
    o["extra"] = max(d.get("extraMaxMiB") or [0])
    o["slow"] = sum(d.get("slowMbit") or []) if d.get("slowMbit") else None
    return o

def chg(d):
    """speed change 300 -> 50 -> 300 (18-30 s at 50): per phase Mbit/s and resends, back to 90%
    of the rate before the drop (s after the speed came back)"""
    up = isup(d)
    f1 = d.get("up1s") if up else d.get("fast1s")
    o = {}
    if f1 and len(f1) > 41:
        o["pre"] = mean(f1[7:18]); o["low"] = mean(f1[19:30]); o["post"] = mean(f1[31:42])
        o["back90"] = next((i - 30 for i in range(30, len(f1)) if f1[i] >= 0.9 * o["pre"]), None)
        o["rspre"] = rsspan(d, up, 6, 18)
        o["rslow"] = rsspan(d, up, 18, 30)
        o["rspost"] = rsspan(d, up, 30, 42)
        o["rsall"] = rsspan(d, up, 6, 42)
    return o

COLS = ["T", "rsm", "rs3", "p50", "p99", "cpu", "hcpu", "stall", "cuts", "heapx", "extra"]
ND = {"rsm": 2, "rs3": 2, "rsmU": 2, "stall": 0, "cuts": 0, "extra": 1, "heapx": 0}
SELF_FILL = ("dl1i@100-50", "ul1i@100-50")  # cells the base overfills itself (with dl8/dl16)

def accept(sc, M):
    """acceptance flags for core against base (WINDOW_CORE §9.4); M[v][k] = list of values"""
    if "base" not in M or "core" not in M:
        return [], {}
    b = {k: med(v) for k, v in M["base"].items()}
    c = {k: med(v) for k, v in M["core"].items()}
    n = {k: med(v) for k, v in M["n4"].items()} if "n4" in M else {}
    fl, ok = [], {}
    lossy = "-l" in sc.split("@")[1]
    # gain
    if b.get("T") and c.get("T"):
        g4 = n.get("T")
        if g4 and g4 > 1.1 * b["T"]:
            share = 0.6 if "-l0.1" in sc else 0.8
            g = (c["T"] - b["T"]) / (g4 - b["T"])
            ok["gain"] = g >= share
            fl.append(f"gain {100*g:.0f}% of N4's ({'ok' if ok['gain'] else 'FAIL'} >={100*share:.0f}%)")
        else:
            ok["noslow"] = c["T"] >= 0.95 * b["T"]
            fl.append(f"N4 no gain; core {c['T']/b['T']:.2f}x base ({'ok' if ok['noslow'] else 'FAIL'} >=0.95)")
    # resends (mean of the 3 s windows)
    if b.get("rsm") is not None and c.get("rsm") is not None:
        if sc.startswith("dl4"):
            lim, why = b["rsm"], "<=base"
        elif b["rsm"] < 0.5 and not lossy:
            lim, why = b["rsm"] + 0.5, "base~0: +0.5"
        else:
            lim, why = b["rsm"] + 3, "+3"
        ok["rs"] = c["rsm"] <= lim + 1e-9
        ok["rs3pp"] = c["rsm"] <= b["rsm"] + 3 + 1e-9
        fl.append(f"rs {c['rsm']:.2f} vs {b['rsm']:.2f} ({'ok' if ok['rs'] else 'FAIL'} {why})")
    # latency
    if b.get("p50") and c.get("p50") and b.get("p99") and c.get("p99"):
        r50, r99 = c["p50"] / b["p50"], c["p99"] / b["p99"]
        lim = 1.05 if (sc in SELF_FILL or sc.startswith("dl8") or sc.startswith("dl16")) else 1.3
        ok["lat"] = max(r50, r99) <= lim
        fl.append(f"lat {r50:.2f}x/{r99:.2f}x ({'ok' if ok['lat'] else 'FAIL'} <={lim})")
    # CPU per MB, without the emulator's threads
    if b.get("hcpu") and c.get("hcpu"):
        r = c["hcpu"] / b["hcpu"]
        ok["cpu"] = r <= 1.1
        fl.append(f"cpu {r:.2f}x ({'ok' if ok['cpu'] else 'FAIL'} <=1.1; with emu {c['cpu']/b['cpu']:.2f}x)")
    # stalls and cuts: every core run
    cs = sum(x or 0 for x in M["core"].get("stall", []))
    cc = sum(x or 0 for x in M["core"].get("cuts", []))
    bs = sum(x or 0 for x in M["base"].get("stall", []))
    ok["stall"] = cs == 0 and cc == 0
    fl.append(f"stall/cut core {cs}/{cc} base {bs} ({'ok' if ok['stall'] else 'FAIL'})")
    return fl, ok

def table(sets, compact=False, only=None):
    R = load(*sets)
    by = {}
    for d in R:
        by.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
    order = []
    for d in R:
        if d["scen"] not in order:
            order.append(d["scen"])
    out, counts = [], {}
    for sc in order:
        if only and not sc.startswith(only):
            continue
        V = by[sc]
        M, X = {}, {}
        for v in ("base", "n4", "core"):
            ds = sorted(V.get(v, []), key=lambda d: d["rep"])
            if not ds:
                continue
            ms = [metrics(d) for d in ds]
            keys = COLS + [k for k in ("Tdl", "Tul", "rsmU", "slow") if any(k in m for m in ms)]
            M[v] = {k: [m.get(k) for m in ms] for k in keys}
            if sc.startswith(("dlchg", "ulchg")):
                X[v] = [chg(d) for d in ds]
        fl, ok = accept(sc, M)
        if sc.startswith(("dlchg", "ulchg")) and "core" in X and "base" in X:
            b90 = [c.get("back90") for c in X["core"]]
            okb = all(x is not None and x <= 10 for x in b90)
            ra_c, ra_b = med([c.get("rsall") for c in X["core"]]), med([c.get("rsall") for c in X["base"]])
            ok["chg"] = okb and ra_c is not None and ra_b is not None and ra_c <= ra_b + 3
            fl.append(f"back90 core {b90} ({'ok' if okb else 'FAIL'} <=10 s); rs whole run {ra_c:.2f} vs {ra_b:.2f}")
        for k, v in ok.items():
            c = counts.setdefault(k, [0, 0, []])
            c[0 if v else 1] += 1
            if not v:
                c[2].append(sc)
        if compact:
            def g(v, k, nd=None):
                return cell(M[v][k], nd) if v in M and k in M[v] else "-"
            line = f"{sc:26} | T b {g('base','T')} n4 {g('n4','T')} core {g('core','T')}"
            line += f" | rs b/n/c {g('base','rsm',2)} / {g('n4','rsm',2)} / {g('core','rsm',2)}"
            line += f" | p50 {g('base','p50')}/{g('core','p50')} p99 {g('base','p99')}/{g('core','p99')}"
            out.append(line)
            out.append(f"{'':26}   => " + "; ".join(fl))
            continue
        out.append(f"== {sc}  " + " ".join(f"{v}={len(V[v])}" for v in ("base", "n4", "core") if v in V))
        for v in ("base", "n4", "core"):
            if v not in M:
                continue
            out.append(f"   {v:<5} " + "  ".join(f"{k}={cell(M[v][k], ND.get(k))}" for k in M[v]))
            if v in X:
                out.append("         chg: " + "  ".join(f"{k}={cell([c.get(k) for c in X[v]], 2 if k.startswith('rs') else None)}"
                                                       for k in ["pre", "low", "post", "back90", "rspre", "rslow", "rspost", "rsall"]))
        if fl:
            out.append("   => " + "; ".join(fl))
    return out, counts

if __name__ == "__main__":
    sets = sys.argv[1].split(",")
    compact = "-c" in sys.argv
    only = sys.argv[sys.argv.index("-only") + 1] if "-only" in sys.argv else None
    out, counts = table(sets, compact, only)
    print("\n".join(out))
    print("\n== acceptance counts (ok / FAIL; cells failing)")
    for k, (a, b, fs) in counts.items():
        print(f"   {k:8} {a} / {b}  {' '.join(fs)}")
