#!/usr/bin/env python3
"""dpi_count.py DPI_EVAL2 REAL_SIZES: the round-2 judge's use of
obfs/dpi_eval2.py's family B (behavioral, cross-flow) with the connection
count each binary really opens for a pool of N links: base N, β1 2N (a
QUIET and a BULK connection per link), β2 max(N, 1 + QuietCount(N)) (one
QUIET link at N < 4, two from 4, counted within max_links). Each row is the
single-signal AUC of the connection count against dpi_eval2's crowd (0.5:
a coin flip, 1.0: trivial), and the all-feature AUC with dpi_eval2's
"after hardening" values for the other three features (establishment
spread, keepalive regularity, lifetime) and the count fixed.
The definitions are dpi_eval2's own: its source is run up to its report."""
import math
import random
import sys

src = open(sys.argv[1]).read()
src = src[:src.index('print("DPI v2')]
sys.argv = [sys.argv[1], sys.argv[2]]
g = {"__name__": "dpi_eval2_defs"}
exec(compile(src, sys.argv[0], "exec"), g)


def quiet_count(n):
    return 2 if n >= 4 else 1


def conns(label, n):
    if label == "base":
        return n
    if label == "b1":
        return 2 * n
    return max(n, 1 + quiet_count(n))


def tunnel_fixed(nconn):
    def gen():
        est = abs(random.gauss(nconn * 0.1, 0.25))
        ka_cv = random.uniform(0.30, 0.60)
        return g["beh_feats"](nconn, est, ka_cv, abs(random.gauss(7200, 3600)) + 600)
    return gen


print("| N (max_links) | اتصال پایه / β1 / β2 | AUC تک‌نشانهٔ شمار اتصال: پایه / β1 / β2 | AUC همهٔ نشانه‌ها: پایه / β1 / β2 |")
print("|---|---|---|---|")
for n in (1, 2, 4, 8, 16, 64, 300):
    cs = [conns(l, n) for l in ("base", "b1", "b2")]
    one, allf = [], []
    for c in cs:
        random.seed(7)
        one.append(g["evaluate"](lambda: [tunnel_fixed(c)()[0]], lambda: [g["crowd_behavior"]()[0]]))
        random.seed(7)
        allf.append(g["evaluate"](tunnel_fixed(c), g["crowd_behavior"]))
    print("| %d | %d / %d / %d | %.3f / %.3f / %.3f | %.3f / %.3f / %.3f |" % (n, *cs, *one, *allf))
