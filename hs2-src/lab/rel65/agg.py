import sys, time
fi, mo, fo = sys.argv[1:4]
# rel65: probe0 = wall-clock origin of the probe's sample seconds (min of now - s over the first 120 s
# of samples; pipe delay only adds lag), so rel65view.sh can map probe seconds onto the soak clock (t0).
p0f = mo.rsplit("/", 1)[0] + "/probe0"; p0 = None
M = {}; top = 0.0
mout = open(mo, "a", buffering=1); fout = open(fo, "a", buffering=1)
def q(v, p):
    if not v: return "-"
    v.sort(); return "%.1f" % v[min(len(v) - 1, int(len(v) * p))]
def emit(m):
    d = M.pop(m); c = d.get("c", [])
    mout.write("%d %d %s %s %d %s %s\n" % (m, len(c), q(c, .5), q(c, .99), d.get("F", 0), q(d.get("k", []), .99), q(d.get("g", []), .99)))
for ln in open(fi):
    p = ln.split()
    if len(p) < 3: continue
    try: s = float(p[1]); v = float(p[2])
    except ValueError: continue
    if s < 120:
        o = time.time() - s
        if p0 is None or o < p0 - 0.05:
            p0 = o
            with open(p0f, "w") as f: f.write("%.3f\n" % p0)
    m = int(s // 60); d = M.setdefault(m, {})
    if p[0] == "F":
        d["F"] = d.get("F", 0) + 1; fout.write("%.1f\n" % s)
    else: d.setdefault(p[0], []).append(v)
    if s > top + 1:
        top = s
        for k in sorted(M):
            if k < int((top - 15) // 60): emit(k)
for k in sorted(M): emit(k)
