# table.py FILE... -k METRIC[,METRIC2] [-p prefix] [-v base,need,needf1,s632]: one line per scenario,
# median [runs] of each metric per variant
import json, sys, statistics as st
sys.argv.pop(0)
args = sys.argv
def opt(name, default):
    if name in args:
        i = args.index(name); v = args[i+1]; del args[i:i+2]; return v
    return default
keys = opt("-k", "F").split(",")
pfx = opt("-p", "")
vs = opt("-v", "base,need,needf1,s632").split(",")
runs = opt("-r", "0") == "1"

rows = []
for f in args:
    rows += [json.loads(l) for l in open(f) if l.strip()]
R = {}
for row in rows:
    d = row["r"]
    R.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
import importlib.util
spec = importlib.util.spec_from_file_location("anal_met", __file__.replace("table.py", "anal_met.py"))
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
def f1(xs):
    xs = [x for x in xs if x is not None]
    if not xs: return "-"
    md = st.median(xs)
    nd = 0 if abs(md) >= 100 else 1
    s = f"{md:.{nd}f}"
    if runs: s += "[" + " ".join(f"{x:.{nd}f}" for x in xs) + "]"
    return s
print("scen".ljust(26) + "".join(f"{v+':'+'/'.join(keys):>{14 if not runs else 26}}" for v in vs))
for scen in sorted(R):
    if not scen.startswith(pfx): continue
    line = scen.ljust(26)
    for v in vs:
        ds = sorted(R[scen].get(v, []), key=lambda d: d["rep"])
        ms = [m.met(d) for d in ds]
        cell = "/".join(f1([x.get(k) for x in ms]) for k in keys) + f"({len(ds)})"
        line += f"{cell:>{14 if not runs else 26}}"
    print(line)
