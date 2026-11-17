import sys
# samples "KIND SEC VALUE": crowd latencies (c), call (k), game (g), F = crowd fail
fi, mo, fo, lo = sys.argv[1:5]
M = {}
fout = open(fo, "a", buffering=1); lout = open(lo, "a", buffering=1)
for ln in open(fi):
    p = ln.split()
    if len(p) < 3: continue
    try: s = float(p[1]); v = float(p[2])
    except ValueError: continue
    if p[0] == "F": fout.write("%.1f\n" % s)
    elif p[0] == "c": lout.write("%.1f %.1f\n" % (s, v))
