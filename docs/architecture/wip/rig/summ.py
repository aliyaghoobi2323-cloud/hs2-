# summ.py FILE... [-k m1,m2] [-v base,n4,pf]: per scenario, per metric, each
# version's median [runs]
import json, sys, statistics as st, importlib.util
args = sys.argv[1:]
def opt(n, d):
    if n in args:
        i = args.index(n); v = args[i+1]; del args[i:i+2]; return v
    return d
keys = opt("-k", "F,U,rsAll,rsMax,ip50,ip99,cpuMB").split(",")
vs = opt("-v", "base,n4,pf").split(",")
spec = importlib.util.spec_from_file_location("m", __file__.replace("summ.py", "anal_met.py"))
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
R = {}
for f in args:
    for l in open(f):
        if not l.strip(): continue
        d = json.loads(l)["r"]
        R.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
def fmt(x):
    return "-" if x is None else (f"{x:.0f}" if abs(x) >= 100 else f"{x:.1f}")
for scen in sorted(R):
    print(scen)
    for k in keys:
        cells = []
        for v in vs:
            ds = sorted(R[scen].get(v, []), key=lambda d: d["rep"])
            xs = [m.met(d).get(k) for d in ds]
            xs = [x for x in xs if x is not None]
            if not xs: cells.append(f"{v}: -"); continue
            cells.append(f"{v}: {fmt(st.median(xs))} [" + " ".join(fmt(x) for x in xs) + "]")
        if all(c.endswith(": -") for c in cells): continue
        print(f"  {k:<7} " + " | ".join(cells))
