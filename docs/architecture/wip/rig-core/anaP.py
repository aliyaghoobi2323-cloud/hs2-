# anaP.py SET[,SET...] [CORE]: the z12 pilot table, median [runs] per variant, with the acceptance
# targets of WINDOW_CORE §9.4 for CORE (default: the highest coreK in the sets) against base,
# N4's gain from any set that has n4 (P0: N4 with Z5_TXQ=10000).
import sys, re, statistics as st
sys.path.insert(0, "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z12")
from z11lib import *
from anaB import chg, wctrace

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

ORDER = ["dl1i@300-150", "ul1i@300-150", "dl1i@300-150-q200", "dl1i@1000-100", "dl1i@100-100", "ul1i@100-100",
         "dl1i@100-50", "dl1i@300-150-l0.1", "dl1i@1000-100-l0.1", "dlchg@300-150", "dl4@300-150", "dl4@1000-100"]

def table(sets, core=None, n4sets=("P0",)):
    R = load(*sets)
    N = load(*n4sets) if n4sets else []
    by = {}
    for d in R:
        by.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
    n4 = {}
    for d in N:
        if d["ver"] == "n4":
            n4.setdefault(d["scen"], []).append(basic(d)["T"])
    if core is None:
        cs = sorted({v for s in by.values() for v in s if re.fullmatch(r"core\d", v)})
        core = cs[-1] if cs else None
    out = []
    for sc in ORDER + sorted(s for s in by if s not in ORDER):
        if sc not in by:
            continue
        V = by[sc]
        out.append(f"== {sc}  " + " ".join(f"{v}={len(V[v])}" for v in sorted(V)))
        M = {}
        for v in sorted(V):
            ds = sorted(V[v], key=lambda d: d["rep"])
            ms = [basic(d) for d in ds]
            M[v] = {k: [m.get(k) for m in ms] for k in ["T", "rs", "rs3", "p50", "p99", "cpu"]}
            loads = []
            out.append(f"   {v:<6} " + "  ".join(f"{k}={cell(M[v][k], 2 if k in ('rs', 'rs3') else None)}" for k in M[v]))
            if sc.startswith("dlchg"):
                cs_ = [chg(d) for d in ds]
                out.append("          chg: " + "  ".join(f"{k}={cell([c.get(k) for c in cs_])}" for k in ["pre", "low", "post", "back90", "rslow", "rsall"]))
        if "base" in M and core in M:
            b = {k: med(v) for k, v in M["base"].items()}
            c = {k: med(v) for k, v in M[core].items()}
            fl = []
            g4 = med(n4.get(sc, []))
            if g4 and b["T"] and c["T"]:
                if g4 > 1.1 * b["T"]:
                    share = 0.6 if "-l" in sc else 0.8
                    g = (c["T"] - b["T"]) / (g4 - b["T"])
                    fl.append(f"gain {100*g:.0f}% of N4's ({'ok' if g >= share else 'FAIL'} ≥{100*share:.0f}%)")
                else:
                    fl.append(f"N4 no gain; {c['T']/b['T']:.2f}x base ({'ok' if c['T'] >= 0.95*b['T'] else 'FAIL'})")
            elif b["T"] and c["T"]:
                fl.append(f"{c['T']/b['T']:.2f}x base")
            if b["rs"] is not None and c["rs"] is not None:
                lim = b["rs"] + (0.5 if b["rs"] < 0.5 or "-l" in sc else 3)
                if sc.startswith("dl4"):
                    lim = b["rs"]
                fl.append(f"rs {c['rs']:.2f} vs base {b['rs']:.2f} ({'ok' if c['rs'] <= lim + 1e-9 else 'FAIL'} ≤{lim:.2f})")
            if b["p50"] and c["p50"] and b["p99"] and c["p99"]:
                r50, r99 = c["p50"] / b["p50"], c["p99"] / b["p99"]
                fl.append(f"lat {r50:.2f}x/{r99:.2f}x ({'ok' if max(r50, r99) <= 1.3 else 'FAIL'})")
            if b["cpu"] and c["cpu"]:
                fl.append(f"cpu {c['cpu']/b['cpu']:.2f}x ({'ok' if c['cpu'] <= 1.1 * b['cpu'] else 'FAIL'})")
            out.append(f"   => {core}: " + "; ".join(fl))
    return out

if __name__ == "__main__":
    sets = sys.argv[1].split(",")
    core = sys.argv[2] if len(sys.argv) > 2 else None
    print("\n".join(table(sets, core)))
