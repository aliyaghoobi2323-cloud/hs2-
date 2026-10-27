#!/usr/bin/env python3
"""c3sum.py RES...: the C3 variant decision table (T12): per variant and
prototype, how many cells pass each gate, and the p99 tie-break."""
import sys
import os
sys.path.insert(0, "/root/r1-judge")
import judge  # noqa: E402

recs = judge.load(sys.argv[1:])
cells, corder = judge.by_cell(recs)
VN = {"": "پیش‌فرض سند", "4": "SEGS=4", "8": "SEGS=8", "g": "GATE=0"}
print("| گونه | نمونه | بازارسال ≤ پایه+۱ (هر دو سر) | توان ≥ ۰٫۹۷× پایه (هر دو جهت) | p99 کاوشگر درون کران | p99 TUN درون کران | هر سه دروازهٔ سند | میانهٔ (p99 − کران) کاوشگر ms | میانهٔ p99 TUN − کران ms | خانه‌های ✗ |")
print("|---|---|---|---|---|---|---|---|---|---|")
for v in ("", "4", "8", "g"):
    for proto in ("a", "b", "ab"):
        n = rx = th = pd = pt = allg = 0
        ex, tx, fails = [], [], []
        for c in corder:
            bs = cells[c]
            o = bs.get("old")
            for p in (("a", "b") if proto == "ab" else (proto,)):
                s = bs.get(p + v)
                if not s or not s["_n"]:
                    continue
                n += 1
                r0 = s["_rec"]
                tag = "%s/N%s/%s" % (r0["RATE"], r0["N"], r0["LOSS"])
                a = s["rt_exit"][0] <= o["rt_exit"][0] + 1 and s["rt_edge"][0] <= o["rt_edge"][0] + 1
                rd, ru = judge.ratio(s, o, "dn"), judge.ratio(s, o, "up")
                b = (rd is None or rd >= 0.97) and (ru is None or ru >= 0.97)
                d = s["p99"][0] <= s["d_bound"][0]
                t = s.get("t99") and s["t99"][0] <= s["t_bound"][0]
                rx += a
                th += b
                pd += d
                pt += bool(t)
                allg += a and b and d
                ex.append(s["p99"][0] - s["d_bound"][0])
                if s.get("t99"):
                    tx.append(s["t99"][0] - s["t_bound"][0])
                f = []
                if not a:
                    f.append("بازارسال")
                if not b:
                    f.append("توان")
                if not d:
                    f.append("p99")
                if f and proto != "ab":
                    fails.append("%s(%s)" % (tag, "،".join(f)))
        import statistics as st
        print("| %s | %s | %d/%d | %d/%d | %d/%d | %d/%d | %d/%d | %+.0f | %+.0f | %s |" % (
            VN[v], {"a": "الف", "b": "ب", "ab": "هر دو"}[proto], rx, n, th, n, pd, n, pt, n, allg, n,
            st.median(ex), st.median(tx) if tx else float("nan"), " ".join(fails) if proto != "ab" else ""))
