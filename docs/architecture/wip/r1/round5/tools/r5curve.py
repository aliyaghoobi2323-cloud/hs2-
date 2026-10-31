#!/usr/bin/env python3
"""r5curve.py RES.jsonl...: R1 round 5's loss curve, one row per (N, loss,
binary): download sum (Mbit/s), web p95 page load (ms) and failed pages,
call p99 down (one-way less RTT/2, ms), its late+lost share (%) and the
delivered share (the lesser direction), game p99 down TCP / UDP (ms); each
the median of the runs with every run in brackets (r5class.py's rules)."""
import json
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from r5class import med, num, get, owd, late, deliv  # noqa: E402

ORDER = ["prod", "fin2", "r5"]
NAME = {"prod": "تولید", "fin2": "fin2", "r5": "r5"}


def main():
    recs = []
    for p in sys.argv[1:]:
        recs += [json.loads(l) for l in open(p) if l.strip()]
    rows = {}
    for r in recs:
        if not r.get("ok"):
            continue
        k = (int(r["N"]), float(r["LOSS"]), r["bin"])
        rows.setdefault(k, []).append(r)
    print("| N | اتلاف ٪ | باینری | دانلود Mbit/s | وب p95 ms (شکست) | تماس p99 پایین ms | تماس دیر+گم ٪ | رسیده | بازی p99 پایین TCP / UDP ms |")
    print("|---|---|---|---|---|---|---|---|---|")
    for k in sorted(rows, key=lambda k: (k[0], k[1], ORDER.index(k[2]) if k[2] in ORDER else 9)):
        rs = rows[k]
        web = "%s (%s)" % (med([num(get(r, "classes", "web", "main", "load_p95_ms")) for r in rs]),
                           med([num(get(r, "classes", "web", "main", "failed")) for r in rs]).split(" ")[0])
        print("| %d | %g | %s | %s | %s | %s | %s | %s | %s / %s |" % (
            k[0], k[1], NAME.get(k[2], k[2]), med([num(get(r, "dn_sum_mbit", "main")) for r in rs], 1), web,
            med([owd(r, "call", "dn") for r in rs]), med([late(r, "call", "dn") for r in rs], 1),
            med([deliv(r) for r in rs], 2), med([owd(r, "game", "dn") for r in rs]), med([owd(r, "gameudp", "dn") for r in rs])))


if __name__ == "__main__":
    main()
