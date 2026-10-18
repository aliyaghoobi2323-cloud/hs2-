# sumall.py RESDIR [section...]: the decision tables, base / n4 / fin (delay), median [runs]
#   single: one download (dl) or upload (ul): T Mbit/s, rs3 = largest resend % of a 3 s window,
#           rs = resend % over the window, p50/p99 interactive ms, cpu ms/MB; then the targets
#   multi, chg (speed change), stall, lm
import json, sys, os, statistics as st, importlib.util
R0 = sys.argv[1]; secs = sys.argv[2:] or ["single", "multi", "chg", "stall", "lm"]
spec = importlib.util.spec_from_file_location("anal", os.path.join(os.path.dirname(os.path.abspath(__file__)), "anal.py"))
src = open(spec.origin).read().split("allv = ")[0].replace("args = sys.argv[1:]", "args = []")
A = {}; exec(src, A)
met = A["met"]
def load(*names):
    R = {}
    for n in names:
        f = os.path.join(R0, n + ".jsonl")
        if not os.path.exists(f): continue
        for l in open(f):
            if l.strip():
                d = json.loads(l)["r"]; R.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
    return R
V = ("base", "n4", "fin")
def med(xs):
    xs = [x for x in xs if x is not None]
    return st.median(xs) if xs else None
def cell(xs, nd=None):
    xs = [x for x in xs if x is not None]
    if not xs: return "-"
    m = st.median(xs)
    if nd is None: nd = 0 if abs(m) >= 100 else 1
    s = f"{m:.{nd}f}"
    if len(xs) > 1: s += "[" + " ".join(f"{x:.{nd}f}" for x in xs) + "]"
    return s
def name(scen):
    k, p = scen.split("@"); k = k.split("+")[0]
    q = p.split("-"); s = f"{q[0]}/{q[1]}"
    for x in q[2:]:
        if x.startswith("q"): s += f" q{x[1:]}ms"
        if x.startswith("l"): s += f" loss{x[1:]}%"
    return k.replace("dl1i", "dl").replace("ul1i", "ul") + " " + s
def order(scen):
    k, p = scen.split("@"); q = p.split("-")
    return (k.split("+")[0], int(q[0]), int(q[1]), p)
def rows(R, scen, keys):
    out = {}
    for v in V:
        ds = sorted(R[scen].get(v, []), key=lambda d: d["rep"])
        ms = [met(d) for d in ds]
        out[v] = {k: [m.get(k) for m in ms] for k in keys}; out[v]["n"] = len(ds)
    return out
if "single" in secs:
    R = load("FA", "FB")
    keys = ["T", "rs3", "rs", "p50", "p99", "cpu"]
    print("== single transfer (50 ms queue unless noted): base | n4 | delay ; targets for delay: G=gain kept>=80% of N4's, R=rs3<=base+3, L=p50&p99<=1.3x base, S=not slower than base, C=cpu<=1.1x base")
    for scen in sorted(R, key=order):
        if not scen.split("+")[0] in ("dl1i", "ul1i"): continue
        o = rows(R, scen, keys)
        print(name(scen) + f"  (n={o['base']['n']}/{o['n4']['n']}/{o['fin']['n']})")
        for k in keys:
            print(f"   {k:<4} " + " | ".join(cell(o[v][k]) for v in V))
        m = {v: {k: med(o[v][k]) for k in keys} for v in V}
        fl = []
        b, n, f = m["base"], m["n4"], m["fin"]
        if None not in (b["T"], n["T"], f["T"]):
            if n["T"] > 1.1 * b["T"]:
                g = (f["T"] - b["T"]) / (n["T"] - b["T"]); fl.append(f"G {100*g:.0f}% {'ok' if g >= 0.8 else 'FAIL'}")
            else: fl.append("G n/a (N4 gains <10%)")
            fl.append(f"S {f['T']/b['T']:.2f}x {'ok' if f['T'] >= 0.95*b['T'] else 'FAIL'}")
        if None not in (b["rs3"], f["rs3"]): fl.append(f"R {f['rs3']-b['rs3']:+.1f}pp {'ok' if f['rs3'] <= b['rs3']+3 else 'FAIL'}")
        if None not in (b["p50"], f["p50"], b["p99"], f["p99"]):
            r50, r99 = f["p50"]/b["p50"], f["p99"]/b["p99"]
            fl.append(f"L {r50:.2f}x/{r99:.2f}x {'ok' if max(r50, r99) <= 1.3 else 'FAIL'}")
        if None not in (b["cpu"], f["cpu"]): fl.append(f"C {f['cpu']/b['cpu']:.2f}x {'ok' if f['cpu'] <= 1.1*b['cpu'] else 'FAIL'}")
        print("   => " + "; ".join(fl))
if "multi" in secs:
    R = load("FA", "FM")
    keys = ["T", "rs3", "p50", "p99", "cpu", "heap"]
    print("== several transfers on one link")
    for scen in sorted(R, key=order):
        if not scen.split("+")[0] in ("dl4", "dlul", "dl8", "dl16"): continue
        o = rows(R, scen, keys)
        print(name(scen) + f"  (n={o['base']['n']}/{o['n4']['n']}/{o['fin']['n']})")
        for k in keys: print(f"   {k:<4} " + " | ".join(cell(o[v][k]) for v in V))
if "chg" in secs:
    R = load("FC")
    print("== speed change 300 -> 50 -> 300 Mbit/s (6-18 s at 300, 18-30 at 50, 30-42 back at 300): Mbit/s per phase, rs3, resend % in the 50 phase, p50/p99")
    for scen in sorted(R, key=order):
        print(name(scen))
        for v in V:
            ds = sorted(R[scen].get(v, []), key=lambda d: d["rep"])
            ph = {"pre": [], "low": [], "post": [], "back80": [], "rs3": [], "rslow": [], "p50": [], "p99": []}
            for d in ds:
                up = (d.get("upSum") or 0) > 0 and not d.get("fastSum")
                f1 = d.get("up1s") if up else d.get("fast1s")
                f1 = f1 or []
                ph["pre"].append(st.mean(f1[7:18]) if len(f1) > 18 else None)
                ph["low"].append(st.mean(f1[19:30]) if len(f1) > 30 else None)
                ph["post"].append(st.mean(f1[31:42]) if len(f1) > 33 else None)
                pre = ph["pre"][-1]
                ph["back80"].append(next((i - 30 for i in range(30, len(f1)) if pre and f1[i] >= 0.8 * pre), None))
                m = met(d); ph["rs3"].append(m.get("rs3")); ph["p50"].append(m.get("p50")); ph["p99"].append(m.get("p99"))
                rt, so = (d.get("trRtE"), d.get("trSoE")) if up else (d.get("trRt"), d.get("trSi"))
                if rt and len(rt) > 30:
                    dr = rt[30]-rt[18]; ds_ = so[30]-so[18]
                    ph["rslow"].append(100*dr/max(1, (ds_ if up else ds_+dr)))
            print(f"   {v:<5} n={len(ds)} " + "  ".join(f"{k}={cell(x)}" for k, x in ph.items()))
if "stall" in secs:
    R = load("FS")
    keys = ["stall", "T", "Smin", "p99", "ended", "held", "xtra", "heap"]
    print("== stall matrix (q100 ms): stall = seconds the fast user was under 1 Mbit/s; T fast Mbit/s; Smin slowest slow reader; ended = relays the guard closed")
    for scen in sorted(R, key=lambda s: (s.split("@")[0], order(s)[1:])):
        o = rows(R, scen, keys)
        print(name(scen).replace(" q100ms", "") + f"  (n={o['base']['n']}/{o['n4']['n']}/{o['fin']['n']})  " +
              "  ".join(f"{k}=" + " | ".join(cell(o[v][k]) for v in V) for k in keys))
if "lm" in secs:
    R = load(*(sys.argv[3:] or ["FLP"])) if False else load("FLP")
    print("== full edge stack (z5lm, real LinkManager + RunKharej), lone transfer 130 s: mean Mbit/s per run, cuts, verdicts")
    for scen in sorted(R, key=order):
        print(name(scen))
        for v in V:
            ds = sorted(R[scen].get(v, []), key=lambda d: d["rep"])
            if not ds: continue
            cells, cuts, verd = [], 0, {}
            for d in ds:
                for u in d["users"]:
                    m = u["mbit1s"]; mean = sum(m)/max(1, len(m)); cut = u["endedAtS"] >= 0; cuts += cut
                    cells.append(f"{mean:.0f}" + (f"(cut@{u['endedAtS']:.0f}s)" if cut else ""))
                for x in d["lines"]:
                    if "edge:" not in x and "exit:" not in x: continue
                    import re
                    for w, pat in (("degraded", r"link \d+ degraded \("), ("stuck", r"link \d+ stuck:"), ("poor", r"poor"), ("closed-degraded", r"degraded for .* closed with")):
                        if re.search(pat, x): verd[w] = verd.get(w, 0) + 1
            print(f"   {v:<5} runs={len(ds)} Mbit/s [{' '.join(cells)}] cuts={cuts} verdict-lines={verd or 'none'}")
