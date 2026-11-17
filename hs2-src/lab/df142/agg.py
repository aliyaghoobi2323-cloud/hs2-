import sys
fi, mo, fo = sys.argv[1:4]
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
    m = int(s // 60); d = M.setdefault(m, {})
    if p[0] == "F":
        d["F"] = d.get("F", 0) + 1; fout.write("%.1f\n" % s)
    else: d.setdefault(p[0], []).append(v)
    if s > top + 1:
        top = s
        for k in sorted(M):
            if k < int((top - 15) // 60): emit(k)
for k in sorted(M): emit(k)
